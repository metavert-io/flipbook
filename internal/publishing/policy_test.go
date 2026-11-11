package publishing

import (
	"net/http"
	"testing"
	"time"

	"github.com/metavert-io/flipbook/internal/models"
)

func policyFixture(now time.Time) PolicyInput {
	fb, _ := testPublication(now)
	return PolicyInput{Flipbook: *fb, Resource: ResourceViewer, PageCount: fb.PageCount, Now: now}
}

func member(workspaceID string, role models.Role) Principal {
	return Principal{UserID: "user", WorkspaceID: workspaceID, Role: role, Member: true}
}

func TestEvaluateAccessLifecycleAndVisibility(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	future, past := now.Add(time.Hour), now.Add(-time.Hour)
	tests := []struct {
		name    string
		change  func(*PolicyInput)
		allowed bool
		code    ErrorCode
		status  int
		preview bool
	}{
		{name: "public anonymous", allowed: true},
		{name: "unlisted anonymous", change: func(in *PolicyInput) { in.Flipbook.Visibility = models.VisibilityUnlisted }, allowed: true},
		{name: "draft anonymous", change: func(in *PolicyInput) { in.Flipbook.PublicationState = models.PublicationDraft }, code: CodeNotFound, status: http.StatusNotFound},
		{name: "draft editor preview", change: func(in *PolicyInput) {
			in.Flipbook.PublicationState = models.PublicationDraft
			in.Principal = member("workspace", models.RoleEditor)
		}, allowed: true, preview: true},
		{name: "draft reader denied", change: func(in *PolicyInput) {
			in.Flipbook.PublicationState = models.PublicationDraft
			in.Principal = member("workspace", models.RoleReader)
		}, code: CodeNotFound, status: http.StatusNotFound},
		{name: "scheduled not yet live", change: func(in *PolicyInput) { in.Flipbook.PublishAt = &future }, code: CodeNotFound, status: http.StatusNotFound},
		{name: "expired", change: func(in *PolicyInput) { in.Flipbook.ExpiresAt = &past }, code: CodeExpired, status: http.StatusGone},
		{name: "archived even for staff", change: func(in *PolicyInput) {
			in.Flipbook.ArchivedAt = &past
			in.Principal = member("workspace", models.RoleAdmin)
		}, code: CodeNotFound, status: http.StatusNotFound},
		{name: "not ready staff viewer", change: func(in *PolicyInput) {
			in.Flipbook.Status = models.StatusConverting
			in.Flipbook.PublicationState = models.PublicationDraft
			in.Principal = member("workspace", models.RoleEditor)
		}, code: CodeNotReady, status: http.StatusConflict},
		{name: "not ready staff status", change: func(in *PolicyInput) {
			in.Flipbook.Status = models.StatusConverting
			in.Flipbook.PublicationState = models.PublicationDraft
			in.Principal = member("workspace", models.RoleEditor)
			in.Resource = ResourceStatus
		}, allowed: true, preview: true},
		{name: "private anonymous", change: func(in *PolicyInput) { in.Flipbook.Visibility = models.VisibilityPrivate }, code: CodeUnauthorized, status: http.StatusUnauthorized},
		{name: "private editor", change: func(in *PolicyInput) {
			in.Flipbook.Visibility = models.VisibilityPrivate
			in.Principal = member("workspace", models.RoleEditor)
		}, allowed: true},
		{name: "private reader", change: func(in *PolicyInput) {
			in.Flipbook.Visibility = models.VisibilityPrivate
			in.Principal = member("workspace", models.RoleReader)
		}, code: CodeForbidden, status: http.StatusForbidden},
		{name: "team reader", change: func(in *PolicyInput) {
			in.Flipbook.Visibility = models.VisibilityTeam
			in.Principal = member("workspace", models.RoleReader)
		}, allowed: true},
		{name: "team outsider", change: func(in *PolicyInput) { in.Flipbook.Visibility = models.VisibilityTeam }, code: CodeUnauthorized, status: http.StatusUnauthorized},
		{name: "team cross workspace membership", change: func(in *PolicyInput) {
			in.Flipbook.Visibility = models.VisibilityTeam
			in.Principal = member("other", models.RoleOwner)
		}, code: CodeUnauthorized, status: http.StatusUnauthorized},
		{name: "password missing", change: func(in *PolicyInput) { in.Flipbook.Visibility = models.VisibilityPassword }, code: CodePasswordRequired, status: http.StatusUnauthorized},
		{name: "password evidence", change: func(in *PolicyInput) {
			in.Flipbook.Visibility = models.VisibilityPassword
			in.Evidence.Password = true
		}, allowed: true},
		{name: "token missing", change: func(in *PolicyInput) { in.Flipbook.Visibility = models.VisibilityToken }, code: CodeShareTokenRequired, status: http.StatusUnauthorized},
		{name: "token evidence", change: func(in *PolicyInput) {
			in.Flipbook.Visibility = models.VisibilityToken
			in.Evidence.ShareToken = true
		}, allowed: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := policyFixture(now)
			if test.change != nil {
				test.change(&input)
			}
			decision := EvaluateAccess(input)
			if decision.Allowed != test.allowed || decision.Reason != test.code {
				t.Fatalf("decision = %+v, want allowed=%v code=%q", decision, test.allowed, test.code)
			}
			if !test.allowed && decision.Status != test.status {
				t.Fatalf("status = %d, want %d", decision.Status, test.status)
			}
			if decision.Preview != test.preview {
				t.Fatalf("preview = %v, want %v", decision.Preview, test.preview)
			}
			if !decision.Allowed && decision.CacheControl != CacheNoStore {
				t.Fatalf("denied response cache = %q", decision.CacheControl)
			}
		})
	}
}

func TestEvaluateAccessPaidPreviewAndEntitlement(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	input := policyFixture(now)
	input.Flipbook.Sales = models.SalesSettings{Enabled: true, PriceCents: 500, Currency: "USD", PreviewPages: 2}

	viewer := EvaluateAccess(input)
	if !viewer.Allowed || viewer.FullAccess || !viewer.PurchaseRequired || viewer.PreviewPages != 2 {
		t.Fatalf("paid viewer decision = %+v", viewer)
	}
	input.Resource, input.Page, input.Revision = ResourcePage, 2, 1
	preview := EvaluateAccess(input)
	if !preview.Allowed || preview.CacheControl != CachePublicImmutable {
		t.Fatalf("public paid preview page = %+v", preview)
	}
	input.Page = 3
	if decision := EvaluateAccess(input); decision.Allowed || decision.Reason != CodePurchaseRequired || decision.Status != http.StatusPaymentRequired {
		t.Fatalf("paid page decision = %+v", decision)
	}
	input.Evidence.Entitled = true
	if decision := EvaluateAccess(input); !decision.Allowed || !decision.FullAccess {
		t.Fatalf("entitled page decision = %+v", decision)
	}
	input.Resource, input.Page = ResourceDownloadPDF, 0
	if decision := EvaluateAccess(input); !decision.Allowed {
		t.Fatalf("entitled download decision = %+v", decision)
	}
	input.Evidence.Entitled = false
	if decision := EvaluateAccess(input); decision.Allowed || decision.Reason != CodePurchaseRequired {
		t.Fatalf("unentitled download decision = %+v", decision)
	}
}

func TestEvaluateAccessLeadGateModes(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	input := policyFixture(now)
	input.Flipbook.LeadGate = models.LeadGateSettings{
		Enabled: true, Mandatory: true, PreviewPages: 1, GateReading: true, GateDownloads: true,
	}
	if decision := EvaluateAccess(input); !decision.Allowed || !decision.LeadGateRequired || decision.PreviewPages != 1 {
		t.Fatalf("mandatory lead viewer = %+v", decision)
	}
	input.Resource, input.Page = ResourceText, 2
	if decision := EvaluateAccess(input); decision.Allowed || decision.Reason != CodeLeadRequired {
		t.Fatalf("lead protected text = %+v", decision)
	}
	input.Evidence.LeadDismissed = true
	if decision := EvaluateAccess(input); decision.Allowed {
		t.Fatalf("mandatory gate accepted dismissal: %+v", decision)
	}
	input.Evidence.LeadCaptured = true
	if decision := EvaluateAccess(input); !decision.Allowed || !decision.FullAccess {
		t.Fatalf("captured lead did not unlock text: %+v", decision)
	}

	input = policyFixture(now)
	input.Flipbook.LeadGate = models.LeadGateSettings{Enabled: true, Dismissible: true, PreviewPages: 1, GateReading: true}
	input.Evidence.LeadDismissed = true
	input.Resource, input.Page = ResourcePage, 4
	if decision := EvaluateAccess(input); !decision.Allowed || !decision.FullAccess {
		t.Fatalf("dismissible lead gate did not unlock: %+v", decision)
	}
}

func TestEvaluateAccessCombinedGatesAndStaffBypass(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	input := policyFixture(now)
	input.Flipbook.Sales = models.SalesSettings{Enabled: true, PriceCents: 500, Currency: "USD", PreviewPages: 3}
	input.Flipbook.LeadGate = models.LeadGateSettings{Enabled: true, Mandatory: true, PreviewPages: 1, GateReading: true, GateDownloads: true}
	viewer := EvaluateAccess(input)
	if viewer.PreviewPages != 1 || !viewer.PurchaseRequired || !viewer.LeadGateRequired {
		t.Fatalf("combined gate decision = %+v", viewer)
	}
	input.Principal = member("workspace", models.RoleEditor)
	input.Resource, input.Page = ResourceDownloadOriginal, 99
	if decision := EvaluateAccess(input); !decision.Allowed || !decision.FullAccess || !decision.Capabilities.DownloadOriginal {
		t.Fatalf("staff gate bypass = %+v", decision)
	}
}

func TestEvaluateAccessDistributionSettings(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	input := policyFixture(now)
	input.Resource = ResourceEmbed
	input.ViaEmbed = true
	input.Flipbook.Embed.AllowedDomains = []string{"example.com", "*.trusted.org"}
	input.EmbedHost = "https://example.com/path"
	if decision := EvaluateAccess(input); !decision.Allowed {
		t.Fatalf("allowed embed origin rejected: %+v", decision)
	}
	input.EmbedHost = "reader.trusted.org"
	if decision := EvaluateAccess(input); !decision.Allowed {
		t.Fatalf("wildcard embed origin rejected: %+v", decision)
	}
	input.EmbedHost = "evil-example.com"
	if decision := EvaluateAccess(input); decision.Allowed || decision.Reason != CodeEmbedDomain {
		t.Fatalf("host suffix attack accepted: %+v", decision)
	}
	input.EmbedHost = "trusted.org.evil.com"
	if decision := EvaluateAccess(input); decision.Allowed {
		t.Fatalf("wildcard suffix attack accepted: %+v", decision)
	}

	input = policyFixture(now)
	input.Resource = ResourceShare
	input.ShareChannel = ShareEmail
	input.Flipbook.Share.Email = false
	if decision := EvaluateAccess(input); decision.Allowed || decision.Reason != CodeShareDisabled {
		t.Fatalf("disabled share channel accepted: %+v", decision)
	}
	input.Resource = ResourceDownloadOriginal
	input.Flipbook.Download.AllowOriginal = false
	if decision := EvaluateAccess(input); decision.Allowed || decision.Reason != CodeDownloadDisabled {
		t.Fatalf("disabled original download accepted: %+v", decision)
	}
	input.Flipbook.Download.AllowOriginal = true
	input.ViaEmbed = true
	input.Flipbook.Embed.ShowDownload = false
	if decision := EvaluateAccess(input); decision.Allowed || decision.Reason != CodeDownloadDisabled {
		t.Fatalf("embed download setting ignored: %+v", decision)
	}
}

func TestDecisionCacheAndHeaders(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	input := policyFixture(now)
	viewer := EvaluateAccess(input)
	if viewer.CacheControl != CachePublicReload || viewer.NoIndex {
		t.Fatalf("public viewer cache/index = %+v", viewer)
	}
	input.Resource, input.Page, input.Revision = ResourcePage, 1, 1
	asset := EvaluateAccess(input)
	if asset.CacheControl != CachePublicImmutable {
		t.Fatalf("public revision asset cache = %q", asset.CacheControl)
	}
	headers := asset.Headers()
	if headers.Get("Cache-Control") != CachePublicImmutable || headers.Get("Vary") != "" {
		t.Fatalf("public asset headers = %+v", headers)
	}
	input.Flipbook.Visibility = models.VisibilityUnlisted
	restricted := EvaluateAccess(input)
	if restricted.CacheControl != CachePrivate || !restricted.NoIndex || restricted.Headers().Get("Vary") == "" {
		t.Fatalf("unlisted asset headers = %+v, %+v", restricted, restricted.Headers())
	}
	input.Resource = ResourceEmbed
	input.ViaEmbed = true
	input.Flipbook.Embed.AllowedDomains = []string{"example.com"}
	embed := EvaluateAccess(input)
	if got := embed.Headers().Get("Content-Security-Policy"); got != "frame-ancestors 'self' https://example.com https://www.example.com" {
		t.Fatalf("embed CSP = %q", got)
	}
	if !embed.NoIndex {
		t.Fatal("embed must be noindex")
	}
}

func TestFrameAncestorsCoversWWWForBareApex(t *testing.T) {
	tests := []struct {
		domains []string
		want    string
	}{
		{nil, "*"},
		{[]string{"penninn.is"}, "'self' https://penninn.is https://www.penninn.is"},
		{[]string{"www.penninn.is"}, "'self' https://www.penninn.is"},
		{[]string{"shop.penninn.is"}, "'self' https://shop.penninn.is"},
		{[]string{"*.penninn.is"}, "'self' https://*.penninn.is"},
		{[]string{"https://penninn.is"}, "'self' https://penninn.is"},
		{[]string{"example.co.uk"}, "'self' https://example.co.uk"},
	}
	for _, test := range tests {
		if got := FrameAncestors(test.domains); got != test.want {
			t.Errorf("FrameAncestors(%v) = %q, want %q", test.domains, got, test.want)
		}
	}
}

func TestEmbedHostAllowedMatchesFrameAncestors(t *testing.T) {
	tests := []struct {
		host    string
		domains []string
		allowed bool
	}{
		{"https://www.penninn.is/", []string{"penninn.is"}, true},
		{"https://penninn.is/", []string{"penninn.is"}, true},
		{"https://www.penninn.is/", []string{"https://penninn.is"}, false},
		{"https://shop.penninn.is/", []string{"penninn.is"}, false},
		{"https://www.evilpenninn.is/", []string{"penninn.is"}, false},
		{"https://www.penninn.is.evil.com/", []string{"penninn.is"}, false},
		{"", []string{"penninn.is"}, true},
	}
	for _, test := range tests {
		if got := embedHostAllowed(test.host, test.domains); got != test.allowed {
			t.Errorf("embedHostAllowed(%q, %v) = %v, want %v", test.host, test.domains, got, test.allowed)
		}
	}
}

func TestDefaultEmbedSettingsAllowAnonymousEmbed(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	input := policyFixture(now)
	input.Flipbook.Embed = models.DefaultEmbedSettings()
	input.Resource, input.ViaEmbed = ResourceEmbed, true
	if decision := EvaluateAccess(input); !decision.Allowed || decision.FrameAncestors != "*" {
		t.Fatalf("default embed settings must allow anonymous embedding anywhere: %+v", decision)
	}
	input.Flipbook.Embed = models.EmbedSettings{}
	if decision := EvaluateAccess(input); decision.Allowed || decision.Reason != CodeEmbedDisabled {
		t.Fatalf("zero embed settings must stay disabled: %+v", decision)
	}
}
