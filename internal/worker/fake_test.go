package worker

import (
	"context"
	"errors"
	"io"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/metavert-io/flipbook/internal/converter"
	"github.com/metavert-io/flipbook/internal/models"
)

type statusUpdate struct {
	ID      string
	Status  string
	Message string
}

type fakeDB struct {
	mu sync.Mutex

	flipbooks  map[string]*models.Flipbook
	revisions  map[string]*models.Revision
	jobs       map[string]*models.Job
	subs       map[string]*models.WebhookSubscription
	deliveries map[string]*models.WebhookDelivery
	gridfs     map[string][]byte

	nextJobID     int
	nextGridFSID  int
	uploadCount   int
	statusUpdates []statusUpdate
	heartbeats    []string

	enqueueErr          error
	heartbeatErr        error
	activateErr         error
	conversionResultErr error
	uploadErr           error
	downloadErr         error
	setGridFSErr        error
}

func newFakeDB() *fakeDB {
	return &fakeDB{
		flipbooks: make(map[string]*models.Flipbook), revisions: make(map[string]*models.Revision),
		jobs: make(map[string]*models.Job), subs: make(map[string]*models.WebhookSubscription),
		deliveries: make(map[string]*models.WebhookDelivery), gridfs: make(map[string][]byte),
	}
}

func cloneJob(job *models.Job) *models.Job {
	copy := *job
	return &copy
}

func (f *fakeDB) EnqueueJob(_ context.Context, job *models.Job) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.enqueueErr != nil {
		return f.enqueueErr
	}
	if job.IdempotencyKey != "" {
		for _, existing := range f.jobs {
			if existing.WorkspaceID == job.WorkspaceID && existing.IdempotencyKey == job.IdempotencyKey {
				*job = *cloneJob(existing)
				return nil
			}
		}
	}
	f.nextJobID++
	if job.ID == "" {
		job.ID = "job-" + strconv.Itoa(f.nextJobID)
	}
	if job.Status == "" {
		job.Status = models.JobQueued
	}
	if job.RunAt.IsZero() {
		job.RunAt = time.Now()
	}
	if job.MaxAttempts == 0 {
		job.MaxAttempts = 5
	}
	if job.CreatedAt.IsZero() {
		job.CreatedAt = time.Unix(0, int64(f.nextJobID)).UTC()
	}
	f.jobs[job.ID] = cloneJob(job)
	return nil
}

func (f *fakeDB) GetJob(_ context.Context, jobID string) (*models.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	job := f.jobs[jobID]
	if job == nil {
		return nil, errors.New("job not found")
	}
	return cloneJob(job), nil
}

func (f *fakeDB) LeaseJobs(_ context.Context, workerID string, types []models.JobType, limit int, leaseDuration time.Duration) ([]*models.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now()
	ids := make([]string, 0, len(f.jobs))
	for id := range f.jobs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var leased []*models.Job
	for _, id := range ids {
		job := f.jobs[id]
		typeAllowed := len(types) == 0
		for _, jobType := range types {
			typeAllowed = typeAllowed || job.Type == jobType
		}
		available := ((job.Status == models.JobQueued || job.Status == models.JobRetry) && !job.CancelRequested && !job.RunAt.After(now)) ||
			(job.Status == models.JobRunning && job.LeaseExpiresAt != nil && !job.LeaseExpiresAt.After(now))
		if !typeAllowed || !available {
			continue
		}
		job.Status = models.JobRunning
		job.LeaseOwner = workerID
		expires := now.Add(leaseDuration)
		job.LeaseExpiresAt = &expires
		job.Attempts++
		leased = append(leased, cloneJob(job))
		if len(leased) == limit {
			break
		}
	}
	return leased, nil
}

func (f *fakeDB) HeartbeatJob(_ context.Context, jobID, workerID string, _ time.Duration, progress int, message string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.heartbeatErr != nil {
		return f.heartbeatErr
	}
	job := f.jobs[jobID]
	if job == nil || job.Status != models.JobRunning || job.LeaseOwner != workerID {
		return errors.New("lease lost")
	}
	job.Progress = progress
	job.ProgressMessage = message
	f.heartbeats = append(f.heartbeats, message)
	return nil
}

func (f *fakeDB) CompleteJob(_ context.Context, jobID, workerID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	job := f.jobs[jobID]
	if job == nil || job.Status != models.JobRunning || job.LeaseOwner != workerID {
		return errors.New("lease lost")
	}
	job.Status = models.JobSucceeded
	job.Progress = 100
	return nil
}

func (f *fakeDB) FailJob(_ context.Context, jobID, workerID, failure string, baseBackoff, maxBackoff time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	job := f.jobs[jobID]
	if job == nil || job.Status != models.JobRunning || job.LeaseOwner != workerID {
		return errors.New("lease lost")
	}
	job.LastError = failure
	switch {
	case job.CancelRequested:
		job.Status = models.JobCancelled
	case job.Attempts >= job.MaxAttempts:
		job.Status = models.JobDead
	default:
		job.Status = models.JobRetry
		job.RunAt = time.Now().Add(models.RetryDelay(job.Attempts, baseBackoff, maxBackoff))
	}
	return nil
}

func (f *fakeDB) FailJobPermanently(_ context.Context, jobID, workerID, failure string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	job := f.jobs[jobID]
	if job == nil || job.Status != models.JobRunning || job.LeaseOwner != workerID {
		return errors.New("lease lost")
	}
	job.LastError = failure
	if job.CancelRequested {
		job.Status = models.JobCancelled
	} else {
		job.Status = models.JobDead
	}
	return nil
}

func (f *fakeDB) CancelJob(_ context.Context, jobID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	job := f.jobs[jobID]
	if job == nil {
		return errors.New("job not found")
	}
	job.CancelRequested = true
	if job.Status == models.JobQueued || job.Status == models.JobRetry {
		job.Status = models.JobCancelled
	}
	return nil
}

func (f *fakeDB) RetryJob(_ context.Context, jobID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	job := f.jobs[jobID]
	if job == nil || (job.Status != models.JobDead && job.Status != models.JobCancelled) {
		return errors.New("job is not retryable")
	}
	job.Status = models.JobQueued
	job.CancelRequested = false
	job.Attempts = 0
	job.RunAt = time.Now()
	return nil
}

func (f *fakeDB) GetFlipbook(id string) (*models.Flipbook, error) {
	return f.GetFlipbookInWorkspace(context.Background(), "", id)
}

func (f *fakeDB) GetFlipbookInWorkspace(_ context.Context, workspaceID, id string) (*models.Flipbook, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	flipbook := f.flipbooks[id]
	if flipbook == nil || (workspaceID != "" && flipbook.WorkspaceID != workspaceID) {
		return nil, errors.New("flipbook not found")
	}
	copy := *flipbook
	return &copy, nil
}

func (f *fakeDB) UpdateStatus(id, status, errorMsg string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	flipbook := f.flipbooks[id]
	if flipbook == nil {
		return errors.New("flipbook not found")
	}
	flipbook.Status = status
	flipbook.ErrorMessage = errorMsg
	f.statusUpdates = append(f.statusUpdates, statusUpdate{ID: id, Status: status, Message: errorMsg})
	return nil
}

func (f *fakeDB) UpdateConversionResult(id string, pageCount, width, height int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.conversionResultErr != nil {
		return f.conversionResultErr
	}
	flipbook := f.flipbooks[id]
	if flipbook == nil {
		return errors.New("flipbook not found")
	}
	flipbook.PageCount = pageCount
	flipbook.PageWidth = width
	flipbook.PageHeight = height
	flipbook.Status = models.StatusReady
	flipbook.ErrorMessage = ""
	if revision := f.revisions[flipbook.CurrentRevisionID]; revision != nil {
		revision.Status = models.RevisionActive
		revision.PageCount = pageCount
		revision.Progress = 100
	}
	return nil
}

func (f *fakeDB) SetPublicationState(_ context.Context, workspaceID, id string, state models.PublicationState, visibility models.Visibility, now time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	flipbook := f.flipbooks[id]
	if flipbook == nil || flipbook.WorkspaceID != workspaceID {
		return errors.New("flipbook not found")
	}
	flipbook.PublicationState = state
	flipbook.Visibility = visibility
	flipbook.IsPublic = state == models.PublicationPublished && (visibility == models.VisibilityPublic || visibility == models.VisibilityUnlisted)
	if state == models.PublicationPublished {
		flipbook.PublishedAt = &now
	} else {
		flipbook.UnpublishedAt = &now
		flipbook.IsPublic = false
	}
	return nil
}

func (f *fakeDB) ListDuePublications(_ context.Context, now time.Time) ([]*models.Flipbook, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ids := make([]string, 0, len(f.flipbooks))
	for id := range f.flipbooks {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var result []*models.Flipbook
	for _, id := range ids {
		flipbook := f.flipbooks[id]
		duePublish := flipbook.PublicationState == models.PublicationDraft && flipbook.PublishAt != nil && !flipbook.PublishAt.After(now)
		dueExpiry := flipbook.PublicationState == models.PublicationPublished && flipbook.ExpiresAt != nil && !flipbook.ExpiresAt.After(now)
		if duePublish || dueExpiry {
			copy := *flipbook
			result = append(result, &copy)
		}
	}
	return result, nil
}

func (f *fakeDB) CreateRevision(_ context.Context, revision *models.Revision) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextJobID++
	revision.ID = "revision-" + strconv.Itoa(f.nextJobID)
	revision.Number = 1
	for _, existing := range f.revisions {
		if existing.FlipbookID == revision.FlipbookID && existing.Number >= revision.Number {
			revision.Number = existing.Number + 1
		}
	}
	revision.AssetVersion = int64(revision.Number)
	revision.Status = models.RevisionPending
	f.revisions[revision.ID] = revision
	return nil
}

func (f *fakeDB) GetRevision(_ context.Context, workspaceID, revisionID string) (*models.Revision, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	revision := f.revisions[revisionID]
	if revision == nil || revision.WorkspaceID != workspaceID {
		return nil, errors.New("revision not found")
	}
	copy := *revision
	return &copy, nil
}

func (f *fakeDB) UpdateRevisionProgress(_ context.Context, revisionID string, progress int, message string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	revision := f.revisions[revisionID]
	if revision == nil {
		return errors.New("revision not found")
	}
	revision.Progress = progress
	return nil
}

func (f *fakeDB) MarkRevisionReady(_ context.Context, revisionID, manifestKey string, pageCount int, warnings []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	revision := f.revisions[revisionID]
	if revision == nil {
		return errors.New("revision not found")
	}
	revision.Status = models.RevisionReady
	revision.ManifestKey = manifestKey
	revision.PageCount = pageCount
	revision.Progress = 100
	revision.Warnings = warnings
	return nil
}

func (f *fakeDB) MarkRevisionFailed(_ context.Context, revisionID, errorMessage string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	revision := f.revisions[revisionID]
	if revision == nil {
		return errors.New("revision not found")
	}
	revision.Status = models.RevisionFailed
	revision.ErrorMessage = errorMessage
	return nil
}

func (f *fakeDB) ActivateRevision(_ context.Context, workspaceID, flipbookID, revisionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.activateErr != nil {
		return f.activateErr
	}
	flipbook := f.flipbooks[flipbookID]
	revision := f.revisions[revisionID]
	if flipbook == nil || revision == nil || flipbook.WorkspaceID != workspaceID {
		return errors.New("activation target not found")
	}
	for _, candidate := range f.revisions {
		if candidate.FlipbookID == flipbookID && candidate.ID != revisionID && candidate.Status == models.RevisionActive {
			candidate.Status = models.RevisionSuperseded
		}
	}
	revision.Status = models.RevisionActive
	flipbook.CurrentRevisionID = revisionID
	flipbook.AssetVersion = revision.AssetVersion
	flipbook.PageCount = revision.PageCount
	flipbook.Status = models.StatusReady
	return nil
}

func (f *fakeDB) UploadToGridFS(_ context.Context, _ string, reader io.Reader) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.uploadErr != nil {
		return "", f.uploadErr
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		return "", err
	}
	f.nextGridFSID++
	f.uploadCount++
	id := "grid-" + strconv.Itoa(f.nextGridFSID)
	f.gridfs[id] = data
	return id, nil
}

func (f *fakeDB) DownloadFromGridFS(_ context.Context, fileIDHex string, writer io.Writer) (int64, error) {
	f.mu.Lock()
	data, ok := f.gridfs[fileIDHex]
	err := f.downloadErr
	f.mu.Unlock()
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, errors.New("GridFS object not found")
	}
	written, err := writer.Write(data)
	return int64(written), err
}

func (f *fakeDB) SetGridFSFileID(id, gridfsFileID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.setGridFSErr != nil {
		return f.setGridFSErr
	}
	flipbook := f.flipbooks[id]
	if flipbook == nil {
		return errors.New("flipbook not found")
	}
	now := time.Now()
	flipbook.GridFSFileID = gridfsFileID
	flipbook.BackupStatus = "complete"
	flipbook.BackedUpAt = &now
	flipbook.BackupError = ""
	return nil
}

func (f *fakeDB) ListWebhookSubscriptions(_ context.Context, workspaceID, eventType string, enabledOnly bool) ([]*models.WebhookSubscription, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var result []*models.WebhookSubscription
	for _, subscription := range f.subs {
		if subscription.WorkspaceID != workspaceID || (enabledOnly && !subscription.Enabled) {
			continue
		}
		matches := eventType == ""
		for _, event := range subscription.Events {
			matches = matches || event == "*" || event == eventType
		}
		if matches {
			result = append(result, subscription)
		}
	}
	return result, nil
}

func (f *fakeDB) LeaseWebhookDeliveries(_ context.Context, workerID string, limit int, leaseDuration time.Duration) ([]*models.WebhookDelivery, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now()
	var result []*models.WebhookDelivery
	for _, delivery := range f.deliveries {
		available := ((delivery.Status == models.WebhookPending || delivery.Status == models.WebhookRetry) && !delivery.NextAttemptAt.After(now)) ||
			(delivery.Status == models.WebhookDelivering && delivery.LeaseExpiresAt != nil && !delivery.LeaseExpiresAt.After(now))
		if !available {
			continue
		}
		delivery.Status = models.WebhookDelivering
		delivery.LeaseOwner = workerID
		expires := now.Add(leaseDuration)
		delivery.LeaseExpiresAt = &expires
		delivery.Attempts++
		result = append(result, delivery)
		if len(result) == limit {
			break
		}
	}
	return result, nil
}

func (f *fakeDB) CompleteWebhookDelivery(_ context.Context, deliveryID, workerID string, responseStatus int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delivery := f.deliveries[deliveryID]
	if delivery == nil || delivery.Status != models.WebhookDelivering || delivery.LeaseOwner != workerID {
		return errors.New("webhook lease lost")
	}
	delivery.Status = models.WebhookDelivered
	delivery.ResponseStatus = responseStatus
	return nil
}

func (f *fakeDB) FailWebhookDelivery(_ context.Context, deliveryID, workerID, failure string, responseStatus int, baseBackoff time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delivery := f.deliveries[deliveryID]
	if delivery == nil || delivery.Status != models.WebhookDelivering || delivery.LeaseOwner != workerID {
		return errors.New("webhook lease lost")
	}
	delivery.LastError = failure
	delivery.ResponseStatus = responseStatus
	if delivery.Attempts >= delivery.MaxAttempts {
		delivery.Status = models.WebhookFailed
	} else {
		delivery.Status = models.WebhookRetry
		delivery.NextAttemptAt = time.Now().Add(models.RetryDelay(delivery.Attempts, baseBackoff, 24*time.Hour))
	}
	return nil
}

type fakeConverter struct {
	calls int
	fn    func(ctx context.Context, sourcePath, stagingDir string) (*converter.Result, error)
}

func (f *fakeConverter) ConvertStaged(ctx context.Context, sourcePath, stagingDir string) (*converter.Result, error) {
	f.calls++
	return f.fn(ctx, sourcePath, stagingDir)
}
