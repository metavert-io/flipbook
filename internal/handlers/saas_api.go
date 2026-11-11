package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/metavert-io/flipbook/internal/database"
	"github.com/metavert-io/flipbook/internal/models"
	"github.com/metavert-io/flipbook/internal/saas"
	"github.com/metavert-io/flipbook/internal/storage"
	"github.com/metavert-io/flipbook/internal/worker"
)

// SaaSAPIHandler handles tenant-scoped flipbook operations for SaaS mode.
type SaaSAPIHandler struct {
	db            *database.DB
	storage       *storage.Storage
	worker        *worker.Worker
	billing       *saas.BillingChecker
	baseURL       string
	maxUploadSize int64
}

func NewSaaSAPIHandler(db *database.DB, store *storage.Storage, w *worker.Worker, billing *saas.BillingChecker, baseURL string, maxUploadSize int64) *SaaSAPIHandler {
	return &SaaSAPIHandler{db: db, storage: store, worker: w, billing: billing, baseURL: baseURL, maxUploadSize: maxUploadSize}
}

// ListFlipbooks returns flipbooks owned by the current tenant.
func (h *SaaSAPIHandler) ListFlipbooks(w http.ResponseWriter, r *http.Request) {
	tenantID := saas.TenantIDFromContext(r.Context())

	flipbooks, err := h.db.ListFlipbooksByTenant(tenantID)
	if err != nil {
		jsonError(w, "Failed to list flipbooks", 500)
		return
	}

	type item struct {
		ID           string `json:"id"`
		Title        string `json:"title"`
		Slug         string `json:"slug"`
		Status       string `json:"status"`
		PageCount    int    `json:"pageCount"`
		ViewerURL    string `json:"viewerUrl"`
		EmbedURL     string `json:"embedUrl"`
		ThumbnailURL string `json:"thumbnailUrl,omitempty"`
		CreatedAt    string `json:"createdAt"`
	}

	items := make([]item, 0, len(flipbooks))
	for _, fb := range flipbooks {
		var thumb string
		if fb.PageCount > 0 && fb.Status == "ready" {
			pageFmt := h.storage.DetectPageFormat(fb.ID)
			thumb = h.baseURL + h.storage.ThumbImageURL(fb.ID, pageFmt, 1)
		}
		items = append(items, item{
			ID:           fb.ID,
			Title:        fb.Title,
			Slug:         fb.Slug,
			Status:       fb.Status,
			PageCount:    fb.PageCount,
			ViewerURL:    h.baseURL + "/v/" + fb.Slug,
			EmbedURL:     h.baseURL + "/embed/" + fb.Slug,
			ThumbnailURL: thumb,
			CreatedAt:    fb.CreatedAt.Format("2006-01-02T15:04:05Z"),
		})
	}

	jsonResponse(w, map[string]interface{}{
		"flipbooks": items,
	})
}

// GetFlipbook returns a single flipbook owned by the current tenant.
func (h *SaaSAPIHandler) GetFlipbook(w http.ResponseWriter, r *http.Request) {
	tenantID := saas.TenantIDFromContext(r.Context())
	id := chi.URLParam(r, "id")

	fb, err := h.db.GetFlipbookForTenant(id, tenantID)
	if err != nil {
		jsonError(w, "Flipbook not found", 404)
		return
	}

	pageFmt := h.storage.DetectPageFormat(fb.ID)
	var pages []string
	for i := 1; i <= fb.PageCount; i++ {
		pages = append(pages, h.baseURL+h.storage.PageImageURL(fb.ID, pageFmt, i))
	}

	jsonResponse(w, map[string]interface{}{
		"id":        fb.ID,
		"title":     fb.Title,
		"slug":      fb.Slug,
		"status":    fb.Status,
		"pageCount": fb.PageCount,
		"width":     fb.PageWidth,
		"height":    fb.PageHeight,
		"viewerUrl": h.baseURL + "/v/" + fb.Slug,
		"embedUrl":  h.baseURL + "/embed/" + fb.Slug,
		"embedCode": embedCode(h.baseURL, fb.Slug),
		"pages":     pages,
		"error":     fb.ErrorMessage,
		"createdAt": fb.CreatedAt.Format("2006-01-02T15:04:05Z"),
	})
}

// Upload handles file upload for the current tenant with plan enforcement.
func (h *SaaSAPIHandler) Upload(w http.ResponseWriter, r *http.Request) {
	tenant := saas.TenantFromContext(r.Context())
	plan := saas.PlanFromContext(r.Context())

	// Check billing status
	if tenant.BillingStatus != "active" && !tenant.BillingWaived {
		jsonError(w, "Active subscription required. Please select a plan.", 402)
		return
	}

	// Check page limit
	canUpload, used, limit := h.billing.CanUpload(r.Context(), tenant, plan)
	if !canUpload {
		resp := map[string]interface{}{
			"error":     "Page limit reached for your plan.",
			"used":      used,
			"limit":     limit,
			"upgradeAt": "/plan",
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(resp)
		return
	}

	// Allow 1MB for multipart framing and form fields on top of the file.
	r.Body = http.MaxBytesReader(w, r.Body, h.maxUploadSize+(1<<20))

	// Keep at most 32MB in memory; larger files spill to a temp file instead
	// of being buffered whole in RAM.
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		jsonError(w, fmt.Sprintf("File too large (max %dMB)", h.maxUploadSize/(1<<20)), 400)
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		jsonError(w, "No file provided", 400)
		return
	}
	defer file.Close()

	ext := strings.ToLower(filepath.Ext(header.Filename))
	if ext != ".pptx" && ext != ".ppt" && ext != ".pdf" {
		jsonError(w, "Only .pptx, .ppt, and .pdf files are supported", 400)
		return
	}

	tenantID := saas.TenantIDFromContext(r.Context())
	id := uuid.New().String()
	title := r.FormValue("title")
	if title == "" {
		title = strings.TrimSuffix(header.Filename, filepath.Ext(header.Filename))
	}
	slug := slugify(title)
	slug = h.db.EnsureUniqueSlug(slug)

	srcPath, err := h.storage.SaveUpload(id, header.Filename, file)
	if err != nil {
		log.Printf("Failed to save upload: %v", err)
		jsonError(w, "Failed to save file", 500)
		return
	}

	fb := &models.Flipbook{
		ID:       id,
		Title:    title,
		Slug:     slug,
		Filename: header.Filename,
		FileSize: header.Size,
		Status:   models.StatusPending,
		TenantID: tenantID,
		Embed:    models.DefaultEmbedSettings(),
	}
	if err := h.db.CreateFlipbook(fb); err != nil {
		log.Printf("Failed to create flipbook: %v", err)
		jsonError(w, "Failed to create flipbook", 500)
		return
	}

	// GridFS backup
	go func() {
		f, err := os.Open(srcPath)
		if err != nil {
			log.Printf("GridFS: failed to open original for %s: %v", id, err)
			return
		}
		defer f.Close()
		gridfsID, err := h.db.UploadToGridFS(context.Background(), header.Filename, f)
		if err != nil {
			log.Printf("GridFS: upload failed for %s: %v", id, err)
			return
		}
		if err := h.db.SetGridFSFileID(id, gridfsID); err != nil {
			log.Printf("GridFS: failed to save file ID for %s: %v", id, err)
		}
	}()

	h.worker.Enqueue(worker.Job{FlipbookID: id, SourcePath: srcPath})

	w.WriteHeader(http.StatusCreated)
	jsonResponse(w, map[string]interface{}{
		"id":        id,
		"title":     title,
		"slug":      slug,
		"status":    "pending",
		"statusUrl": h.baseURL + "/api/flipbooks/" + id + "/status",
	})
}

// DeleteFlipbook deletes a flipbook owned by the current tenant.
func (h *SaaSAPIHandler) DeleteFlipbook(w http.ResponseWriter, r *http.Request) {
	tenantID := saas.TenantIDFromContext(r.Context())
	id := chi.URLParam(r, "id")

	fb, err := h.db.GetFlipbookForTenant(id, tenantID)
	if err != nil {
		jsonError(w, "Flipbook not found", 404)
		return
	}

	h.db.DeleteFlipbook(id)
	h.storage.DeleteFlipbook(id)
	if fb.GridFSFileID != "" {
		go func() {
			if err := h.db.DeleteFromGridFS(context.Background(), fb.GridFSFileID); err != nil {
				log.Printf("GridFS: cleanup failed for %s: %v", id, err)
			}
		}()
	}
	w.WriteHeader(http.StatusNoContent)
}

// UpdateFlipbook updates metadata for a flipbook owned by the current tenant.
func (h *SaaSAPIHandler) UpdateFlipbook(w http.ResponseWriter, r *http.Request) {
	tenantID := saas.TenantIDFromContext(r.Context())
	id := chi.URLParam(r, "id")

	_, err := h.db.GetFlipbookForTenant(id, tenantID)
	if err != nil {
		jsonError(w, "Flipbook not found", 404)
		return
	}

	var body struct {
		Title       string `json:"title"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "Invalid request body", 400)
		return
	}

	if err := h.db.UpdateFlipbook(id, body.Title, body.Description); err != nil {
		jsonError(w, "Failed to update flipbook", 500)
		return
	}
	jsonResponse(w, map[string]string{"status": "ok"})
}

// Usage returns page usage stats for the current tenant.
func (h *SaaSAPIHandler) Usage(w http.ResponseWriter, r *http.Request) {
	tenant := saas.TenantFromContext(r.Context())
	plan := saas.PlanFromContext(r.Context())

	used, err := h.billing.CountTenantPages(r.Context(), tenant.ID.Hex())
	if err != nil {
		jsonError(w, "Failed to count pages", 500)
		return
	}

	limit := saas.GetPageLimit(plan)

	planName := "Free"
	if plan != nil {
		planName = plan.Name
	}

	jsonResponse(w, map[string]interface{}{
		"pagesUsed":  used,
		"pagesLimit": limit,
		"planName":   planName,
	})
}
