import { useEffect, useRef, useState } from 'react';
import { Badge, type BadgeTone } from '../components/Badge';
import { Button } from '../components/Button';
import { Card, CardHeader } from '../components/Card';
import { ipc } from '../lib/ipc';
import type { LogEvent, SseConnectionState } from '../lib/types';

const MAX_LINES = 1000;

export function Logs(): JSX.Element {
  const [events, setEvents] = useState<LogEvent[]>([]);
  const [state, setState] = useState<SseConnectionState>('connecting');
  const handleRef = useRef<{ close: () => void; reconnect: () => void } | null>(null);
  const containerRef = useRef<HTMLDivElement | null>(null);
  const stickToBottomRef = useRef<boolean>(true);

  useEffect(() => {
    const handle = ipc.logsStream(
      (ev) => {
        setEvents((prev) => {
          const next = prev.length >= MAX_LINES ? prev.slice(prev.length - MAX_LINES + 1) : prev.slice();
          next.push(ev);
          return next;
        });
      },
      (s) => setState(s),
    );
    handleRef.current = handle;
    return () => {
      handle.close();
      handleRef.current = null;
    };
  }, []);

  useEffect(() => {
    if (!stickToBottomRef.current) return;
    const el = containerRef.current;
    if (!el) return;
    el.scrollTop = el.scrollHeight;
  }, [events]);

  const onScroll = (): void => {
    const el = containerRef.current;
    if (!el) return;
    const distance = el.scrollHeight - el.scrollTop - el.clientHeight;
    stickToBottomRef.current = distance < 16;
  };

  return (
    <Card>
      <CardHeader
        title="Live logs"
        subtitle={`${events.length} line${events.length === 1 ? '' : 's'}`}
        actions={
          <div className="flex items-center gap-2">
            <Badge tone={stateTone(state)}>{state}</Badge>
            {state === 'give-up' ? (
              <Button
                size="sm"
                variant="secondary"
                onClick={() => handleRef.current?.reconnect()}
              >
                Reconnect
              </Button>
            ) : null}
          </div>
        }
      />
      <div
        ref={containerRef}
        onScroll={onScroll}
        className="h-[60vh] overflow-y-auto rounded-md border border-zinc-200/60 bg-zinc-950 p-2 font-mono text-[11px] text-zinc-100"
      >
        {events.length === 0 ? (
          <p className="text-center text-zinc-500">Waiting for log events…</p>
        ) : (
          events.map((ev, i) => (
            <div key={i} className="whitespace-pre-wrap">
              <span className="text-zinc-500">{ev.ts}</span>
              <span className={['ml-2 uppercase', levelColor(ev.level)].join(' ')}>{ev.level}</span>
              <span className="ml-2">{ev.msg}</span>
              {ev.kv ? (
                <span className="ml-2 text-zinc-400">{JSON.stringify(ev.kv)}</span>
              ) : null}
            </div>
          ))
        )}
      </div>
    </Card>
  );
}

function stateTone(s: SseConnectionState): BadgeTone {
  switch (s) {
    case 'open':
      return 'success';
    case 'connecting':
      return 'info';
    case 'reconnecting':
      return 'warning';
    case 'give-up':
      return 'danger';
    default:
      return 'neutral';
  }
}

function levelColor(level: LogEvent['level']): string {
  switch (level) {
    case 'error':
      return 'text-red-400';
    case 'warn':
      return 'text-amber-300';
    case 'info':
      return 'text-emerald-300';
    case 'debug':
      return 'text-zinc-400';
    default:
      return 'text-zinc-200';
  }
}
