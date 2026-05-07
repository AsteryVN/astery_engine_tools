// Shared DTOs mirroring the Go daemon model (internal/ipc handlers).
// Keep field names in sync with internal/ipc/handlers_*.go writeJSON outputs.

export interface Status {
  app_version: string;
  paused: boolean;
  active: number;
  timestamp: string;
  // Optional fields the architect spec contract carries; daemon may add
  // these later. Renderer treats them as best-effort display values.
  version?: string;
  uptime_s?: number;
  queue_depth?: number;
  running?: number;
  capabilities?: string[];
}

export type JobState = 'queued' | 'running' | 'done' | 'failed' | 'cancelled';

export interface Job {
  id: string;
  workload_id?: string;
  organization_id?: string;
  workload_type?: string;
  workload_version?: number;
  payload_json?: string;
  status: JobState;
  created_at: string;
  updated_at: string;
  // Architect-spec optional fields (may not be populated yet by Go side).
  type?: string;
  state?: JobState;
  progress?: number;
  error?: string;
}

export type LogLevel = 'debug' | 'info' | 'warn' | 'error';

export interface LogEvent {
  ts: string;
  level: LogLevel;
  msg: string;
  kv?: Record<string, unknown>;
}

export type IpcErrorKind =
  | 'connection-refused'
  | 'auth-rejected'
  | 'not-found'
  | 'conflict'
  | 'unknown';

export class IpcError extends Error {
  public readonly kind: IpcErrorKind;
  public readonly status: number | undefined;

  constructor(kind: IpcErrorKind, message: string, status?: number) {
    super(message);
    this.name = 'IpcError';
    this.kind = kind;
    this.status = status;
  }
}

export type SseConnectionState =
  | 'connecting'
  | 'open'
  | 'reconnecting'
  | 'give-up';
