package handlers

import (
	"html/template"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/metavert-io/flipbook/internal/database"
	"github.com/metavert-io/flipbook/internal/models"
	"github.com/metavert-io/flipbook/internal/publishing"
)

func embedErrorTemplates(t *testing.T) *template.Template {
	t.Helper()
	return template.Must(template.New("").Parse(
		`{{define "public_error"}}<p>{{.Message}}</p>{{end}}{{define "public_access"}}<form></form>{{end}}`))
}

// withGlobalHeaders mimics main.securityHeaders, which runs before every handler.
func withGlobalHeaders() *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	w.Header().Set("X-Frame-Options", "SAMEORIGIN")
	return w
}

func TestEmbedErrorPageIsFrameable(t *testing.T) {
	h := NewViewerHandler(nil, nil, embedErrorTemplates(t), "https://bok.example")
	disabled := &publishing.HTTPError{Status: http.StatusForbidden, Code: publishing.CodeEmbedDisabled, Message: "embedding is disabled"}
	tests := []struct {
		name string
		fb   *models.Flipbook
		err  error
		csp  string
	}{
		{"embedding disabled, any ancestor", &models.Flipbook{ID: "fb", Slug: "book"}, disabled, "frame-ancestors *"},
		{"embedding disabled, allowlist", &models.Flipbook{ID: "fb", Slug: "book", Embed: models.EmbedSettings{AllowedDomains: []string{"penninn.is"}}},
			disabled, "frame-ancestors 'self' https://penninn.is https://www.penninn.is"},
		{"publication not found", nil, database.ErrNotFound, "frame-ancestors *"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			w := withGlobalHeaders()
			h.renderAccessError(w, httptest.NewRequest(http.MethodGet, "/embed/book", nil), test.fb, test.err, true)
			if got := w.Header().Get("X-Frame-Options"); got != "" {
				t.Fatalf("embed error kept X-Frame-Options %q; the browser would hide the message", got)
			}
			if got := w.Header().Get("Content-Security-Policy"); got != test.csp {
				t.Fatalf("CSP = %q, want %q", got, test.csp)
			}
		})
	}
	w := withGlobalHeaders()
	h.renderAccessError(w, httptest.NewRequest(http.MethodGet, "/embed/book", nil), tests[0].fb, disabled, true)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "embedding is disabled") {
		t.Fatalf("embed error = %d %q", w.Code, w.Body.String())
	}
}

func TestViewerErrorPageStaysSameOrigin(t *testing.T) {
	h := NewViewerHandler(nil, nil, embedErrorTemplates(t), "https://bok.example")
	w := withGlobalHeaders()
	h.renderAccessError(w, httptest.NewRequest(http.MethodGet, "/v/book", nil), &models.Flipbook{ID: "fb"}, database.ErrNotFound, false)
	if got := w.Header().Get("X-Frame-Options"); got != "SAMEORIGIN" {
		t.Fatalf("non-embed error lost X-Frame-Options: %q", got)
	}
	if got := w.Header().Get("Content-Security-Policy"); got != "" {
		t.Fatalf("non-embed error gained CSP %q", got)
	}
}

func TestApplyPublishingHeadersDropsFrameOptionsForEmbeds(t *testing.T) {
	w := withGlobalHeaders()
	applyPublishingHeaders(w, publishing.Decision{FrameAncestors: "*"})
	if got := w.Header().Get("X-Frame-Options"); got != "" {
		t.Fatalf("embed kept X-Frame-Options %q alongside frame-ancestors", got)
	}
	w = withGlobalHeaders()
	applyPublishingHeaders(w, publishing.Decision{})
	if got := w.Header().Get("X-Frame-Options"); got != "SAMEORIGIN" {
		t.Fatalf("viewer lost X-Frame-Options: %q", got)
	}
}
