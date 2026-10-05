import { useEffect, useState } from 'react';
import {
  ChevronLeft,
  Copy,
  Link,
  Server,
} from 'lucide-react';
import {
  APIError,
  deleteModelAdapter,
  getModelAdapter,
} from '../utils/api';
import type { ModelAdapter } from '../utils/api';
import { copyToClipboard } from '../utils/clipboard';
import {
  formatResourceAge,
  MODEL_ADAPTER_REFRESH_INTERVAL_MS,
  ModelAdapterPhase,
} from './LoraShared';

interface LoraAdapterDetailProps {
  adapterName: string;
  onBack: () => void;
}

export function LoraAdapterDetail({ adapterName, onBack }: LoraAdapterDetailProps) {
  const [adapter, setAdapter] = useState<ModelAdapter | null>(null);
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
        const result = await getModelAdapter(adapterName);
        if (active) {
          setAdapter(result);
          setError(null);
        }
      } catch (err) {
        if (active) {
          setError(err instanceof Error ? err.message : 'Failed to load ModelAdapter.');
          if (err instanceof APIError && err.status === 404) {
            setAdapter(null);
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
    }, MODEL_ADAPTER_REFRESH_INTERVAL_MS);

    return () => {
      active = false;
      window.clearInterval(interval);
    };
  }, [adapterName]);

  const showToast = (message: string) => {
    setToast(message);
    window.setTimeout(() => setToast(null), 1800);
  };

  const copyValue = async (value: string, message: string) => {
    await copyToClipboard(value);
    showToast(message);
  };

  const handleDelete = async () => {
    if (!adapter || deleting) return;
    if (!window.confirm(`Delete ModelAdapter "${adapter.name}"? This will unload it from all running instances.`)) {
      return;
    }
    setDeleting(true);
    setError(null);
    try {
      await deleteModelAdapter(adapter.name);
      onBack();
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to delete ModelAdapter.');
      setDeleting(false);
    }
  };

  if (loading) {
    return <div className="p-8 text-sm text-gray-400">Loading ModelAdapter...</div>;
  }
  if (!adapter) {
    return (
      <div className="p-8">
        <button type="button" onClick={onBack} className="mb-4 text-sm text-teal-700">
          Back to LoRA deployments
        </button>
        <div className="rounded-lg border border-red-200 bg-red-50 px-4 py-3 text-sm text-red-700">
          {error || 'ModelAdapter not found.'}
        </div>
      </div>
    );
  }

  const target = adapter.target;

  return (
    <div className="mx-auto w-full max-w-6xl p-8">
      <button
        type="button"
        onClick={onBack}
        className="mb-4 inline-flex items-center gap-1 text-sm text-gray-500 hover:text-gray-900"
      >
        <ChevronLeft className="h-4 w-4" />
        LoRA deployments
      </button>

      <div className="mb-6 flex items-start justify-between gap-4">
        <div>
          <div className="mb-1 text-xs font-semibold uppercase tracking-widest text-teal-700">
            ModelAdapter
          </div>
          <div className="mb-1 flex items-center gap-3">
            <h1 className="text-3xl font-semibold tracking-tight text-gray-900">
              {adapter.name}
            </h1>
            <ModelAdapterPhase phase={adapter.phase} />
          </div>
          <p className="text-sm text-gray-500">
            LoRA adapter attached to base model <strong className="text-gray-700">{adapter.baseModel}</strong>.
          </p>
        </div>
        <div className="flex gap-3">
          <button
            type="button"
            onClick={() => copyValue(adapter.name, 'Model ID copied')}
            className="inline-flex items-center gap-2 rounded-lg border border-gray-200 bg-white px-4 py-2.5 text-sm font-medium text-gray-700 hover:bg-gray-50"
          >
            <Copy className="h-4 w-4" />
            Copy model ID
          </button>
          <button
            type="button"
            disabled={deleting}
            onClick={handleDelete}
            className="rounded-lg border border-red-200 bg-white px-4 py-2.5 text-sm font-medium text-red-700 hover:bg-red-50 disabled:opacity-50"
          >
            {deleting ? 'Deleting...' : 'Delete'}
          </button>
        </div>
      </div>

      {error && (
        <div className="mb-4 rounded-lg border border-red-200 bg-red-50 px-4 py-3 text-sm text-red-700">
          {error}
        </div>
      )}

      <div className="mb-5 grid overflow-hidden rounded-xl border border-gray-200 bg-white shadow-sm sm:grid-cols-5">
        <Stat label="Phase" value={adapter.phase} />
        <Stat label="Ready" value={String(adapter.readyReplicas)} />
        <Stat label="Desired" value={String(adapter.desiredReplicas)} />
        <Stat label="Candidates" value={String(adapter.candidates)} />
        <Stat label="Age" value={formatResourceAge(adapter.createdAt)} last />
      </div>

      <div className="grid items-start gap-5 xl:grid-cols-[minmax(0,1.55fr)_minmax(18rem,0.8fr)]">
        <div className="space-y-5">
          <section className="rounded-xl border border-gray-200 bg-white p-5 shadow-sm">
            <CardHeader title="ModelAdapter configuration" code="spec" />
            <PropertyGrid
              items={[
                ['Base model', adapter.baseModel || '-'],
                ['Replicas', adapter.placement === 'all' ? 'All matching Pods' : '1'],
                ['Pod selector', adapter.podSelector || '-', true],
                ['Scheduler', adapter.schedulerName || 'default'],
              ]}
            />
          </section>

          <section className="rounded-xl border border-gray-200 bg-white p-5 shadow-sm">
            <div className="mb-4 flex items-center justify-between">
              <h2 className="font-semibold text-gray-900">Base model deployment</h2>
              <span className="rounded-md border border-teal-100 bg-teal-50 px-2 py-1 text-xs font-medium text-teal-700">
                Kubernetes
              </span>
            </div>
            {target ? (
              <>
                <div className="mb-4 flex items-start gap-3 rounded-lg border border-gray-200 bg-gray-50 p-4">
                  <span className="flex h-9 w-9 shrink-0 items-center justify-center rounded-lg border border-gray-200 bg-white text-teal-700">
                    <Server className="h-4 w-4" />
                  </span>
                  <div className="min-w-0 flex-1">
                    <div className="truncate text-sm font-semibold text-gray-900">
                      {target.name}
                    </div>
                    <div className="mt-1 truncate text-xs text-gray-500">
                      {target.baseModel} · {target.engine}
                    </div>
                  </div>
                  <span className="rounded-md border border-gray-200 bg-white px-2 py-1 text-xs text-gray-600">
                    {target.kind}
                  </span>
                  <span className="rounded-full bg-green-50 px-2.5 py-1 text-xs font-medium text-green-700">
                    {target.readyReplicas > 0 ? 'Ready' : 'Pending'}
                  </span>
                </div>
                <PropertyGrid
                  items={[
                    ['Resource type', target.kind],
                    ['API version', target.apiVersion],
                    ['Ready replicas', `${target.readyReplicas} / ${target.desiredReplicas}`],
                    ['Update strategy', target.updateStrategy || '-'],
                    ['Engine', target.engine],
                    ['Namespace', target.namespace],
                  ]}
                />
              </>
            ) : (
              <div className="rounded-lg border border-amber-200 bg-amber-50 px-4 py-3 text-sm text-amber-700">
                The referenced Deployment is no longer available.
              </div>
            )}
          </section>
        </div>

        <aside className="rounded-xl border border-gray-200 bg-white p-5 shadow-sm">
          <CardHeader title="LoRA artifact" code="spec.artifactURL" />
          <button
            type="button"
            onClick={() => copyValue(adapter.artifactUrl, 'Artifact URL copied')}
            className="flex w-full items-start gap-2 rounded-lg border border-teal-200 bg-teal-50 px-3 py-3 text-left font-mono text-xs leading-relaxed text-teal-800 hover:bg-teal-100/60"
          >
            <Link className="mt-0.5 h-4 w-4 shrink-0" />
            <span className="break-all">{adapter.artifactUrl}</span>
          </button>
          <p className="mt-3 text-xs leading-relaxed text-gray-400">
            The ModelAdapter controller loads adapter weights from this artifact URL.
          </p>
        </aside>
      </div>

      <section className="mt-5 overflow-hidden rounded-xl border border-gray-200 bg-white shadow-sm">
        <div className="flex items-center justify-between border-b border-gray-200 px-5 py-4">
          <h2 className="font-semibold text-gray-900">Bound Pods</h2>
          <span className="rounded bg-gray-100 px-2 py-1 font-mono text-xs text-gray-500">
            status.instances · {adapter.instances.length} pods
          </span>
        </div>
        <div className="overflow-x-auto">
          <table className="w-full">
            <thead className="border-b border-gray-200 bg-gray-50">
              <tr>
                {['Name', 'Ready', 'Status', 'Restarts', 'Age', 'Pod IP', 'Node'].map((label) => (
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
              {adapter.instances.length === 0 ? (
                <tr>
                  <td colSpan={7} className="px-4 py-8 text-center text-sm text-gray-400">
                    No Pods are currently bound.
                  </td>
                </tr>
              ) : (
                adapter.instances.map((pod) => (
                  <tr key={pod.name}>
                    <td className="px-4 py-3 font-mono text-xs text-gray-700">{pod.name}</td>
                    <td className="px-4 py-3 text-sm text-gray-600">{pod.ready}</td>
                    <td className="px-4 py-3 text-sm font-medium text-green-700">{pod.status}</td>
                    <td className="px-4 py-3 text-sm text-gray-600">{pod.restarts}</td>
                    <td className="px-4 py-3 text-sm text-gray-600">{formatResourceAge(pod.createdAt)}</td>
                    <td className="px-4 py-3 font-mono text-xs text-gray-600">{pod.podIp || '-'}</td>
                    <td className="px-4 py-3 font-mono text-xs text-gray-600">{pod.node || '-'}</td>
                  </tr>
                ))
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
          <div className="mb-1 text-xs font-semibold uppercase tracking-wider text-gray-400">
            {label}
          </div>
          <div className={`truncate text-sm font-medium text-gray-700 ${code ? 'font-mono text-xs' : ''}`}>
            {value}
          </div>
        </div>
      ))}
    </div>
  );
}
