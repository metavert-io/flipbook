package handlers

import (
	"html/template"
	"net/http"
	"net/url"
	"strings"

	"github.com/metavert-io/flipbook/internal/publishing"
)

// fetchMetadataVary names the request headers that decide whether a reader
// URL answers as a page or as an embed, so caches keep the two apart.
const fetchMetadataVary = "Sec-Fetch-Dest, Sec-Fetch-Site"

// framedByAnotherSite reports whether the browser is loading this response
// into a frame whose parent is on another origin, going by Fetch Metadata.
// Browsers that send no Sec-Fetch-* headers report false and keep the
// default page behaviour.
func framedByAnotherSite(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Dest") {
	case "iframe", "frame":
	default:
		return false
	}
	switch r.Header.Get("Sec-Fetch-Site") {
	case "cross-site", "same-site":
		return true
	}
	return false
}

// redirectToEmbed sends a viewer URL that another site put in an iframe to
// the embed route, which applies the publication's own embed policy. The
// standalone viewer stays SAMEORIGIN, so without this a pasted share link
// shows up on the host page only as "refused to connect".
func redirectToEmbed(w http.ResponseWriter, r *http.Request, slug string) {
	target := "/embed/" + url.PathEscape(slug)
	if query := r.URL.RawQuery; query != "" {
		target += "?" + query
	}
	w.Header().Set("Cache-Control", publishing.CacheNoStore)
	http.Redirect(w, r, target, http.StatusFound)
}

// EmbedAddressHint wraps the site root. When another site frames the root —
// usually because the bare domain was used as the iframe src — it answers
// with a frameable page naming the embed address, instead of redirecting to
// the SAMEORIGIN sign-in page that the host page can only show as "refused to
// connect". The hint has no forms or data, so letting any site frame it is
// safe. Every other request reaches next unchanged.
func EmbedAddressHint(next http.Handler, tmpl *template.Template, baseURL string) http.Handler {
	embedPrefix := strings.TrimRight(baseURL, "/") + "/embed/"
	message := "This embed points at the site's home page, not a publication. " +
		"Use the embed code from the publication's page in the admin; its address starts with " + embedPrefix
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", fetchMetadataVary)
		if !framedByAnotherSite(r) {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Cache-Control", publishing.CacheNoStore)
		w.Header().Set("X-Robots-Tag", "noindex, nofollow, noarchive")
		allowFraming(w, nil)
		data := map[string]any{
			"Status": http.StatusNotFound, "Code": publishing.CodeNotFound, "Message": message,
			"Language": "en", "Direction": "ltr",
		}
		if err := readerRender(tmpl, w, "public_error", data, http.StatusNotFound); err != nil {
			http.Error(w, message, http.StatusNotFound)
		}
	})
}
