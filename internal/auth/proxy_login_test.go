package auth

import (
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/metavert-io/flipbook/internal/config"
	"github.com/metavert-io/flipbook/internal/models"
)

// proxiedLoginRequest mirrors production: TLS terminates at the reverse proxy,
// which forwards plain HTTP from its container IP with the browser's https Origin.
func proxiedLoginRequest(a *Auth, host string, values url.Values) *http.Request {
	csrf := testCSRFCookie()
	values.Set(CSRFFormField, a.csrfToken(csrf, ""))
	r := httptest.NewRequest(http.MethodPost, "http://"+host+"/login", strings.NewReader(values.Encode()))
	r.RemoteAddr = "172.18.0.14:47062"
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "https://"+host)
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("X-Forwarded-For", "198.51.100.20")
	r.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: csrf})
	return r
}

func TestLoginBehindTLSProxyOnEveryConfiguredDomain(t *testing.T) {
	values := func() url.Values {
		return url.Values{"username": {"editor@example.com"}, "password": {testPassword}}
	}
	cases := map[string]Options{
		"https mode":    {HTTPSMode: true, BaseURL: "https://flipbook.example.com"},
		"trusted proxy": {BaseURL: "https://flipbook.example.com", TrustedProxies: []string{"172.18.0.0/16"}},
	}
	for name, opts := range cases {
		t.Run(name, func(t *testing.T) {
			store := newFakeStore()
			addUser(t, store, "editor@example.com", testPassword, models.RoleEditor)
			a := testAuth(t, store, opts)
			for _, host := range []string{"flipbook.example.com", "books.example.org"} {
				recorder := httptest.NewRecorder()
				a.LoginSubmit(recorder, proxiedLoginRequest(a, host, values()))
				if recorder.Code != http.StatusSeeOther {
					t.Fatalf("%s: login behind TLS proxy failed: %d %s", host, recorder.Code, recorder.Body.String())
				}
			}
		})
	}

	// Without HTTPS mode or a trusted proxy the app cannot know the browser
	// used https, so the Origin check must keep rejecting the request.
	store := newFakeStore()
	addUser(t, store, "editor@example.com", testPassword, models.RoleEditor)
	plain := testAuth(t, store, Options{})
	recorder := httptest.NewRecorder()
	plain.LoginSubmit(recorder, proxiedLoginRequest(plain, "books.example.org", values()))
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("untrusted proxied https Origin was accepted: %d", recorder.Code)
	}
}

func TestNewWithConfigCarriesSecuritySettings(t *testing.T) {
	cfg := &config.Config{
		SessionSecret: strings.Repeat("test-secret-", 8),
		BaseURL:       "https://books.example.org",
		Security: config.SecurityConfig{
			HTTPSMode: true, SecureCookies: true, HSTS: true,
			TrustedProxies: []string{"172.18.0.0/16", "10.0.0.0/8"},
		},
	}
	a, err := NewWithConfig(nil, cfg, template.New(""))
	if err != nil {
		t.Fatal(err)
	}
	if !a.opts.HTTPSMode || !a.opts.SecureCookies || !a.opts.HSTS || a.opts.BaseURL != cfg.BaseURL {
		t.Fatalf("security options were not carried over: %+v", a.opts)
	}
	if len(a.trustedNets) != 2 {
		t.Fatalf("trusted proxies were not parsed: %v", a.trustedNets)
	}
}
