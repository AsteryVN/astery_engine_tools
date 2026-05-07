-- jobqueue schema — see wiki/architecture/engine-tools-hybrid.md §4.10
PRAGMA journal_mode=WAL;
PRAGMA busy_timeout=5000;
PRAGMA foreign_keys=ON;

CREATE TABLE IF NOT EXISTS jobs (
    id                    TEXT PRIMARY KEY,
    workload_id           TEXT NOT NULL UNIQUE,
    organization_id       TEXT NOT NULL,
    workload_type         TEXT NOT NULL,
    workload_version      INTEGER NOT NULL,
    payload_json          TEXT NOT NULL,
    status                TEXT NOT NULL,
    lease_token           TEXT,
    lease_expires_at      TEXT,
    resumable_state       TEXT,
    created_at            TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at            TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS jobs_status_idx ON jobs(status);

CREATE TABLE IF NOT EXISTS job_events (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    job_id      TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    ts          TEXT NOT NULL DEFAULT (datetime('now')),
    kind        TEXT NOT NULL,
    message     TEXT,
    attrs_json  TEXT
);
CREATE INDEX IF NOT EXISTS job_events_job_ts_idx ON job_events(job_id, ts);

CREATE TABLE IF NOT EXISTS job_attempts (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    job_id          TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    attempt_num     INTEGER NOT NULL,
    started_at      TEXT NOT NULL DEFAULT (datetime('now')),
    finished_at     TEXT,
    outcome         TEXT,
    error           TEXT
);
CREATE INDEX IF NOT EXISTS job_attempts_job_idx ON job_attempts(job_id);
