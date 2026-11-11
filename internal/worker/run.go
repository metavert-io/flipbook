package worker

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/metavert-io/flipbook/internal/converter"
	"github.com/metavert-io/flipbook/internal/models"
)

func (w *Worker) Start(ctx context.Context) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.started {
		return
	}
	runCtx, cancel := context.WithCancel(ctx)
	w.cancel = cancel
	w.started = true

	for i := 0; i < w.opts.WorkerCount; i++ {
		w.wg.Add(1)
		go func() {
			defer w.wg.Done()
			w.jobLoop(runCtx)
		}()
	}
	if w.hasJobType(models.JobPublish) || w.hasJobType(models.JobUnpublish) {
		w.wg.Add(1)
		go func() {
			defer w.wg.Done()
			w.scheduleLoop(runCtx)
		}()
	}
	if w.opts.Webhooks.Enabled {
		for i := 0; i < w.opts.WebhookWorkerCount; i++ {
			w.wg.Add(1)
			go func() {
				defer w.wg.Done()
				w.webhookLoop(runCtx)
			}()
		}
	}
}

func (w *Worker) Run(ctx context.Context) error {
	w.Start(ctx)
	<-ctx.Done()
	w.Wait()
	return nil
}

func (w *Worker) Stop() {
	w.mu.Lock()
	cancel := w.cancel
	w.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	w.Wait()
}

func (w *Worker) Wait() {
	w.wg.Wait()
}

func (w *Worker) hasJobType(jobType models.JobType) bool {
	for _, candidate := range w.opts.JobTypes {
		if candidate == jobType {
			return true
		}
	}
	return false
}

func (w *Worker) jobLoop(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		jobs, err := w.db.LeaseJobs(ctx, w.opts.WorkerID, w.opts.JobTypes, 1, w.opts.LeaseDuration)
		if err != nil {
			if ctx.Err() == nil {
				w.logger.Printf("[worker] lease failed: %v", err)
			}
			if !waitContext(ctx, w.opts.PollInterval) {
				return
			}
			continue
		}
		if len(jobs) == 0 {
			if !waitContext(ctx, w.opts.PollInterval) {
				return
			}
			continue
		}
		for _, job := range jobs {
			if err := w.processLeased(ctx, job); err != nil && ctx.Err() == nil && !errors.Is(err, ErrJobCancelled) {
				w.logger.Printf("[worker] job %s (%s) failed: %v", job.ID, job.Type, err)
			}
		}
	}
}

func (w *Worker) ProcessOne(ctx context.Context) (bool, error) {
	jobs, err := w.db.LeaseJobs(ctx, w.opts.WorkerID, w.opts.JobTypes, 1, w.opts.LeaseDuration)
	if err != nil || len(jobs) == 0 {
		return false, err
	}
	return true, w.processLeased(ctx, jobs[0])
}

func (w *Worker) processLeased(ctx context.Context, job *models.Job) error {
	if job == nil {
		return errors.New("nil leased job")
	}
	if job.MaxAttempts > 0 && job.Attempts > job.MaxAttempts {
		return w.abandonExhausted(ctx, job)
	}
	runCtx, cancel := context.WithCancel(ctx)
	progress := &progressState{}
	progress.Set(1, "leased")
	if err := w.db.HeartbeatJob(runCtx, job.ID, w.opts.WorkerID, w.opts.LeaseDuration, 1, "leased"); err != nil {
		cancel()
		return err
	}
	heartbeatDone := make(chan error, 1)
	go func() {
		heartbeatDone <- w.heartbeatLoop(runCtx, cancel, job, progress)
	}()

	err := w.processJob(runCtx, job, progress)
	cancel()
	heartbeatErr := <-heartbeatDone
	if err == nil && heartbeatErr != nil && !errors.Is(heartbeatErr, context.Canceled) {
		err = heartbeatErr
	}

	completionCtx, completionCancel := context.WithTimeout(context.WithoutCancel(ctx), w.opts.CompletionTimeout)
	defer completionCancel()
	if err != nil {
		failure := boundedError(err)
		var failErr error
		if isPermanentFailure(err) {
			failErr = w.db.FailJobPermanently(completionCtx, job.ID, w.opts.WorkerID, failure)
		} else {
			failErr = w.db.FailJob(completionCtx, job.ID, w.opts.WorkerID, failure, w.opts.RetryBackoff, w.opts.MaxRetryBackoff)
		}
		if failErr != nil {
			return errors.Join(err, fmt.Errorf("record job failure: %w", failErr))
		}
		return err
	}
	current, getErr := w.db.GetJob(completionCtx, job.ID)
	if getErr != nil {
		return getErr
	}
	if current.CancelRequested {
		if failErr := w.db.FailJob(completionCtx, job.ID, w.opts.WorkerID, ErrJobCancelled.Error(), w.opts.RetryBackoff, w.opts.MaxRetryBackoff); failErr != nil {
			return failErr
		}
		return ErrJobCancelled
	}
	if err := w.db.CompleteJob(completionCtx, job.ID, w.opts.WorkerID); err != nil {
		return err
	}
	return nil
}

// abandonExhausted handles a job leased again after its final attempt's lease
// expired, which only happens when the worker running it died (restart,
// deploy, or an out-of-memory kill) before recording a result.
func (w *Worker) abandonExhausted(ctx context.Context, job *models.Job) error {
	exhausted := fmt.Errorf("%w: the last of %d attempts stopped without recording a result; the worker was restarted or killed, for example by running out of memory", ErrAttemptsExhausted, job.MaxAttempts)
	if job.LastError != "" {
		exhausted = fmt.Errorf("%w; earlier error: %s", exhausted, job.LastError)
	}
	failure := boundedError(exhausted)
	completionCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), w.opts.CompletionTimeout)
	defer cancel()
	errs := []error{exhausted}
	if job.Type == models.JobConvert {
		if err := w.abandonConversion(completionCtx, job, failure); err != nil {
			errs = append(errs, fmt.Errorf("record abandoned conversion: %w", err))
		}
	}
	if err := w.db.FailJobPermanently(completionCtx, job.ID, w.opts.WorkerID, failure); err != nil {
		errs = append(errs, fmt.Errorf("record job failure: %w", err))
	}
	return errors.Join(errs...)
}

func (w *Worker) heartbeatLoop(ctx context.Context, cancel context.CancelFunc, job *models.Job, progress *progressState) error {
	ticker := time.NewTicker(w.opts.HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			current, err := w.db.GetJob(ctx, job.ID)
			if err != nil {
				cancel()
				return err
			}
			if current.CancelRequested || current.Status != models.JobRunning || current.LeaseOwner != w.opts.WorkerID {
				cancel()
				return ErrJobCancelled
			}
			value, message := progress.Get()
			if err := w.db.HeartbeatJob(ctx, job.ID, w.opts.WorkerID, w.opts.LeaseDuration, value, message); err != nil {
				cancel()
				return err
			}
		}
	}
}

func (w *Worker) processJob(ctx context.Context, job *models.Job, progress *progressState) error {
	if job.CancelRequested {
		return ErrJobCancelled
	}
	switch job.Type {
	case models.JobConvert:
		return w.processConversion(ctx, job, progress)
	case models.JobBackup:
		return w.processBackup(ctx, job, progress)
	case models.JobPublish, models.JobUnpublish:
		return w.processPublication(ctx, job, progress)
	default:
		return fmt.Errorf("unsupported job type %q", job.Type)
	}
}

func (w *Worker) scheduleLoop(ctx context.Context) {
	for {
		if _, err := w.SchedulePublications(ctx); err != nil && ctx.Err() == nil {
			w.logger.Printf("[worker] publication scheduling failed: %v", err)
		}
		if !waitContext(ctx, w.opts.ScheduleInterval) {
			return
		}
	}
}

func waitContext(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// isPermanentFailure reports conversion errors caused by the input document or
// configured limits. Retrying them reruns the whole conversion for the same
// result, so the job is failed immediately instead. That includes the
// conversion time limit: it already scales with page count, and retrying a
// document that exceeded it only repeats the wait several times over.
func isPermanentFailure(err error) bool {
	for _, kind := range []error{
		converter.ErrUnsupportedFormat,
		converter.ErrEncryptedPDF,
		converter.ErrQuotaExceeded,
		converter.ErrInvalidOptions,
		converter.ErrTimeout,
	} {
		if errors.Is(err, kind) {
			return true
		}
	}
	return false
}

func boundedError(err error) string {
	if err == nil {
		return ""
	}
	message := err.Error()
	if len(message) > 4096 {
		return message[:4096]
	}
	return message
}
