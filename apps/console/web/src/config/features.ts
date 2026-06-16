// Feature flags that gate in-progress console capabilities.
//
// Some features (online Deployments, the Playground) are not production-ready
// yet. Rather than hard-coding whether they show a real UI or a "Coming Soon"
// placeholder, each is driven by a Vite build-time env var so that:
//   - production builds stay conservative (features off by default), and
//   - dev / test builds can enable them for fast iteration.
//
// Resolution order for each flag:
//   1. explicit VITE_ENABLE_* env var ("true"/"1" -> on, "false"/"0" -> off)
//   2. otherwise default to import.meta.env.DEV (on for `vite dev`, off for
//      production builds).
//
// To enable a feature in a non-dev build, pass the env var at build time, e.g.
//   VITE_ENABLE_PLAYGROUND=true VITE_ENABLE_DEPLOYMENTS=true npm run build

function resolveFlag(value: string | undefined, fallback: boolean): boolean {
  if (value === undefined || value === '') return fallback;
  const normalized = value.trim().toLowerCase();
  if (normalized === 'true' || normalized === '1') return true;
  if (normalized === 'false' || normalized === '0') return false;
  return fallback;
}

const devDefault = import.meta.env.DEV;

export const features = {
  deployments: resolveFlag(import.meta.env.VITE_ENABLE_DEPLOYMENTS, devDefault),
  playground: resolveFlag(import.meta.env.VITE_ENABLE_PLAYGROUND, devDefault),
} as const;

export type Features = typeof features;
