package integrations

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/metavert-io/flipbook/internal/config"
	"github.com/metavert-io/flipbook/internal/models"
	"github.com/metavert-io/flipbook/internal/storage"
	"github.com/metavert-io/flipbook/internal/worker"
)

const (
	ProviderCanva        = "canva"
	ProviderAdobeExpress = "adobe_express"
)

var (
	ErrNotConfigured = errors.New("integration is not configured")
	ErrNotConnected  = errors.New("integration is not connected")
	ErrPermission    = errors.New("integration permission denied")
	ErrInvalidState  = errors.New("integration OAuth state is invalid or expired")
	ErrInvalidInput  = errors.New("integration input is invalid")
	ErrNotFound      = errors.New("integration resource was not found")
	ErrConflict      = errors.New("integration resource conflict")
	ErrRateLimited   = errors.New("integration provider rate limit reached")
	ErrProvider      = errors.New("integration provider request failed")
)

type Error struct {
	Kind       error
	Provider   string
	Operation  string
	StatusCode int
	Message    string
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Message != "" {
		return e.Message
	}
	if e.Kind != nil {
		return e.Kind.Error()
	}
	return ErrProvider.Error()
}

func (e *Error) Unwrap() error { return e.Kind }

func integrationError(kind error, provider, operation, message string) error {
	return &Error{Kind: kind, Provider: provider, Operation: operation, Message: message}
}

func HTTPStatus(err error) int {
	switch {
	case err == nil:
		return http.StatusOK
	case errors.Is(err, ErrNotConfigured):
		return http.StatusServiceUnavailable
	case errors.Is(err, ErrPermission):
		return http.StatusForbidden
	case errors.Is(err, ErrNotConnected), errors.Is(err, ErrConflict):
		return http.StatusConflict
	case errors.Is(err, ErrInvalidState):
		return http.StatusBadRequest
	case errors.Is(err, ErrInvalidInput):
		return http.StatusUnprocessableEntity
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, ErrRateLimited):
		return http.StatusTooManyRequests
	case errors.Is(err, ErrProvider):
		return http.StatusBadGateway
	default:
		return http.StatusInternalServerError
	}
}

func ErrorCode(err error) string {
	switch {
	case errors.Is(err, ErrNotConfigured):
		return "not_configured"
	case errors.Is(err, ErrNotConnected):
		return "not_connected"
	case errors.Is(err, ErrPermission):
		return "permission_denied"
	case errors.Is(err, ErrInvalidState):
		return "invalid_oauth_state"
	case errors.Is(err, ErrInvalidInput):
		return "invalid_input"
	case errors.Is(err, ErrNotFound):
		return "not_found"
	case errors.Is(err, ErrConflict):
		return "conflict"
	case errors.Is(err, ErrRateLimited):
		return "rate_limited"
	case errors.Is(err, ErrProvider):
		return "provider_error"
	default:
		return "internal_error"
	}
}

func PublicMessage(err error) string {
	if err == nil {
		return ""
	}
	var integrationErr *Error
	if errors.As(err, &integrationErr) && integrationErr.Message != "" {
		return integrationErr.Message
	}
	switch {
	case errors.Is(err, ErrNotConfigured):
		return "This integration is not configured. Ask an administrator to complete the provider and encryption settings."
	case errors.Is(err, ErrNotConnected):
		return "The provider connection is unavailable or expired. Reconnect it and try again."
	case errors.Is(err, ErrPermission):
		return "The provider denied access. Check app approval, OAuth scopes, and document sharing."
	case errors.Is(err, ErrInvalidState):
		return "The authorization session is invalid or expired. Start the connection again."
	case errors.Is(err, ErrInvalidInput):
		return "Check the integration input and try again."
	case errors.Is(err, ErrNotFound):
		return "The integration resource was not found."
	case errors.Is(err, ErrRateLimited):
		return "The provider rate limit was reached. Wait before trying again."
	case errors.Is(err, ErrProvider):
		return "The provider could not complete the request. Try again later."
	default:
		return "The integration operation could not be completed."
	}
}

type ProviderConfig struct {
	Enabled          bool
	ClientID         string
	ClientSecret     string
	RedirectURL      string
	Scopes           []string
	AuthorizationURL string
	TokenURL         string
	RevocationURL    string
	APIBaseURL       string
	VerifyPath       string
	ExportPath       string
	ExportStatusPath string
	StatisticsPath   string
}

func (c ProviderConfig) Configured() bool {
	return c.Enabled && strings.TrimSpace(c.ClientID) != "" && strings.TrimSpace(c.ClientSecret) != "" &&
		strings.TrimSpace(c.RedirectURL) != "" && strings.TrimSpace(c.AuthorizationURL) != "" &&
		strings.TrimSpace(c.TokenURL) != "" && strings.TrimSpace(c.APIBaseURL) != ""
}

type ProviderInfo struct {
	Name                 string `json:"name"`
	DisplayName          string `json:"display_name"`
	Configured           bool   `json:"configured"`
	ConfigurationMessage string `json:"configuration_message,omitempty"`
	ApprovalNote         string `json:"approval_note,omitempty"`
	SupportsStatistics   bool   `json:"supports_statistics"`
}

type Options struct {
	Providers          map[string]ProviderConfig
	EncryptionKey      string
	HTTPClient         *http.Client
	DownloadClient     *http.Client
	Now                func() time.Time
	Random             io.Reader
	SecureCookies      bool
	MaxDownloadBytes   int64
	ExportPollInterval time.Duration
	ExportPollAttempts int
	WebhookMaxAttempts int
}

type Store interface {
	CreateIntegrationConnection(ctx context.Context, connection *models.IntegrationConnection) error
	UpdateIntegrationConnection(ctx context.Context, connection *models.IntegrationConnection) error
	ListIntegrationConnections(ctx context.Context, workspaceID, userID string) ([]*models.IntegrationConnection, error)
	DisconnectIntegrationConnection(ctx context.Context, workspaceID, connectionID string) error

	GetWorkspace(ctx context.Context, workspaceID string) (*models.Workspace, error)
	GetFlipbookByExternalID(ctx context.Context, workspaceID, source, externalID string) (*models.Flipbook, error)
	CreateFlipbookContext(ctx context.Context, flipbook *models.Flipbook) error
	CreateRevision(ctx context.Context, revision *models.Revision) error
	GenerateUniqueSlug(ctx context.Context, workspaceID, slug string) (string, error)

	CreateWebhookSubscription(ctx context.Context, subscription *models.WebhookSubscription) error
	ListWebhookSubscriptions(ctx context.Context, workspaceID, eventType string, enabledOnly bool) ([]*models.WebhookSubscription, error)
	SetWebhookSubscriptionEnabled(ctx context.Context, workspaceID, subscriptionID string, enabled bool) error
	EnqueueWebhookDelivery(ctx context.Context, delivery *models.WebhookDelivery) error
	ListWebhookDeliveries(ctx context.Context, workspaceID, subscriptionID string, limit int64) ([]*models.WebhookDelivery, error)
}

type AssetStore interface {
	CreateRevisionStaging(id string) (string, error)
	SaveStagedUpload(ctx context.Context, id, stagingDir, filename string, reader io.Reader, maxBytes int64) (storage.UploadInfo, error)
	DeleteFlipbookContext(ctx context.Context, id string) error
}

type ConversionQueue interface {
	EnqueueContext(ctx context.Context, job worker.Job) (*models.Job, error)
}

type OAuthRequest struct {
	Provider    string
	WorkspaceID string
	UserID      string
}

type OAuthSession struct {
	AuthorizationURL string
	Binding          string
	ExpiresAt        time.Time
}

type OAuthCallback struct {
	Provider    string
	WorkspaceID string
	UserID      string
	State       string
	Code        string
	Binding     string
	Error       string
}

type DisconnectResult struct {
	Disconnected    bool   `json:"disconnected"`
	ProviderRevoked bool   `json:"provider_revoked"`
	Warning         string `json:"warning,omitempty"`
}

type ExportRequest struct {
	Provider    string
	WorkspaceID string
	UserID      string
	ExternalID  string
	Title       string
	Pages       string
}

type ExportResult struct {
	Provider   string   `json:"provider"`
	ExternalID string   `json:"external_id"`
	FlipbookID string   `json:"flipbook_id"`
	RevisionID string   `json:"revision_id"`
	JobID      string   `json:"job_id"`
	AdminURL   string   `json:"admin_url"`
	Status     string   `json:"status"`
	Warnings   []string `json:"warnings,omitempty"`
}

type Statistics struct {
	Provider    string         `json:"provider"`
	ExternalID  string         `json:"external_id"`
	Title       string         `json:"title,omitempty"`
	PageCount   int64          `json:"page_count,omitempty"`
	CreatedAt   string         `json:"created_at,omitempty"`
	UpdatedAt   string         `json:"updated_at,omitempty"`
	Data        map[string]any `json:"data"`
	RetrievedAt time.Time      `json:"retrieved_at"`
}

type WebhookSubscriptionResult struct {
	Subscription *models.WebhookSubscription `json:"subscription"`
	Secret       string                      `json:"secret"`
}

type WebhookEvent struct {
	ID          string         `json:"id"`
	Type        string         `json:"type"`
	WorkspaceID string         `json:"workspace_id"`
	CreatedAt   time.Time      `json:"created_at"`
	Data        map[string]any `json:"data"`
}

func DefaultProviderConfigs() map[string]ProviderConfig {
	return map[string]ProviderConfig{
		ProviderCanva: {
			AuthorizationURL: "https://www.canva.com/api/oauth/authorize",
			TokenURL:         "https://api.canva.com/rest/v1/oauth/token",
			RevocationURL:    "https://api.canva.com/rest/v1/oauth/revoke",
			APIBaseURL:       "https://api.canva.com/rest/v1",
			VerifyPath:       "/designs?limit=1",
			ExportPath:       "/exports",
			ExportStatusPath: "/exports/{jobID}",
			StatisticsPath:   "/designs/{externalID}",
			Scopes:           []string{"design:content:read", "design:meta:read"},
		},
		ProviderAdobeExpress: {
			AuthorizationURL: "https://ims-na1.adobelogin.com/ims/authorize/v2",
			TokenURL:         "https://ims-na1.adobelogin.com/ims/token/v3",
			APIBaseURL:       "https://express-api.adobe.io",
			VerifyPath:       "/beta/tagged-documents?limit=1",
			ExportPath:       "/beta/export-rendition",
			ExportStatusPath: "/status/{jobID}",
			StatisticsPath:   "/beta/tagged-documents/{externalID}",
		},
	}
}

func OptionsFromConfig(cfg *config.Config) Options {
	options := Options{Providers: DefaultProviderConfigs()}
	if cfg == nil {
		return options
	}
	options.EncryptionKey = cfg.Security.TokenEncryptionKey
	options.SecureCookies = cfg.Security.SecureCookies || cfg.Security.HTTPSMode
	options.MaxDownloadBytes = cfg.Conversion.MaxRemoteDownloadBytes
	options.WebhookMaxAttempts = cfg.Webhooks.MaxAttempts
	applyOAuth := func(name string, oauth config.OAuthProviderConfig) {
		provider := options.Providers[name]
		provider.Enabled = oauth.Enabled
		provider.ClientID = oauth.ClientID
		provider.ClientSecret = oauth.ClientSecret
		provider.RedirectURL = oauth.RedirectURL
		if oauth.Scopes != nil {
			provider.Scopes = append([]string(nil), oauth.Scopes...)
		}
		base := strings.TrimRight(strings.TrimSpace(oauth.APIBaseURL), "/")
		switch name {
		case ProviderCanva:
			if base == "" || base == "https://api.canva.com" {
				base = provider.APIBaseURL
			}
		case ProviderAdobeExpress:
			if base == "" || base == "https://cc-api-behance.adobe.io" {
				base = provider.APIBaseURL
			}
		}
		if base != "" {
			provider.APIBaseURL = base
		}
		options.Providers[name] = provider
	}
	applyOAuth(ProviderCanva, cfg.Providers.Canva)
	applyOAuth(ProviderAdobeExpress, cfg.Providers.AdobeExpress)
	return options
}
