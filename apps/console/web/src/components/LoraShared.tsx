import { Layers } from 'lucide-react';

export const MODEL_ADAPTER_REFRESH_INTERVAL_MS = 5000;

export function formatResourceAge(createdAt?: string): string {
  if (!createdAt) return '-';
  const created = new Date(createdAt).getTime();
  if (!Number.isFinite(created)) return '-';
  const seconds = Math.max(0, Math.floor((Date.now() - created) / 1000));
  if (seconds < 60) return `${seconds}s`;
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m`;
  if (seconds < 86400) return `${Math.floor(seconds / 3600)}h`;
  return `${Math.floor(seconds / 86400)}d`;
}

export function ModelAdapterIcon() {
  return (
    <span className="flex h-8 w-8 shrink-0 items-center justify-center rounded-lg border border-teal-100 bg-teal-50 text-teal-700">
      <Layers className="h-4 w-4" />
    </span>
  );
}

export function ModelAdapterPhase({ phase }: { phase: string }) {
  const normalized = phase || 'Pending';
  const className = normalized === 'Running'
    ? 'bg-green-50 text-green-700'
    : normalized === 'Failed'
      ? 'bg-red-50 text-red-700'
      : 'bg-amber-50 text-amber-700';
  return (
    <span className={`inline-flex items-center gap-1.5 rounded-full px-2.5 py-1 text-xs font-medium ${className}`}>
      <span className="h-1.5 w-1.5 rounded-full bg-current" />
      {normalized}
    </span>
  );
}
