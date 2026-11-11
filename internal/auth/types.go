package auth

import (
	"context"
	"crypto/rand"
	"errors"
	"html/template"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/metavert-io/flipbook/internal/config"
	"github.com/metavert-io/flipbook/internal/database"
	"github.com/metavert-io/flipbook/internal/models"
)

const (
	SessionCookieName     = "flipbook_session"
	CSRFCookieName        = "flipbook_csrf"
	OIDCBindingCookieName = "flipbook_oidc_binding"
	CSRFHeaderName        = "X-CSRF-Token"
	CSRFFormField         = "csrf_token"

	sessionCookie  = SessionCookieName
	settingPwdHash = "admin_password_hash"
	bcryptCost     = 12

	// SettingSessionTTLSeconds and SettingLoginAttemptsPerMinute are runtime
	// overrides stored in the settings collection by the admin panel. They
	// fall back to the server configuration when unset or invalid.
	SettingSessionTTLSeconds      = "session_ttl_seconds"
	SettingLoginAttemptsPerMinute = "login_attempts_per_minute"
)

const (
	minSessionTTLSeconds = 3600           // 1 hour
	maxSessionTTLSeconds = 30 * 24 * 3600 // 30 days
	maxLoginAttemptLimit = 1000           // attempts per minute
)

var (
	ErrUnauthorized         = errors.New("authentication required")
	ErrForbidden            = errors.New("insufficient permissions")
	ErrInvalidCSRF          = errors.New("invalid CSRF token")
	ErrInvalidOrigin        = errors.New("invalid request origin")
	ErrRequestTooLarge      = errors.New("request body too large")
	ErrTooManyAttempts      = errors.New("too many login attempts")
	ErrNotConfigured        = errors.New("authentication is not configured")
	ErrBootstrapClosed      = errors.New("bootstrap is not available")
	ErrInvalidToken         = errors.New("invalid or expired token")
	ErrNoWorkspace          = errors.New("user has no accessible workspace")
	ErrAccountExists        = errors.New("account already exists")
	ErrInvalidPassword      = errors.New("password must contain 12 to 72 bytes")
	ErrOIDC                 = errors.New("OpenID Connect authentication failed")
	ErrAccountLinkRequired  = errors.New("existing account must be linked before OIDC login")
	errAuthenticationFailed = errors.New("invalid credentials")
)

type Store interface {
	GetSetting(key string) (string, error)
	SetSetting(key, value string) error

	CreateSessionRecord(ctx context.Context, session *models.Session) error
	GetSessionByToken(ctx context.Context, token string) (*models.Session, error)
	TouchSession(ctx context.Context, token string) error
	RevokeSession(ctx context.Context, token string) error
	RevokeUserSessions(ctx context.Context, userID string) error

	CreateUser(ctx context.Context, user *models.User) error
	GetUser(ctx context.Context, userID string) (*models.User, error)
	GetUserByEmail(ctx context.Context, email string) (*models.User, error)
	GetUserByOIDC(ctx context.Context, issuer, subject string) (*models.User, error)
	UpdateUserProfile(ctx context.Context, userID, name, locale, timeZone string) error
	SetUserPassword(ctx context.Context, userID, passwordHash string) error

	GetWorkspace(ctx context.Context, workspaceID string) (*models.Workspace, error)
	ListWorkspacesForUser(ctx context.Context, userID string) ([]*models.Workspace, error)
	GetMembership(ctx context.Context, workspaceID, userID string) (*models.Membership, error)
	UpsertMembership(ctx context.Context, membership *models.Membership) error

	CreateInvitation(ctx context.Context, invitation *models.Invitation) error
	GetInvitationByToken(ctx context.Context, token string) (*models.Invitation, error)
	AcceptInvitation(ctx context.Context, token, userID string) error
	RevokeInvitation(ctx context.Context, workspaceID, invitationID string) error

	CreateAPIKey(ctx context.Context, key *models.APIKey) error
	AuthenticateAPIKey(ctx context.Context, secret, requiredScope string) (*models.APIKey, error)
	RevokeAPIKey(ctx context.Context, workspaceID, keyID string) error

	RecordAuditEvent(ctx context.Context, event *models.AuditEvent) error
}

type AuditHook func(context.Context, *models.AuditEvent) error
type DeliveryHook func(ctx context.Context, destination, token string) error

type Options struct {
	SessionSecret          string
	SessionTTL             time.Duration
	SecureCookies          bool
	HTTPSMode              bool
	HSTS                   bool
	HSTSMaxAge             time.Duration
	HSTSIncludeSubdomains  bool
	LoginAttemptsPerMinute int
	BaseURL                string
	TrustedProxies         []string
	OIDC                   config.OIDCConfig
	LegacyAPIKey           string
	HTTPClient             *http.Client
	AuditHook              AuditHook
	PasswordResetDelivery  DeliveryHook
}

type Principal struct {
	User       *models.User
	Session    *models.Session
	Membership *models.Membership
	APIKey     *models.APIKey
}

func (p *Principal) WorkspaceID() string {
	if p == nil {
		return ""
	}
	if p.Membership != nil {
		return p.Membership.WorkspaceID
	}
	if p.APIKey != nil {
		return p.APIKey.WorkspaceID
	}
	if p.Session != nil {
		return p.Session.WorkspaceID
	}
	return ""
}

func (p *Principal) Role() models.Role {
	if p == nil || p.Membership == nil {
		return ""
	}
	return p.Membership.Role
}

type contextKey uint8

const (
	principalContextKey contextKey = iota
	csrfContextKey
)

func PrincipalFromContext(ctx context.Context) (*Principal, bool) {
	principal, ok := ctx.Value(principalContextKey).(*Principal)
	return principal, ok && principal != nil
}

func UserFromContext(ctx context.Context) (*models.User, bool) {
	principal, ok := PrincipalFromContext(ctx)
	if !ok || principal.User == nil {
		return nil, false
	}
	return principal.User, true
}

func RoleFromContext(ctx context.Context) (models.Role, bool) {
	principal, ok := PrincipalFromContext(ctx)
	if !ok || !principal.Role().Valid() {
		return "", false
	}
	return principal.Role(), true
}

func WorkspaceIDFromContext(ctx context.Context) string {
	principal, ok := PrincipalFromContext(ctx)
	if !ok {
		return ""
	}
	return principal.WorkspaceID()
}

func CSRFTokenFromContext(ctx context.Context) string {
	token, _ := ctx.Value(csrfContextKey).(string)
	return token
}

type LoginData struct {
	Error         string
	Notice        string
	CSRFToken     string
	Username      string
	ReturnTo      string
	Mode          string
	Token         string
	Email         string
	Name          string
	Bootstrap     bool
	SetupRequired bool
	OIDCEnabled   bool
	OIDCURL       string
	NewUser       bool
}

type oidcState struct {
	Nonce     string
	Verifier  string
	Binding   string
	ReturnTo  string
	ExpiresAt time.Time
}

type Auth struct {
	store  Store
	secret string
	tmpl   *template.Template
	opts   Options

	initErr      error
	now          func() time.Time
	random       io.Reader
	httpClient   *http.Client
	trustedNets  []*net.IPNet
	trustedIPs   map[string]struct{}
	throttle     *loginThrottle
	bootstrapMu  sync.Mutex
	oidcMu       sync.Mutex
	oidcProvider *oidcRuntime
	oidcStates   map[string]oidcState
	auditHook    AuditHook
}

func NewWithConfig(db *database.DB, cfg *config.Config, tmpl *template.Template) (*Auth, error) {
	if cfg == nil {
		return nil, ErrNotConfigured
	}
	if cfg.Security.RequirePersistentSecrets && (cfg.SessionSecretGenerated || cfg.APIKeyGenerated) {
		return nil, ErrNotConfigured
	}
	opts := Options{
		SessionSecret:          cfg.SessionSecret,
		SessionTTL:             time.Duration(cfg.Security.SessionTTLSeconds) * time.Second,
		SecureCookies:          cfg.Security.SecureCookies,
		HTTPSMode:              cfg.Security.HTTPSMode,
		HSTS:                   cfg.Security.HSTS,
		HSTSMaxAge:             time.Duration(cfg.Security.HSTSMaxAgeSeconds) * time.Second,
		HSTSIncludeSubdomains:  cfg.Security.HSTSIncludeSubdomains,
		LoginAttemptsPerMinute: cfg.Security.LoginAttemptsPerMinute,
		BaseURL:                cfg.BaseURL,
		TrustedProxies:         cfg.Security.TrustedProxies,
		OIDC:                   cfg.OIDC,
		LegacyAPIKey:           cfg.APIKey,
	}
	return NewWithOptions(db, tmpl, opts)
}

func NewWithOptions(store Store, tmpl *template.Template, opts Options) (*Auth, error) {
	a, err := newAuth(store, tmpl, opts)
	if err != nil {
		return nil, err
	}
	return a, nil
}

func secureProviderURL(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	if parsed.Scheme == "https" {
		return true
	}
	if parsed.Scheme != "http" {
		return false
	}
	host := parsed.Hostname()
	return host == "localhost" || (net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback())
}

func newAuth(store Store, tmpl *template.Template, opts Options) (*Auth, error) {
	if opts.SessionTTL == 0 {
		opts.SessionTTL = 7 * 24 * time.Hour
	}
	if opts.LoginAttemptsPerMinute == 0 {
		opts.LoginAttemptsPerMinute = 10
	}
	if opts.HSTSMaxAge == 0 {
		opts.HSTSMaxAge = 365 * 24 * time.Hour
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: 15 * time.Second}
	}
	a := &Auth{
		store: store, secret: opts.SessionSecret, tmpl: tmpl, opts: opts,
		now: func() time.Time { return time.Now().UTC() }, random: rand.Reader,
		httpClient: opts.HTTPClient, trustedIPs: make(map[string]struct{}),
		oidcStates: make(map[string]oidcState),
	}
	a.throttle = newLoginThrottle(opts.LoginAttemptsPerMinute, time.Minute, a.now)
	a.auditHook = opts.AuditHook
	if a.auditHook == nil && store != nil {
		a.auditHook = store.RecordAuditEvent
	}

	var errs []error
	if store == nil {
		errs = append(errs, errors.New("authentication store is required"))
	}
	if opts.SessionSecret == "" {
		errs = append(errs, errors.New("session secret is required"))
	}
	if opts.SessionTTL < 0 || opts.LoginAttemptsPerMinute < 0 || opts.HSTSMaxAge < 0 {
		errs = append(errs, errors.New("invalid authentication limits"))
	}
	if opts.OIDC.Enabled && !opts.OIDC.Configured() {
		errs = append(errs, errors.New("enabled OIDC requires issuer, client, secret, and redirect URL"))
	}
	if opts.OIDC.Configured() && (!secureProviderURL(opts.OIDC.IssuerURL) || !secureProviderURL(opts.OIDC.RedirectURL)) {
		errs = append(errs, errors.New("OIDC issuer and redirect URLs must use HTTPS except on loopback"))
	}
	for group, role := range opts.OIDC.GroupRoles {
		if strings.TrimSpace(group) == "" || !models.Role(role).Valid() {
			errs = append(errs, errors.New("invalid OIDC group role mapping"))
		}
	}
	if opts.BaseURL != "" {
		parsed, err := url.Parse(opts.BaseURL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			errs = append(errs, errors.New("invalid authentication base URL"))
		}
	}
	for _, proxy := range opts.TrustedProxies {
		proxy = strings.TrimSpace(proxy)
		if proxy == "" {
			continue
		}
		if strings.Contains(proxy, "/") {
			_, network, err := net.ParseCIDR(proxy)
			if err != nil {
				errs = append(errs, errors.New("invalid trusted proxy CIDR"))
				continue
			}
			a.trustedNets = append(a.trustedNets, network)
			continue
		}
		ip := net.ParseIP(proxy)
		if ip == nil {
			errs = append(errs, errors.New("invalid trusted proxy IP"))
			continue
		}
		a.trustedIPs[ip.String()] = struct{}{}
	}
	a.initErr = errors.Join(errs...)
	return a, a.initErr
}
