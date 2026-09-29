package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"migo/internal/auth"
	"migo/internal/config"
	"migo/internal/database"
	"migo/internal/fulfillment"
	"migo/internal/mailer"
	"migo/internal/messaging"
	"migo/internal/payment"
	"migo/internal/providerconfig"
	"migo/internal/providercrypto"
	"migo/internal/renewal"
	"migo/internal/rental"
	"migo/internal/server"
	"migo/internal/user"
	"migo/internal/wallet"
	"migo/internal/walletpurchase"
	"migo/migrations"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	// Optional local .env; real environment variables always win.
	_ = godotenv.Load()

	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := database.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	// `go run ./cmd/api migrate` applies migrations and exits.
	if len(os.Args) > 1 && os.Args[1] == "migrate" {
		return database.Migrate(ctx, pool, migrations.FS, log)
	}
	if cfg.AutoMigrate {
		if err := database.Migrate(ctx, pool, migrations.FS, log); err != nil {
			return err
		}
	}

	hasher := auth.NewHasher(auth.DefaultParams(), 4)
	tokens := auth.NewTokenManager(cfg.AccessTokenSecret, "migo", cfg.AccessTokenTTL)
	svc, err := auth.NewService(user.NewRepository(pool), auth.NewSessionRepository(pool), hasher, tokens, cfg.RefreshTokenTTL)
	if err != nil {
		return err
	}

	mailer.DefaultBrand = mailer.Brand{Name: cfg.MailBrandName, LogoURL: cfg.MailLogoURL}

	// Email is sent through Resend when configured. In development without a
	// Resend API key, messages are logged instead. Delivery is asynchronous.
	var sender mailer.Sender
	if cfg.ResendAPIKey != "" {
		sender = mailer.NewResend(cfg.ResendAPIKey, cfg.MailFrom)
	} else {
		log.Warn("RESEND_API_KEY not set: emails are only LOGGED (development)")
		sender = mailer.Log{Logger: log}
	}
	mailQueue := mailer.NewAsync(sender, log, 2, 100)
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := mailQueue.Close(c); err != nil {
			log.Warn("mail queue did not drain before shutdown", "error", err)
		}
	}()
	svc.EnablePasswordReset(auth.NewResetRepository(pool), mailQueue, cfg.PasswordResetURL, cfg.PasswordResetTTL, log)

	dashboardURL := ""
	if len(cfg.AllowedOrigins) > 0 {
		dashboardURL = cfg.AllowedOrigins[0] + "/dashboard"
	}
	svc.EnableEmailVerification(auth.NewVerifyRepository(pool), mailQueue, cfg.EmailVerificationURL, dashboardURL, cfg.EmailVerificationTTL, log)
	svc.EnableChangePassword(auth.NewCredentialRepository(pool), mailQueue, log)

	rentalSvc, err := rental.NewService(rental.NewRepository(pool), cfg.RentalReservationTTL)
	if err != nil {
		return err
	}
	rentalHandler := rental.NewHandler(rentalSvc)
	walletSvc, err := wallet.NewService(wallet.NewRepository(pool))
	if err != nil {
		return err
	}
	walletHandler := wallet.NewHandler(walletSvc)

	renewalSvc, err := renewal.NewService(rentalSvc, walletSvc)
	if err != nil {
		return err
	}
	topUpURL := ""
	if len(cfg.AllowedOrigins) > 0 {
		topUpURL = cfg.AllowedOrigins[0] + "/wallet"
	}
	renewalSvc.EnableFailureEmails(mailQueue, topUpURL)
	renewalHandler := renewal.NewHandler(renewalSvc)

	// Runs the monthly (or whatever a plan's duration is) rental renewal
	// pass on a fixed interval: re-bill each active rental whose period has
	// ended, or release it if the customer's wallet can't cover the next
	// one. Stops when ctx is cancelled (process shutdown).
	go func() {
		ticker := time.NewTicker(cfg.RenewalInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				result, err := renewalSvc.RunDue(ctx)
				if err != nil {
					log.Error("renewal run failed", "error", err)
					continue
				}
				if result.Renewed > 0 || result.Expired > 0 || result.Failed > 0 {
					log.Info("renewal run complete", "renewed", result.Renewed, "expired", result.Expired, "failed", result.Failed)
				}
			}
		}
	}()

	// Provider credential encryption is optional until PROVIDER_CREDENTIAL_KEYS
	// is set; without it, admins simply cannot create provider configs, and
	// payment processing (which needs a decrypted Paystack secret key) stays
	// disabled too.
	var providerConfigHandler *providerconfig.Handler
	var paymentHandler *payment.Handler
	var messagingHandler *messaging.Handler
	var walletPurchaseHandler *walletpurchase.Handler
	if len(cfg.ProviderCredentialKeys) > 0 {
		keyRing, err := providercrypto.NewKeyRing(cfg.ProviderCredentialKeys, cfg.ProviderCredentialKeyVersion)
		if err != nil {
			return err
		}
		providerConfigSvc, err := providerconfig.NewService(providerconfig.NewRepository(pool), keyRing)
		if err != nil {
			return err
		}
		providerConfigHandler = providerconfig.NewHandler(providerConfigSvc)

		telephonyResolver := fulfillment.NewProviderConfigResolver(providerConfigSvc)
		fulfillmentSvc, err := fulfillment.NewService(rentalSvc, telephonyResolver)
		if err != nil {
			return err
		}

		paystackResolver := payment.NewProviderConfigResolver(providerConfigSvc)
		paymentSvc, err := payment.NewService(
			payment.NewRepository(pool),
			payment.NewRentalOrders(rentalSvc),
			payment.NewAuthUserEmails(svc),
			paystackResolver,
			fulfillmentSvc,
			cfg.PaystackCallbackURL,
		)
		if err != nil {
			return err
		}
		paymentHandler = payment.NewHandler(paymentSvc)
		fundingSvc, err := wallet.NewFundingService(walletSvc, wallet.NewRepository(pool), paystackResolver, payment.NewAuthUserEmails(svc), cfg.PaystackCallbackURL)
		if err != nil {
			return err
		}
		walletHandler.EnableFunding(fundingSvc)
		paymentSvc.EnableWalletFunding(fundingSvc)
		fundingSvc.EnableReceipts(mailQueue, payment.NewAuthUserEmails(svc))

		walletPurchaseSvc, err := walletpurchase.NewService(rentalSvc, walletSvc, fulfillmentSvc)
		if err != nil {
			return err
		}
		walletPurchaseSvc.EnableReceipts(mailQueue, payment.NewAuthUserEmails(svc))
		walletPurchaseHandler = walletpurchase.NewHandler(walletPurchaseSvc)

		messagingSvc, err := messaging.NewService(messaging.NewRepository(pool), telephonyResolver)
		if err != nil {
			return err
		}
		messagingHandler = messaging.NewHandler(messagingSvc, telephonyResolver)
	} else {
		log.Warn("PROVIDER_CREDENTIAL_KEYS not set: provider config management and payment processing are disabled")
	}

	authHandler := auth.NewHandler(svc, log)
	var limiters server.Limiters
	if cfg.RateLimitEnabled {
		limiters = server.DefaultLimiters()
		authHandler.SetLoginLimiter(limiters.LoginAccount)
		authHandler.SetForgotEmailLimiter(limiters.ForgotEmail)
		authHandler.SetResendLimiter(limiters.ResendUser)
		authHandler.SetChangePasswordLimiter(limiters.ChangePasswordUser)
	} else {
		log.Warn("rate limiting is DISABLED")
	}
	if len(cfg.TrustedProxies) == 0 && cfg.AppEnv != "development" {
		log.Warn("TRUSTED_PROXIES is empty: behind a reverse proxy every client will share the proxy's IP for rate limiting")
	}

	srv := &http.Server{
		Addr: ":" + cfg.Port,
		Handler: server.New(server.Deps{
			Log: log, DB: pool, Auth: authHandler, Tokens: tokens, AuthService: svc,
			Rental: rentalHandler, Payment: paymentHandler, ProviderConfig: providerConfigHandler, Messaging: messagingHandler, Wallet: walletHandler,
			WalletPurchase: walletPurchaseHandler, Renewal: renewalHandler,
			Limiters: limiters, AllowedOrigins: cfg.AllowedOrigins, TrustedProxies: cfg.TrustedProxies,
		}),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      20 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 16,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("server listening", "addr", srv.Addr, "env", cfg.AppEnv)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
	return nil
}
