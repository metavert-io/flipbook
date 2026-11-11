package models

import (
	"strings"
	"time"
)

const (
	StatusPending      = "pending"
	StatusConverting   = "converting"
	StatusReady        = "ready"
	StatusError        = "error"
	StatusRegenerating = "regenerating"
)

type PublicationState string

const (
	PublicationDraft       PublicationState = "draft"
	PublicationPublished   PublicationState = "published"
	PublicationUnpublished PublicationState = "unpublished"
)

type Visibility string

const (
	VisibilityPublic   Visibility = "public"
	VisibilityUnlisted Visibility = "unlisted"
	VisibilityPrivate  Visibility = "private"
	VisibilityTeam     Visibility = "team"
	VisibilityPassword Visibility = "password"
	VisibilityToken    Visibility = "token"
)

type ReviewState string

const (
	ReviewNotRequired ReviewState = "not_required"
	ReviewPending     ReviewState = "pending"
	ReviewApproved    ReviewState = "approved"
	ReviewRejected    ReviewState = "rejected"
)

type SEOMetadata struct {
	Title             string   `bson:"title,omitempty" json:"title,omitempty"`
	Description       string   `bson:"description,omitempty" json:"description,omitempty"`
	CanonicalURL      string   `bson:"canonical_url,omitempty" json:"canonical_url,omitempty"`
	CoverImage        string   `bson:"cover_image,omitempty" json:"cover_image,omitempty"`
	SocialTitle       string   `bson:"social_title,omitempty" json:"social_title,omitempty"`
	SocialDescription string   `bson:"social_description,omitempty" json:"social_description,omitempty"`
	SocialImage       string   `bson:"social_image,omitempty" json:"social_image,omitempty"`
	Keywords          []string `bson:"keywords,omitempty" json:"keywords,omitempty"`
	NoIndex           bool     `bson:"no_index" json:"no_index"`
}

type BrandingSettings struct {
	LogoURL         string `bson:"logo_url,omitempty" json:"logo_url,omitempty"`
	FaviconURL      string `bson:"favicon_url,omitempty" json:"favicon_url,omitempty"`
	PrimaryColor    string `bson:"primary_color,omitempty" json:"primary_color,omitempty"`
	BackgroundColor string `bson:"background_color,omitempty" json:"background_color,omitempty"`
	Attribution     string `bson:"attribution,omitempty" json:"attribution,omitempty"`
	PublisherName   string `bson:"publisher_name,omitempty" json:"publisher_name,omitempty"`
	PublisherURL    string `bson:"publisher_url,omitempty" json:"publisher_url,omitempty"`
	HideBranding    bool   `bson:"hide_branding" json:"hide_branding"`
}

type DownloadSettings struct {
	Enabled       bool `bson:"enabled" json:"enabled"`
	AllowOriginal bool `bson:"allow_original" json:"allow_original"`
	AllowPDF      bool `bson:"allow_pdf" json:"allow_pdf"`
	AllowPrint    bool `bson:"allow_print" json:"allow_print"`
}

type EmbedSettings struct {
	Enabled        bool     `bson:"enabled" json:"enabled"`
	AllowedDomains []string `bson:"allowed_domains,omitempty" json:"allowed_domains,omitempty"`
	StartPage      int      `bson:"start_page,omitempty" json:"start_page,omitempty"`
	ShowToolbar    bool     `bson:"show_toolbar" json:"show_toolbar"`
	ShowSearch     bool     `bson:"show_search" json:"show_search"`
	ShowShare      bool     `bson:"show_share" json:"show_share"`
	ShowDownload   bool     `bson:"show_download" json:"show_download"`
	ReadingMode    string   `bson:"reading_mode,omitempty" json:"reading_mode,omitempty"`
	Background     string   `bson:"background,omitempty" json:"background,omitempty"`
}

// DefaultEmbedSettings is what new workspaces and flipbooks start with.
// Embedding is on so a copied embed code works for anonymous readers on other
// sites; an empty AllowedDomains lets any site frame the publication.
func DefaultEmbedSettings() EmbedSettings {
	return EmbedSettings{Enabled: true, StartPage: 1, ShowToolbar: true, ShowSearch: true}
}

type ShareSettings struct {
	Enabled  bool `bson:"enabled" json:"enabled"`
	Native   bool `bson:"native" json:"native"`
	Email    bool `bson:"email" json:"email"`
	Facebook bool `bson:"facebook" json:"facebook"`
	LinkedIn bool `bson:"linkedin" json:"linkedin"`
	X        bool `bson:"x" json:"x"`
}

type CTASettings struct {
	Enabled   bool   `bson:"enabled" json:"enabled"`
	Text      string `bson:"text,omitempty" json:"text,omitempty"`
	URL       string `bson:"url,omitempty" json:"url,omitempty"`
	Style     string `bson:"style,omitempty" json:"style,omitempty"`
	StartPage int    `bson:"start_page,omitempty" json:"start_page,omitempty"`
	ShowOnEnd bool   `bson:"show_on_end" json:"show_on_end"`
}

type LeadGateSettings struct {
	Enabled        bool   `bson:"enabled" json:"enabled"`
	Mandatory      bool   `bson:"mandatory" json:"mandatory"`
	Dismissible    bool   `bson:"dismissible" json:"dismissible"`
	PreviewPages   int    `bson:"preview_pages" json:"preview_pages"`
	GateReading    bool   `bson:"gate_reading" json:"gate_reading"`
	GateDownloads  bool   `bson:"gate_downloads" json:"gate_downloads"`
	ConsentText    string `bson:"consent_text,omitempty" json:"consent_text,omitempty"`
	SuccessMessage string `bson:"success_message,omitempty" json:"success_message,omitempty"`
}

type SalesSettings struct {
	Enabled       bool   `bson:"enabled" json:"enabled"`
	PriceCents    int64  `bson:"price_cents" json:"price_cents"`
	Currency      string `bson:"currency,omitempty" json:"currency,omitempty"`
	PreviewPages  int    `bson:"preview_pages" json:"preview_pages"`
	StripePriceID string `bson:"stripe_price_id,omitempty" json:"stripe_price_id,omitempty"`
}

type AccessToken struct {
	ID         string     `bson:"id" json:"id"`
	Name       string     `bson:"name,omitempty" json:"name,omitempty"`
	TokenHash  string     `bson:"token_hash" json:"-"`
	CreatedAt  time.Time  `bson:"created_at" json:"created_at"`
	ExpiresAt  *time.Time `bson:"expires_at,omitempty" json:"expires_at,omitempty"`
	RevokedAt  *time.Time `bson:"revoked_at,omitempty" json:"revoked_at,omitempty"`
	LastUsedAt *time.Time `bson:"last_used_at,omitempty" json:"last_used_at,omitempty"`
}

func (t AccessToken) Active(now time.Time) bool {
	return t.RevokedAt == nil && (t.ExpiresAt == nil || now.Before(*t.ExpiresAt))
}

type Flipbook struct {
	ID           string `bson:"_id" json:"id"`
	WorkspaceID  string `bson:"workspace_id" json:"workspace_id"`
	OwnerID      string `bson:"owner_id" json:"owner_id"`
	Title        string `bson:"title" json:"title"`
	Slug         string `bson:"slug" json:"slug"`
	Description  string `bson:"description" json:"description"`
	Filename     string `bson:"filename" json:"filename"`
	FileSize     int64  `bson:"file_size" json:"file_size"`
	PageCount    int    `bson:"page_count" json:"page_count"`
	Status       string `bson:"status" json:"status"`
	ErrorMessage string `bson:"error_message" json:"error_message"`
	PageWidth    int    `bson:"page_width" json:"page_width"`
	PageHeight   int    `bson:"page_height" json:"page_height"`

	// IsPublic is retained for compatibility with legacy records and clients.
	// PublicationState and Visibility are authoritative for new access policy.
	IsPublic         bool             `bson:"is_public" json:"is_public"`
	PublicationState PublicationState `bson:"publication_state" json:"publication_state"`
	Visibility       Visibility       `bson:"visibility" json:"visibility"`
	ReviewState      ReviewState      `bson:"review_state" json:"review_state"`
	ReviewNote       string           `bson:"review_note,omitempty" json:"review_note,omitempty"`
	ReviewedBy       string           `bson:"reviewed_by,omitempty" json:"reviewed_by,omitempty"`
	ReviewedAt       *time.Time       `bson:"reviewed_at,omitempty" json:"reviewed_at,omitempty"`
	PublishAt        *time.Time       `bson:"publish_at,omitempty" json:"publish_at,omitempty"`
	ExpiresAt        *time.Time       `bson:"expires_at,omitempty" json:"expires_at,omitempty"`
	PublishedAt      *time.Time       `bson:"published_at,omitempty" json:"published_at,omitempty"`
	UnpublishedAt    *time.Time       `bson:"unpublished_at,omitempty" json:"unpublished_at,omitempty"`

	CurrentRevisionID string   `bson:"current_revision_id,omitempty" json:"current_revision_id,omitempty"`
	AssetVersion      int64    `bson:"asset_version" json:"asset_version"`
	Tags              []string `bson:"tags,omitempty" json:"tags,omitempty"`
	CollectionIDs     []string `bson:"collection_ids,omitempty" json:"collection_ids,omitempty"`
	ExternalSource    string   `bson:"external_source,omitempty" json:"external_source,omitempty"`
	ExternalID        string   `bson:"external_id,omitempty" json:"external_id,omitempty"`
	Language          string   `bson:"language" json:"language"`

	SEO      SEOMetadata      `bson:"seo" json:"seo"`
	Branding BrandingSettings `bson:"branding" json:"branding"`
	Download DownloadSettings `bson:"download" json:"download"`
	Embed    EmbedSettings    `bson:"embed" json:"embed"`
	Share    ShareSettings    `bson:"share" json:"share"`
	CTA      CTASettings      `bson:"cta" json:"cta"`
	LeadGate LeadGateSettings `bson:"lead_gate" json:"lead_gate"`
	Sales    SalesSettings    `bson:"sales" json:"sales"`

	PasswordHash string        `bson:"password_hash,omitempty" json:"-"`
	AccessTokens []AccessToken `bson:"access_tokens,omitempty" json:"access_tokens,omitempty"`

	GridFSFileID   string     `bson:"gridfs_file_id,omitempty" json:"gridfs_file_id,omitempty"`
	TenantID       string     `bson:"tenant_id,omitempty" json:"tenant_id,omitempty"` // Owning tenant (empty = legacy admin flipbook)
	SourceChecksum string     `bson:"source_checksum,omitempty" json:"source_checksum,omitempty"`
	BackupStatus   string     `bson:"backup_status,omitempty" json:"backup_status,omitempty"`
	BackupError    string     `bson:"backup_error,omitempty" json:"backup_error,omitempty"`
	BackedUpAt     *time.Time `bson:"backed_up_at,omitempty" json:"backed_up_at,omitempty"`

	ArchivedAt    *time.Time `bson:"archived_at,omitempty" json:"archived_at,omitempty"`
	ArchiveReason string     `bson:"archive_reason,omitempty" json:"archive_reason,omitempty"`
	DeletedAt     *time.Time `bson:"deleted_at,omitempty" json:"deleted_at,omitempty"`
	PurgeAfter    *time.Time `bson:"purge_after,omitempty" json:"purge_after,omitempty"`
	CreatedAt     time.Time  `bson:"created_at" json:"created_at"`
	UpdatedAt     time.Time  `bson:"updated_at" json:"updated_at"`
	ConvertedAt   *time.Time `bson:"converted_at,omitempty" json:"converted_at,omitempty"`
}

func (f *Flipbook) ApplyDefaults() {
	if f.WorkspaceID == "" {
		f.WorkspaceID = DefaultWorkspaceID
	}
	if f.OwnerID == "" {
		f.OwnerID = LegacyAdminUserID
	}
	if f.PublicationState == "" {
		f.PublicationState = PublicationDraft
	}
	if f.Visibility == "" {
		f.Visibility = VisibilityPublic
	}
	if f.ReviewState == "" {
		f.ReviewState = ReviewNotRequired
	}
	if f.AssetVersion < 1 {
		f.AssetVersion = 1
	}
	if f.Language == "" {
		f.Language = "en"
	}
	if f.Embed.StartPage < 1 {
		f.Embed.StartPage = 1
	}
	if f.Sales.Currency == "" {
		f.Sales.Currency = "USD"
	}
	f.Sales.Currency = strings.ToUpper(f.Sales.Currency)
	f.IsPublic = f.PublicationState == PublicationPublished &&
		(f.Visibility == VisibilityPublic || f.Visibility == VisibilityUnlisted)
}

func (f *Flipbook) IsArchived() bool {
	return f.ArchivedAt != nil && f.DeletedAt == nil
}

func (f *Flipbook) IsPublished(now time.Time) bool {
	if f.PublicationState != PublicationPublished || f.Status != StatusReady ||
		f.ArchivedAt != nil || f.DeletedAt != nil {
		return false
	}
	if f.PublishAt != nil && now.Before(*f.PublishAt) {
		return false
	}
	return f.ExpiresAt == nil || now.Before(*f.ExpiresAt)
}

func (f *Flipbook) AllowsAnonymousAccess(now time.Time) bool {
	return f.IsPublished(now) && (f.Visibility == VisibilityPublic || f.Visibility == VisibilityUnlisted)
}
