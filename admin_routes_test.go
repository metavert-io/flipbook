package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/metavert-io/flipbook/internal/auth"
	"github.com/metavert-io/flipbook/internal/database"
	"github.com/metavert-io/flipbook/internal/handlers"
	"github.com/metavert-io/flipbook/internal/models"
)

type adminRouteAuthStore struct {
	auth.Store
	session    *models.Session
	user       *models.User
	membership *models.Membership
}

func (s *adminRouteAuthStore) GetSessionByToken(_ context.Context, token string) (*models.Session, error) {
	if models.HashToken(token) != s.session.TokenHash {
		return nil, database.ErrNotFound
	}
	return s.session, nil
}

func (s *adminRouteAuthStore) GetUser(_ context.Context, id string) (*models.User, error) {
	if id != s.user.ID {
		return nil, database.ErrNotFound
	}
	return s.user, nil
}

func (s *adminRouteAuthStore) GetMembership(_ context.Context, workspaceID, userID string) (*models.Membership, error) {
	if workspaceID != s.membership.WorkspaceID || userID != s.membership.UserID {
		return nil, database.ErrNotFound
	}
	return s.membership, nil
}

func (s *adminRouteAuthStore) TouchSession(context.Context, string) error { return nil }

func (s *adminRouteAuthStore) RecordAuditEvent(context.Context, *models.AuditEvent) error {
	return nil
}

// adminRouter mounts the real admin routes behind a signed-in owner. The
// handler has no database, so any request that gets past the middleware to a
// handler touching storage panics and fails the test.
func adminRouter(t *testing.T) (chi.Router, func(*http.Request), string) {
	t.Helper()
	now := time.Now().UTC()
	rawSession := "admin-route-session"
	store := &adminRouteAuthStore{
		session: &models.Session{
			TokenHash: models.HashToken(rawSession), UserID: "owner", WorkspaceID: models.DefaultWorkspaceID,
			CreatedAt: now, ExpiresAt: now.Add(time.Hour), LastSeenAt: &now,
		},
		user:       &models.User{ID: "owner", Email: "owner@example.com", Name: "Owner"},
		membership: &models.Membership{ID: "membership", WorkspaceID: models.DefaultWorkspaceID, UserID: "owner", Role: models.RoleOwner},
	}
	a, err := auth.NewWithOptions(store, nil, auth.Options{SessionSecret: strings.Repeat("route-test-secret-", 4)})
	if err != nil {
		t.Fatal(err)
	}
	router := chi.NewRouter()
	mountAdminRoutes(router, a, handlers.NewAdminHandlerWithOptions(nil, nil, nil, nil, "http://example.com", handlers.AdminOptions{Auth: a}))

	csrfCookie := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	signIn := func(r *http.Request) {
		r.Header.Set("Origin", "http://"+r.Host)
		r.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: rawSession})
		r.AddCookie(&http.Cookie{Name: auth.CSRFCookieName, Value: csrfCookie})
	}
	probe := httptest.NewRequest(http.MethodGet, "/admin", nil)
	signIn(probe)
	token, err := a.CSRFToken(httptest.NewRecorder(), probe)
	if err != nil {
		t.Fatal(err)
	}
	return router, signIn, token
}

// serve reports a handler panic as status 599 so a request that slips past
// the middleware fails the assertion instead of crashing the test binary.
func serve(router http.Handler, r *http.Request) (recorder *httptest.ResponseRecorder) {
	recorder = httptest.NewRecorder()
	defer func() {
		if recover() != nil {
			recorder.Code = 599
		}
	}()
	router.ServeHTTP(recorder, r)
	return recorder
}

func TestAdminMutationsRejectRequestsWithoutCSRFToken(t *testing.T) {
	router, signIn, _ := adminRouter(t)
	placeholders := regexp.MustCompile(`\{[^}]+\}`)
	checked := map[string]bool{}
	err := chi.Walk(router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions {
			return nil
		}
		target := placeholders.ReplaceAllString(route, "x")

		urlencoded := httptest.NewRequest(method, target, strings.NewReader("title=forged&role=owner"))
		urlencoded.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		signIn(urlencoded)
		recorder := serve(router, urlencoded)
		if recorder.Code != http.StatusForbidden {
			t.Errorf("%s %s without CSRF token: status %d, want 403", method, route, recorder.Code)
		}

		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		_ = writer.WriteField("title", "forged")
		_ = writer.Close()
		multipartRequest := httptest.NewRequest(method, target, &body)
		multipartRequest.Header.Set("Content-Type", writer.FormDataContentType())
		signIn(multipartRequest)
		recorder = serve(router, multipartRequest)
		if recorder.Code != http.StatusForbidden {
			t.Errorf("%s %s multipart without CSRF token: status %d, want 403", method, route, recorder.Code)
		}
		checked[method+" "+route] = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range []string{
		"POST /admin/upload", "POST /admin/import", "POST /admin/bulk",
		"POST /admin/flipbooks/{id}/delete", "POST /admin/flipbooks/{id}/replace", "POST /admin/flipbooks/{id}/publish",
		"POST /admin/flipbooks/{id}/share-tokens", "POST /admin/team/members/{userID}/role",
		"POST /admin/security/api-keys", "POST /admin/settings/admins",
	} {
		if !checked[route] {
			t.Errorf("%s is not mounted by mountAdminRoutes", route)
		}
	}
}

func TestAdminUploadAcceptsCSRFTokenAndKeepsBody(t *testing.T) {
	router, signIn, token := adminRouter(t)
	for name, inHeader := range map[string]bool{"header token": true, "form token": false} {
		t.Run(name, func(t *testing.T) {
			var body bytes.Buffer
			writer := multipart.NewWriter(&body)
			if !inHeader {
				if err := writer.WriteField(auth.CSRFFormField, token); err != nil {
					t.Fatal(err)
				}
			}
			if err := writer.WriteField("title", "Quarterly report"); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, "/admin/upload", &body)
			request.Header.Set("Content-Type", writer.FormDataContentType())
			request.Header.Set("X-Requested-With", "XMLHttpRequest")
			if inHeader {
				request.Header.Set(auth.CSRFHeaderName, token)
			}
			signIn(request)
			recorder := serve(router, request)
			// The handler's own validation answering proves the request passed
			// CSRF and that it parsed an intact body: a body consumed by the
			// middleware would fail with "could not be parsed" instead.
			if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "Choose at least one document") {
				t.Fatalf("status %d: %s", recorder.Code, recorder.Body.String())
			}
		})
	}
}
