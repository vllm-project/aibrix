import { useEffect, useMemo, useState } from 'react';
import { ChevronRight, Plus, Search } from 'lucide-react';
import { APIError, listModelClaims } from '../utils/api';
import type { ModelClaim } from '../utils/api';
import { MODEL_CLAIM_REFRESH_INTERVAL_MS, modelClaimAttention } from '../utils/modelClaim';
import { formatResourceAge } from './LoraShared';
import { ModelClaimIcon, ModelClaimPhase } from './ModelClaimShared';

interface ModelClaimsProps {
  onSelectClaim: (name: string) => void;
  onCreateClaim: () => void;
}

export function ModelClaims({ onSelectClaim, onCreateClaim }: ModelClaimsProps) {
  const [claims, setClaims] = useState<ModelClaim[]>([]);
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
        const items = await listModelClaims();
        if (active) {
          setClaims(items);
          setError(null);
        }
      } catch (err) {
        if (active) {
          setError(
            err instanceof APIError && err.status === 404
              ? 'The ModelClaim API is not available in this cluster. Install the ModelClaim CRD and enable the ModelClaim controller.'
              : err instanceof Error ? err.message : 'Failed to load ModelClaims.',
          );
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
  }, []);

  const filteredClaims = useMemo(() => {
    const query = search.trim().toLowerCase();
    if (!query) return claims;
    return claims.filter((claim) => (
      `${claim.name} ${claim.modelName} ${claim.poolName} ${claim.artifactUrl}`
        .toLowerCase()
        .includes(query)
    ));
  }, [claims, search]);

  return (
    <div className="mx-auto w-full max-w-6xl p-8">
      <div className="mb-6 flex items-start justify-between gap-4">
        <div>
          <div className="mb-1 text-xs font-semibold uppercase tracking-widest text-teal-700">
            High-density serving
          </div>
          <h1 className="mb-1 text-3xl font-semibold tracking-tight text-gray-900">
            ModelClaims
          </h1>
          <p className="text-sm text-gray-500">
            Models attached to warm GPU runtime pools, several to a GPU. Claims applied with kubectl are listed too.
          </p>
        </div>
        <button
          type="button"
          onClick={onCreateClaim}
          className="inline-flex items-center gap-2 rounded-lg bg-teal-700 px-4 py-2.5 text-sm font-medium text-white shadow-sm hover:bg-teal-800"
        >
          <Plus className="h-4 w-4" />
          Create ModelClaim
        </button>
      </div>

      <div className="relative mb-4">
        <Search className="absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-gray-400" />
        <input
          value={search}
          onChange={(event) => setSearch(event.target.value)}
          placeholder="Search ModelClaim, served model, pool, or artifact"
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
              <col className="w-[27%]" />
              <col className="w-[17%]" />
              <col className="w-[9%]" />
              <col className="w-[8%]" />
              <col className="w-[31%]" />
              <col className="w-[6%]" />
              <col className="w-[2%]" />
            </colgroup>
            <thead className="border-b border-gray-200 bg-gray-50">
              <tr>
                {['ModelClaim', 'Pool', 'Engine', 'Ready', 'Phase', 'Age', ''].map((label) => (
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
                    Loading ModelClaims...
                  </td>
                </tr>
              ) : filteredClaims.length === 0 ? (
                <tr>
                  <td colSpan={7} className="px-4 py-10 text-center text-sm text-gray-400">
                    No matching ModelClaims.
                  </td>
                </tr>
              ) : (
                filteredClaims.map((claim) => {
                  const attention = modelClaimAttention(claim);
                  return (
                    <tr
                      key={claim.name}
                      onClick={() => onSelectClaim(claim.name)}
                      className="cursor-pointer transition-colors hover:bg-gray-50"
                    >
                      <td className="px-4 py-4">
                        <div className="flex min-w-0 items-center gap-3">
                          <ModelClaimIcon />
                          <div className="min-w-0">
                            <div className="truncate text-sm font-medium text-gray-900">
                              {claim.name}
                            </div>
                            <div className="truncate font-mono text-xs text-gray-400">
                              {claim.modelName}
                            </div>
                          </div>
                        </div>
                      </td>
                      <td className="px-4 py-4">
                        <div className="truncate text-sm font-medium text-gray-800">
                          {claim.poolName || 'Custom selector'}
                        </div>
                        {!claim.poolName && (
                          <div className="truncate font-mono text-xs text-gray-400">{claim.podSelector || '-'}</div>
                        )}
                      </td>
                      <td className="truncate px-4 py-4 text-sm text-gray-700">{claim.engine}</td>
                      <td className="px-4 py-4 text-sm font-medium text-gray-700">
                        {claim.readyReplicas}
                        <span className="font-normal text-gray-400">
                          {' '}/ {claim.desiredReplicas || 1}
                        </span>
                      </td>
                      <td className="px-4 py-4">
                        <ModelClaimPhase phase={claim.phase} deleting={claim.deleting} />
                        {attention && (
                          <div className="mt-1 truncate text-xs text-gray-500" title={attention.message}>
                            {attention.reason}
                            {attention.message ? ` · ${attention.message}` : ''}
                          </div>
                        )}
                      </td>
                      <td className="px-4 py-4 text-sm text-gray-500">
                        {formatResourceAge(claim.createdAt)}
                      </td>
                      <td className="px-2 py-4 text-gray-400">
                        <ChevronRight className="h-4 w-4" />
                      </td>
                    </tr>
                  );
                })
              )}
            </tbody>
          </table>
        </div>
      </div>
    </div>
  );
}
