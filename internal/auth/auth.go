package auth

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/metavert-io/flipbook/internal/database"
	"github.com/metavert-io/flipbook/internal/models"
	"golang.org/x/crypto/bcrypt"
)

var dummyPasswordHash = func() string {
	hash, _ := bcrypt.GenerateFromPassword([]byte("dummy-password-for-timing"), bcryptCost)
	return string(hash)
}()

func (a *Auth) localPasswordStatus() (bool, error) {
	if a == nil || a.initErr != nil {
		return false, ErrNotConfigured
	}
	legacyHash, err := a.store.GetSetting(settingPwdHash)
	if err == nil && legacyHash != "" {
		return true, nil
	}
	if err != nil && !errors.Is(err, database.ErrNotFound) {
		return false, err
	}
	user, err := a.store.GetUser(context.Background(), models.LegacyAdminUserID)
	if errors.Is(err, database.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return user.PasswordHash != "", nil
}

// HasPassword retains the legacy local-admin password check. Database failures
// are reported as false; HTTP authorization never uses this to grant access.
func (a *Auth) HasPassword() bool {
	configured, err := a.localPasswordStatus()
	return err == nil && configured
}

// SetPassword updates both the named legacy owner and the legacy setting so
// existing CLI and integrations continue to observe the same credential.
func (a *Auth) SetPassword(password string) error {
	if a == nil || a.initErr != nil {
		return ErrNotConfigured
	}
	hash, err := hashPassword(password)
	if err != nil {
		return err
	}
	ctx := context.Background()
	user, err := a.store.GetUser(ctx, models.LegacyAdminUserID)
	if err != nil {
		return err
	}
	if err := a.store.SetSetting(settingPwdHash, hash); err != nil {
		return err
	}
	if err := a.store.SetUserPassword(ctx, user.ID, hash); err != nil {
		return err
	}
	a.audit(ctx, models.DefaultWorkspaceID, user.ID, "", "auth.password.set", "user", user.ID, "success", nil)
	return nil
}

// CheckPassword retains the legacy password-only verification method.
func (a *Auth) CheckPassword(password string) bool {
	if a == nil || a.initErr != nil {
		return false
	}
	_, err := a.legacyPasswordUser(context.Background(), password, false)
	return err == nil
}

func (a *Auth) legacyPasswordUser(ctx context.Context, password string, migrate bool) (*models.User, error) {
	user, err := a.store.GetUser(ctx, models.LegacyAdminUserID)
	if errors.Is(err, database.ErrNotFound) {
		passwordMatches(dummyPasswordHash, password)
		return nil, errAuthenticationFailed
	}
	if err != nil {
		return nil, err
	}
	if !user.Active() {
		return nil, errAuthenticationFailed
	}
	legacyHash, settingErr := a.store.GetSetting(settingPwdHash)
	if settingErr != nil && !errors.Is(settingErr, database.ErrNotFound) {
		return nil, settingErr
	}
	if settingErr == nil && legacyHash != "" {
		if !passwordMatches(legacyHash, password) {
			return nil, errAuthenticationFailed
		}
		if migrate && user.PasswordHash != legacyHash {
			if err := a.store.SetUserPassword(ctx, user.ID, legacyHash); err != nil {
				return nil, err
			}
			user.PasswordHash = legacyHash
		}
		return user, nil
	}
	if !passwordMatches(user.PasswordHash, password) {
		return nil, errAuthenticationFailed
	}
	return user, nil
}

func (a *Auth) namedPasswordUser(ctx context.Context, identifier, password string) (*models.User, error) {
	user, err := a.store.GetUserByEmail(ctx, identifier)
	if errors.Is(err, database.ErrNotFound) {
		passwordMatches(dummyPasswordHash, password)
		return nil, errAuthenticationFailed
	}
	if err != nil {
		return nil, err
	}
	if !user.Active() || !passwordMatches(user.PasswordHash, password) {
		return nil, errAuthenticationFailed
	}
	return user, nil
}

// Login retains the original password-only adapter. New HTTP code should use
// LoginWithCredentials, which also supports named users and throttling.
func (a *Auth) Login(w http.ResponseWriter, password string) bool {
	if a == nil || a.initErr != nil {
		return false
	}
	_, err := a.loginWithCredentials(context.Background(), w, "", password)
	return err == nil
}

func (a *Auth) LoginWithCredentials(ctx context.Context, w http.ResponseWriter, identifier, password string) (*Principal, error) {
	if a == nil || a.initErr != nil {
		return nil, ErrNotConfigured
	}
	return a.loginWithCredentials(ctx, w, identifier, password)
}

func (a *Auth) loginWithCredentials(ctx context.Context, w http.ResponseWriter, identifier, password string) (*Principal, error) {
	identifier = strings.ToLower(strings.TrimSpace(identifier))
	var user *models.User
	var err error
	if identifier == "" || identifier == "admin" || identifier == models.LegacyAdminUserID {
		user, err = a.legacyPasswordUser(ctx, password, true)
	} else {
		user, err = a.namedPasswordUser(ctx, identifier, password)
	}
	if err != nil {
		return nil, err
	}
	principal, err := a.createSessionForUser(ctx, w, user, "")
	if err != nil {
		return nil, err
	}
	a.audit(ctx, principal.WorkspaceID(), user.ID, "", "auth.login", "session", "", "success", map[string]string{"method": "password"})
	return principal, nil
}

// sessionTTL returns the configured session lifetime, overridden by the
// runtime SettingSessionTTLSeconds value stored by the admin panel. Values
// outside the supported window are ignored.
func (a *Auth) sessionTTL() time.Duration {
	ttl := a.opts.SessionTTL
	if a.store != nil {
		value, err := a.store.GetSetting(SettingSessionTTLSeconds)
		if err == nil {
			seconds, parseErr := strconv.Atoi(strings.TrimSpace(value))
			if parseErr == nil && seconds >= minSessionTTLSeconds && seconds <= maxSessionTTLSeconds {
				ttl = time.Duration(seconds) * time.Second
			}
		}
	}
	return ttl
}

// throttleAllow applies the login throttle, first syncing its rate limit from
// the runtime SettingLoginAttemptsPerMinute value stored by the admin panel.
func (a *Auth) throttleAllow(keys ...string) (bool, time.Duration) {
	if a.throttle == nil {
		return false, time.Minute
	}
	if a.store != nil {
		value, err := a.store.GetSetting(SettingLoginAttemptsPerMinute)
		if err == nil {
			limit, parseErr := strconv.Atoi(strings.TrimSpace(value))
			if parseErr == nil && limit >= 1 && limit <= maxLoginAttemptLimit {
				a.throttle.setLimit(limit)
			}
		}
	}
	return a.throttle.allow(keys...)
}

func (a *Auth) createSessionForUser(ctx context.Context, w http.ResponseWriter, user *models.User, preferredWorkspace string) (*Principal, error) {
	if user == nil || !user.Active() {
		return nil, ErrUnauthorized
	}
	workspaceID := preferredWorkspace
	var membership *models.Membership
	if workspaceID != "" {
		var err error
		membership, err = a.store.GetMembership(ctx, workspaceID, user.ID)
		if err != nil {
			return nil, err
		}
	} else {
		workspaceID = models.DefaultWorkspaceID
		var err error
		membership, err = a.store.GetMembership(ctx, workspaceID, user.ID)
		if errors.Is(err, database.ErrNotFound) {
			workspaces, listErr := a.store.ListWorkspacesForUser(ctx, user.ID)
			if listErr != nil {
				return nil, listErr
			}
			if len(workspaces) == 0 {
				return nil, ErrNoWorkspace
			}
			workspaceID = workspaces[0].ID
			membership, err = a.store.GetMembership(ctx, workspaceID, user.ID)
		}
		if err != nil {
			return nil, err
		}
	}
	if membership == nil || !membership.Role.Valid() {
		return nil, ErrForbidden
	}
	token, err := randomToken(a.random, 32)
	if err != nil {
		return nil, err
	}
	now := a.now()
	session := &models.Session{
		TokenHash: models.HashToken(token), UserID: user.ID, WorkspaceID: workspaceID,
		CreatedAt: now, ExpiresAt: now.Add(a.sessionTTL()),
	}
	if err := a.store.CreateSessionRecord(ctx, session); err != nil {
		return nil, err
	}
	a.setSessionCookie(w, token, session)
	return &Principal{User: user, Session: session, Membership: membership}, nil
}

// setSessionCookie writes the session cookie. Max-Age is derived from the
// stored session expiry so runtime TTL overrides are honored by browsers,
// which prefer Max-Age over the Expires attribute.
func (a *Auth) setSessionCookie(w http.ResponseWriter, token string, session *models.Session) {
	expires := time.Time{}
	if session != nil {
		expires = session.ExpiresAt
	}
	maxAge := 0
	if session != nil && !session.CreatedAt.IsZero() && session.ExpiresAt.After(session.CreatedAt) {
		maxAge = int(session.ExpiresAt.Sub(session.CreatedAt).Seconds())
	}
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookieName, Value: token, Path: "/", Expires: expires,
		MaxAge: maxAge, HttpOnly: true,
		Secure: a.opts.SecureCookies || a.opts.HTTPSMode, SameSite: http.SameSiteLaxMode,
	})
}

func (a *Auth) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookieName, Value: "", Path: "/", MaxAge: -1,
		Expires: time.Unix(1, 0), HttpOnly: true,
		Secure: a.opts.SecureCookies || a.opts.HTTPSMode, SameSite: http.SameSiteLaxMode,
	})
}

func (a *Auth) principalFromRequest(r *http.Request) (*Principal, error) {
	if a == nil || a.initErr != nil {
		return nil, ErrNotConfigured
	}
	cookie, err := r.Cookie(SessionCookieName)
	if err != nil || cookie.Value == "" {
		return nil, ErrUnauthorized
	}
	session, err := a.store.GetSessionByToken(r.Context(), cookie.Value)
	if errors.Is(err, database.ErrNotFound) {
		return nil, ErrUnauthorized
	}
	if err != nil {
		return nil, err
	}
	if session == nil || !session.Active(a.now()) {
		return nil, ErrUnauthorized
	}
	userID := session.UserID
	if userID == "" {
		userID = models.LegacyAdminUserID
	}
	user, err := a.store.GetUser(r.Context(), userID)
	if errors.Is(err, database.ErrNotFound) {
		return nil, ErrUnauthorized
	}
	if err != nil {
		return nil, err
	}
	if !user.Active() || (user.PasswordChangedAt != nil && session.CreatedAt.Before(*user.PasswordChangedAt)) {
		return nil, ErrUnauthorized
	}
	if user.ID == models.LegacyAdminUserID {
		legacyHash, settingErr := a.store.GetSetting(settingPwdHash)
		if settingErr != nil && !errors.Is(settingErr, database.ErrNotFound) {
			return nil, settingErr
		}
		if settingErr == nil && legacyHash != "" && legacyHash != user.PasswordHash {
			return nil, ErrUnauthorized
		}
	}
	workspaceID := session.WorkspaceID
	if workspaceID == "" {
		workspaceID = models.DefaultWorkspaceID
	}
	membership, err := a.store.GetMembership(r.Context(), workspaceID, user.ID)
	if errors.Is(err, database.ErrNotFound) {
		return nil, ErrForbidden
	}
	if err != nil {
		return nil, err
	}
	if !membership.Role.Valid() {
		return nil, ErrForbidden
	}
	if session.LastSeenAt == nil || a.now().Sub(*session.LastSeenAt) > 5*time.Minute {
		_ = a.store.TouchSession(r.Context(), cookie.Value)
	}
	return &Principal{User: user, Session: session, Membership: membership}, nil
}

func withPrincipal(r *http.Request, principal *Principal) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), principalContextKey, principal))
}

// IsAuthenticated retains the legacy request check.
func (a *Auth) IsAuthenticated(r *http.Request) bool {
	_, err := a.principalFromRequest(r)
	return err == nil
}

// Logout revokes only the current session and clears its cookie.
func (a *Auth) Logout(w http.ResponseWriter, r *http.Request) {
	if a == nil || a.initErr != nil {
		return
	}
	if cookie, err := r.Cookie(SessionCookieName); err == nil && cookie.Value != "" {
		_ = a.store.RevokeSession(r.Context(), cookie.Value)
	}
	a.clearSessionCookie(w)
}

func (a *Auth) audit(ctx context.Context, workspaceID, actorUserID, actorAPIKeyID, action, resourceType, resourceID, outcome string, metadata map[string]string) {
	if a == nil || a.auditHook == nil {
		return
	}
	if workspaceID == "" {
		workspaceID = models.DefaultWorkspaceID
	}
	if outcome == "" {
		outcome = "success"
	}
	_ = a.auditHook(ctx, &models.AuditEvent{
		WorkspaceID: workspaceID, ActorUserID: actorUserID, ActorAPIKeyID: actorAPIKeyID,
		Action: action, ResourceType: resourceType, ResourceID: resourceID,
		Outcome: outcome, Metadata: metadata, CreatedAt: a.now(),
	})
}

func (a *Auth) RecordAudit(r *http.Request, action, resourceType, resourceID, outcome string, metadata map[string]string) {
	var workspaceID, userID, apiKeyID string
	if principal, ok := PrincipalFromContext(r.Context()); ok {
		workspaceID = principal.WorkspaceID()
		if principal.User != nil {
			userID = principal.User.ID
		}
		if principal.APIKey != nil {
			apiKeyID = principal.APIKey.ID
		}
	}
	a.audit(r.Context(), workspaceID, userID, apiKeyID, action, resourceType, resourceID, outcome, metadata)
}
