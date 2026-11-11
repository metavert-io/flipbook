package auth

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/metavert-io/flipbook/internal/multipartform"
)

// csrfSpoolSize is above every maxMemory the CSRF check has used for
// multipart bodies, so a parse there spools the file part to disk.
const csrfSpoolSize = 2 << 20

type fillByte byte

func (b fillByte) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(b)
	}
	return len(p), nil
}

func csrfSpoolDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	return dir
}

func assertCSRFSpoolEmpty(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("multipart temp file left behind: %d file(s), first %q", len(entries), entries[0].Name())
	}
}

// csrfUploadRequest is a browser-ordered multipart form: the token field
// first, then a file part large enough to spool.
func csrfUploadRequest(t *testing.T, target, token string) *http.Request {
	t.Helper()
	var head bytes.Buffer
	writer := multipart.NewWriter(&head)
	if err := writer.WriteField(CSRFFormField, token); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteField("title", "Report"); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.CreateFormFile("file", "report.pdf"); err != nil {
		t.Fatal(err)
	}
	prefix := bytes.Clone(head.Bytes())
	head.Reset()
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	body := io.MultiReader(bytes.NewReader(prefix), io.LimitReader(fillByte('A'), csrfSpoolSize), &head)
	r := httptest.NewRequest(http.MethodPost, target, body)
	r.Header.Set("Content-Type", writer.FormDataContentType())
	r.Header.Set("Origin", "http://"+r.Host)
	r.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: testCSRFCookie()})
	return r
}

func TestCSRFMiddlewareReleasesMultipartForms(t *testing.T) {
	a := testAuth(t, newFakeStore(), Options{})
	valid := a.csrfToken(testCSRFCookie(), "")
	for name, test := range map[string]struct {
		token  string
		status int
	}{
		"rejected token": {token: "not-the-token", status: http.StatusForbidden},
		"accepted token": {token: valid, status: http.StatusNoContent},
	} {
		t.Run(name, func(t *testing.T) {
			dir := csrfSpoolDir(t)
			handler := a.CSRFMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// A handler reading the same form the CSRF check may have
				// parsed, as the admin forms do.
				if err := r.ParseMultipartForm(1 << 20); err != nil || r.FormValue("title") != "Report" {
					t.Errorf("handler could not read the form: %v", err)
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, csrfUploadRequest(t, "/admin/settings", test.token))
			if recorder.Code != test.status {
				t.Fatalf("status = %d, want %d", recorder.Code, test.status)
			}
			assertCSRFSpoolEmpty(t, dir)
		})
	}
}

// The sign-in and password forms check CSRF themselves and mount without
// middleware, so main.go's root multipartform.Cleanup is what releases a form
// their check parsed. This pins down that they parse the request they are
// handed rather than a further copy.
func TestFormSecurityHandlersReleasedByRootCleanup(t *testing.T) {
	a := testAuth(t, newFakeStore(), Options{})
	valid := a.csrfToken(testCSRFCookie(), "")
	for name, handler := range map[string]http.HandlerFunc{
		"login":                  a.LoginSubmit,
		"password reset request": a.PasswordResetRequestHandler,
	} {
		for tokenName, token := range map[string]string{"rejected token": "not-the-token", "accepted token": valid} {
			t.Run(name+"/"+tokenName, func(t *testing.T) {
				dir := csrfSpoolDir(t)
				recorder := httptest.NewRecorder()
				multipartform.Cleanup(handler).ServeHTTP(recorder, csrfUploadRequest(t, "/login", token))
				if rejected := recorder.Code == http.StatusForbidden; rejected != (token != valid) {
					t.Fatalf("status = %d for %s", recorder.Code, tokenName)
				}
				assertCSRFSpoolEmpty(t, dir)
			})
		}
	}
}
