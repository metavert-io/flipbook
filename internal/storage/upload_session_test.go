package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestSession(t *testing.T, store *Storage, id string, size int64, created time.Time) UploadSession {
	t.Helper()
	session, err := store.CreateUploadSession(UploadSession{
		ID: id, WorkspaceID: "ws", UserID: "user", Filename: "Catalog.PDF", Size: size, CreatedAt: created,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	return session
}

func TestUploadSessionAssemblesChunksAndRetriesIdempotently(t *testing.T) {
	store := New(t.TempDir())
	content := []byte("0123456789abcdefghij")
	newTestSession(t, store, "session-1", int64(len(content)), time.Now())
	ctx := context.Background()

	if session, err := store.WriteUploadChunk(ctx, "session-1", 0, bytes.NewReader(content[:8]), 8); err != nil || session.Received != 8 {
		t.Fatalf("first chunk: received %d, %v", session.Received, err)
	}
	// A retry of a chunk whose response was lost rewrites the same range.
	if session, err := store.WriteUploadChunk(ctx, "session-1", 0, bytes.NewReader(content[:8]), 8); err != nil || session.Received != 8 {
		t.Fatalf("retried chunk: received %d, %v", session.Received, err)
	}
	// Skipping ahead is rejected and reports where to resume.
	session, err := store.WriteUploadChunk(ctx, "session-1", 12, bytes.NewReader(content[12:]), 8)
	if !errors.Is(err, ErrUploadOffsetMismatch) || session.Received != 8 {
		t.Fatalf("gap chunk: received %d, %v", session.Received, err)
	}
	if _, _, err := store.CompleteUploadSession("session-1"); !errors.Is(err, ErrUploadIncomplete) {
		t.Fatalf("complete before all bytes arrived returned %v", err)
	}
	if session, err := store.WriteUploadChunk(ctx, "session-1", 8, bytes.NewReader(content[8:16]), 8); err != nil || session.Received != 16 {
		t.Fatalf("second chunk: received %d, %v", session.Received, err)
	}
	if session, err := store.WriteUploadChunk(ctx, "session-1", 16, bytes.NewReader(content[16:]), 8); err != nil || session.Received != 20 {
		t.Fatalf("last chunk: received %d, %v", session.Received, err)
	}
	if got, err := store.GetUploadSession("session-1"); err != nil || got.Received != 20 || got.Filename != "Catalog.PDF" || got.UserID != "user" {
		t.Fatalf("session status = %+v, %v", got, err)
	}

	completed, file, err := store.CompleteUploadSession("session-1")
	if err != nil {
		t.Fatal(err)
	}
	assembled, err := io.ReadAll(file)
	_ = file.Close()
	if err != nil || !bytes.Equal(assembled, content) || completed.Received != 20 {
		t.Fatalf("assembled %q, received %d, %v", assembled, completed.Received, err)
	}
	if _, err := store.WriteUploadChunk(ctx, "session-1", 0, bytes.NewReader(content[:8]), 8); !errors.Is(err, ErrUploadSessionComplete) {
		t.Fatalf("chunk after completion returned %v", err)
	}
	// Completion is repeatable so a retried finalize request can reopen it.
	if _, again, err := store.CompleteUploadSession("session-1"); err != nil {
		t.Fatalf("second completion: %v", err)
	} else {
		_ = again.Close()
	}

	if err := store.DeleteUploadSession("session-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetUploadSession("session-1"); !errors.Is(err, ErrUploadSessionNotFound) {
		t.Fatalf("deleted session lookup returned %v", err)
	}
	if _, err := store.WriteUploadChunk(ctx, "session-1", 0, bytes.NewReader(content), 32); !errors.Is(err, ErrUploadSessionNotFound) {
		t.Fatalf("chunk for deleted session returned %v", err)
	}
}

func TestUploadSessionRejectsOversizeAndInvalidInput(t *testing.T) {
	store := New(t.TempDir())
	ctx := context.Background()
	newTestSession(t, store, "session-2", 10, time.Now())

	if _, err := store.WriteUploadChunk(ctx, "session-2", 0, strings.NewReader("0123456789"), 4); !errors.Is(err, ErrUploadTooLarge) {
		t.Fatalf("chunk above the per-request cap returned %v", err)
	}
	if _, err := store.WriteUploadChunk(ctx, "session-2", 6, strings.NewReader("0123456789"), 0); !errors.Is(err, ErrUploadTooLarge) {
		t.Fatalf("chunk past the declared size returned %v", err)
	}
	if _, err := store.WriteUploadChunk(ctx, "session-2", -1, strings.NewReader("x"), 4); !errors.Is(err, ErrInvalidUpload) {
		t.Fatalf("negative offset returned %v", err)
	}
	if got, err := store.GetUploadSession("session-2"); err != nil || got.Received != 0 {
		t.Fatalf("rejected chunks changed the session: %+v, %v", got, err)
	}
	if _, err := store.GetUploadSession("../escape"); !errors.Is(err, ErrUploadSessionNotFound) {
		t.Fatalf("path-like session ID returned %v", err)
	}
	if _, err := store.CreateUploadSession(UploadSession{ID: "session-3", Filename: "doc.pdf", Size: 0}); !errors.Is(err, ErrInvalidUpload) {
		t.Fatalf("zero-size session returned %v", err)
	}
	if _, err := store.CreateUploadSession(UploadSession{ID: "session-3", Filename: "no-extension", Size: 10}); !errors.Is(err, ErrInvalidUpload) {
		t.Fatalf("filename without extension returned %v", err)
	}
	if _, err := store.CreateUploadSession(UploadSession{ID: "session-2", Filename: "doc.pdf", Size: 10}); err == nil {
		t.Fatal("duplicate session ID was accepted")
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := store.WriteUploadChunk(cancelled, "session-2", 0, strings.NewReader("0123"), 4); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled chunk returned %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(store.BaseDir, uploadSessionsDirName, "session-2"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".chunk-") {
			t.Errorf("staged chunk %s was left behind", entry.Name())
		}
	}
}

func TestUploadSessionListAndPurge(t *testing.T) {
	store := New(t.TempDir())
	now := time.Now()
	newTestSession(t, store, "old", 5, now.Add(-48*time.Hour))
	newTestSession(t, store, "new", 5, now)
	orphan := filepath.Join(store.BaseDir, uploadSessionsDirName, "orphan")
	if err := os.Mkdir(orphan, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(orphan, now.Add(-48*time.Hour), now.Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}

	sessions, err := store.ListUploadSessions()
	if err != nil || len(sessions) != 2 || sessions[0].ID != "old" || sessions[1].ID != "new" {
		t.Fatalf("list = %+v, %v", sessions, err)
	}
	removed, err := store.PurgeUploadSessions(now.Add(-24 * time.Hour))
	if err != nil || removed != 2 {
		t.Fatalf("purge removed %d, %v", removed, err)
	}
	sessions, err = store.ListUploadSessions()
	if err != nil || len(sessions) != 1 || sessions[0].ID != "new" {
		t.Fatalf("after purge = %+v, %v", sessions, err)
	}
	if _, err := os.Stat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphan directory still present: %v", err)
	}
}
