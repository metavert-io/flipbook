package handlers

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/metavert-io/flipbook/internal/converter"
	"github.com/metavert-io/flipbook/internal/database"
	"github.com/metavert-io/flipbook/internal/models"
	"github.com/metavert-io/flipbook/internal/multipartform"
	"github.com/metavert-io/flipbook/internal/storage"
	"github.com/metavert-io/flipbook/internal/worker"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

func (h *APIHandler) ReplaceFlipbook(w http.ResponseWriter, r *http.Request) {
	identity, err := h.identity(r, models.RoleEditor)
	if err != nil {
		apiError(w, err)
		return
	}
	fb, err := h.db.GetFlipbookInWorkspace(r.Context(), identity.WorkspaceID, chi.URLParam(r, "id"))
	if err != nil {
		apiError(w, err)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, h.maxUploadBytes+(1<<20))
	defer multipartform.Release(r)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		invalidAPIRequest(w, "Invalid replacement upload")
		return
	}
	idempotencyKey := strings.TrimSpace(r.FormValue("idempotency_key"))
	if idempotencyKey == "" {
		idempotencyKey = strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	}
	jobKey := ""
	if idempotencyKey != "" {
		jobKey = "replace:" + fb.ID + ":" + idempotencyKey
		if existing, getErr := h.db.GetJobByIdempotencyKey(r.Context(), identity.WorkspaceID, jobKey); getErr == nil {
			if existing.FlipbookID != fb.ID {
				apiError(w, database.ErrConflict)
				return
			}
			w.Header().Set("Idempotent-Replay", "true")
			jsonResponse(w, map[string]any{"job": existing, "revision_id": existing.RevisionID})
			return
		} else if !errors.Is(getErr, mongo.ErrNoDocuments) {
			apiError(w, getErr)
			return
		}
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		invalidAPIRequest(w, "No replacement file provided")
		return
	}
	defer file.Close()
	filename := header.Filename
	if err := converter.ValidateSourceFormat(filename); err != nil {
		invalidAPIRequest(w, "Unsupported replacement format")
		return
	}
	revision := &models.Revision{
		WorkspaceID: identity.WorkspaceID, FlipbookID: fb.ID,
		SourceFilename: filename, ExternalSource: fb.ExternalSource, ExternalID: fb.ExternalID,
		CreatedBy: identity.UserID,
	}
	if err := h.db.CreateRevision(r.Context(), revision); err != nil {
		apiError(w, err)
		return
	}
	upload, err := h.storage.SaveUploadStream(r.Context(), fb.ID, filename, file, h.maxUploadBytes)
	if err != nil {
		_ = h.db.MarkRevisionFailed(r.Context(), revision.ID, err.Error())
		apiError(w, err)
		return
	}
	job, err := h.worker.EnqueueContext(r.Context(), worker.Job{
		FlipbookID: fb.ID, WorkspaceID: identity.WorkspaceID, RevisionID: revision.ID,
		SourcePath: upload.Path, IdempotencyKey: jobKey,
	})
	if err != nil {
		apiError(w, err)
		return
	}
	h.audit(r, identity, "flipbook.replace", "flipbook", fb.ID, map[string]string{"revision_id": revision.ID})
	h.emitWebhook(r, identity.WorkspaceID, "conversion.queued", fb.ID, "conversion.queued:"+revision.ID, map[string]any{
		"flipbook_id": fb.ID, "revision_id": revision.ID,
	})
	jsonStatus(w, http.StatusAccepted, map[string]any{"job": job, "revision": revision})
}

func (h *APIHandler) ListRevisions(w http.ResponseWriter, r *http.Request) {
	identity, err := h.identity(r, models.RoleReader)
	if err != nil {
		apiError(w, err)
		return
	}
	id := chi.URLParam(r, "id")
	if _, err := h.db.GetFlipbookInWorkspace(r.Context(), identity.WorkspaceID, id); err != nil {
		apiError(w, err)
		return
	}
	revisions, err := h.db.ListRevisions(r.Context(), identity.WorkspaceID, id)
	if err != nil {
		apiError(w, err)
		return
	}
	jsonResponse(w, map[string]any{"items": revisions})
}

func (h *APIHandler) GetRevision(w http.ResponseWriter, r *http.Request) {
	identity, err := h.identity(r, models.RoleReader)
	if err != nil {
		apiError(w, err)
		return
	}
	revision, err := h.db.GetRevision(r.Context(), identity.WorkspaceID, chi.URLParam(r, "revisionID"))
	if err != nil {
		apiError(w, err)
		return
	}
	if revision.FlipbookID != chi.URLParam(r, "id") {
		apiError(w, mongo.ErrNoDocuments)
		return
	}
	jsonResponse(w, revision)
}

func (h *APIHandler) DeleteRevision(w http.ResponseWriter, r *http.Request) {
	identity, err := h.identity(r, models.RoleEditor)
	if err != nil {
		apiError(w, err)
		return
	}
	flipbookID := chi.URLParam(r, "id")
	revision, err := h.db.GetRevision(r.Context(), identity.WorkspaceID, chi.URLParam(r, "revisionID"))
	if err != nil {
		apiError(w, err)
		return
	}
	if revision.FlipbookID != flipbookID {
		apiError(w, mongo.ErrNoDocuments)
		return
	}
	if err := h.storage.DeleteRevisionContext(r.Context(), flipbookID, revision.Number); err != nil && !errors.Is(err, storage.ErrRevisionMissing) {
		apiError(w, err)
		return
	}
	if err := h.db.DeleteRevision(r.Context(), identity.WorkspaceID, flipbookID, revision.ID); err != nil {
		apiError(w, err)
		return
	}
	h.audit(r, identity, "flipbook.revision.delete", "revision", revision.ID, nil)
	w.WriteHeader(http.StatusNoContent)
}

type rollbackRequest struct {
	RevisionID string `json:"revision_id"`
}

func (h *APIHandler) ActivateRevision(w http.ResponseWriter, r *http.Request) {
	h.activateRevision(w, r, false)
}

func (h *APIHandler) RollbackRevision(w http.ResponseWriter, r *http.Request) {
	h.activateRevision(w, r, true)
}

func (h *APIHandler) activateRevision(w http.ResponseWriter, r *http.Request, rollback bool) {
	identity, err := h.identity(r, models.RoleEditor)
	if err != nil {
		apiError(w, err)
		return
	}
	revisionID := chi.URLParam(r, "revisionID")
	if revisionID == "" {
		var request rollbackRequest
		if err := decodeAPIJSON(r, &request); err != nil {
			invalidAPIRequest(w, "revision_id is required")
			return
		}
		revisionID = request.RevisionID
	}
	result, err := h.activateFlipbookRevision(r, identity, chi.URLParam(r, "id"), revisionID, rollback)
	if err != nil {
		apiError(w, err)
		return
	}
	h.audit(r, identity, "flipbook.revision.activate", "revision", revisionID, map[string]string{"rollback": fmt.Sprint(rollback)})
	h.emitWebhook(r, identity.WorkspaceID, "publication.revision_activated", chi.URLParam(r, "id"), "", result)
	jsonResponse(w, result)
}

func (h *APIHandler) activateFlipbookRevision(r *http.Request, identity *apiIdentity, flipbookID, revisionID string, rollback bool) (map[string]any, error) {
	fb, err := h.db.GetFlipbookInWorkspace(r.Context(), identity.WorkspaceID, flipbookID)
	if err != nil {
		return nil, err
	}
	current, err := h.db.GetCurrentRevision(r.Context(), identity.WorkspaceID, flipbookID)
	if err != nil {
		return nil, err
	}
	target, err := h.db.GetRevision(r.Context(), identity.WorkspaceID, revisionID)
	if err != nil {
		return nil, err
	}
	if target.FlipbookID != flipbookID || target.Number < 1 || target.AssetVersion < 1 || target.PageCount < 1 || target.ValidatedAt == nil {
		return nil, fmt.Errorf("%w: revision is not a validated artifact", database.ErrConflict)
	}
	switch target.Status {
	case models.RevisionReady, models.RevisionActive, models.RevisionSuperseded:
	default:
		return nil, fmt.Errorf("%w: revision is not ready", database.ErrConflict)
	}
	if rollback && target.Number >= current.Number {
		return nil, fmt.Errorf("%w: rollback target must be older than the active revision", database.ErrConflict)
	}
	if target.ID == current.ID {
		return map[string]any{"flipbook": h.presentFlipbook(fb, false), "revision": target, "rollback": rollback}, nil
	}
	if err := h.storage.ActivateRevision(flipbookID, target.Number); err != nil {
		return nil, err
	}
	if err := h.db.ActivateRevision(r.Context(), identity.WorkspaceID, flipbookID, revisionID); err != nil {
		if storageErr := h.storage.ActivateRevision(flipbookID, current.Number); storageErr != nil {
			return nil, errors.Join(err, storageErr)
		}
		return nil, err
	}
	updated, err := h.db.GetFlipbookInWorkspace(r.Context(), identity.WorkspaceID, flipbookID)
	if err != nil {
		return nil, err
	}
	target.Status = models.RevisionActive
	return map[string]any{"flipbook": h.presentFlipbook(updated, false), "revision": target, "rollback": rollback}, nil
}

type retryRequest struct {
	RevisionID     string `json:"revision_id"`
	IdempotencyKey string `json:"idempotency_key"`
}

func (h *APIHandler) RetryFlipbook(w http.ResponseWriter, r *http.Request) {
	identity, err := h.identity(r, models.RoleEditor)
	if err != nil {
		apiError(w, err)
		return
	}
	var request retryRequest
	if r.Body != nil && r.ContentLength != 0 {
		if err := decodeAPIJSON(r, &request); err != nil {
			invalidAPIRequest(w, "Invalid retry request: "+err.Error())
			return
		}
	}
	id := chi.URLParam(r, "id")
	if request.RevisionID != "" {
		revision, getErr := h.db.GetRevision(r.Context(), identity.WorkspaceID, request.RevisionID)
		if getErr != nil {
			apiError(w, getErr)
			return
		}
		if revision.FlipbookID != id {
			apiError(w, mongo.ErrNoDocuments)
			return
		}
	}
	job, err := h.worker.EnqueueContext(r.Context(), worker.Job{
		FlipbookID: id, WorkspaceID: identity.WorkspaceID, RevisionID: request.RevisionID,
		IdempotencyKey: request.IdempotencyKey,
	})
	if err != nil {
		apiError(w, err)
		return
	}
	h.audit(r, identity, "flipbook.retry", "flipbook", id, map[string]string{"job_id": job.ID})
	jsonStatus(w, http.StatusAccepted, job)
}

func (h *APIHandler) CancelFlipbook(w http.ResponseWriter, r *http.Request) {
	identity, err := h.identity(r, models.RoleEditor)
	if err != nil {
		apiError(w, err)
		return
	}
	id := chi.URLParam(r, "id")
	if _, err := h.db.GetFlipbookInWorkspace(r.Context(), identity.WorkspaceID, id); err != nil {
		apiError(w, err)
		return
	}
	cancelled := make([]string, 0)
	for _, status := range []models.JobStatus{models.JobQueued, models.JobRetry, models.JobRunning} {
		jobs, err := h.db.ListJobs(r.Context(), database.JobFilter{WorkspaceID: identity.WorkspaceID, FlipbookID: id, Status: status, Limit: 1000})
		if err != nil {
			apiError(w, err)
			return
		}
		for _, job := range jobs {
			if job.Type != models.JobConvert && job.Type != models.JobBackup {
				continue
			}
			if err := h.worker.Cancel(r.Context(), job.ID); err != nil {
				apiError(w, err)
				return
			}
			cancelled = append(cancelled, job.ID)
		}
	}
	h.audit(r, identity, "flipbook.cancel", "flipbook", id, nil)
	jsonResponse(w, map[string]any{"cancelled_job_ids": cancelled, "count": len(cancelled)})
}
