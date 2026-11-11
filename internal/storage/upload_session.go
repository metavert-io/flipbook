package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Upload sessions let a browser send a large document as a sequence of small
// requests. A single multi-hundred-megabyte request outlives the request read
// timeout of common reverse proxies (Traefik v3 cuts request bodies off after
// 60 seconds by default), and one dropped connection would restart the whole
// transfer. With sessions, each chunk is a short request that can be retried,
// and the assembled file is handed to the normal upload path once complete.

const (
	uploadSessionsDirName = ".upload-sessions"
	uploadSessionMetaFile = "session.json"
	uploadSessionDataFile = "data"
	uploadSessionDoneFile = "complete"
)

var (
	ErrUploadSessionNotFound = errors.New("upload session not found")
	ErrUploadOffsetMismatch  = errors.New("upload chunk offset does not match the bytes received")
	ErrUploadIncomplete      = errors.New("upload session is incomplete")
	ErrUploadSessionComplete = errors.New("upload session is already complete")
)

// uploadSessionLocks serializes chunk writes per session directory. A client
// retrying a chunk after a timeout can race the original request; the lock
// is held only while appending an already-received chunk, never while reading
// from the network.
var uploadSessionLocks sync.Map

// UploadSession describes a resumable upload. Received is derived from the
// bytes on disk and is not persisted.
type UploadSession struct {
	ID          string    `json:"id"`
	WorkspaceID string    `json:"workspace_id"`
	UserID      string    `json:"user_id"`
	Filename    string    `json:"filename"`
	Size        int64     `json:"size"`
	CreatedAt   time.Time `json:"created_at"`
	Received    int64     `json:"-"`
}

func (s *Storage) uploadSessionsRoot() string {
	return filepath.Join(s.BaseDir, uploadSessionsDirName)
}

func (s *Storage) uploadSessionDir(id string) (string, error) {
	if err := validateID(id); err != nil {
		return "", ErrUploadSessionNotFound
	}
	return filepath.Join(s.uploadSessionsRoot(), id), nil
}

func uploadSessionLock(dir string) *sync.Mutex {
	lock, _ := uploadSessionLocks.LoadOrStore(dir, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

// CreateUploadSession reserves an empty session. The caller supplies the ID,
// owner, filename, declared size and creation time.
func (s *Storage) CreateUploadSession(session UploadSession) (UploadSession, error) {
	dir, err := s.uploadSessionDir(session.ID)
	if err != nil {
		return UploadSession{}, fmt.Errorf("%w: invalid session ID", ErrInvalidUpload)
	}
	if session.Size <= 0 {
		return UploadSession{}, fmt.Errorf("%w: size must be positive", ErrInvalidUpload)
	}
	if _, err := uploadExtension(session.Filename); err != nil {
		return UploadSession{}, err
	}
	if err := os.MkdirAll(s.uploadSessionsRoot(), 0755); err != nil {
		return UploadSession{}, fmt.Errorf("create upload sessions directory: %w", err)
	}
	if err := os.Mkdir(dir, 0755); err != nil {
		return UploadSession{}, fmt.Errorf("create upload session: %w", err)
	}
	created := false
	defer func() {
		if !created {
			_ = os.RemoveAll(dir)
		}
	}()
	encoded, err := json.Marshal(session)
	if err != nil {
		return UploadSession{}, err
	}
	if err := writeFileAtomic(filepath.Join(dir, uploadSessionMetaFile), encoded); err != nil {
		return UploadSession{}, err
	}
	data, err := os.OpenFile(filepath.Join(dir, uploadSessionDataFile), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return UploadSession{}, fmt.Errorf("create upload session data: %w", err)
	}
	if err := data.Close(); err != nil {
		return UploadSession{}, err
	}
	created = true
	session.Received = 0
	return session, nil
}

// GetUploadSession returns the session and the number of bytes received.
func (s *Storage) GetUploadSession(id string) (UploadSession, error) {
	dir, err := s.uploadSessionDir(id)
	if err != nil {
		return UploadSession{}, err
	}
	return readUploadSession(dir)
}

func readUploadSession(dir string) (UploadSession, error) {
	encoded, err := os.ReadFile(filepath.Join(dir, uploadSessionMetaFile))
	if errors.Is(err, os.ErrNotExist) {
		return UploadSession{}, ErrUploadSessionNotFound
	}
	if err != nil {
		return UploadSession{}, err
	}
	var session UploadSession
	if err := json.Unmarshal(encoded, &session); err != nil {
		return UploadSession{}, fmt.Errorf("read upload session: %w", err)
	}
	if info, err := os.Stat(filepath.Join(dir, uploadSessionDataFile)); err == nil {
		session.Received = info.Size()
	} else if _, doneErr := os.Stat(filepath.Join(dir, uploadSessionDoneFile)); doneErr == nil {
		session.Received = session.Size
	} else {
		return UploadSession{}, ErrUploadSessionNotFound
	}
	return session, nil
}

// WriteUploadChunk stores the bytes of r at offset. The offset may not be past
// the bytes already received; an offset before that point overwrites from
// there, which makes a retried chunk idempotent. maxChunk bounds a single
// request. The returned session reports the new received count.
func (s *Storage) WriteUploadChunk(ctx context.Context, id string, offset int64, r io.Reader, maxChunk int64) (UploadSession, error) {
	dir, err := s.uploadSessionDir(id)
	if err != nil {
		return UploadSession{}, err
	}
	session, err := readUploadSession(dir)
	if err != nil {
		return UploadSession{}, err
	}
	if offset < 0 || offset > session.Size {
		return session, fmt.Errorf("%w: offset %d is outside the declared size %d", ErrInvalidUpload, offset, session.Size)
	}
	limit := session.Size - offset
	if maxChunk > 0 && maxChunk < limit {
		limit = maxChunk
	}

	// Receive the chunk before taking the lock so a slow or stalled request
	// cannot block a retry of the same chunk.
	staged, err := os.CreateTemp(dir, ".chunk-*")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return session, ErrUploadSessionNotFound
		}
		return session, fmt.Errorf("stage upload chunk: %w", err)
	}
	stagedPath := staged.Name()
	defer os.Remove(stagedPath)
	written, copyErr := io.Copy(staged, io.LimitReader(&contextReader{ctx: ctx, reader: r}, limit+1))
	if copyErr != nil {
		_ = staged.Close()
		return session, fmt.Errorf("receive upload chunk: %w", copyErr)
	}
	if written > limit {
		_ = staged.Close()
		return session, fmt.Errorf("%w: chunk at offset %d exceeds %d bytes", ErrUploadTooLarge, offset, limit)
	}
	if _, err := staged.Seek(0, io.SeekStart); err != nil {
		_ = staged.Close()
		return session, err
	}
	defer staged.Close()

	lock := uploadSessionLock(dir)
	lock.Lock()
	defer lock.Unlock()
	data, err := os.OpenFile(filepath.Join(dir, uploadSessionDataFile), os.O_WRONLY, 0)
	if errors.Is(err, os.ErrNotExist) {
		if _, doneErr := os.Stat(filepath.Join(dir, uploadSessionDoneFile)); doneErr == nil {
			return session, ErrUploadSessionComplete
		}
		return session, ErrUploadSessionNotFound
	}
	if err != nil {
		return session, err
	}
	info, err := data.Stat()
	if err != nil {
		_ = data.Close()
		return session, err
	}
	if offset > info.Size() {
		_ = data.Close()
		session.Received = info.Size()
		return session, fmt.Errorf("%w: offset %d, received %d", ErrUploadOffsetMismatch, offset, info.Size())
	}
	if err := data.Truncate(offset); err != nil {
		_ = data.Close()
		return session, err
	}
	if _, err := data.Seek(offset, io.SeekStart); err != nil {
		_ = data.Close()
		return session, err
	}
	if _, err := io.Copy(data, staged); err != nil {
		_ = data.Close()
		return session, fmt.Errorf("write upload chunk: %w", err)
	}
	if err := data.Close(); err != nil {
		return session, err
	}
	session.Received = offset + written
	return session, nil
}

// CompleteUploadSession seals a fully received session and opens the
// assembled document. Later chunk writes fail with ErrUploadSessionComplete.
// Calling it again on a sealed session reopens the same document.
func (s *Storage) CompleteUploadSession(id string) (UploadSession, *os.File, error) {
	dir, err := s.uploadSessionDir(id)
	if err != nil {
		return UploadSession{}, nil, err
	}
	session, err := readUploadSession(dir)
	if err != nil {
		return UploadSession{}, nil, err
	}
	dataPath := filepath.Join(dir, uploadSessionDataFile)
	donePath := filepath.Join(dir, uploadSessionDoneFile)
	lock := uploadSessionLock(dir)
	lock.Lock()
	if info, statErr := os.Stat(dataPath); statErr == nil {
		if info.Size() != session.Size {
			lock.Unlock()
			session.Received = info.Size()
			return session, nil, fmt.Errorf("%w: received %d of %d bytes", ErrUploadIncomplete, info.Size(), session.Size)
		}
		if err := os.Rename(dataPath, donePath); err != nil {
			lock.Unlock()
			return session, nil, err
		}
	}
	lock.Unlock()
	file, err := os.Open(donePath)
	if errors.Is(err, os.ErrNotExist) {
		return session, nil, ErrUploadSessionNotFound
	}
	if err != nil {
		return session, nil, err
	}
	info, err := file.Stat()
	if err != nil || info.Size() != session.Size {
		_ = file.Close()
		if err == nil {
			err = fmt.Errorf("%w: sealed size %d does not match %d", ErrUploadIncomplete, info.Size(), session.Size)
		}
		return session, nil, err
	}
	session.Received = session.Size
	return session, file, nil
}

// DeleteUploadSession removes a session and its data. Deleting a missing
// session is not an error.
func (s *Storage) DeleteUploadSession(id string) error {
	dir, err := s.uploadSessionDir(id)
	if err != nil {
		return nil
	}
	err = os.RemoveAll(dir)
	uploadSessionLocks.Delete(dir)
	return err
}

// ListUploadSessions returns the readable sessions, oldest first. Sessions
// whose metadata cannot be read are skipped; PurgeUploadSessions removes them.
func (s *Storage) ListUploadSessions() ([]UploadSession, error) {
	entries, err := os.ReadDir(s.uploadSessionsRoot())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	sessions := make([]UploadSession, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || validateID(entry.Name()) != nil {
			continue
		}
		session, err := readUploadSession(filepath.Join(s.uploadSessionsRoot(), entry.Name()))
		if err != nil {
			continue
		}
		sessions = append(sessions, session)
	}
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].CreatedAt.Before(sessions[j].CreatedAt) })
	return sessions, nil
}

// PurgeUploadSessions deletes sessions created before cutoff, plus session
// directories without readable metadata that were last modified before it.
func (s *Storage) PurgeUploadSessions(cutoff time.Time) (int, error) {
	root := s.uploadSessionsRoot()
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	removed := 0
	var firstErr error
	for _, entry := range entries {
		if !entry.IsDir() || validateID(entry.Name()) != nil {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		stale := false
		if session, err := readUploadSession(dir); err == nil {
			stale = session.CreatedAt.Before(cutoff)
		} else if info, statErr := entry.Info(); statErr == nil {
			stale = info.ModTime().Before(cutoff)
		}
		if !stale {
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		uploadSessionLocks.Delete(dir)
		removed++
	}
	return removed, firstErr
}

func writeFileAtomic(path string, data []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tempPath := file.Name()
	defer os.Remove(tempPath)
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(tempPath, path)
}
