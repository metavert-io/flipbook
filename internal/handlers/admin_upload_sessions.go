package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/metavert-io/flipbook/internal/converter"
	"github.com/metavert-io/flipbook/internal/models"
	"github.com/metavert-io/flipbook/internal/storage"
)

const (
	// uploadChunkSize is the first chunk the browser sends; it then sizes
	// chunks from measured throughput so each request stays well under the
	// 60-second request read timeout of the edge proxy.
	uploadChunkSize = 4 << 20
	// uploadChunkMaxBytes bounds a single chunk request.
	uploadChunkMaxBytes = 32 << 20
	uploadSessionTTL    = 24 * time.Hour
	// uploadSessionsPerUser caps abandoned sessions a single user can leave
	// on disk; the oldest are evicted first.
	uploadSessionsPerUser = 10
)

type uploadSessionResponse struct {
	ID           string `json:"id"`
	Filename     string `json:"filename"`
	Size         int64  `json:"size"`
	Received     int64  `json:"received"`
	ChunkSize    int64  `json:"chunk_size"`
	MaxChunkSize int64  `json:"max_chunk_size"`
	URL          string `json:"url"`
}

func newUploadSessionResponse(session storage.UploadSession) uploadSessionResponse {
	return uploadSessionResponse{
		ID: session.ID, Filename: session.Filename, Size: session.Size, Received: session.Received,
		ChunkSize: uploadChunkSize, MaxChunkSize: uploadChunkMaxBytes, URL: "/admin/uploads/" + session.ID,
	}
}

// RegisterUploadSessionRoutes registers the resumable upload API. Callers must
// wrap these routes with the session principal and CSRF middleware.
func (h *AdminHandler) RegisterUploadSessionRoutes(r chi.Router) {
	r.Post("/admin/uploads", h.CreateUploadSession)
	r.Get("/admin/uploads/{uploadID}", h.UploadSessionStatus)
	r.Put("/admin/uploads/{uploadID}", h.UploadSessionChunk)
	r.Delete("/admin/uploads/{uploadID}", h.AbortUploadSession)
}

func (h *AdminHandler) CreateUploadSession(w http.ResponseWriter, r *http.Request) {
	actor, err := adminActorFromRequest(r, models.RoleEditor)
	if err != nil {
		h.uploadSessionError(w, err)
		return
	}
	var input struct {
		Filename string `json:"filename"`
		Size     int64  `json:"size"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	if err := decoder.Decode(&input); err != nil {
		adminJSONError(w, "The upload request is invalid.", http.StatusBadRequest)
		return
	}
	filename, err := safeAdminFilename(input.Filename)
	if err != nil {
		h.uploadSessionError(w, err)
		return
	}
	if err := converter.ValidateSourceFormat(filename); err != nil {
		adminJSONError(w, fmt.Sprintf("%s is not a supported document. Supported formats: %s.", filename, strings.Join(converter.SupportedExtensions(), ", ")), http.StatusBadRequest)
		return
	}
	if input.Size <= 0 {
		adminJSONError(w, filename+" is empty.", http.StatusBadRequest)
		return
	}
	if input.Size > h.maxBytes {
		adminJSONError(w, fmt.Sprintf("%s is %s, which exceeds the %s upload limit.", filename, formatUploadBytes(input.Size), formatUploadBytes(h.maxBytes)), http.StatusRequestEntityTooLarge)
		return
	}
	h.pruneUploadSessions(actor.UserID)
	session, err := h.storage.CreateUploadSession(storage.UploadSession{
		ID: uuid.NewString(), WorkspaceID: actor.WorkspaceID, UserID: actor.UserID,
		Filename: filename, Size: input.Size, CreatedAt: h.now(),
	})
	if err != nil {
		h.uploadSessionError(w, err)
		return
	}
	adminJSONResponse(w, http.StatusCreated, newUploadSessionResponse(session))
}

func (h *AdminHandler) UploadSessionStatus(w http.ResponseWriter, r *http.Request) {
	actor, err := adminActorFromRequest(r, models.RoleEditor)
	if err != nil {
		h.uploadSessionError(w, err)
		return
	}
	session, err := h.ownedUploadSession(actor, chi.URLParam(r, "uploadID"))
	if err != nil {
		h.uploadSessionError(w, err)
		return
	}
	adminJSONResponse(w, http.StatusOK, newUploadSessionResponse(session))
}

func (h *AdminHandler) UploadSessionChunk(w http.ResponseWriter, r *http.Request) {
	actor, err := adminActorFromRequest(r, models.RoleEditor)
	if err != nil {
		h.uploadSessionError(w, err)
		return
	}
	id := chi.URLParam(r, "uploadID")
	if _, err := h.ownedUploadSession(actor, id); err != nil {
		h.uploadSessionError(w, err)
		return
	}
	offset, err := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	if err != nil || offset < 0 {
		adminJSONError(w, "The chunk offset is missing or invalid.", http.StatusBadRequest)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, uploadChunkMaxBytes+1)
	session, err := h.storage.WriteUploadChunk(r.Context(), id, offset, r.Body, uploadChunkMaxBytes)
	if errors.Is(err, storage.ErrUploadOffsetMismatch) {
		adminJSONResponse(w, http.StatusConflict, map[string]any{
			"error": "The upload is out of sync; resume from the received offset.", "received": session.Received, "size": session.Size,
		})
		return
	}
	if err != nil {
		h.uploadSessionError(w, err)
		return
	}
	adminJSONResponse(w, http.StatusOK, newUploadSessionResponse(session))
}

func (h *AdminHandler) AbortUploadSession(w http.ResponseWriter, r *http.Request) {
	actor, err := adminActorFromRequest(r, models.RoleEditor)
	if err != nil {
		h.uploadSessionError(w, err)
		return
	}
	id := chi.URLParam(r, "uploadID")
	if _, err := h.ownedUploadSession(actor, id); err != nil {
		if errors.Is(err, storage.ErrUploadSessionNotFound) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.uploadSessionError(w, err)
		return
	}
	if err := h.storage.DeleteUploadSession(id); err != nil {
		h.uploadSessionError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ownedUploadSession returns the session only when it belongs to the actor's
// user and workspace. Sessions of other users read as not found.
func (h *AdminHandler) ownedUploadSession(actor adminActor, id string) (storage.UploadSession, error) {
	session, err := h.storage.GetUploadSession(id)
	if err != nil {
		return storage.UploadSession{}, err
	}
	if session.UserID != actor.UserID || session.WorkspaceID != actor.WorkspaceID {
		return storage.UploadSession{}, storage.ErrUploadSessionNotFound
	}
	if !session.CreatedAt.IsZero() && h.now().Sub(session.CreatedAt) > uploadSessionTTL {
		_ = h.storage.DeleteUploadSession(id)
		return storage.UploadSession{}, storage.ErrUploadSessionNotFound
	}
	return session, nil
}

// completedUploadSession seals an owned session and opens the assembled
// document for the normal create or replace path.
func (h *AdminHandler) completedUploadSession(actor adminActor, id string) (storage.UploadSession, *os.File, error) {
	if _, err := h.ownedUploadSession(actor, id); err != nil {
		return storage.UploadSession{}, nil, err
	}
	return h.storage.CompleteUploadSession(id)
}

// pruneUploadSessions removes expired sessions and evicts the user's oldest
// sessions beyond the per-user cap. Failures are logged, not fatal: they only
// delay disk reclamation.
func (h *AdminHandler) pruneUploadSessions(userID string) {
	if _, err := h.storage.PurgeUploadSessions(h.now().Add(-uploadSessionTTL)); err != nil {
		log.Printf("upload sessions: purge expired: %v", err)
	}
	sessions, err := h.storage.ListUploadSessions()
	if err != nil {
		log.Printf("upload sessions: list: %v", err)
		return
	}
	owned := make([]storage.UploadSession, 0, len(sessions))
	for _, session := range sessions {
		if session.UserID == userID {
			owned = append(owned, session)
		}
	}
	for len(owned) >= uploadSessionsPerUser {
		if err := h.storage.DeleteUploadSession(owned[0].ID); err != nil {
			log.Printf("upload sessions: evict %s: %v", owned[0].ID, err)
		}
		owned = owned[1:]
	}
}

func (h *AdminHandler) uploadSessionError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, storage.ErrUploadSessionNotFound):
		adminJSONError(w, "This upload session has expired or does not exist. Start the upload again.", http.StatusNotFound)
	case errors.Is(err, storage.ErrUploadSessionComplete):
		adminJSONError(w, "This upload is already complete.", http.StatusConflict)
	case errors.Is(err, storage.ErrUploadIncomplete):
		adminJSONError(w, "The upload is incomplete. Resume it before finishing.", http.StatusConflict)
	case errors.Is(err, storage.ErrUploadTooLarge):
		adminJSONError(w, "The chunk is larger than the upload allows.", http.StatusRequestEntityTooLarge)
	default:
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			adminJSONError(w, "The chunk is larger than the upload allows.", http.StatusRequestEntityTooLarge)
			return
		}
		status, message := adminErrorMessage(err)
		if status >= http.StatusInternalServerError {
			log.Printf("upload sessions: %v", err)
		}
		adminJSONError(w, message, status)
	}
}

func formatUploadBytes(bytes int64) string {
	const mb = 1 << 20
	if bytes >= mb {
		return fmt.Sprintf("%.1f MB", float64(bytes)/mb)
	}
	return fmt.Sprintf("%d bytes", bytes)
}
