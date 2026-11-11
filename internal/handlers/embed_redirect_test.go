package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

func framedRequest(target, dest, site string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if dest != "" {
		req.Header.Set("Sec-Fetch-Dest", dest)
	}
	if site != "" {
		req.Header.Set("Sec-Fetch-Site", site)
	}
	return req
}

func TestViewerFramedByAnotherSiteRedirectsToEmbed(t *testing.T) {
	h := NewViewerHandler(nil, nil, embedErrorTemplates(t), "https://bok.example")
	router := chi.NewRouter()
	router.Get("/v/{slug}", h.View)

	tests := []struct {
		name, target, dest, site, location string
	}{
		{"cross-site iframe", "/v/book", "iframe", "cross-site", "/embed/book"},
		{"same-site iframe keeps query", "/v/book?page=3", "iframe", "same-site", "/embed/book?page=3"},
		{"legacy frame element", "/v/book", "frame", "cross-site", "/embed/book"},
		{"escaped slug", "/v/vor%20bok", "iframe", "cross-site", "/embed/vor%20bok"},
		{"top-level page", "/v/book", "document", "cross-site", ""},
		{"same-origin iframe", "/v/book", "iframe", "same-origin", ""},
		{"no fetch metadata", "/v/book", "", "", ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			w := withGlobalHeaders()
			router.ServeHTTP(w, framedRequest(test.target, test.dest, test.site))
			if !strings.Contains(strings.Join(w.Header().Values("Vary"), ","), "Sec-Fetch-Dest") {
				t.Fatalf("Vary = %q, want Sec-Fetch-Dest so caches keep page and embed apart", w.Header().Values("Vary"))
			}
			got := w.Header().Get("Location")
			if test.location == "" {
				if w.Code == http.StatusFound || got != "" {
					t.Fatalf("redirected to %q; the viewer should render as a page", got)
				}
				return
			}
			if w.Code != http.StatusFound || got != test.location {
				t.Fatalf("got %d Location %q, want 302 %q", w.Code, got, test.location)
			}
			if w.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("redirect Cache-Control = %q, want no-store", w.Header().Get("Cache-Control"))
			}
		})
	}
}

func TestEmbedAddressHintFramesOnlyForOtherSites(t *testing.T) {
	root := EmbedAddressHint(http.RedirectHandler("/admin", http.StatusFound), embedErrorTemplates(t), "https://bok.example/")

	w := withGlobalHeaders()
	root.ServeHTTP(w, framedRequest("/", "iframe", "cross-site"))
	if w.Code != http.StatusNotFound {
		t.Fatalf("framed root = %d, want 404 hint", w.Code)
	}
	if got := w.Header().Get("X-Frame-Options"); got != "" {
		t.Fatalf("framed root kept X-Frame-Options %q; the browser would hide the hint", got)
	}
	if got := w.Header().Get("Content-Security-Policy"); got != "frame-ancestors *" {
		t.Fatalf("framed root CSP = %q", got)
	}
	if body := w.Body.String(); !strings.Contains(body, "https://bok.example/embed/") {
		t.Fatalf("hint does not name the embed address: %q", body)
	}

	for _, req := range []*http.Request{
		framedRequest("/", "document", "none"),
		framedRequest("/", "iframe", "same-origin"),
		framedRequest("/", "", ""),
	} {
		w := withGlobalHeaders()
		root.ServeHTTP(w, req)
		if w.Code != http.StatusFound || w.Header().Get("Location") != "/admin" {
			t.Fatalf("dest=%q site=%q: got %d %q, want the /admin redirect", req.Header.Get("Sec-Fetch-Dest"), req.Header.Get("Sec-Fetch-Site"), w.Code, w.Header().Get("Location"))
		}
		if got := w.Header().Get("X-Frame-Options"); got != "SAMEORIGIN" {
			t.Fatalf("unframed root lost X-Frame-Options: %q", got)
		}
	}
}
