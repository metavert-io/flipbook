package multipartform

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

// spoolDir points multipart spooling at an empty directory so a test can see
// exactly which temp files a request left behind.
func spoolDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	return dir
}

func tempFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

type repeatByte byte

func (b repeatByte) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(b)
	}
	return len(p), nil
}

// uploadRequest builds a POST whose single file part is size bytes, streamed
// so the test does not hold it in memory.
func uploadRequest(t *testing.T, target string, size int64) *http.Request {
	t.Helper()
	var head bytes.Buffer
	writer := multipart.NewWriter(&head)
	if _, err := writer.CreateFormFile("file", "document.pdf"); err != nil {
		t.Fatal(err)
	}
	prefix := bytes.Clone(head.Bytes())
	head.Reset()
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	body := io.MultiReader(bytes.NewReader(prefix), io.LimitReader(repeatByte('A'), size), &head)
	r := httptest.NewRequest(http.MethodPost, target, body)
	r.Header.Set("Content-Type", writer.FormDataContentType())
	return r
}

type copyKey struct{}

// copyRequest stands in for chi and the auth middleware: next receives a
// copy of the request, so net/http's own cleanup never sees its form.
func copyRequest(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), copyKey{}, true)))
	})
}

const spooledSize = 2 << 20 // above the 1 MB maxMemory used below

// TestReleaseRemovesFormParsedOnRequestCopy runs a real net/http server, so
// the "without Release" case pins down the leak itself: the server's own
// cleanup never reaches a form parsed on a request copy.
func TestReleaseRemovesFormParsedOnRequestCopy(t *testing.T) {
	for name, release := range map[string]bool{"without Release": false, "with Release": true} {
		t.Run(name, func(t *testing.T) {
			dir := spoolDir(t)
			spooled := false
			server := httptest.NewServer(copyRequest(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if release {
					defer Release(r)
				}
				if err := r.ParseMultipartForm(1 << 20); err != nil {
					t.Errorf("parse upload: %v", err)
					return
				}
				spooled = len(tempFiles(t, dir)) == 1
				w.WriteHeader(http.StatusNoContent)
			})))
			defer server.Close()

			upload := uploadRequest(t, server.URL+"/upload", spooledSize)
			upload.RequestURI = ""
			response, err := server.Client().Do(upload)
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			// Close waits for the handler, so no cleanup can still be pending.
			server.Close()

			if response.StatusCode != http.StatusNoContent || !spooled {
				t.Fatalf("status %d, spooled %v: the upload never reached disk, so the test proves nothing", response.StatusCode, spooled)
			}
			left := tempFiles(t, dir)
			if release && len(left) != 0 {
				t.Fatalf("Release left multipart temp files behind: %v", left)
			}
			if !release && len(left) != 1 {
				t.Fatalf("net/http removed a form parsed on a request copy (%v); Release may no longer be needed", left)
			}
		})
	}
}

func TestReleaseRemovesFormWhenParseReportsQueryError(t *testing.T) {
	dir := spoolDir(t)
	r := uploadRequest(t, "/upload?broken=%zz", spooledSize)
	func() {
		defer Release(r)
		if err := r.ParseMultipartForm(1 << 20); err == nil {
			t.Fatal("malformed query was accepted")
		}
		if r.MultipartForm == nil || len(tempFiles(t, dir)) != 1 {
			t.Fatal("ParseMultipartForm no longer spools the body before reporting the query error")
		}
	}()
	if left := tempFiles(t, dir); len(left) != 0 {
		t.Fatalf("rejected upload left multipart temp files behind: %v", left)
	}
}

func TestCleanupReleasesFormParsedByHandler(t *testing.T) {
	dir := spoolDir(t)
	handler := copyRequest(Cleanup(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// FormValue parses multipart bodies with a 32 MB limit, so use the
		// explicit call to force a file to disk.
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatalf("parse upload: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	})))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, uploadRequest(t, "/upload", spooledSize))
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d", recorder.Code)
	}
	if left := tempFiles(t, dir); len(left) != 0 {
		t.Fatalf("Cleanup left multipart temp files behind: %v", left)
	}
}

func TestReleaseWithoutFormIsNoop(t *testing.T) {
	Release(httptest.NewRequest(http.MethodGet, "/", nil))
}
