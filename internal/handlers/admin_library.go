package handlers

import (
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"mime/multipart"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/metavert-io/flipbook/internal/converter"
	"github.com/metavert-io/flipbook/internal/database"
	"github.com/metavert-io/flipbook/internal/models"
	"github.com/metavert-io/flipbook/internal/storage"
	"github.com/metavert-io/flipbook/internal/worker"
)

type adminPaginationLink struct {
	Label    string
	URL      string
	Current  bool
	Ellipsis bool
}

type adminPagination struct {
	Page        int
	Pages       int
	PerPage     int
	Total       int64
	PreviousURL string
	NextURL     string
	Links       []adminPaginationLink
}

type adminUploadItem struct {
	ID        string `json:"id,omitempty"`
	Title     string `json:"title"`
	Slug      string `json:"slug,omitempty"`
	Filename  string `json:"filename"`
	Status    string `json:"status,omitempty"`
	StatusURL string `json:"status_url,omitempty"`
	ManageURL string `json:"manage_url,omitempty"`
	Error     string `json:"error,omitempty"`
}

func (h *AdminHandler) Index(w http.ResponseWriter, r *http.Request) {
	actor, err := adminActorFromRequest(r, models.RoleReader)
	if err != nil {
		h.renderError(w, r, err)
		return
	}
	query := r.URL.Query()
	page, err := adminQueryInteger(query.Get("page"), 1, 1, 1000000)
	if err != nil {
		h.renderError(w, r, err)
		return
	}
	perPage, err := adminQueryInteger(query.Get("per_page"), 24, 12, 96)
	if err != nil {
		h.renderError(w, r, err)
		return
	}
	sortField := query.Get("sort")
	switch sortField {
	case "", "created_at", "updated_at", "title", "published_at", "page_count":
	default:
		h.renderError(w, r, adminInputf("Unsupported publication sort field."))
		return
	}
	filter := databaseFlipbookFilter(actor.WorkspaceID, query)
	filter.Sort = sortField
	filter.Descending = query.Get("direction") != "asc"
	filter.Limit = int64(perPage)
	filter.Offset = int64((page - 1) * perPage)
	flipbooks, total, err := h.db.SearchFlipbooks(r.Context(), filter)
	if err != nil {
		h.renderError(w, r, err)
		return
	}
	pages := int(math.Ceil(float64(total) / float64(perPage)))
	if pages < 1 {
		pages = 1
	}
	if page > pages {
		page = pages
		filter.Offset = int64((page - 1) * perPage)
		flipbooks, _, err = h.db.SearchFlipbooks(r.Context(), filter)
		if err != nil {
			h.renderError(w, r, err)
			return
		}
	}
	collections, err := h.db.ListCollections(r.Context(), actor.WorkspaceID, "")
	if err != nil {
		h.renderError(w, r, err)
		return
	}
	thumbnails := make(map[string]string)
	for _, flipbook := range flipbooks {
		if flipbook.Status == models.StatusReady && flipbook.PageCount > 0 {
			format := h.storage.DetectPageFormat(flipbook.ID)
			thumbnails[flipbook.ID] = h.storage.ThumbImageURL(flipbook.ID, format, 1)
		}
	}
	filters := map[string]string{}
	for _, key := range []string{"q", "status", "state", "visibility", "tag", "collection", "view", "sort", "direction"} {
		filters[key] = query.Get(key)
	}
	data := map[string]any{
		"PageTitle":   "Library - Penninn Midlun",
		"Flipbooks":   flipbooks,
		"ThumbMap":    thumbnails,
		"Collections": collections,
		"Filters":     filters,
		"Pagination":  adminPaginationFor(r.URL, page, pages, perPage, total),
		"ReturnTo":    safeAdminReturn(r.URL.RequestURI(), "/admin"),
	}
	if err := h.render(w, r, actor, "library", "admin_index", http.StatusOK, data); err != nil {
		h.renderError(w, r, err)
	}
}

func adminQueryInteger(raw string, fallback, minimum, maximum int) (int, error) {
	if strings.TrimSpace(raw) == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < minimum || value > maximum {
		return 0, adminInputf("Page values must be between %d and %d.", minimum, maximum)
	}
	return value, nil
}

func databaseFlipbookFilter(workspaceID string, query url.Values) database.FlipbookFilter {
	filter := database.FlipbookFilter{
		WorkspaceID: workspaceID,
		Query:       strings.TrimSpace(query.Get("q")), Status: query.Get("status"),
		PublicationState: models.PublicationState(query.Get("state")),
		Visibility:       models.Visibility(query.Get("visibility")),
		CollectionID:     query.Get("collection"), Sort: query.Get("sort"),
	}
	if tag := strings.TrimSpace(query.Get("tag")); tag != "" {
		filter.Tags = []string{tag}
	}
	switch query.Get("view") {
	case "archived":
		value := true
		filter.Archived = &value
	case "all":
	default:
		value := false
		filter.Archived = &value
	}
	return filter
}

func adminPaginationFor(rawURL *url.URL, page, pages, perPage int, total int64) adminPagination {
	copyURL := *rawURL
	withPage := func(target int) string {
		query := copyURL.Query()
		query.Set("page", strconv.Itoa(target))
		query.Set("per_page", strconv.Itoa(perPage))
		copyURL.RawQuery = query.Encode()
		return copyURL.RequestURI()
	}
	pagination := adminPagination{Page: page, Pages: pages, PerPage: perPage, Total: total}
	if page > 1 {
		pagination.PreviousURL = withPage(page - 1)
	}
	if page < pages {
		pagination.NextURL = withPage(page + 1)
	}
	candidates := []int{}
	for _, candidate := range []int{1, page - 2, page - 1, page, page + 1, page + 2, pages} {
		if candidate >= 1 && candidate <= pages {
			candidates = append(candidates, candidate)
		}
	}
	last := 0
	for _, candidate := range candidates {
		if candidate <= last {
			continue
		}
		if last > 0 && candidate > last+1 {
			pagination.Links = append(pagination.Links, adminPaginationLink{Ellipsis: true, Label: "…"})
		}
		pagination.Links = append(pagination.Links, adminPaginationLink{Label: strconv.Itoa(candidate), URL: withPage(candidate), Current: candidate == page})
		last = candidate
	}
	return pagination
}

func (h *AdminHandler) UploadForm(w http.ResponseWriter, r *http.Request) {
	actor, err := adminActorFromRequest(r, models.RoleEditor)
	if err != nil {
		h.renderError(w, r, err)
		return
	}
	h.renderUpload(w, r, actor, http.StatusOK, "")
}

func (h *AdminHandler) renderUpload(w http.ResponseWriter, r *http.Request, actor adminActor, status int, errorMessage string) {
	data := map[string]any{
		"PageTitle": "Upload publications - Penninn Midlun",
		"Formats":   strings.Join(converter.SupportedExtensions(), ", "),
		"MaxFiles":  50,
	}
	if errorMessage != "" {
		data["Error"] = errorMessage
	}
	if err := h.render(w, r, actor, "upload", "admin_upload", status, data); err != nil {
		h.renderError(w, r, err)
	}
}

func (h *AdminHandler) Upload(w http.ResponseWriter, r *http.Request) {
	actor, err := adminActorFromRequest(r, models.RoleEditor)
	if err != nil {
		h.renderError(w, r, err)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, h.maxBytes+1<<20)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		h.uploadError(w, r, actor, adminInputf("The upload could not be parsed. Check the total upload size and try again."))
		return
	}
	if uploadID := strings.TrimSpace(r.FormValue("upload_id")); uploadID != "" {
		h.uploadFromSession(w, r, actor, uploadID)
		return
	}
	if r.MultipartForm == nil {
		h.uploadError(w, r, actor, adminInputf("Choose at least one document to upload."))
		return
	}
	headers := append([]*multipart.FileHeader(nil), r.MultipartForm.File["file"]...)
	headers = append(headers, r.MultipartForm.File["files"]...)
	if len(headers) == 0 {
		h.uploadError(w, r, actor, adminInputf("Choose at least one document to upload."))
		return
	}
	if len(headers) > 50 {
		h.uploadError(w, r, actor, adminInputf("Upload at most 50 documents at a time."))
		return
	}
	items := make([]adminUploadItem, 0, len(headers))
	created := 0
	for _, header := range headers {
		title := ""
		if len(headers) == 1 {
			title = r.FormValue("title")
		}
		file, openErr := header.Open()
		if openErr != nil {
			items = append(items, adminUploadItem{Filename: header.Filename, Title: header.Filename, Error: "The file could not be read."})
			continue
		}
		item, saveErr := h.saveNewFlipbook(r, actor, title, header.Filename, file)
		closeErr := file.Close()
		if saveErr == nil && closeErr != nil {
			saveErr = closeErr
		}
		if saveErr != nil {
			_, message := adminErrorMessage(saveErr)
			item.Error = message
		}
		if item.ID != "" {
			created++
		}
		items = append(items, item)
	}
	h.respondUploadResults(w, r, actor, items, created)
}

// uploadFromSession creates a publication from a completed resumable upload.
// The session is removed afterwards whatever the outcome, so a failed finish
// never leaves a full-size document behind on disk.
func (h *AdminHandler) uploadFromSession(w http.ResponseWriter, r *http.Request, actor adminActor, uploadID string) {
	session, file, err := h.completedUploadSession(actor, uploadID)
	if err != nil {
		if errors.Is(err, storage.ErrUploadSessionNotFound) || errors.Is(err, storage.ErrUploadIncomplete) {
			h.uploadError(w, r, actor, adminInputf("The upload is incomplete or has expired. Upload the document again."))
			return
		}
		h.uploadError(w, r, actor, err)
		return
	}
	defer func() {
		if err := h.storage.DeleteUploadSession(uploadID); err != nil {
			log.Printf("upload sessions: delete %s: %v", uploadID, err)
		}
	}()
	item, saveErr := h.saveNewFlipbook(r, actor, r.FormValue("title"), session.Filename, file)
	closeErr := file.Close()
	if saveErr == nil && closeErr != nil {
		saveErr = closeErr
	}
	if saveErr != nil {
		_, message := adminErrorMessage(saveErr)
		item.Error = message
	}
	created := 0
	if item.ID != "" {
		created = 1
	}
	h.respondUploadResults(w, r, actor, []adminUploadItem{item}, created)
}

func (h *AdminHandler) uploadError(w http.ResponseWriter, r *http.Request, actor adminActor, err error) {
	status, message := adminErrorMessage(err)
	if adminWantsJSON(r) {
		adminJSONError(w, message, status)
		return
	}
	h.renderUpload(w, r, actor, status, message)
}

func (h *AdminHandler) respondUploadResults(w http.ResponseWriter, r *http.Request, actor adminActor, items []adminUploadItem, created int) {
	failures := len(items) - created
	if created == 0 && failures > 0 {
		message := items[0].Error
		if adminWantsJSON(r) {
			adminJSONResponse(w, http.StatusBadRequest, map[string]any{"items": items, "error": message})
			return
		}
		h.renderUpload(w, r, actor, http.StatusBadRequest, message)
		return
	}
	if adminWantsJSON(r) {
		status := http.StatusCreated
		if failures > 0 {
			status = http.StatusMultiStatus
		}
		response := map[string]any{"items": items, "created": created, "failed": failures}
		if len(items) == 1 {
			response["id"] = items[0].ID
			response["slug"] = items[0].Slug
			response["status_url"] = items[0].StatusURL
		}
		adminJSONResponse(w, status, response)
		return
	}
	first := items[0]
	for _, item := range items {
		if item.ID != "" {
			first = item
			break
		}
	}
	target := "/admin/flipbooks/" + first.ID
	values := url.Values{}
	values.Set("notice", fmt.Sprintf("%d upload(s) created as drafts.", created))
	if failures > 0 {
		values.Set("error", fmt.Sprintf("%d upload(s) failed; review the upload queue and item errors.", failures))
	}
	target += "?" + values.Encode()
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func (h *AdminHandler) saveNewFlipbook(r *http.Request, actor adminActor, title, filename string, reader io.Reader) (adminUploadItem, error) {
	filename, err := safeAdminFilename(filename)
	if err != nil {
		return adminUploadItem{Filename: filename, Title: title}, err
	}
	item := adminUploadItem{Filename: filename}
	if err := converter.ValidateSourceFormat(filename); err != nil {
		item.Title = strings.TrimSuffix(filename, path.Ext(filename))
		return item, adminInputf("%s is not a supported document. Supported formats: %s.", filename, strings.Join(converter.SupportedExtensions(), ", "))
	}
	title = strings.TrimSpace(title)
	if title == "" {
		title = strings.TrimSuffix(filename, path.Ext(filename))
	}
	if len(title) > 200 {
		return item, adminInputf("Titles must be at most 200 characters.")
	}
	item.Title = title
	workspace, err := h.db.GetWorkspace(r.Context(), actor.WorkspaceID)
	if err != nil {
		return item, err
	}
	id := uuid.NewString()
	upload, err := h.storage.SaveUploadStream(r.Context(), id, filename, reader, h.maxBytes)
	if err != nil {
		return item, err
	}
	slug, err := h.db.GenerateUniqueSlug(r.Context(), actor.WorkspaceID, slugify(title))
	if err != nil {
		_ = h.storage.DeleteFlipbookContext(r.Context(), id)
		return item, err
	}
	flipbook := &models.Flipbook{
		ID: id, WorkspaceID: actor.WorkspaceID, OwnerID: actor.UserID, Title: title, Slug: slug,
		Filename: filename, FileSize: upload.Size, Status: models.StatusPending,
		PublicationState: models.PublicationDraft, Visibility: workspace.Settings.DefaultVisibility,
		ReviewState: models.ReviewNotRequired, Download: workspace.Settings.DefaultDownload,
		Embed: workspace.Settings.DefaultEmbed, Share: workspace.Settings.DefaultShare,
		SourceChecksum: upload.SHA256, BackupStatus: "pending",
	}
	if err := h.db.CreateFlipbookContext(r.Context(), flipbook); err != nil {
		_ = h.storage.DeleteFlipbookContext(r.Context(), id)
		return item, err
	}
	item.ID, item.Slug, item.Status = id, slug, models.StatusPending
	item.StatusURL = "/admin/flipbooks/" + id + "/status"
	item.ManageURL = "/admin/flipbooks/" + id
	_, queueErr := h.worker.EnqueueContext(r.Context(), worker.Job{FlipbookID: id, WorkspaceID: actor.WorkspaceID, SourcePath: upload.Path})
	if queueErr != nil {
		message := "The document was saved, but conversion could not be queued. Use Retry on the publication."
		if err := h.db.UpdateStatus(id, models.StatusError, message); err != nil {
			message += " Updating its status also failed."
		}
		item.Status = models.StatusError
		h.audit(r, actor, "publication.upload", "flipbook", id, "failure", map[string]string{"stage": "queue"})
		return item, fmt.Errorf("%s: %w", message, queueErr)
	}
	h.audit(r, actor, "publication.upload", "flipbook", id, "success", map[string]string{"filename": filename})
	return item, nil
}

func safeAdminFilename(filename string) (string, error) {
	filename = strings.ReplaceAll(strings.TrimSpace(filename), "\\", "/")
	filename = path.Base(filename)
	if filename == "" || filename == "." || len(filename) > 255 || strings.ContainsAny(filename, "\x00\r\n") {
		return "", adminInputf("The uploaded filename is invalid.")
	}
	return filename, nil
}

func (h *AdminHandler) ImportURL(w http.ResponseWriter, r *http.Request) {
	actor, err := adminActorFromRequest(r, models.RoleEditor)
	if err != nil {
		h.renderError(w, r, err)
		return
	}
	if err := r.ParseForm(); err != nil {
		h.uploadError(w, r, actor, adminInputf("The import request is invalid."))
		return
	}
	rawURL := strings.TrimSpace(r.FormValue("url"))
	matches := googleSlidesRe.FindStringSubmatch(rawURL)
	if len(matches) < 2 {
		h.uploadError(w, r, actor, adminInputf("Enter a valid Google Slides presentation URL."))
		return
	}
	exportURL := fmt.Sprintf("https://docs.google.com/presentation/d/%s/export/pdf", matches[1])
	request, err := http.NewRequestWithContext(r.Context(), http.MethodGet, exportURL, nil)
	if err != nil {
		h.uploadError(w, r, actor, err)
		return
	}
	response, err := h.httpClient.Do(request)
	if err != nil {
		h.uploadError(w, r, actor, adminInputf("Google Slides could not be downloaded. Check the sharing settings and try again."))
		return
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		h.uploadError(w, r, actor, adminInputf("Google Slides returned %s. Make the presentation available to anyone with the link.", response.Status))
		return
	}
	item, saveErr := h.saveNewFlipbook(r, actor, r.FormValue("title"), "import.pdf", response.Body)
	if saveErr != nil {
		item.Error = saveErr.Error()
	}
	created := 0
	if item.ID != "" {
		created = 1
	}
	h.respondUploadResults(w, r, actor, []adminUploadItem{item}, created)
}

var googleSlidesRe = regexp.MustCompile(`/presentation/d/([a-zA-Z0-9_-]+)`)
