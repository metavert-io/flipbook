package handlers

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/metavert-io/flipbook/internal/models"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

func (h *AdminHandler) Workspace(w http.ResponseWriter, r *http.Request) {
	actor, err := adminActorFromRequest(r, models.RoleAdmin)
	if err != nil {
		h.renderError(w, r, err)
		return
	}
	h.renderWorkspace(w, r, actor, http.StatusOK, nil)
}

func (h *AdminHandler) renderWorkspace(w http.ResponseWriter, r *http.Request, actor adminActor, status int, overrides map[string]any) {
	workspace, err := h.db.GetWorkspace(r.Context(), actor.WorkspaceID)
	if err != nil {
		h.renderError(w, r, err)
		return
	}
	data := map[string]any{
		"PageTitle": "Workspace settings - Penninn Midlun", "TeamSection": "workspace", "CurrentWorkspace": workspace,
		"DefaultDomainString": joinAdminValues(workspace.Settings.DefaultEmbed.AllowedDomains),
	}
	for key, value := range overrides {
		data[key] = value
	}
	if err := h.render(w, r, actor, "team", "admin_workspace", status, data); err != nil {
		h.renderError(w, r, err)
	}
}

func (h *AdminHandler) UpdateWorkspace(w http.ResponseWriter, r *http.Request) {
	actor, err := adminActorFromRequest(r, models.RoleAdmin)
	if err != nil {
		h.renderError(w, r, err)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	if err := r.ParseForm(); err != nil {
		h.renderWorkspace(w, r, actor, http.StatusBadRequest, map[string]any{"Error": "The workspace form is invalid."})
		return
	}
	workspace, err := h.db.GetWorkspace(r.Context(), actor.WorkspaceID)
	if err != nil {
		h.renderError(w, r, err)
		return
	}
	if err := applyAdminWorkspaceForm(workspace, r); err != nil {
		h.renderWorkspace(w, r, actor, http.StatusBadRequest, map[string]any{"Error": err.Error()})
		return
	}
	if err := h.db.UpdateWorkspaceSettings(r.Context(), workspace); err != nil {
		status, message := adminErrorMessage(err)
		h.renderWorkspace(w, r, actor, status, map[string]any{"Error": message})
		return
	}
	h.audit(r, actor, "workspace.settings.update", "workspace", workspace.ID, "success", nil)
	http.Redirect(w, r, "/admin/workspace?notice="+url.QueryEscape("Workspace settings saved. Defaults apply to new uploads."), http.StatusSeeOther)
}

func applyAdminWorkspaceForm(workspace *models.Workspace, r *http.Request) error {
	workspace.Name = strings.TrimSpace(r.FormValue("name"))
	if workspace.Name == "" || len(workspace.Name) > 200 {
		return adminInputf("Workspace name is required and must be at most 200 characters.")
	}
	settings := &workspace.Settings
	visibility := models.Visibility(r.FormValue("default_visibility"))
	switch visibility {
	case models.VisibilityPublic, models.VisibilityUnlisted, models.VisibilityPrivate, models.VisibilityTeam:
		settings.DefaultVisibility = visibility
	default:
		return adminInputf("Choose a valid default visibility.")
	}
	var err error
	if settings.Branding.LogoURL, err = validateAdminURL(r.FormValue("workspace_logo_url"), "Workspace logo URL", true); err != nil {
		return err
	}
	if settings.Branding.FaviconURL, err = validateAdminURL(r.FormValue("workspace_favicon_url"), "Workspace favicon URL", true); err != nil {
		return err
	}
	if settings.Branding.PrimaryColor, err = validateAdminColor(r.FormValue("workspace_primary_color"), "Workspace primary color"); err != nil {
		return err
	}
	if settings.Branding.BackgroundColor, err = validateAdminColor(r.FormValue("workspace_background_color"), "Workspace background color"); err != nil {
		return err
	}
	settings.Branding.Attribution = strings.TrimSpace(r.FormValue("workspace_attribution"))
	settings.Branding.PublisherName = strings.TrimSpace(r.FormValue("workspace_publisher_name"))
	if settings.Branding.PublisherURL, err = validateAdminURL(r.FormValue("workspace_publisher_url"), "Workspace publisher URL", true); err != nil {
		return err
	}
	settings.Branding.HideBranding = adminFormBool(r, "workspace_hide_branding")

	settings.DefaultDownload.Enabled = adminFormBool(r, "default_download_enabled")
	settings.DefaultDownload.AllowOriginal = adminFormBool(r, "default_download_original")
	settings.DefaultDownload.AllowPDF = adminFormBool(r, "default_download_pdf")
	settings.DefaultDownload.AllowPrint = adminFormBool(r, "default_download_print")
	settings.DefaultEmbed.Enabled = adminFormBool(r, "default_embed_enabled")
	settings.DefaultEmbed.ShowToolbar = adminFormBool(r, "default_embed_toolbar")
	settings.DefaultEmbed.ShowSearch = adminFormBool(r, "default_embed_search")
	settings.DefaultEmbed.ShowShare = adminFormBool(r, "default_embed_share")
	settings.DefaultEmbed.ShowDownload = adminFormBool(r, "default_embed_download")
	if settings.DefaultEmbed.AllowedDomains, err = validateAdminDomains(r.FormValue("default_embed_domains")); err != nil {
		return err
	}
	if settings.DefaultEmbed.StartPage, err = parseAdminInt(r.FormValue("default_embed_start_page"), "Default embed start page", 1, 1000000); err != nil {
		return err
	}
	settings.DefaultEmbed.ReadingMode = strings.TrimSpace(r.FormValue("default_embed_reading_mode"))
	switch settings.DefaultEmbed.ReadingMode {
	case "", "flip", "single", "double", "continuous", "html":
	default:
		return adminInputf("Choose a supported default reading mode.")
	}
	if settings.DefaultEmbed.Background, err = validateAdminColor(r.FormValue("default_embed_background"), "Default embed background"); err != nil {
		return err
	}
	settings.DefaultShare.Enabled = adminFormBool(r, "default_share_enabled")
	settings.DefaultShare.Native = adminFormBool(r, "default_share_native")
	settings.DefaultShare.Email = adminFormBool(r, "default_share_email")
	settings.DefaultShare.Facebook = adminFormBool(r, "default_share_facebook")
	settings.DefaultShare.LinkedIn = adminFormBool(r, "default_share_linkedin")
	settings.DefaultShare.X = adminFormBool(r, "default_share_x")

	settings.RequirePublicationReview = adminFormBool(r, "require_review")
	settings.AnalyticsConsentPolicy = strings.TrimSpace(r.FormValue("analytics_consent_policy"))
	switch settings.AnalyticsConsentPolicy {
	case "required", "anonymous", "disabled":
	default:
		return adminInputf("Choose a valid analytics consent policy.")
	}
	if settings.AnalyticsRetentionDays, err = parseAdminInt(r.FormValue("analytics_retention_days"), "Analytics retention", 1, 3650); err != nil {
		return err
	}
	settings.NotificationEmail = strings.TrimSpace(r.FormValue("notification_email"))
	if settings.NotificationEmail != "" {
		if settings.NotificationEmail, err = validateAdminEmail(settings.NotificationEmail); err != nil {
			return err
		}
	}
	return nil
}

func (h *AdminHandler) CreateWorkspace(w http.ResponseWriter, r *http.Request) {
	actor, err := adminActorFromRequest(r, models.RoleAdmin)
	if err != nil {
		h.renderError(w, r, err)
		return
	}
	if err := r.ParseForm(); err != nil {
		h.renderWorkspace(w, r, actor, http.StatusBadRequest, map[string]any{"Error": "The new-workspace form is invalid."})
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	workspace := &models.Workspace{
		Name: name, Slug: models.NormalizeSlug(r.FormValue("slug")),
		Settings: models.WorkspaceSettings{DefaultEmbed: models.DefaultEmbedSettings()},
	}
	if name == "" || len(name) > 200 {
		h.renderWorkspace(w, r, actor, http.StatusBadRequest, map[string]any{"Error": "A workspace name of at most 200 characters is required."})
		return
	}
	if err := h.db.CreateWorkspace(r.Context(), workspace, actor.UserID); err != nil {
		status, message := adminErrorMessage(err)
		if mongo.IsDuplicateKeyError(err) {
			status, message = http.StatusConflict, "That workspace slug is already in use."
		}
		h.renderWorkspace(w, r, actor, status, map[string]any{"Error": message})
		return
	}
	h.audit(r, actor, "workspace.create", "workspace", workspace.ID, "success", nil)
	http.Redirect(w, r, "/admin/workspace?notice="+url.QueryEscape("Workspace created. Workspace switching requires a new session selection."), http.StatusSeeOther)
}
