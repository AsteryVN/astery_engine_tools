import { useRef, useState, type FormEvent } from 'react';
import { useNavigate } from 'react-router-dom';
import { Badge } from '../components/Badge';
import { Button } from '../components/Button';
import { Card, CardHeader } from '../components/Card';
import { Spinner } from '../components/Spinner';
import { ipc, type PairResult } from '../lib/ipc';
import { IpcError } from '../lib/types';

// Mirrors the cloud's display-code generator
// (internal/modules/device/service.go::generateDisplayCode + model.go
// constants displayCodeAlphabet/displayCodeGroups/displayCodeGroupSize).
// Crockford-ish alphabet excludes 0/1/I/O and the format is two 3-char
// groups separated by a hyphen — e.g. "5CD-GZF". Keep this regex in sync
// if the cloud constants change.
const CODE_PATTERN = /^[A-HJ-NP-Z2-9]{3}-[A-HJ-NP-Z2-9]{3}$/;

// Mode is the pairing page's state machine. Centralised here so the JSX
// just renders the current mode rather than juggling 5 booleans.
//
//   input              — ready to accept a fresh display code.
//   pending            — /v1/pair in flight.
//   paired             — success; navigate to dashboard.
//   already-paired     — daemon refused with 409 (a stored session exists).
//                        Input is disabled; "Re-pair" button shown.
//   unpair-confirm     — user clicked "Re-pair"; native confirm gate.
//                        Held in state so the UI can render a styled gate
//                        instead of relying on browser-modal styling.
//   unpairing          — /v1/unpair in flight.
//   unpair-cloud-failed— /v1/unpair returned 502; offer Retry / Force.
//
// Transitions:
//   input → pending → paired (happy path)
//   input → pending → already-paired (409)
//   already-paired → unpair-confirm → unpairing → input (success)
//   already-paired → unpair-confirm → unpairing → unpair-cloud-failed
//   unpair-cloud-failed → unpairing (Retry) → ...
//   unpair-cloud-failed → unpairing (Force) → input (success)
type Mode =
  | { kind: 'input' }
  | { kind: 'pending' }
  | { kind: 'paired'; result: PairResult }
  | { kind: 'already-paired' }
  | { kind: 'unpair-confirm' }
  | { kind: 'unpairing' }
  | { kind: 'unpair-cloud-failed' };

export function Pairing(): JSX.Element {
  const navigate = useNavigate();
  const [code, setCode] = useState<string>('');
  const [mode, setMode] = useState<Mode>({ kind: 'input' });
  const [error, setError] = useState<string | null>(null);
  const [errorKind, setErrorKind] = useState<string | null>(null);
  const inputRef = useRef<HTMLInputElement | null>(null);

  const inputDisabled = mode.kind !== 'input';
  const isPaired =
    mode.kind === 'paired' ||
    mode.kind === 'already-paired' ||
    mode.kind === 'unpair-confirm' ||
    mode.kind === 'unpairing' ||
    mode.kind === 'unpair-cloud-failed';

  const submit = async (e: FormEvent): Promise<void> => {
    e.preventDefault();
    if (mode.kind !== 'input') return;
    const normalized = code.trim().toUpperCase();
    if (!CODE_PATTERN.test(normalized)) {
      setError('Code must look like ABC-123 (3 characters, dash, 3 characters).');
      setErrorKind('format');
      return;
    }
    setMode({ kind: 'pending' });
    setError(null);
    setErrorKind(null);
    try {
      const r = await ipc.pair(normalized);
      setMode({ kind: 'paired', result: r });
      // Brief success toast then send the user back to dashboard.
      setTimeout(() => navigate('/dashboard'), 800);
    } catch (err) {
      if (err instanceof IpcError) {
        setErrorKind(err.kind);
        switch (err.kind) {
          case 'auth-rejected':
            setError('Invalid pairing code.');
            setMode({ kind: 'input' });
            break;
          case 'conflict':
            setError(
              'This device is already paired. Click Re-pair to clear the existing pairing and enter a new code.',
            );
            setMode({ kind: 'already-paired' });
            break;
          case 'connection-refused':
            setError('Daemon not running.');
            setMode({ kind: 'input' });
            break;
          default:
            setError(err.message);
            setMode({ kind: 'input' });
        }
      } else {
        setError((err as Error).message);
        setMode({ kind: 'input' });
      }
    }
  };

  const askConfirmRepair = (): void => {
    setError(null);
    setErrorKind(null);
    setMode({ kind: 'unpair-confirm' });
  };

  const cancelConfirm = (): void => {
    setMode({ kind: 'already-paired' });
    setError(
      'This device is already paired. Click Re-pair to clear the existing pairing and enter a new code.',
    );
  };

  const doUnpair = async (force: boolean): Promise<void> => {
    setMode({ kind: 'unpairing' });
    setError(null);
    setErrorKind(null);
    try {
      const r = await ipc.unpair(force);
      // Success — keystore cleared, jobs swept. Drop back to input mode and
      // focus the input so the user can paste a fresh code.
      setCode('');
      setMode({ kind: 'input' });
      // Best-effort focus; ignore if ref hasn't attached yet.
      requestAnimationFrame(() => inputRef.current?.focus());
      if (r.forced) {
        setError(
          `Local pairing cleared. Cloud was unreachable, so you must manually revoke this device in the web UI before re-pairing on this machine. ${r.cleared_jobs} active job(s) terminated.`,
        );
        setErrorKind('forced-cleanup');
      } else if (r.cleared_jobs > 0) {
        setError(`Pairing cleared. ${r.cleared_jobs} active job(s) terminated.`);
        setErrorKind('info');
      }
    } catch (err) {
      if (err instanceof IpcError) {
        setErrorKind(err.kind);
        if (err.kind === 'cloud-unreachable') {
          setMode({ kind: 'unpair-cloud-failed' });
          setError(
            'Cloud unreachable. Retry, or force-clear the local pairing (you will need to revoke this device in the web UI before re-pairing).',
          );
          return;
        }
        if (err.kind === 'cloud-rejected') {
          setMode({ kind: 'already-paired' });
          setError(`Cloud rejected the unpair request: ${err.message}. Try again later.`);
          return;
        }
        if (err.kind === 'not-paired') {
          // Daemon thought we were paired but Load returned ErrNoSession by
          // the time the request reached the handler. Race / external
          // wipe — drop straight to input.
          setMode({ kind: 'input' });
          setError(null);
          return;
        }
        setMode({ kind: 'already-paired' });
        setError(err.message);
      } else {
        setMode({ kind: 'already-paired' });
        setError((err as Error).message);
      }
    }
  };

  return (
    <div className="mx-auto max-w-md">
      <Card>
        <CardHeader
          title="Pair this device"
          subtitle="Enter the display code from the cloud admin UI."
        />

        {mode.kind === 'unpair-confirm' ? (
          <div className="rounded-md border border-amber-300 bg-amber-50 p-3 text-xs text-amber-900">
            <p className="mb-2 font-medium">Re-pair this device?</p>
            <p className="mb-3">
              This will revoke the existing pairing on the cloud and terminate any
              in-flight jobs locally. Job history is preserved; running work is
              flipped to <span className="font-mono">failed</span>.
            </p>
            <div className="flex items-center justify-end gap-2">
              <Button type="button" variant="ghost" size="sm" onClick={cancelConfirm}>
                Cancel
              </Button>
              <Button
                type="button"
                variant="danger"
                size="sm"
                onClick={() => void doUnpair(false)}
              >
                Re-pair
              </Button>
            </div>
          </div>
        ) : null}

        <form onSubmit={(e) => void submit(e)} className="mt-3 space-y-3">
          <div>
            <label className="kpi-label" htmlFor="pair-code">
              Display code
            </label>
            <input
              id="pair-code"
              ref={inputRef}
              autoFocus
              spellCheck={false}
              autoComplete="off"
              value={code}
              onChange={(e) => setCode(e.target.value)}
              placeholder="ABC-123"
              maxLength={7}
              disabled={inputDisabled}
              className={[
                'mt-1 block w-full rounded-md border px-2.5 py-1.5 font-mono text-sm uppercase tracking-widest',
                'focus:outline-none focus:ring-2 focus:ring-zinc-400',
                'disabled:cursor-not-allowed disabled:bg-zinc-50 disabled:text-zinc-400',
                error && errorKind !== 'info' && errorKind !== 'forced-cleanup'
                  ? 'border-red-300 bg-red-50'
                  : 'border-zinc-200/60 bg-white',
              ].join(' ')}
            />
          </div>
          {error ? (
            <p
              className={[
                'text-xs',
                errorKind === 'info'
                  ? 'text-zinc-600'
                  : errorKind === 'forced-cleanup'
                    ? 'text-amber-700'
                    : 'text-red-600',
              ].join(' ')}
            >
              {error}
              {errorKind &&
              errorKind !== 'info' &&
              errorKind !== 'forced-cleanup' ? (
                <span className="ml-1 text-zinc-400">({errorKind})</span>
              ) : null}
            </p>
          ) : null}

          <div className="flex items-center justify-between">
            {/* Primary action depends on mode. */}
            {mode.kind === 'already-paired' ? (
              <Button
                type="button"
                variant="danger"
                onClick={askConfirmRepair}
                aria-label="Re-pair this device"
              >
                Re-pair
              </Button>
            ) : mode.kind === 'unpair-cloud-failed' ? (
              <div className="flex gap-2">
                <Button
                  type="button"
                  variant="primary"
                  onClick={() => void doUnpair(false)}
                >
                  Retry
                </Button>
                <Button
                  type="button"
                  variant="danger"
                  onClick={() => void doUnpair(true)}
                  aria-label="Force clear local pairing"
                >
                  Force clear local
                </Button>
              </div>
            ) : (
              <Button
                type="submit"
                disabled={
                  mode.kind === 'pending' ||
                  mode.kind === 'unpair-confirm' ||
                  mode.kind === 'unpairing'
                }
              >
                {mode.kind === 'pending' || mode.kind === 'unpairing' ? (
                  <Spinner />
                ) : (
                  'Pair'
                )}
              </Button>
            )}
            {isPaired && mode.kind !== 'unpair-cloud-failed' ? (
              <Badge tone={mode.kind === 'paired' ? 'success' : 'warning'}>
                {mode.kind === 'paired' ? 'Paired' : 'Already paired'}
              </Badge>
            ) : null}
          </div>
        </form>

        {mode.kind === 'paired' ? (
          <dl className="mt-4 space-y-1 border-t border-zinc-200/60 pt-3 text-xs text-zinc-600">
            <div className="flex justify-between">
              <dt className="kpi-label">Org</dt>
              <dd className="num">{mode.result.org_id}</dd>
            </div>
            <div className="flex justify-between">
              <dt className="kpi-label">Device</dt>
              <dd className="num">{mode.result.device_id}</dd>
            </div>
            <div className="flex justify-between">
              <dt className="kpi-label">Expires</dt>
              <dd className="num">{mode.result.expires_at}</dd>
            </div>
          </dl>
        ) : null}
      </Card>
    </div>
  );
}
