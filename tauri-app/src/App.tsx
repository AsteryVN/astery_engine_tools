import { useEffect, useState } from 'react';
import { invoke } from '@tauri-apps/api/core';
import {
  BrowserRouter,
  Link,
  Navigate,
  Route,
  Routes,
  useLocation,
} from 'react-router-dom';
import { Button } from './components/Button';
import { Card } from './components/Card';
import { Spinner } from './components/Spinner';
import { ipc } from './lib/ipc';
import { IpcError } from './lib/types';
import { checkForUpdate, type UpdateStatus } from './lib/updater';
import { Dashboard } from './pages/Dashboard';
import { Logs } from './pages/Logs';
import { Pairing } from './pages/Pairing';

type DaemonState = 'checking' | 'up' | 'down';

export function App(): JSX.Element {
  return (
    <BrowserRouter>
      <Shell />
    </BrowserRouter>
  );
}

function Shell(): JSX.Element {
  const [daemonState, setDaemonState] = useState<DaemonState>('checking');
  const [daemonError, setDaemonError] = useState<string | null>(null);
  const [starting, setStarting] = useState<boolean>(false);

  const probe = async (): Promise<void> => {
    setDaemonState('checking');
    setDaemonError(null);
    try {
      await ipc.status();
      setDaemonState('up');
    } catch (err) {
      if (err instanceof IpcError && err.kind === 'connection-refused') {
        setDaemonState('down');
        setDaemonError(err.message);
      } else if (err instanceof IpcError) {
        // Auth-rejected here typically means the daemon is up but the
        // token file is stale. Treat it as 'up' so the user can navigate
        // to Pairing or surface the error inside a page.
        setDaemonState('up');
      } else {
        setDaemonState('down');
        setDaemonError((err as Error).message ?? String(err));
      }
    }
  };

  useEffect(() => {
    void probe();
  }, []);

  const handleStartDaemon = async (): Promise<void> => {
    setStarting(true);
    try {
      await invoke('start_daemon_sidecar');
      // Give the daemon a moment to bind its listener, then re-probe.
      setTimeout(() => {
        void probe();
      }, 600);
    } catch (err) {
      setDaemonError((err as Error).message ?? String(err));
    } finally {
      setStarting(false);
    }
  };

  if (daemonState === 'checking') {
    return (
      <div className="flex h-screen items-center justify-center">
        <Spinner size="md" />
      </div>
    );
  }

  if (daemonState === 'down') {
    return (
      <div className="flex h-screen items-center justify-center bg-zinc-50 px-6">
        <Card className="max-w-md text-center">
          <h1 className="text-base font-semibold text-zinc-900">
            Daemon not running
          </h1>
          <p className="mt-2 text-sm text-zinc-600">
            engine-toold is not responding on the loopback IPC port.
          </p>
          {daemonError ? (
            <pre className="mt-3 max-h-40 overflow-auto rounded-md bg-zinc-100 p-2 text-left text-[11px] text-zinc-600">
              {daemonError}
            </pre>
          ) : null}
          <div className="mt-4 flex justify-center gap-2">
            <Button onClick={() => void probe()} variant="secondary">
              Retry
            </Button>
            <Button onClick={() => void handleStartDaemon()} disabled={starting}>
              {starting ? 'Starting…' : 'Start daemon'}
            </Button>
          </div>
        </Card>
      </div>
    );
  }

  return (
    <div className="flex min-h-screen flex-col">
      <Header />
      <UpdateBanner />
      <main className="flex-1 px-6 py-5">
        <Routes>
          <Route path="/" element={<Navigate to="/dashboard" replace />} />
          <Route path="/dashboard" element={<Dashboard />} />
          <Route path="/logs" element={<Logs />} />
          <Route path="/pairing" element={<Pairing />} />
          <Route path="*" element={<Navigate to="/dashboard" replace />} />
        </Routes>
      </main>
    </div>
  );
}

// UpdateBanner runs `checkForUpdate()` once on mount and renders a slim
// banner above the main content when a newer release is available. The
// banner is dismissable per-session; clicking "Restart to update" applies
// the update and relaunches the app.
function UpdateBanner(): JSX.Element | null {
  const [update, setUpdate] = useState<UpdateStatus | null>(null);
  const [applying, setApplying] = useState<boolean>(false);
  const [dismissed, setDismissed] = useState<boolean>(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    void (async () => {
      const status = await checkForUpdate();
      setUpdate(status);
    })();
  }, []);

  if (!update || update.status !== 'available' || dismissed) {
    return null;
  }

  const onApply = async (): Promise<void> => {
    setApplying(true);
    setError(null);
    try {
      await update.apply();
      // If apply() returns we did NOT relaunch — surface as failure.
      setError('Update did not relaunch — try restarting manually.');
    } catch (err) {
      setError((err as Error).message ?? String(err));
    } finally {
      setApplying(false);
    }
  };

  return (
    <div className="flex items-center justify-between gap-3 border-b border-zinc-200/60 bg-amber-50 px-6 py-2 text-xs text-amber-900">
      <span>
        Update available: v{update.currentVersion} → v{update.version}
        {error ? <span className="ml-2 text-red-600">({error})</span> : null}
      </span>
      <div className="flex items-center gap-2">
        <Button
          size="sm"
          variant="primary"
          onClick={() => void onApply()}
          disabled={applying}
        >
          {applying ? 'Applying…' : 'Restart to update'}
        </Button>
        <Button
          size="sm"
          variant="ghost"
          onClick={() => setDismissed(true)}
          disabled={applying}
        >
          Later
        </Button>
      </div>
    </div>
  );
}

function Header(): JSX.Element {
  const { pathname } = useLocation();
  const link = (to: string, label: string): JSX.Element => {
    const active = pathname === to || (to === '/dashboard' && pathname === '/');
    return (
      <Link
        to={to}
        className={[
          'rounded-md px-2.5 py-1 text-sm transition-colors',
          active
            ? 'bg-zinc-900 text-white'
            : 'text-zinc-700 hover:bg-zinc-100',
        ].join(' ')}
      >
        {label}
      </Link>
    );
  };
  return (
    <header className="flex h-12 items-center justify-between border-b border-zinc-200/60 bg-white px-6">
      <div className="flex items-center gap-2">
        <span className="text-sm font-semibold text-zinc-900">
          Astery Engine Tools
        </span>
      </div>
      <nav className="flex items-center gap-1">
        {link('/dashboard', 'Dashboard')}
        {link('/logs', 'Logs')}
        {link('/pairing', 'Pairing')}
      </nav>
    </header>
  );
}
