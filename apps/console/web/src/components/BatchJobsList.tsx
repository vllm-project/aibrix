import { useState, useEffect, useMemo } from 'react';
import { Search, ChevronDown, ChevronLeft, ChevronRight } from 'lucide-react';
import { listJobs, getUserInfo } from '../utils/api';
import { Job, JobStatus } from '../data/mockData';

interface BatchJobsListProps {
  onSelectJob: (id: string) => void;
  onCreateJob: () => void;
}

const STATUS_OPTIONS: ('All' | JobStatus)[] = [
  'All',
  'queued',
  'resource_preparing',
  'submitting',
  'validating',
  'in_progress',
  'finalizing',
  'completed',
  'failed',
  'expired',
  'cancelling',
  'cancelled',
  'resource_failed',
  'submit_failed',
];

const PAGE_SIZE = 20;

const TERMINAL_STATUSES = new Set<JobStatus>([
  'completed',
  'failed',
  'expired',
  'cancelled',
  'resource_failed',
  'submit_failed',
]);

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
    case 'queued':
    case 'resource_preparing':
    case 'submitting':
    case 'validating':
    case 'in_progress':
    case 'finalizing':
      return 'bg-amber-50 text-amber-700 border border-amber-200';
    case 'cancelling':
    case 'cancelled':
    case 'expired':
      return 'bg-gray-50 text-gray-700 border border-gray-200';
    case 'failed':
    case 'resource_failed':
    case 'submit_failed':
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
  const [currentPage, setCurrentPage] = useState(1);
  const [mineOnly, setMineOnly] = useState(false);
  // Current user's identity used to match job ownership. The BFF stamps
  // job.createdBy from the user's email, so we compare against that.
  const [currentUser, setCurrentUser] = useState('');

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
          const hasActive = next.some(j => !TERMINAL_STATUSES.has(j.status));
          if (hasActive) {
            timer = setTimeout(() => fetchJobs(false), 120000);
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

  useEffect(() => {
    let cancelled = false;
    getUserInfo()
      .then(u => {
        if (!cancelled && u) setCurrentUser((u.email || u.username || u.id || '').toLowerCase());
      })
      .catch(() => {
        // Not authenticated or fetch failed; the "Only my jobs" filter stays disabled.
      });
    return () => {
      cancelled = true;
    };
  }, []);

  const filtered = useMemo(() => {
    const q = searchQuery.trim().toLowerCase();
    return jobs.filter(j => {
      if (statusFilter && j.status !== statusFilter) return false;
      if (mineOnly && (j.createdBy || '').toLowerCase() !== currentUser) return false;
      if (!q) return true;
      return (
        j.id.toLowerCase().includes(q) ||
        (j.name || '').toLowerCase().includes(q) ||
        (j.model || '').toLowerCase().includes(q) ||
        (j.createdBy || '').toLowerCase().includes(q)
      );
    });
  }, [jobs, searchQuery, statusFilter, mineOnly, currentUser]);

  const totalPages = Math.max(1, Math.ceil(filtered.length / PAGE_SIZE));

  // Reset to the first page whenever the result set changes.
  useEffect(() => {
    setCurrentPage(1);
  }, [searchQuery, statusFilter, mineOnly]);

  // Keep the current page in range if the result set shrinks (e.g. after polling).
  useEffect(() => {
    setCurrentPage(p => Math.min(p, totalPages));
  }, [totalPages]);

  const pageStart = (currentPage - 1) * PAGE_SIZE;
  const paged = filtered.slice(pageStart, pageStart + PAGE_SIZE);

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

        <label
          title={currentUser ? undefined : 'Sign in to filter your jobs'}
          className={`flex items-center gap-2 px-4 py-2 border border-gray-200 rounded-lg text-sm bg-white select-none ${
            currentUser ? 'cursor-pointer hover:bg-gray-50' : 'opacity-50 cursor-not-allowed'
          }`}
        >
          <input
            type="checkbox"
            checked={mineOnly}
            disabled={!currentUser}
            onChange={(e) => setMineOnly(e.target.checked)}
            className="w-4 h-4 accent-teal-600"
          />
          Only my jobs
        </label>
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
                paged.map((job, idx) => {
                  const created = formatDate(job.createdAt);
                  const counts = job.requestCounts;
                  const clickable = !!job.id;
                  return (
                    <tr
                      key={job.id || `row-${idx}`}
                      className={`transition-colors ${clickable ? 'hover:bg-gray-50/50 cursor-pointer' : 'opacity-60'}`}
                      onClick={clickable ? () => onSelectJob(job.id) : undefined}
                    >
                      <td className="px-6 py-4">
                        <div className="text-sm text-gray-900">{job.name || job.id}</div>
                        <div className="text-xs text-gray-400">ID: {job.id || '—'}</div>
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

        {!loading && filtered.length > 0 && (
          <div className="flex items-center justify-between px-6 py-3 border-t border-gray-100 bg-gray-50/50">
            <p className="text-xs text-gray-500">
              Showing {pageStart + 1}–{Math.min(pageStart + PAGE_SIZE, filtered.length)} of {filtered.length}
            </p>
            <div className="flex items-center gap-2">
              <button
                onClick={() => setCurrentPage(p => Math.max(1, p - 1))}
                disabled={currentPage <= 1}
                className="flex items-center gap-1 px-3 py-1.5 border border-gray-200 rounded-lg text-sm bg-white hover:bg-gray-50 disabled:opacity-40 disabled:cursor-not-allowed"
              >
                <ChevronLeft className="w-4 h-4" />
                Prev
              </button>
              <span className="text-sm text-gray-600">
                Page {currentPage} of {totalPages}
              </span>
              <button
                onClick={() => setCurrentPage(p => Math.min(totalPages, p + 1))}
                disabled={currentPage >= totalPages}
                className="flex items-center gap-1 px-3 py-1.5 border border-gray-200 rounded-lg text-sm bg-white hover:bg-gray-50 disabled:opacity-40 disabled:cursor-not-allowed"
              >
                Next
                <ChevronRight className="w-4 h-4" />
              </button>
            </div>
          </div>
        )}
      </div>
    </div>
  );
}
