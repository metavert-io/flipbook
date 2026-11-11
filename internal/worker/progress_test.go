package worker

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/metavert-io/flipbook/internal/converter"
	"github.com/metavert-io/flipbook/internal/models"
)

func TestConversionProgressMapsPagesIntoRenderBand(t *testing.T) {
	db, store, _ := conversionSetup(t)
	worker := testWorker(t, db, store, &fakeConverter{})
	state := &progressState{}
	report := worker.conversionProgress(context.Background(), "revision-2", state)

	cases := []struct {
		progress converter.Progress
		value    int
		message  string
	}{
		{converter.Progress{Stage: converter.ProgressConvertDocument}, 10, "converting document to PDF"},
		{converter.Progress{Stage: converter.ProgressRenderPages, Done: 0, Total: 60}, 10, "rendered 0 of 60 pages"},
		{converter.Progress{Stage: converter.ProgressRenderPages, Done: 1, Total: 60}, 11, "rendered 1 of 60 pages"},
		{converter.Progress{Stage: converter.ProgressRenderPages, Done: 30, Total: 60}, 40, "rendered 30 of 60 pages"},
		{converter.Progress{Stage: converter.ProgressRenderPages, Done: 60, Total: 60}, 70, "rendered 60 of 60 pages"},
		{converter.Progress{Stage: converter.ProgressExtractText}, 72, "extracting text and links"},
	}
	for _, tc := range cases {
		report(tc.progress)
		value, message := state.Get()
		if value != tc.value || message != tc.message {
			t.Fatalf("%+v reported %d %q, want %d %q", tc.progress, value, message, tc.value, tc.message)
		}
		if got := db.revisions["revision-2"].Progress; tc.value > 10 && got != tc.value {
			t.Fatalf("%+v persisted revision progress %d, want %d", tc.progress, got, tc.value)
		}
	}
}

func TestConversionPublishesRenderProgressDuringConversion(t *testing.T) {
	db, store, sourcePath := conversionSetup(t)
	var midway int
	conv := &fakeConverter{fn: func(ctx context.Context, source, staging string) (*converter.Result, error) {
		converter.ReportProgress(ctx, converter.Progress{Stage: converter.ProgressRenderPages, Done: 2, Total: 4})
		midway = db.revisions["revision-2"].Progress
		return writeConversionFixture(source, staging)
	}}
	worker := testWorker(t, db, store, conv)
	job := conversionJob(db, worker, sourcePath)
	if err := worker.processLeased(context.Background(), job); err != nil {
		t.Fatalf("process conversion: %v", err)
	}
	if midway != 40 {
		t.Fatalf("revision progress at 2 of 4 pages = %d, want 40", midway)
	}
}

func TestConversionTimeoutIsNotRetried(t *testing.T) {
	db, store, sourcePath := conversionSetup(t)
	conv := &fakeConverter{fn: func(context.Context, string, string) (*converter.Result, error) {
		return nil, &converter.ConversionError{Kind: converter.ErrTimeout, Cause: errors.New("stopped after 40m0s on a 60-page document")}
	}}
	worker := testWorker(t, db, store, conv)
	job := conversionJob(db, worker, sourcePath)
	if err := worker.processLeased(context.Background(), job); !errors.Is(err, converter.ErrTimeout) {
		t.Fatalf("timeout returned %v", err)
	}
	if status := db.jobs[job.ID].Status; status != models.JobDead {
		t.Fatalf("timed-out job status = %s, want dead", status)
	}
}

// A worker killed during its final attempt leaves the job running with an
// expired lease. The next lease must close the publication out as failed
// rather than leave it "converting" forever.
func TestExhaustedConversionMarksPublicationFailed(t *testing.T) {
	db, store, sourcePath := conversionSetup(t)
	flipbook := db.flipbooks["book"]
	flipbook.Status = models.StatusConverting
	flipbook.CurrentRevisionID = "revision-2"
	conv := &fakeConverter{fn: func(context.Context, string, string) (*converter.Result, error) {
		t.Fatal("exhausted job must not be converted again")
		return nil, nil
	}}
	worker := testWorker(t, db, store, conv)
	job := conversionJob(db, worker, sourcePath)
	job.Attempts = job.MaxAttempts + 1
	job.LastError = "conversion pdftoppm page 3: signal: killed"

	err := worker.processLeased(context.Background(), job)
	if !errors.Is(err, ErrAttemptsExhausted) {
		t.Fatalf("exhausted job returned %v", err)
	}
	if db.jobs[job.ID].Status != models.JobDead {
		t.Fatalf("exhausted job status = %s, want dead", db.jobs[job.ID].Status)
	}
	if flipbook.Status != models.StatusError || !strings.Contains(flipbook.ErrorMessage, "maximum attempts exceeded") || !strings.Contains(flipbook.ErrorMessage, "signal: killed") {
		t.Fatalf("flipbook not marked failed: status=%s message=%q", flipbook.Status, flipbook.ErrorMessage)
	}
	if db.revisions["revision-2"].Status != models.RevisionFailed {
		t.Fatalf("revision status = %s, want failed", db.revisions["revision-2"].Status)
	}
}

func TestExhaustedReconversionKeepsLiveRevision(t *testing.T) {
	db, store, sourcePath := conversionSetup(t)
	worker := testWorker(t, db, store, &fakeConverter{})
	job := conversionJob(db, worker, sourcePath)
	job.Attempts = job.MaxAttempts + 1

	if err := worker.processLeased(context.Background(), job); !errors.Is(err, ErrAttemptsExhausted) {
		t.Fatalf("exhausted job returned %v", err)
	}
	flipbook := db.flipbooks["book"]
	if flipbook.Status != models.StatusReady || flipbook.CurrentRevisionID != "revision-1" || flipbook.ErrorMessage == "" {
		t.Fatalf("live revision not kept: %+v", flipbook)
	}
	if db.revisions["revision-1"].Status != models.RevisionActive || db.revisions["revision-2"].Status != models.RevisionFailed {
		t.Fatalf("revision states: live=%s new=%s", db.revisions["revision-1"].Status, db.revisions["revision-2"].Status)
	}
}

func TestExhaustedJobAfterActivationLeavesPublicationReady(t *testing.T) {
	db, store, sourcePath := conversionSetup(t)
	flipbook := db.flipbooks["book"]
	flipbook.CurrentRevisionID = "revision-2"
	db.revisions["revision-2"].Status = models.RevisionActive
	worker := testWorker(t, db, store, &fakeConverter{})
	job := conversionJob(db, worker, sourcePath)
	job.Attempts = job.MaxAttempts + 1

	_ = worker.processLeased(context.Background(), job)
	if flipbook.Status != models.StatusReady || flipbook.ErrorMessage != "" {
		t.Fatalf("activated publication was marked failed: %+v", flipbook)
	}
	if db.revisions["revision-2"].Status != models.RevisionActive {
		t.Fatalf("activated revision status = %s", db.revisions["revision-2"].Status)
	}
}
