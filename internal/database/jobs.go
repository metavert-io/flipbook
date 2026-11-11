package database

import (
	"context"
	"errors"
	"time"

	"github.com/metavert-io/flipbook/internal/models"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func (d *DB) EnqueueJob(ctx context.Context, job *models.Job) error {
	if job.WorkspaceID == "" || job.Type == "" {
		return errors.New("job workspace and type are required")
	}
	now := time.Now().UTC()
	if job.ID == "" {
		job.ID = newID()
	}
	if job.Status == "" {
		job.Status = models.JobQueued
	}
	if job.RunAt.IsZero() {
		job.RunAt = now
	}
	if job.MaxAttempts == 0 {
		job.MaxAttempts = 5
	}
	if job.MaxAttempts < 1 {
		return errors.New("job max attempts must be positive")
	}
	if job.CreatedAt.IsZero() {
		job.CreatedAt = now
	}
	job.UpdatedAt = now
	if job.IdempotencyKey != "" {
		filter := bson.M{"workspace_id": job.WorkspaceID, "idempotency_key": job.IdempotencyKey}
		result, err := d.jobs.UpdateOne(ctx, filter, bson.M{"$setOnInsert": job}, options.UpdateOne().SetUpsert(true))
		if mongo.IsDuplicateKeyError(err) || (err == nil && result.MatchedCount > 0) {
			existing, getErr := d.GetJobByIdempotencyKey(ctx, job.WorkspaceID, job.IdempotencyKey)
			if getErr != nil {
				return getErr
			}
			*job = *existing
			return nil
		}
		return err
	}
	_, err := d.jobs.InsertOne(ctx, job)
	return err
}

func (d *DB) GetJob(ctx context.Context, jobID string) (*models.Job, error) {
	var job models.Job
	if err := d.jobs.FindOne(ctx, bson.M{"_id": jobID}).Decode(&job); err != nil {
		return nil, err
	}
	return &job, nil
}

func (d *DB) GetJobByIdempotencyKey(ctx context.Context, workspaceID, key string) (*models.Job, error) {
	var job models.Job
	if err := d.jobs.FindOne(ctx, bson.M{"workspace_id": workspaceID, "idempotency_key": key}).Decode(&job); err != nil {
		return nil, err
	}
	return &job, nil
}

func (d *DB) LeaseJobs(ctx context.Context, workerID string, types []models.JobType, limit int, leaseDuration time.Duration) ([]*models.Job, error) {
	if workerID == "" || leaseDuration <= 0 {
		return nil, errors.New("worker id and positive lease duration are required")
	}
	if limit == 0 {
		limit = 1
	}
	if limit < 1 || limit > 100 {
		return nil, errors.New("job lease limit must be between 1 and 100")
	}
	leased := make([]*models.Job, 0, limit)
	for len(leased) < limit {
		now := time.Now().UTC()
		statusOr := bson.A{
			bson.M{"status": bson.M{"$in": bson.A{models.JobQueued, models.JobRetry}}, "cancel_requested": false},
			bson.M{"status": models.JobRunning, "lease_expires_at": bson.M{"$lte": now}},
		}
		query := bson.M{"run_at": bson.M{"$lte": now}, "$or": statusOr}
		if len(types) > 0 {
			query["type"] = bson.M{"$in": types}
		}
		update := bson.M{
			"$set": bson.M{
				"status": models.JobRunning, "lease_owner": workerID,
				"lease_expires_at": now.Add(leaseDuration), "heartbeat_at": now,
				"started_at": now, "updated_at": now,
			},
			"$inc": bson.M{"attempts": 1},
		}
		opts := options.FindOneAndUpdate().SetSort(bson.D{{Key: "priority", Value: -1}, {Key: "created_at", Value: 1}}).SetReturnDocument(options.After)
		var job models.Job
		err := d.jobs.FindOneAndUpdate(ctx, query, update, opts).Decode(&job)
		if err == mongo.ErrNoDocuments {
			break
		}
		if err != nil {
			return leased, err
		}
		if job.CancelRequested {
			_, _ = d.jobs.UpdateByID(ctx, job.ID, bson.M{
				"$set":   bson.M{"status": models.JobCancelled, "completed_at": time.Now().UTC(), "updated_at": time.Now().UTC()},
				"$unset": bson.M{"lease_owner": "", "lease_expires_at": "", "heartbeat_at": ""},
			})
			continue
		}
		// A job past MaxAttempts is still handed out: its last attempt died
		// with the lease held, and only the worker can close out the job's
		// side effects (such as a flipbook left in "converting").
		leased = append(leased, &job)
	}
	return leased, nil
}

func (d *DB) HeartbeatJob(ctx context.Context, jobID, workerID string, leaseDuration time.Duration, progress int, message string) error {
	if leaseDuration <= 0 {
		return errors.New("lease duration must be positive")
	}
	if progress < 0 {
		progress = 0
	}
	if progress > 100 {
		progress = 100
	}
	now := time.Now().UTC()
	result, err := d.jobs.UpdateOne(ctx, bson.M{"_id": jobID, "status": models.JobRunning, "lease_owner": workerID}, bson.M{"$set": bson.M{
		"heartbeat_at": now, "lease_expires_at": now.Add(leaseDuration),
		"progress": progress, "progress_message": message, "updated_at": now,
	}})
	if err == nil && result.MatchedCount == 0 {
		return mongo.ErrNoDocuments
	}
	return err
}

func (d *DB) CompleteJob(ctx context.Context, jobID, workerID string) error {
	now := time.Now().UTC()
	result, err := d.jobs.UpdateOne(ctx, bson.M{"_id": jobID, "status": models.JobRunning, "lease_owner": workerID}, bson.M{
		"$set":   bson.M{"status": models.JobSucceeded, "progress": 100, "completed_at": now, "updated_at": now},
		"$unset": bson.M{"lease_owner": "", "lease_expires_at": "", "heartbeat_at": ""},
	})
	if err == nil && result.MatchedCount == 0 {
		return mongo.ErrNoDocuments
	}
	return err
}

func (d *DB) FailJob(ctx context.Context, jobID, workerID, failure string, baseBackoff, maxBackoff time.Duration) error {
	return d.failJob(ctx, jobID, workerID, failure, baseBackoff, maxBackoff, false)
}

// FailJobPermanently records a failure that retrying cannot fix (bad input,
// quota exceeded) and marks the job dead without spending remaining attempts.
func (d *DB) FailJobPermanently(ctx context.Context, jobID, workerID, failure string) error {
	return d.failJob(ctx, jobID, workerID, failure, 0, 0, true)
}

func (d *DB) failJob(ctx context.Context, jobID, workerID, failure string, baseBackoff, maxBackoff time.Duration, permanent bool) error {
	job, err := d.GetJob(ctx, jobID)
	if err != nil {
		return err
	}
	if job.Status != models.JobRunning || job.LeaseOwner != workerID {
		return ErrConflict
	}
	now := time.Now().UTC()
	status := models.JobRetry
	runAt := now.Add(models.RetryDelay(job.Attempts, baseBackoff, maxBackoff))
	if job.CancelRequested {
		status = models.JobCancelled
		runAt = now
	} else if permanent || job.Attempts >= job.MaxAttempts {
		status = models.JobDead
		runAt = now
	}
	set := bson.M{
		"status": status, "run_at": runAt, "last_error": failure, "updated_at": now,
	}
	if status == models.JobDead || status == models.JobCancelled {
		set["completed_at"] = now
	}
	result, err := d.jobs.UpdateOne(ctx, bson.M{"_id": jobID, "status": models.JobRunning, "lease_owner": workerID}, bson.M{
		"$set": set, "$unset": bson.M{"lease_owner": "", "lease_expires_at": "", "heartbeat_at": ""},
	})
	if err == nil && result.MatchedCount == 0 {
		return ErrConflict
	}
	return err
}

func (d *DB) CancelJob(ctx context.Context, jobID string) error {
	now := time.Now().UTC()
	result, err := d.jobs.UpdateOne(ctx, bson.M{"_id": jobID, "status": bson.M{"$in": bson.A{models.JobQueued, models.JobRetry}}}, bson.M{"$set": bson.M{
		"status": models.JobCancelled, "cancel_requested": true, "completed_at": now, "updated_at": now,
	}})
	if err != nil {
		return err
	}
	if result.MatchedCount == 0 {
		_, err = d.jobs.UpdateOne(ctx, bson.M{"_id": jobID, "status": models.JobRunning}, bson.M{"$set": bson.M{
			"cancel_requested": true, "updated_at": now,
		}})
	}
	return err
}

func (d *DB) RetryJob(ctx context.Context, jobID string) error {
	now := time.Now().UTC()
	result, err := d.jobs.UpdateOne(ctx, bson.M{"_id": jobID, "status": bson.M{"$in": bson.A{models.JobDead, models.JobCancelled}}}, bson.M{"$set": bson.M{
		"status": models.JobQueued, "run_at": now, "attempts": 0, "cancel_requested": false,
		"last_error": "", "completed_at": nil, "updated_at": now,
	}})
	if err == nil && result.MatchedCount == 0 {
		return ErrConflict
	}
	return err
}

type JobFilter struct {
	WorkspaceID string
	FlipbookID  string
	Type        models.JobType
	Status      models.JobStatus
	Limit       int64
}

func (d *DB) ListJobs(ctx context.Context, filter JobFilter) ([]*models.Job, error) {
	query := bson.M{}
	if filter.WorkspaceID != "" {
		query["workspace_id"] = filter.WorkspaceID
	}
	if filter.FlipbookID != "" {
		query["flipbook_id"] = filter.FlipbookID
	}
	if filter.Type != "" {
		query["type"] = filter.Type
	}
	if filter.Status != "" {
		query["status"] = filter.Status
	}
	limit := filter.Limit
	if limit == 0 {
		limit = 100
	}
	if limit < 1 || limit > 1000 {
		return nil, errors.New("job limit must be between 1 and 1000")
	}
	opts := options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}}).SetLimit(limit)
	return decodeCursor[models.Job](ctx, d.jobs, query, opts)
}

func (d *DB) GetQueueMetrics(ctx context.Context, workspaceID string) (models.QueueMetrics, error) {
	var metrics models.QueueMetrics
	query := bson.M{}
	if workspaceID != "" {
		query["workspace_id"] = workspaceID
	}
	count := func(status models.JobStatus) (int64, error) {
		q := bson.M{"status": status}
		for key, value := range query {
			q[key] = value
		}
		return d.jobs.CountDocuments(ctx, q)
	}
	var err error
	if metrics.Queued, err = count(models.JobQueued); err != nil {
		return metrics, err
	}
	if metrics.Running, err = count(models.JobRunning); err != nil {
		return metrics, err
	}
	if metrics.Retry, err = count(models.JobRetry); err != nil {
		return metrics, err
	}
	if metrics.Dead, err = count(models.JobDead); err != nil {
		return metrics, err
	}
	q := bson.M{"status": bson.M{"$in": bson.A{models.JobQueued, models.JobRetry}}}
	for key, value := range query {
		q[key] = value
	}
	var oldest models.Job
	err = d.jobs.FindOne(ctx, q, options.FindOne().SetSort(bson.D{{Key: "run_at", Value: 1}})).Decode(&oldest)
	if err == nil {
		metrics.OldestQueuedAt = &oldest.RunAt
	} else if err != mongo.ErrNoDocuments {
		return metrics, err
	}
	return metrics, nil
}
