package handlers

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/metavert-io/flipbook/internal/auth"
	"github.com/metavert-io/flipbook/internal/database"
	"github.com/metavert-io/flipbook/internal/models"
	"github.com/metavert-io/flipbook/internal/storage"
)

// These tests send uploads that spool to disk through the middleware that
// copies the request in production, then require the temp directory to be
// empty once the handler has returned. Each upload is rejected only after the
// body has been parsed, so the handler's own cleanup is what is under test;
// the accepted path needs MongoDB and is covered by a manual end-to-end run.

const (
	uploadSessionToken = "multipart-cleanup-session"
	uploadUserID       = "uploader"
	adminSpoolSize     = 9 << 20  // above the admin handlers' 8 MB maxMemory
	apiSpoolSize       = 33 << 20 // above the API handlers' 32 MB maxMemory
)

// uploadTempDir points multipart spooling at an empty directory so a test can
// prove an upload left nothing behind.
func uploadTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	return dir
}

func assertNoUploadTempFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("upload left %d multipart temp file(s) behind: %v", len(entries), names)
	}
}

type filler byte

func (b filler) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(b)
	}
	return len(p), nil
}

type uploadPart struct {
	field, filename string
	size            int64
}

// streamedUpload builds a multipart body whose file parts are streamed, so a
// test can send tens of megabytes without holding them in memory.
func streamedUpload(t *testing.T, fields map[string]string, files ...uploadPart) (io.Reader, string) {
	t.Helper()
	var buffer bytes.Buffer
	writer := multipart.NewWriter(&buffer)
	for name, value := range fields {
		if err := writer.WriteField(name, value); err != nil {
			t.Fatal(err)
		}
	}
	var readers []io.Reader
	for _, file := range files {
		if _, err := writer.CreateFormFile(file.field, file.filename); err != nil {
			t.Fatal(err)
		}
		readers = append(readers, bytes.NewReader(bytes.Clone(buffer.Bytes())), io.LimitReader(filler('A'), file.size))
		buffer.Reset()
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	readers = append(readers, bytes.NewReader(bytes.Clone(buffer.Bytes())))
	return io.MultiReader(readers...), writer.FormDataContentType()
}

// uploadAuthStore resolves one session to an editor. RequireAuth needs only
// these lookups; any other Store call panics on the nil embedded interface.
type uploadAuthStore struct {
	auth.Store
}

func (uploadAuthStore) GetSessionByToken(_ context.Context, token string) (*models.Session, error) {
	if token != uploadSessionToken {
		return nil, database.ErrNotFound
	}
	now := time.Now().UTC()
	return &models.Session{UserID: uploadUserID, WorkspaceID: models.DefaultWorkspaceID, CreatedAt: now, ExpiresAt: now.Add(time.Hour), LastSeenAt: &now}, nil
}

func (uploadAuthStore) GetUser(_ context.Context, userID string) (*models.User, error) {
	if userID != uploadUserID {
		return nil, database.ErrNotFound
	}
	return &models.User{ID: uploadUserID, Email: "uploader@example.test"}, nil
}

func (uploadAuthStore) GetMembership(_ context.Context, workspaceID, userID string) (*models.Membership, error) {
	if workspaceID != models.DefaultWorkspaceID || userID != uploadUserID {
		return nil, database.ErrNotFound
	}
	return &models.Membership{WorkspaceID: workspaceID, UserID: userID, Role: models.RoleEditor}, nil
}

func (uploadAuthStore) RecordAuditEvent(context.Context, *models.AuditEvent) error { return nil }

// adminUploadRouter mounts the admin routes behind auth.RequireAuth exactly as
// main.go does, so handlers receive the request copy RequireAuth makes.
func adminUploadRouter(t *testing.T) http.Handler {
	t.Helper()
	a, err := auth.NewWithOptions(uploadAuthStore{}, nil, auth.Options{SessionSecret: strings.Repeat("multipart-", 4)})
	if err != nil {
		t.Fatal(err)
	}
	h := NewAdminHandlerWithOptions(nil, storage.New(t.TempDir()), nil, nil, "http://flipbook.test", AdminOptions{Auth: a, MaxBytes: 32 << 20})
	router := chi.NewRouter()
	router.Group(func(r chi.Router) {
		r.Use(a.RequireAuth)
		h.RegisterRoutes(r)
	})
	return router
}

func adminUpload(t *testing.T, router http.Handler, target string, body io.Reader, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, target, body)
	r.Header.Set("Content-Type", contentType)
	r.Header.Set("Accept", "application/json")
	r.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: uploadSessionToken})
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, r)
	return recorder
}

func TestAdminUploadsBehindRequireAuthLeaveNoTempFiles(t *testing.T) {
	for name, test := range map[string]struct {
		target  string
		fields  map[string]string
		files   []uploadPart
		status  int
		message string
	}{
		"upload: unsupported document": {
			target: "/admin/upload",
			files:  []uploadPart{{"file", "payload.exe", adminSpoolSize}},
			status: http.StatusBadRequest, message: "is not a supported document",
		},
		"upload: several documents": {
			target: "/admin/upload",
			files:  []uploadPart{{"files", "first.exe", adminSpoolSize}, {"files", "second.exe", adminSpoolSize}},
			status: http.StatusBadRequest, message: "is not a supported document",
		},
		"upload: parsed body, malformed query": {
			target: "/admin/upload?broken=%zz",
			files:  []uploadPart{{"file", "report.pdf", adminSpoolSize}},
			status: http.StatusBadRequest, message: "could not be parsed",
		},
		"upload: over the size limit": {
			target: "/admin/upload",
			files:  []uploadPart{{"file", "report.pdf", 40 << 20}},
			status: http.StatusBadRequest, message: "could not be parsed",
		},
		"replace: unsupported document": {
			target: "/admin/flipbooks/fb-1/replace",
			files:  []uploadPart{{"file", "payload.exe", adminSpoolSize}},
			status: http.StatusBadRequest, message: "format is unsupported",
		},
		"replace: parsed body, malformed query": {
			target: "/admin/flipbooks/fb-1/replace?broken=%zz",
			files:  []uploadPart{{"file", "report.pdf", adminSpoolSize}},
			status: http.StatusBadRequest, message: "invalid or too large",
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := uploadTempDir(t)
			router := adminUploadRouter(t)
			body, contentType := streamedUpload(t, test.fields, test.files...)
			recorder := adminUpload(t, router, test.target, body, contentType)
			if recorder.Code != test.status || !strings.Contains(recorder.Body.String(), test.message) {
				t.Fatalf("response %d %q, want %d containing %q", recorder.Code, recorder.Body.String(), test.status, test.message)
			}
			assertNoUploadTempFiles(t, dir)
		})
	}
}

// withAPIIdentity stands in for RequireScope, which hands the handler a copy
// of the request carrying the caller's identity.
func withAPIIdentity(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity := &apiIdentity{UserID: uploadUserID, WorkspaceID: models.DefaultWorkspaceID, Role: models.RoleEditor}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), apiIdentityContextKey{}, identity)))
	})
}

func TestAPIUploadBehindIdentityCopyLeavesNoTempFiles(t *testing.T) {
	for name, test := range map[string]struct {
		target string
		size   int64
		status int
	}{
		// The parsed-then-rejected case: without the cleanup, a 33 MB temp
		// file outlives the request.
		"parsed body, malformed query": {target: "/api/flipbooks?broken=%zz", size: apiSpoolSize, status: http.StatusBadRequest},
		"over the size limit":          {target: "/api/flipbooks", size: 48 << 20, status: http.StatusRequestEntityTooLarge},
	} {
		t.Run(name, func(t *testing.T) {
			dir := uploadTempDir(t)
			h := NewAPIHandler(nil, storage.New(t.TempDir()), nil, "http://flipbook.test", 40<<20)
			router := chi.NewRouter()
			router.With(withAPIIdentity).Post("/api/flipbooks", h.UploadFlipbook)

			body, contentType := streamedUpload(t, map[string]string{"title": "Report"}, uploadPart{"file", "report.pdf", test.size})
			r := httptest.NewRequest(http.MethodPost, test.target, body)
			r.Header.Set("Content-Type", contentType)
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, r)
			if recorder.Code != test.status {
				t.Fatalf("response %d %q, want %d", recorder.Code, recorder.Body.String(), test.status)
			}
			assertNoUploadTempFiles(t, dir)
		})
	}
}
