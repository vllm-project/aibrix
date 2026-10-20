import { useEffect, useState } from 'react';
import { AlertTriangle, ChevronLeft, Copy, Moon, Pencil } from 'lucide-react';
import {
  APIError,
  deleteModelClaim,
  getModelClaim,
  updateModelClaimPerGpu,
} from '../utils/api';
import type { ModelClaim, ModelClaimInstance } from '../utils/api';
import { copyToClipboard } from '../utils/clipboard';
import {
  formatBytes,
  MODEL_CLAIM_REFRESH_INTERVAL_MS,
  modelClaimAttention,
  validatePerGpuFigure,
} from '../utils/modelClaim';
import { formatResourceAge } from './LoraShared';
import { ModelClaimPhase } from './ModelClaimShared';

interface ModelClaimDetailProps {
  claimName: string;
  onBack: () => void;
}

export function ModelClaimDetail({ claimName, onBack }: ModelClaimDetailProps) {
  const [claim, setClaim] = useState<ModelClaim | null>(null);
  const [loading, setLoading] = useState(true);
  const [deleting, setDeleting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [toast, setToast] = useState<string | null>(null);

  useEffect(() => {
    let active = true;
    let requestInFlight = false;
    let initialLoad = true;

    const refresh = async () => {
      if (requestInFlight) return;
      requestInFlight = true;
      try {
        const result = await getModelClaim(claimName);
        if (active) {
          setClaim(result);
          setError(null);
        }
      } catch (err) {
        if (active) {
          setError(err instanceof Error ? err.message : 'Failed to load ModelClaim.');
          if (err instanceof APIError && err.status === 404) {
            setClaim(null);
          }
        }
      } finally {
        requestInFlight = false;
        if (active && initialLoad) {
          initialLoad = false;
          setLoading(false);
        }
      }
    };

    void refresh();
    const interval = window.setInterval(() => {
      void refresh();
    }, MODEL_CLAIM_REFRESH_INTERVAL_MS);

    return () => {
      active = false;
      window.clearInterval(interval);
    };
  }, [claimName]);

  const showToast = (message: string) => {
    setToast(message);
    window.setTimeout(() => setToast(null), 1800);
  };

  const copyValue = async (value: string, message: string) => {
    await copyToClipboard(value);
    showToast(message);
  };

  const handleDelete = async () => {
    if (!claim || deleting) return;
    if (!window.confirm(
      `Delete ModelClaim "${claim.name}"? Its engine is stopped and ${claim.modelName} stops being served.`,
    )) {
      return;
    }
    setDeleting(true);
    setError(null);
    try {
      await deleteModelClaim(claim.name);
      onBack();
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to delete ModelClaim.');
      setDeleting(false);
    }
  };

  if (loading) {
    return <div className="p-8 text-sm text-gray-400">Loading ModelClaim...</div>;
  }
  if (!claim) {
    return (
      <div className="p-8">
        <button type="button" onClick={onBack} className="mb-4 text-sm text-teal-700">
          Back to ModelClaims
        </button>
        <div className="rounded-lg border border-red-200 bg-red-50 px-4 py-3 text-sm text-red-700">
          {error || 'ModelClaim not found.'}
        </div>
      </div>
    );
  }

  const attention = modelClaimAttention(claim);
  const engineArgs = Object.entries(claim.engineArgs);

  return (
    <div className="mx-auto w-full max-w-6xl p-8">
      <button
        type="button"
        onClick={onBack}
        className="mb-4 inline-flex items-center gap-1 text-sm text-gray-500 hover:text-gray-900"
      >
        <ChevronLeft className="h-4 w-4" />
        ModelClaims
      </button>

      <div className="mb-6 flex items-start justify-between gap-4">
        <div className="min-w-0">
          <div className="mb-1 text-xs font-semibold uppercase tracking-widest text-teal-700">
            ModelClaim
          </div>
          <div className="mb-1 flex items-center gap-3">
            <h1 className="truncate text-3xl font-semibold tracking-tight text-gray-900">{claim.name}</h1>
            <ModelClaimPhase phase={claim.phase} deleting={claim.deleting} />
          </div>
          <p className="text-sm text-gray-500">
            Serves <strong className="font-mono text-gray-700">{claim.modelName}</strong> through the AIBrix gateway
            {claim.poolName ? <> from pool <strong className="text-gray-700">{claim.poolName}</strong></> : null}.
            {!claim.managedByConsole && ' Created outside the console.'}
          </p>
        </div>
        <div className="flex shrink-0 gap-3">
          <button
            type="button"
            onClick={() => copyValue(claim.modelName, 'Served model name copied')}
            className="inline-flex items-center gap-2 rounded-lg border border-gray-200 bg-white px-4 py-2.5 text-sm font-medium text-gray-700 hover:bg-gray-50"
          >
            <Copy className="h-4 w-4" />
            Copy model name
          </button>
          <button
            type="button"
            disabled={deleting || claim.deleting}
            onClick={handleDelete}
            className="rounded-lg border border-red-200 bg-white px-4 py-2.5 text-sm font-medium text-red-700 hover:bg-red-50 disabled:opacity-50"
          >
            {deleting || claim.deleting ? 'Deleting...' : 'Delete'}
          </button>
        </div>
      </div>

      {error && (
        <div className="mb-4 rounded-lg border border-red-200 bg-red-50 px-4 py-3 text-sm text-red-700">
          {error}
        </div>
      )}

      {claim.phase === 'Sleeping' ? (
        <div className="mb-4 flex items-start gap-3 rounded-lg border border-indigo-200 bg-indigo-50 px-4 py-3 text-sm text-indigo-800">
          <Moon className="mt-0.5 h-4 w-4 shrink-0" />
          <div>
            The engine is asleep and not routed. A request for {claim.modelName} asks the controller to wake it; until it is
            Active again the gateway answers 503 with Retry-After.
          </div>
        </div>
      ) : attention && (
        <div className={`mb-4 flex items-start gap-3 rounded-lg border px-4 py-3 text-sm ${
          claim.phase === 'Failed'
            ? 'border-red-200 bg-red-50 text-red-800'
            : 'border-amber-200 bg-amber-50 text-amber-800'
        }`}>
          <AlertTriangle className="mt-0.5 h-4 w-4 shrink-0" />
          <div className="min-w-0">
            <div className="font-semibold">{attention.type}: {attention.reason || attention.status}</div>
            {attention.message && <div className="mt-1 break-words">{attention.message}</div>}
          </div>
        </div>
      )}

      <div className="mb-5 grid overflow-hidden rounded-xl border border-gray-200 bg-white shadow-sm sm:grid-cols-5">
        <Stat label="Phase" value={claim.deleting ? 'Deleting' : claim.phase} />
        <Stat label="Ready" value={String(claim.readyReplicas)} />
        <Stat label="Desired" value={String(claim.desiredReplicas || 1)} />
        <Stat label="Candidates" value={String(claim.candidates)} />
        <Stat label="Age" value={formatResourceAge(claim.createdAt)} last />
      </div>

      <div className="grid items-start gap-5 xl:grid-cols-[minmax(0,1.55fr)_minmax(18rem,0.8fr)]">
        <div className="space-y-5">
          <section className="rounded-xl border border-gray-200 bg-white p-5 shadow-sm">
            <CardHeader title="Conditions" code="status.conditions" />
            {claim.conditions.length === 0 ? (
              <div className="rounded-lg border border-gray-200 px-4 py-6 text-center text-sm text-gray-400">
                The controller has not reported on this claim yet.
              </div>
            ) : (
              <div className="divide-y divide-gray-100 rounded-lg border border-gray-200">
                {claim.conditions.map((condition) => (
                  <div key={condition.type} className="grid gap-1 px-4 py-3 sm:grid-cols-[8rem_minmax(0,1fr)_5rem]">
                    <div className="text-sm font-medium text-gray-800">{condition.type}</div>
                    <div className="min-w-0">
                      <div className={`text-sm font-medium ${
                        condition.status === 'True' ? 'text-green-700' : 'text-amber-700'
                      }`}>
                        {condition.status}{condition.reason ? ` · ${condition.reason}` : ''}
                      </div>
                      {condition.message && (
                        <div className="mt-0.5 break-words text-xs text-gray-500">{condition.message}</div>
                      )}
                    </div>
                    <div className="text-xs text-gray-400 sm:text-right">
                      {formatResourceAge(condition.lastTransitionTime)}
                    </div>
                  </div>
                ))}
              </div>
            )}
          </section>

          <section className="rounded-xl border border-gray-200 bg-white p-5 shadow-sm">
            <CardHeader title="Configuration" code="spec" />
            <PropertyGrid
              items={[
                ['Served model name', claim.modelName, true],
                ['Engine', claim.engine],
                ['Pool', claim.poolName || 'Custom selector'],
                ['Pod selector', claim.podSelector || '-', true],
                ['Placement priority', String(claim.priority ?? 0)],
              ]}
            />
            <div className="mt-4">
              <div className="mb-2 text-xs font-semibold uppercase tracking-wider text-gray-400">Engine arguments</div>
              {engineArgs.length === 0 ? (
                <div className="text-sm text-gray-400">None</div>
              ) : (
                <div className="flex flex-wrap gap-2">
                  {engineArgs.map(([key, value]) => (
                    <code key={key} className="rounded bg-gray-100 px-2 py-1 font-mono text-xs text-gray-700">
                      {key}{value ? ` ${value}` : ''}
                    </code>
                  ))}
                </div>
              )}
            </div>
            <p className="mt-4 text-xs text-gray-400">
              The engine starts with these fields once, so they cannot change. To change one, create a new ModelClaim.
            </p>
          </section>
        </div>

        <aside className="space-y-5">
          <PerGpuCard claim={claim} onUpdated={(updated) => {
            setClaim(updated);
            showToast('GPU cost updated');
          }} />
          <section className="rounded-xl border border-gray-200 bg-white p-5 shadow-sm">
            <CardHeader title="Model artifact" code="spec.artifactURL" />
            <button
              type="button"
              onClick={() => copyValue(claim.artifactUrl, 'Artifact URL copied')}
              className="w-full break-all rounded-lg border border-teal-200 bg-teal-50 px-3 py-3 text-left font-mono text-xs leading-relaxed text-teal-800 hover:bg-teal-100/60"
            >
              {claim.artifactUrl}
            </button>
          </section>
        </aside>
      </div>

      <section className="mt-5 overflow-hidden rounded-xl border border-gray-200 bg-white shadow-sm">
        <div className="flex items-center justify-between border-b border-gray-200 px-5 py-4">
          <h2 className="font-semibold text-gray-900">Engine instances</h2>
          <span className="rounded bg-gray-100 px-2 py-1 font-mono text-xs text-gray-500">
            status.instances · {claim.instances.length}
          </span>
        </div>
        <div className="overflow-x-auto">
          <table className="w-full">
            <thead className="border-b border-gray-200 bg-gray-50">
              <tr>
                {['Pod', 'Instance', 'Route', 'KV limit', 'Pod status', 'Node'].map((label) => (
                  <th
                    key={label}
                    className="px-4 py-3 text-left text-xs font-semibold uppercase tracking-wider text-gray-500"
                  >
                    {label}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody className="divide-y divide-gray-100">
              {claim.instances.length === 0 ? (
                <tr>
                  <td colSpan={6} className="px-4 py-8 text-center text-sm text-gray-400">
                    Not placed on a Pod yet.
                  </td>
                </tr>
              ) : (
                claim.instances.map((instance) => <InstanceRow key={instance.pod} instance={instance} />)
              )}
            </tbody>
          </table>
        </div>
      </section>

      {toast && (
        <div className="fixed bottom-6 right-6 rounded-lg bg-slate-900 px-4 py-3 text-sm text-white shadow-xl">
          {toast}
        </div>
      )}
    </div>
  );
}

function InstanceRow({ instance }: { instance: ModelClaimInstance }) {
  const route = instance.route;
  return (
    <tr className="align-top">
      <td className="px-4 py-3 font-mono text-xs text-gray-700">
        {instance.pod}
        <div className="mt-1 text-gray-400">{instance.podStatus.podIp || '-'}</div>
      </td>
      <td className="px-4 py-3 text-sm text-gray-700">
        {instance.phase || '-'}
        <span className="text-gray-400"> · port {instance.port || '-'}</span>
        {instance.reason && <div className="mt-1 text-xs text-amber-700">{instance.reason}</div>}
      </td>
      <td className="px-4 py-3 text-sm text-gray-700">
        {route ? (
          <>
            {route.state}
            <span className="text-gray-400"> · {route.port > 0 ? `port ${route.port}` : 'not routed'}</span>
            {route.reason && <div className="mt-1 text-xs text-amber-700">{route.reason}</div>}
            {instance.wakeRequestedAt && (
              <div className="mt-1 text-xs text-indigo-700">
                Wake requested {formatResourceAge(instance.wakeRequestedAt)} ago
              </div>
            )}
          </>
        ) : (
          <span className="text-gray-400">No route</span>
        )}
      </td>
      <td className="px-4 py-3 text-sm text-gray-600">{formatBytes(instance.kvLimitBytes)}</td>
      <td className="px-4 py-3 text-sm text-gray-600">
        {instance.podStatus.status}
        <span className="text-gray-400"> · {instance.podStatus.ready} ready</span>
      </td>
      <td className="px-4 py-3 font-mono text-xs text-gray-600">{instance.podStatus.node || '-'}</td>
    </tr>
  );
}

function PerGpuCard({ claim, onUpdated }: { claim: ModelClaim; onUpdated: (claim: ModelClaim) => void }) {
  const [editing, setEditing] = useState(false);
  const [maximumFootprint, setMaximumFootprint] = useState('');
  const [kvFloor, setKvFloor] = useState('');
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const startEditing = () => {
    setMaximumFootprint(claim.perGpu?.maximumFootprint ?? '');
    setKvFloor(claim.perGpu?.kvFloor ?? '');
    setError(null);
    setEditing(true);
  };

  const footprintError = validatePerGpuFigure(maximumFootprint);
  const kvFloorError = validatePerGpuFigure(kvFloor);

  const save = async () => {
    if (saving || footprintError || kvFloorError) return;
    setSaving(true);
    setError(null);
    try {
      const updated = await updateModelClaimPerGpu(claim.name, {
        maximumFootprint: maximumFootprint.trim(),
        kvFloor: kvFloor.trim(),
      });
      setEditing(false);
      onUpdated(updated);
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to update GPU cost.');
    } finally {
      setSaving(false);
    }
  };

  return (
    <section className="rounded-xl border border-gray-200 bg-white p-5 shadow-sm">
      <div className="mb-4 flex items-center justify-between">
        <h2 className="font-semibold text-gray-900">GPU cost per instance</h2>
        {!editing && !claim.deleting && (
          <button
            type="button"
            onClick={startEditing}
            className="inline-flex items-center gap-1.5 rounded-md border border-gray-200 px-2 py-1 text-xs text-gray-600 hover:bg-gray-50"
          >
            <Pencil className="h-3.5 w-3.5" />
            Edit
          </button>
        )}
      </div>
      {editing ? (
        <div className="space-y-3">
          <PerGpuInput label="Maximum footprint" value={maximumFootprint} onChange={setMaximumFootprint} error={footprintError} />
          <PerGpuInput label="KV floor" value={kvFloor} onChange={setKvFloor} error={kvFloorError} />
          {error && <div className="rounded-md bg-red-50 px-3 py-2 text-xs text-red-700">{error}</div>}
          <div className="flex justify-end gap-2">
            <button
              type="button"
              onClick={() => setEditing(false)}
              className="rounded-lg border border-gray-200 px-3 py-1.5 text-sm text-gray-700 hover:bg-gray-50"
            >
              Cancel
            </button>
            <button
              type="button"
              disabled={saving || Boolean(footprintError || kvFloorError)}
              onClick={save}
              className="rounded-lg bg-teal-700 px-3 py-1.5 text-sm font-medium text-white hover:bg-teal-800 disabled:opacity-50"
            >
              {saving ? 'Saving...' : 'Save'}
            </button>
          </div>
        </div>
      ) : claim.perGpu ? (
        <PropertyGrid
          items={[
            ['Maximum footprint', claim.perGpu.maximumFootprint, true],
            ['KV floor', claim.perGpu.kvFloor, true],
          ]}
        />
      ) : (
        <div className="rounded-lg border border-amber-200 bg-amber-50 px-3 py-3 text-xs leading-relaxed text-amber-800">
          Not declared. The controller does not place a claim until it declares what one instance costs on a GPU.
        </div>
      )}
      <p className="mt-3 text-xs leading-relaxed text-gray-400">
        spec.perGPU is the only field that can change. Placement reads it on the next pass.
      </p>
    </section>
  );
}

function PerGpuInput({
  label,
  value,
  onChange,
  error,
}: {
  label: string;
  value: string;
  onChange: (value: string) => void;
  error: string | null;
}) {
  return (
    <label className="block">
      <span className="mb-1 block text-xs font-semibold uppercase tracking-wider text-gray-400">{label}</span>
      <input
        value={value}
        onChange={(event) => onChange(event.target.value)}
        placeholder="e.g. 30Gi"
        className={`w-full rounded-lg border px-3 py-2 font-mono text-sm focus:outline-none focus:ring-2 ${
          error ? 'border-red-300 focus:ring-red-200' : 'border-gray-200 focus:border-teal-500 focus:ring-teal-500/20'
        }`}
      />
      {error && <span className="mt-1 block text-xs text-red-600">{error}</span>}
    </label>
  );
}

function Stat({ label, value, last = false }: { label: string; value: string; last?: boolean }) {
  return (
    <div className={`px-4 py-3 ${last ? '' : 'border-r border-gray-200'}`}>
      <div className="mb-1 text-xs font-semibold uppercase tracking-wider text-gray-400">{label}</div>
      <div className="truncate text-sm font-semibold text-gray-800">{value}</div>
    </div>
  );
}

function CardHeader({ title, code }: { title: string; code: string }) {
  return (
    <div className="mb-4 flex items-center justify-between">
      <h2 className="font-semibold text-gray-900">{title}</h2>
      <span className="rounded bg-gray-100 px-2 py-1 font-mono text-xs text-gray-500">{code}</span>
    </div>
  );
}

function PropertyGrid({ items }: { items: Array<[string, string, boolean?]> }) {
  return (
    <div className="grid overflow-hidden rounded-lg border border-gray-200 sm:grid-cols-2">
      {items.map(([label, value, code], index) => (
        <div
          key={label}
          className={`min-w-0 p-3 ${
            index % 2 === 0 ? 'border-r border-gray-200' : ''
          } ${index < items.length - 2 ? 'border-b border-gray-200' : ''}`}
        >
          <div className="mb-1 text-xs font-semibold uppercase tracking-wider text-gray-400">{label}</div>
          <div className={`truncate text-sm font-medium text-gray-700 ${code ? 'font-mono text-xs' : ''}`} title={value}>
            {value}
          </div>
        </div>
      ))}
    </div>
  );
}
