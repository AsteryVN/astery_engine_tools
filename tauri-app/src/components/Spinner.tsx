interface SpinnerProps {
  size?: 'sm' | 'md';
}

export function Spinner({ size = 'sm' }: SpinnerProps): JSX.Element {
  const dim = size === 'sm' ? 'h-3 w-3' : 'h-5 w-5';
  return (
    <span
      role="status"
      aria-label="loading"
      className={[
        'inline-block animate-spin rounded-full border-2 border-zinc-300 border-t-zinc-900',
        dim,
      ].join(' ')}
    />
  );
}
