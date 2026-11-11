package publishing

import (
	"net/http"
	"time"

	"github.com/metavert-io/flipbook/internal/models"
)

type Resource string

const (
	ResourceViewer           Resource = "viewer"
	ResourceEmbed            Resource = "embed"
	ResourceStatus           Resource = "status"
	ResourcePage             Resource = "page"
	ResourceThumbnail        Resource = "thumbnail"
	ResourceRendition        Resource = "rendition"
	ResourceText             Resource = "text"
	ResourceManifest         Resource = "manifest"
	ResourceLinks            Resource = "links"
	ResourceDownloadOriginal Resource = "download_original"
	ResourceDownloadPDF      Resource = "download_pdf"
	ResourcePrint            Resource = "print"
	ResourceShare            Resource = "share"
)

type AssetKind string

const (
	AssetPage      AssetKind = "page"
	AssetThumbnail AssetKind = "thumbnail"
	AssetRendition AssetKind = "rendition"
	AssetText      AssetKind = "text"
	AssetManifest  AssetKind = "manifest"
	AssetLinks     AssetKind = "links"
)

type DownloadKind string

const (
	DownloadOriginal DownloadKind = "original"
	DownloadPDF      DownloadKind = "pdf"
	DownloadPrint    DownloadKind = "print"
)

type ShareChannel string

const (
	ShareAny      ShareChannel = ""
	ShareNative   ShareChannel = "native"
	ShareEmail    ShareChannel = "email"
	ShareFacebook ShareChannel = "facebook"
	ShareLinkedIn ShareChannel = "linkedin"
	ShareX        ShareChannel = "x"
)

const (
	CachePublicImmutable = "public, max-age=31536000, immutable"
	CachePublicReload    = "public, max-age=60, must-revalidate"
	CachePrivate         = "private, max-age=0, must-revalidate"
	CacheNoStore         = "no-store"
)

type Reader struct {
	UserID    string
	EmailHash string
}

type Actor struct {
	UserID string
}

type AccessRequest struct {
	Reader     Reader
	Grant      string
	Password   string
	ShareToken string
	Page       int
	Revision   int
	EmbedHost  string
	ViaEmbed   bool
}

type Principal struct {
	UserID      string
	WorkspaceID string
	Role        models.Role
	Member      bool
}

type AccessEvidence struct {
	Password      bool
	ShareToken    bool
	LeadCaptured  bool
	LeadDismissed bool
	Entitled      bool
}

type PolicyInput struct {
	Flipbook     models.Flipbook
	Principal    Principal
	Evidence     AccessEvidence
	Resource     Resource
	Page         int
	PageCount    int
	Revision     int
	Now          time.Time
	EmbedHost    string
	ViaEmbed     bool
	ShareChannel ShareChannel
}

type Capabilities struct {
	Embed            bool `json:"embed"`
	Share            bool `json:"share"`
	DownloadOriginal bool `json:"download_original"`
	DownloadPDF      bool `json:"download_pdf"`
	Print            bool `json:"print"`
}

type Decision struct {
	Allowed             bool         `json:"allowed"`
	Status              int          `json:"status,omitempty"`
	Reason              ErrorCode    `json:"reason,omitempty"`
	CacheControl        string       `json:"cache_control"`
	NoIndex             bool         `json:"no_index"`
	FrameAncestors      string       `json:"frame_ancestors,omitempty"`
	FullAccess          bool         `json:"full_access"`
	PreviewPages        int          `json:"preview_pages"`
	LeadGateRequired    bool         `json:"lead_gate_required"`
	LeadGateDismissible bool         `json:"lead_gate_dismissible"`
	PurchaseRequired    bool         `json:"purchase_required"`
	Preview             bool         `json:"preview"`
	Capabilities        Capabilities `json:"capabilities"`
}

func (d Decision) Headers() http.Header {
	headers := make(http.Header)
	cache := d.CacheControl
	if cache == "" {
		cache = CacheNoStore
	}
	headers.Set("Cache-Control", cache)
	if d.NoIndex {
		headers.Set("X-Robots-Tag", "noindex, nofollow, noarchive")
	}
	if d.FrameAncestors != "" {
		// Callers must also drop the global X-Frame-Options: SAMEORIGIN so no
		// browser is left to resolve the conflict (see handlers.allowFraming).
		headers.Set("Content-Security-Policy", "frame-ancestors "+d.FrameAncestors)
	}
	if cache != CachePublicImmutable && cache != CachePublicReload {
		headers.Set("Vary", "Cookie, Authorization")
	}
	return headers
}

type Authorization struct {
	Flipbook  models.Flipbook  `json:"flipbook"`
	Revision  *models.Revision `json:"revision,omitempty"`
	Resource  Resource         `json:"resource"`
	Decision  Decision         `json:"decision"`
	AssetName string           `json:"asset_name,omitempty"`
	AssetURL  string           `json:"asset_url,omitempty"`
}

type GrantResult struct {
	Token     string
	Cookie    *http.Cookie
	ExpiresAt time.Time
}

type ShareTokenResult struct {
	ID        string     `json:"id"`
	Token     string     `json:"token"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

type PublishOptions struct {
	Visibility models.Visibility
	PublishAt  *time.Time
	ExpiresAt  *time.Time
}

type LifecycleFailure struct {
	WorkspaceID string `json:"workspace_id"`
	FlipbookID  string `json:"flipbook_id"`
	Err         error  `json:"-"`
}

type ScheduleReport struct {
	Published   int                `json:"published"`
	Unpublished int                `json:"unpublished"`
	Failed      []LifecycleFailure `json:"-"`
}

type PurgeReport struct {
	Purged int                `json:"purged"`
	Failed []LifecycleFailure `json:"-"`
}

type ActivationResult struct {
	Flipbook models.Flipbook `json:"flipbook"`
	Revision models.Revision `json:"revision"`
	Rollback bool            `json:"rollback"`
}
