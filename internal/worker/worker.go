package worker

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/metavert-io/flipbook/internal/converter"
	"github.com/metavert-io/flipbook/internal/database"
	"github.com/metavert-io/flipbook/internal/models"
	"github.com/metavert-io/flipbook/internal/storage"
)

var (
	ErrJobCancelled      = errors.New("job cancellation requested")
	ErrAttemptsExhausted = errors.New("maximum attempts exceeded")
)

type Job struct {
	FlipbookID     string
	SourcePath     string
	WorkspaceID    string
	RevisionID     string
	IdempotencyKey string
	Priority       int
	MaxAttempts    int
}

type workerDB interface {
	EnqueueJob(ctx context.Context, job *models.Job) error
	GetJob(ctx context.Context, jobID string) (*models.Job, error)
	LeaseJobs(ctx context.Context, workerID string, types []models.JobType, limit int, leaseDuration time.Duration) ([]*models.Job, error)
	HeartbeatJob(ctx context.Context, jobID, workerID string, leaseDuration time.Duration, progress int, message string) error
	CompleteJob(ctx context.Context, jobID, workerID string) error
	FailJob(ctx context.Context, jobID, workerID, failure string, baseBackoff, maxBackoff time.Duration) error
	FailJobPermanently(ctx context.Context, jobID, workerID, failure string) error
	CancelJob(ctx context.Context, jobID string) error
	RetryJob(ctx context.Context, jobID string) error

	GetFlipbook(id string) (*models.Flipbook, error)
	GetFlipbookInWorkspace(ctx context.Context, workspaceID, id string) (*models.Flipbook, error)
	UpdateStatus(id, status, errorMsg string) error
	UpdateConversionResult(id string, pageCount, width, height int) error
	SetPublicationState(ctx context.Context, workspaceID, id string, state models.PublicationState, visibility models.Visibility, now time.Time) error
	ListDuePublications(ctx context.Context, now time.Time) ([]*models.Flipbook, error)

	CreateRevision(ctx context.Context, revision *models.Revision) error
	GetRevision(ctx context.Context, workspaceID, revisionID string) (*models.Revision, error)
	UpdateRevisionProgress(ctx context.Context, revisionID string, progress int, message string) error
	MarkRevisionReady(ctx context.Context, revisionID, manifestKey string, pageCount int, warnings []string) error
	MarkRevisionFailed(ctx context.Context, revisionID, errorMessage string) error
	ActivateRevision(ctx context.Context, workspaceID, flipbookID, revisionID string) error

	UploadToGridFS(ctx context.Context, filename string, r io.Reader) (string, error)
	DownloadFromGridFS(ctx context.Context, fileIDHex string, w io.Writer) (int64, error)
	SetGridFSFileID(id, gridfsFileID string) error

	ListWebhookSubscriptions(ctx context.Context, workspaceID, eventType string, enabledOnly bool) ([]*models.WebhookSubscription, error)
	LeaseWebhookDeliveries(ctx context.Context, workerID string, limit int, leaseDuration time.Duration) ([]*models.WebhookDelivery, error)
	CompleteWebhookDelivery(ctx context.Context, deliveryID, workerID string, responseStatus int) error
	FailWebhookDelivery(ctx context.Context, deliveryID, workerID, failure string, responseStatus int, baseBackoff time.Duration) error
}

type revisionStorage interface {
	CreateRevisionStaging(id string) (string, error)
	InstallRevision(id, stagingDir string, revision int) (string, error)
	ActivateRevision(id string, revision int) error
	CurrentRevision(id string) (int, error)
	RevisionDir(id string, revision int) string
	OriginalPath(id, ext string) string
	DeleteRevision(id string, revision int) error
	SyncRevision(ctx context.Context, id string, revision int, verify bool) ([]storage.ObjectInfo, error)
}

type stagedConverter interface {
	ConvertStaged(ctx context.Context, srcPath, stagingDir string) (*converter.Result, error)
}

type Worker struct {
	db      workerDB
	storage revisionStorage
	conv    stagedConverter
	opts    Options
	logger  *log.Logger

	mu      sync.Mutex
	started bool
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

func New(db *database.DB, store *storage.Storage, conv *converter.Converter) *Worker {
	worker, err := NewWithOptions(db, store, conv, DefaultOptions())
	if err != nil {
		panic(err)
	}
	return worker
}

func NewWithOptions(db *database.DB, store *storage.Storage, conv *converter.Converter, options Options) (*Worker, error) {
	return newWorker(db, store, conv, options)
}

func newWorker(db workerDB, store revisionStorage, conv stagedConverter, options Options) (*Worker, error) {
	options = normalizeOptions(options)
	if err := validateOptions(options); err != nil {
		return nil, err
	}
	if db == nil || store == nil || conv == nil {
		return nil, errors.New("worker database, storage, and converter are required")
	}
	logger := options.Logger
	if logger == nil {
		logger = log.Default()
	}
	return &Worker{db: db, storage: store, conv: conv, opts: options, logger: logger}, nil
}

type progressState struct {
	mu      sync.RWMutex
	value   int
	message string
}

func (p *progressState) Set(value int, message string) {
	if value < 0 {
		value = 0
	}
	if value > 100 {
		value = 100
	}
	p.mu.Lock()
	p.value = value
	p.message = message
	p.mu.Unlock()
}

func (p *progressState) Get() (int, string) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.value, p.message
}

func webhookHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}
