package models

import (
	"slices"
	"strings"
	"time"
)

const (
	DefaultWorkspaceID = "default"
	LegacyAdminUserID  = "legacy-admin"
)

type Role string

const (
	RoleReader    Role = "reader"
	RoleEditor    Role = "editor"
	RolePublisher Role = "publisher"
	RoleAdmin     Role = "admin"
	RoleOwner     Role = "owner"
)

func (r Role) Valid() bool {
	switch r {
	case RoleReader, RoleEditor, RolePublisher, RoleAdmin, RoleOwner:
		return true
	default:
		return false
	}
}

func (r Role) rank() int {
	switch r {
	case RoleReader:
		return 1
	case RoleEditor:
		return 2
	case RolePublisher:
		return 3
	case RoleAdmin:
		return 4
	case RoleOwner:
		return 5
	default:
		return 0
	}
}

func (r Role) AtLeast(required Role) bool {
	return r.Valid() && required.Valid() && r.rank() >= required.rank()
}

func (r Role) CanEdit() bool    { return r.AtLeast(RoleEditor) }
func (r Role) CanPublish() bool { return r.AtLeast(RolePublisher) }
func (r Role) CanManage() bool  { return r.AtLeast(RoleAdmin) }

type User struct {
	ID                string     `bson:"_id" json:"id"`
	Email             string     `bson:"email,omitempty" json:"email,omitempty"`
	NormalizedEmail   string     `bson:"normalized_email,omitempty" json:"-"`
	Name              string     `bson:"name" json:"name"`
	PasswordHash      string     `bson:"password_hash,omitempty" json:"-"`
	OIDCIssuer        string     `bson:"oidc_issuer,omitempty" json:"oidc_issuer,omitempty"`
	OIDCSubject       string     `bson:"oidc_subject,omitempty" json:"oidc_subject,omitempty"`
	Locale            string     `bson:"locale,omitempty" json:"locale,omitempty"`
	TimeZone          string     `bson:"time_zone,omitempty" json:"time_zone,omitempty"`
	PasswordChangedAt *time.Time `bson:"password_changed_at,omitempty" json:"password_changed_at,omitempty"`
	LastLoginAt       *time.Time `bson:"last_login_at,omitempty" json:"last_login_at,omitempty"`
	DisabledAt        *time.Time `bson:"disabled_at,omitempty" json:"disabled_at,omitempty"`
	CreatedAt         time.Time  `bson:"created_at" json:"created_at"`
	UpdatedAt         time.Time  `bson:"updated_at" json:"updated_at"`
}

func (u *User) Normalize() {
	u.NormalizedEmail = strings.ToLower(strings.TrimSpace(u.Email))
	if u.NormalizedEmail == "" {
		u.Email = ""
	}
	if u.Locale == "" {
		u.Locale = "en"
	}
}

func (u User) Active() bool { return u.DisabledAt == nil }

type CustomDomain struct {
	Host              string     `bson:"host" json:"host"`
	Verified          bool       `bson:"verified" json:"verified"`
	VerificationToken string     `bson:"verification_token,omitempty" json:"verification_token,omitempty"`
	VerifiedAt        *time.Time `bson:"verified_at,omitempty" json:"verified_at,omitempty"`
}

type WorkspaceSettings struct {
	Branding                 BrandingSettings `bson:"branding" json:"branding"`
	CustomDomains            []CustomDomain   `bson:"custom_domains,omitempty" json:"custom_domains,omitempty"`
	DefaultVisibility        Visibility       `bson:"default_visibility" json:"default_visibility"`
	DefaultDownload          DownloadSettings `bson:"download" json:"download"`
	DefaultEmbed             EmbedSettings    `bson:"embed" json:"embed"`
	DefaultShare             ShareSettings    `bson:"share" json:"share"`
	RequirePublicationReview bool             `bson:"require_publication_review" json:"require_publication_review"`
	AnalyticsConsentPolicy   string           `bson:"analytics_consent_policy,omitempty" json:"analytics_consent_policy,omitempty"`
	AnalyticsRetentionDays   int              `bson:"analytics_retention_days,omitempty" json:"analytics_retention_days,omitempty"`
	NotificationEmail        string           `bson:"notification_email,omitempty" json:"notification_email,omitempty"`
}

type Workspace struct {
	ID        string            `bson:"_id" json:"id"`
	Name      string            `bson:"name" json:"name"`
	Slug      string            `bson:"slug" json:"slug"`
	OwnerID   string            `bson:"owner_id" json:"owner_id"`
	Settings  WorkspaceSettings `bson:"settings" json:"settings"`
	CreatedAt time.Time         `bson:"created_at" json:"created_at"`
	UpdatedAt time.Time         `bson:"updated_at" json:"updated_at"`
}

func (w *Workspace) ApplyDefaults() {
	if w.ID == DefaultWorkspaceID && w.Slug == "" {
		w.Slug = DefaultWorkspaceID
	}
	if w.Settings.DefaultVisibility == "" {
		w.Settings.DefaultVisibility = VisibilityPublic
	}
	if w.Settings.AnalyticsConsentPolicy == "" {
		w.Settings.AnalyticsConsentPolicy = "required"
	}
	if w.Settings.AnalyticsRetentionDays == 0 {
		w.Settings.AnalyticsRetentionDays = 365
	}
}

// FlipbookDefaults are the per-publication settings a new flipbook inherits
// from its workspace. Every creation path (admin upload, API, provider export)
// applies the same set so a publication behaves the same however it arrived.
type FlipbookDefaults struct {
	Visibility Visibility
	Download   DownloadSettings
	Embed      EmbedSettings
	Share      ShareSettings
}

// FallbackFlipbookDefaults is used when there is no workspace to inherit from
// (legacy SaaS tenant uploads): public, embeddable, no downloads or share bar.
func FallbackFlipbookDefaults() FlipbookDefaults {
	return FlipbookDefaults{Visibility: VisibilityPublic, Embed: DefaultEmbedSettings()}
}

// NewFlipbookDefaults returns the settings a flipbook created in this
// workspace starts with. A nil workspace yields FallbackFlipbookDefaults.
func (w *Workspace) NewFlipbookDefaults() FlipbookDefaults {
	if w == nil {
		return FallbackFlipbookDefaults()
	}
	defaults := FlipbookDefaults{
		Visibility: w.Settings.DefaultVisibility,
		Download:   w.Settings.DefaultDownload,
		Embed:      w.Settings.DefaultEmbed,
		Share:      w.Settings.DefaultShare,
	}
	if defaults.Visibility == "" {
		defaults.Visibility = VisibilityPublic
	}
	return defaults
}

// Apply copies the defaults onto a flipbook being created. Download, embed and
// share are always set; visibility only when the caller has not chosen one.
func (d FlipbookDefaults) Apply(fb *Flipbook) {
	if fb.Visibility == "" {
		fb.Visibility = d.Visibility
	}
	fb.Download = d.Download
	fb.Embed = d.Embed
	// Clone so editing one flipbook's allowlist can't write through to the
	// workspace or to other flipbooks created from the same defaults.
	fb.Embed.AllowedDomains = slices.Clone(d.Embed.AllowedDomains)
	fb.Share = d.Share
}

type Membership struct {
	ID          string    `bson:"_id" json:"id"`
	WorkspaceID string    `bson:"workspace_id" json:"workspace_id"`
	UserID      string    `bson:"user_id" json:"user_id"`
	Role        Role      `bson:"role" json:"role"`
	InvitedBy   string    `bson:"invited_by,omitempty" json:"invited_by,omitempty"`
	CreatedAt   time.Time `bson:"created_at" json:"created_at"`
	UpdatedAt   time.Time `bson:"updated_at" json:"updated_at"`
}

type Invitation struct {
	ID              string     `bson:"_id" json:"id"`
	WorkspaceID     string     `bson:"workspace_id" json:"workspace_id"`
	Email           string     `bson:"email" json:"email"`
	NormalizedEmail string     `bson:"normalized_email" json:"-"`
	Role            Role       `bson:"role" json:"role"`
	TokenHash       string     `bson:"token_hash" json:"-"`
	InvitedBy       string     `bson:"invited_by" json:"invited_by"`
	ExpiresAt       time.Time  `bson:"expires_at" json:"expires_at"`
	AcceptedAt      *time.Time `bson:"accepted_at,omitempty" json:"accepted_at,omitempty"`
	RevokedAt       *time.Time `bson:"revoked_at,omitempty" json:"revoked_at,omitempty"`
	CreatedAt       time.Time  `bson:"created_at" json:"created_at"`
}
