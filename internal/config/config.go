package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// SaaSConfig holds multi-tenant SaaS mode settings. When Enabled is false,
// flipbook runs in single-tenant admin-only mode (the original behavior).
type SaaSConfig struct {
	Enabled         bool   `yaml:"enabled"`
	JWTSecret       string `yaml:"jwt_access_secret"`
	GracePeriodDays int    `yaml:"grace_period_days"`
	FrontendDir     string `yaml:"frontend_dir"`
}

type SecurityConfig struct {
	HTTPSMode                bool     `yaml:"https_mode"`
	SecureCookies            bool     `yaml:"secure_cookies"`
	HSTS                     bool     `yaml:"hsts"`
	HSTSMaxAgeSeconds        int      `yaml:"hsts_max_age_seconds"`
	HSTSIncludeSubdomains    bool     `yaml:"hsts_include_subdomains"`
	SessionTTLSeconds        int      `yaml:"session_ttl_seconds"`
	LoginAttemptsPerMinute   int      `yaml:"login_attempts_per_minute"`
	RequirePersistentSecrets bool     `yaml:"require_persistent_secrets"`
	TokenEncryptionKey       string   `yaml:"token_encryption_key"`
	TrustedProxies           []string `yaml:"trusted_proxies"`
}

type AnalyticsConfig struct {
	Enabled             bool   `yaml:"enabled"`
	RetentionDays       int    `yaml:"retention_days"`
	ConsentPolicy       string `yaml:"consent_policy"`
	AllowCountry        bool   `yaml:"allow_country"`
	ReaderIDPepper      string `yaml:"reader_id_pepper"`
	IgnoreAdminPreviews bool   `yaml:"ignore_admin_previews"`
}

type ConversionConfig struct {
	WorkerCount            int   `yaml:"worker_count"`
	JobLeaseSeconds        int   `yaml:"job_lease_seconds"`
	JobHeartbeatSeconds    int   `yaml:"job_heartbeat_seconds"`
	MaxJobAttempts         int   `yaml:"max_job_attempts"`
	RetryBackoffSeconds    int   `yaml:"retry_backoff_seconds"`
	MaxRetryBackoffSeconds int   `yaml:"max_retry_backoff_seconds"`
	TimeoutSeconds         int   `yaml:"timeout_seconds"`
	MaxPages               int   `yaml:"max_pages"`
	MaxPagePixels          int64 `yaml:"max_page_pixels"`
	MaxTotalPixels         int64 `yaml:"max_total_pixels"`
	MinDPI                 int   `yaml:"min_dpi"`
	MaxDiskBytes           int64 `yaml:"max_disk_bytes"`
	MaxRemoteDownloadBytes int64 `yaml:"max_remote_download_bytes"`

	// PageTimeoutSeconds is added to TimeoutSeconds for every page, so the
	// time limit scales with document length. Negative disables the scaling.
	PageTimeoutSeconds int `yaml:"page_timeout_seconds"`
	// RenderConcurrency is the number of pages one conversion renders at
	// once; 0 uses the available CPUs, up to 4.
	RenderConcurrency int `yaml:"render_concurrency"`
}

type OIDCConfig struct {
	Enabled      bool              `yaml:"enabled"`
	IssuerURL    string            `yaml:"issuer_url"`
	ClientID     string            `yaml:"client_id"`
	ClientSecret string            `yaml:"client_secret"`
	RedirectURL  string            `yaml:"redirect_url"`
	Scopes       []string          `yaml:"scopes"`
	GroupClaim   string            `yaml:"group_claim"`
	GroupRoles   map[string]string `yaml:"group_roles"`
	RequireMFA   bool              `yaml:"require_mfa"`
}

func (c OIDCConfig) Configured() bool {
	return c.Enabled && c.IssuerURL != "" && c.ClientID != "" && c.ClientSecret != "" && c.RedirectURL != ""
}

type SMTPConfig struct {
	Enabled  bool   `yaml:"enabled"`
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
	From     string `yaml:"from"`
	FromName string `yaml:"from_name"`
	TLSMode  string `yaml:"tls_mode"`
}

func (c SMTPConfig) Configured() bool {
	return c.Enabled && c.Host != "" && c.Port > 0 && c.From != ""
}

type StripeConfig struct {
	Enabled       bool   `yaml:"enabled"`
	SecretKey     string `yaml:"secret_key"`
	WebhookSecret string `yaml:"webhook_secret"`
	SuccessURL    string `yaml:"success_url"`
	CancelURL     string `yaml:"cancel_url"`
}

func (c StripeConfig) Configured() bool {
	return c.Enabled && c.SecretKey != "" && c.WebhookSecret != ""
}

type CRMProviderConfig struct {
	Enabled     bool   `yaml:"enabled"`
	APIKey      string `yaml:"api_key"`
	AccessToken string `yaml:"access_token"`
	InstanceURL string `yaml:"instance_url"`
	ListID      string `yaml:"list_id"`
}

func (c CRMProviderConfig) Configured() bool {
	return c.Enabled && (c.APIKey != "" || c.AccessToken != "")
}

type CRMConfig struct {
	HubSpot    CRMProviderConfig `yaml:"hubspot"`
	Salesforce CRMProviderConfig `yaml:"salesforce"`
	Mailchimp  CRMProviderConfig `yaml:"mailchimp"`
}

type WebhookConfig struct {
	Enabled              bool   `yaml:"enabled"`
	SigningSecret        string `yaml:"signing_secret"`
	TimeoutSeconds       int    `yaml:"timeout_seconds"`
	MaxAttempts          int    `yaml:"max_attempts"`
	RetryBackoffSeconds  int    `yaml:"retry_backoff_seconds"`
	MaxResponseBodyBytes int64  `yaml:"max_response_body_bytes"`
}

type OAuthProviderConfig struct {
	Enabled      bool     `yaml:"enabled"`
	ClientID     string   `yaml:"client_id"`
	ClientSecret string   `yaml:"client_secret"`
	RedirectURL  string   `yaml:"redirect_url"`
	Scopes       []string `yaml:"scopes"`
	APIBaseURL   string   `yaml:"api_base_url"`
}

func (c OAuthProviderConfig) Configured() bool {
	return c.Enabled && c.ClientID != "" && c.ClientSecret != "" && c.RedirectURL != ""
}

type ProviderConfig struct {
	Canva        OAuthProviderConfig `yaml:"canva"`
	AdobeExpress OAuthProviderConfig `yaml:"adobe_express"`
}

type DomainConfig struct {
	Enabled              bool     `yaml:"enabled"`
	AllowedHosts         []string `yaml:"allowed_hosts"`
	DefaultCanonicalHost string   `yaml:"default_canonical_host"`
}

type OCRConfig struct {
	Enabled        bool   `yaml:"enabled"`
	TesseractBin   string `yaml:"tesseract_bin"`
	Language       string `yaml:"language"`
	TimeoutSeconds int    `yaml:"timeout_seconds"`
}

type S3Config struct {
	Enabled         bool   `yaml:"enabled"`
	Endpoint        string `yaml:"endpoint"`
	Region          string `yaml:"region"`
	Bucket          string `yaml:"bucket"`
	AccessKeyID     string `yaml:"access_key_id"`
	SecretAccessKey string `yaml:"secret_access_key"`
	SessionToken    string `yaml:"session_token"`
	PathStyle       bool   `yaml:"path_style"`
	Prefix          string `yaml:"prefix"`
	CDNBaseURL      string `yaml:"cdn_base_url"`
}

func (c S3Config) Configured() bool {
	return c.Enabled && c.Bucket != "" && c.Region != ""
}

type StorageConfig struct {
	Backend string   `yaml:"backend"`
	S3      S3Config `yaml:"s3"`
}

type Config struct {
	Port           string     `yaml:"port"`
	BaseURL        string     `yaml:"base_url"`
	DataDir        string     `yaml:"data_dir"`
	MongoURI       string     `yaml:"mongo_uri"`
	MongoDB        string     `yaml:"mongo_db"`
	LibreOfficeBin string     `yaml:"libreoffice_bin"`
	MaxUploadSize  int64      `yaml:"max_upload_size"`
	ConversionDPI  int        `yaml:"conversion_dpi"`
	ThumbnailDPI   int        `yaml:"thumbnail_dpi"`
	SessionSecret  string     `yaml:"session_secret"`
	APIKey         string     `yaml:"api_key"`
	SaaS           SaaSConfig `yaml:"saas"`

	Security   SecurityConfig   `yaml:"security"`
	Analytics  AnalyticsConfig  `yaml:"analytics"`
	Conversion ConversionConfig `yaml:"conversion"`
	OIDC       OIDCConfig       `yaml:"oidc"`
	SMTP       SMTPConfig       `yaml:"smtp"`
	Stripe     StripeConfig     `yaml:"stripe"`
	CRM        CRMConfig        `yaml:"crm"`
	Webhooks   WebhookConfig    `yaml:"webhooks"`
	Providers  ProviderConfig   `yaml:"providers"`
	Domains    DomainConfig     `yaml:"domains"`
	OCR        OCRConfig        `yaml:"ocr"`
	Storage    StorageConfig    `yaml:"storage"`

	SessionSecretGenerated bool `yaml:"-"`
	APIKeyGenerated        bool `yaml:"-"`
}

func defaultConfig() *Config {
	return &Config{
		Port: "8080", BaseURL: "http://localhost:8080", DataDir: "./data",
		MongoDB: "flipbook", MaxUploadSize: 524288000, ConversionDPI: 300, ThumbnailDPI: 72,
		Security: SecurityConfig{
			HSTSMaxAgeSeconds: 31536000, SessionTTLSeconds: 604800,
			LoginAttemptsPerMinute: 10,
		},
		Analytics: AnalyticsConfig{
			Enabled: true, RetentionDays: 365, ConsentPolicy: "required", IgnoreAdminPreviews: true,
		},
		Conversion: ConversionConfig{
			WorkerCount: 2, JobLeaseSeconds: 60, JobHeartbeatSeconds: 15,
			MaxJobAttempts: 5, RetryBackoffSeconds: 30, MaxRetryBackoffSeconds: 3600,
			TimeoutSeconds: 900, PageTimeoutSeconds: 30, MaxPages: 1000, MaxPagePixels: 50000000,
			MaxTotalPixels: 2000000000, MinDPI: 100,
			MaxDiskBytes: 5368709120, MaxRemoteDownloadBytes: 209715200,
		},
		OIDC:     OIDCConfig{Scopes: []string{"openid", "profile", "email"}, GroupClaim: "groups", GroupRoles: map[string]string{}},
		SMTP:     SMTPConfig{Port: 587, TLSMode: "starttls"},
		Webhooks: WebhookConfig{TimeoutSeconds: 15, MaxAttempts: 8, RetryBackoffSeconds: 30, MaxResponseBodyBytes: 65536},
		Providers: ProviderConfig{
			Canva:        OAuthProviderConfig{APIBaseURL: "https://api.canva.com"},
			AdobeExpress: OAuthProviderConfig{APIBaseURL: "https://cc-api-behance.adobe.io"},
		},
		OCR:     OCRConfig{TesseractBin: "tesseract", Language: "eng", TimeoutSeconds: 60},
		Storage: StorageConfig{Backend: "local"},
		SaaS:    SaaSConfig{GracePeriodDays: 7},
	}
}

// Load preserves the original best-effort configuration contract. New services
// should use LoadWithError so malformed configuration is not silently ignored.
func Load() *Config {
	cfg, _ := load(os.Getenv, os.ReadFile, false)
	return cfg
}

func LoadWithError() (*Config, error) {
	return load(os.Getenv, os.ReadFile, true)
}

func load(getenv func(string) string, readFile func(string) ([]byte, error), strict bool) (*Config, error) {
	cfg := defaultConfig()
	explicitPath := getenv("FLIPBOOK_CONFIG")
	configPaths := []string{explicitPath, "config.dev.yaml", "config.yaml"}
	for _, path := range configPaths {
		if path == "" {
			continue
		}
		data, err := readFile(path)
		if err != nil {
			if strict && path == explicitPath {
				return cfg, fmt.Errorf("read config %s: %w", path, err)
			}
			continue
		}
		if err := yaml.Unmarshal(data, cfg); err != nil && strict {
			return cfg, fmt.Errorf("parse config %s: %w", path, err)
		}
		break
	}

	b := &envBinder{getenv: getenv, strict: strict}
	b.apply(cfg)
	if strict && b.err != nil {
		return cfg, b.err
	}

	// SaaS defaults
	if cfg.SaaS.GracePeriodDays == 0 {
		cfg.SaaS.GracePeriodDays = 7
	}

	if cfg.LibreOfficeBin == "" {
		cfg.LibreOfficeBin = findLibreOffice()
	}
	if cfg.SessionSecret == "" {
		cfg.SessionSecret = randomHex(32)
		cfg.SessionSecretGenerated = true
	}
	if cfg.APIKey == "" {
		cfg.APIKey = randomHex(32)
		cfg.APIKeyGenerated = true
	}
	if cfg.Security.HTTPSMode {
		cfg.Security.SecureCookies = true
		cfg.Security.HSTS = true
	}
	cfg.Analytics.ConsentPolicy = strings.ToLower(strings.TrimSpace(cfg.Analytics.ConsentPolicy))
	cfg.SMTP.TLSMode = strings.ToLower(strings.TrimSpace(cfg.SMTP.TLSMode))
	cfg.Storage.Backend = strings.ToLower(strings.TrimSpace(cfg.Storage.Backend))
	if strict {
		if err := cfg.Validate(); err != nil {
			return cfg, err
		}
	}
	return cfg, nil
}

func (c *Config) Validate() error {
	var errs []error
	if c.Port == "" {
		errs = append(errs, errors.New("port must not be empty"))
	}
	if c.MaxUploadSize <= 0 || c.ConversionDPI <= 0 || c.ThumbnailDPI <= 0 {
		errs = append(errs, errors.New("upload size and conversion DPI values must be positive"))
	}
	if c.Analytics.RetentionDays <= 0 {
		errs = append(errs, errors.New("analytics.retention_days must be positive"))
	}
	switch c.Analytics.ConsentPolicy {
	case "required", "anonymous", "disabled":
	default:
		errs = append(errs, errors.New("analytics.consent_policy must be required, anonymous, or disabled"))
	}
	if c.Conversion.WorkerCount <= 0 || c.Conversion.MaxJobAttempts <= 0 ||
		c.Conversion.JobLeaseSeconds <= 0 || c.Conversion.JobHeartbeatSeconds <= 0 ||
		c.Conversion.JobHeartbeatSeconds >= c.Conversion.JobLeaseSeconds {
		errs = append(errs, errors.New("conversion workers, attempts, and lease timings are invalid"))
	}
	if c.Conversion.MaxPages <= 0 || c.Conversion.MaxPagePixels <= 0 ||
		c.Conversion.MaxTotalPixels <= 0 || c.Conversion.MinDPI <= 0 ||
		c.Conversion.MaxDiskBytes <= 0 || c.Conversion.TimeoutSeconds <= 0 ||
		c.Conversion.MaxRemoteDownloadBytes <= 0 || c.Conversion.RetryBackoffSeconds <= 0 ||
		c.Conversion.MaxRetryBackoffSeconds < c.Conversion.RetryBackoffSeconds {
		errs = append(errs, errors.New("conversion quotas, timeout, and backoff must be positive"))
	}
	if c.Conversion.RenderConcurrency < 0 || c.Conversion.RenderConcurrency > 64 {
		errs = append(errs, errors.New("conversion.render_concurrency must be between 0 (automatic) and 64"))
	}
	if c.Security.SessionTTLSeconds <= 0 || c.Security.LoginAttemptsPerMinute <= 0 || c.Security.HSTSMaxAgeSeconds < 0 {
		errs = append(errs, errors.New("security session, throttling, and HSTS values are invalid"))
	}
	if c.Webhooks.TimeoutSeconds <= 0 || c.Webhooks.MaxAttempts <= 0 ||
		c.Webhooks.RetryBackoffSeconds <= 0 || c.Webhooks.MaxResponseBodyBytes < 0 {
		errs = append(errs, errors.New("webhook timeout, attempts, backoff, and response limit are invalid"))
	}
	if c.OCR.Enabled && (c.OCR.TesseractBin == "" || c.OCR.TimeoutSeconds <= 0 || c.OCR.Language == "") {
		errs = append(errs, errors.New("enabled OCR requires a binary, language, and positive timeout"))
	}
	switch c.SMTP.TLSMode {
	case "none", "starttls", "tls":
	default:
		errs = append(errs, errors.New("smtp.tls_mode must be none, starttls, or tls"))
	}
	switch c.Storage.Backend {
	case "local":
	case "s3":
		if !c.Storage.S3.Configured() {
			errs = append(errs, errors.New("storage backend s3 requires enabled s3 bucket and region settings"))
		}
	default:
		errs = append(errs, errors.New("storage.backend must be local or s3"))
	}
	if c.Security.RequirePersistentSecrets && (c.SessionSecretGenerated || c.APIKeyGenerated) {
		errs = append(errs, errors.New("persistent session_secret and api_key are required"))
	}
	return errors.Join(errs...)
}

func randomHex(size int) string {
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		panic(fmt.Sprintf("secure random generation failed: %v", err))
	}
	return hex.EncodeToString(buf)
}

type envBinder struct {
	getenv func(string) string
	strict bool
	err    error
}

func (b *envBinder) add(key string, fn func(string) error) {
	value := b.getenv(key)
	if value == "" {
		return
	}
	if err := fn(value); err != nil && b.strict {
		b.err = errors.Join(b.err, fmt.Errorf("%s: %w", key, err))
	}
}

func (b *envBinder) string(key string, target *string) {
	b.add(key, func(v string) error { *target = v; return nil })
}

func (b *envBinder) bool(key string, target *bool) {
	b.add(key, func(v string) error {
		n, err := strconv.ParseBool(v)
		if err == nil {
			*target = n
		}
		return err
	})
}

func (b *envBinder) int(key string, target *int) {
	b.add(key, func(v string) error {
		n, err := strconv.Atoi(v)
		if err == nil {
			*target = n
		}
		return err
	})
}

func (b *envBinder) int64(key string, target *int64) {
	b.add(key, func(v string) error {
		n, err := strconv.ParseInt(v, 10, 64)
		if err == nil {
			*target = n
		}
		return err
	})
}

func (b *envBinder) list(key string, target *[]string) {
	b.add(key, func(v string) error {
		parts := strings.Split(v, ",")
		values := make([]string, 0, len(parts))
		for _, part := range parts {
			if part = strings.TrimSpace(part); part != "" {
				values = append(values, part)
			}
		}
		*target = values
		return nil
	})
}

func (b *envBinder) apply(c *Config) {
	b.string("FLIPBOOK_PORT", &c.Port)
	b.string("FLIPBOOK_BASE_URL", &c.BaseURL)
	b.string("FLIPBOOK_DATA_DIR", &c.DataDir)
	b.string("FLIPBOOK_MONGO_URI", &c.MongoURI)
	b.string("FLIPBOOK_MONGO_DB", &c.MongoDB)
	b.string("FLIPBOOK_LIBREOFFICE_BIN", &c.LibreOfficeBin)
	b.int64("FLIPBOOK_MAX_UPLOAD_SIZE", &c.MaxUploadSize)
	b.int("FLIPBOOK_CONVERSION_DPI", &c.ConversionDPI)
	b.int("FLIPBOOK_THUMBNAIL_DPI", &c.ThumbnailDPI)
	b.string("FLIPBOOK_SESSION_SECRET", &c.SessionSecret)
	b.string("FLIPBOOK_API_KEY", &c.APIKey)

	b.bool("FLIPBOOK_SAAS_ENABLED", &c.SaaS.Enabled)
	b.string("FLIPBOOK_JWT_ACCESS_SECRET", &c.SaaS.JWTSecret)
	b.int("FLIPBOOK_GRACE_PERIOD_DAYS", &c.SaaS.GracePeriodDays)
	b.string("FLIPBOOK_FRONTEND_DIR", &c.SaaS.FrontendDir)

	b.bool("FLIPBOOK_SECURITY_HTTPS_MODE", &c.Security.HTTPSMode)
	b.bool("FLIPBOOK_SECURITY_SECURE_COOKIES", &c.Security.SecureCookies)
	b.bool("FLIPBOOK_SECURITY_HSTS", &c.Security.HSTS)
	b.int("FLIPBOOK_SECURITY_HSTS_MAX_AGE_SECONDS", &c.Security.HSTSMaxAgeSeconds)
	b.bool("FLIPBOOK_SECURITY_HSTS_INCLUDE_SUBDOMAINS", &c.Security.HSTSIncludeSubdomains)
	b.int("FLIPBOOK_SECURITY_SESSION_TTL_SECONDS", &c.Security.SessionTTLSeconds)
	b.int("FLIPBOOK_SECURITY_LOGIN_ATTEMPTS_PER_MINUTE", &c.Security.LoginAttemptsPerMinute)
	b.bool("FLIPBOOK_SECURITY_REQUIRE_PERSISTENT_SECRETS", &c.Security.RequirePersistentSecrets)
	b.string("FLIPBOOK_SECURITY_TOKEN_ENCRYPTION_KEY", &c.Security.TokenEncryptionKey)
	b.list("FLIPBOOK_SECURITY_TRUSTED_PROXIES", &c.Security.TrustedProxies)

	b.bool("FLIPBOOK_ANALYTICS_ENABLED", &c.Analytics.Enabled)
	b.int("FLIPBOOK_ANALYTICS_RETENTION_DAYS", &c.Analytics.RetentionDays)
	b.string("FLIPBOOK_ANALYTICS_CONSENT_POLICY", &c.Analytics.ConsentPolicy)
	b.bool("FLIPBOOK_ANALYTICS_ALLOW_COUNTRY", &c.Analytics.AllowCountry)
	b.string("FLIPBOOK_ANALYTICS_READER_ID_PEPPER", &c.Analytics.ReaderIDPepper)
	b.bool("FLIPBOOK_ANALYTICS_IGNORE_ADMIN_PREVIEWS", &c.Analytics.IgnoreAdminPreviews)

	b.int("FLIPBOOK_CONVERSION_WORKER_COUNT", &c.Conversion.WorkerCount)
	b.int("FLIPBOOK_CONVERSION_JOB_LEASE_SECONDS", &c.Conversion.JobLeaseSeconds)
	b.int("FLIPBOOK_CONVERSION_JOB_HEARTBEAT_SECONDS", &c.Conversion.JobHeartbeatSeconds)
	b.int("FLIPBOOK_CONVERSION_MAX_JOB_ATTEMPTS", &c.Conversion.MaxJobAttempts)
	b.int("FLIPBOOK_CONVERSION_RETRY_BACKOFF_SECONDS", &c.Conversion.RetryBackoffSeconds)
	b.int("FLIPBOOK_CONVERSION_MAX_RETRY_BACKOFF_SECONDS", &c.Conversion.MaxRetryBackoffSeconds)
	b.int("FLIPBOOK_CONVERSION_TIMEOUT_SECONDS", &c.Conversion.TimeoutSeconds)
	b.int("FLIPBOOK_CONVERSION_PAGE_TIMEOUT_SECONDS", &c.Conversion.PageTimeoutSeconds)
	b.int("FLIPBOOK_CONVERSION_RENDER_CONCURRENCY", &c.Conversion.RenderConcurrency)
	b.int("FLIPBOOK_CONVERSION_MAX_PAGES", &c.Conversion.MaxPages)
	b.int64("FLIPBOOK_CONVERSION_MAX_PAGE_PIXELS", &c.Conversion.MaxPagePixels)
	b.int64("FLIPBOOK_CONVERSION_MAX_TOTAL_PIXELS", &c.Conversion.MaxTotalPixels)
	b.int("FLIPBOOK_CONVERSION_MIN_DPI", &c.Conversion.MinDPI)
	b.int64("FLIPBOOK_CONVERSION_MAX_DISK_BYTES", &c.Conversion.MaxDiskBytes)
	b.int64("FLIPBOOK_CONVERSION_MAX_REMOTE_DOWNLOAD_BYTES", &c.Conversion.MaxRemoteDownloadBytes)

	b.bool("FLIPBOOK_OIDC_ENABLED", &c.OIDC.Enabled)
	b.string("FLIPBOOK_OIDC_ISSUER_URL", &c.OIDC.IssuerURL)
	b.string("FLIPBOOK_OIDC_CLIENT_ID", &c.OIDC.ClientID)
	b.string("FLIPBOOK_OIDC_CLIENT_SECRET", &c.OIDC.ClientSecret)
	b.string("FLIPBOOK_OIDC_REDIRECT_URL", &c.OIDC.RedirectURL)
	b.list("FLIPBOOK_OIDC_SCOPES", &c.OIDC.Scopes)
	b.string("FLIPBOOK_OIDC_GROUP_CLAIM", &c.OIDC.GroupClaim)
	b.bool("FLIPBOOK_OIDC_REQUIRE_MFA", &c.OIDC.RequireMFA)

	b.bool("FLIPBOOK_SMTP_ENABLED", &c.SMTP.Enabled)
	b.string("FLIPBOOK_SMTP_HOST", &c.SMTP.Host)
	b.int("FLIPBOOK_SMTP_PORT", &c.SMTP.Port)
	b.string("FLIPBOOK_SMTP_USERNAME", &c.SMTP.Username)
	b.string("FLIPBOOK_SMTP_PASSWORD", &c.SMTP.Password)
	b.string("FLIPBOOK_SMTP_FROM", &c.SMTP.From)
	b.string("FLIPBOOK_SMTP_FROM_NAME", &c.SMTP.FromName)
	b.string("FLIPBOOK_SMTP_TLS_MODE", &c.SMTP.TLSMode)

	b.bool("FLIPBOOK_STRIPE_ENABLED", &c.Stripe.Enabled)
	b.string("FLIPBOOK_STRIPE_SECRET_KEY", &c.Stripe.SecretKey)
	b.string("FLIPBOOK_STRIPE_WEBHOOK_SECRET", &c.Stripe.WebhookSecret)
	b.string("FLIPBOOK_STRIPE_SUCCESS_URL", &c.Stripe.SuccessURL)
	b.string("FLIPBOOK_STRIPE_CANCEL_URL", &c.Stripe.CancelURL)

	applyCRMEnv := func(prefix string, c *CRMProviderConfig) {
		b.bool(prefix+"_ENABLED", &c.Enabled)
		b.string(prefix+"_API_KEY", &c.APIKey)
		b.string(prefix+"_ACCESS_TOKEN", &c.AccessToken)
		b.string(prefix+"_INSTANCE_URL", &c.InstanceURL)
		b.string(prefix+"_LIST_ID", &c.ListID)
	}
	applyCRMEnv("FLIPBOOK_CRM_HUBSPOT", &c.CRM.HubSpot)
	applyCRMEnv("FLIPBOOK_CRM_SALESFORCE", &c.CRM.Salesforce)
	applyCRMEnv("FLIPBOOK_CRM_MAILCHIMP", &c.CRM.Mailchimp)

	b.bool("FLIPBOOK_WEBHOOKS_ENABLED", &c.Webhooks.Enabled)
	b.string("FLIPBOOK_WEBHOOKS_SIGNING_SECRET", &c.Webhooks.SigningSecret)
	b.int("FLIPBOOK_WEBHOOKS_TIMEOUT_SECONDS", &c.Webhooks.TimeoutSeconds)
	b.int("FLIPBOOK_WEBHOOKS_MAX_ATTEMPTS", &c.Webhooks.MaxAttempts)
	b.int("FLIPBOOK_WEBHOOKS_RETRY_BACKOFF_SECONDS", &c.Webhooks.RetryBackoffSeconds)
	b.int64("FLIPBOOK_WEBHOOKS_MAX_RESPONSE_BODY_BYTES", &c.Webhooks.MaxResponseBodyBytes)

	applyOAuthEnv := func(prefix string, c *OAuthProviderConfig) {
		b.bool(prefix+"_ENABLED", &c.Enabled)
		b.string(prefix+"_CLIENT_ID", &c.ClientID)
		b.string(prefix+"_CLIENT_SECRET", &c.ClientSecret)
		b.string(prefix+"_REDIRECT_URL", &c.RedirectURL)
		b.list(prefix+"_SCOPES", &c.Scopes)
		b.string(prefix+"_API_BASE_URL", &c.APIBaseURL)
	}
	applyOAuthEnv("FLIPBOOK_PROVIDERS_CANVA", &c.Providers.Canva)
	applyOAuthEnv("FLIPBOOK_PROVIDERS_ADOBE_EXPRESS", &c.Providers.AdobeExpress)

	b.bool("FLIPBOOK_DOMAINS_ENABLED", &c.Domains.Enabled)
	b.list("FLIPBOOK_DOMAINS_ALLOWED_HOSTS", &c.Domains.AllowedHosts)
	b.string("FLIPBOOK_DOMAINS_DEFAULT_CANONICAL_HOST", &c.Domains.DefaultCanonicalHost)

	b.bool("FLIPBOOK_OCR_ENABLED", &c.OCR.Enabled)
	b.string("FLIPBOOK_OCR_TESSERACT_BIN", &c.OCR.TesseractBin)
	b.string("FLIPBOOK_OCR_LANGUAGE", &c.OCR.Language)
	b.int("FLIPBOOK_OCR_TIMEOUT_SECONDS", &c.OCR.TimeoutSeconds)

	b.string("FLIPBOOK_STORAGE_BACKEND", &c.Storage.Backend)
	b.bool("FLIPBOOK_STORAGE_S3_ENABLED", &c.Storage.S3.Enabled)
	b.string("FLIPBOOK_STORAGE_S3_ENDPOINT", &c.Storage.S3.Endpoint)
	b.string("FLIPBOOK_STORAGE_S3_REGION", &c.Storage.S3.Region)
	b.string("FLIPBOOK_STORAGE_S3_BUCKET", &c.Storage.S3.Bucket)
	b.string("FLIPBOOK_STORAGE_S3_ACCESS_KEY_ID", &c.Storage.S3.AccessKeyID)
	b.string("FLIPBOOK_STORAGE_S3_SECRET_ACCESS_KEY", &c.Storage.S3.SecretAccessKey)
	b.string("FLIPBOOK_STORAGE_S3_SESSION_TOKEN", &c.Storage.S3.SessionToken)
	b.bool("FLIPBOOK_STORAGE_S3_PATH_STYLE", &c.Storage.S3.PathStyle)
	b.string("FLIPBOOK_STORAGE_S3_PREFIX", &c.Storage.S3.Prefix)
	b.string("FLIPBOOK_STORAGE_S3_CDN_BASE_URL", &c.Storage.S3.CDNBaseURL)
}

func findLibreOffice() string {
	paths := []string{
		"/Applications/LibreOffice.app/Contents/MacOS/soffice", "/usr/bin/soffice",
		"/usr/local/bin/soffice", "/snap/bin/soffice",
	}
	for _, path := range paths {
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return "soffice"
}
