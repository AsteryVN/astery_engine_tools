import { useCallback, useEffect, useState } from 'react';
import { Badge, jobStateTone } from '../components/Badge';
import { Button } from '../components/Button';
import { Card, CardHeader } from '../components/Card';
import { Spinner } from '../components/Spinner';
import { ipc } from '../lib/ipc';
import type { Job, Status } from '../lib/types';

const POLL_MS = 5000;

export function Dashboard(): JSX.Element {
  const [status, setStatus] = useState<Status | null>(null);
  const [jobs, setJobs] = useState<Job[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState<boolean>(false);

  const refresh = useCallback(async (): Promise<void> => {
    try {
      const [s, list] = await Promise.all([ipc.status(), ipc.jobsList({ limit: 50 })]);
      setStatus(s);
      setJobs(list);
      setError(null);
    } catch (err) {
      setError((err as Error).message ?? String(err));
    }
  }, []);

  useEffect(() => {
    void refresh();
    const t = setInterval(() => {
      void refresh();
    }, POLL_MS);
    return () => clearInterval(t);
  }, [refresh]);

  const togglePause = async (): Promise<void> => {
    if (!status) return;
    setBusy(true);
    try {
      if (status.paused) await ipc.resume();
      else await ipc.pause();
      await refresh();
    } catch (err) {
      setError((err as Error).message ?? String(err));
    } finally {
      setBusy(false);
    }
  };

  const cancelJob = async (id: string): Promise<void> => {
    if (!window.confirm(`Cancel job ${id.slice(0, 8)}?`)) return;
    setBusy(true);
    try {
      await ipc.jobsCancel(id);
      await refresh();
    } catch (err) {
      setError((err as Error).message ?? String(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="grid grid-cols-3 gap-4 max-lg:grid-cols-2 max-sm:grid-cols-1">
      <Card>
        <CardHeader title="Status" />
        {status ? (
          <dl className="space-y-2 text-sm">
            <Row label="Version" value={<span className="num">{status.app_version}</span>} />
            <Row
              label="Paused"
              value={
                status.paused ? <Badge tone="warning">paused</Badge> : <Badge tone="success">running</Badge>
              }
            />
            <Row label="Active" value={<span className="num">{status.active}</span>} />
          </dl>
        ) : (
          <Spinner />
        )}
        <div className="mt-4">
          <Button
            variant="secondary"
            disabled={!status || busy}
            onClick={() => void togglePause()}
          >
            {status?.paused ? 'Resume' : 'Pause'}
          </Button>
        </div>
      </Card>

      <Card className="col-span-2 max-lg:col-span-2 max-sm:col-span-1">
        <CardHeader
          title="Jobs"
          subtitle={`${jobs.length} record${jobs.length === 1 ? '' : 's'}`}
          actions={
            <Button size="sm" variant="ghost" onClick={() => void refresh()}>
              Refresh
            </Button>
          }
        />
        {error ? (
          <p className="mb-2 text-xs text-red-600">{error}</p>
        ) : null}
        <div className="overflow-x-auto">
          <table className="w-full border-separate border-spacing-y-1 text-sm">
            <thead className="kpi-label">
              <tr>
                <th className="text-left">ID</th>
                <th className="text-left">Type</th>
                <th className="text-left">State</th>
                <th className="text-right">Updated</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {jobs.length === 0 ? (
                <tr>
                  <td colSpan={5} className="py-3 text-center text-xs text-zinc-500">
                    No jobs.
                  </td>
                </tr>
              ) : (
                jobs.map((j) => (
                  <tr key={j.id} className="bg-white">
                    <td className="num truncate py-1.5 pr-2 text-xs text-zinc-700">
                      {j.id.slice(0, 8)}
                    </td>
                    <td className="py-1.5 pr-2 text-xs text-zinc-700">
                      {j.workload_type ?? j.type ?? '—'}
                    </td>
                    <td className="py-1.5 pr-2">
                      <Badge tone={jobStateTone(j.status)}>{j.status}</Badge>
                    </td>
                    <td className="py-1.5 pr-2 text-right text-xs text-zinc-500">
                      {fmtTime(j.updated_at)}
                    </td>
                    <td className="py-1.5 text-right">
                      {j.status === 'running' || j.status === 'queued' ? (
                        <Button
                          size="sm"
                          variant="secondary"
                          disabled={busy}
                          onClick={() => void cancelJob(j.id)}
                        >
                          Cancel
                        </Button>
                      ) : null}
                    </td>
                  </tr>
                ))
              )}
            </tbody>
          </table>
        </div>
      </Card>
    </div>
  );
}

function Row({ label, value }: { label: string; value: JSX.Element | string }): JSX.Element {
  return (
    <div className="flex items-center justify-between">
      <dt className="kpi-label">{label}</dt>
      <dd className="text-sm text-zinc-900">{value}</dd>
    </div>
  );
}

function fmtTime(iso: string): string {
  try {
    return new Date(iso).toLocaleTimeString();
  } catch {
    return iso;
  }
}
