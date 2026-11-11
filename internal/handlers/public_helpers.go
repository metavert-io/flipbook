package handlers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html/template"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/metavert-io/flipbook/internal/auth"
	"github.com/metavert-io/flipbook/internal/database"
	"github.com/metavert-io/flipbook/internal/distribution"
	"github.com/metavert-io/flipbook/internal/models"
	"github.com/metavert-io/flipbook/internal/publishing"
	"github.com/metavert-io/flipbook/internal/storage"
)

func readerService(services []*publishing.Service) *publishing.Service {
	if len(services) > 0 {
		return services[0]
	}
	return nil
}

func readerWorkspace(r *http.Request, db *database.DB) (*models.Workspace, error) {
	if id := chi.URLParam(r, "workspaceID"); id != "" {
		return db.GetWorkspace(r.Context(), id)
	}
	if slug := chi.URLParam(r, "workspaceSlug"); slug != "" {
		return db.GetWorkspaceBySlug(r.Context(), slug)
	}
	if slug := chi.URLParam(r, "workspace"); slug != "" {
		return db.GetWorkspaceBySlug(r.Context(), slug)
	}
	if r != nil && r.Host != "" {
		workspace, err := db.GetWorkspaceByDomain(r.Context(), strings.ToLower(r.Host))
		if err == nil {
			return workspace, nil
		}
		if !errors.Is(err, database.ErrNotFound) {
			return nil, err
		}
	}
	return db.GetWorkspace(r.Context(), models.DefaultWorkspaceID)
}

func readerAccessRequest(r *http.Request, service *publishing.Service, flipbookID string, viaEmbed bool) publishing.AccessRequest {
	request := publishing.AccessRequest{ViaEmbed: viaEmbed}
	if r == nil {
		return request
	}
	request.Grant = strings.TrimSpace(r.URL.Query().Get("access_grant"))
	if request.Grant == "" && service != nil {
		request.Grant = service.GrantFromRequest(r, flipbookID)
	}
	request.ShareToken = strings.TrimSpace(r.URL.Query().Get("share_token"))
	request.Revision = readerPositiveInt(r.URL.Query().Get("revision"))
	request.Page = readerPositiveInt(r.URL.Query().Get("asset_page"))
	if viaEmbed {
		request.EmbedHost = readerReferrerOrigin(r.Referer())
	}
	if user, ok := auth.UserFromContext(r.Context()); ok && user != nil {
		request.Reader.UserID = user.ID
		email := user.NormalizedEmail
		if email == "" {
			email = strings.ToLower(strings.TrimSpace(user.Email))
		}
		if email != "" {
			request.Reader.EmailHash = models.HashToken(email)
		}
	}
	return request
}

func readerReferrerOrigin(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return ""
	}
	return parsed.Scheme + "://" + parsed.Host
}

func readerPositiveInt(value string) int {
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 0 {
		return 0
	}
	return parsed
}

func applyPublishingHeaders(w http.ResponseWriter, decision publishing.Decision) {
	for key, values := range decision.Headers() {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	if decision.FrameAncestors != "" {
		w.Header().Del("X-Frame-Options")
	}
}

// allowFraming lets an /embed response that is not the reader itself (error,
// password, wait and unavailable pages) render inside the host site's iframe
// under the publication's own frame-ancestors policy. Without it the global
// X-Frame-Options: SAMEORIGIN makes the browser show "refused to connect"
// and hides the actual reason from the reader and the site owner.
func allowFraming(w http.ResponseWriter, fb *models.Flipbook) {
	var domains []string
	if fb != nil {
		domains = fb.Embed.AllowedDomains
	}
	w.Header().Del("X-Frame-Options")
	w.Header().Set("Content-Security-Policy", "frame-ancestors "+publishing.FrameAncestors(domains))
}

func writePublishingError(w http.ResponseWriter, err error) {
	status := publishing.HTTPStatus(err)
	message := publishing.PublicMessage(err)
	if errors.Is(err, database.ErrNotFound) {
		status, message = http.StatusNotFound, "publication not found"
	}
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Bearer realm="flipbook-publication"`)
	}
	w.Header().Set("Cache-Control", publishing.CacheNoStore)
	w.Header().Set("X-Robots-Tag", "noindex, nofollow, noarchive")
	jsonError(w, message, status)
}

func readerAssetURL(flipbookID string, revision int, kind publishing.AssetKind, page int, name, grant string) string {
	values := make(url.Values)
	if revision > 0 {
		values.Set("revision", strconv.Itoa(revision))
	}
	values.Set("kind", string(kind))
	if page > 0 {
		values.Set("page", strconv.Itoa(page))
	}
	if name != "" {
		values.Set("name", name)
	}
	if grant != "" {
		values.Set("access_grant", grant)
	}
	return "/api/public/flipbooks/" + url.PathEscape(flipbookID) + "/asset?" + values.Encode()
}

func readerEndpoint(flipbookID, action, grant string) string {
	raw := "/api/public/flipbooks/" + url.PathEscape(flipbookID) + "/" + action
	if grant != "" {
		raw += "?access_grant=" + url.QueryEscape(grant)
	}
	return raw
}

func readerArtifactDir(store *storage.Storage, flipbookID string, revision int) (string, error) {
	if store == nil {
		return "", errors.New("storage is not configured")
	}
	current, err := store.CurrentRevision(flipbookID)
	if err != nil {
		return "", err
	}
	if current == 0 && (revision == 0 || revision == 1) {
		return store.FlipbookDir(flipbookID), nil
	}
	if revision == 0 {
		revision = current
	}
	if revision < 1 {
		return "", storage.ErrRevisionMissing
	}
	return store.RevisionDir(flipbookID, revision), nil
}

func readerRender(tmpl *template.Template, w http.ResponseWriter, name string, data any, status int) error {
	if tmpl == nil {
		return errors.New("templates are not configured")
	}
	var output bytes.Buffer
	if err := tmpl.ExecuteTemplate(&output, name, data); err != nil {
		return err
	}
	if status == 0 {
		status = http.StatusOK
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, err := output.WriteTo(w)
	return err
}

func readerServeFile(ctx context.Context, w http.ResponseWriter, r *http.Request, store *storage.Storage, flipbookID string, revision int, name, downloadName string, attachment bool) error {
	if store == nil {
		return errors.New("storage is not configured")
	}
	contentType := mime.TypeByExtension(strings.ToLower(filepath.Ext(name)))
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if downloadName != "" {
		disposition := "inline"
		if attachment {
			disposition = "attachment"
		}
		formatted := mime.FormatMediaType(disposition, map[string]string{"filename": filepath.Base(downloadName)})
		if formatted != "" {
			w.Header().Set("Content-Disposition", formatted)
		}
	}
	directory, err := readerArtifactDir(store, flipbookID, revision)
	if err == nil {
		clean := filepath.Clean(filepath.FromSlash(name))
		if clean != "." && clean != ".." && !strings.HasPrefix(clean, ".."+string(filepath.Separator)) && !filepath.IsAbs(clean) {
			path := filepath.Join(directory, clean)
			file, openErr := os.Open(path)
			if openErr == nil {
				defer file.Close()
				info, statErr := file.Stat()
				if statErr != nil || !info.Mode().IsRegular() {
					return os.ErrNotExist
				}
				http.ServeContent(w, r, info.Name(), info.ModTime(), file)
				return nil
			}
			if !errors.Is(openErr, os.ErrNotExist) {
				return openErr
			}
		}
	}
	if store.ObjectStorageEnabled() {
		object, openErr := store.OpenRevisionObject(ctx, flipbookID, revision, name)
		if openErr == nil {
			defer object.Close()
			_, err = io.Copy(w, object)
			return err
		}
		if !errors.Is(openErr, os.ErrNotExist) {
			return openErr
		}
	}
	if err != nil {
		return err
	}
	return os.ErrNotExist
}

func readerDownloadName(fb *models.Flipbook, extension string) string {
	title := models.NormalizeSlug(fb.Title)
	if title == "" {
		title = "flipbook"
	}
	if !strings.HasPrefix(extension, ".") {
		extension = "." + extension
	}
	return title + extension
}

func readerAbsoluteAsset(baseURL, raw string) string {
	if strings.HasPrefix(raw, "/") {
		return distribution.AbsoluteURL(baseURL, raw)
	}
	return raw
}

func readerInternalError(w http.ResponseWriter, err error) {
	if errors.Is(err, os.ErrNotExist) {
		jsonError(w, "asset not found", http.StatusNotFound)
		return
	}
	jsonError(w, fmt.Sprintf("publication asset unavailable"), http.StatusInternalServerError)
	_ = err
}
