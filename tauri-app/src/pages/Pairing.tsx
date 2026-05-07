import { useState, type FormEvent } from 'react';
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

export function Pairing(): JSX.Element {
  const navigate = useNavigate();
  const [code, setCode] = useState<string>('');
  const [pending, setPending] = useState<boolean>(false);
  const [result, setResult] = useState<PairResult | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [errorKind, setErrorKind] = useState<string | null>(null);

  const submit = async (e: FormEvent): Promise<void> => {
    e.preventDefault();
    const normalized = code.trim().toUpperCase();
    if (!CODE_PATTERN.test(normalized)) {
      setError('Code must look like ABC-123 (3 characters, dash, 3 characters).');
      setErrorKind('format');
      return;
    }
    setPending(true);
    setError(null);
    setErrorKind(null);
    try {
      const r = await ipc.pair(normalized);
      setResult(r);
      // Brief success toast then send the user back to dashboard.
      setTimeout(() => navigate('/dashboard'), 800);
    } catch (err) {
      if (err instanceof IpcError) {
        setErrorKind(err.kind);
        switch (err.kind) {
          case 'auth-rejected':
            setError('Invalid pairing code.');
            break;
          case 'conflict':
            setError('This device is already paired.');
            break;
          case 'connection-refused':
            setError('Daemon not running.');
            break;
          default:
            setError(err.message);
        }
      } else {
        setError((err as Error).message);
      }
    } finally {
      setPending(false);
    }
  };

  return (
    <div className="mx-auto max-w-md">
      <Card>
        <CardHeader
          title="Pair this device"
          subtitle="Enter the display code from the cloud admin UI."
        />
        <form onSubmit={(e) => void submit(e)} className="space-y-3">
          <div>
            <label className="kpi-label" htmlFor="pair-code">
              Display code
            </label>
            <input
              id="pair-code"
              autoFocus
              spellCheck={false}
              autoComplete="off"
              value={code}
              onChange={(e) => setCode(e.target.value)}
              placeholder="ABC-123"
              maxLength={7}
              className={[
                'mt-1 block w-full rounded-md border px-2.5 py-1.5 font-mono text-sm uppercase tracking-widest',
                'focus:outline-none focus:ring-2 focus:ring-zinc-400',
                error ? 'border-red-300 bg-red-50' : 'border-zinc-200/60 bg-white',
              ].join(' ')}
            />
          </div>
          {error ? (
            <p className="text-xs text-red-600">
              {error}
              {errorKind ? <span className="ml-1 text-zinc-400">({errorKind})</span> : null}
            </p>
          ) : null}
          <div className="flex items-center justify-between">
            <Button type="submit" disabled={pending}>
              {pending ? <Spinner /> : 'Pair'}
            </Button>
            {result ? <Badge tone="success">Paired</Badge> : null}
          </div>
        </form>
        {result ? (
          <dl className="mt-4 space-y-1 border-t border-zinc-200/60 pt-3 text-xs text-zinc-600">
            <div className="flex justify-between">
              <dt className="kpi-label">Org</dt>
              <dd className="num">{result.org_id}</dd>
            </div>
            <div className="flex justify-between">
              <dt className="kpi-label">Device</dt>
              <dd className="num">{result.device_id}</dd>
            </div>
            <div className="flex justify-between">
              <dt className="kpi-label">Expires</dt>
              <dd className="num">{result.expires_at}</dd>
            </div>
          </dl>
        ) : null}
      </Card>
    </div>
  );
}
