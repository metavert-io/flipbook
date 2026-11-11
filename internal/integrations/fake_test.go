package integrations

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/metavert-io/flipbook/internal/database"
	"github.com/metavert-io/flipbook/internal/models"
	"github.com/metavert-io/flipbook/internal/storage"
	"github.com/metavert-io/flipbook/internal/worker"
)

type fakeStore struct {
	mu           sync.Mutex
	connections  map[string]*models.IntegrationConnection
	workspaces   map[string]*models.Workspace
	flipbooks    map[string]*models.Flipbook
	revisions    map[string]*models.Revision
	webhooks     map[string]*models.WebhookSubscription
	deliveries   map[string]*models.WebhookDelivery
	nextRevision int
	nextDelivery int
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		// Matches a workspace created through the admin or API handlers.
		workspaces: map[string]*models.Workspace{
			"workspace": {ID: "workspace", Settings: models.WorkspaceSettings{DefaultEmbed: models.DefaultEmbedSettings()}},
		},
		connections: make(map[string]*models.IntegrationConnection),
		flipbooks:   make(map[string]*models.Flipbook),
		revisions:   make(map[string]*models.Revision),
		webhooks:    make(map[string]*models.WebhookSubscription),
		deliveries:  make(map[string]*models.WebhookDelivery),
	}
}

func cloneConnection(connection *models.IntegrationConnection) *models.IntegrationConnection {
	if connection == nil {
		return nil
	}
	clone := *connection
	return &clone
}

func (f *fakeStore) CreateIntegrationConnection(_ context.Context, connection *models.IntegrationConnection) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if connection.WorkspaceID == "" || connection.UserID == "" || connection.Provider == "" {
		return errors.New("missing connection scope")
	}
	if _, exists := f.connections[connection.ID]; exists {
		return database.ErrConflict
	}
	now := time.Now().UTC()
	if connection.CreatedAt.IsZero() {
		connection.CreatedAt = now
	}
	connection.UpdatedAt = now
	f.connections[connection.ID] = cloneConnection(connection)
	return nil
}

func (f *fakeStore) UpdateIntegrationConnection(_ context.Context, connection *models.IntegrationConnection) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	existing := f.connections[connection.ID]
	if existing == nil || existing.WorkspaceID != connection.WorkspaceID {
		return database.ErrNotFound
	}
	connection.UpdatedAt = time.Now().UTC()
	f.connections[connection.ID] = cloneConnection(connection)
	return nil
}

func (f *fakeStore) ListIntegrationConnections(_ context.Context, workspaceID, userID string) ([]*models.IntegrationConnection, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var result []*models.IntegrationConnection
	for _, connection := range f.connections {
		if connection.WorkspaceID == workspaceID && (userID == "" || connection.UserID == userID) {
			result = append(result, cloneConnection(connection))
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Provider == result[j].Provider {
			return result[i].CreatedAt.Before(result[j].CreatedAt)
		}
		return result[i].Provider < result[j].Provider
	})
	return result, nil
}

func (f *fakeStore) DisconnectIntegrationConnection(_ context.Context, workspaceID, connectionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	connection := f.connections[connectionID]
	if connection == nil || connection.WorkspaceID != workspaceID {
		return database.ErrNotFound
	}
	now := time.Now().UTC()
	connection.Status = models.IntegrationDisconnected
	connection.DisconnectedAt = &now
	connection.AccessTokenEncrypted = ""
	connection.RefreshTokenEncrypted = ""
	connection.TokenExpiresAt = nil
	connection.UpdatedAt = now
	return nil
}

func (f *fakeStore) GetFlipbookByExternalID(_ context.Context, workspaceID, source, externalID string) (*models.Flipbook, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, flipbook := range f.flipbooks {
		if flipbook.WorkspaceID == workspaceID && flipbook.ExternalSource == source && flipbook.ExternalID == externalID && flipbook.DeletedAt == nil {
			clone := *flipbook
			return &clone, nil
		}
	}
	return nil, database.ErrNotFound
}

func (f *fakeStore) GetWorkspace(_ context.Context, workspaceID string) (*models.Workspace, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	workspace := f.workspaces[workspaceID]
	if workspace == nil {
		return nil, database.ErrNotFound
	}
	clone := *workspace
	return &clone, nil
}

func (f *fakeStore) CreateFlipbookContext(_ context.Context, flipbook *models.Flipbook) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, existing := range f.flipbooks {
		if existing.WorkspaceID == flipbook.WorkspaceID && existing.ExternalSource == flipbook.ExternalSource && existing.ExternalID == flipbook.ExternalID && flipbook.ExternalID != "" {
			return database.ErrConflict
		}
	}
	now := time.Now().UTC()
	if flipbook.CreatedAt.IsZero() {
		flipbook.CreatedAt = now
	}
	flipbook.UpdatedAt = now
	flipbook.ApplyDefaults()
	if flipbook.CurrentRevisionID == "" {
		flipbook.CurrentRevisionID = flipbook.ID + "-v1"
	}
	revision := &models.Revision{
		ID: flipbook.CurrentRevisionID, WorkspaceID: flipbook.WorkspaceID, FlipbookID: flipbook.ID,
		Number: 1, AssetVersion: flipbook.AssetVersion, Status: models.RevisionPending,
		SourceFilename: flipbook.Filename, SourceChecksum: flipbook.SourceChecksum,
		ExternalSource: flipbook.ExternalSource, ExternalID: flipbook.ExternalID,
		CreatedBy: flipbook.OwnerID, CreatedAt: now, UpdatedAt: now,
	}
	f.revisions[revision.ID] = revision
	clone := *flipbook
	f.flipbooks[flipbook.ID] = &clone
	return nil
}

func (f *fakeStore) CreateRevision(_ context.Context, revision *models.Revision) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if revision.FlipbookID == "" || revision.WorkspaceID == "" {
		return errors.New("missing revision scope")
	}
	f.nextRevision++
	if revision.ID == "" {
		revision.ID = "revision-" + strconv.Itoa(f.nextRevision)
	}
	revision.Number = 1
	revision.AssetVersion = 1
	for _, existing := range f.revisions {
		if existing.FlipbookID == revision.FlipbookID {
			if existing.Number >= revision.Number {
				revision.Number = existing.Number + 1
			}
			if existing.AssetVersion >= revision.AssetVersion {
				revision.AssetVersion = existing.AssetVersion + 1
			}
		}
	}
	now := time.Now().UTC()
	revision.Status = models.RevisionPending
	revision.CreatedAt = now
	revision.UpdatedAt = now
	f.revisions[revision.ID] = revision
	return nil
}

func (f *fakeStore) GenerateUniqueSlug(_ context.Context, workspaceID, slug string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if slug == "" {
		slug = "provider-document"
	}
	base := slug
	for counter := 1; ; counter++ {
		available := true
		for _, flipbook := range f.flipbooks {
			if flipbook.WorkspaceID == workspaceID && flipbook.Slug == slug {
				available = false
				break
			}
		}
		if available {
			return slug, nil
		}
		slug = base + "-" + strconv.Itoa(counter+1)
	}
}

func (f *fakeStore) CreateWebhookSubscription(_ context.Context, subscription *models.WebhookSubscription) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if subscription.ID == "" || subscription.WorkspaceID == "" || len(subscription.Events) == 0 {
		return errors.New("missing webhook scope")
	}
	now := time.Now().UTC()
	subscription.CreatedAt = now
	subscription.UpdatedAt = now
	f.webhooks[subscription.ID] = subscription
	return nil
}

func (f *fakeStore) ListWebhookSubscriptions(_ context.Context, workspaceID, eventType string, enabledOnly bool) ([]*models.WebhookSubscription, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var result []*models.WebhookSubscription
	for _, subscription := range f.webhooks {
		if subscription.WorkspaceID != workspaceID || (enabledOnly && !subscription.Enabled) {
			continue
		}
		matches := eventType == ""
		for _, event := range subscription.Events {
			matches = matches || event == "*" || event == eventType
		}
		if matches {
			clone := *subscription
			result = append(result, &clone)
		}
	}
	return result, nil
}

func (f *fakeStore) SetWebhookSubscriptionEnabled(_ context.Context, workspaceID, subscriptionID string, enabled bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	subscription := f.webhooks[subscriptionID]
	if subscription == nil || subscription.WorkspaceID != workspaceID {
		return database.ErrNotFound
	}
	subscription.Enabled = enabled
	subscription.UpdatedAt = time.Now().UTC()
	return nil
}

func (f *fakeStore) EnqueueWebhookDelivery(_ context.Context, delivery *models.WebhookDelivery) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, existing := range f.deliveries {
		if existing.WorkspaceID == delivery.WorkspaceID && existing.SubscriptionID == delivery.SubscriptionID && existing.EventID == delivery.EventID {
			return nil
		}
	}
	f.nextDelivery++
	if delivery.ID == "" {
		delivery.ID = "delivery-" + strconv.Itoa(f.nextDelivery)
	}
	f.deliveries[delivery.ID] = delivery
	return nil
}

func (f *fakeStore) ListWebhookDeliveries(_ context.Context, workspaceID, subscriptionID string, limit int64) ([]*models.WebhookDelivery, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var result []*models.WebhookDelivery
	for _, delivery := range f.deliveries {
		if delivery.WorkspaceID == workspaceID && (subscriptionID == "" || delivery.SubscriptionID == subscriptionID) {
			result = append(result, delivery)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CreatedAt.After(result[j].CreatedAt) })
	if int64(len(result)) > limit {
		result = result[:limit]
	}
	return result, nil
}

type fakeQueue struct {
	jobs []worker.Job
	err  error
}

func (f *fakeQueue) EnqueueContext(_ context.Context, job worker.Job) (*models.Job, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.jobs = append(f.jobs, job)
	return &models.Job{
		ID:          fmt.Sprintf("conversion-%d", len(f.jobs)),
		WorkspaceID: job.WorkspaceID, FlipbookID: job.FlipbookID, RevisionID: job.RevisionID,
		Status: models.JobQueued,
	}, nil
}

func testService(t *testing.T, store *fakeStore, queue *fakeQueue, provider, serverURL string) *Service {
	t.Helper()
	providers := DefaultProviderConfigs()
	if provider != "" {
		config := providers[provider]
		config.Enabled = true
		config.ClientID = "client-id"
		config.ClientSecret = "client-secret"
		config.RedirectURL = serverURL + "/oauth/callback"
		config.AuthorizationURL = serverURL + "/oauth/authorize"
		config.TokenURL = serverURL + "/oauth/token"
		config.RevocationURL = serverURL + "/oauth/revoke"
		config.APIBaseURL = serverURL
		providers[provider] = config
	}
	options := Options{
		Providers: providers, EncryptionKey: "0123456789abcdef0123456789abcdef",
		Now:              func() time.Time { return time.Unix(1700000100, 0).UTC() },
		MaxDownloadBytes: 1 << 20, ExportPollInterval: time.Millisecond, ExportPollAttempts: 3,
	}
	service, err := NewService(store, storage.New(t.TempDir()), queue, options)
	if err != nil {
		t.Fatalf("new integration service: %v", err)
	}
	return service
}

func seedConnection(t *testing.T, service *Service, store *fakeStore, provider string, expired bool) *models.IntegrationConnection {
	t.Helper()
	now := service.nowUTC()
	connection := &models.IntegrationConnection{
		ID: "connection-" + provider, WorkspaceID: "workspace", UserID: "user", Provider: provider,
		Status: models.IntegrationConnected, DisplayName: providerDisplayName(provider),
		CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour),
	}
	var err error
	connection.AccessTokenEncrypted, err = service.cipher.Seal(connectionTokenContext(connection.ID, "access"), "access-token")
	if err != nil {
		t.Fatal(err)
	}
	connection.RefreshTokenEncrypted, err = service.cipher.Seal(connectionTokenContext(connection.ID, "refresh"), "refresh-token")
	if err != nil {
		t.Fatal(err)
	}
	expires := now.Add(time.Hour)
	if expired {
		expires = now.Add(-time.Hour)
	}
	connection.TokenExpiresAt = &expires
	if err := store.CreateIntegrationConnection(context.Background(), connection); err != nil {
		t.Fatal(err)
	}
	return connection
}
