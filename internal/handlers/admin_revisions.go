package handlers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/metavert-io/flipbook/internal/converter"
	"github.com/metavert-io/flipbook/internal/database"
	"github.com/metavert-io/flipbook/internal/models"
	"github.com/metavert-io/flipbook/internal/publishing"
	"github.com/metavert-io/flipbook/internal/storage"
	"github.com/metavert-io/flipbook/internal/worker"
)

func (h *AdminHandler) Replace(w http.ResponseWriter, r *http.Request) {
	actor, err := adminActorFromRequest(r, models.RoleEditor)
	if err != nil {
		h.renderError(w, r, err)
		return
	}
	id := chi.URLParam(r, "id")
	r.Body = http.MaxBytesReader(w, r.Body, h.maxBytes+1<<20)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		h.lifecycleError(w, r, actor, id, adminInputf("The replacement upload is invalid or too large."))
		return
	}
	var file io.ReadCloser
	var sourceName string
	if uploadID := strings.TrimSpace(r.FormValue("upload_id")); uploadID != "" {
		session, sessionFile, err := h.completedUploadSession(actor, uploadID)
		if err != nil {
			if errors.Is(err, storage.ErrUploadSessionNotFound) || errors.Is(err, storage.ErrUploadIncomplete) {
				err = adminInputf("The replacement upload is incomplete or has expired. Upload the document again.")
			}
			h.lifecycleError(w, r, actor, id, err)
			return
		}
		defer func() {
			if err := h.storage.DeleteUploadSession(uploadID); err != nil {
				log.Printf("upload sessions: delete %s: %v", uploadID, err)
			}
		}()
		file, sourceName = sessionFile, session.Filename
	} else {
		formFile, header, err := r.FormFile("file")
		if err != nil {
			h.lifecycleError(w, r, actor, id, adminInputf("Choose a replacement document."))
			return
		}
		file, sourceName = formFile, header.Filename
	}
	defer file.Close()
	filename, err := safeAdminFilename(sourceName)
	if err != nil {
		h.lifecycleError(w, r, actor, id, err)
		return
	}
	if err := converter.ValidateSourceFormat(filename); err != nil {
		h.lifecycleError(w, r, actor, id, adminInputf("The replacement format is unsupported. Supported formats: %s.", strings.Join(converter.SupportedExtensions(), ", ")))
		return
	}
	flipbook, err := h.db.GetFlipbookInWorkspace(r.Context(), actor.WorkspaceID, id)
	if err != nil {
		h.lifecycleError(w, r, actor, id, err)
		return
	}
	stagingDir, err := h.storage.CreateRevisionStaging(id)
	if err != nil {
		h.lifecycleError(w, r, actor, id, err)
		return
	}
	keepStaging := false
	defer func() {
		if !keepStaging {
			_ = os.RemoveAll(stagingDir)
		}
	}()
	upload, err := h.storage.SaveStagedUpload(r.Context(), id, stagingDir, filename, file, h.maxBytes)
	if err != nil {
		h.lifecycleError(w, r, actor, id, err)
		return
	}
	revision := &models.Revision{
		WorkspaceID: actor.WorkspaceID, FlipbookID: id, SourceFilename: filename,
		SourceKey: upload.Path, StagingKey: stagingDir, SourceChecksum: upload.SHA256, CreatedBy: actor.UserID,
	}
	if err := h.db.CreateRevision(r.Context(), revision); err != nil {
		h.lifecycleError(w, r, actor, id, err)
		return
	}
	keepStaging = true
	job, err := h.worker.EnqueueContext(r.Context(), worker.Job{
		FlipbookID: id, WorkspaceID: actor.WorkspaceID, RevisionID: revision.ID, SourcePath: upload.Path,
	})
	if err != nil {
		_ = h.db.MarkRevisionFailed(context.WithoutCancel(r.Context()), revision.ID, "Conversion could not be queued. Retry this revision.")
		h.lifecycleError(w, r, actor, id, fmt.Errorf("replacement was saved as revision %d but could not be queued: %w", revision.Number, err))
		return
	}
	h.audit(r, actor, "publication.revision.replace", "flipbook", id, "success", map[string]string{"revision_id": revision.ID, "job_id": job.ID})
	if adminWantsJSON(r) {
		adminJSONResponse(w, http.StatusAccepted, map[string]any{"id": id, "revision_id": revision.ID, "job_id": job.ID, "status_url": "/admin/flipbooks/" + id + "/status"})
		return
	}
	message := fmt.Sprintf("Replacement uploaded as revision %d. The current version remains live until conversion succeeds.", revision.Number)
	http.Redirect(w, r, "/admin/flipbooks/"+id+"?notice="+url.QueryEscape(message), http.StatusSeeOther)
	_ = flipbook
}

func (h *AdminHandler) Reconvert(w http.ResponseWriter, r *http.Request) {
	actor, err := adminActorFromRequest(r, models.RoleEditor)
	if err != nil {
		h.renderError(w, r, err)
		return
	}
	id := chi.URLParam(r, "id")
	if err := r.ParseForm(); err != nil {
		h.lifecycleError(w, r, actor, id, adminInputf("The reconversion form is invalid."))
		return
	}
	flipbook, err := h.db.GetFlipbookInWorkspace(r.Context(), actor.WorkspaceID, id)
	if err != nil {
		h.lifecycleError(w, r, actor, id, err)
		return
	}
	var revision *models.Revision
	revisionID := strings.TrimSpace(r.FormValue("revision_id"))
	if revisionID != "" {
		revision, err = h.db.GetRevision(r.Context(), actor.WorkspaceID, revisionID)
		if err == nil && revision.FlipbookID != id {
			err = database.ErrConflict
		}
	} else {
		revision, err = h.db.GetCurrentRevision(r.Context(), actor.WorkspaceID, id)
	}
	if err != nil {
		h.lifecycleError(w, r, actor, id, err)
		return
	}
	if revisionID == "" && flipbook.Status == models.StatusReady {
		revision = &models.Revision{
			WorkspaceID: actor.WorkspaceID, FlipbookID: id, SourceFilename: revision.SourceFilename,
			SourceKey: revision.SourceKey, OriginalGridFSFileID: revision.OriginalGridFSFileID,
			SourceChecksum: revision.SourceChecksum, ExternalSource: revision.ExternalSource,
			ExternalID: revision.ExternalID, CreatedBy: actor.UserID,
		}
		if err := h.db.CreateRevision(r.Context(), revision); err != nil {
			h.lifecycleError(w, r, actor, id, err)
			return
		}
	}
	job, err := h.worker.EnqueueContext(r.Context(), worker.Job{
		FlipbookID: id, WorkspaceID: actor.WorkspaceID, RevisionID: revision.ID,
		SourcePath: h.sourceForRevision(id, revision),
	})
	if err != nil {
		h.lifecycleError(w, r, actor, id, err)
		return
	}
	h.audit(r, actor, "publication.revision.reconvert", "flipbook", id, "success", map[string]string{"revision_id": revision.ID, "job_id": job.ID})
	if adminWantsJSON(r) {
		adminJSONResponse(w, http.StatusAccepted, map[string]any{"id": id, "revision_id": revision.ID, "job_id": job.ID})
		return
	}
	h.mutationSuccess(w, r, id, fmt.Sprintf("Conversion queued for revision %d.", revision.Number))
}

func (h *AdminHandler) sourceForRevision(id string, revision *models.Revision) string {
	extension := path.Ext(strings.ReplaceAll(revision.SourceFilename, "\\", "/"))
	if extension == "" {
		extension = ".pdf"
	}
	if revision.Number > 0 {
		installed := path.Join(h.storage.RevisionDir(id, revision.Number), "original"+extension)
		if info, err := os.Stat(installed); err == nil && info.Mode().IsRegular() {
			return installed
		}
	}
	if revision.SourceKey != "" {
		return revision.SourceKey
	}
	return h.storage.OriginalPath(id, extension)
}

func (h *AdminHandler) Retry(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err == nil && strings.TrimSpace(r.FormValue("job_id")) != "" {
		h.RetryJob(w, r)
		return
	}
	h.Reconvert(w, r)
}

func (h *AdminHandler) Cancel(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err == nil && strings.TrimSpace(r.FormValue("job_id")) != "" {
		h.CancelJob(w, r)
		return
	}
	actor, err := adminActorFromRequest(r, models.RoleEditor)
	if err != nil {
		h.renderError(w, r, err)
		return
	}
	id := chi.URLParam(r, "id")
	jobs, err := h.db.ListJobs(r.Context(), database.JobFilter{WorkspaceID: actor.WorkspaceID, FlipbookID: id, Type: models.JobConvert, Limit: 100})
	if err != nil {
		h.lifecycleError(w, r, actor, id, err)
		return
	}
	for _, job := range jobs {
		if job.Status == models.JobQueued || job.Status == models.JobRunning || job.Status == models.JobRetry {
			r.Form.Set("job_id", job.ID)
			h.CancelJob(w, r)
			return
		}
	}
	h.lifecycleError(w, r, actor, id, adminInputf("No active conversion job can be cancelled."))
}

func (h *AdminHandler) CancelJob(w http.ResponseWriter, r *http.Request) {
	h.mutateJob(w, r, true)
}

func (h *AdminHandler) RetryJob(w http.ResponseWriter, r *http.Request) {
	h.mutateJob(w, r, false)
}

func (h *AdminHandler) mutateJob(w http.ResponseWriter, r *http.Request, cancel bool) {
	actor, err := adminActorFromRequest(r, models.RoleEditor)
	if err != nil {
		h.renderError(w, r, err)
		return
	}
	id := chi.URLParam(r, "id")
	if err := r.ParseForm(); err != nil {
		h.lifecycleError(w, r, actor, id, adminInputf("The job form is invalid."))
		return
	}
	jobID := strings.TrimSpace(r.FormValue("job_id"))
	if jobID == "" {
		jobID = chi.URLParam(r, "jobID")
	}
	job, err := h.db.GetJob(r.Context(), jobID)
	if err != nil || job.WorkspaceID != actor.WorkspaceID || job.FlipbookID != id {
		h.lifecycleError(w, r, actor, id, database.ErrNotFound)
		return
	}
	if cancel {
		err = h.worker.Cancel(r.Context(), jobID)
	} else {
		err = h.worker.Retry(r.Context(), jobID)
	}
	if err != nil {
		h.lifecycleError(w, r, actor, id, err)
		return
	}
	action := "retry"
	message := "Job retry queued."
	if cancel {
		action = "cancel"
		message = "Cancellation requested. The worker will stop at its next safe checkpoint."
	}
	h.audit(r, actor, "publication.job."+action, "flipbook", id, "success", map[string]string{"job_id": jobID})
	h.mutationSuccess(w, r, id, message)
}

func (h *AdminHandler) Rollback(w http.ResponseWriter, r *http.Request) {
	h.activateRevision(w, r, true)
}

func (h *AdminHandler) ActivateRevision(w http.ResponseWriter, r *http.Request) {
	h.activateRevision(w, r, false)
}

func (h *AdminHandler) activateRevision(w http.ResponseWriter, r *http.Request, rollback bool) {
	actor, err := adminActorFromRequest(r, models.RoleEditor)
	if err != nil {
		h.renderError(w, r, err)
		return
	}
	id := chi.URLParam(r, "id")
	if h.publisher == nil {
		h.lifecycleError(w, r, actor, id, errAdminUnavailable)
		return
	}
	revisionID := chi.URLParam(r, "revisionID")
	var result *publishing.ActivationResult
	if rollback {
		result, err = h.publisher.RollbackRevision(r.Context(), publishing.Actor{UserID: actor.UserID}, actor.WorkspaceID, id, revisionID)
	} else {
		result, err = h.publisher.ActivateRevision(r.Context(), publishing.Actor{UserID: actor.UserID}, actor.WorkspaceID, id, revisionID)
	}
	if err != nil {
		h.lifecycleError(w, r, actor, id, err)
		return
	}
	action := "activate"
	message := "Revision activated."
	if rollback {
		action = "rollback"
		message = "Publication rolled back to the selected revision."
	}
	h.audit(r, actor, "publication.revision."+action, "flipbook", id, "success", map[string]string{"revision_id": result.Revision.ID})
	h.mutationSuccess(w, r, id, message)
}

func (h *AdminHandler) ConversionStatus(w http.ResponseWriter, r *http.Request) {
	actor, err := adminActorFromRequest(r, models.RoleReader)
	if err != nil {
		h.renderError(w, r, err)
		return
	}
	id := chi.URLParam(r, "id")
	flipbook, err := h.db.GetFlipbookInWorkspace(r.Context(), actor.WorkspaceID, id)
	if err != nil {
		h.renderError(w, r, err)
		return
	}
	revisions, err := h.db.ListRevisions(r.Context(), actor.WorkspaceID, id)
	if err != nil {
		h.renderError(w, r, err)
		return
	}
	jobs, err := h.db.ListJobs(r.Context(), database.JobFilter{WorkspaceID: actor.WorkspaceID, FlipbookID: id, Limit: 10})
	if err != nil {
		h.renderError(w, r, err)
		return
	}
	revisionViews := make([]map[string]any, 0, len(revisions))
	for _, revision := range revisions {
		revisionViews = append(revisionViews, map[string]any{
			"id": revision.ID, "number": revision.Number, "status": revision.Status,
			"progress": revision.Progress, "page_count": revision.PageCount,
			"error": revision.ErrorMessage, "warnings": revision.Warnings, "updated_at": revision.UpdatedAt,
		})
	}
	jobViews := make([]adminJobView, 0, len(jobs))
	for _, job := range jobs {
		jobViews = append(jobViews, adminJobView{ID: job.ID, Type: job.Type, Status: job.Status, Progress: job.Progress, ProgressMessage: job.ProgressMessage, Attempts: job.Attempts, MaxAttempts: job.MaxAttempts, CancelRequested: job.CancelRequested, LastError: job.LastError, CreatedAt: job.CreatedAt, UpdatedAt: job.UpdatedAt})
	}
	adminJSONResponse(w, http.StatusOK, map[string]any{
		"id": flipbook.ID, "title": flipbook.Title, "slug": flipbook.Slug, "status": flipbook.Status,
		"page_count": flipbook.PageCount, "error": flipbook.ErrorMessage,
		"publication_state": flipbook.PublicationState, "current_revision_id": flipbook.CurrentRevisionID,
		"revisions": revisionViews, "jobs": jobViews,
	})
}

func (h *AdminHandler) Duplicate(w http.ResponseWriter, r *http.Request) {
	actor, err := adminActorFromRequest(r, models.RoleEditor)
	if err != nil {
		h.renderError(w, r, err)
		return
	}
	id := chi.URLParam(r, "id")
	source, err := h.db.GetFlipbookInWorkspace(r.Context(), actor.WorkspaceID, id)
	if err != nil {
		h.lifecycleError(w, r, actor, id, err)
		return
	}
	revision, err := h.db.GetCurrentRevision(r.Context(), actor.WorkspaceID, id)
	if err != nil {
		h.lifecycleError(w, r, actor, id, err)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	reader, err := h.openDuplicateSource(ctx, id, source, revision)
	if err != nil {
		h.lifecycleError(w, r, actor, id, err)
		return
	}
	defer reader.Close()
	newID := uuid.NewString()
	upload, err := h.storage.SaveUploadStream(ctx, newID, source.Filename, reader, h.maxBytes)
	if err != nil {
		h.lifecycleError(w, r, actor, id, err)
		return
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = h.storage.DeleteFlipbookContext(context.WithoutCancel(ctx), newID)
		}
	}()
	title := strings.TrimSpace("Copy of " + source.Title)
	if len(title) > 200 {
		title = title[:200]
	}
	slug, err := h.db.GenerateUniqueSlug(ctx, actor.WorkspaceID, slugify(title))
	if err != nil {
		h.lifecycleError(w, r, actor, id, err)
		return
	}
	duplicate := *source
	duplicate.ID = newID
	duplicate.OwnerID = actor.UserID
	duplicate.Title = title
	duplicate.Slug = slug
	duplicate.FileSize = upload.Size
	duplicate.PageCount = 0
	duplicate.Status = models.StatusPending
	duplicate.ErrorMessage = ""
	duplicate.PageWidth, duplicate.PageHeight = 0, 0
	duplicate.IsPublic = false
	duplicate.PublicationState = models.PublicationDraft
	duplicate.ReviewState = models.ReviewNotRequired
	duplicate.ReviewNote, duplicate.ReviewedBy = "", ""
	duplicate.ReviewedAt, duplicate.PublishAt, duplicate.ExpiresAt = nil, nil, nil
	duplicate.PublishedAt, duplicate.UnpublishedAt = nil, nil
	duplicate.CurrentRevisionID = ""
	duplicate.AssetVersion = 1
	duplicate.Tags = cloneAdminStrings(source.Tags)
	duplicate.CollectionIDs = nil
	duplicate.ExternalSource, duplicate.ExternalID = "", ""
	duplicate.AccessTokens = nil
	duplicate.GridFSFileID = ""
	duplicate.SourceChecksum = upload.SHA256
	duplicate.BackupStatus, duplicate.BackupError = "pending", ""
	duplicate.BackedUpAt = nil
	duplicate.ArchivedAt, duplicate.DeletedAt, duplicate.PurgeAfter = nil, nil, nil
	duplicate.ArchiveReason = ""
	duplicate.CreatedAt, duplicate.UpdatedAt = time.Time{}, time.Time{}
	duplicate.ConvertedAt = nil
	duplicate.SEO.CanonicalURL = ""
	if err := h.db.CreateFlipbookContext(ctx, &duplicate); err != nil {
		h.lifecycleError(w, r, actor, id, err)
		return
	}
	cleanup = false
	job, err := h.worker.EnqueueContext(ctx, worker.Job{FlipbookID: newID, WorkspaceID: actor.WorkspaceID, SourcePath: upload.Path})
	if err != nil {
		_ = h.db.UpdateStatus(newID, models.StatusError, "The duplicate was saved but conversion could not be queued. Use Retry.")
		h.lifecycleError(w, r, actor, id, fmt.Errorf("duplicate %s was created but conversion could not be queued: %w", newID, err))
		return
	}
	h.audit(r, actor, "publication.duplicate", "flipbook", id, "success", map[string]string{"duplicate_id": newID})
	if adminWantsJSON(r) {
		adminJSONResponse(w, http.StatusCreated, map[string]any{"id": newID, "slug": duplicate.Slug, "source_id": id, "job_id": job.ID, "manage_url": "/admin/flipbooks/" + newID})
		return
	}
	http.Redirect(w, r, "/admin/flipbooks/"+newID+"?notice="+url.QueryEscape("Draft duplicate created and queued for conversion."), http.StatusSeeOther)
}

func (h *AdminHandler) openDuplicateSource(ctx context.Context, id string, flipbook *models.Flipbook, revision *models.Revision) (io.ReadCloser, error) {
	sourcePath := h.sourceForRevision(id, revision)
	file, err := os.Open(sourcePath)
	if err == nil {
		return file, nil
	}
	backupID := revision.OriginalGridFSFileID
	if backupID == "" {
		backupID = flipbook.GridFSFileID
	}
	if backupID == "" {
		return nil, fmt.Errorf("open original source: %w", err)
	}
	reader, writer := io.Pipe()
	go func() {
		_, downloadErr := h.db.DownloadFromGridFS(ctx, backupID, writer)
		_ = writer.CloseWithError(downloadErr)
	}()
	return reader, nil
}
