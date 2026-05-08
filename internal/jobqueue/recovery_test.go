package jobqueue

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// seedStatuses opens a fresh in-memory store and inserts one job per
// status. Returns a map of status → job id so tests can re-query specific
// rows by id.
func seedStatuses(t *testing.T) (*Store, map[string]string) {
	t.Helper()
	dir := t.TempDir()
	store, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ctx := context.Background()
	statuses := []string{
		StatusQueued,
		StatusRunning,
		StatusSucceeded,
		StatusFailed,
		StatusAbandoned,
		StatusCancelled,
	}
	ids := make(map[string]string, len(statuses))
	for i, s := range statuses {
		id := s + "-id"
		ids[s] = id
		if err := store.CreateJob(ctx, InsertJobInput{
			ID:              id,
			WorkloadID:      s + "-wl",
			OrganizationID:  "org-1",
			WorkloadType:    "video:extract-audio",
			WorkloadVersion: 1,
			PayloadJSON:     `{}`,
			// Leave lease token + expires zero — pre-existing scan bug
			// in GetJob can't handle a populated lease_expires_at column
			// (modernc.org/sqlite returns it as string, store struct uses
			// sql.NullTime). Out of scope for this PR.
			LeaseToken:     "",
			LeaseExpiresAt: time.Time{},
		}); err != nil {
			t.Fatalf("create job %d: %v", i, err)
		}
		// CreateJob seeds status='queued' — bump to the desired status if
		// different.
		if s != StatusQueued {
			if err := store.UpdateStatus(ctx, id, s); err != nil {
				t.Fatalf("seed %s: %v", s, err)
			}
		}
	}
	return store, ids
}

func TestRecoverOrphaned_FlipsRunningOnly(t *testing.T) {
	store, ids := seedStatuses(t)
	ctx := context.Background()

	n, err := store.RecoverOrphaned(ctx, "")
	if err != nil {
		t.Fatalf("RecoverOrphaned: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 row touched, got %d", n)
	}

	// running → failed
	got, err := store.GetJob(ctx, ids[StatusRunning])
	if err != nil {
		t.Fatalf("get running job: %v", err)
	}
	if got.Status != StatusFailed {
		t.Fatalf("running job status = %q; want failed", got.Status)
	}

	// queued left intact (RecoverOrphaned does NOT touch queued).
	got, err = store.GetJob(ctx, ids[StatusQueued])
	if err != nil {
		t.Fatalf("get queued job: %v", err)
	}
	if got.Status != StatusQueued {
		t.Fatalf("queued job status = %q; want queued", got.Status)
	}

	// terminals left intact.
	for _, term := range []string{StatusSucceeded, StatusFailed, StatusAbandoned, StatusCancelled} {
		got, err := store.GetJob(ctx, ids[term])
		if err != nil {
			t.Fatalf("get %s job: %v", term, err)
		}
		if got.Status != term {
			t.Fatalf("%s job mutated to %q", term, got.Status)
		}
	}

	// Event row written for the orphaned job.
	events, err := store.ListEvents(ctx, ids[StatusRunning], 10)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	var found bool
	for _, e := range events {
		if e.Kind == "orphan_failed" && e.Message == ReasonOrphanedAfterRestart {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no orphan_failed event recorded; events=%+v", events)
	}
}

func TestRecoverOrphaned_Idempotent(t *testing.T) {
	store, _ := seedStatuses(t)
	ctx := context.Background()

	if _, err := store.RecoverOrphaned(ctx, ""); err != nil {
		t.Fatalf("first call: %v", err)
	}
	n, err := store.RecoverOrphaned(ctx, "")
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if n != 0 {
		t.Fatalf("second call touched %d rows; want 0", n)
	}
}

func TestSweepActiveToFailed_FlipsQueuedAndRunning(t *testing.T) {
	store, ids := seedStatuses(t)
	ctx := context.Background()

	n, err := store.SweepActiveToFailed(ctx, "")
	if err != nil {
		t.Fatalf("SweepActiveToFailed: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2 rows touched, got %d", n)
	}

	for _, active := range []string{StatusQueued, StatusRunning} {
		got, err := store.GetJob(ctx, ids[active])
		if err != nil {
			t.Fatalf("get %s job: %v", active, err)
		}
		if got.Status != StatusFailed {
			t.Fatalf("%s job status = %q; want failed", active, got.Status)
		}

		events, err := store.ListEvents(ctx, ids[active], 10)
		if err != nil {
			t.Fatalf("list events for %s: %v", active, err)
		}
		var found bool
		for _, e := range events {
			if e.Kind == "unpair_failed" && e.Message == ReasonUnpaired {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("no unpair_failed event for %s; events=%+v", active, events)
		}
	}

	// Terminals untouched.
	for _, term := range []string{StatusSucceeded, StatusFailed, StatusAbandoned, StatusCancelled} {
		got, err := store.GetJob(ctx, ids[term])
		if err != nil {
			t.Fatalf("get %s job: %v", term, err)
		}
		if got.Status != term {
			t.Fatalf("%s job mutated to %q", term, got.Status)
		}
	}
}

func TestSweepActiveToFailed_Idempotent(t *testing.T) {
	store, _ := seedStatuses(t)
	ctx := context.Background()

	if _, err := store.SweepActiveToFailed(ctx, ""); err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	n, err := store.SweepActiveToFailed(ctx, "")
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if n != 0 {
		t.Fatalf("second sweep touched %d rows; want 0", n)
	}
}

func TestRecoverOrphaned_CustomReason(t *testing.T) {
	store, ids := seedStatuses(t)
	ctx := context.Background()

	custom := "custom_reason_xyz"
	if _, err := store.RecoverOrphaned(ctx, custom); err != nil {
		t.Fatalf("RecoverOrphaned: %v", err)
	}
	events, err := store.ListEvents(ctx, ids[StatusRunning], 10)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	var found bool
	for _, e := range events {
		if e.Message == custom {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("custom reason not recorded; events=%+v", events)
	}
}
