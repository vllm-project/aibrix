import { Cpu } from 'lucide-react';
import { modelClaimPhaseTone } from '../utils/modelClaim';
import type { ModelClaimPhaseTone } from '../utils/modelClaim';

const phaseClasses: Record<ModelClaimPhaseTone, string> = {
  ready: 'bg-green-50 text-green-700',
  sleeping: 'bg-indigo-50 text-indigo-700',
  failed: 'bg-red-50 text-red-700',
  progress: 'bg-amber-50 text-amber-700',
  unknown: 'bg-gray-100 text-gray-600',
};

export function ModelClaimIcon() {
  return (
    <span className="flex h-8 w-8 shrink-0 items-center justify-center rounded-lg border border-teal-100 bg-teal-50 text-teal-700">
      <Cpu className="h-4 w-4" />
    </span>
  );
}

export function ModelClaimPhase({ phase, deleting = false }: { phase: string; deleting?: boolean }) {
  const normalized = deleting ? 'Deleting' : phase || 'Pending';
  const className = deleting ? phaseClasses.unknown : phaseClasses[modelClaimPhaseTone(normalized)];
  return (
    <span className={`inline-flex items-center gap-1.5 rounded-full px-2.5 py-1 text-xs font-medium ${className}`}>
      <span className="h-1.5 w-1.5 rounded-full bg-current" />
      {normalized}
    </span>
  );
}
