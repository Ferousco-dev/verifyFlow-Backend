package auth

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"migo/internal/user"
)

var (
	ErrInvalidCredentials  = errors.New("invalid email or password")
	ErrInvalidRefreshToken = errors.New("invalid refresh token")
	ErrUnauthorized        = errors.New("unauthorized")
	ErrInvalidRole         = errors.New("invalid role")
	ErrLastAdmin           = errors.New("cannot remove the last remaining admin")
)

// UserStore is the user persistence the service depends on.
type UserStore interface {
	Create(ctx context.Context, u user.User) (user.User, error)
	GetByEmail(ctx context.Context, email string) (user.User, error)
	GetByID(ctx context.Context, id string) (user.User, error)
	UpdatePasswordHash(ctx context.Context, id, hash string) error
	UpdateRole(ctx context.Context, id, role string) error
	CountAdmins(ctx context.Context) (int, error)
}

type Service struct {
	users      UserStore
	sessions   SessionStore
	hasher     *Hasher
	tokens     *TokenManager
	refreshTTL time.Duration
	now        func() time.Time
	dummyHash  string // verified against when the user doesn't exist (timing parity)
	reset      *resetConfig
	verify     *verifyConfig
	change     *changeConfig
}

// userNotFound aliases user.ErrNotFound for use in files that don't import user.
var userNotFound = user.ErrNotFound

func NewService(users UserStore, sessions SessionStore, hasher *Hasher, tokens *TokenManager, refreshTTL time.Duration) (*Service, error) {
	dummy, err := hasher.Hash(context.Background(), "migo-dummy-password-for-timing")
	if err != nil {
		return nil, err
	}
	return &Service{users: users, sessions: sessions, hasher: hasher, tokens: tokens,
		refreshTTL: refreshTTL, now: time.Now, dummyHash: dummy}, nil
}

type RegisterInput struct{ FullName, Email, Password string }
type LoginInput struct{ Email, Password string }

// Meta is request context stored on the session (informational only).
type Meta struct{ UserAgent, IP string }

type Tokens struct {
	AccessToken  string
	RefreshToken string
	ExpiresIn    int // access token lifetime in seconds
}

func (s *Service) Register(ctx context.Context, in RegisterInput, meta Meta) (user.User, Tokens, error) {
	in.FullName = strings.TrimSpace(in.FullName)
	in.Email = NormalizeEmail(in.Email)

	fields := map[string]string{}
	if m := validateFullName(in.FullName); m != "" {
		fields["full_name"] = m
	}
	if m := validateEmail(in.Email); m != "" {
		fields["email"] = m
	}
	if m := validatePassword(in.Password); m != "" {
		fields["password"] = m
	}
	if len(fields) > 0 {
		return user.User{}, Tokens{}, &ValidationError{Fields: fields}
	}

	hash, err := s.hasher.Hash(ctx, in.Password)
	if err != nil {
		return user.User{}, Tokens{}, fmt.Errorf("hash password: %w", err)
	}

	base := user.Slugify(in.FullName)
	var created user.User
	// The DB UNIQUE constraint is authoritative: try, and on a username
	// collision retry with a random suffix instead of check-then-insert.
	for attempt := 0; ; attempt++ {
		if attempt >= 10 {
			return user.User{}, Tokens{}, errors.New("could not generate a unique username")
		}
		candidate := base
		if attempt > 0 {
			digits := 4
			if attempt >= 5 {
				digits = 8
			}
			suffix, err := randomDigits(digits)
			if err != nil {
				return user.User{}, Tokens{}, err
			}
			candidate = base + "-" + suffix
		}
		created, err = s.users.Create(ctx, user.User{
			FullName: in.FullName, Username: candidate, Email: in.Email, PasswordHash: &hash,
		})
		if errors.Is(err, user.ErrUsernameTaken) {
			continue
		}
		if err != nil {
			return user.User{}, Tokens{}, err // includes user.ErrEmailTaken
		}
		break
	}

	tokens, err := s.startSession(ctx, created.ID, meta)
	if err != nil {
		return user.User{}, Tokens{}, err
	}
	// Registration must not fail because a verification email couldn't be
	// prepared; the user can request another with ResendVerification.
	if s.verify != nil {
		if err := s.sendVerification(ctx, created); err != nil {
			s.verify.log.Error("could not create email verification token", "error", err)
		}
	}
	return created, tokens, nil
}

func (s *Service) Login(ctx context.Context, in LoginInput, meta Meta) (user.User, Tokens, error) {
	email := NormalizeEmail(in.Email)
	if email == "" || in.Password == "" || len(in.Password) > maxPasswordLen {
		return user.User{}, Tokens{}, ErrInvalidCredentials
	}

	u, err := s.users.GetByEmail(ctx, email)
	if err != nil && !errors.Is(err, user.ErrNotFound) {
		return user.User{}, Tokens{}, err
	}
	hash := s.dummyHash
	found := err == nil && u.PasswordHash != nil
	if found {
		hash = *u.PasswordHash
	}
	// Always run one Argon2 verification so response time doesn't reveal
	// whether the account exists.
	ok, needsRehash, verr := s.hasher.Verify(ctx, in.Password, hash)
	if verr != nil && !errors.Is(verr, errInvalidHash) {
		return user.User{}, Tokens{}, verr
	}
	if !found || !ok || !u.IsActive {
		return user.User{}, Tokens{}, ErrInvalidCredentials
	}

	if needsRehash {
		if nh, err := s.hasher.Hash(ctx, in.Password); err == nil {
			_ = s.users.UpdatePasswordHash(ctx, u.ID, nh) // best effort upgrade
		}
	}
	tokens, err := s.startSession(ctx, u.ID, meta)
	if err != nil {
		return user.User{}, Tokens{}, err
	}
	return u, tokens, nil
}

// Refresh rotates the refresh token and returns a new pair. Replaying an
// already-rotated token revokes the whole session family.
func (s *Service) Refresh(ctx context.Context, rawToken string, meta Meta) (Tokens, error) {
	if rawToken == "" {
		return Tokens{}, ErrInvalidRefreshToken
	}
	newRaw, newHash, err := NewRefreshToken()
	if err != nil {
		return Tokens{}, err
	}
	now := s.now()
	res, err := s.sessions.Rotate(ctx, HashRefreshToken(rawToken), NewSession{
		TokenHash: newHash, ExpiresAt: now.Add(s.refreshTTL), UserAgent: meta.UserAgent, IP: meta.IP,
	}, now)
	if err != nil {
		return Tokens{}, err
	}
	if res.Outcome != RotateOK {
		return Tokens{}, ErrInvalidRefreshToken
	}
	access, _, err := s.tokens.Issue(res.UserID)
	if err != nil {
		return Tokens{}, err
	}
	return Tokens{AccessToken: access, RefreshToken: newRaw, ExpiresIn: int(s.tokens.TTL().Seconds())}, nil
}

// Logout revokes the session family of the presented refresh token.
// It is idempotent and reveals nothing about whether the token was valid.
func (s *Service) Logout(ctx context.Context, rawToken string) error {
	if rawToken == "" {
		return nil
	}
	return s.sessions.RevokeFamily(ctx, HashRefreshToken(rawToken), "logout", s.now())
}

// Me returns the authenticated user, rejecting deactivated accounts.
func (s *Service) Me(ctx context.Context, userID string) (user.User, error) {
	u, err := s.users.GetByID(ctx, userID)
	if errors.Is(err, user.ErrNotFound) {
		return user.User{}, ErrUnauthorized
	}
	if err != nil {
		return user.User{}, err
	}
	if !u.IsActive {
		return user.User{}, ErrUnauthorized
	}
	return u, nil
}

// SetUserRole changes targetUserID's role. It refuses to demote the last
// remaining admin, so an admin can never lock every admin out of the system.
func (s *Service) SetUserRole(ctx context.Context, targetUserID, role string) (user.User, error) {
	if !user.IsValidRole(role) {
		return user.User{}, ErrInvalidRole
	}
	target, err := s.users.GetByID(ctx, targetUserID)
	if errors.Is(err, user.ErrNotFound) {
		return user.User{}, user.ErrNotFound
	}
	if err != nil {
		return user.User{}, err
	}
	if target.Role == role {
		return target, nil
	}
	if target.Role == user.RoleAdmin && role != user.RoleAdmin {
		count, err := s.users.CountAdmins(ctx)
		if err != nil {
			return user.User{}, err
		}
		if count <= 1 {
			return user.User{}, ErrLastAdmin
		}
	}
	if err := s.users.UpdateRole(ctx, targetUserID, role); err != nil {
		return user.User{}, err
	}
	target.Role = role
	return target, nil
}

func (s *Service) startSession(ctx context.Context, userID string, meta Meta) (Tokens, error) {
	raw, hash, err := NewRefreshToken()
	if err != nil {
		return Tokens{}, err
	}
	if err := s.sessions.Create(ctx, NewSession{
		UserID: userID, TokenHash: hash, ExpiresAt: s.now().Add(s.refreshTTL),
		UserAgent: meta.UserAgent, IP: meta.IP,
	}); err != nil {
		return Tokens{}, fmt.Errorf("create session: %w", err)
	}
	access, _, err := s.tokens.Issue(userID)
	if err != nil {
		return Tokens{}, err
	}
	return Tokens{AccessToken: access, RefreshToken: raw, ExpiresIn: int(s.tokens.TTL().Seconds())}, nil
}

func randomDigits(n int) (string, error) {
	max := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil)
	v, err := rand.Int(rand.Reader, max)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%0*d", n, v), nil
}
