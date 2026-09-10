package service

import (
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"

	"lawyer-bot/internal/domain"
	"lawyer-bot/internal/repository"
)

// AuthService is the Admin CRM authentication layer.
//
// Password storage: PBKDF2-HMAC-SHA256 with a 16-byte random salt per account
// and a configurable iteration count. It is a standard library primitive, so the
// CRM adds no cryptographic dependency to a project that deliberately has almost
// none. Comparison is constant time.
//
// Sessions: the browser holds a random 32-byte token in an HttpOnly cookie; the
// database stores only its SHA-256 digest. A database leak therefore cannot be
// replayed as a login. Each session carries its own CSRF token.
//
// Rate limiting: attempts are counted per client-address hash AND per account,
// both in the database, so restarting the process does not reset an attacker's
// budget.
type AuthService struct {
	admins *repository.AdminRepository
	audit  *repository.AuditRepository
	log    *zap.Logger
	cfg    AuthConfig
}

// AuthConfig configures authentication.
type AuthConfig struct {
	// SessionTTL is how long a session stays valid without activity.
	SessionTTL time.Duration
	// PBKDF2Iterations is the work factor for password hashing.
	PBKDF2Iterations int
	// MaxAttempts is how many failures an address may produce in AttemptWindow.
	MaxAttempts int
	// AttemptWindow is the rate-limit window.
	AttemptWindow time.Duration
	// LockoutThreshold is how many consecutive failures lock an account.
	LockoutThreshold int
	// LockoutDuration is how long an account stays locked.
	LockoutDuration time.Duration
	// SecureCookies forces the Secure attribute; production must enable it.
	SecureCookies bool
}

// Normalise fills in safe defaults.
func (c AuthConfig) Normalise() AuthConfig {
	if c.SessionTTL <= 0 {
		c.SessionTTL = 12 * time.Hour
	}
	if c.PBKDF2Iterations < 100_000 {
		c.PBKDF2Iterations = 600_000
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 10
	}
	if c.AttemptWindow <= 0 {
		c.AttemptWindow = 15 * time.Minute
	}
	if c.LockoutThreshold <= 0 {
		c.LockoutThreshold = 8
	}
	if c.LockoutDuration <= 0 {
		c.LockoutDuration = 15 * time.Minute
	}
	return c
}

// NewAuthService builds the auth service.
func NewAuthService(admins *repository.AdminRepository, audit *repository.AuditRepository,
	log *zap.Logger, cfg AuthConfig) *AuthService {

	if log == nil {
		log = zap.NewNop()
	}
	return &AuthService{admins: admins, audit: audit, log: log, cfg: cfg.Normalise()}
}

// Config exposes the effective configuration.
func (s *AuthService) Config() AuthConfig { return s.cfg }

// Auth errors. The login handler maps every credential problem onto the same
// message, so the API never reveals whether an email exists.
var (
	ErrInvalidCredentials = errors.New("invalid email or password")
	ErrAccountLocked      = errors.New("account temporarily locked")
	ErrRateLimited        = errors.New("too many attempts, try again later")
	ErrWeakPassword       = errors.New("password must be at least 10 characters and contain a letter and a digit")
)

// Session is a freshly issued session with its plaintext cookie token. The
// token is returned exactly once and never stored anywhere.
type Session struct {
	Token     string
	CSRFToken string
	ExpiresAt time.Time
	Admin     domain.AdminUser
}

// EnsureBootstrapAdmin creates the first account when none exists.
//
// The credentials come from the environment and the function is a no-op as soon
// as any account exists, so restarting the service never resets a password.
func (s *AuthService) EnsureBootstrapAdmin(ctx context.Context, email, password, name string) (bool, error) {
	email = strings.TrimSpace(email)
	if email == "" || password == "" {
		return false, nil
	}
	count, err := s.admins.CountAdmins(ctx)
	if err != nil {
		return false, err
	}
	if count > 0 {
		return false, nil
	}
	if err := ValidatePassword(password); err != nil {
		return false, err
	}

	cred, err := s.hash(password)
	if err != nil {
		return false, err
	}
	if name == "" {
		name = "Administrator"
	}
	if _, err := s.admins.CreateAdmin(ctx, domain.AdminUser{
		Email: email, Name: name, Role: domain.RoleAdmin, Active: true,
	}, cred); err != nil {
		return false, err
	}
	s.log.Info("bootstrap admin account created", zap.String("email", email))
	return true, nil
}

// CreateAccount adds a CRM account.
func (s *AuthService) CreateAccount(ctx context.Context, email, password, name string, role domain.AdminRole) (*domain.AdminUser, error) {
	if err := ValidatePassword(password); err != nil {
		return nil, err
	}
	cred, err := s.hash(password)
	if err != nil {
		return nil, err
	}
	return s.admins.CreateAdmin(ctx, domain.AdminUser{
		Email: email, Name: name, Role: role, Active: true,
	}, cred)
}

// ChangePassword replaces an account's password and invalidates its sessions.
func (s *AuthService) ChangePassword(ctx context.Context, adminID int64, current, next string) error {
	cred, err := s.admins.Credentials(ctx, adminID)
	if err != nil {
		return err
	}
	if !s.verify(current, cred) {
		return ErrInvalidCredentials
	}
	if err := ValidatePassword(next); err != nil {
		return err
	}
	fresh, err := s.hash(next)
	if err != nil {
		return err
	}
	if err := s.admins.SetPassword(ctx, adminID, fresh); err != nil {
		return err
	}
	// Every existing session is dropped: a password change must log out any
	// stolen session immediately.
	return s.admins.DeleteSessionsForAdmin(ctx, adminID)
}

// ResetPassword sets a password without knowing the current one. Reserved for
// administrators managing other accounts.
func (s *AuthService) ResetPassword(ctx context.Context, adminID int64, next string) error {
	if err := ValidatePassword(next); err != nil {
		return err
	}
	cred, err := s.hash(next)
	if err != nil {
		return err
	}
	if err := s.admins.SetPassword(ctx, adminID, cred); err != nil {
		return err
	}
	return s.admins.DeleteSessionsForAdmin(ctx, adminID)
}

// Login authenticates an account and issues a session.
//
// clientKey is a hash of the caller's address, never the address itself, so the
// rate-limit table holds no personal data.
func (s *AuthService) Login(ctx context.Context, email, password, clientKey string) (*Session, error) {
	since := time.Now().UTC().Add(-s.cfg.AttemptWindow)
	attempts, err := s.admins.CountLoginAttempts(ctx, clientKey, since)
	if err != nil {
		return nil, err
	}
	if attempts >= s.cfg.MaxAttempts {
		return nil, ErrRateLimited
	}
	if err := s.admins.NoteLoginAttempt(ctx, clientKey); err != nil {
		s.log.Warn("record login attempt failed", zap.Error(err))
	}

	admin, err := s.admins.GetAdminByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			// Spend the same work as a real verification, so response timing
			// does not reveal whether the account exists.
			s.decoyVerify(password)
			return nil, ErrInvalidCredentials
		}
		return nil, err
	}
	if !admin.Active {
		return nil, ErrInvalidCredentials
	}
	if admin.Locked(time.Now().UTC()) {
		return nil, ErrAccountLocked
	}

	cred, err := s.admins.Credentials(ctx, admin.ID)
	if err != nil {
		return nil, err
	}
	if !s.verify(password, cred) {
		if err := s.admins.RecordLoginFailure(ctx, admin.ID,
			s.cfg.LockoutThreshold, s.cfg.LockoutDuration); err != nil {
			s.log.Warn("record login failure failed", zap.Error(err))
		}
		s.record(ctx, admin.ID, domain.AuditLoginFailed, clientKey, "invalid password")
		return nil, ErrInvalidCredentials
	}

	if err := s.admins.RecordLoginSuccess(ctx, admin.ID); err != nil {
		s.log.Warn("record login success failed", zap.Error(err))
	}
	if err := s.admins.ClearLoginAttempts(ctx, clientKey); err != nil {
		s.log.Warn("clear login attempts failed", zap.Error(err))
	}

	session, err := s.issue(ctx, *admin)
	if err != nil {
		return nil, err
	}
	s.record(ctx, admin.ID, domain.AuditLogin, clientKey, "session issued")
	return session, nil
}

func (s *AuthService) issue(ctx context.Context, admin domain.AdminUser) (*Session, error) {
	token, err := randomToken(32)
	if err != nil {
		return nil, err
	}
	csrf, err := randomToken(32)
	if err != nil {
		return nil, err
	}
	expires := time.Now().UTC().Add(s.cfg.SessionTTL)

	if err := s.admins.CreateSession(ctx, domain.AdminSession{
		ID:        HashToken(token),
		AdminID:   admin.ID,
		CSRFToken: csrf,
		ExpiresAt: expires,
	}); err != nil {
		return nil, err
	}
	return &Session{Token: token, CSRFToken: csrf, ExpiresAt: expires, Admin: admin}, nil
}

// Authenticate resolves a cookie token into an account and its session.
func (s *AuthService) Authenticate(ctx context.Context, token string) (*domain.AdminUser, *domain.AdminSession, error) {
	if strings.TrimSpace(token) == "" {
		return nil, nil, ErrInvalidCredentials
	}
	session, err := s.admins.GetSession(ctx, HashToken(token))
	if err != nil {
		return nil, nil, ErrInvalidCredentials
	}
	now := time.Now().UTC()
	if session.Expired(now) {
		if err := s.admins.DeleteSession(ctx, session.ID); err != nil {
			s.log.Warn("delete expired session failed", zap.Error(err))
		}
		return nil, nil, ErrInvalidCredentials
	}

	admin, err := s.admins.GetAdmin(ctx, session.AdminID)
	if err != nil || !admin.Active {
		return nil, nil, ErrInvalidCredentials
	}

	// Sliding expiry, refreshed at most once a minute to avoid a write per
	// request on a busy CRM.
	if now.Sub(session.LastSeenAt) > time.Minute {
		if err := s.admins.TouchSession(ctx, session.ID, now.Add(s.cfg.SessionTTL)); err != nil {
			s.log.Warn("touch session failed", zap.Error(err))
		}
	}
	return admin, session, nil
}

// Logout ends one session.
func (s *AuthService) Logout(ctx context.Context, token string, adminID int64, clientKey string) error {
	if strings.TrimSpace(token) == "" {
		return nil
	}
	if err := s.admins.DeleteSession(ctx, HashToken(token)); err != nil {
		return err
	}
	s.record(ctx, adminID, domain.AuditLogout, clientKey, "session ended")
	return nil
}

// Prune removes expired sessions and stale rate-limit rows.
func (s *AuthService) Prune(ctx context.Context) {
	now := time.Now().UTC()
	if err := s.admins.PruneSessions(ctx, now); err != nil {
		s.log.Warn("prune sessions failed", zap.Error(err))
	}
	if err := s.admins.PruneLoginAttempts(ctx, now.Add(-24*time.Hour)); err != nil {
		s.log.Warn("prune login attempts failed", zap.Error(err))
	}
}

// StartJanitor prunes expired auth state on a schedule.
func (s *AuthService) StartJanitor(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = time.Hour
	}
	go func() {
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.Prune(ctx)
			}
		}
	}()
}

// ---------------------------------------------------------------- primitives

func (s *AuthService) hash(password string) (repository.AdminCredentials, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return repository.AdminCredentials{}, fmt.Errorf("generate salt: %w", err)
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, s.cfg.PBKDF2Iterations, 32)
	if err != nil {
		return repository.AdminCredentials{}, fmt.Errorf("derive password key: %w", err)
	}
	return repository.AdminCredentials{
		Hash:       base64.RawStdEncoding.EncodeToString(key),
		Salt:       base64.RawStdEncoding.EncodeToString(salt),
		Iterations: s.cfg.PBKDF2Iterations,
	}, nil
}

func (s *AuthService) verify(password string, cred repository.AdminCredentials) bool {
	salt, err := base64.RawStdEncoding.DecodeString(cred.Salt)
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(cred.Hash)
	if err != nil {
		return false
	}
	iterations := cred.Iterations
	if iterations <= 0 {
		iterations = s.cfg.PBKDF2Iterations
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, iterations, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

// decoyVerify burns the same work as a real verification for an unknown email.
func (s *AuthService) decoyVerify(password string) {
	salt := make([]byte, 16)
	_, _ = pbkdf2.Key(sha256.New, password, salt, s.cfg.PBKDF2Iterations, 32)
}

func (s *AuthService) record(ctx context.Context, adminID int64, action, ipHash, detail string) {
	if s.audit == nil {
		return
	}
	if err := s.audit.Record(ctx, domain.AuditLog{
		AdminID: adminID, Action: action, Entity: "admin", EntityID: adminID,
		Detail: detail, IPHash: ipHash,
	}); err != nil {
		s.log.Warn("write audit entry failed", zap.Error(err))
	}
}

// ValidatePassword enforces a minimum password strength.
func ValidatePassword(password string) error {
	if len([]rune(password)) < 10 {
		return ErrWeakPassword
	}
	var hasLetter, hasDigit bool
	for _, r := range password {
		switch {
		case r >= '0' && r <= '9':
			hasDigit = true
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r > 127:
			hasLetter = true
		}
	}
	if !hasLetter || !hasDigit {
		return ErrWeakPassword
	}
	return nil
}

// HashToken digests a session or CSRF token for storage and for rate-limit keys.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// ConstantTimeEquals compares two secrets without leaking their contents through
// timing. It is used for the CSRF check.
func ConstantTimeEquals(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
