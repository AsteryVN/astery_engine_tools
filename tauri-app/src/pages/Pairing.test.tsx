// Pairing.tsx state-machine tests — focuses on the new 409 → already-paired
// → re-pair flow. The happy-path pair flow is covered indirectly by
// ipc.test.ts; here we drive Pairing's state transitions directly.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';

// Mock the IPC module BEFORE the SUT imports it. vi.mock() is hoisted to
// the top of the file, so the closure body must NOT reference outer-scope
// `let`/`const` (TDZ). vi.hoisted() lifts the mock-fn declarations alongside
// the mock factory so both run before any import resolves.
const { pairMock, unpairMock } = vi.hoisted(() => ({
  pairMock: vi.fn(),
  unpairMock: vi.fn(),
}));
vi.mock('../lib/ipc', async () => {
  const actual = await vi.importActual<typeof import('../lib/ipc')>('../lib/ipc');
  return {
    ...actual,
    ipc: {
      ...actual.ipc,
      pair: (code: string) => pairMock(code),
      unpair: (force?: boolean) => unpairMock(force),
    },
  };
});

import { Pairing } from './Pairing';
import { IpcError } from '../lib/types';

function renderPairing(): ReturnType<typeof render> {
  return render(
    <MemoryRouter>
      <Pairing />
    </MemoryRouter>,
  );
}

describe('Pairing state machine', () => {
  beforeEach(() => {
    pairMock.mockReset();
    unpairMock.mockReset();
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it('disables input and shows Re-pair when daemon returns 409 conflict', async () => {
    pairMock.mockRejectedValue(
      new IpcError('conflict', 'already paired', 409),
    );

    renderPairing();

    const input = screen.getByLabelText(/display code/i) as HTMLInputElement;
    expect(input.disabled).toBe(false);

    fireEvent.change(input, { target: { value: 'ABC-DEF' } });
    fireEvent.submit(input.closest('form')!);

    // Wait for the conflict to land — input becomes disabled, Re-pair button
    // appears in place of "Pair".
    await waitFor(() => {
      expect(input.disabled).toBe(true);
    });
    expect(screen.getByRole('button', { name: /re-pair/i })).toBeTruthy();
    // No "Pair" button while in already-paired mode.
    expect(screen.queryByRole('button', { name: /^pair$/i })).toBeNull();
  });

  it('clicking Re-pair shows the confirm gate; cancel returns to already-paired', async () => {
    pairMock.mockRejectedValue(
      new IpcError('conflict', 'already paired', 409),
    );

    renderPairing();
    const input = screen.getByLabelText(/display code/i) as HTMLInputElement;
    fireEvent.change(input, { target: { value: 'ABC-DEF' } });
    fireEvent.submit(input.closest('form')!);
    await waitFor(() => expect(input.disabled).toBe(true));

    fireEvent.click(screen.getByRole('button', { name: /re-pair this device/i }));

    // Confirm gate shows.
    expect(screen.getByText(/Re-pair this device\?/i)).toBeTruthy();
    expect(
      screen.getByText(/revoke the existing pairing on the cloud/i),
    ).toBeTruthy();

    // Cancel.
    fireEvent.click(screen.getByRole('button', { name: /cancel/i }));

    // Re-pair button reappears, input still disabled.
    await waitFor(() =>
      expect(screen.getByRole('button', { name: /re-pair this device/i })).toBeTruthy(),
    );
    expect(input.disabled).toBe(true);
  });

  it('confirming Re-pair → cloud succeeds → input re-enables and is empty', async () => {
    pairMock.mockRejectedValue(
      new IpcError('conflict', 'already paired', 409),
    );
    unpairMock.mockResolvedValue({
      cleared_jobs: 2,
      forced: false,
    });

    renderPairing();
    const input = screen.getByLabelText(/display code/i) as HTMLInputElement;
    fireEvent.change(input, { target: { value: 'ABC-DEF' } });
    fireEvent.submit(input.closest('form')!);
    await waitFor(() => expect(input.disabled).toBe(true));

    fireEvent.click(screen.getByRole('button', { name: /re-pair this device/i }));
    fireEvent.click(screen.getByRole('button', { name: /^re-pair$/i })); // inside confirm gate

    // After unpair resolves, input re-enables.
    await waitFor(() => expect(input.disabled).toBe(false));
    expect(input.value).toBe(''); // cleared so the user can paste a fresh code

    // Pair button (not Re-pair) is back.
    expect(screen.getByRole('button', { name: /^pair$/i })).toBeTruthy();

    // The cleared-jobs message surfaces (informational, not error).
    expect(screen.getByText(/2 active job\(s\) terminated/i)).toBeTruthy();
  });

  it('confirming Re-pair → cloud unreachable → offers Retry / Force buttons', async () => {
    pairMock.mockRejectedValue(
      new IpcError('conflict', 'already paired', 409),
    );
    unpairMock.mockRejectedValue(
      new IpcError('cloud-unreachable', 'cloud unreachable', 502),
    );

    renderPairing();
    const input = screen.getByLabelText(/display code/i) as HTMLInputElement;
    fireEvent.change(input, { target: { value: 'ABC-DEF' } });
    fireEvent.submit(input.closest('form')!);
    await waitFor(() => expect(input.disabled).toBe(true));

    fireEvent.click(screen.getByRole('button', { name: /re-pair this device/i }));
    fireEvent.click(screen.getByRole('button', { name: /^re-pair$/i }));

    // After cloud failure, Retry + Force-clear buttons appear.
    await waitFor(() =>
      expect(screen.getByRole('button', { name: /retry/i })).toBeTruthy(),
    );
    expect(
      screen.getByRole('button', { name: /force clear local pairing/i }),
    ).toBeTruthy();
    // Input still disabled — user hasn't escaped this branch yet.
    expect(input.disabled).toBe(true);
  });

  it('Force clear local clears state with the cloud-unreachable warning', async () => {
    pairMock.mockRejectedValue(
      new IpcError('conflict', 'already paired', 409),
    );
    // First call (Re-pair) → cloud-unreachable.
    // Second call (Force) → success with forced=true.
    unpairMock
      .mockRejectedValueOnce(
        new IpcError('cloud-unreachable', 'cloud unreachable', 502),
      )
      .mockResolvedValueOnce({ cleared_jobs: 0, forced: true });

    renderPairing();
    const input = screen.getByLabelText(/display code/i) as HTMLInputElement;
    fireEvent.change(input, { target: { value: 'ABC-DEF' } });
    fireEvent.submit(input.closest('form')!);
    await waitFor(() => expect(input.disabled).toBe(true));

    fireEvent.click(screen.getByRole('button', { name: /re-pair this device/i }));
    fireEvent.click(screen.getByRole('button', { name: /^re-pair$/i }));

    await waitFor(() =>
      expect(
        screen.getByRole('button', { name: /force clear local pairing/i }),
      ).toBeTruthy(),
    );
    fireEvent.click(
      screen.getByRole('button', { name: /force clear local pairing/i }),
    );

    // After force succeeds, input re-enables and amber warning is shown.
    await waitFor(() => expect(input.disabled).toBe(false));
    expect(
      screen.getByText(/manually revoke this device in the web UI/i),
    ).toBeTruthy();
  });
});
