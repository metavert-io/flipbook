package publishing

import (
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/metavert-io/flipbook/internal/models"
)

func EvaluateAccess(input PolicyInput) Decision {
	fb := input.Flipbook
	now := input.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	pageCount := input.PageCount
	if pageCount == 0 {
		pageCount = fb.PageCount
	}
	decision := Decision{
		Allowed: true, Status: http.StatusOK, CacheControl: CacheNoStore,
		FullAccess: true, PreviewPages: pageCount,
	}
	if input.Resource == ResourceEmbed || input.ViaEmbed {
		decision.FrameAncestors = FrameAncestors(fb.Embed.AllowedDomains)
	}
	decision.NoIndex = fb.SEO.NoIndex || fb.Visibility != models.VisibilityPublic ||
		input.Resource == ResourceEmbed || input.Resource == ResourceStatus || input.ViaEmbed

	member := input.Principal.Member && input.Principal.UserID != "" &&
		input.Principal.WorkspaceID == fb.WorkspaceID && input.Principal.Role.Valid()
	staff := member && input.Principal.Role.CanEdit()
	live := fb.IsPublished(now)

	if fb.ArchivedAt != nil || fb.DeletedAt != nil {
		return deny(decision, http.StatusNotFound, CodeNotFound)
	}
	if !live {
		staffPreview := staff && (fb.Status == models.StatusReady || input.Resource == ResourceStatus)
		if !staffPreview {
			if fb.PublicationState == models.PublicationPublished && fb.ExpiresAt != nil && !now.Before(*fb.ExpiresAt) {
				return deny(decision, http.StatusGone, CodeExpired)
			}
			if staff && fb.Status != models.StatusReady {
				return deny(decision, http.StatusConflict, CodeNotReady)
			}
			return deny(decision, http.StatusNotFound, CodeNotFound)
		}
		decision.Preview = true
	}

	if !staff {
		switch fb.Visibility {
		case models.VisibilityPublic, models.VisibilityUnlisted:
		case models.VisibilityPrivate:
			return denyMembership(decision, member)
		case models.VisibilityTeam:
			if !member {
				return denyMembership(decision, false)
			}
		case models.VisibilityPassword:
			if !input.Evidence.Password {
				return deny(decision, http.StatusUnauthorized, CodePasswordRequired)
			}
		case models.VisibilityToken:
			if !input.Evidence.ShareToken {
				return deny(decision, http.StatusUnauthorized, CodeShareTokenRequired)
			}
		default:
			return deny(decision, http.StatusForbidden, CodeForbidden)
		}
	}

	if input.Resource == ResourceEmbed || input.ViaEmbed {
		if !staff && !fb.Embed.Enabled {
			return deny(decision, http.StatusForbidden, CodeEmbedDisabled)
		}
		if !embedHostAllowed(input.EmbedHost, fb.Embed.AllowedDomains) {
			return deny(decision, http.StatusForbidden, CodeEmbedDomain)
		}
	}
	if input.Resource == ResourceShare && !staff {
		if !fb.Share.Enabled || !shareChannelEnabled(fb.Share, input.ShareChannel) ||
			(input.ViaEmbed && !fb.Embed.ShowShare) {
			return deny(decision, http.StatusForbidden, CodeShareDisabled)
		}
	}
	if isDownload(input.Resource) && !staff {
		if !downloadSettingEnabled(fb.Download, input.Resource) ||
			(input.ViaEmbed && !fb.Embed.ShowDownload) {
			return deny(decision, http.StatusForbidden, CodeDownloadDisabled)
		}
	}
	if input.Page > 0 && pageCount > 0 && input.Page > pageCount && isReadingResource(input.Resource) {
		return deny(decision, http.StatusNotFound, CodeNotFound)
	}

	leadSatisfied := input.Evidence.LeadCaptured ||
		(input.Evidence.LeadDismissed && fb.LeadGate.Dismissible && !fb.LeadGate.Mandatory)
	leadMode := fb.LeadGate.Enabled && (fb.LeadGate.Mandatory || fb.LeadGate.Dismissible)
	readingResource := isReadingResource(input.Resource)
	downloadResource := isDownload(input.Resource)
	leadReadingGate := leadMode && fb.LeadGate.GateReading && readingResource && !leadSatisfied
	leadDownloadGate := leadMode && fb.LeadGate.GateDownloads && downloadResource && !leadSatisfied
	paidReadingGate := fb.Sales.Enabled && readingResource && !input.Evidence.Entitled &&
		(pageCount <= 0 || fb.Sales.PreviewPages < pageCount)
	paidDownloadGate := fb.Sales.Enabled && downloadResource && !input.Evidence.Entitled

	if staff {
		leadReadingGate, leadDownloadGate, paidReadingGate, paidDownloadGate = false, false, false, false
	}
	decision.LeadGateRequired = leadReadingGate || leadDownloadGate
	decision.LeadGateDismissible = decision.LeadGateRequired && fb.LeadGate.Dismissible && !fb.LeadGate.Mandatory
	decision.PurchaseRequired = paidReadingGate || paidDownloadGate

	previewPages := pageCount
	if pageCount < 0 {
		previewPages = 0
	}
	if leadReadingGate {
		previewPages = minNonNegative(previewPages, fb.LeadGate.PreviewPages)
	}
	if paidReadingGate {
		previewPages = minNonNegative(previewPages, fb.Sales.PreviewPages)
	}
	decision.PreviewPages = previewPages
	decision.FullAccess = !decision.LeadGateRequired && !decision.PurchaseRequired

	if input.Page > 0 && readingResource && !decision.FullAccess && input.Page > previewPages {
		if leadReadingGate && input.Page > fb.LeadGate.PreviewPages {
			return deny(decision, http.StatusForbidden, CodeLeadRequired)
		}
		if paidReadingGate && input.Page > fb.Sales.PreviewPages {
			return deny(decision, http.StatusPaymentRequired, CodePurchaseRequired)
		}
		return deny(decision, http.StatusForbidden, CodeForbidden)
	}
	if downloadResource && !decision.FullAccess {
		if leadDownloadGate {
			return deny(decision, http.StatusForbidden, CodeLeadRequired)
		}
		if paidDownloadGate {
			return deny(decision, http.StatusPaymentRequired, CodePurchaseRequired)
		}
	}

	decision.Capabilities = capabilities(input, staff, leadSatisfied)
	decision.CacheControl = cachePolicy(input, decision, member, staff, live)
	return decision
}

func deny(decision Decision, status int, code ErrorCode) Decision {
	decision.Allowed = false
	decision.Status = status
	decision.Reason = code
	decision.CacheControl = CacheNoStore
	decision.Capabilities = Capabilities{}
	return decision
}

func denyMembership(decision Decision, member bool) Decision {
	if member {
		return deny(decision, http.StatusForbidden, CodeForbidden)
	}
	return deny(decision, http.StatusUnauthorized, CodeUnauthorized)
}

func isReadingResource(resource Resource) bool {
	switch resource {
	case ResourceViewer, ResourceEmbed, ResourcePage, ResourceThumbnail, ResourceRendition,
		ResourceText, ResourceManifest, ResourceLinks:
		return true
	default:
		return false
	}
}

func isDownload(resource Resource) bool {
	switch resource {
	case ResourceDownloadOriginal, ResourceDownloadPDF, ResourcePrint:
		return true
	default:
		return false
	}
}

func minNonNegative(current, candidate int) int {
	if candidate < 0 {
		candidate = 0
	}
	if current < 0 || candidate < current {
		return candidate
	}
	return current
}

func downloadSettingEnabled(settings models.DownloadSettings, resource Resource) bool {
	if !settings.Enabled {
		return false
	}
	switch resource {
	case ResourceDownloadOriginal:
		return settings.AllowOriginal
	case ResourceDownloadPDF:
		return settings.AllowPDF
	case ResourcePrint:
		return settings.AllowPrint
	default:
		return false
	}
}

func shareChannelEnabled(settings models.ShareSettings, channel ShareChannel) bool {
	switch channel {
	case ShareAny:
		return true
	case ShareNative:
		return settings.Native
	case ShareEmail:
		return settings.Email
	case ShareFacebook:
		return settings.Facebook
	case ShareLinkedIn:
		return settings.LinkedIn
	case ShareX:
		return settings.X
	default:
		return false
	}
}

func capabilities(input PolicyInput, staff, leadSatisfied bool) Capabilities {
	fb := input.Flipbook
	leadDownload := fb.LeadGate.Enabled && fb.LeadGate.GateDownloads && !leadSatisfied
	paidDownload := fb.Sales.Enabled && !input.Evidence.Entitled
	downloadOpen := staff || (!leadDownload && !paidDownload)
	embedExtra := !input.ViaEmbed || fb.Embed.ShowDownload
	return Capabilities{
		Embed:            staff || fb.Embed.Enabled,
		Share:            (staff || fb.Share.Enabled) && (!input.ViaEmbed || fb.Embed.ShowShare),
		DownloadOriginal: downloadOpen && embedExtra && (staff || (fb.Download.Enabled && fb.Download.AllowOriginal)),
		DownloadPDF:      downloadOpen && embedExtra && (staff || (fb.Download.Enabled && fb.Download.AllowPDF)),
		Print:            downloadOpen && embedExtra && (staff || (fb.Download.Enabled && fb.Download.AllowPrint)),
	}
}

func cachePolicy(input PolicyInput, decision Decision, member, staff, live bool) string {
	fb := input.Flipbook
	anonymous := !member && input.Principal.UserID == "" && !input.Evidence.Password &&
		!input.Evidence.ShareToken && !input.Evidence.LeadCaptured &&
		!input.Evidence.LeadDismissed && !input.Evidence.Entitled
	if !anonymous || staff || !live || fb.Visibility != models.VisibilityPublic {
		if fb.Visibility == models.VisibilityUnlisted && !member {
			return CachePrivate
		}
		return CacheNoStore
	}
	if input.Resource == ResourceViewer || input.Resource == ResourceEmbed {
		if decision.FullAccess {
			return CachePublicReload
		}
		return CachePrivate
	}
	if isReadingResource(input.Resource) && input.Resource != ResourceStatus {
		anonymousAsset := decision.FullAccess
		if input.Page > 0 && !decision.FullAccess {
			anonymousAsset = input.Page <= decision.PreviewPages
		}
		if !anonymousAsset {
			return CachePrivate
		}
		if input.Revision > 0 {
			return CachePublicImmutable
		}
		return CachePublicReload
	}
	return CachePrivate
}

// FrameAncestors returns the CSP frame-ancestors source list for an embed
// allowlist: "*" when the list is empty, otherwise 'self' plus each entry.
func FrameAncestors(domains []string) string {
	if len(domains) == 0 {
		return "*"
	}
	values := []string{"'self'"}
	for _, domain := range domains {
		domain = strings.TrimSpace(strings.ToLower(domain))
		if domain == "" {
			continue
		}
		if strings.Contains(domain, "://") {
			values = append(values, domain)
			continue
		}
		values = append(values, "https://"+domain)
		if www := wwwVariant(domain); www != "" {
			values = append(values, "https://"+www)
		}
	}
	return strings.Join(values, " ")
}

// wwwVariant returns "www."+domain for a bare apex entry such as "penninn.is",
// because people list their site that way while it is served from
// www.penninn.is. Only name.tld entries count as apex (multi-label suffixes
// like .co.uk need www listed explicitly); subdomains, wildcards and explicit
// origins with a scheme are taken literally.
func wwwVariant(domain string) string {
	if strings.HasPrefix(domain, "www.") || strings.HasPrefix(domain, "*.") ||
		strings.ContainsAny(domain, ":/") || strings.Count(domain, ".") != 1 {
		return ""
	}
	return "www." + domain
}

func embedHostAllowed(rawHost string, domains []string) bool {
	if rawHost == "" || len(domains) == 0 {
		return true
	}
	host := normalizeEmbedHost(rawHost)
	if host == "" {
		return false
	}
	for _, allowed := range domains {
		allowed = strings.ToLower(strings.TrimSpace(allowed))
		explicitOrigin := strings.Contains(allowed, "://")
		allowed = strings.TrimPrefix(allowed, "https://")
		allowed = strings.TrimPrefix(allowed, "http://")
		allowed = strings.TrimSuffix(allowed, "/")
		if strings.HasPrefix(allowed, "*.") {
			suffix := strings.TrimPrefix(allowed, "*.")
			if host != suffix && strings.HasSuffix(host, "."+suffix) {
				return true
			}
			continue
		}
		allowedHost := normalizeEmbedHost(allowed)
		if allowedHost == "" {
			continue
		}
		// Mirror FrameAncestors: only bare entries also cover www.
		if host == allowedHost || (!explicitOrigin && host == wwwVariant(allowed)) {
			return true
		}
	}
	return false
}

func normalizeEmbedHost(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if strings.Contains(value, "://") {
		parsed, err := url.Parse(value)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
			return ""
		}
		value = parsed.Host
	} else if strings.ContainsAny(value, "/?#@") {
		return ""
	}
	if host, _, err := net.SplitHostPort(value); err == nil {
		value = host
	}
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(value)), ".")
}
