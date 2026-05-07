import type { ReactNode } from 'react';

export type BadgeTone =
  | 'neutral'
  | 'info'
  | 'success'
  | 'warning'
  | 'danger'
  | 'muted';

interface BadgeProps {
  tone?: BadgeTone;
  children: ReactNode;
}

const toneClass: Record<BadgeTone, string> = {
  neutral: 'bg-zinc-100 text-zinc-700 ring-zinc-200/60',
  info: 'bg-blue-50 text-blue-700 ring-blue-200/60',
  success: 'bg-emerald-50 text-emerald-700 ring-emerald-200/60',
  warning: 'bg-amber-50 text-amber-700 ring-amber-200/60',
  danger: 'bg-red-50 text-red-700 ring-red-200/60',
  muted: 'bg-zinc-50 text-zinc-500 ring-zinc-200/60',
};

export function Badge({ tone = 'neutral', children }: BadgeProps): JSX.Element {
  return (
    <span
      className={[
        'inline-flex items-center rounded-md px-1.5 py-0.5 text-[10px] font-medium uppercase tracking-wider ring-1 ring-inset',
        toneClass[tone],
      ].join(' ')}
    >
      {children}
    </span>
  );
}

export function jobStateTone(
  state: 'queued' | 'running' | 'done' | 'failed' | 'cancelled',
): BadgeTone {
  switch (state) {
    case 'queued':
      return 'muted';
    case 'running':
      return 'info';
    case 'done':
      return 'success';
    case 'failed':
      return 'danger';
    case 'cancelled':
      return 'warning';
    default:
      return 'neutral';
  }
}
