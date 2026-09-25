package auth

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"migo/internal/httpx"
	"migo/internal/ratelimit"
	"migo/internal/user"
)

const (
	maxBodyBytes   = 16 << 10 // 16 KiB is plenty for auth payloads
	requestTimeout = 10 * time.Second
)

type Handler struct {
	svc *Service
	log *slog.Logger
	// loginAccountLimiter throttles guessing against one account from one IP
	// (key: ip|email). Optional; nil disables it.
	loginAccountLimiter ratelimit.Limiter
	// forgotEmailLimiter caps reset emails per address (across all IPs) to
	// stop mail-bombing a victim. When exhausted the request is silently
	// answered as usual, so nothing about the account is revealed.
	forgotEmailLimiter ratelimit.Limiter
	// resendLimiter caps verification emails per signed-in user;
	// changePasswordLimiter caps current-password guesses per user (a stolen
	// access token must not become an offline-speed password oracle).
	resendLimiter         ratelimit.Limiter
	changePasswordLimiter ratelimit.Limiter
}

func NewHandler(svc *Service, log *slog.Logger) *Handler { return &Handler{svc: svc, log: log} }

// SetResendLimiter installs the per-user resend-verification limiter.
func (h *Handler) SetResendLimiter(l ratelimit.Limiter) { h.resendLimiter = l }

// SetChangePasswordLimiter installs the per-user change-password limiter.
func (h *Handler) SetChangePasswordLimiter(l ratelimit.Limiter) { h.changePasswordLimiter = l }

// SetForgotEmailLimiter installs the per-email forgot-password limiter.
func (h *Handler) SetForgotEmailLimiter(l ratelimit.Limiter) { h.forgotEmailLimiter = l }

// SetLoginLimiter installs the per-(IP, email) login limiter.
func (h *Handler) SetLoginLimiter(l ratelimit.Limiter) { h.loginAccountLimiter = l }

type userResponse struct {
	ID            string    `json:"id"`
	FullName      string    `json:"full_name"`
	Username      string    `json:"username"`
	Email         string    `json:"email"`
	EmailVerified bool      `json:"email_verified"`
	CreatedAt     time.Time `json:"created_at"`
}

func toUserResponse(u user.User) userResponse {
	return userResponse{ID: u.ID, FullName: u.FullName, Username: u.Username, Email: u.Email,
		EmailVerified: u.EmailVerified, CreatedAt: u.CreatedAt}
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
}

func toTokenResponse(t Tokens) tokenResponse {
	return tokenResponse{AccessToken: t.AccessToken, RefreshToken: t.RefreshToken, TokenType: "Bearer", ExpiresIn: t.ExpiresIn}
}

type authResponse struct {
	User userResponse `json:"user"`
	tokenResponse
}

func meta(r *http.Request) Meta {
	ua := r.UserAgent()
	if len(ua) > 256 {
		ua = ua[:256]
	}
	return Meta{UserAgent: ua, IP: httpx.ClientIP(r)}
}

func (h *Handler) Register(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FullName string `json:"full_name"`
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := httpx.DecodeJSON(w, r, &req, maxBodyBytes); err != nil {
		httpx.WriteDecodeError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	u, tokens, err := h.svc.Register(ctx, RegisterInput{FullName: req.FullName, Email: req.Email, Password: req.Password}, meta(r))
	if err != nil {
		h.writeError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, authResponse{User: toUserResponse(u), tokenResponse: toTokenResponse(tokens)})
}

func (h *Handler) Login(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := httpx.DecodeJSON(w, r, &req, maxBodyBytes); err != nil {
		httpx.WriteDecodeError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	// Per-(IP, email) budget. Keying on the IP as well means an attacker can't
	// lock a victim out of their own account from a different address.
	var accountKey string
	if h.loginAccountLimiter != nil {
		email := NormalizeEmail(req.Email)
		if len(email) > maxEmailLen {
			email = email[:maxEmailLen]
		}
		accountKey = ratelimit.IPKey(r) + "|" + email
		d, err := h.loginAccountLimiter.Allow(ctx, accountKey)
		if err != nil {
			h.log.Error("login limiter failed; allowing request", "error", err)
		} else if !d.Allowed {
			ratelimit.Deny(w, d)
			return
		}
	}

	u, tokens, err := h.svc.Login(ctx, LoginInput{Email: req.Email, Password: req.Password}, meta(r))
	if err != nil {
		h.writeError(w, err)
		return
	}
	if h.loginAccountLimiter != nil {
		_ = h.loginAccountLimiter.Reset(ctx, accountKey) // a legit user isn't penalised for earlier typos
	}
	httpx.WriteJSON(w, http.StatusOK, authResponse{User: toUserResponse(u), tokenResponse: toTokenResponse(tokens)})
}

type refreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

func (h *Handler) Refresh(w http.ResponseWriter, r *http.Request) {
	var req refreshRequest
	if err := httpx.DecodeJSON(w, r, &req, maxBodyBytes); err != nil {
		httpx.WriteDecodeError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	tokens, err := h.svc.Refresh(ctx, req.RefreshToken, meta(r))
	if err != nil {
		h.writeError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toTokenResponse(tokens))
}

func (h *Handler) Logout(w http.ResponseWriter, r *http.Request) {
	var req refreshRequest
	if err := httpx.DecodeJSON(w, r, &req, maxBodyBytes); err != nil {
		httpx.WriteDecodeError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	if err := h.svc.Logout(ctx, req.RefreshToken); err != nil {
		h.writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) Me(w http.ResponseWriter, r *http.Request) {
	userID, ok := UserIDFromContext(r.Context())
	if !ok {
		unauthorized(w)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	u, err := h.svc.Me(ctx, userID)
	if err != nil {
		h.writeError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"user": toUserResponse(u)})
}

func (h *Handler) writeError(w http.ResponseWriter, err error) {
	var ve *ValidationError
	switch {
	case errors.As(err, &ve):
		httpx.WriteError(w, http.StatusUnprocessableEntity, "validation_failed", "One or more fields are invalid.", ve.Fields)
	case errors.Is(err, user.ErrEmailTaken):
		httpx.WriteError(w, http.StatusConflict, "email_taken", "An account with this email already exists.", nil)
	case errors.Is(err, ErrInvalidCredentials):
		httpx.WriteError(w, http.StatusUnauthorized, "invalid_credentials", "Invalid email or password.", nil)
	case errors.Is(err, ErrInvalidRefreshToken):
		httpx.WriteError(w, http.StatusUnauthorized, "invalid_refresh_token", "Refresh token is invalid or expired. Please sign in again.", nil)
	case errors.Is(err, ErrInvalidVerificationToken):
		httpx.WriteError(w, http.StatusBadRequest, "invalid_verification_token", "This verification link is invalid or has expired. Request a new one from your account.", nil)
	case errors.Is(err, ErrPasswordNotSet):
		httpx.WriteError(w, http.StatusBadRequest, "password_not_set", "This account has no password yet. Use \"forgot password\" to set one.", nil)
	case errors.Is(err, ErrConcurrentChange):
		httpx.WriteError(w, http.StatusConflict, "conflict", "Your password was changed by another request. Please sign in again.", nil)
	case errors.Is(err, ErrInvalidResetToken):
		httpx.WriteError(w, http.StatusBadRequest, "invalid_reset_token", "This reset link is invalid or has expired. Please request a new one.", nil)
	case errors.Is(err, ErrUnauthorized):
		unauthorized(w)
	case errors.Is(err, context.DeadlineExceeded):
		httpx.WriteError(w, http.StatusGatewayTimeout, "timeout", "The request timed out.", nil)
	default:
		h.log.Error("auth request failed", "error", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "Something went wrong.", nil)
	}
}

const forgotPasswordMessage = "If an account exists for that email, a password reset link has been sent."

func (h *Handler) ForgotPassword(w http.ResponseWriter, r *http.Request) {
	if !h.svc.PasswordResetEnabled() {
		httpx.WriteError(w, http.StatusNotFound, "not_found", "Not found.", nil)
		return
	}
	var req struct {
		Email string `json:"email"`
	}
	if err := httpx.DecodeJSON(w, r, &req, maxBodyBytes); err != nil {
		httpx.WriteDecodeError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	accepted := func() {
		httpx.WriteJSON(w, http.StatusAccepted, map[string]string{"message": forgotPasswordMessage})
	}

	if h.forgotEmailLimiter != nil {
		email := NormalizeEmail(req.Email)
		if len(email) > maxEmailLen {
			email = email[:maxEmailLen]
		}
		// Keyed on the address whether or not it is registered, so behaviour is
		// identical for real and made-up emails.
		if d, err := h.forgotEmailLimiter.Allow(ctx, email); err == nil && !d.Allowed {
			accepted()
			return
		}
	}

	if err := h.svc.RequestPasswordReset(ctx, req.Email, meta(r)); err != nil {
		h.writeError(w, err)
		return
	}
	accepted()
}

func (h *Handler) ResetPassword(w http.ResponseWriter, r *http.Request) {
	if !h.svc.PasswordResetEnabled() {
		httpx.WriteError(w, http.StatusNotFound, "not_found", "Not found.", nil)
		return
	}
	var req struct {
		Token       string `json:"token"`
		NewPassword string `json:"new_password"`
	}
	if err := httpx.DecodeJSON(w, r, &req, maxBodyBytes); err != nil {
		httpx.WriteDecodeError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	if err := h.svc.ResetPassword(ctx, req.Token, req.NewPassword); err != nil {
		h.writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) VerifyEmail(w http.ResponseWriter, r *http.Request) {
	if !h.svc.EmailVerificationEnabled() {
		httpx.WriteError(w, http.StatusNotFound, "not_found", "Not found.", nil)
		return
	}
	var req struct {
		Token string `json:"token"`
	}
	if err := httpx.DecodeJSON(w, r, &req, maxBodyBytes); err != nil {
		httpx.WriteDecodeError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	if err := h.svc.VerifyEmail(ctx, req.Token); err != nil {
		h.writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) ResendVerification(w http.ResponseWriter, r *http.Request) {
	if !h.svc.EmailVerificationEnabled() {
		httpx.WriteError(w, http.StatusNotFound, "not_found", "Not found.", nil)
		return
	}
	userID, ok := UserIDFromContext(r.Context())
	if !ok {
		unauthorized(w)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	if h.resendLimiter != nil {
		d, err := h.resendLimiter.Allow(ctx, userID)
		if err != nil {
			h.log.Error("resend limiter failed; allowing request", "error", err)
		} else if !d.Allowed {
			ratelimit.Deny(w, d)
			return
		}
	}
	sent, err := h.svc.ResendVerification(ctx, userID)
	if err != nil {
		h.writeError(w, err)
		return
	}
	if !sent {
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"message": "Your email is already verified."})
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, map[string]string{"message": "Verification email sent."})
}

func (h *Handler) ChangePassword(w http.ResponseWriter, r *http.Request) {
	if !h.svc.ChangePasswordEnabled() {
		httpx.WriteError(w, http.StatusNotFound, "not_found", "Not found.", nil)
		return
	}
	userID, ok := UserIDFromContext(r.Context())
	if !ok {
		unauthorized(w)
		return
	}
	var req struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if err := httpx.DecodeJSON(w, r, &req, maxBodyBytes); err != nil {
		httpx.WriteDecodeError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	if h.changePasswordLimiter != nil {
		d, err := h.changePasswordLimiter.Allow(ctx, userID)
		if err != nil {
			h.log.Error("change-password limiter failed; allowing request", "error", err)
		} else if !d.Allowed {
			ratelimit.Deny(w, d)
			return
		}
	}
	tokens, err := h.svc.ChangePassword(ctx, userID, ChangePasswordInput{CurrentPassword: req.CurrentPassword, NewPassword: req.NewPassword}, meta(r))
	if err != nil {
		h.writeError(w, err)
		return
	}
	if h.changePasswordLimiter != nil {
		_ = h.changePasswordLimiter.Reset(ctx, userID)
	}
	httpx.WriteJSON(w, http.StatusOK, toTokenResponse(tokens))
}
