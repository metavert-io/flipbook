package auth

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/metavert-io/flipbook/internal/models"
)

const testPassword = "correct-horse-battery"

func testCSRFCookie() string {
	return base64.RawURLEncoding.EncodeToString([]byte("01234567890123456789012345678901"))
}

func formRequest(a *Auth, method, target string, values url.Values, sessionToken string) *http.Request {
	var body *strings.Reader
	if values == nil {
		body = strings.NewReader("")
	} else {
		body = strings.NewReader(values.Encode())
	}
	r := httptest.NewRequest(method, target, body)
	r.RemoteAddr = "127.0.0.1:43210"
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if !safeMethod(method) {
		r.Header.Set("Origin", "http://"+r.Host)
	}
	csrf := testCSRFCookie()
	r.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: csrf})
	if sessionToken != "" {
		r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: sessionToken})
	}
	if values == nil {
		values = make(url.Values)
	}
	values.Set(CSRFFormField, a.csrfToken(csrf, sessionToken))
	r.Body = http.NoBody
	r = httptest.NewRequest(method, target, strings.NewReader(values.Encode()))
	r.RemoteAddr = "127.0.0.1:43210"
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if !safeMethod(method) {
		r.Header.Set("Origin", "http://"+r.Host)
	}
	r.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: csrf})
	if sessionToken != "" {
		r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: sessionToken})
	}
	return r
}

func sessionForUser(t *testing.T, a *Auth, user *models.User, workspaceID string) string {
	t.Helper()
	recorder := httptest.NewRecorder()
	if _, err := a.createSessionForUser(context.Background(), recorder, user, workspaceID); err != nil {
		t.Fatal(err)
	}
	for _, cookie := range recorder.Result().Cookies() {
		if cookie.Name == SessionCookieName {
			return cookie.Value
		}
	}
	t.Fatal("session cookie was not set")
	return ""
}

func authenticatedRequest(token, target string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, target, nil)
	r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: token})
	return r
}

func TestBootstrapIsFailClosedAndLocalOnly(t *testing.T) {
	store := newFakeStore()
	a := testAuth(t, store, Options{})
	protected := a.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	recorder := httptest.NewRecorder()
	protected.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/admin", nil))
	if recorder.Code != http.StatusFound || recorder.Header().Get("Location") != "/login" {
		t.Fatalf("unconfigured admin was not closed: %d %s", recorder.Code, recorder.Header().Get("Location"))
	}

	remote := httptest.NewRequest(http.MethodGet, "/login", nil)
	remote.RemoteAddr = "203.0.113.10:1234"
	recorder = httptest.NewRecorder()
	a.LoginPage(recorder, remote)
	if recorder.Code != http.StatusServiceUnavailable || strings.Contains(recorder.Body.String(), "Create administrator") {
		t.Fatalf("remote bootstrap was exposed: %d %s", recorder.Code, recorder.Body.String())
	}

	local := httptest.NewRequest(http.MethodGet, "/login", nil)
	local.RemoteAddr = "127.0.0.1:1234"
	recorder = httptest.NewRecorder()
	a.LoginPage(recorder, local)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `name="action" value="bootstrap"`) || !strings.Contains(recorder.Body.String(), `name="csrf_token"`) {
		t.Fatalf("local bootstrap form is incomplete: %d %s", recorder.Code, recorder.Body.String())
	}

	values := url.Values{"action": {"bootstrap"}, "password": {testPassword}, "confirm_password": {testPassword}}
	remotePost := formRequest(a, http.MethodPost, "/login", values, "")
	remotePost.RemoteAddr = "203.0.113.10:1234"
	recorder = httptest.NewRecorder()
	a.LoginSubmit(recorder, remotePost)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("remote bootstrap POST was not rejected: %d", recorder.Code)
	}

	recorder = httptest.NewRecorder()
	a.LoginSubmit(recorder, formRequest(a, http.MethodPost, "/login", values, ""))
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("local bootstrap failed: %d %s", recorder.Code, recorder.Body.String())
	}
	if store.settings[settingPwdHash] == "" || store.users[models.LegacyAdminUserID].PasswordHash == "" {
		t.Fatal("bootstrap did not set both compatible password representations")
	}
	var sessionToken string
	for _, cookie := range recorder.Result().Cookies() {
		if cookie.Name == SessionCookieName {
			sessionToken = cookie.Value
		}
	}
	if sessionToken == "" {
		t.Fatal("bootstrap did not create a session")
	}
	recorder = httptest.NewRecorder()
	protected.ServeHTTP(recorder, authenticatedRequest(sessionToken, "/admin"))
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("bootstrap session did not authenticate: %d", recorder.Code)
	}

	store.readFailure = errors.New("database unavailable")
	recorder = httptest.NewRecorder()
	protected.ServeHTTP(recorder, authenticatedRequest(sessionToken, "/admin"))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("store failure did not fail closed: %d", recorder.Code)
	}
}

func TestLegacyAdminMigratesAndOldSessionsLoseAccess(t *testing.T) {
	store := newFakeStore()
	hash, err := hashPassword(testPassword)
	if err != nil {
		t.Fatal(err)
	}
	store.settings[settingPwdHash] = hash
	legacySession := "legacy-raw-session"
	store.sessions[legacySession] = &models.Session{
		TokenHash: legacySession, CreatedAt: time.Now().Add(-time.Hour), ExpiresAt: time.Now().Add(time.Hour),
	}
	a := testAuth(t, store, Options{})
	recorder := httptest.NewRecorder()
	principal, err := a.LoginWithCredentials(context.Background(), recorder, "admin", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	if principal.User.ID != models.LegacyAdminUserID || principal.Role() != models.RoleOwner || store.users[models.LegacyAdminUserID].PasswordHash != hash {
		t.Fatalf("legacy admin did not migrate correctly: %+v", principal)
	}
	if _, err := a.principalFromRequest(authenticatedRequest(legacySession, "/admin")); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("pre-migration legacy session remained valid: %v", err)
	}
}

func TestNamedLoginUsesSecurePerUserSessions(t *testing.T) {
	store := newFakeStore()
	user := addUser(t, store, "Editor@Example.com", testPassword, models.RoleEditor)
	a := testAuth(t, store, Options{SecureCookies: true})
	values := url.Values{"username": {"editor@example.com"}, "password": {testPassword}}
	recorder := httptest.NewRecorder()
	a.LoginSubmit(recorder, formRequest(a, http.MethodPost, "/login", values, ""))
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("named login failed: %d %s", recorder.Code, recorder.Body.String())
	}
	var cookie *http.Cookie
	for _, candidate := range recorder.Result().Cookies() {
		if candidate.Name == SessionCookieName {
			cookie = candidate
		}
	}
	if cookie == nil || !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("session cookie is not secure: %+v", cookie)
	}
	request := authenticatedRequest(cookie.Value, "/admin")
	principal, err := a.principalFromRequest(request)
	if err != nil || principal.User.ID != user.ID || principal.WorkspaceID() != models.DefaultWorkspaceID {
		t.Fatalf("named principal is incorrect: %+v %v", principal, err)
	}
	if err := a.ChangePassword(context.Background(), user.ID, testPassword, "a-new-strong-password"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.principalFromRequest(authenticatedRequest(cookie.Value, "/admin")); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("password change did not revoke existing session: %v", err)
	}
}

func TestLoginThrottlingLimitsAccountAndAddress(t *testing.T) {
	store := newFakeStore()
	addUser(t, store, "editor@example.com", testPassword, models.RoleEditor)
	a := testAuth(t, store, Options{LoginAttemptsPerMinute: 2})
	values := url.Values{"username": {"editor@example.com"}, "password": {"wrong-password"}}
	for attempt := 0; attempt < 2; attempt++ {
		recorder := httptest.NewRecorder()
		a.LoginSubmit(recorder, formRequest(a, http.MethodPost, "/login", values, ""))
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("invalid login attempt %d returned %d", attempt, recorder.Code)
		}
	}
	values.Set("password", testPassword)
	recorder := httptest.NewRecorder()
	a.LoginSubmit(recorder, formRequest(a, http.MethodPost, "/login", values, ""))
	if recorder.Code != http.StatusTooManyRequests || recorder.Header().Get("Retry-After") == "" {
		t.Fatalf("throttling did not block a subsequently valid password: %d", recorder.Code)
	}
}

func TestCSRFFormsOriginChecksAndBearerExemption(t *testing.T) {
	store := newFakeStore()
	addUser(t, store, "editor@example.com", testPassword, models.RoleEditor)
	a := testAuth(t, store, Options{})
	values := url.Values{"username": {"editor@example.com"}, "password": {testPassword}}

	withoutOrigin := formRequest(a, http.MethodPost, "/login", values, "")
	withoutOrigin.Header.Del("Origin")
	recorder := httptest.NewRecorder()
	a.LoginSubmit(recorder, withoutOrigin)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("missing Origin was accepted: %d", recorder.Code)
	}

	crossOrigin := formRequest(a, http.MethodPost, "/login", values, "")
	crossOrigin.Header.Set("Origin", "https://attacker.invalid")
	recorder = httptest.NewRecorder()
	a.LoginSubmit(recorder, crossOrigin)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("cross-origin POST was accepted: %d", recorder.Code)
	}

	withoutToken := formRequest(a, http.MethodPost, "/login", values, "")
	recorder = httptest.NewRecorder()
	a.LoginSubmit(recorder, withoutToken)
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("valid CSRF form failed: %d %s", recorder.Code, recorder.Body.String())
	}

	badToken := formRequest(a, http.MethodPost, "/login", values, "")
	recorder = httptest.NewRecorder()
	badToken.Header.Set(CSRFHeaderName, "bad")
	if err := a.validateCSRF(recorder, badToken); !errors.Is(err, ErrInvalidCSRF) {
		t.Fatalf("invalid CSRF token was accepted: %v", err)
	}

	handler := a.CSRFMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	bearer := httptest.NewRequest(http.MethodPost, "/api/flipbooks", nil)
	bearer.Header.Set("Authorization", "Bearer secret")
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, bearer)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("bearer API request was subjected to browser CSRF checks: %d", recorder.Code)
	}
}

func TestSecurityHeadersAndTrustedProxyHTTPS(t *testing.T) {
	store := newFakeStore()
	a := testAuth(t, store, Options{HSTS: true, HSTSMaxAge: time.Hour, HSTSIncludeSubdomains: true, TrustedProxies: []string{"127.0.0.1"}})
	handler := a.SecurityHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	r := httptest.NewRequest(http.MethodGet, "http://example.com/admin", nil)
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("X-Forwarded-For", "198.51.100.20")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, r)
	if recorder.Header().Get("Strict-Transport-Security") != "max-age=3600; includeSubDomains" {
		t.Fatalf("HTTPS HSTS header missing: %q", recorder.Header().Get("Strict-Transport-Security"))
	}
	if a.clientIP(r) != "198.51.100.20" {
		t.Fatalf("trusted proxy client IP was not used: %q", a.clientIP(r))
	}

	plain := testAuth(t, newFakeStore(), Options{HSTS: true, HSTSMaxAge: time.Hour})
	r = httptest.NewRequest(http.MethodGet, "http://example.com/admin", nil)
	recorder = httptest.NewRecorder()
	plain.SecurityHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {})).ServeHTTP(recorder, r)
	if recorder.Header().Get("Strict-Transport-Security") != "" {
		t.Fatal("HSTS was emitted for a plain HTTP request")
	}
}

func TestRoleAndWorkspaceMiddleware(t *testing.T) {
	store := newFakeStore()
	user := addUser(t, store, "editor@example.com", testPassword, models.RoleEditor)
	store.workspaces["other"] = &models.Workspace{ID: "other", Name: "Other", OwnerID: models.LegacyAdminUserID}
	if err := store.UpsertMembership(context.Background(), &models.Membership{WorkspaceID: "other", UserID: user.ID, Role: models.RoleAdmin}); err != nil {
		t.Fatal(err)
	}
	a := testAuth(t, store, Options{})
	token := sessionForUser(t, a, user, models.DefaultWorkspaceID)

	called := false
	handler := a.RequireAuth(a.RequireRole(models.RolePublisher)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
	})))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, authenticatedRequest(token, "/admin"))
	if recorder.Code != http.StatusForbidden || called {
		t.Fatalf("editor passed publisher middleware: %d", recorder.Code)
	}

	workspaceHandler := a.RequireWorkspaceRole("other", models.RoleAdmin)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if WorkspaceIDFromContext(r.Context()) != "other" {
			t.Fatalf("workspace context was not switched: %q", WorkspaceIDFromContext(r.Context()))
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	recorder = httptest.NewRecorder()
	workspaceHandler.ServeHTTP(recorder, authenticatedRequest(token, "/workspaces/other"))
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("valid workspace role was rejected: %d", recorder.Code)
	}

	missingHandler := a.RequireWorkspaceRole("missing", models.RoleReader)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	recorder = httptest.NewRecorder()
	missingHandler.ServeHTTP(recorder, authenticatedRequest(token, "/workspaces/missing"))
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("missing membership was not forbidden: %d", recorder.Code)
	}
}

func TestScopedAPIKeysAndAudit(t *testing.T) {
	store := newFakeStore()
	a := testAuth(t, store, Options{LegacyAPIKey: "legacy-static-key"})
	secret, key, err := a.IssueAPIKey(context.Background(), models.DefaultWorkspaceID, models.LegacyAdminUserID, "Read only", []string{models.ScopeFlipbooksRead}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if secret == "" || key.KeyHash == secret || key.Prefix == secret {
		t.Fatal("raw API key was persisted")
	}
	readHandler := a.RequireAPIKey(models.ScopeFlipbooksRead)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal, ok := PrincipalFromContext(r.Context())
		if !ok || principal.APIKey.ID != key.ID || principal.WorkspaceID() != models.DefaultWorkspaceID {
			t.Fatalf("API key principal missing: %+v", principal)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	r := httptest.NewRequest(http.MethodGet, "/api/flipbooks", nil)
	r.Header.Set("Authorization", "Bearer "+secret)
	recorder := httptest.NewRecorder()
	readHandler.ServeHTTP(recorder, r)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("valid scoped key was rejected: %d %s", recorder.Code, recorder.Body.String())
	}
	anyKeyHandler := a.RequireAPIKey("")(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	r = httptest.NewRequest(http.MethodGet, "/api/flipbooks", nil)
	r.Header.Set("Authorization", "Bearer "+secret)
	recorder = httptest.NewRecorder()
	anyKeyHandler.ServeHTTP(recorder, r)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("authentication-only API key middleware rejected a valid key: %d", recorder.Code)
	}

	writeHandler := a.RequireAPIKey(models.ScopeFlipbooksWrite)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	r = httptest.NewRequest(http.MethodPost, "/api/flipbooks", nil)
	r.Header.Set("Authorization", "Bearer "+secret)
	recorder = httptest.NewRecorder()
	writeHandler.ServeHTTP(recorder, r)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("read-only key gained write scope: %d", recorder.Code)
	}

	if err := a.RevokeAPIKey(context.Background(), models.DefaultWorkspaceID, models.LegacyAdminUserID, key.ID); err != nil {
		t.Fatal(err)
	}
	r = httptest.NewRequest(http.MethodGet, "/api/flipbooks", nil)
	r.Header.Set("Authorization", "Bearer "+secret)
	recorder = httptest.NewRecorder()
	readHandler.ServeHTTP(recorder, r)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("revoked key remained valid: %d", recorder.Code)
	}

	r = httptest.NewRequest(http.MethodGet, "/api/flipbooks", nil)
	r.Header.Set("Authorization", "Bearer legacy-static-key")
	legacyHandler := a.RequireAPIKey(models.ScopeWorkspaceAdmin)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	recorder = httptest.NewRecorder()
	legacyHandler.ServeHTTP(recorder, r)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("legacy API key adapter failed: %d", recorder.Code)
	}

	if len(store.audits) == 0 {
		t.Fatal("security operations did not emit audit events")
	}
	auditText := fmt.Sprintf("%+v", store.audits)
	if strings.Contains(auditText, secret) || strings.Contains(auditText, "legacy-static-key") {
		t.Fatal("audit history contains raw credentials")
	}
}

func TestInvitationAndPasswordResetFlows(t *testing.T) {
	store := newFakeStore()
	a := testAuth(t, store, Options{})
	token, invitation, err := a.CreateInvitation(context.Background(), models.DefaultWorkspaceID, models.LegacyAdminUserID, "New.User@Example.com", models.RoleEditor, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if invitation.TokenHash == token || invitation.NormalizedEmail != "new.user@example.com" {
		t.Fatal("invitation token or email normalization is incorrect")
	}
	recorder := httptest.NewRecorder()
	a.InvitationHandler(recorder, httptest.NewRequest(http.MethodGet, "/invitation?token="+url.QueryEscape(token), nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "new.user@example.com") {
		t.Fatalf("invitation form failed: %d %s", recorder.Code, recorder.Body.String())
	}

	values := url.Values{"token": {token}, "name": {"New User"}, "password": {testPassword}, "confirm_password": {testPassword}}
	recorder = httptest.NewRecorder()
	a.InvitationHandler(recorder, formRequest(a, http.MethodPost, "/invitation", values, ""))
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("invitation acceptance failed: %d %s", recorder.Code, recorder.Body.String())
	}
	user, err := store.GetUserByEmail(context.Background(), "new.user@example.com")
	if err != nil {
		t.Fatal(err)
	}
	membership, err := store.GetMembership(context.Background(), models.DefaultWorkspaceID, user.ID)
	if err != nil || membership.Role != models.RoleEditor {
		t.Fatalf("invited membership missing: %+v %v", membership, err)
	}
	recorder = httptest.NewRecorder()
	a.InvitationHandler(recorder, httptest.NewRequest(http.MethodGet, "/invitation?token="+url.QueryEscape(token), nil))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("invitation replay was accepted: %d", recorder.Code)
	}

	sessionToken := sessionForUser(t, a, user, models.DefaultWorkspaceID)
	resetToken, err := a.RequestPasswordReset(context.Background(), user.Email, time.Hour)
	if err != nil || resetToken == "" {
		t.Fatalf("reset token was not issued: %v", err)
	}
	unknownToken, err := a.RequestPasswordReset(context.Background(), "unknown@example.com", time.Hour)
	if err != nil || unknownToken != "" {
		t.Fatalf("unknown account reset was enumerable: %q %v", unknownToken, err)
	}
	if _, err := a.ResetPassword(context.Background(), resetToken+"tampered", "another-strong-password"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("tampered reset token was accepted: %v", err)
	}
	if _, err := a.ResetPassword(context.Background(), resetToken, "another-strong-password"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.principalFromRequest(authenticatedRequest(sessionToken, "/admin")); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("password reset did not revoke sessions: %v", err)
	}
	if !passwordMatches(store.users[user.ID].PasswordHash, "another-strong-password") {
		t.Fatal("new password was not stored")
	}
	if _, err := a.ResetPassword(context.Background(), resetToken, "third-strong-password"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("used reset token remained valid: %v", err)
	}
}

func TestPasswordResetExpiry(t *testing.T) {
	store := newFakeStore()
	user := addUser(t, store, "editor@example.com", testPassword, models.RoleEditor)
	a := testAuth(t, store, Options{})
	token, err := a.RequestPasswordReset(context.Background(), user.Email, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	a.now = func() time.Time { return time.Now().Add(2 * time.Second).UTC() }
	if _, err := a.ResetPassword(context.Background(), token, "another-strong-password"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expired reset token was accepted: %v", err)
	}
}
