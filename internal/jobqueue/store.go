// Package jobqueue is the local SQLite-backed job store. Three tables: jobs
// (current state), job_events (append-only log), job_attempts (retry
// history). WAL mode + busy_timeout for concurrent reader safety.
package jobqueue

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite — no cgo, clean cross-compile
)

//go:embed schema.sql
var schemaSQL string

// Store wraps the SQLite connection plus a write-mutex so concurrent
// goroutines (scheduler + executor + IPC) stay single-writer.
type Store struct {
	db    *sql.DB
	wmu   sync.Mutex
	clock func() time.Time
}

// Open opens the SQLite database at path (creating the schema if needed).
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(on)")
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if err := db.PingContext(context.Background()); err != nil {
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{db: db, clock: time.Now}, nil
}

// Close shuts the underlying connection.
func (s *Store) Close() error {
	if s.db == nil {
		return nil
	}
	return s.db.Close()
}

// ─── jobs ────────────────────────────────────────────────────────────────

// Job is the current-state row.
type Job struct {
	ID              string
	WorkloadID      string
	OrganizationID  string
	WorkloadType    string
	WorkloadVersion int
	PayloadJSON     string
	Status          string
	LeaseToken      sql.NullString
	LeaseExpiresAt  sql.NullTime
	ResumableState  sql.NullString
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// Status enums.
const (
	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
	StatusAbandoned = "abandoned"
	StatusCancelled = "cancelled"
)

// ErrNotFound is returned by GetJob etc when the row is missing.
var ErrNotFound = errors.New("job not found")

// ErrJobTerminal is returned by Cancel when the target job is already in a
// terminal state and therefore cannot be cancelled.
var ErrJobTerminal = errors.New("job already terminal")

// IsTerminal reports whether a status string represents a terminal state
// (no further transitions allowed).
func IsTerminal(status string) bool {
	switch status {
	case StatusSucceeded, StatusFailed, StatusAbandoned, StatusCancelled:
		return true
	default:
		return false
	}
}

// Cancel transitions a non-terminal job to StatusCancelled.
//
// Returns [ErrNotFound] if no row matches, [ErrJobTerminal] if the job is
// already in a terminal state. NOTE: this only flips the database row; an
// in-flight executor goroutine has its own context cancellation path and is
// not interrupted from here. Surrendering the cloud lease + actually
// stopping a running executor is a follow-up (see TODO in scheduler).
func (s *Store) Cancel(ctx context.Context, jobID string) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()

	var status string
	if err := s.db.QueryRowContext(ctx, `SELECT status FROM jobs WHERE id = ?`, jobID).Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("cancel: lookup status: %w", err)
	}
	if IsTerminal(status) {
		return ErrJobTerminal
	}
	if _, err := s.db.ExecContext(ctx, `
		UPDATE jobs SET status = ?, updated_at = datetime('now') WHERE id = ?`,
		StatusCancelled, jobID); err != nil {
		return fmt.Errorf("cancel: update status: %w", err)
	}
	return nil
}

// InsertJobInput is the body of CreateJob.
type InsertJobInput struct {
	ID              string
	WorkloadID      string
	OrganizationID  string
	WorkloadType    string
	WorkloadVersion int
	PayloadJSON     string
	LeaseToken      string
	LeaseExpiresAt  time.Time
}

// CreateJob inserts a new queued job.
func (s *Store) CreateJob(ctx context.Context, in InsertJobInput) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO jobs (id, workload_id, organization_id, workload_type, workload_version,
		                  payload_json, status, lease_token, lease_expires_at)
		VALUES (?, ?, ?, ?, ?, ?, 'queued', ?, ?)`,
		in.ID, in.WorkloadID, in.OrganizationID, in.WorkloadType, in.WorkloadVersion,
		in.PayloadJSON, nullStr(in.LeaseToken), nullTime(in.LeaseExpiresAt),
	)
	if err != nil {
		return fmt.Errorf("insert job: %w", err)
	}
	return nil
}

// GetJob returns one job by id.
func (s *Store) GetJob(ctx context.Context, id string) (*Job, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, workload_id, organization_id, workload_type, workload_version,
		       payload_json, status, lease_token, lease_expires_at, resumable_state,
		       created_at, updated_at
		  FROM jobs WHERE id = ?`, id)
	var j Job
	var created, updated string
	if err := row.Scan(
		&j.ID, &j.WorkloadID, &j.OrganizationID, &j.WorkloadType, &j.WorkloadVersion,
		&j.PayloadJSON, &j.Status, &j.LeaseToken, &j.LeaseExpiresAt, &j.ResumableState,
		&created, &updated,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get job: %w", err)
	}
	j.CreatedAt = parseSQLiteTime(created)
	j.UpdatedAt = parseSQLiteTime(updated)
	return &j, nil
}

// ListJobs returns the most recent jobs (paginated by limit/offset).
func (s *Store) ListJobs(ctx context.Context, status string, limit, offset int) ([]Job, error) {
	if limit <= 0 {
		limit = 50
	}
	q := `SELECT id, workload_id, organization_id, workload_type, workload_version,
	             payload_json, status, lease_token, lease_expires_at, resumable_state,
	             created_at, updated_at
	      FROM jobs `
	args := []any{}
	if status != "" {
		q += `WHERE status = ? `
		args = append(args, status)
	}
	q += `ORDER BY created_at DESC LIMIT ? OFFSET ?`
	args = append(args, limit, offset)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	defer rows.Close()
	out := make([]Job, 0)
	for rows.Next() {
		var j Job
		var created, updated string
		if err := rows.Scan(
			&j.ID, &j.WorkloadID, &j.OrganizationID, &j.WorkloadType, &j.WorkloadVersion,
			&j.PayloadJSON, &j.Status, &j.LeaseToken, &j.LeaseExpiresAt, &j.ResumableState,
			&created, &updated,
		); err != nil {
			return nil, fmt.Errorf("scan job: %w", err)
		}
		j.CreatedAt = parseSQLiteTime(created)
		j.UpdatedAt = parseSQLiteTime(updated)
		out = append(out, j)
	}
	return out, rows.Err()
}

// UpdateStatus transitions a job's status.
func (s *Store) UpdateStatus(ctx context.Context, id, status string) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.db.ExecContext(ctx, `
		UPDATE jobs SET status = ?, updated_at = datetime('now') WHERE id = ?`, status, id)
	if err != nil {
		return fmt.Errorf("update status: %w", err)
	}
	return nil
}

// UpdateResumableState writes the executor-defined recovery hint.
func (s *Store) UpdateResumableState(ctx context.Context, id, state string) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.db.ExecContext(ctx, `
		UPDATE jobs SET resumable_state = ?, updated_at = datetime('now') WHERE id = ?`,
		nullStr(state), id)
	if err != nil {
		return fmt.Errorf("update resumable state: %w", err)
	}
	return nil
}

// UpdateLease replaces the lease token + expiry on a job.
func (s *Store) UpdateLease(ctx context.Context, id, token string, expires time.Time) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.db.ExecContext(ctx, `
		UPDATE jobs SET lease_token = ?, lease_expires_at = ?, updated_at = datetime('now')
		 WHERE id = ?`, nullStr(token), nullTime(expires), id)
	if err != nil {
		return fmt.Errorf("update lease: %w", err)
	}
	return nil
}

// PendingRecovery returns jobs that were running when the daemon last shut
// down (lease still set, status=running). Called at boot to drive
// Executor.Recover.
func (s *Store) PendingRecovery(ctx context.Context) ([]Job, error) {
	return s.ListJobs(ctx, StatusRunning, 100, 0)
}

// ─── events ──────────────────────────────────────────────────────────────

// Event is one row in job_events.
type Event struct {
	ID        int64
	JobID     string
	TS        time.Time
	Kind      string
	Message   string
	AttrsJSON string
}

// AppendEvent inserts an event.
func (s *Store) AppendEvent(ctx context.Context, jobID, kind, message, attrsJSON string) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO job_events (job_id, kind, message, attrs_json) VALUES (?, ?, ?, ?)`,
		jobID, kind, message, nullStr(attrsJSON))
	if err != nil {
		return fmt.Errorf("append event: %w", err)
	}
	return nil
}

// ListEvents returns the most recent N events for a job.
func (s *Store) ListEvents(ctx context.Context, jobID string, limit int) ([]Event, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, job_id, ts, kind, message, COALESCE(attrs_json, '')
		  FROM job_events WHERE job_id = ?
		 ORDER BY ts DESC, id DESC LIMIT ?`, jobID, limit)
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	defer rows.Close()
	out := make([]Event, 0)
	for rows.Next() {
		var e Event
		var ts string
		if err := rows.Scan(&e.ID, &e.JobID, &ts, &e.Kind, &e.Message, &e.AttrsJSON); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		e.TS = parseSQLiteTime(ts)
		out = append(out, e)
	}
	return out, rows.Err()
}

// ─── attempts ────────────────────────────────────────────────────────────

// StartAttempt opens a new attempt row and returns its id.
func (s *Store) StartAttempt(ctx context.Context, jobID string, attemptNum int) (int64, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO job_attempts (job_id, attempt_num) VALUES (?, ?)`, jobID, attemptNum)
	if err != nil {
		return 0, fmt.Errorf("start attempt: %w", err)
	}
	return res.LastInsertId()
}

// FinishAttempt records the outcome of an attempt.
func (s *Store) FinishAttempt(ctx context.Context, attemptID int64, outcome, errMsg string) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.db.ExecContext(ctx, `
		UPDATE job_attempts SET finished_at = datetime('now'), outcome = ?, error = ?
		 WHERE id = ?`, outcome, nullStr(errMsg), attemptID)
	if err != nil {
		return fmt.Errorf("finish attempt: %w", err)
	}
	return nil
}

// AttemptCount returns the number of attempts for a job. Used to populate
// the next attempt's attempt_num.
func (s *Store) AttemptCount(ctx context.Context, jobID string) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM job_attempts WHERE job_id = ?`, jobID).Scan(&n); err != nil {
		return 0, fmt.Errorf("count attempts: %w", err)
	}
	return n, nil
}

// ─── helpers ─────────────────────────────────────────────────────────────

func nullStr(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

func nullTime(t time.Time) sql.NullTime {
	if t.IsZero() {
		return sql.NullTime{}
	}
	return sql.NullTime{Time: t, Valid: true}
}

// parseSQLiteTime parses a "YYYY-MM-DD HH:MM:SS" timestamp.
func parseSQLiteTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse("2006-01-02 15:04:05", s)
	if err != nil {
		return time.Time{}
	}
	return t
}
