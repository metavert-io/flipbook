package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"net/url"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/metavert-io/flipbook/internal/converter"
	"github.com/metavert-io/flipbook/internal/database"
	"github.com/metavert-io/flipbook/internal/distribution"
	"github.com/metavert-io/flipbook/internal/models"
	"github.com/metavert-io/flipbook/internal/publishing"
	"github.com/metavert-io/flipbook/internal/saas"
	"github.com/metavert-io/flipbook/internal/storage"
)

type ViewerHandler struct {
	db         *database.DB
	storage    *storage.Storage
	tmpl       *template.Template
	baseURL    string
	publishing *publishing.Service
	billing    *saas.BillingChecker // nil when SaaS mode is disabled
}

func NewViewerHandler(db *database.DB, store *storage.Storage, tmpl *template.Template, baseURL string, services ...*publishing.Service) *ViewerHandler {
	return &ViewerHandler{db: db, storage: store, tmpl: tmpl, baseURL: strings.TrimRight(baseURL, "/"), publishing: readerService(services)}
}

func (h *ViewerHandler) SetPublishing(service *publishing.Service) *ViewerHandler {
	h.publishing = service
	return h
}

// SetBilling enables billing status checks for tenant-owned flipbooks.
func (h *ViewerHandler) SetBilling(bc *saas.BillingChecker) {
	h.billing = bc
}

func (h *ViewerHandler) View(w http.ResponseWriter, r *http.Request) {
	h.serve(w, r, false)
}

func (h *ViewerHandler) ViewShare(w http.ResponseWriter, r *http.Request) {
	if h.publishing == nil {
		http.Error(w, "publication access is not configured", http.StatusServiceUnavailable)
		return
	}
	request := readerAccessRequest(r, h.publishing, "", false)
	authorization, err := h.publishing.ResolveShareToken(r.Context(), chi.URLParam(r, "token"), request)
	if err != nil {
		h.renderAccessError(w, r, nil, err, false)
		return
	}
	h.renderAuthorized(w, r, authorization, false)
}

func (h *ViewerHandler) serve(w http.ResponseWriter, r *http.Request, embedded bool) {
	if h.publishing == nil {
		http.Error(w, "publication access is not configured", http.StatusServiceUnavailable)
		return
	}
	workspace, err := readerWorkspace(r, h.db)
	if err != nil {
		h.renderAccessError(w, r, nil, err, embedded)
		return
	}
	slug := chi.URLParam(r, "slug")
	fb, redirected, err := h.db.GetFlipbookBySlugOrRedirect(r.Context(), workspace.ID, models.NormalizeSlug(slug))
	if err != nil {
		h.renderAccessError(w, r, nil, err, embedded)
		return
	}
	// Billing check for tenant-owned flipbooks (legacy admin flipbooks always served)
	if fb.TenantID != "" && h.billing != nil {
		if !h.billing.TenantCanServe(r.Context(), fb.TenantID) {
			if embedded {
				allowFraming(w, fb)
			}
			_ = readerRender(h.tmpl, w, "unavailable", map[string]interface{}{
				"Flipbook": fb,
				"BaseURL":  h.baseURL,
			}, http.StatusServiceUnavailable)
			return
		}
	}
	request := readerAccessRequest(r, h.publishing, fb.ID, embedded)
	var authorization *publishing.Authorization
	if embedded {
		authorization, err = h.publishing.AuthorizeEmbed(r.Context(), workspace.ID, fb.ID, request)
	} else {
		authorization, err = h.publishing.AuthorizeViewer(r.Context(), workspace.ID, fb.ID, request)
	}
	if err != nil {
		if fb.Status == models.StatusPending || fb.Status == models.StatusConverting || fb.Status == models.StatusRegenerating {
			if _, statusErr := h.publishing.AuthorizeStatus(r.Context(), workspace.ID, fb.ID, request); statusErr == nil {
				w.Header().Set("Cache-Control", publishing.CacheNoStore)
				w.Header().Set("X-Robots-Tag", "noindex, nofollow, noarchive")
				if embedded {
					allowFraming(w, fb)
				}
				_ = readerRender(h.tmpl, w, "viewer_wait", map[string]any{
					"Flipbook": fb, "BaseURL": h.baseURL, "Language": distribution.Language(fb.Language),
					"Direction": distribution.Direction(fb.Language), "Embedded": embedded,
				}, http.StatusOK)
				return
			}
		}
		h.renderAccessError(w, r, fb, err, embedded)
		return
	}
	if redirected && chi.URLParam(r, "workspace") == "" && chi.URLParam(r, "workspaceSlug") == "" {
		target := "/v/" + url.PathEscape(fb.Slug)
		if embedded {
			target = "/embed/" + url.PathEscape(fb.Slug)
		}
		if query := r.URL.RawQuery; query != "" {
			target += "?" + query
		}
		http.Redirect(w, r, target, http.StatusPermanentRedirect)
		return
	}
	h.renderAuthorized(w, r, authorization, embedded)
}

func (h *ViewerHandler) renderAuthorized(w http.ResponseWriter, r *http.Request, authorization *publishing.Authorization, embedded bool) {
	data, err := h.buildViewData(r, authorization, embedded)
	if err != nil {
		h.renderAccessError(w, r, &authorization.Flipbook, err, embedded)
		return
	}
	applyPublishingHeaders(w, authorization.Decision)
	if err := readerRender(h.tmpl, w, data.TemplateName(), data, http.StatusOK); err != nil {
		http.Error(w, "reader template unavailable", http.StatusInternalServerError)
	}
	if !models.IsObviousBot(r.UserAgent()) {
		go h.db.RecordView(authorization.Flipbook.ID, models.NormalizeReferrer(r.Referer()), r.UserAgent())
	}
}

type readerLink struct {
	ID         string            `json:"id,omitempty"`
	Type       string            `json:"type"`
	URL        string            `json:"url,omitempty"`
	VideoEmbed string            `json:"video_embed,omitempty"`
	TargetPage int               `json:"target_page,omitempty"`
	Title      string            `json:"title"`
	X          float64           `json:"x"`
	Y          float64           `json:"y"`
	Width      float64           `json:"width"`
	Height     float64           `json:"height"`
	Style      *models.LinkStyle `json:"style,omitempty"`
}

type readerPage struct {
	Number       int                  `json:"number"`
	Source       string               `json:"src"`
	HighSource   string               `json:"high_src"`
	Thumbnail    string               `json:"thumb"`
	Width        int                  `json:"width"`
	Height       int                  `json:"height"`
	WidthPoints  float64              `json:"width_points"`
	HeightPoints float64              `json:"height_points"`
	Text         string               `json:"text"`
	Spans        []converter.TextSpan `json:"spans,omitempty"`
	Links        []readerLink         `json:"links,omitempty"`
}

type readerEndpoints struct {
	Events   string `json:"events"`
	Lead     string `json:"lead"`
	Dismiss  string `json:"dismiss"`
	Checkout string `json:"checkout"`
	Access   string `json:"access"`
	Download string `json:"download"`
	Print    string `json:"print"`
	QR       string `json:"qr"`
}

type readerCTA struct {
	Text      string `json:"text"`
	URL       string `json:"url"`
	Style     string `json:"style"`
	StartPage int    `json:"start_page"`
	ShowOnEnd bool   `json:"show_on_end"`
}

type readerShare struct {
	Enabled   bool                      `json:"enabled"`
	Native    bool                      `json:"native"`
	Links     map[string]string         `json:"links"`
	EmbedCode string                    `json:"embed_code"`
	Options   distribution.EmbedOptions `json:"options"`
}

type readerGate struct {
	Required    bool   `json:"required"`
	Dismissible bool   `json:"dismissible"`
	ConsentText string `json:"consent_text,omitempty"`
	SuccessText string `json:"success_text,omitempty"`
}

type readerSale struct {
	Required   bool   `json:"required"`
	PriceCents int64  `json:"price_cents"`
	Currency   string `json:"currency"`
}

type readerAnalytics struct {
	Policy    string `json:"policy"`
	Consented bool   `json:"consented"`
	Placement string `json:"placement"`
}

type readerClientData struct {
	ID           string                  `json:"id"`
	WorkspaceID  string                  `json:"workspace_id"`
	Title        string                  `json:"title"`
	Slug         string                  `json:"slug"`
	BaseURL      string                  `json:"base_url"`
	CanonicalURL string                  `json:"canonical_url"`
	Language     string                  `json:"language"`
	Direction    string                  `json:"direction"`
	Strings      map[string]string       `json:"strings"`
	Pages        []readerPage            `json:"pages"`
	PageCount    int                     `json:"page_count"`
	PreviewPages int                     `json:"preview_pages"`
	FullAccess   bool                    `json:"full_access"`
	Outline      []converter.OutlineItem `json:"outline"`
	StartPage    int                     `json:"start_page"`
	Mode         string                  `json:"mode"`
	Embedded     bool                    `json:"embedded"`
	ShowToolbar  bool                    `json:"show_toolbar"`
	ShowSearch   bool                    `json:"show_search"`
	ShowDownload bool                    `json:"show_download"`
	ShowBranding bool                    `json:"show_branding"`
	DownloadGate string                  `json:"download_gate"`
	Background   string                  `json:"background"`
	Capabilities publishing.Capabilities `json:"capabilities"`
	Endpoints    readerEndpoints         `json:"endpoints"`
	Share        readerShare             `json:"share"`
	CTA          *readerCTA              `json:"cta,omitempty"`
	Lead         readerGate              `json:"lead"`
	Sale         readerSale              `json:"sale"`
	Analytics    readerAnalytics         `json:"analytics"`
}

type readerViewData struct {
	Flipbook      *models.Flipbook
	Workspace     *models.Workspace
	Pages         []readerPage
	PageTexts     []string
	Client        readerClientData
	DataJSON      template.JS
	BaseURL       string
	CanonicalURL  string
	OEmbedURL     string
	OGImage       string
	MetaTitle     string
	MetaDesc      string
	SocialTitle   string
	SocialDesc    string
	Language      string
	Direction     string
	NoIndex       bool
	Embedded      bool
	ShowToolbar   bool
	ShowBranding  bool
	Branding      models.BrandingSettings
	FaviconURL    string
	PrimaryColor  string
	Background    string
	EmbedCode     string
	QRSVGURL      string
	QRPNGURL      string
	PublishedDate string
}

func (d *readerViewData) TemplateName() string {
	if d.Embedded {
		return "embed"
	}
	return "viewer"
}

func (h *ViewerHandler) buildViewData(r *http.Request, authorization *publishing.Authorization, embedded bool) (*readerViewData, error) {
	if authorization == nil {
		return nil, errors.New("missing publication authorization")
	}
	fb := authorization.Flipbook
	decision := authorization.Decision
	workspace, err := readerWorkspace(r, h.db)
	if err != nil {
		return nil, err
	}
	request := readerAccessRequest(r, h.publishing, fb.ID, embedded)
	manifestAuth, err := h.publishing.AuthorizeAsset(r.Context(), fb.WorkspaceID, fb.ID, publishing.AssetManifest, "", request)
	revision := 0
	if err == nil && manifestAuth.Revision != nil {
		revision = manifestAuth.Revision.Number
	} else if err != nil && publishing.ErrorCodeOf(err) != publishing.CodeNotFound {
		return nil, err
	}
	directory, dirErr := readerArtifactDir(h.storage, fb.ID, revision)
	var manifest *converter.Manifest
	if dirErr == nil {
		manifest, _ = converter.LoadManifest(directory)
	}
	pageCount := fb.PageCount
	if manifest != nil && manifest.PageCount > 0 {
		pageCount = manifest.PageCount
	}
	allowedPages := decision.PreviewPages
	if decision.FullAccess {
		allowedPages = pageCount
	}
	if allowedPages > pageCount {
		allowedPages = pageCount
	}
	if allowedPages < 0 {
		allowedPages = 0
	}
	var pageTexts []string
	if allowedPages > 0 {
		textRequest := request
		textRequest.Page = 1
		if _, err := h.publishing.AuthorizeAsset(r.Context(), fb.WorkspaceID, fb.ID, publishing.AssetText, "", textRequest); err != nil && publishing.ErrorCodeOf(err) != publishing.CodeNotFound {
			return nil, err
		}
		pageTexts = h.storage.LoadPageTexts(fb.ID)
	}
	customLinks := make(map[int][]readerLink)
	importedRects := make(map[int][]models.LinkRect)
	if allowedPages > 0 {
		linksRequest := request
		linksRequest.Page = 1
		if _, err := h.publishing.AuthorizeLinks(r.Context(), fb.WorkspaceID, fb.ID, linksRequest); err == nil {
			overlays, listErr := h.db.ListLinkOverlaysForEditor(r.Context(), fb.WorkspaceID, fb.ID, fb.CurrentRevisionID)
			if listErr != nil {
				return nil, listErr
			}
			importedRects = importedManifestRects(overlays)
			for _, overlay := range overlays {
				if overlay == nil || overlay.Page > allowedPages || !overlay.Enabled {
					continue
				}
				if link, ok := makeCustomReaderLink(overlay); ok {
					customLinks[overlay.Page] = append(customLinks[overlay.Page], link)
				}
			}
		} else if publishing.ErrorCodeOf(err) != publishing.CodeNotFound {
			return nil, err
		}
	}
	pages := make([]readerPage, 0, allowedPages)
	texts := make([]string, 0, allowedPages)
	pageFormat := h.storage.DetectPageFormat(fb.ID)
	for number := 1; number <= allowedPages; number++ {
		page := readerPage{Number: number, Width: fb.PageWidth, Height: fb.PageHeight, WidthPoints: float64(fb.PageWidth), HeightPoints: float64(fb.PageHeight)}
		var manifestPage *converter.PageManifest
		if manifest != nil && number <= len(manifest.Pages) {
			manifestPage = &manifest.Pages[number-1]
			page.Width, page.Height = manifestPage.Width, manifestPage.Height
			page.WidthPoints, page.HeightPoints = manifestPage.WidthPoints, manifestPage.HeightPoints
			page.Spans = manifestPage.Text
			for _, link := range manifestPage.Links {
				if converted, ok := makeManifestReaderLink(link, page.WidthPoints, page.HeightPoints); ok {
					if linkMatchesSourceRect(converted, importedRects[number]) {
						continue
					}
					page.Links = append(page.Links, converted)
				}
			}
		}
		if page.Width < 1 {
			page.Width = 1200
		}
		if page.Height < 1 {
			page.Height = 900
		}
		if page.WidthPoints <= 0 {
			page.WidthPoints = float64(page.Width)
		}
		if page.HeightPoints <= 0 {
			page.HeightPoints = float64(page.Height)
		}
		highName := fmt.Sprintf("pages/page-"+pageFormat+".png", number)
		thumbName := fmt.Sprintf("thumbs/page-"+pageFormat+".png", number)
		sourceName, sourceKind := highName, publishing.AssetPage
		thumbKind := publishing.AssetThumbnail
		if manifestPage != nil {
			if rendition, ok := chooseRendition(manifestPage.Images, "page", "png"); ok {
				highName = rendition.Path
			}
			if rendition, ok := chooseOptimizedRendition(manifestPage.Images, "page"); ok {
				sourceName, sourceKind = rendition.Path, assetKindForPath("page", rendition.Path)
			}
			if rendition, ok := chooseOptimizedRendition(manifestPage.Images, "thumbnail"); ok {
				thumbName, thumbKind = rendition.Path, assetKindForPath("thumbnail", rendition.Path)
			}
		}
		page.Source = readerAssetURL(fb.ID, revision, sourceKind, number, sourceName, request.Grant)
		page.HighSource = readerAssetURL(fb.ID, revision, publishing.AssetPage, number, highName, request.Grant)
		page.Thumbnail = readerAssetURL(fb.ID, revision, thumbKind, number, thumbName, request.Grant)
		if number-1 < len(pageTexts) {
			page.Text = pageTexts[number-1]
		}
		if page.Text == "" && manifestPage != nil {
			page.Text = joinTextSpans(manifestPage.Text)
		}
		page.Links = append(page.Links, customLinks[number]...)
		pages = append(pages, page)
		texts = append(texts, page.Text)
	}
	baseURL := h.baseURL
	if baseURL == "" {
		baseURL = distribution.RequestBaseURL("", r)
	}
	canonical := distribution.CanonicalViewerURL(baseURL, &fb, 0)
	branding := mergeReaderBranding(workspace.Settings.Branding, fb.Branding)
	embedOptions := distribution.ParseEmbedOptions(r.URL.Query(), fb.Embed, pageCount)
	embedCode, _ := distribution.EmbedCode(baseURL, &fb, embedOptions)
	shareLinks := h.authorizedShareLinks(r, &fb, request, canonical, embedded)
	startPage := 1
	if embedded {
		startPage = embedOptions.StartPage
	} else if requested := readerPositiveInt(r.URL.Query().Get("page")); requested > 0 {
		startPage = requested
	}
	if startPage > allowedPages {
		startPage = maxInt(allowedPages, 1)
	}
	mode := "single"
	if embedded {
		mode = embedOptions.ReadingMode
	}
	showBranding := !branding.HideBranding
	if embedded {
		showBranding = showBranding && embedOptions.ShowBranding
	}
	downloadGate := ""
	if !decision.Capabilities.DownloadPDF {
		switch {
		case decision.LeadGateRequired && fb.LeadGate.GateDownloads:
			downloadGate = "lead"
		case decision.PurchaseRequired:
			downloadGate = "purchase"
		}
	}
	client := readerClientData{
		ID: fb.ID, WorkspaceID: fb.WorkspaceID, Title: fb.Title, Slug: fb.Slug,
		BaseURL: baseURL, CanonicalURL: canonical, Language: distribution.Language(fb.Language),
		Direction: distribution.Direction(fb.Language), Strings: distribution.Strings(fb.Language),
		Pages: pages, PageCount: pageCount, PreviewPages: allowedPages, FullAccess: decision.FullAccess,
		StartPage: startPage, Mode: mode, Embedded: embedded, ShowToolbar: !embedded || embedOptions.ShowToolbar,
		ShowSearch: !embedded || embedOptions.ShowSearch, ShowDownload: !embedded || embedOptions.ShowDownload,
		ShowBranding: showBranding, DownloadGate: downloadGate,
		Background:   distribution.SafeColor(embedOptions.Background, distribution.SafeColor(branding.BackgroundColor, "#111827")),
		Capabilities: decision.Capabilities,
		Endpoints: readerEndpoints{
			Events: readerEndpoint(fb.ID, "events", request.Grant), Lead: readerEndpoint(fb.ID, "lead", request.Grant),
			Dismiss: readerEndpoint(fb.ID, "lead/dismiss", request.Grant), Checkout: readerEndpoint(fb.ID, "checkout", request.Grant),
			Access:   readerEndpoint(fb.ID, "access", request.Grant),
			Download: readerAddQuery(readerEndpoint(fb.ID, "download", request.Grant), "kind", "pdf"),
			Print:    readerAddQuery(readerEndpoint(fb.ID, "download", request.Grant), "kind", "print"), QR: readerEndpoint(fb.ID, "qr", request.Grant),
		},
		Share:     readerShare{Enabled: decision.Capabilities.Share, Native: fb.Share.Native, Links: shareLinks, EmbedCode: embedCode, Options: embedOptions},
		Lead:      readerGate{Required: decision.LeadGateRequired && fb.LeadGate.GateReading && allowedPages < pageCount, Dismissible: decision.LeadGateDismissible, ConsentText: fb.LeadGate.ConsentText, SuccessText: fb.LeadGate.SuccessMessage},
		Sale:      readerSale{Required: decision.PurchaseRequired && allowedPages < pageCount, PriceCents: fb.Sales.PriceCents, Currency: fb.Sales.Currency},
		Analytics: readerAnalytics{Policy: workspace.Settings.AnalyticsConsentPolicy, Consented: readerAnalyticsConsent(r), Placement: readerPlacement(embedded)},
	}
	if fb.CTA.Enabled {
		if target := distribution.SafeExternalURL(fb.CTA.URL); target != "" {
			client.CTA = &readerCTA{Text: fb.CTA.Text, URL: target, Style: fb.CTA.Style, StartPage: fb.CTA.StartPage, ShowOnEnd: fb.CTA.ShowOnEnd}
		}
	}
	if manifest != nil {
		client.Outline = filterReaderOutline(manifest.Outline, allowedPages)
	}
	encoded, err := json.Marshal(client)
	if err != nil {
		return nil, err
	}
	metaDesc := fb.SEO.Description
	if metaDesc == "" {
		metaDesc = fb.Description
	}
	if metaDesc == "" {
		metaDesc = buildDescription(fb.Title, texts)
	}
	metaTitle := fb.SEO.Title
	if metaTitle == "" {
		metaTitle = fb.Title
	}
	socialTitle := fb.SEO.SocialTitle
	if socialTitle == "" {
		socialTitle = metaTitle
	}
	socialDesc := fb.SEO.SocialDescription
	if socialDesc == "" {
		socialDesc = metaDesc
	}
	ogImage := distribution.SafeImageURL(fb.SEO.SocialImage)
	if ogImage == "" {
		ogImage = distribution.SafeImageURL(fb.SEO.CoverImage)
	}
	if ogImage == "" && len(pages) > 0 && fb.Visibility == models.VisibilityPublic {
		ogImage = readerAbsoluteAsset(baseURL, pages[0].Source)
	} else if ogImage != "" {
		ogImage = readerAbsoluteAsset(baseURL, ogImage)
	}
	qrURL := client.Endpoints.QR
	data := &readerViewData{
		Flipbook: &fb, Workspace: workspace, Pages: pages, PageTexts: texts, Client: client, DataJSON: template.JS(encoded),
		BaseURL: baseURL, CanonicalURL: canonical, OEmbedURL: baseURL + "/oembed?url=" + url.QueryEscape(canonical) + "&format=json",
		OGImage: ogImage, MetaTitle: metaTitle, MetaDesc: metaDesc, SocialTitle: socialTitle, SocialDesc: socialDesc,
		Language: client.Language, Direction: client.Direction, NoIndex: decision.NoIndex, Embedded: embedded,
		ShowToolbar: client.ShowToolbar, ShowBranding: showBranding, Branding: branding,
		FaviconURL: distribution.SafeImageURL(branding.FaviconURL), PrimaryColor: distribution.SafeColor(branding.PrimaryColor, "#6366f1"),
		Background: client.Background, EmbedCode: embedCode, QRSVGURL: readerAddQuery(qrURL, "format", "svg"), QRPNGURL: readerAddQuery(qrURL, "format", "png"),
	}
	if data.FaviconURL == "" {
		data.FaviconURL = "/static/img/favicon.svg"
	}
	if fb.PublishedAt != nil {
		data.PublishedDate = fb.PublishedAt.UTC().Format("2006-01-02")
	} else {
		data.PublishedDate = fb.CreatedAt.UTC().Format("2006-01-02")
	}
	return data, nil
}

func (h *ViewerHandler) authorizedShareLinks(r *http.Request, fb *models.Flipbook, request publishing.AccessRequest, canonical string, embedded bool) map[string]string {
	links := make(map[string]string)
	channels := []publishing.ShareChannel{publishing.ShareEmail, publishing.ShareFacebook, publishing.ShareLinkedIn, publishing.ShareX}
	for _, channel := range channels {
		if _, err := h.publishing.AuthorizeShare(r.Context(), fb.WorkspaceID, fb.ID, channel, request); err != nil {
			continue
		}
		if share, err := distribution.ShareURL(channel, canonical, fb.Title); err == nil {
			links[string(channel)] = share
		}
	}
	return links
}

func (h *ViewerHandler) renderAccessError(w http.ResponseWriter, r *http.Request, fb *models.Flipbook, err error, embedded bool) {
	status := publishing.HTTPStatus(err)
	code := publishing.ErrorCodeOf(err)
	message := publishing.PublicMessage(err)
	if errors.Is(err, database.ErrNotFound) {
		status, code, message = http.StatusNotFound, publishing.CodeNotFound, "publication not found"
	}
	if status == http.StatusOK {
		status = http.StatusNotFound
	}
	if code == "" {
		code = publishing.CodeNotFound
	}
	w.Header().Set("Cache-Control", publishing.CacheNoStore)
	w.Header().Set("X-Robots-Tag", "noindex, nofollow, noarchive")
	if embedded {
		allowFraming(w, fb)
	}
	data := map[string]any{
		"Flipbook": fb, "Embedded": embedded, "Code": code, "Message": message,
		"Status": status, "BaseURL": h.baseURL, "Language": "en", "Direction": "ltr",
	}
	if fb != nil {
		data["Language"] = distribution.Language(fb.Language)
		data["Direction"] = distribution.Direction(fb.Language)
		data["AccessURL"] = readerEndpoint(fb.ID, "access", "")
		data["ViewerURL"] = distribution.ViewerURL(h.baseURL, fb.Slug, 0)
	}
	templateName := "public_error"
	if code == publishing.CodePasswordRequired || code == publishing.CodeShareTokenRequired {
		templateName = "public_access"
	}
	if renderErr := readerRender(h.tmpl, w, templateName, data, status); renderErr != nil {
		http.Error(w, http.StatusText(status), status)
	}
}

func chooseRendition(renditions []converter.Rendition, role, format string) (converter.Rendition, bool) {
	for _, rendition := range renditions {
		if rendition.Role == role && rendition.Format == format && rendition.Path != "" {
			return rendition, true
		}
	}
	return converter.Rendition{}, false
}

func chooseOptimizedRendition(renditions []converter.Rendition, role string) (converter.Rendition, bool) {
	for _, format := range []string{"webp", "jpeg", "jpg"} {
		if rendition, ok := chooseRendition(renditions, role, format); ok {
			return rendition, true
		}
	}
	return converter.Rendition{}, false
}

func assetKindForPath(role, path string) publishing.AssetKind {
	if strings.HasPrefix(path, "renditions/") {
		return publishing.AssetRendition
	}
	if role == "thumbnail" {
		return publishing.AssetThumbnail
	}
	return publishing.AssetPage
}

func joinTextSpans(spans []converter.TextSpan) string {
	var builder strings.Builder
	for _, span := range spans {
		text := strings.TrimSpace(span.Text)
		if text == "" {
			continue
		}
		if builder.Len() > 0 {
			builder.WriteByte(' ')
		}
		builder.WriteString(text)
	}
	return builder.String()
}

func makeManifestReaderLink(link converter.Link, widthPoints, heightPoints float64) (readerLink, bool) {
	if widthPoints <= 0 || heightPoints <= 0 {
		return readerLink{}, false
	}
	result := readerLink{
		Type: link.Kind, TargetPage: link.TargetPage,
		X: link.Rect.X / widthPoints, Y: link.Rect.Y / heightPoints,
		Width: link.Rect.Width / widthPoints, Height: link.Rect.Height / heightPoints,
	}
	if link.Kind == "external" {
		result.URL = safeReaderLinkURL(link.URI)
		result.VideoEmbed = distribution.VideoEmbedURL(link.URI)
		if result.VideoEmbed != "" {
			result.Type = "video"
		}
	}
	if result.Title == "" {
		result.Title = "Open link"
	}
	if result.Type == "internal" {
		result.Title = fmt.Sprintf("Go to page %d", result.TargetPage)
	}
	return result, validReaderLink(result)
}

func makeCustomReaderLink(link *models.LinkOverlay) (readerLink, bool) {
	result := readerLink{
		ID: link.ID, Type: string(link.Type), URL: safeReaderLinkURL(link.URL), TargetPage: link.TargetPage,
		Title: link.Title, X: link.X, Y: link.Y, Width: link.Width, Height: link.Height,
		Style: link.Style,
	}
	result.VideoEmbed = distribution.VideoEmbedURL(link.URL)
	if result.Type == "video" && result.VideoEmbed == "" {
		result.Type = "external"
	}
	if result.Title == "" {
		result.Title = "Open link"
		if result.Type == "internal" {
			result.Title = fmt.Sprintf("Go to page %d", result.TargetPage)
		}
	}
	return result, validReaderLink(result)
}

func validReaderLink(link readerLink) bool {
	if link.X < 0 || link.Y < 0 || link.Width <= 0 || link.Height <= 0 || link.X+link.Width > 1.001 || link.Y+link.Height > 1.001 {
		return false
	}
	switch link.Type {
	case "internal":
		return link.TargetPage > 0
	case "external":
		return link.URL != ""
	case "video":
		return link.URL != ""
	default:
		return false
	}
}

// sourceRectTolerance is the normalized-coordinate tolerance used to match a
// manifest link against a custom overlay imported from it.
const sourceRectTolerance = 0.005

// importedManifestRects indexes the source rectangles of overlays that were
// imported from PDF manifest links, keyed by page number.
func importedManifestRects(overlays []*models.LinkOverlay) map[int][]models.LinkRect {
	rects := make(map[int][]models.LinkRect)
	for _, overlay := range overlays {
		if overlay == nil || overlay.SourceRect == nil || overlay.Page < 1 {
			continue
		}
		rects[overlay.Page] = append(rects[overlay.Page], *overlay.SourceRect)
	}
	return rects
}

// linkMatchesSourceRect reports whether a normalized manifest link rect matches
// an imported overlay's source rect, so the reader does not render both.
func linkMatchesSourceRect(link readerLink, rects []models.LinkRect) bool {
	for _, rect := range rects {
		if math.Abs(link.X-rect.X) <= sourceRectTolerance &&
			math.Abs(link.Y-rect.Y) <= sourceRectTolerance &&
			math.Abs(link.Width-rect.Width) <= sourceRectTolerance &&
			math.Abs(link.Height-rect.Height) <= sourceRectTolerance {
			return true
		}
	}
	return false
}

func safeReaderLinkURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 4096 || strings.ContainsAny(raw, "\r\n\x00") {
		return ""
	}
	if safe := distribution.SafeExternalURL(raw); safe != "" {
		return safe
	}
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "mailto" && parsed.Scheme != "tel") || parsed.Opaque == "" && parsed.Path == "" {
		return ""
	}
	return parsed.String()
}

func filterReaderOutline(items []converter.OutlineItem, allowedPages int) []converter.OutlineItem {
	result := make([]converter.OutlineItem, 0, len(items))
	for _, item := range items {
		item.Children = filterReaderOutline(item.Children, allowedPages)
		if item.TargetPage > allowedPages {
			item.TargetPage = 0
		}
		if item.TargetPage > 0 || len(item.Children) > 0 {
			result = append(result, item)
		}
	}
	return result
}

func mergeReaderBranding(workspace, publication models.BrandingSettings) models.BrandingSettings {
	result := workspace
	if publication.LogoURL != "" {
		result.LogoURL = publication.LogoURL
	}
	if publication.FaviconURL != "" {
		result.FaviconURL = publication.FaviconURL
	}
	if publication.PrimaryColor != "" {
		result.PrimaryColor = publication.PrimaryColor
	}
	if publication.BackgroundColor != "" {
		result.BackgroundColor = publication.BackgroundColor
	}
	if publication.Attribution != "" {
		result.Attribution = publication.Attribution
	}
	if publication.PublisherName != "" {
		result.PublisherName = publication.PublisherName
	}
	if publication.PublisherURL != "" {
		result.PublisherURL = publication.PublisherURL
	}
	result.HideBranding = workspace.HideBranding || publication.HideBranding
	result.LogoURL = distribution.SafeImageURL(result.LogoURL)
	result.FaviconURL = distribution.SafeImageURL(result.FaviconURL)
	result.PublisherURL = distribution.SafeExternalURL(result.PublisherURL)
	return result
}

func readerAnalyticsConsent(r *http.Request) bool {
	cookie, err := r.Cookie("flipbook_analytics_consent")
	return err == nil && cookie.Value == "yes"
}

func readerPlacement(embedded bool) string {
	if embedded {
		return "embed"
	}
	return "standalone"
}

func readerAddQuery(raw, key, value string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	query := parsed.Query()
	query.Set(key, value)
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

// buildDescription generates a meta description from the flipbook title and page text.
func buildDescription(title string, pageTexts []string) string {
	var parts []string
	charCount := 0
	for _, text := range pageTexts {
		text = strings.Join(strings.Fields(text), " ")
		if text == "" {
			continue
		}
		remaining := 300 - charCount
		if remaining <= 20 {
			break
		}
		if utf8.RuneCountInString(text) > remaining {
			runes := []rune(text)
			parts = append(parts, string(runes[:remaining])+"…")
			break
		}
		parts = append(parts, text)
		charCount += utf8.RuneCountInString(text) + 1
	}
	if len(parts) == 0 {
		return fmt.Sprintf("%s — interactive flipbook with full-page presentation slides.", title)
	}
	return strings.Join(parts, " ")
}

func readerFileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
