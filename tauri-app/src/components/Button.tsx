import type { ButtonHTMLAttributes, ReactNode } from 'react';

interface ButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: 'primary' | 'secondary' | 'danger' | 'ghost';
  size?: 'sm' | 'md';
  children: ReactNode;
}

const variantClass: Record<NonNullable<ButtonProps['variant']>, string> = {
  primary: 'bg-zinc-900 text-white hover:bg-zinc-800 disabled:bg-zinc-400',
  secondary:
    'bg-white text-zinc-900 border border-zinc-200/60 hover:bg-zinc-50 disabled:text-zinc-400',
  danger: 'bg-red-600 text-white hover:bg-red-700 disabled:bg-red-300',
  ghost: 'bg-transparent text-zinc-700 hover:bg-zinc-100',
};

const sizeClass: Record<NonNullable<ButtonProps['size']>, string> = {
  sm: 'h-7 px-2.5 text-xs',
  md: 'h-9 px-3.5 text-sm',
};

export function Button({
  variant = 'primary',
  size = 'md',
  className,
  children,
  ...rest
}: ButtonProps): JSX.Element {
  const cls = [
    'inline-flex items-center justify-center rounded-md font-medium transition-colors',
    'focus:outline-none focus:ring-2 focus:ring-zinc-400 focus:ring-offset-1',
    'disabled:cursor-not-allowed',
    variantClass[variant],
    sizeClass[size],
    className ?? '',
  ].join(' ');
  return (
    <button className={cls} {...rest}>
      {children}
    </button>
  );
}
