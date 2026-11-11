package handlers

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/mail"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/metavert-io/flipbook/internal/database"
	"github.com/metavert-io/flipbook/internal/models"
)

func (h *APIHandler) ListWorkspaces(w http.ResponseWriter, r *http.Request) {
	identity, err := h.identity(r, models.RoleReader)
	if err != nil {
		apiError(w, err)
		return
	}
	if identity.APIKey != nil {
		workspace, err := h.db.GetWorkspace(r.Context(), identity.WorkspaceID)
		if err != nil {
			apiError(w, err)
			return
		}
		jsonResponse(w, map[string]any{"items": []*models.Workspace{workspace}})
		return
	}
	workspaces, err := h.db.ListWorkspacesForUser(r.Context(), identity.UserID)
	if err != nil {
		apiError(w, err)
		return
	}
	jsonResponse(w, map[string]any{"items": workspaces})
}

type workspaceRequest struct {
	Name     *string                   `json:"name"`
	Slug     *string                   `json:"slug"`
	Settings *models.WorkspaceSettings `json:"settings"`
}

func (h *APIHandler) CreateWorkspace(w http.ResponseWriter, r *http.Request) {
	identity, err := h.identity(r, models.RoleAdmin)
	if err != nil {
		apiError(w, err)
		return
	}
	var request workspaceRequest
	if err := decodeAPIJSON(r, &request); err != nil || request.Name == nil || strings.TrimSpace(*request.Name) == "" {
		invalidAPIRequest(w, "workspace name is required")
		return
	}
	workspace := &models.Workspace{
		Name: strings.TrimSpace(*request.Name), OwnerID: identity.UserID,
		Settings: models.WorkspaceSettings{DefaultEmbed: models.DefaultEmbedSettings()},
	}
	if request.Slug != nil {
		workspace.Slug = models.NormalizeSlug(*request.Slug)
	}
	if request.Settings != nil {
		workspace.Settings = *request.Settings
	}
	if err := h.db.CreateWorkspace(r.Context(), workspace, identity.UserID); err != nil {
		apiError(w, err)
		return
	}
	h.audit(r, identity, "workspace.create", "workspace", workspace.ID, nil)
	jsonStatus(w, http.StatusCreated, workspace)
}

func (h *APIHandler) GetWorkspace(w http.ResponseWriter, r *http.Request) {
	identity, err := h.identity(r, models.RoleReader)
	if err != nil {
		apiError(w, err)
		return
	}
	workspace, err := h.db.GetWorkspace(r.Context(), identity.WorkspaceID)
	if err != nil {
		apiError(w, err)
		return
	}
	jsonResponse(w, workspace)
}

func (h *APIHandler) PatchWorkspace(w http.ResponseWriter, r *http.Request) {
	identity, err := h.identity(r, models.RoleAdmin)
	if err != nil {
		apiError(w, err)
		return
	}
	var request workspaceRequest
	if err := decodeAPIJSON(r, &request); err != nil {
		invalidAPIRequest(w, "Invalid workspace: "+err.Error())
		return
	}
	workspace, err := h.db.GetWorkspace(r.Context(), identity.WorkspaceID)
	if err != nil {
		apiError(w, err)
		return
	}
	if request.Slug != nil {
		invalidAPIRequest(w, "workspace slugs are immutable")
		return
	}
	if request.Name != nil {
		workspace.Name = strings.TrimSpace(*request.Name)
		if workspace.Name == "" {
			invalidAPIRequest(w, "workspace name is required")
			return
		}
	}
	if request.Settings != nil {
		workspace.Settings = *request.Settings
	}
	if workspace.Settings.AnalyticsRetentionDays < 0 {
		invalidAPIRequest(w, "analytics retention must not be negative")
		return
	}
	if err := h.db.UpdateWorkspaceSettings(r.Context(), workspace); err != nil {
		apiError(w, err)
		return
	}
	h.audit(r, identity, "workspace.update", "workspace", workspace.ID, nil)
	jsonResponse(w, workspace)
}

type memberDTO struct {
	*models.Membership
	Name  string `json:"name,omitempty"`
	Email string `json:"email,omitempty"`
}

func (h *APIHandler) ListMemberships(w http.ResponseWriter, r *http.Request) {
	identity, err := h.identity(r, models.RoleReader)
	if err != nil {
		apiError(w, err)
		return
	}
	memberships, err := h.db.ListMemberships(r.Context(), identity.WorkspaceID)
	if err != nil {
		apiError(w, err)
		return
	}
	result := make([]memberDTO, 0, len(memberships))
	for _, membership := range memberships {
		item := memberDTO{Membership: membership}
		if user, getErr := h.db.GetUser(r.Context(), membership.UserID); getErr == nil {
			item.Name = user.Name
			item.Email = user.Email
		}
		result = append(result, item)
	}
	jsonResponse(w, map[string]any{"items": result})
}

type membershipRequest struct {
	Role models.Role `json:"role"`
}

func (h *APIHandler) PutMembership(w http.ResponseWriter, r *http.Request) {
	identity, err := h.identity(r, models.RoleAdmin)
	if err != nil {
		apiError(w, err)
		return
	}
	var request membershipRequest
	if err := decodeAPIJSON(r, &request); err != nil || !request.Role.Valid() {
		invalidAPIRequest(w, "a valid workspace role is required")
		return
	}
	if request.Role == models.RoleOwner && !identity.Role.AtLeast(models.RoleOwner) {
		apiError(w, database.ErrForbidden)
		return
	}
	userID := chi.URLParam(r, "userID")
	if _, err := h.db.GetUser(r.Context(), userID); err != nil {
		apiError(w, err)
		return
	}
	membership := &models.Membership{WorkspaceID: identity.WorkspaceID, UserID: userID, Role: request.Role, InvitedBy: identity.UserID}
	if err := h.db.UpsertMembership(r.Context(), membership); err != nil {
		apiError(w, err)
		return
	}
	h.audit(r, identity, "workspace.member.update", "membership", membership.ID, map[string]string{"role": string(request.Role)})
	jsonResponse(w, membership)
}

func (h *APIHandler) RemoveMembership(w http.ResponseWriter, r *http.Request) {
	identity, err := h.identity(r, models.RoleAdmin)
	if err != nil {
		apiError(w, err)
		return
	}
	userID := chi.URLParam(r, "userID")
	if err := h.db.RemoveMembership(r.Context(), identity.WorkspaceID, userID); err != nil {
		apiError(w, err)
		return
	}
	h.audit(r, identity, "workspace.member.remove", "user", userID, nil)
	w.WriteHeader(http.StatusNoContent)
}

type invitationRequest struct {
	Email          string      `json:"email"`
	Role           models.Role `json:"role"`
	ExpiresInHours int         `json:"expires_in_hours"`
}

func (h *APIHandler) CreateInvitation(w http.ResponseWriter, r *http.Request) {
	identity, err := h.identity(r, models.RoleAdmin)
	if err != nil {
		apiError(w, err)
		return
	}
	var request invitationRequest
	if err := decodeAPIJSON(r, &request); err != nil {
		invalidAPIRequest(w, "Invalid invitation: "+err.Error())
		return
	}
	address, err := mail.ParseAddress(strings.TrimSpace(request.Email))
	if err != nil || !strings.EqualFold(address.Address, strings.TrimSpace(request.Email)) {
		invalidAPIRequest(w, "a valid invitation email is required")
		return
	}
	if !request.Role.Valid() || request.Role == models.RoleOwner {
		invalidAPIRequest(w, "invitation role must be admin, publisher, editor, or reader")
		return
	}
	if request.ExpiresInHours == 0 {
		request.ExpiresInHours = 24 * 7
	}
	if request.ExpiresInHours < 1 || request.ExpiresInHours > 24*30 {
		invalidAPIRequest(w, "expires_in_hours must be between 1 and 720")
		return
	}
	token, err := randomAPISecret(32)
	if err != nil {
		apiError(w, err)
		return
	}
	invitation := &models.Invitation{
		WorkspaceID: identity.WorkspaceID, Email: strings.ToLower(address.Address), Role: request.Role,
		TokenHash: models.HashToken(token), InvitedBy: identity.UserID,
		ExpiresAt: h.now().Add(time.Duration(request.ExpiresInHours) * time.Hour).UTC(),
	}
	if err := h.db.CreateInvitation(r.Context(), invitation); err != nil {
		apiError(w, err)
		return
	}
	h.audit(r, identity, "workspace.invitation.create", "invitation", invitation.ID, map[string]string{"role": string(request.Role)})
	jsonStatus(w, http.StatusCreated, map[string]any{"invitation": invitation, "token": token})
}

func (h *APIHandler) RevokeInvitation(w http.ResponseWriter, r *http.Request) {
	identity, err := h.identity(r, models.RoleAdmin)
	if err != nil {
		apiError(w, err)
		return
	}
	id := chi.URLParam(r, "invitationID")
	if err := h.db.RevokeInvitation(r.Context(), identity.WorkspaceID, id); err != nil {
		apiError(w, err)
		return
	}
	h.audit(r, identity, "workspace.invitation.revoke", "invitation", id, nil)
	w.WriteHeader(http.StatusNoContent)
}

type apiKeyRequest struct {
	Name           string     `json:"name"`
	Scopes         []string   `json:"scopes"`
	ExpiresAt      *time.Time `json:"expires_at"`
	ExpiresInHours int        `json:"expires_in_hours"`
}

func (h *APIHandler) ListAPIKeys(w http.ResponseWriter, r *http.Request) {
	identity, err := h.identity(r, models.RoleAdmin)
	if err != nil {
		apiError(w, err)
		return
	}
	keys, err := h.db.ListAPIKeys(r.Context(), identity.WorkspaceID)
	if err != nil {
		apiError(w, err)
		return
	}
	jsonResponse(w, map[string]any{"items": keys})
}

func (h *APIHandler) CreateAPIKey(w http.ResponseWriter, r *http.Request) {
	identity, err := h.identity(r, models.RoleAdmin)
	if err != nil {
		apiError(w, err)
		return
	}
	var request apiKeyRequest
	if err := decodeAPIJSON(r, &request); err != nil {
		invalidAPIRequest(w, "Invalid API key: "+err.Error())
		return
	}
	request.Name = strings.TrimSpace(request.Name)
	if request.Name == "" || len(request.Name) > 200 || len(request.Scopes) == 0 {
		invalidAPIRequest(w, "API key name and scopes are required")
		return
	}
	scopes := make([]string, 0, len(request.Scopes))
	seen := make(map[string]struct{})
	for _, scope := range request.Scopes {
		scope = strings.TrimSpace(scope)
		if !validAPIScope(scope) || !identity.Role.AtLeast(apiScopeRole(scope)) {
			apiError(w, database.ErrForbidden)
			return
		}
		if _, ok := seen[scope]; !ok {
			seen[scope] = struct{}{}
			scopes = append(scopes, scope)
		}
	}
	expiresAt := request.ExpiresAt
	if expiresAt == nil && request.ExpiresInHours != 0 {
		if request.ExpiresInHours < 1 || request.ExpiresInHours > 24*366*10 {
			invalidAPIRequest(w, "expires_in_hours is outside the allowed range")
			return
		}
		value := h.now().Add(time.Duration(request.ExpiresInHours) * time.Hour).UTC()
		expiresAt = &value
	}
	if expiresAt != nil && !expiresAt.After(h.now()) {
		invalidAPIRequest(w, "API key expiry must be in the future")
		return
	}
	secret, err := randomAPISecret(32)
	if err != nil {
		apiError(w, err)
		return
	}
	secret = "fbk_" + secret
	key := &models.APIKey{
		WorkspaceID: identity.WorkspaceID, UserID: identity.UserID, Name: request.Name,
		Prefix: secret[:12], KeyHash: models.HashToken(secret), Scopes: scopes, ExpiresAt: expiresAt,
	}
	if err := h.db.CreateAPIKey(r.Context(), key); err != nil {
		apiError(w, err)
		return
	}
	h.audit(r, identity, "workspace.api_key.create", "api_key", key.ID, map[string]string{"scopes": strings.Join(scopes, " ")})
	jsonStatus(w, http.StatusCreated, map[string]any{"api_key": key, "secret": secret})
}

func (h *APIHandler) RevokeAPIKey(w http.ResponseWriter, r *http.Request) {
	identity, err := h.identity(r, models.RoleAdmin)
	if err != nil {
		apiError(w, err)
		return
	}
	id := chi.URLParam(r, "keyID")
	if err := h.db.RevokeAPIKey(r.Context(), identity.WorkspaceID, id); err != nil {
		apiError(w, err)
		return
	}
	h.audit(r, identity, "workspace.api_key.revoke", "api_key", id, nil)
	w.WriteHeader(http.StatusNoContent)
}

func (h *APIHandler) ListAuditEvents(w http.ResponseWriter, r *http.Request) {
	identity, err := h.identity(r, models.RoleAdmin)
	if err != nil {
		apiError(w, err)
		return
	}
	from, to, err := apiDateRange(r)
	if err != nil {
		invalidAPIRequest(w, err.Error())
		return
	}
	limit, _, err := apiPage(r, 100, 1000)
	if err != nil {
		invalidAPIRequest(w, err.Error())
		return
	}
	events, err := h.db.ListAuditEvents(r.Context(), database.AuditFilter{
		WorkspaceID: identity.WorkspaceID, ResourceType: r.URL.Query().Get("resource_type"),
		ResourceID: r.URL.Query().Get("resource_id"), Action: r.URL.Query().Get("action"), From: from, To: to, Limit: limit,
	})
	if err != nil {
		apiError(w, err)
		return
	}
	jsonResponse(w, map[string]any{"items": events})
}

func (h *APIHandler) ListIntegrations(w http.ResponseWriter, r *http.Request) {
	identity, err := h.identity(r, models.RoleAdmin)
	if err != nil {
		apiError(w, err)
		return
	}
	connections, err := h.db.ListIntegrationConnections(r.Context(), identity.WorkspaceID, r.URL.Query().Get("user_id"))
	if err != nil {
		apiError(w, err)
		return
	}
	jsonResponse(w, map[string]any{"items": connections})
}

func (h *APIHandler) DisconnectIntegration(w http.ResponseWriter, r *http.Request) {
	identity, err := h.identity(r, models.RoleAdmin)
	if err != nil {
		apiError(w, err)
		return
	}
	id := chi.URLParam(r, "connectionID")
	if err := h.db.DisconnectIntegrationConnection(r.Context(), identity.WorkspaceID, id); err != nil {
		apiError(w, err)
		return
	}
	h.audit(r, identity, "integration.disconnect", "integration", id, nil)
	w.WriteHeader(http.StatusNoContent)
}

type webhookRequest struct {
	URL     *string  `json:"url"`
	Events  []string `json:"events"`
	Enabled *bool    `json:"enabled"`
}

func (h *APIHandler) ListWebhooks(w http.ResponseWriter, r *http.Request) {
	identity, err := h.identity(r, models.RoleAdmin)
	if err != nil {
		apiError(w, err)
		return
	}
	subscriptions, err := h.db.ListWebhookSubscriptions(r.Context(), identity.WorkspaceID, "", false)
	if err != nil {
		apiError(w, err)
		return
	}
	jsonResponse(w, map[string]any{"items": subscriptions})
}

func (h *APIHandler) CreateWebhook(w http.ResponseWriter, r *http.Request) {
	identity, err := h.identity(r, models.RoleAdmin)
	if err != nil {
		apiError(w, err)
		return
	}
	var request webhookRequest
	if err := decodeAPIJSON(r, &request); err != nil || request.URL == nil {
		invalidAPIRequest(w, "webhook URL and events are required")
		return
	}
	parsed, err := url.Parse(strings.TrimSpace(*request.URL))
	if err != nil || parsed.Host == "" || parsed.User != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		invalidAPIRequest(w, "webhook URL must be an absolute http or https URL without user information")
		return
	}
	if len(request.Events) == 0 || len(request.Events) > 100 {
		invalidAPIRequest(w, "at least one and at most 100 webhook events are required")
		return
	}
	events := make([]string, 0, len(request.Events))
	for _, event := range request.Events {
		event = strings.TrimSpace(event)
		if event == "" || len(event) > 100 {
			invalidAPIRequest(w, "webhook event names must contain 1 to 100 bytes")
			return
		}
		events = append(events, event)
	}
	enabled := true
	if request.Enabled != nil {
		enabled = *request.Enabled
	}
	secret, err := randomAPISecret(32)
	if err != nil {
		apiError(w, err)
		return
	}
	subscription := &models.WebhookSubscription{
		WorkspaceID: identity.WorkspaceID, URL: parsed.String(), SecretCiphertext: secret,
		Events: events, Enabled: enabled,
	}
	if key := strings.TrimSpace(r.Header.Get("Idempotency-Key")); key != "" {
		digest := sha256.Sum256([]byte(identity.WorkspaceID + "\x00" + key))
		subscription.ID = "whs_" + hex.EncodeToString(digest[:16])
		existing, err := h.db.ListWebhookSubscriptions(r.Context(), identity.WorkspaceID, "", false)
		if err != nil {
			apiError(w, err)
			return
		}
		for _, candidate := range existing {
			if candidate.ID == subscription.ID {
				w.Header().Set("Idempotent-Replay", "true")
				jsonResponse(w, candidate)
				return
			}
		}
	}
	if err := h.db.CreateWebhookSubscription(r.Context(), subscription); err != nil {
		apiError(w, err)
		return
	}
	h.audit(r, identity, "webhook.create", "webhook", subscription.ID, nil)
	jsonStatus(w, http.StatusCreated, map[string]any{"webhook": subscription, "signing_secret": secret})
}

func (h *APIHandler) apiWebhook(r *http.Request, identity *apiIdentity, id string) (*models.WebhookSubscription, error) {
	subscriptions, err := h.db.ListWebhookSubscriptions(r.Context(), identity.WorkspaceID, "", false)
	if err != nil {
		return nil, err
	}
	for _, subscription := range subscriptions {
		if subscription.ID == id {
			return subscription, nil
		}
	}
	return nil, database.ErrNotFound
}

func (h *APIHandler) SetWebhookEnabled(w http.ResponseWriter, r *http.Request) {
	h.setWebhookEnabled(w, r, true)
}

func (h *APIHandler) DisableWebhook(w http.ResponseWriter, r *http.Request) {
	h.setWebhookEnabled(w, r, false)
}

func (h *APIHandler) setWebhookEnabled(w http.ResponseWriter, r *http.Request, patch bool) {
	identity, err := h.identity(r, models.RoleAdmin)
	if err != nil {
		apiError(w, err)
		return
	}
	enabled := false
	if patch {
		var request webhookRequest
		if err := decodeAPIJSON(r, &request); err != nil || request.Enabled == nil {
			invalidAPIRequest(w, "enabled is required")
			return
		}
		enabled = *request.Enabled
	}
	id := chi.URLParam(r, "webhookID")
	if err := h.db.SetWebhookSubscriptionEnabled(r.Context(), identity.WorkspaceID, id, enabled); err != nil {
		apiError(w, err)
		return
	}
	h.audit(r, identity, "webhook.enable", "webhook", id, map[string]string{"enabled": strconv.FormatBool(enabled)})
	if !patch {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	subscription, err := h.apiWebhook(r, identity, id)
	if err != nil {
		apiError(w, err)
		return
	}
	jsonResponse(w, subscription)
}

func (h *APIHandler) ListWebhookDeliveries(w http.ResponseWriter, r *http.Request) {
	identity, err := h.identity(r, models.RoleAdmin)
	if err != nil {
		apiError(w, err)
		return
	}
	limit, _, err := apiPage(r, 100, 1000)
	if err != nil {
		invalidAPIRequest(w, err.Error())
		return
	}
	deliveries, err := h.db.ListWebhookDeliveries(r.Context(), identity.WorkspaceID, chi.URLParam(r, "webhookID"), limit)
	if err != nil {
		apiError(w, err)
		return
	}
	jsonResponse(w, map[string]any{"items": deliveries})
}

func (h *APIHandler) TestWebhook(w http.ResponseWriter, r *http.Request) {
	identity, err := h.identity(r, models.RoleAdmin)
	if err != nil {
		apiError(w, err)
		return
	}
	subscription, err := h.apiWebhook(r, identity, chi.URLParam(r, "webhookID"))
	if err != nil {
		apiError(w, err)
		return
	}
	if !subscription.Enabled {
		invalidAPIRequest(w, "webhook is disabled")
		return
	}
	eventType := "webhook.test"
	for _, candidate := range subscription.Events {
		if candidate != "*" {
			eventType = candidate
			break
		}
	}
	eventID := "webhook.test:" + uuid.NewString()
	event := apiWebhookEvent{ID: eventID, Type: eventType, WorkspaceID: identity.WorkspaceID, ResourceID: subscription.ID, OccurredAt: h.now().UTC(), Data: map[string]any{"test": true, "subscription_id": subscription.ID}}
	payload, err := json.Marshal(event)
	if err != nil {
		apiError(w, err)
		return
	}
	delivery := &models.WebhookDelivery{
		WorkspaceID: identity.WorkspaceID, SubscriptionID: subscription.ID,
		EventID: eventID, EventType: eventType, PayloadJSON: string(payload), IdempotencyKey: eventID,
	}
	if err := h.db.EnqueueWebhookDelivery(r.Context(), delivery); err != nil {
		apiError(w, err)
		return
	}
	jsonStatus(w, http.StatusAccepted, delivery)
}

func (h *APIHandler) PurgeDueFlipbooks(w http.ResponseWriter, r *http.Request) {
	identity, err := h.identity(r, models.RoleAdmin)
	if err != nil {
		apiError(w, err)
		return
	}
	items, err := h.db.ListFlipbooksPendingPurge(r.Context(), h.now().UTC())
	if err != nil {
		apiError(w, err)
		return
	}
	results := make([]bulkResult, 0)
	for _, fb := range items {
		if fb.WorkspaceID != identity.WorkspaceID {
			continue
		}
		result := bulkResult{ID: fb.ID}
		if err := h.purgeFlipbook(r, identity, fb.ID); err != nil {
			result.Error = err.Error()
		} else {
			result.OK = true
			result.Status = "purged"
		}
		results = append(results, result)
	}
	jsonResponse(w, map[string]any{"items": results})
}
