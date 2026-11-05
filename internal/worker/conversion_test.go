package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/metavert-io/flipbook/internal/converter"
	"github.com/metavert-io/flipbook/internal/models"
	"github.com/metavert-io/flipbook/internal/storage"
)

func conversionSetup(t *testing.T) (*fakeDB, *storage.Storage, string) {
	t.Helper()
	db := newFakeDB()
	store := storage.New(filepath.Join(t.TempDir(), "flipbooks"))
	sourcePath := store.OriginalPath("book", ".pdf")
	writeSource(t, sourcePath)
	db.flipbooks["book"] = &models.Flipbook{
		ID: "book", WorkspaceID: "workspace", OwnerID: "owner", Filename: "book.pdf",
		Status: models.StatusReady, CurrentRevisionID: "revision-1", AssetVersion: 1,
		PublicationState: models.PublicationDraft, Visibility: models.VisibilityPublic, ReviewState: models.ReviewNotRequired,
	}
	db.revisions["revision-1"] = &models.Revision{
		ID: "revision-1", WorkspaceID: "workspace", FlipbookID: "book", Number: 1, AssetVersion: 1,
		Status: models.RevisionActive, SourceFilename: "book.pdf", PageCount: 1,
	}
	db.revisions["revision-2"] = &models.Revision{
		ID: "revision-2", WorkspaceID: "workspace", FlipbookID: "book", Number: 2, AssetVersion: 2,
		Status: models.RevisionPending, SourceFilename: "book.pdf",
	}
	addRevisionFixture(t, store, "book", 1, sourcePath)
	return db, store, sourcePath
}

func conversionJob(db *fakeDB, worker *Worker, sourcePath string) *models.Job {
	source := inspectSource(sourcePath)
	job := runningJob(worker, &models.Job{
		ID: "conversion-job", WorkspaceID: "workspace", FlipbookID: "book", RevisionID: "revision-2",
		Type: models.JobConvert, MaxAttempts: 2,
		Payload: map[string]any{
			"source_path": sourcePath, "source_checksum": source.SHA256, "source_size": source.Size, "source_filename": "book.pdf",
		},
	})
	db.jobs[job.ID] = job
	return job
}

func TestConversionInstallsAndActivatesRevision(t *testing.T) {
	db, store, sourcePath := conversionSetup(t)
	conv := &fakeConverter{fn: func(_ context.Context, source, staging string) (*converter.Result, error) {
		if source != sourcePath {
			t.Fatalf("conversion source = %q, want %q", source, sourcePath)
		}
		return writeConversionFixture(source, staging)
	}}
	worker := testWorker(t, db, store, conv)
	job := conversionJob(db, worker, sourcePath)

	if err := worker.processLeased(context.Background(), job); err != nil {
		t.Fatalf("process conversion: %v", err)
	}
	if conv.calls != 1 {
		t.Fatalf("converter calls = %d, want 1", conv.calls)
	}
	current, err := store.CurrentRevision("book")
	if err != nil || current != 2 {
		t.Fatalf("active storage revision = %d, %v", current, err)
	}
	flipbook := db.flipbooks["book"]
	if flipbook.CurrentRevisionID != "revision-2" || flipbook.Status != models.StatusReady || flipbook.PageWidth != 2 || flipbook.PageHeight != 3 {
		t.Fatalf("flipbook activation incorrect: %+v", flipbook)
	}
	if db.revisions["revision-1"].Status != models.RevisionSuperseded || db.revisions["revision-2"].Status != models.RevisionActive {
		t.Fatalf("revision states incorrect: old=%s new=%s", db.revisions["revision-1"].Status, db.revisions["revision-2"].Status)
	}
	if _, err := os.Stat(store.RevisionDir("book", 1)); err != nil {
		t.Fatalf("previous revision was removed: %v", err)
	}
	if db.jobs[job.ID].Status != models.JobSucceeded {
		t.Fatalf("job status = %s", db.jobs[job.ID].Status)
	}
	foundBackup := false
	for _, queued := range db.jobs {
		foundBackup = foundBackup || queued.Type == models.JobBackup && queued.RevisionID == "revision-2"
	}
	if !foundBackup {
		t.Fatal("conversion did not enqueue revision backup")
	}
}

func TestConversionFailureKeepsPreviousVersionLive(t *testing.T) {
	db, store, sourcePath := conversionSetup(t)
	conv := &fakeConverter{fn: func(context.Context, string, string) (*converter.Result, error) {
		return nil, errors.New("render exploded")
	}}
	worker := testWorker(t, db, store, conv)
	job := conversionJob(db, worker, sourcePath)

	if err := worker.processLeased(context.Background(), job); err == nil {
		t.Fatal("expected conversion failure")
	}
	current, err := store.CurrentRevision("book")
	if err != nil || current != 1 {
		t.Fatalf("previous storage revision is not live: %d, %v", current, err)
	}
	flipbook := db.flipbooks["book"]
	if flipbook.CurrentRevisionID != "revision-1" || flipbook.Status != models.StatusReady || !strings.Contains(flipbook.ErrorMessage, "render exploded") {
		t.Fatalf("previous database revision is not available: %+v", flipbook)
	}
	if db.revisions["revision-2"].Status != models.RevisionFailed {
		t.Fatalf("failed revision status = %s", db.revisions["revision-2"].Status)
	}
	if db.jobs[job.ID].Status != models.JobRetry {
		t.Fatalf("failed job status = %s, want retry", db.jobs[job.ID].Status)
	}
}

func TestConversionRecoveryUsesInstalledRevision(t *testing.T) {
	db, store, sourcePath := conversionSetup(t)
	mustWriteConversionFixture(t, sourcePath, store.RevisionDir("book", 2))
	db.revisions["revision-2"].Status = models.RevisionConverting
	conv := &fakeConverter{fn: func(context.Context, string, string) (*converter.Result, error) {
		return nil, errors.New("converter must not run during installed-revision recovery")
	}}
	worker := testWorker(t, db, store, conv)
	job := conversionJob(db, worker, sourcePath)

	if err := worker.processLeased(context.Background(), job); err != nil {
		t.Fatalf("recover conversion: %v", err)
	}
	if conv.calls != 0 {
		t.Fatalf("converter ran %d times during recovery", conv.calls)
	}
	current, err := store.CurrentRevision("book")
	if err != nil || current != 2 || db.jobs[job.ID].Status != models.JobSucceeded {
		t.Fatalf("recovery did not activate installed revision: current=%d err=%v job=%s", current, err, db.jobs[job.ID].Status)
	}
}

func TestConversionCancellationIsRecorded(t *testing.T) {
	db, store, sourcePath := conversionSetup(t)
	db.flipbooks["book"].Status = models.StatusPending
	db.flipbooks["book"].CurrentRevisionID = "revision-2"
	delete(db.revisions, "revision-1")
	started := make(chan struct{})
	conv := &fakeConverter{fn: func(ctx context.Context, _, _ string) (*converter.Result, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	worker := testWorker(t, db, store, conv)
	worker.opts.HeartbeatInterval = 5 * time.Millisecond
	worker.opts.LeaseDuration = time.Second
	job := conversionJob(db, worker, sourcePath)

	done := make(chan error, 1)
	go func() {
		done <- worker.processLeased(context.Background(), job)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("converter did not start")
	}
	if err := worker.Cancel(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation did not stop conversion")
	}
	if db.jobs[job.ID].Status != models.JobCancelled {
		t.Fatalf("cancelled job status = %s", db.jobs[job.ID].Status)
	}
}

func TestPermanentConversionFailureIsNotRetried(t *testing.T) {
	db, store, sourcePath := conversionSetup(t)
	conv := &fakeConverter{fn: func(context.Context, string, string) (*converter.Result, error) {
		return nil, &converter.ConversionError{Kind: converter.ErrQuotaExceeded, Op: "pixel limit", Cause: errors.New("too many pixels")}
	}}
	worker := testWorker(t, db, store, conv)
	job := conversionJob(db, worker, sourcePath)

	if err := worker.processLeased(context.Background(), job); err == nil {
		t.Fatal("expected conversion failure")
	}
	if got := db.jobs[job.ID]; got.Status != models.JobDead || got.Attempts >= got.MaxAttempts {
		t.Fatalf("job status = %s after attempt %d/%d, want dead without exhausting attempts", got.Status, got.Attempts, got.MaxAttempts)
	}
}
