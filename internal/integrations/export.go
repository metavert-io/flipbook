package integrations

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/metavert-io/flipbook/internal/database"
	"github.com/metavert-io/flipbook/internal/models"
	"github.com/metavert-io/flipbook/internal/worker"
)

type exportJob struct {
	ID           string
	Status       string
	StatusURL    string
	DownloadURLs []string
	ErrorCode    string
	ErrorMessage string
}

func (j *exportJob) succeeded() bool {
	switch strings.ToLower(j.Status) {
	case "success", "succeeded", "completed", "complete":
		return true
	default:
		return false
	}
}

func (j *exportJob) failed() bool {
	switch strings.ToLower(j.Status) {
	case "failed", "failure", "error", "cancelled", "canceled":
		return true
	default:
		return false
	}
}

func (j *exportJob) failure(provider string) error {
	kind := ErrProvider
	code := strings.ToLower(j.ErrorCode)
	if strings.Contains(code, "license") || strings.Contains(code, "approval") || strings.Contains(code, "permission") || strings.Contains(code, "access") {
		kind = ErrPermission
	}
	message := providerDisplayName(provider) + " export failed"
	if j.ErrorCode != "" {
		message += ": " + cleanProviderMessage(j.ErrorCode)
	} else {
		message += "."
	}
	if kind == ErrPermission {
		message += " Check app approval, export entitlements, and document sharing."
	}
	if j.ErrorMessage != "" {
		message += " Provider response: " + cleanProviderMessage(j.ErrorMessage)
	}
	return integrationError(kind, provider, "export", message)
}

func waitExportPoll(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (c *providerClient) exportPDF(ctx context.Context, connection *models.IntegrationConnection, externalID, pages string) (*exportJob, error) {
	if strings.TrimSpace(c.config.ExportPath) == "" {
		return nil, integrationError(ErrNotConfigured, c.provider, "export", "The provider export endpoint is not configured.")
	}
	var job *exportJob
	var err error
	switch c.provider {
	case ProviderCanva:
		job, err = c.createCanvaExport(ctx, connection, externalID)
	case ProviderAdobeExpress:
		job, err = c.createAdobeExport(ctx, connection, externalID, pages)
	default:
		err = integrationError(ErrNotFound, c.provider, "export", "The requested integration provider is not supported.")
	}
	if err != nil {
		return nil, err
	}
	for attempt := 0; ; attempt++ {
		if job.failed() {
			return nil, job.failure(c.provider)
		}
		if job.succeeded() {
			if len(job.DownloadURLs) == 0 {
				return nil, integrationError(ErrProvider, c.provider, "export", "The provider completed the export without a download URL.")
			}
			return job, nil
		}
		if job.ID == "" {
			return nil, integrationError(ErrProvider, c.provider, "export", "The provider returned an export job without an ID.")
		}
		if attempt >= c.service.opts.ExportPollAttempts {
			return nil, integrationError(ErrProvider, c.provider, "export", "The provider export did not complete within the polling limit. Try again later.")
		}
		if err := waitExportPoll(ctx, c.service.opts.ExportPollInterval); err != nil {
			return nil, err
		}
		switch c.provider {
		case ProviderCanva:
			job, err = c.canvaExportStatus(ctx, connection, job)
		case ProviderAdobeExpress:
			job, err = c.adobeExportStatus(ctx, connection, job)
		}
		if err != nil {
			return nil, err
		}
	}
}

func validateExportRequest(request ExportRequest) (ExportRequest, error) {
	request.Provider = strings.TrimSpace(request.Provider)
	request.WorkspaceID = strings.TrimSpace(request.WorkspaceID)
	request.UserID = strings.TrimSpace(request.UserID)
	request.ExternalID = strings.TrimSpace(request.ExternalID)
	request.Title = strings.TrimSpace(request.Title)
	request.Pages = strings.TrimSpace(request.Pages)
	if request.WorkspaceID == "" || request.UserID == "" || request.ExternalID == "" {
		return request, integrationError(ErrInvalidInput, request.Provider, "export", "Workspace, user, and external document ID are required.")
	}
	if len(request.ExternalID) > 512 || len(request.Title) > 200 || len(request.Pages) > 100 {
		return request, integrationError(ErrInvalidInput, request.Provider, "export", "The external ID, title, or page range is too long.")
	}
	return request, nil
}

func (s *Service) ExportAndReupload(ctx context.Context, request ExportRequest) (*ExportResult, error) {
	request, err := validateExportRequest(request)
	if err != nil {
		return nil, err
	}
	config, err := s.providerConfig(request.Provider)
	if err != nil {
		return nil, err
	}
	if !config.Configured() || s.cipher == nil {
		return nil, integrationError(ErrNotConfigured, request.Provider, "export", providerDisplayName(request.Provider)+" OAuth, API endpoints, and token encryption must be configured before exporting.")
	}
	if err := validateProviderEndpoints(config); err != nil {
		return nil, err
	}
	connection, err := s.connectedConnection(ctx, request.WorkspaceID, request.UserID, request.Provider)
	if err != nil {
		return nil, err
	}
	client := s.newProviderClient(request.Provider, config)
	job, err := client.exportPDF(ctx, connection, request.ExternalID, request.Pages)
	if err != nil {
		s.recordConnectionError(ctx, connection, err)
		return nil, err
	}
	if len(job.DownloadURLs) != 1 {
		return nil, integrationError(ErrProvider, request.Provider, "export", "A single PDF download was expected, but the provider returned a different number of files.")
	}
	response, err := client.download(ctx, job.DownloadURLs[0])
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()

	flipbook, lookupErr := s.store.GetFlipbookByExternalID(ctx, request.WorkspaceID, request.Provider, request.ExternalID)
	isNew := false
	if errors.Is(lookupErr, database.ErrNotFound) || errors.Is(lookupErr, ErrNotFound) {
		isNew = true
		flipbook = &models.Flipbook{ID: uuid.NewString()}
	} else if lookupErr != nil {
		return nil, lookupErr
	}
	title := request.Title
	if title == "" {
		title = providerDisplayName(request.Provider) + " document " + truncateIntegrationText(request.ExternalID, 64)
	}
	var defaults models.FlipbookDefaults
	if isNew {
		workspace, err := s.store.GetWorkspace(ctx, request.WorkspaceID)
		if err != nil {
			return nil, fmt.Errorf("load workspace defaults: %w", err)
		}
		defaults = workspace.NewFlipbookDefaults()
		slug, err := s.store.GenerateUniqueSlug(ctx, request.WorkspaceID, integrationSlug(title))
		if err != nil {
			return nil, err
		}
		flipbook.Slug = slug
	}
	stagingDir, err := s.assets.CreateRevisionStaging(flipbook.ID)
	if err != nil {
		return nil, err
	}
	keepSource := false
	defer func() {
		if !keepSource {
			_ = os.RemoveAll(stagingDir)
		}
	}()
	reader := bufio.NewReader(response.Body)
	header, err := reader.Peek(5)
	if err != nil || string(header) != "%PDF-" {
		return nil, integrationError(ErrProvider, request.Provider, "download export", "The provider download was not a valid PDF document.")
	}
	filename := truncateIntegrationText(integrationSlug(title), 100) + ".pdf"
	upload, err := s.assets.SaveStagedUpload(ctx, flipbook.ID, stagingDir, filename, reader, s.opts.MaxDownloadBytes)
	if err != nil {
		return nil, fmt.Errorf("save provider export: %w", err)
	}

	revisionID := ""
	if isNew {
		flipbook.WorkspaceID = request.WorkspaceID
		flipbook.OwnerID = request.UserID
		flipbook.Title = title
		flipbook.Filename = filename
		flipbook.FileSize = upload.Size
		flipbook.Status = models.StatusPending
		flipbook.PublicationState = models.PublicationDraft
		flipbook.ExternalSource = request.Provider
		flipbook.ExternalID = request.ExternalID
		flipbook.SourceChecksum = upload.SHA256
		defaults.Apply(flipbook)
		if err := s.store.CreateFlipbookContext(ctx, flipbook); err != nil {
			_ = s.assets.DeleteFlipbookContext(context.WithoutCancel(ctx), flipbook.ID)
			return nil, err
		}
		revisionID = flipbook.CurrentRevisionID
	} else {
		revision := &models.Revision{
			WorkspaceID: request.WorkspaceID, FlipbookID: flipbook.ID, SourceFilename: filename,
			SourceChecksum: upload.SHA256, ExternalSource: request.Provider, ExternalID: request.ExternalID,
			CreatedBy: request.UserID,
		}
		if err := s.store.CreateRevision(ctx, revision); err != nil {
			return nil, err
		}
		revisionID = revision.ID
	}
	keepSource = true
	queued, err := s.queue.EnqueueContext(ctx, worker.Job{
		WorkspaceID: request.WorkspaceID, FlipbookID: flipbook.ID, RevisionID: revisionID,
		SourcePath: upload.Path, IdempotencyKey: "integration:" + request.Provider + ":" + revisionID,
	})
	result := &ExportResult{
		Provider: request.Provider, ExternalID: request.ExternalID, FlipbookID: flipbook.ID,
		RevisionID: revisionID, AdminURL: "/admin/flipbooks/" + flipbook.ID, Status: "queued",
	}
	if queued != nil {
		result.JobID = queued.ID
	}
	if err != nil {
		return result, integrationError(ErrProvider, request.Provider, "queue conversion", "The PDF was saved as a new revision, but conversion could not be queued. Retry conversion from the publication.")
	}
	if _, webhookErr := s.EmitWebhook(ctx, request.WorkspaceID, "integration.export.queued", uuid.NewString(), map[string]any{
		"provider": request.Provider, "external_id": request.ExternalID, "flipbook_id": flipbook.ID,
		"revision_id": revisionID, "job_id": result.JobID,
	}); webhookErr != nil {
		result.Warnings = append(result.Warnings, "The revision was queued, but the integration webhook could not be recorded.")
	}
	return result, nil
}

func truncateIntegrationText(value string, maximum int) string {
	if maximum < 1 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= maximum {
		return value
	}
	return string(runes[:maximum-1]) + "…"
}

func integrationSlug(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var builder strings.Builder
	separator := false
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if separator && builder.Len() > 0 {
				builder.WriteByte('-')
			}
			separator = false
			builder.WriteRune(r)
			continue
		}
		separator = true
	}
	slug := strings.Trim(builder.String(), "-")
	if slug == "" {
		return "provider-document"
	}
	return slug
}

func (s *Service) Statistics(ctx context.Context, workspaceID, userID, provider, externalID string) (*Statistics, error) {
	externalID = strings.TrimSpace(externalID)
	if workspaceID == "" || userID == "" || externalID == "" || len(externalID) > 512 {
		return nil, integrationError(ErrInvalidInput, provider, "statistics", "Workspace, user, and a valid external document ID are required.")
	}
	config, err := s.providerConfig(provider)
	if err != nil {
		return nil, err
	}
	if !config.Configured() || s.cipher == nil || strings.TrimSpace(config.StatisticsPath) == "" {
		return nil, integrationError(ErrNotConfigured, provider, "statistics", providerDisplayName(provider)+" statistics are not configured. An approved app and a supported document statistics/metadata endpoint are required.")
	}
	connection, err := s.connectedConnection(ctx, workspaceID, userID, provider)
	if err != nil {
		return nil, err
	}
	client := s.newProviderClient(provider, config)
	reference := expandProviderPath(config.StatisticsPath, "externalID", externalID)
	data := make(map[string]any)
	if err := client.doJSON(ctx, connection, http.MethodGet, reference, "statistics", nil, &data); err != nil {
		s.recordConnectionError(ctx, connection, err)
		return nil, err
	}
	statistics := &Statistics{
		Provider: provider, ExternalID: externalID, Data: data, RetrievedAt: s.nowUTC(),
		Title:     statisticString(data, "title", "name"),
		PageCount: statisticInt(data, "page_count", "pageCount", "pages"),
		CreatedAt: statisticTime(data, "created_at", "createdAt"),
		UpdatedAt: statisticTime(data, "updated_at", "updatedAt", "modified_at", "modifiedAt"),
	}
	return statistics, nil
}

func statisticValue(data map[string]any, keys ...string) any {
	for _, key := range keys {
		if value, ok := data[key]; ok {
			return value
		}
	}
	for _, value := range data {
		switch nested := value.(type) {
		case map[string]any:
			if found := statisticValue(nested, keys...); found != nil {
				return found
			}
		case []any:
			for _, item := range nested {
				if object, ok := item.(map[string]any); ok {
					if found := statisticValue(object, keys...); found != nil {
						return found
					}
				}
			}
		}
	}
	return nil
}

func statisticString(data map[string]any, keys ...string) string {
	value, _ := statisticValue(data, keys...).(string)
	return value
}

func statisticInt(data map[string]any, keys ...string) int64 {
	switch value := statisticValue(data, keys...).(type) {
	case float64:
		return int64(value)
	case int64:
		return value
	case int:
		return int64(value)
	case string:
		parsed, _ := strconv.ParseInt(value, 10, 64)
		return parsed
	default:
		return 0
	}
}

func statisticTime(data map[string]any, keys ...string) string {
	value := statisticValue(data, keys...)
	switch typed := value.(type) {
	case string:
		return typed
	case float64:
		return time.Unix(int64(typed), 0).UTC().Format(time.RFC3339)
	case int64:
		return time.Unix(typed, 0).UTC().Format(time.RFC3339)
	default:
		return ""
	}
}
