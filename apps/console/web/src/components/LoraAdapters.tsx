import { useEffect, useMemo, useState } from 'react';
import { ChevronRight, Plus, Search } from 'lucide-react';
import { listModelAdapters } from '../utils/api';
import type { ModelAdapter } from '../utils/api';
import {
  formatResourceAge,
  MODEL_ADAPTER_REFRESH_INTERVAL_MS,
  ModelAdapterIcon,
  ModelAdapterPhase,
} from './LoraShared';

interface LoraAdaptersProps {
  onSelectAdapter: (name: string) => void;
  onCreateAdapter: () => void;
}

export function LoraAdapters({ onSelectAdapter, onCreateAdapter }: LoraAdaptersProps) {
  const [adapters, setAdapters] = useState<ModelAdapter[]>([]);
  const [search, setSearch] = useState('');
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let active = true;
    let requestInFlight = false;
    let initialLoad = true;

    const refresh = async () => {
      if (requestInFlight) return;
      requestInFlight = true;
      try {
        const items = await listModelAdapters();
        if (active) {
          setAdapters(items);
          setError(null);
        }
      } catch (err) {
        if (active) {
          setError(err instanceof Error ? err.message : 'Failed to load ModelAdapters.');
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
  }, []);

  const filteredAdapters = useMemo(() => {
    const query = search.trim().toLowerCase();
    if (!query) return adapters;
    return adapters.filter((adapter) => (
      `${adapter.name} ${adapter.baseModel} ${adapter.target?.name ?? ''}`
        .toLowerCase()
        .includes(query)
    ));
  }, [adapters, search]);

  return (
    <div className="mx-auto w-full max-w-6xl p-8">
      <div className="mb-6 flex items-start justify-between gap-4">
        <div>
          <div className="mb-1 text-xs font-semibold uppercase tracking-widest text-teal-700">
            Model adapters
          </div>
          <h1 className="mb-1 text-3xl font-semibold tracking-tight text-gray-900">
            LoRA deployments
          </h1>
          <p className="text-sm text-gray-500">
            Manage ModelAdapter resources and the base model deployments they attach to.
          </p>
        </div>
        <button
          type="button"
          onClick={onCreateAdapter}
          className="inline-flex items-center gap-2 rounded-lg bg-teal-700 px-4 py-2.5 text-sm font-medium text-white shadow-sm hover:bg-teal-800"
        >
          <Plus className="h-4 w-4" />
          Deploy LoRA
        </button>
      </div>

      <div className="relative mb-4">
        <Search className="absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-gray-400" />
        <input
          value={search}
          onChange={(event) => setSearch(event.target.value)}
          placeholder="Search ModelAdapter or base model deployment"
          className="w-full rounded-lg border border-gray-200 bg-white py-2.5 pl-10 pr-4 text-sm focus:border-teal-500 focus:outline-none focus:ring-2 focus:ring-teal-500/20"
        />
      </div>

      {error && (
        <div className="mb-4 rounded-lg border border-red-200 bg-red-50 px-4 py-3 text-sm text-red-700">
          {error}
        </div>
      )}

      <div className="overflow-hidden rounded-xl border border-gray-200 bg-white shadow-sm">
        <div className="overflow-x-auto">
          <table className="w-full table-fixed">
            <colgroup>
              <col className="w-[26%]" />
              <col className="w-[19%]" />
              <col className="w-[25%]" />
              <col className="w-[10%]" />
              <col className="w-[12%]" />
              <col className="w-[6%]" />
              <col className="w-[2%]" />
            </colgroup>
            <thead className="border-b border-gray-200 bg-gray-50">
              <tr>
                {['ModelAdapter', 'Base model', 'Base model deployment', 'Ready', 'Phase', 'Age', ''].map((label) => (
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
              {loading ? (
                <tr>
                  <td colSpan={7} className="px-4 py-10 text-center text-sm text-gray-400">
                    Loading ModelAdapters...
                  </td>
                </tr>
              ) : filteredAdapters.length === 0 ? (
                <tr>
                  <td colSpan={7} className="px-4 py-10 text-center text-sm text-gray-400">
                    No matching ModelAdapters.
                  </td>
                </tr>
              ) : (
                filteredAdapters.map((adapter) => (
                  <tr
                    key={adapter.name}
                    onClick={() => onSelectAdapter(adapter.name)}
                    className="cursor-pointer transition-colors hover:bg-gray-50"
                  >
                    <td className="px-4 py-4">
                      <div className="flex min-w-0 items-center gap-3">
                        <ModelAdapterIcon />
                        <div className="min-w-0">
                          <div className="truncate text-sm font-medium text-gray-900">
                            {adapter.name}
                          </div>
                          <div className="truncate font-mono text-xs text-gray-400">
                            {adapter.apiVersion}
                          </div>
                        </div>
                      </div>
                    </td>
                    <td className="truncate px-4 py-4 text-sm text-gray-700">
                      {adapter.baseModel || '-'}
                    </td>
                    <td className="px-4 py-4">
                      <div className="truncate text-sm font-medium text-gray-800">
                        {adapter.target?.name || 'Unavailable'}
                      </div>
                      <div className="truncate font-mono text-xs text-gray-400">
                        {adapter.target ? `${adapter.target.kind} · Kubernetes` : '-'}
                      </div>
                    </td>
                    <td className="px-4 py-4 text-sm font-medium text-gray-700">
                      {adapter.readyReplicas}
                      <span className="font-normal text-gray-400">
                        {' '}/ {adapter.desiredReplicas}
                      </span>
                    </td>
                    <td className="px-4 py-4">
                      <ModelAdapterPhase phase={adapter.phase} />
                    </td>
                    <td className="px-4 py-4 text-sm text-gray-500">
                      {formatResourceAge(adapter.createdAt)}
                    </td>
                    <td className="px-2 py-4 text-gray-400">
                      <ChevronRight className="h-4 w-4" />
                    </td>
                  </tr>
                ))
              )}
            </tbody>
          </table>
        </div>
      </div>
    </div>
  );
}
