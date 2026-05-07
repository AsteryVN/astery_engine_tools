package jobqueue

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "jobs.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func seedJob(t *testing.T, s *Store, id, status string) {
	t.Helper()
	ctx := context.Background()
	if err := s.CreateJob(ctx, InsertJobInput{
		ID:              id,
		WorkloadID:      id + "-w",
		OrganizationID:  "org",
		WorkloadType:    "clip-video",
		WorkloadVersion: 1,
		PayloadJSON:     "{}",
		LeaseToken:      "",
		LeaseExpiresAt:  time.Time{},
	}); err != nil {
		t.Fatalf("create job: %v", err)
	}
	if status != StatusQueued {
		if err := s.UpdateStatus(ctx, id, status); err != nil {
			t.Fatalf("seed status: %v", err)
		}
	}
}

func TestCancel_HappyPath(t *testing.T) {
	s := newTestStore(t)
	seedJob(t, s, "job1", StatusQueued)

	if err := s.Cancel(context.Background(), "job1"); err != nil {
		t.Fatalf("cancel queued job: %v", err)
	}
	got, err := s.GetJob(context.Background(), "job1")
	if err != nil {
		t.Fatalf("get after cancel: %v", err)
	}
	if got.Status != StatusCancelled {
		t.Fatalf("expected cancelled, got %q", got.Status)
	}
}

func TestCancel_NotFound(t *testing.T) {
	s := newTestStore(t)
	err := s.Cancel(context.Background(), "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestCancel_Terminal(t *testing.T) {
	s := newTestStore(t)
	for _, st := range []string{StatusSucceeded, StatusFailed, StatusAbandoned, StatusCancelled} {
		seedJob(t, s, "job-"+st, st)
		err := s.Cancel(context.Background(), "job-"+st)
		if !errors.Is(err, ErrJobTerminal) {
			t.Fatalf("status=%s: expected ErrJobTerminal, got %v", st, err)
		}
	}
}
