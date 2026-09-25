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
	"migo/internal/mailer"
	"migo/internal/server"
	"migo/internal/user"
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

	svc.EnableEmailVerification(auth.NewVerifyRepository(pool), mailQueue, cfg.EmailVerificationURL, cfg.EmailVerificationTTL, log)
	svc.EnableChangePassword(auth.NewCredentialRepository(pool), mailQueue, log)

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
			Log: log, DB: pool, Auth: authHandler, Tokens: tokens,
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
