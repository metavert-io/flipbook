package auth

import (
	"bytes"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/metavert-io/flipbook/internal/database"
	"github.com/metavert-io/flipbook/internal/models"
)

func (a *Auth) renderLogin(w http.ResponseWriter, r *http.Request, status int, data LoginData) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if a.tmpl == nil {
		http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
		return
	}
	var output bytes.Buffer
	if err := a.tmpl.ExecuteTemplate(&output, "login", data); err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(status)
	_, _ = w.Write(output.Bytes())
}

func safeReturnTo(value string) string {
	if value == "" || strings.Contains(value, "\\") || strings.ContainsAny(value, "\r\n\x00") {
		return "/admin"
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.IsAbs() || parsed.Host != "" || !strings.HasPrefix(parsed.Path, "/") || strings.HasPrefix(value, "//") {
		return "/admin"
	}
	return value
}

func directLoopback(r *http.Request) bool {
	for _, header := range []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Proto", "X-Real-IP"} {
		if r.Header.Get(header) != "" {
			return false
		}
	}
	ip := directRemoteIP(r)
	return ip != nil && ip.IsLoopback()
}

// formSecurity checks origin and CSRF for the auth forms. Those forms are
// reachable without a session and a valid CSRF token is free to obtain from
// GET /login, so the body is capped before anything reads it and parsed in
// full here: the handlers' FormValue calls would otherwise spool multipart
// file parts of any size to disk.
func (a *Auth) formSecurity(w http.ResponseWriter, r *http.Request) (string, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxAuthFormBytes)
	if err := a.validateOrigin(r); err != nil {
		return "", err
	}
	token, err := a.ensureCSRF(w, r)
	if err != nil {
		return "", err
	}
	if err := a.validateCSRF(w, r); err != nil {
		return "", err
	}
	if err := parseAuthForm(r); err != nil {
		return "", err
	}
	return token, nil
}

var errMalformedForm = errors.New("malformed form body")

func parseAuthForm(r *http.Request) error {
	err := r.ParseForm()
	if err == nil {
		err = r.ParseMultipartForm(maxAuthFormBytes)
		if errors.Is(err, http.ErrNotMultipart) {
			err = nil
		}
	}
	switch {
	case err == nil:
		return nil
	case bodyTooLarge(err):
		return ErrRequestTooLarge
	default:
		return errMalformedForm
	}
}

func formSecurityError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrInvalidOrigin) || errors.Is(err, ErrInvalidCSRF) {
		http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
		return
	}
	if errors.Is(err, ErrRequestTooLarge) {
		http.Error(w, http.StatusText(http.StatusRequestEntityTooLarge), http.StatusRequestEntityTooLarge)
		return
	}
	if errors.Is(err, errMalformedForm) {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
}

func (a *Auth) baseLoginData(w http.ResponseWriter, r *http.Request) (LoginData, error) {
	token, err := a.ensureCSRF(w, r)
	if err != nil {
		return LoginData{}, err
	}
	return LoginData{
		CSRFToken: token, ReturnTo: safeReturnTo(r.URL.Query().Get("return")),
		Mode: "login", OIDCEnabled: a.opts.OIDC.Configured(), OIDCURL: "/oidc/login",
	}, nil
}

func loginMessage(code string) string {
	switch code {
	case "password-reset":
		return "Your password has been reset. Sign in with your new password."
	case "password-changed":
		return "Your password has been changed. Sign in again."
	case "invitation-accepted":
		return "The invitation was accepted."
	case "logged-out":
		return "You have been signed out."
	default:
		return ""
	}
}

func (a *Auth) LoginPage(w http.ResponseWriter, r *http.Request) {
	if a == nil || a.initErr != nil {
		http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
		return
	}
	if _, err := a.principalFromRequest(r); err == nil {
		http.Redirect(w, r, "/admin", http.StatusFound)
		return
	}
	data, err := a.baseLoginData(w, r)
	if err != nil {
		http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
		return
	}
	configured, err := a.localPasswordStatus()
	if err != nil {
		http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
		return
	}
	data.Notice = loginMessage(r.URL.Query().Get("notice"))
	if !configured {
		data.Bootstrap = directLoopback(r)
		data.SetupRequired = !data.Bootstrap && !data.OIDCEnabled
	}
	if data.Bootstrap {
		data.Mode = "bootstrap"
	}
	status := http.StatusOK
	if data.SetupRequired {
		status = http.StatusServiceUnavailable
		data.Error = "Local authentication is not configured. Run 'flipbook set-password' on the server."
	}
	a.renderLogin(w, r, status, data)
}

func (a *Auth) LoginSubmit(w http.ResponseWriter, r *http.Request) {
	token, err := a.formSecurity(w, r)
	if err != nil {
		formSecurityError(w, err)
		return
	}
	data := LoginData{
		CSRFToken: token, Mode: "login", OIDCEnabled: a.opts.OIDC.Configured(), OIDCURL: "/oidc/login",
		ReturnTo: safeReturnTo(r.FormValue("return")), Username: strings.TrimSpace(r.FormValue("username")),
	}
	if r.FormValue("action") == "bootstrap" {
		a.bootstrapSubmit(w, r, data)
		return
	}
	identifier := strings.ToLower(strings.TrimSpace(data.Username))
	throttleIdentifier := identifier
	if throttleIdentifier == "" {
		throttleIdentifier = "admin"
	}
	allowed, retry := a.throttleAllow("ip:"+a.clientIP(r), "account:"+throttleIdentifier)
	if !allowed {
		seconds := int(retry / time.Second)
		if seconds < 1 {
			seconds = 1
		}
		w.Header().Set("Retry-After", strconv.Itoa(seconds))
		data.Error = "Too many sign-in attempts. Try again later."
		a.audit(r.Context(), models.DefaultWorkspaceID, "", "", "auth.login", "session", "", "throttled", nil)
		a.renderLogin(w, r, http.StatusTooManyRequests, data)
		return
	}
	principal, err := a.loginWithCredentials(r.Context(), w, identifier, r.FormValue("password"))
	if err != nil {
		switch {
		case errors.Is(err, errAuthenticationFailed):
			data.Error = "Invalid email/username or password."
			a.audit(r.Context(), models.DefaultWorkspaceID, "", "", "auth.login", "session", "", "failure", map[string]string{"method": "password"})
			a.renderLogin(w, r, http.StatusUnauthorized, data)
		case errors.Is(err, ErrNoWorkspace), errors.Is(err, ErrForbidden):
			data.Error = "Your account does not have access to a workspace."
			a.renderLogin(w, r, http.StatusForbidden, data)
		default:
			http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
		}
		return
	}
	if principal != nil {
		http.Redirect(w, r, safeReturnTo(data.ReturnTo), http.StatusSeeOther)
		return
	}
	http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
}

func (a *Auth) bootstrapSubmit(w http.ResponseWriter, r *http.Request, data LoginData) {
	data.Mode = "bootstrap"
	data.Bootstrap = true
	if !directLoopback(r) {
		http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
		return
	}
	a.bootstrapMu.Lock()
	defer a.bootstrapMu.Unlock()
	configured, err := a.localPasswordStatus()
	if err != nil {
		http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
		return
	}
	if configured {
		http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
		return
	}
	password := r.FormValue("password")
	if password != r.FormValue("confirm_password") {
		data.Error = "Passwords do not match."
		a.renderLogin(w, r, http.StatusBadRequest, data)
		return
	}
	if err := a.SetPassword(password); err != nil {
		if errors.Is(err, ErrInvalidPassword) {
			data.Error = "Use a password between 12 and 72 bytes."
			a.renderLogin(w, r, http.StatusBadRequest, data)
			return
		}
		http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
		return
	}
	if _, err := a.loginWithCredentials(r.Context(), w, "admin", password); err != nil {
		http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (a *Auth) LogoutHandler(w http.ResponseWriter, r *http.Request) {
	if _, err := a.formSecurity(w, r); err != nil {
		formSecurityError(w, err)
		return
	}
	if cookie, err := r.Cookie(SessionCookieName); err == nil && cookie.Value != "" {
		if principal, principalErr := a.principalFromRequest(r); principalErr == nil {
			a.audit(r.Context(), principal.WorkspaceID(), principal.User.ID, "", "auth.logout", "session", models.HashToken(cookie.Value), "success", nil)
		}
	}
	a.Logout(w, r)
	http.Redirect(w, r, "/login?notice=logged-out", http.StatusSeeOther)
}

func requestToken(r *http.Request) string {
	if token := chi.URLParam(r, "token"); token != "" {
		return token
	}
	if r.Method == http.MethodGet {
		return r.URL.Query().Get("token")
	}
	return r.FormValue("token")
}

func (a *Auth) InvitationHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}
	data := LoginData{Mode: "invitation", Token: requestToken(r), OIDCEnabled: a.opts.OIDC.Configured(), OIDCURL: "/oidc/login"}
	if r.Method == http.MethodPost {
		token, err := a.formSecurity(w, r)
		if err != nil {
			formSecurityError(w, err)
			return
		}
		data.CSRFToken = token
		data.Token = requestToken(r)
	} else {
		token, err := a.ensureCSRF(w, r)
		if err != nil {
			http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
			return
		}
		data.CSRFToken = token
	}
	invitation, err := a.store.GetInvitationByToken(r.Context(), data.Token)
	if err != nil {
		if !errors.Is(err, database.ErrNotFound) {
			http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
			return
		}
		data.Error = "This invitation is invalid, expired, or has already been used."
		data.Token = ""
		a.renderLogin(w, r, http.StatusBadRequest, data)
		return
	}
	data.Email = invitation.Email
	principal, principalErr := a.principalFromRequest(r)
	if principalErr != nil && !errors.Is(principalErr, ErrUnauthorized) {
		roleError(w, principalErr)
		return
	}
	if principalErr == nil {
		data.Name = principal.User.Name
	} else {
		user, err := a.store.GetUserByEmail(r.Context(), invitation.Email)
		if err == nil {
			data.Name = user.Name
		} else if errors.Is(err, database.ErrNotFound) {
			data.NewUser = true
		} else {
			http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
			return
		}
	}
	if r.Method == http.MethodGet {
		a.renderLogin(w, r, http.StatusOK, data)
		return
	}

	user := (*models.User)(nil)
	if principalErr == nil {
		user = principal.User
	} else if !data.NewUser {
		user, err = a.namedPasswordUser(r.Context(), invitation.Email, r.FormValue("password"))
		if err != nil {
			data.Error = "Enter the current password for the invited account."
			a.renderLogin(w, r, http.StatusUnauthorized, data)
			return
		}
	} else {
		password := r.FormValue("password")
		if password != r.FormValue("confirm_password") {
			data.Error = "Passwords do not match."
			a.renderLogin(w, r, http.StatusBadRequest, data)
			return
		}
		hash, hashErr := hashPassword(password)
		if hashErr != nil {
			data.Error = "Use a password between 12 and 72 bytes."
			a.renderLogin(w, r, http.StatusBadRequest, data)
			return
		}
		name := strings.TrimSpace(r.FormValue("name"))
		if name == "" {
			name = strings.Split(invitation.Email, "@")[0]
		}
		user = &models.User{Email: invitation.Email, Name: name, PasswordHash: hash, Locale: "en"}
		if err := a.store.CreateUser(r.Context(), user); err != nil {
			data.Error = "The account could not be created. Sign in to the existing account or request a new invitation."
			a.renderLogin(w, r, http.StatusConflict, data)
			return
		}
		data.Name = user.Name
	}
	accepted, err := a.AcceptInvitation(r.Context(), data.Token, user.ID)
	if err != nil {
		if !errors.Is(err, ErrInvalidToken) && !errors.Is(err, database.ErrConflict) && !errors.Is(err, ErrForbidden) {
			http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
			return
		}
		data.Error = "The invitation could not be accepted for this account."
		a.renderLogin(w, r, http.StatusForbidden, data)
		return
	}
	if _, err := a.createSessionForUser(r.Context(), w, user, accepted.WorkspaceID); err != nil {
		http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (a *Auth) PasswordResetRequestHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}
	data := LoginData{Mode: "forgot", OIDCEnabled: a.opts.OIDC.Configured(), OIDCURL: "/oidc/login"}
	if r.Method == http.MethodGet {
		token, err := a.ensureCSRF(w, r)
		if err != nil {
			http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
			return
		}
		data.CSRFToken = token
		a.renderLogin(w, r, http.StatusOK, data)
		return
	}
	token, err := a.formSecurity(w, r)
	if err != nil {
		formSecurityError(w, err)
		return
	}
	data.CSRFToken = token
	data.Email = strings.TrimSpace(r.FormValue("email"))
	if a.opts.PasswordResetDelivery == nil {
		http.Error(w, "Password reset delivery is not configured.", http.StatusServiceUnavailable)
		return
	}
	resetToken, err := a.RequestPasswordReset(r.Context(), data.Email, 0)
	if err != nil {
		http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
		return
	}
	if resetToken != "" {
		outcome := "success"
		if err := a.opts.PasswordResetDelivery(r.Context(), data.Email, resetToken); err != nil {
			outcome = "failure"
		}
		a.audit(r.Context(), models.DefaultWorkspaceID, "", "", "auth.password.reset_request", "password_reset", "", outcome, nil)
	}
	data.Notice = "If the account exists, password reset instructions have been sent."
	data.Email = ""
	a.renderLogin(w, r, http.StatusOK, data)
}

func (a *Auth) PasswordResetHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}
	data := LoginData{Mode: "reset", Token: requestToken(r), OIDCEnabled: a.opts.OIDC.Configured(), OIDCURL: "/oidc/login"}
	if r.Method == http.MethodGet {
		token, err := a.ensureCSRF(w, r)
		if err != nil {
			http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
			return
		}
		data.CSRFToken = token
		if _, err := a.parsePasswordReset(r.Context(), data.Token); err != nil {
			data.Error = "This reset link is invalid or expired."
			data.Token = ""
			a.renderLogin(w, r, http.StatusBadRequest, data)
			return
		}
		a.renderLogin(w, r, http.StatusOK, data)
		return
	}
	token, err := a.formSecurity(w, r)
	if err != nil {
		formSecurityError(w, err)
		return
	}
	data.CSRFToken = token
	data.Token = requestToken(r)
	if r.FormValue("password") != r.FormValue("confirm_password") {
		data.Error = "Passwords do not match."
		a.renderLogin(w, r, http.StatusBadRequest, data)
		return
	}
	if _, err := a.ResetPassword(r.Context(), data.Token, r.FormValue("password")); err != nil {
		if errors.Is(err, ErrInvalidPassword) {
			data.Error = "Use a password between 12 and 72 bytes."
		} else {
			data.Error = "This reset link is invalid or expired."
			data.Token = ""
		}
		a.renderLogin(w, r, http.StatusBadRequest, data)
		return
	}
	a.clearSessionCookie(w)
	http.Redirect(w, r, "/login?notice=password-reset", http.StatusSeeOther)
}

func (a *Auth) ChangePasswordHandler(w http.ResponseWriter, r *http.Request) {
	principal, err := a.principalFromRequest(r)
	if err != nil {
		http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
		return
	}
	data := LoginData{Mode: "change", Name: principal.User.Name}
	if r.Method == http.MethodGet {
		token, err := a.ensureCSRF(w, r)
		if err != nil {
			http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
			return
		}
		data.CSRFToken = token
		a.renderLogin(w, r, http.StatusOK, data)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}
	token, err := a.formSecurity(w, r)
	if err != nil {
		formSecurityError(w, err)
		return
	}
	data.CSRFToken = token
	if r.FormValue("password") != r.FormValue("confirm_password") {
		data.Error = "Passwords do not match."
		a.renderLogin(w, r, http.StatusBadRequest, data)
		return
	}
	if err := a.ChangePassword(r.Context(), principal.User.ID, r.FormValue("current_password"), r.FormValue("password")); err != nil {
		if errors.Is(err, errAuthenticationFailed) {
			data.Error = "The current password is incorrect."
		} else if errors.Is(err, ErrInvalidPassword) {
			data.Error = "Use a password between 12 and 72 bytes."
		} else {
			http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
			return
		}
		a.renderLogin(w, r, http.StatusBadRequest, data)
		return
	}
	a.clearSessionCookie(w)
	http.Redirect(w, r, "/login?notice=password-changed", http.StatusSeeOther)
}
