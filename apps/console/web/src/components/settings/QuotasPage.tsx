import { useState } from 'react';
import { Search, Copy, MoreVertical } from 'lucide-react';

interface Quota {
  id: string;
  name: string;
  quotaId: string;
  currentUsage: number;
  usagePercentage: number;
  quota: number;
}

export function QuotasPage() {
  const [searchQuery, setSearchQuery] = useState('');

  const quotas: Quota[] = [
    {
      id: '1',
      name: 'Deployed Model Count',
      quotaId: 'deployed-model-count',
      currentUsage: 0,
      usagePercentage: 0,
      quota: 100,
    },
    {
      id: '2',
      name: 'Eval Protocol Free Daily Credits',
      quotaId: 'eval-protocol-free-daily-credits',
      currentUsage: 0,
      usagePercentage: 0,
      quota: 0,
    },
    {
      id: '3',
      name: 'GLOBAL - A100 Count',
      quotaId: 'global--a100-count',
      currentUsage: 0,
      usagePercentage: 0,
      quota: 16,
    },
    {
      id: '4',
      name: 'GLOBAL - B200 Count',
      quotaId: 'global--b200-count',
      currentUsage: 0,
      usagePercentage: 0,
      quota: 16,
    },
    {
      id: '5',
      name: 'GLOBAL - H100 Count',
      quotaId: 'global--h100-count',
      currentUsage: 1,
      usagePercentage: 6.25,
      quota: 16,
    },
    {
      id: '6',
      name: 'GLOBAL - H200 Count',
      quotaId: 'global--h200-count',
      currentUsage: 0,
      usagePercentage: 0,
      quota: 16,
    },
    {
      id: '7',
      name: 'Job Submission Count',
      quotaId: 'job-submission-count',
      currentUsage: 0,
      usagePercentage: 0,
      quota: 8,
    },
  ];

  const filteredQuotas = quotas.filter(
    (q) =>
      q.name.toLowerCase().includes(searchQuery.toLowerCase()) ||
      q.quotaId.toLowerCase().includes(searchQuery.toLowerCase())
  );

  return (
    <div className="p-8">
      {/* Header */}
      <div className="mb-8">
        <h1 className="text-2xl mb-1">Quotas</h1>
        <p className="text-sm text-gray-500">
          Quotas are service limits for your account.{' '}
          <a href="#" className="text-teal-600 hover:underline">Learn more</a>
        </p>
      </div>

      {/* Search */}
      <div className="relative mb-6 w-56">
        <Search className="absolute left-3 top-1/2 transform -translate-y-1/2 w-4 h-4 text-gray-400" />
        <input
          type="text"
          value={searchQuery}
          onChange={(e) => setSearchQuery(e.target.value)}
          placeholder="Search"
          className="w-full pl-10 pr-4 py-2 border border-gray-200 rounded-lg text-sm focus:outline-none focus:ring-2 focus:ring-teal-500/30 focus:border-teal-500 bg-white"
        />
      </div>

      {/* Table */}
      <div className="bg-white rounded-xl shadow-sm border border-gray-100 overflow-hidden">
        <table className="w-full">
          <thead>
            <tr className="border-b border-gray-100 bg-gray-50/80">
              <th className="text-left px-6 py-3 text-sm text-gray-500">
                <div className="flex items-center gap-1">
                  Name
                  <svg className="w-3 h-3 text-gray-400" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                    <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M7 16V4m0 0L3 8m4-4l4 4m6 0v12m0 0l4-4m-4 4l-4-4" />
                  </svg>
                </div>
              </th>
              <th className="text-left px-6 py-3 text-sm text-gray-500">
                <div className="flex items-center gap-1">
                  Current usage
                  <svg className="w-3 h-3 text-gray-400" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                    <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M7 16V4m0 0L3 8m4-4l4 4m6 0v12m0 0l4-4m-4 4l-4-4" />
                  </svg>
                </div>
              </th>
              <th className="text-left px-6 py-3 text-sm text-gray-500">Current usage percentage</th>
              <th className="text-left px-6 py-3 text-sm text-gray-500">
                <div className="flex items-center gap-1">
                  Quota
                  <svg className="w-3 h-3 text-gray-400" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                    <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M7 16V4m0 0L3 8m4-4l4 4m6 0v12m0 0l4-4m-4 4l-4-4" />
                  </svg>
                </div>
              </th>
              <th className="w-10"></th>
            </tr>
          </thead>
          <tbody>
            {filteredQuotas.map((quota) => (
              <tr key={quota.id} className="border-b border-gray-50 hover:bg-gray-50/50 transition-colors">
                <td className="px-6 py-4">
                  <div className="text-sm">{quota.name}</div>
                  <div className="text-xs text-gray-400 flex items-center gap-1 mt-0.5">
                    ID: {quota.quotaId}
                    <button className="hover:text-gray-600">
                      <Copy className="w-3 h-3" />
                    </button>
                  </div>
                </td>
                <td className="px-6 py-4">
                  <span className={`text-sm ${quota.quotaId === 'eval-protocol-free-daily-credits' && quota.currentUsage === 0 ? 'text-red-500' : ''}`}>
                    {quota.currentUsage}
                  </span>
                </td>
                <td className="px-6 py-4">
                  {quota.quota === 0 ? (
                    <span className="text-sm text-gray-400">-</span>
                  ) : (
                    <div className="flex items-center gap-3">
                      <div className="w-28 h-2 bg-gray-100 rounded-full overflow-hidden">
                        <div
                          className={`h-full rounded-full ${
                            quota.usagePercentage > 0 ? 'bg-teal-500' : 'bg-gray-200'
                          }`}
                          style={{ width: `${Math.max(quota.usagePercentage, quota.usagePercentage > 0 ? 5 : 0)}%` }}
                        />
                      </div>
                      <span className="text-sm text-gray-500">{quota.usagePercentage}%</span>
                    </div>
                  )}
                </td>
                <td className="px-6 py-4 text-sm text-gray-900">{quota.quota}</td>
                <td className="px-4 py-4">
                  <button className="text-gray-300 hover:text-gray-500">
                    <MoreVertical className="w-4 h-4" />
                  </button>
                </td>
              </tr>
            ))}
            {filteredQuotas.length === 0 && (
              <tr>
                <td colSpan={5} className="px-6 py-8 text-center text-sm text-gray-400">
                  No quotas found
                </td>
              </tr>
            )}
          </tbody>
        </table>
      </div>
    </div>
  );
}
