// Regression tests for the IPC client. The five named scenarios from the
// architect spec map to:
//   1. token rotation              → 'retries 401 once with fresh token'
//   2. no infinite retry           → 'gives up after second 401'
//   3. connection refused          → 'classifies fetch TypeError as connection-refused'
//   4. SSE reconnect schedule      → 'follows the exact backoff schedule'
//   5. no reconnect storm          → 'stops retrying after give-up'
//
// invoke() is mocked to return controllable token/port values; fetch is
// mocked per test; EventSource is replaced with a controllable stub.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

vi.mock('@tauri-apps/api/core', () => ({
  invoke: vi.fn(),
}));

import { invoke as mockInvoke } from '@tauri-apps/api/core';
import { _resetPortCacheForTests, ipc, openLogStream } from './ipc';
import { IpcError } from './types';

const invoke = mockInvoke as unknown as ReturnType<typeof vi.fn>;

interface FakeFetchResponse {
  status: number;
  body?: unknown;
}

function fakeResponse(r: FakeFetchResponse): Response {
  const ct = r.body !== undefined ? 'application/json' : 'text/plain';
  return {
    ok: r.status >= 200 && r.status < 300,
    status: r.status,
    headers: new Headers({ 'content-type': ct }),
    json: async () => r.body,
  } as unknown as Response;
}

describe('ipc.request', () => {
  beforeEach(() => {
    _resetPortCacheForTests();
    invoke.mockReset();
    invoke.mockImplementation(async (cmd: string) => {
      if (cmd === 'ipc_port') return 12345;
      if (cmd === 'ipc_token') return 'tok-1';
      throw new Error(`unexpected invoke ${cmd}`);
    });
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it('retries 401 once with fresh token', async () => {
    // Token rotates: first call returns tok-1, second call returns tok-2.
    let tokenCall = 0;
    invoke.mockImplementation(async (cmd: string) => {
      if (cmd === 'ipc_port') return 12345;
      if (cmd === 'ipc_token') {
        tokenCall += 1;
        return tokenCall === 1 ? 'tok-1' : 'tok-2';
      }
      throw new Error('nope');
    });

    const fetchMock = vi
      .fn<typeof fetch>()
      .mockResolvedValueOnce(fakeResponse({ status: 401 }))
      .mockResolvedValueOnce(
        fakeResponse({ status: 200, body: { app_version: '0.2.0', paused: false, active: 0, timestamp: 'now' } }),
      );
    vi.stubGlobal('fetch', fetchMock);

    const status = await ipc.status();
    expect(status.app_version).toBe('0.2.0');
    expect(fetchMock).toHaveBeenCalledTimes(2);

    const firstHeaders = (fetchMock.mock.calls[0]?.[1]?.headers ?? {}) as Record<string, string>;
    const secondHeaders = (fetchMock.mock.calls[1]?.[1]?.headers ?? {}) as Record<string, string>;
    expect(firstHeaders['Authorization']).toBe('Bearer tok-1');
    expect(secondHeaders['Authorization']).toBe('Bearer tok-2');
  });

  it('calls invoke("ipc_token") exactly twice during a single-retry rotation', async () => {
    // Regression test #1 explicit invoke-count assertion.
    // The spec requires: on 401 → re-read token via invoke('ipc_token') → retry once.
    // Total invoke('ipc_token') calls = 2 (initial fetch + retry).
    invoke.mockReset();
    let tokenInvokeCount = 0;
    invoke.mockImplementation(async (cmd: string) => {
      if (cmd === 'ipc_port') return 12345;
      if (cmd === 'ipc_token') {
        tokenInvokeCount += 1;
        return tokenInvokeCount === 1 ? 'tok-a' : 'tok-b';
      }
      throw new Error('nope');
    });

    const fetchMock = vi
      .fn<typeof fetch>()
      .mockResolvedValueOnce(fakeResponse({ status: 401 }))
      .mockResolvedValueOnce(
        fakeResponse({ status: 200, body: { app_version: '0.2.0', paused: false, active: 0, timestamp: 'now' } }),
      );
    vi.stubGlobal('fetch', fetchMock);

    await ipc.status();

    expect(tokenInvokeCount).toBe(2);
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  it('gives up after second 401 with auth-rejected', async () => {
    const fetchMock = vi
      .fn<typeof fetch>()
      .mockResolvedValueOnce(fakeResponse({ status: 401 }))
      .mockResolvedValueOnce(fakeResponse({ status: 401 }));
    vi.stubGlobal('fetch', fetchMock);

    await expect(ipc.status()).rejects.toMatchObject({
      kind: 'auth-rejected',
    });
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  it('classifies fetch TypeError as connection-refused', async () => {
    const fetchMock = vi
      .fn<typeof fetch>()
      .mockRejectedValue(new TypeError('Failed to fetch'));
    vi.stubGlobal('fetch', fetchMock);

    let caught: unknown;
    try {
      await ipc.status();
    } catch (err) {
      caught = err;
    }
    expect(caught).toBeInstanceOf(IpcError);
    expect((caught as IpcError).kind).toBe('connection-refused');
  });

  it('classifies 404 as not-found and 409 as conflict', async () => {
    const fetchMock = vi
      .fn<typeof fetch>()
      .mockResolvedValueOnce(fakeResponse({ status: 404 }));
    vi.stubGlobal('fetch', fetchMock);
    await expect(ipc.jobsGet('nope')).rejects.toMatchObject({
      kind: 'not-found',
    });

    const fetchMock2 = vi
      .fn<typeof fetch>()
      .mockResolvedValueOnce(fakeResponse({ status: 409 }));
    vi.stubGlobal('fetch', fetchMock2);
    await expect(ipc.jobsCancel('done-job')).rejects.toMatchObject({
      kind: 'conflict',
    });
  });

  it('classifies 409 with body.error="not_paired" as not-paired', async () => {
    // Distinct from a generic 409 — the unpair handler returns 409 when no
    // local session exists, and the renderer's Pairing state machine needs
    // to branch on this without parsing strings.
    const fetchMock = vi
      .fn<typeof fetch>()
      .mockResolvedValueOnce(
        fakeResponse({ status: 409, body: { error: 'not_paired' } }),
      );
    vi.stubGlobal('fetch', fetchMock);
    await expect(ipc.unpair()).rejects.toMatchObject({ kind: 'not-paired' });
  });

  it('classifies 502 with body.error="cloud_unreachable" as cloud-unreachable', async () => {
    const fetchMock = vi
      .fn<typeof fetch>()
      .mockResolvedValueOnce(
        fakeResponse({ status: 502, body: { error: 'cloud_unreachable' } }),
      );
    vi.stubGlobal('fetch', fetchMock);
    await expect(ipc.unpair()).rejects.toMatchObject({
      kind: 'cloud-unreachable',
    });
  });

  it('classifies 502 with body.error="cloud_rejected" as cloud-rejected', async () => {
    const fetchMock = vi
      .fn<typeof fetch>()
      .mockResolvedValueOnce(
        fakeResponse({ status: 502, body: { error: 'cloud_rejected' } }),
      );
    vi.stubGlobal('fetch', fetchMock);
    await expect(ipc.unpair()).rejects.toMatchObject({
      kind: 'cloud-rejected',
    });
  });

  it('unpair happy-path returns the cleared_jobs + forced fields', async () => {
    const fetchMock = vi.fn<typeof fetch>().mockResolvedValueOnce(
      fakeResponse({
        status: 200,
        body: { cleared_jobs: 3, forced: false },
      }),
    );
    vi.stubGlobal('fetch', fetchMock);
    const result = await ipc.unpair();
    expect(result.cleared_jobs).toBe(3);
    expect(result.forced).toBe(false);

    // body must include the force flag — defaults to false when omitted.
    const calls = fetchMock.mock.calls;
    const firstCall = calls[0]!;
    const init = firstCall[1] as RequestInit;
    expect(init.body).toBe(JSON.stringify({ force: false }));
  });

  it('unpair propagates the force=true flag in the request body', async () => {
    const fetchMock = vi.fn<typeof fetch>().mockResolvedValueOnce(
      fakeResponse({
        status: 200,
        body: { cleared_jobs: 0, forced: true },
      }),
    );
    vi.stubGlobal('fetch', fetchMock);
    const result = await ipc.unpair(true);
    expect(result.forced).toBe(true);
    const calls = fetchMock.mock.calls;
    const firstCall = calls[0]!;
    const init = firstCall[1] as RequestInit;
    expect(init.body).toBe(JSON.stringify({ force: true }));
  });
});

// ─── SSE stream tests ────────────────────────────────────────────────────

class FakeEventSource {
  public static instances: FakeEventSource[] = [];
  public onopen: ((ev: Event) => void) | null = null;
  public onmessage: ((ev: MessageEvent<string>) => void) | null = null;
  public onerror: ((ev: Event) => void) | null = null;
  public closed = false;

  constructor(public url: string) {
    FakeEventSource.instances.push(this);
  }

  close(): void {
    this.closed = true;
  }

  fireError(): void {
    this.onerror?.(new Event('error'));
  }

  static reset(): void {
    FakeEventSource.instances = [];
  }
}

describe('openLogStream', () => {
  beforeEach(() => {
    _resetPortCacheForTests();
    FakeEventSource.reset();
    vi.stubGlobal('EventSource', FakeEventSource as unknown as typeof EventSource);
    invoke.mockReset();
    invoke.mockImplementation(async (cmd: string) => {
      if (cmd === 'ipc_port') return 12345;
      if (cmd === 'ipc_token') return 'tok-sse';
      throw new Error('nope');
    });
    vi.useFakeTimers();
  });

  afterEach(() => {
    vi.useRealTimers();
    vi.unstubAllGlobals();
  });

  it('follows the exact backoff schedule and gives up at 10 retries', async () => {
    const states: string[] = [];
    const handle = openLogStream(
      () => undefined,
      (s) => {
        states.push(s);
      },
    );

    // Drive the async connect() to completion: the implementation awaits
    // invoke() before constructing the EventSource. flushPromises drains
    // microtasks left by those awaits.
    const flush = async (): Promise<void> => {
      for (let i = 0; i < 10; i += 1) {
        await Promise.resolve();
      }
    };
    await flush();

    expect(FakeEventSource.instances.length).toBe(1);

    const expectedSchedule = [500, 1000, 2000, 4000, 8000, 8000, 8000, 8000, 8000, 8000];
    for (let i = 0; i < expectedSchedule.length; i += 1) {
      const es = FakeEventSource.instances[FakeEventSource.instances.length - 1];
      expect(es).toBeDefined();
      es!.fireError();
      const delay = expectedSchedule[i] ?? 0;
      vi.advanceTimersByTime(delay);
      await flush();
    }

    // After 10 retries we must hit give-up. The 10th retry produced one
    // more EventSource (total instances = 1 initial + 10 retries = 11).
    expect(FakeEventSource.instances.length).toBe(11);
    // Fire one more error to trigger the give-up path explicitly.
    const last = FakeEventSource.instances[FakeEventSource.instances.length - 1];
    expect(last).toBeDefined();
    last!.fireError();
    await flush();

    expect(states).toContain('give-up');
    handle.close();
  });

  it('transitions to give-up then reconnects from scratch when reconnect() is called', async () => {
    // Regression test #3: daemon-crash + UI reconnect state machine.
    // Simulates: open stream → errors × 11 → give-up → user clicks reconnect
    // → state resets to "connecting" → new EventSource created.
    const states: string[] = [];
    const handle = openLogStream(
      () => undefined,
      (s) => {
        states.push(s);
      },
    );

    const flush = async (): Promise<void> => {
      for (let i = 0; i < 10; i += 1) {
        await Promise.resolve();
      }
    };
    await flush();

    // Drive to give-up (10 retries = 11 EventSource instances, then one more error).
    const schedule = [500, 1000, 2000, 4000, 8000, 8000, 8000, 8000, 8000, 8000];
    for (let i = 0; i < schedule.length; i += 1) {
      FakeEventSource.instances[FakeEventSource.instances.length - 1]!.fireError();
      vi.advanceTimersByTime(schedule[i] ?? 0);
      await flush();
    }
    FakeEventSource.instances[FakeEventSource.instances.length - 1]!.fireError();
    await flush();

    expect(states).toContain('give-up');
    const instancesBeforeReconnect = FakeEventSource.instances.length;

    // Simulate user clicking "Reconnect" button.
    handle.reconnect();
    await flush();

    // A new EventSource must be created after reconnect().
    expect(FakeEventSource.instances.length).toBeGreaterThan(instancesBeforeReconnect);
    // State must re-enter "connecting".
    expect(states).toContain('connecting');

    handle.close();
  });

  it('stops retrying after give-up', async () => {
    const flush = async (): Promise<void> => {
      for (let i = 0; i < 10; i += 1) {
        await Promise.resolve();
      }
    };
    const handle = openLogStream(() => undefined);

    await flush();
    const schedule = [500, 1000, 2000, 4000, 8000, 8000, 8000, 8000, 8000, 8000];
    for (let i = 0; i < schedule.length; i += 1) {
      const es = FakeEventSource.instances[FakeEventSource.instances.length - 1];
      expect(es).toBeDefined();
      es!.fireError();
      vi.advanceTimersByTime(schedule[i] ?? 0);
      await flush();
    }
    const lastEs = FakeEventSource.instances[FakeEventSource.instances.length - 1];
    expect(lastEs).toBeDefined();
    lastEs!.fireError();
    await flush();

    const beforeStorm = FakeEventSource.instances.length;
    // Advance 60s — no further constructions should happen.
    vi.advanceTimersByTime(60_000);
    await flush();
    expect(FakeEventSource.instances.length).toBe(beforeStorm);

    handle.close();
  });
});
