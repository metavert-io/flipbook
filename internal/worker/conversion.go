package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/metavert-io/flipbook/internal/converter"
	"github.com/metavert-io/flipbook/internal/models"
	"github.com/metavert-io/flipbook/internal/storage"
)

type sourceInfo struct {
	Path   string
	Size   int64
	SHA256 string
}

func inspectSource(path string) sourceInfo {
	info := sourceInfo{Path: path}
	stat, err := os.Stat(path)
	if err != nil || !stat.Mode().IsRegular() {
		return info
	}
	info.Size = stat.Size()
	file, err := os.Open(path)
	if err != nil {
		return info
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err == nil {
		info.SHA256 = hex.EncodeToString(hash.Sum(nil))
	}
	return info
}

func (w *Worker) processConversion(ctx context.Context, job *models.Job, progress *progressState) (returnErr error) {
	flipbook, err := w.db.GetFlipbookInWorkspace(ctx, job.WorkspaceID, job.FlipbookID)
	if err != nil {
		return err
	}
	revision, err := w.db.GetRevision(ctx, job.WorkspaceID, job.RevisionID)
	if err != nil {
		return err
	}
	if revision.FlipbookID != flipbook.ID {
		return errors.New("conversion revision does not belong to flipbook")
	}
	liveRevision := w.liveRevision(ctx, flipbook)
	activated := false
	defer func() {
		if returnErr == nil || activated {
			return
		}
		w.markConversionFailed(context.WithoutCancel(ctx), flipbook.ID, revision.ID, liveRevision != nil, boundedError(returnErr))
	}()

	if liveRevision == nil {
		if err := w.db.UpdateStatus(flipbook.ID, models.StatusConverting, ""); err != nil {
			return err
		}
	}
	w.setRevisionProgress(ctx, revision.ID, progress, 2, "preparing conversion")

	revisionDir := w.storage.RevisionDir(flipbook.ID, revision.Number)
	manifest, installed, err := w.installedManifest(flipbook.ID, revision.Number)
	if err != nil {
		return err
	}
	if !installed {
		manifest, revisionDir, err = w.renderRevision(ctx, job, flipbook, revision, progress)
		if err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	w.setRevisionProgress(ctx, revision.ID, progress, 85, "validating revision")
	if err := converter.ValidateArtifacts(revisionDir, manifest); err != nil {
		return err
	}
	warnings := make([]string, 0, len(manifest.Warnings))
	for _, warning := range manifest.Warnings {
		message := warning.Code + ": " + warning.Message
		if warning.Page > 0 {
			message = fmt.Sprintf("page %d: %s", warning.Page, message)
		}
		warnings = append(warnings, message)
	}
	manifestKey := fmt.Sprintf("%s/revisions/%d/manifest.json", flipbook.ID, revision.Number)
	if err := w.db.MarkRevisionReady(ctx, revision.ID, manifestKey, manifest.PageCount, warnings); err != nil {
		return err
	}

	w.setRevisionProgress(ctx, revision.ID, progress, 90, "syncing revision assets")
	if _, err := w.storage.SyncRevision(ctx, flipbook.ID, revision.Number, true); err != nil {
		return err
	}

	latest, err := w.db.GetFlipbookInWorkspace(ctx, job.WorkspaceID, flipbook.ID)
	if err != nil {
		return err
	}
	if latest.CurrentRevisionID != flipbook.CurrentRevisionID && latest.CurrentRevisionID != revision.ID {
		progress.Set(100, "a newer revision is already active")
		return nil
	}
	previousNumber, previousErr := w.storage.CurrentRevision(flipbook.ID)
	if previousErr != nil {
		return previousErr
	}
	w.setRevisionProgress(ctx, revision.ID, progress, 96, "activating revision")
	if err := w.storage.ActivateRevision(flipbook.ID, revision.Number); err != nil {
		return err
	}
	if err := w.db.ActivateRevision(ctx, flipbook.WorkspaceID, flipbook.ID, revision.ID); err != nil {
		current, getErr := w.db.GetFlipbookInWorkspace(context.WithoutCancel(ctx), flipbook.WorkspaceID, flipbook.ID)
		if getErr == nil && current.CurrentRevisionID == revision.ID {
			activated = true
			return err
		}
		if previousNumber > 0 && previousNumber != revision.Number {
			if rollbackErr := w.storage.ActivateRevision(flipbook.ID, previousNumber); rollbackErr != nil {
				w.logger.Printf("[worker] rollback storage revision for %s: %v", flipbook.ID, rollbackErr)
			}
		}
		return err
	}
	activated = true
	if err := w.db.UpdateConversionResult(flipbook.ID, manifest.PageCount, manifest.Width, manifest.Height); err != nil {
		return err
	}

	originalPath := ""
	if manifest.Original != "" {
		originalPath = filepath.Join(revisionDir, filepath.FromSlash(manifest.Original))
	}
	if originalPath != "" {
		source := inspectSource(originalPath)
		if source.SHA256 == "" {
			return fmt.Errorf("inspect installed original: %s", originalPath)
		}
		_, err = w.EnqueueBackup(context.WithoutCancel(ctx), BackupRequest{
			WorkspaceID: flipbook.WorkspaceID, FlipbookID: flipbook.ID, RevisionID: revision.ID,
			SourcePath: originalPath, Filename: revision.SourceFilename,
			SHA256: source.SHA256, Size: source.Size,
		})
		if err != nil {
			return err
		}
	}
	progress.Set(100, "conversion complete")
	return nil
}

// markConversionFailed records a failed conversion on the revision and the
// flipbook. A flipbook that still has a live revision stays ready and only
// carries the error message.
func (w *Worker) markConversionFailed(ctx context.Context, flipbookID, revisionID string, keepLive bool, message string) {
	if err := w.db.MarkRevisionFailed(ctx, revisionID, message); err != nil {
		w.logger.Printf("[worker] mark revision %s failed: %v", revisionID, err)
	}
	status := models.StatusError
	if keepLive {
		status = models.StatusReady
	}
	if err := w.db.UpdateStatus(flipbookID, status, message); err != nil {
		w.logger.Printf("[worker] update flipbook %s failure status: %v", flipbookID, err)
	}
}

// abandonConversion closes out a conversion whose final attempt died without
// recording a result, so the publication shows an error instead of staying in
// "converting" with no worker behind it.
func (w *Worker) abandonConversion(ctx context.Context, job *models.Job, message string) error {
	flipbook, err := w.db.GetFlipbookInWorkspace(ctx, job.WorkspaceID, job.FlipbookID)
	if err != nil {
		return err
	}
	if flipbook.Status == models.StatusReady && flipbook.CurrentRevisionID == job.RevisionID {
		// The attempt activated its revision before it died; nothing failed.
		return nil
	}
	w.markConversionFailed(ctx, flipbook.ID, job.RevisionID, flipbook.Status == models.StatusReady, message)
	return nil
}

func (w *Worker) liveRevision(_ context.Context, flipbook *models.Flipbook) *models.Revision {
	if flipbook.Status != models.StatusReady {
		return nil
	}
	return &models.Revision{ID: flipbook.CurrentRevisionID}
}

func (w *Worker) installedManifest(flipbookID string, number int) (*converter.Manifest, bool, error) {
	dir := w.storage.RevisionDir(flipbookID, number)
	manifest, err := converter.LoadManifest(dir)
	if err == nil {
		if err := storage.ValidateRevisionArtifacts(dir); err == nil {
			return manifest, true, nil
		}
	}
	if _, statErr := os.Stat(dir); errors.Is(statErr, os.ErrNotExist) {
		return nil, false, nil
	}
	current, currentErr := w.storage.CurrentRevision(flipbookID)
	if currentErr != nil {
		return nil, false, currentErr
	}
	if current == number {
		if err != nil {
			return nil, false, err
		}
		return nil, false, storage.ValidateRevisionArtifacts(dir)
	}
	if err := w.storage.DeleteRevision(flipbookID, number); err != nil && !errors.Is(err, storage.ErrRevisionMissing) {
		return nil, false, err
	}
	return nil, false, nil
}

func (w *Worker) renderRevision(ctx context.Context, job *models.Job, flipbook *models.Flipbook, revision *models.Revision, progress *progressState) (*converter.Manifest, string, error) {
	stagingDir, err := w.storage.CreateRevisionStaging(flipbook.ID)
	if err != nil {
		return nil, "", err
	}
	defer os.RemoveAll(stagingDir)
	w.setRevisionProgress(ctx, revision.ID, progress, 5, "resolving source")
	sourcePath := payloadString(job.Payload, "source_path")
	if sourcePath == "" {
		sourcePath = w.storage.OriginalPath(flipbook.ID, fileExtension(revision.SourceFilename))
	}
	expectedChecksum := payloadString(job.Payload, "source_checksum")
	expectedSize := payloadInt64(job.Payload, "source_size")
	source, err := w.resolveSource(ctx, flipbook, revision, sourcePath, stagingDir, expectedChecksum, expectedSize)
	if err != nil {
		return nil, "", err
	}
	w.setRevisionProgress(ctx, revision.ID, progress, 10, "rendering pages")
	convertCtx := converter.WithProgress(ctx, w.conversionProgress(ctx, revision.ID, progress))
	if _, err := w.conv.ConvertStaged(convertCtx, source.Path, stagingDir); err != nil {
		return nil, "", err
	}
	progress.Set(75, "rendering complete")
	manifest, err := converter.LoadManifest(stagingDir)
	if err != nil {
		return nil, "", err
	}
	if err := converter.ValidateArtifacts(stagingDir, manifest); err != nil {
		return nil, "", err
	}
	w.setRevisionProgress(ctx, revision.ID, progress, 80, "installing immutable revision")
	revisionDir, err := w.storage.InstallRevision(flipbook.ID, stagingDir, revision.Number)
	if errors.Is(err, storage.ErrRevisionExists) {
		revisionDir = w.storage.RevisionDir(flipbook.ID, revision.Number)
		manifest, err = converter.LoadManifest(revisionDir)
		if err == nil {
			err = storage.ValidateRevisionArtifacts(revisionDir)
		}
	}
	if err != nil {
		return nil, "", err
	}
	return manifest, revisionDir, nil
}

func (w *Worker) resolveSource(ctx context.Context, flipbook *models.Flipbook, revision *models.Revision, sourcePath, stagingDir, expectedChecksum string, expectedSize int64) (sourceInfo, error) {
	source := inspectSource(sourcePath)
	if source.SHA256 != "" && sourceMatches(source, expectedChecksum, expectedSize) {
		return source, nil
	}
	gridfsID := revision.OriginalGridFSFileID
	if gridfsID == "" && flipbook.CurrentRevisionID == revision.ID {
		gridfsID = flipbook.GridFSFileID
	}
	if gridfsID == "" {
		if source.SHA256 == "" {
			return sourceInfo{}, fmt.Errorf("source document is unavailable: %s", sourcePath)
		}
		return sourceInfo{}, errors.New("source document differs from the queued revision")
	}
	extension := fileExtension(revision.SourceFilename)
	restorePath := filepath.Join(stagingDir, "source"+extension)
	restored, err := w.restoreGridFSSource(ctx, gridfsID, restorePath)
	if err != nil {
		return sourceInfo{}, err
	}
	if !sourceMatches(restored, expectedChecksum, expectedSize) {
		return sourceInfo{}, errors.New("GridFS source does not match the queued revision")
	}
	return restored, nil
}

func (w *Worker) restoreGridFSSource(ctx context.Context, fileID, path string) (sourceInfo, error) {
	file, err := os.Create(path)
	if err != nil {
		return sourceInfo{}, err
	}
	hash := sha256.New()
	written, downloadErr := w.db.DownloadFromGridFS(ctx, fileID, io.MultiWriter(file, hash))
	closeErr := file.Close()
	if downloadErr != nil {
		return sourceInfo{}, downloadErr
	}
	if closeErr != nil {
		return sourceInfo{}, closeErr
	}
	return sourceInfo{Path: path, Size: written, SHA256: hex.EncodeToString(hash.Sum(nil))}, nil
}

func sourceMatches(source sourceInfo, checksum string, size int64) bool {
	if source.SHA256 == "" {
		return false
	}
	if checksum != "" && !strings.EqualFold(source.SHA256, checksum) {
		return false
	}
	return size <= 0 || source.Size == size
}

// Converter progress is mapped onto the 10-75% band between "rendering pages"
// and "rendering complete"; page rendering takes most of it.
const (
	renderProgressStart = 10
	renderProgressEnd   = 70
	textProgress        = 72
)

// conversionProgress publishes converter progress to the job heartbeat and
// the revision. The revision is written only when the percentage changes so a
// long document costs at most one write per percent.
func (w *Worker) conversionProgress(ctx context.Context, revisionID string, progress *progressState) converter.ProgressFunc {
	lastPersisted := renderProgressStart
	return func(report converter.Progress) {
		value, message := renderProgressStart, ""
		switch report.Stage {
		case converter.ProgressConvertDocument:
			message = "converting document to PDF"
		case converter.ProgressRenderPages:
			if report.Total > 0 {
				value += (renderProgressEnd - renderProgressStart) * report.Done / report.Total
			}
			message = fmt.Sprintf("rendered %d of %d pages", report.Done, report.Total)
		case converter.ProgressExtractText:
			value, message = textProgress, "extracting text and links"
		default:
			return
		}
		if value == lastPersisted {
			progress.Set(value, message)
			return
		}
		lastPersisted = value
		w.setRevisionProgress(ctx, revisionID, progress, value, message)
	}
}

func (w *Worker) setRevisionProgress(ctx context.Context, revisionID string, progress *progressState, value int, message string) {
	progress.Set(value, message)
	if err := w.db.UpdateRevisionProgress(ctx, revisionID, value, message); err != nil {
		w.logger.Printf("[worker] update revision %s progress: %v", revisionID, err)
	}
}
