import { useState, useEffect, useMemo } from 'react';
import { Search, ChevronDown } from 'lucide-react';
import { listJobs } from '../utils/api';
import { Job, JobStatus } from '../data/mockData';

interface BatchJobsListProps {
  onSelectJob: (id: string) => void;
  onCreateJob: () => void;
}

const STATUS_OPTIONS: ('All' | JobStatus)[] = [
  'All',
  'pending',
  'provisioning',
  'validating',
  'in_progress',
  'finalizing',
  'completed',
  'failed',
  'expired',
  'cancelling',
  'cancelled',
];

function formatDate(unixSec: number): { date: string; time: string } {
  const d = new Date(unixSec * 1000);
  return {
    date: d.toLocaleDateString(undefined, { month: 'short', day: 'numeric', year: 'numeric' }),
    time: d.toLocaleTimeString(undefined, { hour: 'numeric', minute: '2-digit' }),
  };
}

function statusClass(s: JobStatus): string {
  switch (s) {
    case 'completed':
      return 'bg-emerald-50 text-emerald-700 border border-emerald-200';
    case 'pending':
    case 'provisioning':
    case 'in_progress':
    case 'validating':
    case 'finalizing':
      return 'bg-amber-50 text-amber-700 border border-amber-200';
    case 'cancelling':
    case 'cancelled':
    case 'expired':
      return 'bg-gray-50 text-gray-700 border border-gray-200';
    case 'failed':
      return 'bg-red-50 text-red-700 border border-red-200';
  }
}

export function BatchJobsList({ onSelectJob, onCreateJob }: BatchJobsListProps) {
  const [jobs, setJobs] = useState<Job[]>([]);
  const [loading, setLoading] = useState(true);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [searchQuery, setSearchQuery] = useState('');
  const [statusFilter, setStatusFilter] = useState<'' | JobStatus>('');
  const [showStatusDropdown, setShowStatusDropdown] = useState(false);

  useEffect(() => {
    let cancelled = false;
    let timer: ReturnType<typeof setTimeout> | null = null;

    const fetchJobs = (initial: boolean) => {
      if (initial) {
        setLoading(true);
        setLoadError(null);
      }
      listJobs()
        .then(res => {
          if (cancelled) return;
          const next = res.jobs ?? [];
          setJobs(next);
          // Poll while any job is in a non-terminal state.
          const TERMINAL = new Set(['completed', 'failed', 'expired', 'cancelled']);
          const hasActive = next.some(j => !TERMINAL.has(j.status));
          if (hasActive) {
            timer = setTimeout(() => fetchJobs(false), 5000);
          }
        })
        .catch(err => {
          if (cancelled) return;
          console.error('Failed to fetch jobs:', err);
          if (initial) {
            setLoadError(err instanceof Error ? err.message : String(err));
            setJobs([]);
          }
          // Keep polling on transient errors so the page recovers when MDS comes back.
          timer = setTimeout(() => fetchJobs(false), 10000);
        })
        .finally(() => {
          if (!cancelled && initial) setLoading(false);
        });
    };

    fetchJobs(true);
    return () => {
      cancelled = true;
      if (timer) clearTimeout(timer);
    };
  }, []);

  const filtered = useMemo(() => {
    const q = searchQuery.trim().toLowerCase();
    return jobs.filter(j => {
      if (statusFilter && j.status !== statusFilter) return false;
      if (!q) return true;
      return (
        j.id.toLowerCase().includes(q) ||
        (j.name || '').toLowerCase().includes(q) ||
        (j.model || '').toLowerCase().includes(q) ||
        (j.createdBy || '').toLowerCase().includes(q)
      );
    });
  }, [jobs, searchQuery, statusFilter]);

  return (
    <div className="p-8">
      <div className="mb-6 flex items-start justify-between">
        <div>
          <h1 className="text-2xl mb-2">Batch Inference Jobs</h1>
          <p className="text-sm text-gray-500">View your past batch inference jobs or create new ones.</p>
          {loadError && !loading && (
            <p className="text-xs text-red-600 mt-1">Failed to load jobs: {loadError}</p>
          )}
        </div>
        <button
          onClick={onCreateJob}
          className="px-4 py-2 bg-teal-600 text-white rounded-lg text-sm hover:bg-teal-700 transition-colors"
        >
          Create Batch Inference Job
        </button>
      </div>

      <div className="mb-6 flex items-center gap-4">
        <div className="flex-1 relative">
          <Search className="absolute left-3 top-1/2 transform -translate-y-1/2 w-4 h-4 text-gray-400" />
          <input
            type="text"
            placeholder="Search by id, name, model, or created by"
            value={searchQuery}
            onChange={(e) => setSearchQuery(e.target.value)}
            className="w-full pl-10 pr-4 py-2 border border-gray-200 rounded-lg text-sm focus:outline-none focus:ring-2 focus:ring-teal-500/30 focus:border-teal-500 bg-white"
          />
        </div>

        <div className="relative">
          <div
            onClick={() => setShowStatusDropdown(!showStatusDropdown)}
            className="flex items-center gap-2 px-4 py-2 border border-gray-200 rounded-lg text-sm cursor-pointer hover:bg-gray-50 bg-white"
          >
            <span className="text-gray-500">Status:</span>
            <span>{statusFilter || 'All'}</span>
            <ChevronDown className="w-4 h-4" />
          </div>
          {showStatusDropdown && (
            <div className="absolute z-10 right-0 mt-1 w-44 bg-white border border-gray-200 rounded-lg shadow-lg overflow-hidden">
              {STATUS_OPTIONS.map((option) => (
                <button
                  key={option}
                  onClick={() => {
                    setStatusFilter(option === 'All' ? '' : option);
                    setShowStatusDropdown(false);
                  }}
                  className="w-full px-4 py-2 text-left text-sm hover:bg-gray-50"
                >
                  {option}
                </button>
              ))}
            </div>
          )}
        </div>
      </div>

      <div className="bg-white rounded-xl shadow-sm border border-gray-100 overflow-hidden">
        <div className="overflow-x-auto">
          <table className="w-full">
            <thead className="bg-gray-50/80 border-b border-gray-100">
              <tr>
                <th className="px-6 py-3 text-left text-xs text-gray-500 uppercase tracking-wider">Batch</th>
                <th className="px-6 py-3 text-left text-xs text-gray-500 uppercase tracking-wider">Model</th>
                <th className="px-6 py-3 text-left text-xs text-gray-500 uppercase tracking-wider">Input dataset</th>
                <th className="px-6 py-3 text-left text-xs text-gray-500 uppercase tracking-wider">Created</th>
                <th className="px-6 py-3 text-left text-xs text-gray-500 uppercase tracking-wider">Created by</th>
                <th className="px-6 py-3 text-left text-xs text-gray-500 uppercase tracking-wider">Requests</th>
                <th className="px-6 py-3 text-left text-xs text-gray-500 uppercase tracking-wider">Status</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-gray-50">
              {loading ? (
                <tr>
                  <td colSpan={7} className="px-6 py-12 text-center text-sm text-gray-500">Loading...</td>
                </tr>
              ) : filtered.length === 0 ? (
                <tr>
                  <td colSpan={7} className="px-6 py-12 text-center text-sm text-gray-500">No jobs found.</td>
                </tr>
              ) : (
                filtered.map((job) => {
                  const created = formatDate(job.createdAt);
                  const counts = job.requestCounts;
                  return (
                    <tr
                      key={job.id}
                      className="hover:bg-gray-50/50 cursor-pointer transition-colors"
                      onClick={() => onSelectJob(job.id)}
                    >
                      <td className="px-6 py-4">
                        <div className="text-sm text-gray-900">{job.name || job.id}</div>
                        <div className="text-xs text-gray-400">ID: {job.id}</div>
                      </td>
                      <td className="px-6 py-4">
                        <div className="text-sm text-gray-900">{job.model || '—'}</div>
                        <div className="text-xs text-gray-400">{job.endpoint}</div>
                      </td>
                      <td className="px-6 py-4 text-sm text-gray-900">{job.inputDataset}</td>
                      <td className="px-6 py-4 text-sm text-gray-500">
                        <div>{created.date}</div>
                        <div className="text-xs text-gray-400">{created.time}</div>
                      </td>
                      <td className="px-6 py-4 text-sm text-gray-500">{job.createdBy || '—'}</td>
                      <td className="px-6 py-4 text-sm text-gray-500">
                        {counts ? `${counts.completed}/${counts.total}` : '—'}
                        {counts && counts.failed > 0 && (
                          <span className="ml-1 text-red-500">({counts.failed} failed)</span>
                        )}
                      </td>
                      <td className="px-6 py-4">
                        <span className={`inline-flex px-2.5 py-1 text-xs rounded-full ${statusClass(job.status)}`}>
                          {job.status}
                        </span>
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
