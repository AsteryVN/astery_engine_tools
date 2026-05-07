import type { HTMLAttributes, ReactNode } from 'react';

interface CardProps extends HTMLAttributes<HTMLDivElement> {
  size?: 'sm' | 'md';
  children: ReactNode;
}

export function Card({ size = 'md', className, children, ...rest }: CardProps): JSX.Element {
  const padding = size === 'sm' ? 'p-3' : 'p-4';
  const cls = [
    'rounded-lg border border-zinc-200/60 bg-white shadow-hairline',
    padding,
    className ?? '',
  ].join(' ');
  return (
    <div className={cls} {...rest}>
      {children}
    </div>
  );
}

interface CardHeaderProps {
  title: ReactNode;
  subtitle?: ReactNode;
  actions?: ReactNode;
}

export function CardHeader({ title, subtitle, actions }: CardHeaderProps): JSX.Element {
  return (
    <div className="mb-3 flex items-start justify-between gap-3">
      <div className="min-w-0">
        <h3 className="text-sm font-semibold text-zinc-900">{title}</h3>
        {subtitle ? (
          <p className="mt-0.5 text-xs text-zinc-500">{subtitle}</p>
        ) : null}
      </div>
      {actions ? <div className="flex shrink-0 items-center gap-2">{actions}</div> : null}
    </div>
  );
}
