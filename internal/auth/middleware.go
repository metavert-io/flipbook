package auth

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/metavert-io/flipbook/internal/database"
	"github.com/metavert-io/flipbook/internal/models"
	"github.com/metavert-io/flipbook/internal/multipartform"
)

func (a *Auth) SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		if a != nil && a.opts.HSTS && a.opts.HSTSMaxAge > 0 && a.requestScheme(r) == "https" {
			value := "max-age=" + strconv.FormatInt(int64(a.opts.HSTSMaxAge/time.Second), 10)
			if a.opts.HSTSIncludeSubdomains {
				value += "; includeSubDomains"
			}
			w.Header().Set("Strict-Transport-Security", value)
		}
		next.ServeHTTP(w, r)
	})
}

func directRemoteIP(r *http.Request) net.IP {
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err != nil {
		host = strings.TrimSpace(r.RemoteAddr)
	}
	return net.ParseIP(host)
}

func (a *Auth) trustedProxy(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if _, ok := a.trustedIPs[ip.String()]; ok {
		return true
	}
	for _, network := range a.trustedNets {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

func (a *Auth) clientIP(r *http.Request) string {
	remote := directRemoteIP(r)
	if remote == nil || !a.trustedProxy(remote) {
		if remote == nil {
			return "unknown"
		}
		return remote.String()
	}
	forwarded := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	candidate := remote.String()
	for index := len(forwarded) - 1; index >= 0; index-- {
		ip := net.ParseIP(strings.TrimSpace(forwarded[index]))
		if ip == nil {
			return remote.String()
		}
		candidate = ip.String()
		if !a.trustedProxy(ip) {
			return candidate
		}
	}
	return candidate
}

func (a *Auth) requestScheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	if a != nil && a.opts.HTTPSMode {
		return "https"
	}
	if a != nil && a.trustedProxy(directRemoteIP(r)) {
		scheme := strings.ToLower(strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0]))
		if scheme == "https" || scheme == "http" {
			return scheme
		}
	}
	return "http"
}

func safeMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return true
	default:
		return false
	}
}

func canonicalOrigin(scheme, host string) (string, bool) {
	scheme = strings.ToLower(scheme)
	if scheme != "http" && scheme != "https" {
		return "", false
	}
	parsed, err := url.Parse(scheme + "://" + host)
	if err != nil || parsed.Host == "" || parsed.User != nil {
		return "", false
	}
	hostname := strings.ToLower(parsed.Hostname())
	if hostname == "" {
		return "", false
	}
	port := parsed.Port()
	if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
		port = ""
	}
	if port != "" {
		host = net.JoinHostPort(hostname, port)
	} else if strings.Contains(hostname, ":") {
		host = "[" + hostname + "]"
	} else {
		host = hostname
	}
	return scheme + "://" + host, true
}

func (a *Auth) validateOrigin(r *http.Request) error {
	if safeMethod(r.Method) {
		return nil
	}
	if strings.EqualFold(r.Header.Get("Sec-Fetch-Site"), "cross-site") {
		return ErrInvalidOrigin
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		referer := strings.TrimSpace(r.Header.Get("Referer"))
		if referer == "" {
			return ErrInvalidOrigin
		}
		parsed, err := url.Parse(referer)
		if err != nil {
			return ErrInvalidOrigin
		}
		origin = parsed.Scheme + "://" + parsed.Host
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Host == "" {
		return ErrInvalidOrigin
	}
	origin, valid := canonicalOrigin(parsed.Scheme, parsed.Host)
	if !valid {
		return ErrInvalidOrigin
	}
	expected, valid := canonicalOrigin(a.requestScheme(r), r.Host)
	if valid && hmac.Equal([]byte(origin), []byte(expected)) {
		return nil
	}
	if a.opts.BaseURL != "" {
		base, err := url.Parse(a.opts.BaseURL)
		if err == nil {
			configured, valid := canonicalOrigin(base.Scheme, base.Host)
			if valid && hmac.Equal([]byte(origin), []byte(configured)) {
				return nil
			}
		}
	}
	return ErrInvalidOrigin
}

func validCSRFCookie(value string) bool {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	return err == nil && len(decoded) == 32
}

func (a *Auth) csrfToken(cookieValue, sessionValue string) string {
	mac := hmac.New(sha256.New, []byte(a.secret))
	mac.Write([]byte("csrf\x00"))
	mac.Write([]byte(cookieValue))
	mac.Write([]byte("\x00"))
	mac.Write([]byte(sessionValue))
	return hex.EncodeToString(mac.Sum(nil))
}

func (a *Auth) ensureCSRF(w http.ResponseWriter, r *http.Request) (string, error) {
	if a == nil || a.initErr != nil {
		return "", ErrNotConfigured
	}
	cookieValue := ""
	if cookie, err := r.Cookie(CSRFCookieName); err == nil && validCSRFCookie(cookie.Value) {
		cookieValue = cookie.Value
	}
	if cookieValue == "" {
		var err error
		cookieValue, err = randomToken(a.random, 32)
		if err != nil {
			return "", err
		}
		http.SetCookie(w, &http.Cookie{
			Name: CSRFCookieName, Value: cookieValue, Path: "/", HttpOnly: true,
			Secure: a.opts.SecureCookies || a.opts.HTTPSMode, SameSite: http.SameSiteLaxMode,
		})
		r.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: cookieValue})
	}
	sessionValue := ""
	if cookie, err := r.Cookie(SessionCookieName); err == nil {
		sessionValue = cookie.Value
	}
	token := a.csrfToken(cookieValue, sessionValue)
	*r = *r.WithContext(context.WithValue(r.Context(), csrfContextKey, token))
	return token, nil
}

func (a *Auth) CSRFToken(w http.ResponseWriter, r *http.Request) (string, error) {
	return a.ensureCSRF(w, r)
}

const (
	// maxCSRFFormBytes caps a url-encoded body the CSRF check parses into
	// memory. It matches the largest url-encoded form the admin handlers
	// accept; the parsed form is what those handlers then read.
	maxCSRFFormBytes = 2 << 20
	// maxCSRFMultipartScan is how far into a multipart body the CSRF check
	// looks for the token. The scanned bytes stay in memory and are replayed
	// to the handler, so the check never spools file parts to disk and the
	// handler's own MaxBytesReader still governs the upload size.
	maxCSRFMultipartScan = 64 << 10
	maxCSRFTokenBytes    = 256
	// maxAuthFormBytes caps the whole body of the forms that are reachable
	// without a session (login, invitation, password reset). They hold a few
	// short fields.
	maxAuthFormBytes = 64 << 10
)

func bodyTooLarge(err error) bool {
	var maxBytesErr *http.MaxBytesError
	return errors.As(err, &maxBytesErr)
}

// replayedBody hands the bytes the CSRF scan already read back to the
// handler ahead of the unread remainder of the request body.
type replayedBody struct {
	io.Reader
	io.Closer
}

// requestCSRFToken prefers the X-CSRF-Token header, which leaves the body
// untouched. Url-encoded bodies are parsed under maxCSRFFormBytes; multipart
// bodies are scanned without being consumed. Tokens in the URL query are not
// accepted.
func requestCSRFToken(w http.ResponseWriter, r *http.Request) (string, error) {
	if token := strings.TrimSpace(r.Header.Get(CSRFHeaderName)); token != "" {
		return token, nil
	}
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return "", ErrInvalidCSRF
	}
	switch mediaType {
	case "application/x-www-form-urlencoded":
		r.Body = http.MaxBytesReader(w, r.Body, maxCSRFFormBytes)
		if err := r.ParseForm(); err != nil {
			if bodyTooLarge(err) {
				return "", ErrRequestTooLarge
			}
			return "", ErrInvalidCSRF
		}
		return r.PostForm.Get(CSRFFormField), nil
	case "multipart/form-data":
		return multipartCSRFToken(r, params["boundary"])
	default:
		return "", ErrInvalidCSRF
	}
}

// multipartCSRFToken finds the token field within the first
// maxCSRFMultipartScan bytes of a multipart body and then restores the body
// so the handler can parse it in full. The token must come before any file
// part; browsers serialize fields in document order and every admin form
// renders csrf_token first.
func multipartCSRFToken(r *http.Request, boundary string) (string, error) {
	if boundary == "" {
		return "", ErrInvalidCSRF
	}
	original := r.Body
	var scanned bytes.Buffer
	reader := multipart.NewReader(io.TeeReader(io.LimitReader(original, maxCSRFMultipartScan), &scanned), boundary)
	defer func() {
		r.Body = replayedBody{Reader: io.MultiReader(&scanned, original), Closer: original}
	}()
	for {
		part, err := reader.NextPart()
		if err != nil {
			return "", ErrInvalidCSRF
		}
		if part.FileName() != "" {
			return "", ErrInvalidCSRF
		}
		if part.FormName() != CSRFFormField {
			continue
		}
		value, err := io.ReadAll(io.LimitReader(part, maxCSRFTokenBytes+1))
		if err != nil || len(value) > maxCSRFTokenBytes {
			return "", ErrInvalidCSRF
		}
		return strings.TrimSpace(string(value)), nil
	}
}

func (a *Auth) validateCSRF(w http.ResponseWriter, r *http.Request) error {
	cookie, err := r.Cookie(CSRFCookieName)
	if err != nil || !validCSRFCookie(cookie.Value) {
		return ErrInvalidCSRF
	}
	token, err := requestCSRFToken(w, r)
	if errors.Is(err, ErrRequestTooLarge) {
		return err
	}
	if err != nil || token == "" {
		return ErrInvalidCSRF
	}
	sessionValue := ""
	if session, err := r.Cookie(SessionCookieName); err == nil {
		sessionValue = session.Value
	}
	expected := a.csrfToken(cookie.Value, sessionValue)
	if !hmac.Equal([]byte(token), []byte(expected)) {
		return ErrInvalidCSRF
	}
	return nil
}

func (a *Auth) CSRFMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The token check may parse a multipart body, and next receives this
		// same request, so the form is released here whether the check
		// rejects it or a handler goes on to use it.
		defer multipartform.Release(r)
		_, sessionCookieErr := r.Cookie(SessionCookieName)
		if !safeMethod(r.Method) && sessionCookieErr != nil && strings.HasPrefix(strings.ToLower(r.Header.Get("Authorization")), "bearer ") {
			next.ServeHTTP(w, r)
			return
		}
		if err := a.validateOrigin(r); err != nil {
			http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
			return
		}
		if _, err := a.ensureCSRF(w, r); err != nil {
			http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
			return
		}
		if !safeMethod(r.Method) {
			if err := a.validateCSRF(w, r); err != nil {
				status := http.StatusForbidden
				if errors.Is(err, ErrRequestTooLarge) {
					status = http.StatusRequestEntityTooLarge
				}
				http.Error(w, http.StatusText(status), status)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// OptionalAuth populates the request principal when the request carries valid
// credentials, but never rejects or redirects anonymous requests. Used on
// public reader routes so signed-in staff can preview publications.
func (a *Auth) OptionalAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := PrincipalFromContext(r.Context()); !ok {
			if principal, err := a.principalFromRequest(r); err == nil {
				r = withPrincipal(r, principal)
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (a *Auth) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := PrincipalFromContext(r.Context()); ok {
			next.ServeHTTP(w, r)
			return
		}
		principal, err := a.principalFromRequest(r)
		if err == nil {
			next.ServeHTTP(w, withPrincipal(r, principal))
			return
		}
		if errors.Is(err, ErrUnauthorized) {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		if errors.Is(err, ErrForbidden) {
			http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
			return
		}
		http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
	})
}

func (a *Auth) principalForRole(r *http.Request, workspaceID string, required models.Role) (*Principal, error) {
	if !required.Valid() {
		return nil, ErrForbidden
	}
	principal, ok := PrincipalFromContext(r.Context())
	if !ok {
		var err error
		principal, err = a.principalFromRequest(r)
		if err != nil {
			return nil, err
		}
	}
	if principal.APIKey != nil && workspaceID != "" && workspaceID != principal.APIKey.WorkspaceID {
		return nil, ErrForbidden
	}
	if workspaceID == "" {
		workspaceID = principal.WorkspaceID()
	}
	membership := principal.Membership
	var err error
	if membership == nil || membership.WorkspaceID != workspaceID {
		membership, err = a.store.GetMembership(r.Context(), workspaceID, principal.User.ID)
		if err != nil {
			return nil, err
		}
	}
	if !membership.Role.AtLeast(required) {
		return nil, ErrForbidden
	}
	clone := *principal
	clone.Membership = membership
	return &clone, nil
}

func (a *Auth) requireRole(r *http.Request, workspaceID string, required models.Role) (*http.Request, error) {
	principal, err := a.principalForRole(r, workspaceID, required)
	if err != nil {
		return nil, err
	}
	return withPrincipal(r, principal), nil
}

func roleError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrUnauthorized):
		http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
	case errors.Is(err, ErrForbidden), errors.Is(err, database.ErrNotFound):
		http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
	default:
		http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
	}
}

func (a *Auth) RequireRole(required models.Role) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			updated, err := a.requireRole(r, "", required)
			if err != nil {
				roleError(w, err)
				return
			}
			next.ServeHTTP(w, updated)
		})
	}
}

func (a *Auth) RequireWorkspace(required models.Role) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			workspaceID := chi.URLParam(r, "workspaceID")
			if workspaceID == "" {
				workspaceID = strings.TrimSpace(r.Header.Get("X-Workspace-ID"))
			}
			updated, err := a.requireRole(r, workspaceID, required)
			if err != nil {
				roleError(w, err)
				return
			}
			next.ServeHTTP(w, updated)
		})
	}
}

func (a *Auth) RequireWorkspaceRole(workspaceID string, required models.Role) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			updated, err := a.requireRole(r, workspaceID, required)
			if err != nil {
				roleError(w, err)
				return
			}
			next.ServeHTTP(w, updated)
		})
	}
}

func bearerToken(r *http.Request) (string, bool) {
	parts := strings.Fields(r.Header.Get("Authorization"))
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || parts[1] == "" {
		return "", false
	}
	return parts[1], true
}

func writeAPIError(w http.ResponseWriter, status int, value string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"error":%q}`, value)
}

func (a *Auth) RequireAPIKey(requiredScope string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if a == nil || a.initErr != nil {
				writeAPIError(w, http.StatusServiceUnavailable, "authentication unavailable")
				return
			}
			if requiredScope != "" && !validScope(requiredScope) {
				writeAPIError(w, http.StatusInternalServerError, "invalid scope configuration")
				return
			}
			secret, ok := bearerToken(r)
			if !ok {
				w.Header().Set("WWW-Authenticate", `Bearer realm="flipbook"`)
				writeAPIError(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			var key *models.APIKey
			var user *models.User
			var membership *models.Membership
			if a.opts.LegacyAPIKey != "" && hmac.Equal([]byte(secret), []byte(a.opts.LegacyAPIKey)) {
				var err error
				user, err = a.store.GetUser(r.Context(), models.LegacyAdminUserID)
				if err == nil {
					membership, err = a.store.GetMembership(r.Context(), models.DefaultWorkspaceID, user.ID)
				}
				if err != nil {
					writeAPIError(w, http.StatusServiceUnavailable, "authentication unavailable")
					return
				}
				key = &models.APIKey{ID: "legacy", WorkspaceID: models.DefaultWorkspaceID, UserID: user.ID, Scopes: []string{"*"}}
			} else {
				var err error
				key, err = a.store.AuthenticateAPIKey(r.Context(), secret, requiredScope)
				if errors.Is(err, database.ErrNotFound) {
					writeAPIError(w, http.StatusUnauthorized, "unauthorized")
					return
				}
				if errors.Is(err, database.ErrForbidden) {
					writeAPIError(w, http.StatusForbidden, "forbidden")
					return
				}
				if err != nil {
					writeAPIError(w, http.StatusServiceUnavailable, "authentication unavailable")
					return
				}
				user, err = a.store.GetUser(r.Context(), key.UserID)
				if err == nil {
					membership, err = a.store.GetMembership(r.Context(), key.WorkspaceID, key.UserID)
				}
				if errors.Is(err, database.ErrNotFound) {
					writeAPIError(w, http.StatusForbidden, "forbidden")
					return
				}
				if err != nil {
					writeAPIError(w, http.StatusServiceUnavailable, "authentication unavailable")
					return
				}
			}
			if key == nil || !key.Active(a.now()) || (requiredScope != "" && !key.HasScope(requiredScope)) || user == nil || !user.Active() || membership == nil || !membership.Role.Valid() {
				writeAPIError(w, http.StatusForbidden, "forbidden")
				return
			}
			if requiredScope != "" && !membership.Role.AtLeast(scopeRole(requiredScope)) {
				writeAPIError(w, http.StatusForbidden, "forbidden")
				return
			}
			principal := &Principal{User: user, Membership: membership, APIKey: key}
			next.ServeHTTP(w, withPrincipal(r, principal))
		})
	}
}
