// IPC TOKEN INVARIANT: token must never appear in module scope, localStorage,
// sessionStorage, IndexedDB, or any persistence. Read fresh from invoke() per
// request. CI grep gates this — if you add a top-level `let token` here you
// will break the v0.2.x security review.
//
// Typed client for the engine-toold loopback IPC server. All HTTP calls go to
// 127.0.0.1:<port> with a Bearer token; both port and token are surfaced from
// the Rust side via tauri::command (`ipc_port`, `ipc_token`). The token can
// rotate at runtime — every request that gets a 401 is retried exactly once
// with a freshly-fetched token before surfacing IpcError('auth-rejected').

import { invoke as tauriInvoke } from '@tauri-apps/api/core';
import {
  IpcError,
  type Job,
  type JobState,
  type LogEvent,
  type SseConnectionState,
  type Status,
} from './types';

// Port doesn't rotate during the lifetime of the daemon process, so caching
// it is safe (and saves an invoke() round-trip per request). Token IS NOT
// cached — see invariant above.
let cachedPort: number | null = null;

// invoke is split out so vitest can patch a single symbol.
const invoke: <T>(cmd: string, args?: Record<string, unknown>) => Promise<T> =
  tauriInvoke;

async function getPort(): Promise<number> {
  if (cachedPort !== null) return cachedPort;
  try {
    const port = await invoke<number>('ipc_port');
    cachedPort = port;
    return port;
  } catch (err) {
    throw new IpcError(
      'connection-refused',
      `port file unreadable: ${(err as Error).message ?? String(err)}`,
    );
  }
}

async function getFreshToken(): Promise<string> {
  try {
    return await invoke<string>('ipc_token');
  } catch (err) {
    throw new IpcError(
      'connection-refused',
      `token file unreadable: ${(err as Error).message ?? String(err)}`,
    );
  }
}

// resetPortCache is exported for tests that simulate a daemon restart.
export function _resetPortCacheForTests(): void {
  cachedPort = null;
}

interface RequestOptions {
  method?: 'GET' | 'POST' | 'DELETE';
  body?: unknown;
  query?: Record<string, string | number | undefined>;
}

async function buildUrl(
  path: string,
  query?: Record<string, string | number | undefined>,
): Promise<string> {
  const port = await getPort();
  const url = new URL(`http://127.0.0.1:${port}${path}`);
  if (query) {
    for (const [key, val] of Object.entries(query)) {
      if (val === undefined) continue;
      url.searchParams.set(key, String(val));
    }
  }
  return url.toString();
}

function classifyHttp(status: number): IpcError {
  switch (status) {
    case 401:
      return new IpcError('auth-rejected', 'unauthorized', status);
    case 404:
      return new IpcError('not-found', 'not found', status);
    case 409:
      return new IpcError('conflict', 'conflict', status);
    default:
      return new IpcError('unknown', `http ${status}`, status);
  }
}

function classifyFetchError(err: unknown): IpcError {
  // Tauri's webview surfaces connection-refused as a TypeError with a
  // "Failed to fetch" / "Load failed" / "NetworkError" message. We treat
  // every non-IpcError thrown by fetch as connection-refused — there is no
  // other meaningful failure mode for a same-host loopback call.
  if (err instanceof IpcError) return err;
  const message = (err as Error)?.message ?? String(err);
  return new IpcError('connection-refused', message);
}

async function doFetch(
  path: string,
  token: string,
  opts: RequestOptions,
): Promise<Response> {
  const url = await buildUrl(path, opts.query);
  const init: RequestInit = {
    method: opts.method ?? 'GET',
    headers: {
      Authorization: `Bearer ${token}`,
      Accept: 'application/json',
      ...(opts.body !== undefined ? { 'Content-Type': 'application/json' } : {}),
    },
  };
  if (opts.body !== undefined) {
    init.body = JSON.stringify(opts.body);
  }
  return fetch(url, init);
}

async function request<T>(path: string, opts: RequestOptions = {}): Promise<T> {
  // Single-retry on 401 with a freshly-fetched token (rotation scenario).
  // We deliberately do NOT loop — a second 401 means the daemon really did
  // reject the new token, and the caller should re-authenticate.
  let token = await getFreshToken();
  let response: Response;
  try {
    response = await doFetch(path, token, opts);
  } catch (err) {
    throw classifyFetchError(err);
  }

  if (response.status === 401) {
    token = await getFreshToken();
    try {
      response = await doFetch(path, token, opts);
    } catch (err) {
      throw classifyFetchError(err);
    }
    if (response.status === 401) {
      throw new IpcError('auth-rejected', 'unauthorized after retry', 401);
    }
  }

  if (!response.ok) {
    throw classifyHttp(response.status);
  }

  // 204 No Content is legal; return undefined cast as T.
  if (response.status === 204) {
    return undefined as T;
  }
  const ct = response.headers.get('content-type') ?? '';
  if (!ct.includes('application/json')) {
    return undefined as T;
  }
  return (await response.json()) as T;
}

// ─── public API ──────────────────────────────────────────────────────────

export interface PairResult {
  org_id: string;
  device_id: string;
  expires_at: string;
}

export interface CapabilitiesResult {
  features: string[];
}

export interface JobsListResponse {
  jobs: Job[];
}

export const ipc = {
  status(): Promise<Status> {
    return request<Status>('/v1/status');
  },

  async jobsList(opts?: {
    state?: JobState;
    limit?: number;
  }): Promise<Job[]> {
    const query: Record<string, string | number | undefined> = {};
    if (opts?.state) query['status'] = opts.state;
    if (opts?.limit !== undefined) query['limit'] = opts.limit;
    const res = await request<JobsListResponse>('/v1/jobs', { query });
    return res.jobs ?? [];
  },

  jobsGet(id: string): Promise<Job> {
    return request<Job>(`/v1/jobs/${encodeURIComponent(id)}`);
  },

  async jobsCancel(id: string): Promise<void> {
    await request<unknown>(`/v1/jobs/${encodeURIComponent(id)}/cancel`, {
      method: 'POST',
    });
  },

  pair(displayCode: string): Promise<PairResult> {
    return request<PairResult>('/v1/pair', {
      method: 'POST',
      body: { display_code: displayCode },
    });
  },

  async pause(): Promise<void> {
    await request<unknown>('/v1/pause', { method: 'POST' });
  },

  async resume(): Promise<void> {
    await request<unknown>('/v1/resume', { method: 'POST' });
  },

  capabilities(): Promise<CapabilitiesResult> {
    return request<CapabilitiesResult>('/v1/capabilities');
  },

  logsStream(
    onEvent: (e: LogEvent) => void,
    onState?: (s: SseConnectionState) => void,
  ): { close: () => void; reconnect: () => void } {
    return openLogStream(onEvent, onState);
  },
};

// ─── SSE log stream with bounded backoff ────────────────────────────────

const BACKOFF_MS: ReadonlyArray<number> = [
  500, 1000, 2000, 4000, 8000, 8000, 8000, 8000, 8000, 8000,
];

interface LogStreamHandle {
  close: () => void;
  reconnect: () => void;
}

// We resolve EventSource at construction time rather than module load so
// vitest's `vi.stubGlobal('EventSource', …)` (which runs after import) can
// override the constructor. jsdom does not ship EventSource by default.
function resolveEventSourceCtor(): typeof EventSource {
  const ctor = (globalThis as { EventSource?: typeof EventSource }).EventSource;
  if (!ctor) {
    throw new Error('EventSource is not available in this environment');
  }
  return ctor;
}

export function openLogStream(
  onEvent: (e: LogEvent) => void,
  onState?: (s: SseConnectionState) => void,
): LogStreamHandle {
  let attempt = 0;
  let closed = false;
  let timer: ReturnType<typeof setTimeout> | null = null;
  let source: EventSource | null = null;

  const emitState = (s: SseConnectionState): void => {
    onState?.(s);
  };

  const cleanup = (): void => {
    if (timer !== null) {
      clearTimeout(timer);
      timer = null;
    }
    if (source !== null) {
      source.close();
      source = null;
    }
  };

  const connect = async (): Promise<void> => {
    if (closed) return;
    emitState(attempt === 0 ? 'connecting' : 'reconnecting');

    let token: string;
    let url: string;
    try {
      token = await getFreshToken();
      url = await buildUrl('/v1/logs/stream', {
        // EventSource cannot set custom request headers, so the bearer is
        // passed via ?token=…. The daemon's sseQueryAuthShim promotes it
        // to an Authorization header before the regular middleware runs
        // and strips it from the URL so it never reaches handlers/logs.
        token,
      });
    } catch {
      scheduleRetry();
      return;
    }

    let es: EventSource;
    try {
      const Ctor = resolveEventSourceCtor();
      es = new Ctor(url, { withCredentials: false });
    } catch {
      scheduleRetry();
      return;
    }
    source = es;

    es.onopen = (): void => {
      attempt = 0;
      emitState('open');
    };
    es.onmessage = (ev: MessageEvent<string>): void => {
      try {
        const parsed = JSON.parse(ev.data) as LogEvent;
        onEvent(parsed);
      } catch {
        // Ignore unparseable frames — keep the stream alive.
      }
    };
    es.onerror = (): void => {
      // Native EventSource will sometimes auto-reconnect; we always close
      // and run our own backoff so reconnect schedules are deterministic.
      es.close();
      source = null;
      scheduleRetry();
    };
  };

  const scheduleRetry = (): void => {
    if (closed) return;
    if (attempt >= BACKOFF_MS.length) {
      emitState('give-up');
      return;
    }
    const delay = BACKOFF_MS[attempt] ?? 8000;
    attempt += 1;
    timer = setTimeout(() => {
      void connect();
    }, delay);
  };

  void connect();

  return {
    close(): void {
      closed = true;
      cleanup();
    },
    reconnect(): void {
      attempt = 0;
      cleanup();
      closed = false;
      void connect();
    },
  };
}
