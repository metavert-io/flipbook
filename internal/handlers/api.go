package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/metavert-io/flipbook/internal/converter"
	"github.com/metavert-io/flipbook/internal/database"
	"github.com/metavert-io/flipbook/internal/models"
	"github.com/metavert-io/flipbook/internal/storage"
	"github.com/metavert-io/flipbook/internal/worker"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

type APIHandler struct {
	db      *database.DB
	storage *storage.Storage
	worker  *worker.Worker
	baseURL string

	maxUploadBytes int64
	purgeDelay     time.Duration
	httpClient     *http.Client
	now            func() time.Time
}

type APIOptions struct {
	MaxUploadBytes int64
	PurgeDelay     time.Duration
	HTTPClient     *http.Client
	Now            func() time.Time
}

func NewAPIHandler(db *database.DB, store *storage.Storage, w *worker.Worker, baseURL string, maxUploadSize int64) *APIHandler {
	return NewAPIHandlerWithOptions(db, store, w, baseURL, APIOptions{MaxUploadBytes: maxUploadSize})
}

func NewAPIHandlerWithOptions(db *database.DB, store *storage.Storage, w *worker.Worker, baseURL string, options APIOptions) *APIHandler {
	if options.MaxUploadBytes <= 0 {
		options.MaxUploadBytes = maxUploadSize
	}
	if options.PurgeDelay <= 0 {
		options.PurgeDelay = 30 * 24 * time.Hour
	}
	if options.HTTPClient == nil {
		options.HTTPClient = &http.Client{Timeout: 45 * time.Second}
	}
	if options.Now == nil {
		options.Now = func() time.Time { return time.Now().UTC() }
	}
	return &APIHandler{
		db: db, storage: store, worker: w, baseURL: strings.TrimRight(baseURL, "/"),
		maxUploadBytes: options.MaxUploadBytes, purgeDelay: options.PurgeDelay,
		httpClient: options.HTTPClient, now: options.Now,
	}
}

type flipbookDTO struct {
	*models.Flipbook
	ViewerURL string   `json:"viewer_url"`
	EmbedURL  string   `json:"embed_url"`
	EmbedCode string   `json:"embed_code"`
	StatusURL string   `json:"status_url"`
	Error     string   `json:"error"`
	Pages     []string `json:"pages,omitempty"`
}

func (h *APIHandler) presentFlipbook(fb *models.Flipbook, includePages bool) flipbookDTO {
	dto := flipbookDTO{
		Flipbook:  fb,
		ViewerURL: h.baseURL + "/v/" + fb.Slug,
		EmbedURL:  h.baseURL + "/embed/" + fb.Slug,
		EmbedCode: embedCode(h.baseURL, fb.Slug),
		StatusURL: h.baseURL + "/api/flipbooks/" + fb.ID + "/status",
		Error:     fb.ErrorMessage,
	}
	if includePages {
		format := h.storage.DetectPageFormat(fb.ID)
		dto.Pages = make([]string, 0, fb.PageCount)
		for page := 1; page <= fb.PageCount; page++ {
			dto.Pages = append(dto.Pages, h.absoluteAPIURL(h.storage.PageImageURL(fb.ID, format, page)))
		}
	}
	return dto
}

func (h *APIHandler) absoluteAPIURL(value string) string {
	if strings.HasPrefix(value, "http://") || strings.HasPrefix(value, "https://") || h.baseURL == "" {
		return value
	}
	if strings.HasPrefix(value, "/") {
		return h.baseURL + value
	}
	return h.baseURL + "/" + value
}

func (h *APIHandler) ListFlipbooks(w http.ResponseWriter, r *http.Request) {
	identity, err := h.identity(r, models.RoleReader)
	if err != nil {
		apiError(w, err)
		return
	}
	items, _, err := h.db.SearchFlipbooks(r.Context(), database.FlipbookFilter{
		WorkspaceID: identity.WorkspaceID, Limit: 500, Sort: "created_at", Descending: true,
	})
	if err != nil {
		apiError(w, err)
		return
	}
	type item struct {
		ID        string `json:"id"`
		Title     string `json:"title"`
		Slug      string `json:"slug"`
		Status    string `json:"status"`
		PageCount int    `json:"page_count"`
		ViewerURL string `json:"viewer_url"`
		EmbedURL  string `json:"embed_url"`
		CreatedAt string `json:"created_at"`
	}
	result := make([]item, 0, len(items))
	for _, fb := range items {
		result = append(result, item{
			ID: fb.ID, Title: fb.Title, Slug: fb.Slug, Status: fb.Status,
			PageCount: fb.PageCount, ViewerURL: h.baseURL + "/v/" + fb.Slug,
			EmbedURL: h.baseURL + "/embed/" + fb.Slug, CreatedAt: fb.CreatedAt.Format(time.RFC3339),
		})
	}
	jsonResponse(w, result)
}

func (h *APIHandler) GetFlipbook(w http.ResponseWriter, r *http.Request) {
	identity, err := h.identity(r, models.RoleReader)
	if err != nil {
		apiError(w, err)
		return
	}
	fb, err := h.db.GetFlipbookInWorkspace(r.Context(), identity.WorkspaceID, chi.URLParam(r, "id"))
	if err != nil {
		apiError(w, err)
		return
	}
	h.emitConversionReady(r, fb)
	jsonResponse(w, h.presentFlipbook(fb, r.URL.Query().Get("include_pages") != "false"))
}

type uploadMetadata struct {
	Title          string
	Slug           string
	Description    string
	Language       string
	Visibility     models.Visibility
	Tags           []string
	CollectionIDs  []string
	ExternalSource string
	ExternalID     string
}

func metadataFromMultipart(r *http.Request) uploadMetadata {
	source := strings.TrimSpace(r.FormValue("external_source"))
	externalID := strings.TrimSpace(r.FormValue("external_id"))
	if source == "" && externalID != "" {
		source = "api"
	}
	if key := strings.TrimSpace(r.Header.Get("Idempotency-Key")); key != "" && externalID == "" {
		source, externalID = "idempotency", key
	}
	visibility := models.Visibility(strings.TrimSpace(r.FormValue("visibility")))
	return uploadMetadata{
		Title: strings.TrimSpace(r.FormValue("title")), Slug: models.NormalizeSlug(r.FormValue("slug")),
		Description: r.FormValue("description"), Language: strings.TrimSpace(r.FormValue("language")),
		Visibility: visibility, Tags: apiStringList(r.FormValue("tags")),
		CollectionIDs: apiStringList(r.FormValue("collection_ids")), ExternalSource: source, ExternalID: externalID,
	}
}

func (h *APIHandler) idempotentFlipbook(r *http.Request, metadata uploadMetadata) (*models.Flipbook, error) {
	if metadata.ExternalID == "" {
		return nil, nil
	}
	fb, err := h.db.GetFlipbookByExternalID(r.Context(), requestedAPIWorkspace(r, models.DefaultWorkspaceID), metadata.ExternalSource, metadata.ExternalID)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}
	return fb, err
}

func (h *APIHandler) UploadFlipbook(w http.ResponseWriter, r *http.Request) {
	identity, err := h.identity(r, models.RoleEditor)
	if err != nil {
		apiError(w, err)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, h.maxUploadBytes+(1<<20))
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			jsonError(w, "Uploaded file exceeds the configured size limit", http.StatusRequestEntityTooLarge)
		} else {
			invalidAPIRequest(w, "Invalid multipart upload")
		}
		return
	}
	metadata := metadataFromMultipart(r)
	if existing, err := h.db.GetFlipbookByExternalID(r.Context(), identity.WorkspaceID, metadata.ExternalSource, metadata.ExternalID); err == nil && metadata.ExternalID != "" {
		w.Header().Set("Idempotent-Replay", "true")
		jsonResponse(w, h.presentFlipbook(existing, false))
		return
	} else if err != nil && !errors.Is(err, mongo.ErrNoDocuments) {
		apiError(w, err)
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		invalidAPIRequest(w, "No file provided")
		return
	}
	defer file.Close()
	if metadata.Title == "" {
		metadata.Title = strings.TrimSuffix(filepath.Base(header.Filename), filepath.Ext(header.Filename))
	}
	fb, job, err := h.createFlipbookFromSource(r, identity, header.Filename, file, metadata)
	if err != nil {
		apiError(w, err)
		return
	}
	h.audit(r, identity, "flipbook.create", "flipbook", fb.ID, map[string]string{"external_id": fb.ExternalID})
	h.emitWebhook(r, identity.WorkspaceID, "conversion.queued", fb.ID, "conversion.queued:"+fb.CurrentRevisionID, h.presentFlipbook(fb, false))
	response := map[string]any{
		"id": fb.ID, "title": fb.Title, "slug": fb.Slug, "status": fb.Status,
		"status_url":  h.baseURL + "/api/flipbooks/" + fb.ID + "/status",
		"revision_id": fb.CurrentRevisionID, "external_source": fb.ExternalSource, "external_id": fb.ExternalID,
	}
	if job != nil {
		response["job_id"] = job.ID
	}
	w.Header().Set("Location", "/api/v1/flipbooks/"+fb.ID)
	jsonStatus(w, http.StatusCreated, response)
}

func (h *APIHandler) createFlipbookFromSource(r *http.Request, identity *apiIdentity, filename string, source ioReader, metadata uploadMetadata) (*models.Flipbook, *models.Job, error) {
	filename = filepath.Base(strings.TrimSpace(filename))
	if filename == "" || filename == "." {
		return nil, nil, apiInvalidError("a source filename is required")
	}
	if err := converter.ValidateSourceFormat(filename); err != nil {
		return nil, nil, apiInvalid(fmt.Errorf("unsupported source format: %w", err))
	}
	if strings.TrimSpace(metadata.Title) == "" {
		return nil, nil, apiInvalidError("title is required")
	}
	if len(metadata.Title) > 500 || len(metadata.Slug) > 200 {
		return nil, nil, apiInvalidError("title or slug exceeds its length limit")
	}
	if (metadata.ExternalSource == "") != (metadata.ExternalID == "") {
		return nil, nil, apiInvalidError("external_source and external_id must be provided together")
	}
	if len(metadata.ExternalSource) > 100 || len(metadata.ExternalID) > 300 {
		return nil, nil, apiInvalidError("external identifiers exceed their length limits")
	}
	if metadata.ExternalID != "" {
		existing, err := h.db.GetFlipbookByExternalID(r.Context(), identity.WorkspaceID, metadata.ExternalSource, metadata.ExternalID)
		if err == nil {
			return existing, nil, nil
		}
		if !errors.Is(err, mongo.ErrNoDocuments) {
			return nil, nil, err
		}
	}
	tags, err := normalizeAPITags(metadata.Tags)
	if err != nil {
		return nil, nil, apiInvalid(err)
	}
	for _, collectionID := range metadata.CollectionIDs {
		if _, err := h.db.GetCollection(r.Context(), identity.WorkspaceID, collectionID); err != nil {
			return nil, nil, err
		}
	}
	slug := metadata.Slug
	if slug == "" {
		slug = models.NormalizeSlug(metadata.Title)
	}
	slug, err = h.db.GenerateUniqueSlug(r.Context(), identity.WorkspaceID, slug)
	if err != nil {
		return nil, nil, err
	}
	if metadata.Slug != "" && slug != metadata.Slug {
		return nil, nil, database.ErrConflict
	}
	if metadata.Language == "" {
		metadata.Language = "en"
	}
	// An empty visibility is left for the workspace default to fill in.
	switch metadata.Visibility {
	case "", models.VisibilityPublic, models.VisibilityUnlisted, models.VisibilityPrivate, models.VisibilityTeam, models.VisibilityPassword, models.VisibilityToken:
	default:
		return nil, nil, apiInvalidError("unsupported visibility")
	}
	workspace, err := h.db.GetWorkspace(r.Context(), identity.WorkspaceID)
	if err != nil {
		return nil, nil, err
	}
	id := uuid.NewString()
	upload, err := h.storage.SaveUploadStream(r.Context(), id, filename, source, h.maxUploadBytes)
	if err != nil {
		return nil, nil, err
	}
	fb := newAPIFlipbook(workspace, identity, id, filename, slug, upload, tags, metadata)
	if err := h.db.CreateFlipbookContext(r.Context(), fb); err != nil {
		_ = h.storage.DeleteFlipbookContext(context.Background(), id)
		return nil, nil, err
	}
	for _, collectionID := range metadata.CollectionIDs {
		if err := h.db.AddFlipbookToCollection(r.Context(), identity.WorkspaceID, collectionID, id); err != nil {
			log.Printf("API collection membership %s/%s failed: %v", collectionID, id, err)
		}
	}
	job, err := h.worker.EnqueueContext(r.Context(), worker.Job{
		FlipbookID: id, WorkspaceID: identity.WorkspaceID, SourcePath: upload.Path,
		IdempotencyKey: "convert:" + fb.CurrentRevisionID,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("queue conversion: %w", err)
	}
	return fb, job, nil
}

// newAPIFlipbook builds the record for an API-created publication. Download,
// embed and share come from the workspace defaults exactly as for admin
// uploads; visibility only when the caller did not send one.
func newAPIFlipbook(workspace *models.Workspace, identity *apiIdentity, id, filename, slug string, upload storage.UploadInfo, tags []string, metadata uploadMetadata) *models.Flipbook {
	fb := &models.Flipbook{
		ID: id, WorkspaceID: identity.WorkspaceID, OwnerID: identity.UserID,
		Title: strings.TrimSpace(metadata.Title), Slug: slug, Description: metadata.Description,
		Filename: filename, FileSize: upload.Size, SourceChecksum: upload.SHA256,
		Status: models.StatusPending, PublicationState: models.PublicationDraft,
		Visibility: metadata.Visibility, Tags: tags, CollectionIDs: metadata.CollectionIDs,
		ExternalSource: metadata.ExternalSource, ExternalID: metadata.ExternalID, Language: metadata.Language,
	}
	workspace.NewFlipbookDefaults().Apply(fb)
	return fb
}

// ioReader is kept local to the upload contract so migration clients can use
// any streaming source without buffering the document in memory.
type ioReader interface {
	Read([]byte) (int, error)
}

func (h *APIHandler) FlipbookStatus(w http.ResponseWriter, r *http.Request) {
	identity, err := h.identity(r, models.RoleReader)
	if err != nil {
		apiError(w, err)
		return
	}
	fb, err := h.db.GetFlipbookInWorkspace(r.Context(), identity.WorkspaceID, chi.URLParam(r, "id"))
	if err != nil {
		apiError(w, err)
		return
	}
	h.emitConversionReady(r, fb)
	response := map[string]any{
		"id": fb.ID, "status": fb.Status, "slug": fb.Slug, "page_count": fb.PageCount,
		"error": fb.ErrorMessage, "publication_state": fb.PublicationState,
		"current_revision_id": fb.CurrentRevisionID, "asset_version": fb.AssetVersion,
	}
	if revision, revisionErr := h.db.GetCurrentRevision(r.Context(), identity.WorkspaceID, fb.ID); revisionErr == nil {
		response["revision"] = revision
	}
	jsonResponse(w, response)
}

var apiGoogleSlidesRe = regexp.MustCompile(`/presentation/d/([a-zA-Z0-9_-]+)`)

func (h *APIHandler) ImportURL(w http.ResponseWriter, r *http.Request) {
	identity, err := h.identity(r, models.RoleEditor)
	if err != nil {
		apiError(w, err)
		return
	}
	if err := r.ParseForm(); err != nil {
		invalidAPIRequest(w, "Invalid import request")
		return
	}
	rawURL := strings.TrimSpace(r.FormValue("url"))
	matches := apiGoogleSlidesRe.FindStringSubmatch(rawURL)
	if len(matches) < 2 {
		invalidAPIRequest(w, "A valid public Google Slides URL is required")
		return
	}
	metadata := metadataFromMultipart(r)
	if metadata.Title == "" {
		metadata.Title = "Google Slides Import"
	}
	if metadata.ExternalID != "" {
		existing, getErr := h.db.GetFlipbookByExternalID(r.Context(), identity.WorkspaceID, metadata.ExternalSource, metadata.ExternalID)
		if getErr == nil {
			w.Header().Set("Idempotent-Replay", "true")
			jsonResponse(w, h.presentFlipbook(existing, false))
			return
		}
		if !errors.Is(getErr, mongo.ErrNoDocuments) {
			apiError(w, getErr)
			return
		}
	}
	exportURL := fmt.Sprintf("https://docs.google.com/presentation/d/%s/export/pdf", matches[1])
	request, err := http.NewRequestWithContext(r.Context(), http.MethodGet, exportURL, nil)
	if err != nil {
		apiError(w, err)
		return
	}
	response, err := h.httpClient.Do(request)
	if err != nil {
		jsonError(w, "Failed to download the publicly accessible presentation", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		jsonError(w, "Failed to download the publicly accessible presentation", http.StatusBadGateway)
		return
	}
	if response.ContentLength > h.maxUploadBytes {
		jsonError(w, "Remote document exceeds the configured size limit", http.StatusRequestEntityTooLarge)
		return
	}
	fb, job, err := h.createFlipbookFromSource(r, identity, "import.pdf", response.Body, metadata)
	if err != nil {
		apiError(w, err)
		return
	}
	h.audit(r, identity, "flipbook.import", "flipbook", fb.ID, map[string]string{"provider": "google_slides"})
	h.emitWebhook(r, identity.WorkspaceID, "conversion.queued", fb.ID, "conversion.queued:"+fb.CurrentRevisionID, h.presentFlipbook(fb, false))
	result := map[string]any{"id": fb.ID, "title": fb.Title, "slug": fb.Slug, "status": fb.Status, "status_url": h.baseURL + "/api/flipbooks/" + fb.ID + "/status"}
	if job != nil {
		result["job_id"] = job.ID
	}
	jsonStatus(w, http.StatusCreated, result)
}

func (h *APIHandler) DeleteFlipbook(w http.ResponseWriter, r *http.Request) {
	identity, err := h.identity(r, models.RoleAdmin)
	if err != nil {
		apiError(w, err)
		return
	}
	if _, err := h.scheduleFlipbookPurge(r, identity, chi.URLParam(r, "id")); err != nil {
		apiError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func jsonResponse(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(data)
}

func jsonStatus(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
