import { useState, useEffect } from 'react';
import {
  AlertTriangle,
  CheckCircle,
  ChevronLeft,
  Clock,
  Copy,
  Download,
  Server,
  XCircle,
} from 'lucide-react';
import { cancelJob, getJob, getUserInfo } from '../utils/api';
import { canCancelBatchJob, copyBatchIdentifier, getBatchDatasetRows } from '../utils/batchProduct';
import { copyToClipboard } from '../utils/clipboard';
import { Job, JobEvent, JobStatus } from '../data/mockData';

interface JobDetailProps {
  jobId: string | null;
  onBack: () => void;
}

const TERMINAL_STATUSES = new Set<JobStatus>([
  'completed',
  'failed',
  'expired',
  'cancelled',
  'resource_failed',
  'submit_failed',
]);

const CONSOLE_METADATA_KEYS = new Set([
  'aibrix.console.display_name',
  'aibrix.console.created_by',
  'aibrix.console.template_name',
  'aibrix.console.template_version',
  'display_name',
]);

function formatTimestamp(unixSec?: number): string {
  if (!unixSec || unixSec <= 0) return '—';
  return new Date(unixSec * 1000).toLocaleString();
}

function formatCompactTime(unixSec?: number): string {
  if (!unixSec || unixSec <= 0) return '—';
  return new Date(unixSec * 1000).toLocaleTimeString(undefined, {
    hour: 'numeric',
    minute: '2-digit',
    second: '2-digit',
  });
}

function formatNumber(n?: number): string {
  return typeof n === 'number' ? n.toLocaleString() : '—';
}

function terminalStatus(status: JobStatus): boolean {
  return TERMINAL_STATUSES.has(status);
}

function progressCount(job: Job): number | null {
  const counts = job.requestCounts;
  if (!counts) return null;
  return counts.completed + counts.failed;
}

function getEvents(job: Job): JobEvent[] {
  if (job.events && job.events.length > 0) {
    return [...job.events].sort((a, b) => a.at - b.at);
  }
  const fallback: Array<Omit<JobEvent, 'at'> & { at?: number }> = [
    { id: 'created', label: 'Created', status: 'validating', source: 'mds', at: job.createdAt },
    { id: 'in_progress', label: 'In progress', status: 'in_progress', source: 'mds', at: job.inProgressAt },
    { id: 'finalizing', label: 'Finalizing', status: 'finalizing', source: 'mds', at: job.finalizingAt },
    { id: 'completed', label: 'Completed', status: 'completed', source: 'mds', at: job.completedAt },
    { id: 'failed', label: 'Failed', status: 'failed', source: 'mds', at: job.failedAt },
    { id: 'expired', label: 'Expired', status: 'expired', source: 'mds', at: job.expiredAt },
    { id: 'cancelling', label: 'Cancelling', status: 'cancelling', source: 'mds', at: job.cancellingAt },
    { id: 'cancelled', label: 'Cancelled', status: 'cancelled', source: 'mds', at: job.cancelledAt },
    { id: 'resource_failed', label: 'Provision failed', status: 'resource_failed', source: 'planner', at: job.resourceFailedAt },
    { id: 'submit_failed', label: 'Submit failed', status: 'submit_failed', source: 'planner', at: job.submitFailedAt },
  ].filter((event): event is JobEvent => typeof event.at === 'number' && event.at > 0);
  return fallback.sort((a, b) => a.at - b.at);
}

function eventDotClass(status: string): string {
  if (status === 'completed') return 'bg-emerald-500 ring-emerald-100';
  if (status === 'failed' || status === 'resource_failed' || status === 'submit_failed') {
    return 'bg-red-500 ring-red-100';
  }
  if (status === 'cancelled' || status === 'expired') return 'bg-gray-400 ring-gray-100';
  return 'bg-amber-500 ring-amber-100';
}

function visibleMetadata(job: Job): [string, string][] {
  return Object.entries(job.metadata || {})
    .filter(([key]) => !CONSOLE_METADATA_KEYS.has(key))
    .sort(([a], [b]) => a.localeCompare(b));
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

export function JobDetail({ jobId, onBack }: JobDetailProps) {
  const [job, setJob] = useState<Job | null>(null);
  const [loading, setLoading] = useState(true);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [confirmCancel, setConfirmCancel] = useState(false);
  const [cancellingJob, setCancellingJob] = useState(false);
  const [cancelError, setCancelError] = useState<string | null>(null);
  const [copyFeedback, setCopyFeedback] = useState<'Copied' | 'Copy failed' | null>(null);
  // Current viewer; datasets are downloadable only by the job owner.
  const [currentUser, setCurrentUser] = useState('');

  useEffect(() => {
    let cancelled = false;
    getUserInfo()
      .then(u => {
        if (!cancelled && u) setCurrentUser((u.email || u.username || u.id || '').toLowerCase());
      })
      .catch(() => {
        // Not authenticated or fetch failed; downloads stay disabled.
      });
    return () => {
      cancelled = true;
    };
  }, []);

  useEffect(() => {
    if (!jobId) return;
    let cancelled = false;
    let timer: ReturnType<typeof setTimeout> | null = null;

    const fetchJob = (initial: boolean) => {
      if (initial) {
        setLoading(true);
        setLoadError(null);
      }
      getJob(jobId)
        .then(j => {
          if (cancelled) return;
          setJob(j);
          if (!terminalStatus(j.status)) {
            timer = setTimeout(() => fetchJob(false), 5000);
          }
        })
        .catch(err => {
          if (cancelled) return;
          console.error('Failed to fetch job:', err);
          if (initial) {
            setLoadError(err instanceof Error ? err.message : String(err));
            setJob(null);
          }
          timer = setTimeout(() => fetchJob(false), 10000);
        })
        .finally(() => {
          if (!cancelled && initial) setLoading(false);
        });
    };

    fetchJob(true);
    return () => {
      cancelled = true;
      if (timer) clearTimeout(timer);
    };
  }, [jobId]);

  const handleCancelJob = async () => {
    if (!job || cancellingJob) return;
    setCancellingJob(true);
    setCancelError(null);
    try {
      const next = await cancelJob(job.id);
      setJob(next);
      setConfirmCancel(false);
    } catch (err) {
      setCancelError(err instanceof Error ? err.message : String(err));
    } finally {
      setCancellingJob(false);
    }
  };

  const handleCopyJobId = async () => {
    if (!job) return;
    const feedback = await copyBatchIdentifier(job.id, copyToClipboard);
    setCopyFeedback(feedback);
    setTimeout(() => setCopyFeedback(null), 1800);
  };

  if (loading) {
    return (
      <div className="p-8">
        <button onClick={onBack} className="flex items-center gap-2 text-sm text-gray-500 hover:text-gray-900 mb-4">
          <ChevronLeft className="w-4 h-4" /> Back
        </button>
        <p className="text-sm text-gray-500">Loading...</p>
      </div>
    );
  }

  if (!job) {
    return (
      <div className="p-8">
        <button onClick={onBack} className="flex items-center gap-2 text-sm text-gray-500 hover:text-gray-900 mb-4">
          <ChevronLeft className="w-4 h-4" /> Back
        </button>
        {loadError ? (
          <p className="text-sm text-red-600">Failed to load job: {loadError}</p>
        ) : (
          <p>Job not found</p>
        )}
      </div>
    );
  }

  const displayName = job.name || job.id;
  const isTerminal = terminalStatus(job.status);
  const counts = job.requestCounts;
  const usage = job.usage;
  const doneCount = progressCount(job);
  const successPct = counts && counts.total > 0
    ? ((counts.completed / counts.total) * 100).toFixed(2)
    : '0.00';
  // Only the owner may download datasets (input/output/error); other users are
  // view-only. This is a UX guard and must also be enforced server-side.
  const isOwner = !!currentUser && (job.createdBy || '').toLowerCase() === currentUser;
  const viewerIsKnownNonOwner = !!currentUser && !!job.createdBy && (job.createdBy || '').toLowerCase() !== currentUser;
  const canCancel = canCancelBatchJob(job.status) && !viewerIsKnownNonOwner && !cancellingJob;
  const cancelDisabledReason = isTerminal
    ? 'Terminal jobs cannot be cancelled'
    : viewerIsKnownNonOwner
      ? 'Only the job owner can cancel this batch'
      : 'Cancel batch';
  const events = getEvents(job);
  const metadataEntries = visibleMetadata(job);
  const datasetRows = getBatchDatasetRows(job);

  return (
    <div className="p-8">
      <div className="mb-6">
        <button onClick={onBack} className="flex items-center gap-2 text-sm text-gray-500 hover:text-gray-900 mb-4">
          <ChevronLeft className="w-4 h-4" />
          Batch Inference / <span className="text-gray-400">{job.id}</span>
        </button>

        <div className="flex items-center justify-between">
          <div>
            <h1 className="text-2xl mb-2">{displayName}</h1>
            <div className="flex items-center gap-3">
              <code className="text-sm text-gray-600 bg-gray-100 px-2 py-1 rounded-md">{job.id}</code>
              <button
                type="button"
                onClick={handleCopyJobId}
                className="inline-flex items-center gap-1 text-gray-400 hover:text-gray-600"
                title="Copy batch id"
              >
                <Copy className="w-4 h-4" />
                {copyFeedback && <span className="text-xs">{copyFeedback}</span>}
              </button>
              <span className={`inline-flex px-2.5 py-1 text-xs rounded-full ${statusClass(job.status)}`}>
                {job.status}
              </span>
            </div>
          </div>

          <button
            type="button"
            onClick={() => setConfirmCancel(true)}
            disabled={!canCancel}
            title={cancelDisabledReason}
            className="inline-flex items-center gap-2 px-3 py-2 rounded-lg border border-gray-200 text-sm text-red-600 hover:bg-red-50 disabled:opacity-40 disabled:cursor-not-allowed disabled:hover:bg-white"
          >
            <XCircle className="w-4 h-4" />
            Cancel batch
          </button>
        </div>

        {confirmCancel && (
          <div className="mt-4 flex items-start justify-between gap-4 rounded-lg border border-red-200 bg-red-50 p-4">
            <div>
              <div className="text-sm font-medium text-red-800">Cancel this batch?</div>
              <p className="text-xs text-red-700 mt-1">
                In-flight requests may keep running briefly. Completed results remain available if the backend returns output files.
              </p>
              {cancelError && <p className="text-xs text-red-700 mt-2">Failed to cancel: {cancelError}</p>}
            </div>
            <div className="flex items-center gap-2 shrink-0">
              <button
                type="button"
                onClick={() => {
                  setConfirmCancel(false);
                  setCancelError(null);
                }}
                disabled={cancellingJob}
                className="px-3 py-1.5 rounded-lg border border-red-200 bg-white text-xs text-red-700 hover:bg-red-50 disabled:opacity-40"
              >
                Keep running
              </button>
              <button
                type="button"
                onClick={handleCancelJob}
                disabled={cancellingJob}
                className="px-3 py-1.5 rounded-lg bg-red-600 text-xs text-white hover:bg-red-700 disabled:opacity-50"
              >
                {cancellingJob ? 'Cancelling...' : 'Confirm cancel'}
              </button>
            </div>
          </div>
        )}
      </div>

      <div className="grid grid-cols-1 lg:grid-cols-3 gap-6">
        <div className="lg:col-span-2 space-y-6">
          {!isTerminal && (
            <div className="bg-white rounded-xl shadow-sm border border-gray-100 p-6">
              <div className="flex items-center gap-2 mb-4">
                <div className="w-5 h-5 rounded-full border-2 border-gray-300 border-t-teal-600 animate-spin"></div>
                <span className="text-sm capitalize">{job.status.replace('_', ' ')}</span>
              </div>

              <div className="relative">
                <div className="h-2 bg-gray-100 rounded-full overflow-hidden">
                  <div
                    className="h-full bg-teal-500 rounded-full transition-all"
                    style={{
                      width: counts && counts.total > 0
                        ? `${((doneCount ?? 0) / counts.total) * 100}%`
                        : '0%',
                    }}
                  ></div>
                </div>
                <div className="text-center mt-2 text-sm text-gray-500">
                  {counts ? `${doneCount ?? 0} / ${counts.total}` : 'Pending'}
                </div>
              </div>

              <p className="text-center text-sm text-gray-500 mt-4">Batch is in progress.</p>
            </div>
          )}

          {isTerminal && (
            <>
              <div className="bg-white rounded-xl shadow-sm border border-gray-100 p-6">
                <h2 className="text-lg mb-4">Processing Summary</h2>

                <div className="mb-6">
                  <div className="text-sm text-gray-500 mb-2">Total requests</div>
                  <div className="text-3xl">{counts?.total ?? '—'}</div>

                  <div className="grid grid-cols-2 gap-4 mt-4">
                    <div className="flex items-start gap-2">
                      <CheckCircle className="w-5 h-5 text-emerald-500 mt-1" />
                      <div>
                        <div className="text-sm text-gray-500">Completed</div>
                        <div className="text-xl">{counts?.completed ?? '—'}</div>
                        {counts && counts.total > 0 && (
                          <div className="text-sm text-emerald-600">{successPct}%</div>
                        )}
                      </div>
                    </div>

                    <div className="flex items-start gap-2">
                      <XCircle className="w-5 h-5 text-red-500 mt-1" />
                      <div>
                        <div className="text-sm text-gray-500">Failed</div>
                        <div className="text-xl">{counts?.failed ?? '—'}</div>
                      </div>
                    </div>
                  </div>
                </div>
              </div>

            </>
          )}

          {(job.errorMessage || (job.errors && job.errors.length > 0)) && (
            <div className="bg-white rounded-xl shadow-sm border border-gray-100 p-6">
              <div className="flex items-center gap-2 mb-4">
                <AlertTriangle className="w-5 h-5 text-red-500" />
                <h2 className="text-lg">Errors</h2>
              </div>
              {job.errorMessage && (
                <p className="text-sm text-red-700 bg-red-50 border border-red-100 rounded-lg px-3 py-2 mb-3">
                  {job.errorMessage}
                </p>
              )}
              {job.errors && job.errors.length > 0 && (
                <div className="space-y-2">
                  {job.errors.map((err, idx) => (
                    <div key={`${err.code}-${idx}`} className="border border-gray-100 rounded-lg p-3">
                      <div className="flex items-center justify-between gap-3">
                        <code className="text-xs bg-gray-100 px-2 py-1 rounded-md text-gray-700">
                          {err.code || 'error'}
                        </code>
                        {err.line && err.line > 0 ? <span className="text-xs text-gray-400">line {err.line}</span> : null}
                      </div>
                      <p className="text-sm text-gray-800 mt-2">{err.message || '—'}</p>
                      {err.param && <p className="text-xs text-gray-400 mt-1">param: {err.param}</p>}
                    </div>
                  ))}
                </div>
              )}
            </div>
          )}

          <div className="bg-white rounded-xl shadow-sm border border-gray-100 p-6">
            <div className="flex items-center gap-2 mb-5">
              <Clock className="w-5 h-5 text-teal-600" />
              <h2 className="text-lg">Timeline</h2>
            </div>
            {events.length > 0 ? (
              <div className="space-y-4">
                {events.map((event) => (
                  <div key={`${event.id}-${event.at}`} className="flex gap-3">
                    <div className="pt-1">
                      <div className={`w-3 h-3 rounded-full ring-4 ${eventDotClass(event.status)}`} />
                    </div>
                    <div className="min-w-0 flex-1 pb-4 border-b border-gray-100 last:border-b-0 last:pb-0">
                      <div className="flex items-center justify-between gap-3">
                        <div className="text-sm text-gray-900">{event.label}</div>
                        <div className="text-xs text-gray-400 whitespace-nowrap">{formatCompactTime(event.at)}</div>
                      </div>
                      <div className="mt-1 flex items-center gap-2">
                        <span className="text-xs text-gray-500">{formatTimestamp(event.at)}</span>
                        <span className="text-xs px-1.5 py-0.5 rounded bg-gray-100 text-gray-500">
                          {event.source}
                        </span>
                      </div>
                      {event.message && <p className="text-xs text-gray-500 mt-1">{event.message}</p>}
                    </div>
                  </div>
                ))}
              </div>
            ) : (
              <p className="text-sm text-gray-500">No timeline data available.</p>
            )}
          </div>

          {usage && (
            <div className="bg-white rounded-xl shadow-sm border border-gray-100 p-6">
              <h2 className="text-lg mb-4">Token Usage</h2>
              <div className="grid grid-cols-3 gap-6">
                <div>
                  <div className="text-sm text-gray-500 mb-2">Total tokens</div>
                  <div className="text-2xl">{formatNumber(usage.totalTokens)}</div>
                </div>
                <div>
                  <div className="text-sm text-gray-500 mb-2">Input tokens</div>
                  <div className="text-2xl">{formatNumber(usage.inputTokens)}</div>
                </div>
                <div>
                  <div className="text-sm text-gray-500 mb-2">Output tokens</div>
                  <div className="text-2xl">{formatNumber(usage.outputTokens)}</div>
                </div>
              </div>
            </div>
          )}
        </div>

        <div className="space-y-6">
          <div className="bg-white rounded-xl shadow-sm border border-gray-100 p-6">
            <h2 className="text-lg mb-4">Info</h2>

            <div className="space-y-3 text-sm">
              <div>
                <div className="text-gray-500 mb-1">Created By</div>
                <div className="text-gray-900">{job.createdBy || '—'}</div>
              </div>
              <div>
                <div className="text-gray-500 mb-1">Created</div>
                <div className="text-gray-900">{formatTimestamp(job.createdAt)}</div>
              </div>
              <div>
                <div className="text-gray-500 mb-1">Expires</div>
                <div className="text-gray-900">{formatTimestamp(job.expiresAt)}</div>
              </div>
              <div>
                <div className="text-gray-500 mb-1">Completion Window</div>
                <div className="text-gray-900">{job.completionWindow || '—'}</div>
              </div>
              {job.batchId && (
                <div>
                  <div className="text-gray-500 mb-1">MDS Batch ID</div>
                  <code className="text-sm bg-gray-100 px-2 py-1 rounded-md break-all">{job.batchId}</code>
                </div>
              )}
              {(job.completedAt ?? 0) > 0 && (
                <div>
                  <div className="text-gray-500 mb-1">Completed</div>
                  <div className="text-gray-900">{formatTimestamp(job.completedAt)}</div>
                </div>
              )}
              {(job.failedAt ?? 0) > 0 && (
                <div>
                  <div className="text-gray-500 mb-1">Failed</div>
                  <div className="text-gray-900">{formatTimestamp(job.failedAt)}</div>
                </div>
              )}
              <div>
                <div className="text-gray-500 mb-1">Display Name</div>
                <div className="text-gray-900">{displayName}</div>
              </div>
            </div>
          </div>

          <div className="bg-white rounded-xl shadow-sm border border-gray-100 p-6">
            <div className="flex items-center gap-2 mb-4">
              <Server className="w-5 h-5 text-teal-600" />
              <h2 className="text-lg">Execution</h2>
            </div>

            <div className="space-y-3 text-sm">
              <div>
                <div className="text-gray-500 mb-1">Runtime</div>
                <div className="text-gray-900">{job.runtime?.target || '—'}</div>
              </div>
              {job.runtime?.options && Object.keys(job.runtime.options).length > 0 && (
                <KeyValueRows entries={Object.entries(job.runtime.options)} />
              )}
              <div>
                <div className="text-gray-500 mb-1">Provision</div>
                <code className="text-sm bg-gray-100 px-2 py-1 rounded-md break-all">
                  {job.provisionId || job.resourceAllocation?.provisionId || '—'}
                </code>
              </div>
              {job.provision && (
                <>
                  <div className="grid grid-cols-2 gap-3">
                    <div>
                      <div className="text-gray-500 mb-1">Provider</div>
                      <div className="text-gray-900">{job.provision.provider || '—'}</div>
                    </div>
                    <div>
                      <div className="text-gray-500 mb-1">Status</div>
                      <div className="text-gray-900">{job.provision.status || '—'}</div>
                    </div>
                  </div>
                  {job.provision.region && (
                    <div>
                      <div className="text-gray-500 mb-1">Region</div>
                      <div className="text-gray-900 break-all">{job.provision.region}</div>
                    </div>
                  )}
                  {(job.provision.updatedAt ?? 0) > 0 && (
                    <div>
                      <div className="text-gray-500 mb-1">Provision Updated</div>
                      <div className="text-gray-900">{formatTimestamp(job.provision.updatedAt)}</div>
                    </div>
                  )}
                </>
              )}
              {job.resourceAllocation?.resourceDetails?.map((detail, idx) => (
                <div key={`${detail.endpointCluster || 'resource'}-${idx}`} className="rounded-lg border border-gray-100 p-3">
                  <div className="text-xs text-gray-500 mb-2">Resource {idx + 1}</div>
                  <div className="grid grid-cols-2 gap-3">
                    <div>
                      <div className="text-gray-500 mb-1">GPU</div>
                      <div className="text-gray-900">{detail.gpuType || '—'}</div>
                    </div>
                    <div>
                      <div className="text-gray-500 mb-1">Replicas</div>
                      <div className="text-gray-900">{detail.replica ?? '—'}</div>
                    </div>
                  </div>
                  {detail.endpointCluster && (
                    <div className="mt-2">
                      <div className="text-gray-500 mb-1">Cluster</div>
                      <div className="text-gray-900 break-all">{detail.endpointCluster}</div>
                    </div>
                  )}
                </div>
              ))}
            </div>
          </div>

          <div className="bg-white rounded-xl shadow-sm border border-gray-100 p-6">
            <h2 className="text-lg mb-4">Model and Datasets</h2>

            <div className="space-y-3 text-sm">
              <div>
                <div className="text-gray-500 mb-1">Model</div>
                <code className="text-sm bg-gray-100 px-2 py-1 rounded-md">{job.model || '—'}</code>
              </div>
              <div>
                <div className="text-gray-500 mb-1">Endpoint</div>
                <code className="text-sm bg-gray-100 px-2 py-1 rounded-md">{job.endpoint}</code>
              </div>
              {job.modelTemplateName && (
                <div>
                  <div className="text-gray-500 mb-1">Deployment Template</div>
                  <code className="text-sm bg-gray-100 px-2 py-1 rounded-md">
                    {job.modelTemplateName}
                    {job.modelTemplateVersion ? ` @ ${job.modelTemplateVersion}` : ''}
                  </code>
                </div>
              )}
              {job.profile?.name && (
                <div>
                  <div className="text-gray-500 mb-1">Batch Profile</div>
                  <code className="text-sm bg-gray-100 px-2 py-1 rounded-md">{job.profile.name}</code>
                </div>
              )}
              {!isOwner && datasetRows.some(row => row.fileId) && (
                <p className="text-xs text-gray-400">View only — only the job owner can download datasets.</p>
              )}
              {datasetRows.length > 0 ? (
                datasetRows.map(row => (
                  <FileRow
                    key={row.key}
                    label={row.label}
                    fileId={row.fileId}
                    canDownload={isOwner}
                    unavailableReason={row.unavailableReason}
                  />
                ))
              ) : (
                <p className="text-xs text-gray-400">No datasets linked to this batch.</p>
              )}
            </div>
          </div>

          {metadataEntries.length > 0 && (
            <div className="bg-white rounded-xl shadow-sm border border-gray-100 p-6">
              <h2 className="text-lg mb-4">Metadata</h2>
              <KeyValueRows entries={metadataEntries} />
            </div>
          )}
        </div>
      </div>
    </div>
  );
}

function KeyValueRows({ entries }: { entries: [string, string][] }) {
  return (
    <div className="space-y-2">
      {entries.map(([key, value]) => (
        <div key={key} className="grid grid-cols-[minmax(0,0.9fr)_minmax(0,1.1fr)] gap-3 text-xs">
          <div className="text-gray-500 truncate" title={key}>{key}</div>
          <div className="text-gray-900 break-all">{value || '—'}</div>
        </div>
      ))}
    </div>
  );
}

function FileRow({
  label,
  fileId,
  canDownload,
  unavailableReason,
}: {
  label: string;
  fileId: string;
  canDownload: boolean;
  unavailableReason?: string;
}) {
  const hasFile = fileId.trim() !== '';
  const disabledTitle = unavailableReason || 'Only the job owner can download files';
  return (
    <div>
      <div className="text-gray-500 mb-1">{label}</div>
      <div className="flex items-center gap-2">
        {hasFile ? (
          <code className="text-sm bg-gray-100 px-2 py-1 rounded-md break-all">{fileId}</code>
        ) : (
          <span className="text-xs text-gray-400 bg-gray-50 border border-gray-100 px-2 py-1 rounded-md">
            Not available
          </span>
        )}
        {canDownload && hasFile ? (
          <a
            href={`/api/v1/files/${encodeURIComponent(fileId)}/content`}
            download
            className="inline-flex items-center gap-1 px-2 py-1 text-xs text-teal-600 hover:text-teal-700 hover:bg-teal-50 rounded-md transition-colors"
            title="Download file content"
          >
            <Download className="w-3.5 h-3.5" />
            Download
          </a>
        ) : (
          <span
            aria-disabled="true"
            className="inline-flex items-center gap-1 px-2 py-1 text-xs text-gray-300 rounded-md cursor-not-allowed select-none"
            title={disabledTitle}
          >
            <Download className="w-3.5 h-3.5" />
            Download
          </span>
        )}
      </div>
    </div>
  );
}
