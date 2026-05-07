import type { Config } from 'tailwindcss';

// Light-only data-dense palette per wiki/concepts/surface-system.
// Hairline borders + soft tints; no dark-mode tokens shipped in v0.2.x.
export default {
  content: ['./index.html', './src/**/*.{ts,tsx}'],
  theme: {
    extend: {
      colors: {
        border: 'rgb(228 228 231 / 0.6)',
      },
      fontFamily: {
        sans: ['Inter', 'system-ui', 'sans-serif'],
        mono: ['"Geist Mono"', 'ui-monospace', 'monospace'],
      },
      boxShadow: {
        hairline: '0 1px 0 rgb(0 0 0 / 0.04)',
      },
      borderRadius: {
        lg: '0.5rem',
        xl: '0.75rem',
      },
    },
  },
  plugins: [],
} satisfies Config;
