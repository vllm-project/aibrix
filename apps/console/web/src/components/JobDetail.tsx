import { useState, useEffect } from 'react';
import { ChevronLeft, Copy, Trash2, CheckCircle, XCircle, Download } from 'lucide-react';
import { getJob } from '../utils/api';
import { Job, JobStatus } from '../data/mockData';

interface JobDetailProps {
  jobId: string | null;
  onBack: () => void;
}

const TERMINAL: JobStatus[] = ['completed', 'failed', 'expired', 'cancelled'];

function formatTimestamp(unixSec?: number): string {
  if (!unixSec) return '—';
  return new Date(unixSec * 1000).toLocaleString();
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

export function JobDetail({ jobId, onBack }: JobDetailProps) {
  const [job, setJob] = useState<Job | null>(null);
  const [loading, setLoading] = useState(true);
  const [loadError, setLoadError] = useState<string | null>(null);

  useEffect(() => {
    if (!jobId) return;
    let cancelled = false;
    let timer: ReturnType<typeof setTimeout> | null = null;
    const TERMINAL = new Set(['completed', 'failed', 'expired', 'cancelled']);

    const fetchJob = (initial: boolean) => {
      if (initial) {
        setLoading(true);
        setLoadError(null);
      }
      getJob(jobId)
        .then(j => {
          if (cancelled) return;
          setJob(j);
          if (!TERMINAL.has(j.status)) {
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
  const isTerminal = TERMINAL.includes(job.status);
  const isCompleted = job.status === 'completed';
  const counts = job.requestCounts;
  const usage = job.usage;
  const successPct = counts && counts.total > 0
    ? ((counts.completed / counts.total) * 100).toFixed(2)
    : '0.00';

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
              <button className="text-gray-400 hover:text-gray-600">
                <Copy className="w-4 h-4" />
              </button>
              <span className={`inline-flex px-2.5 py-1 text-xs rounded-full ${statusClass(job.status)}`}>
                {job.status}
              </span>
            </div>
          </div>

          <button className="w-10 h-10 rounded-lg border border-gray-200 flex items-center justify-center hover:bg-gray-50">
            <Trash2 className="w-4 h-4 text-red-500" />
          </button>
        </div>
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
                        ? `${(counts.completed / counts.total) * 100}%`
                        : '0%',
                    }}
                  ></div>
                </div>
                <div className="text-center mt-2 text-sm text-gray-500">
                  {counts ? `${counts.completed} / ${counts.total}` : 'Pending'}
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

              {isCompleted && (
                <div className="bg-white rounded-xl shadow-sm border border-gray-100 p-6">
                  <h2 className="text-lg mb-4">Token Usage</h2>

                  {usage ? (
                    <div className="grid grid-cols-3 gap-6">
                      <div>
                        <div className="text-sm text-gray-500 mb-2">Total tokens</div>
                        <div className="text-2xl">{usage.totalTokens.toLocaleString()}</div>
                      </div>
                      <div>
                        <div className="text-sm text-gray-500 mb-2">Input tokens</div>
                        <div className="text-2xl">{usage.inputTokens.toLocaleString()}</div>
                      </div>
                      <div>
                        <div className="text-sm text-gray-500 mb-2">Output tokens</div>
                        <div className="text-2xl">{usage.outputTokens.toLocaleString()}</div>
                      </div>
                    </div>
                  ) : (
                    <p className="text-sm text-gray-500">No usage data available.</p>
                  )}
                </div>
              )}
            </>
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
              {job.completedAt && (
                <div>
                  <div className="text-gray-500 mb-1">Completed</div>
                  <div className="text-gray-900">{formatTimestamp(job.completedAt)}</div>
                </div>
              )}
              {job.failedAt && (
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
              {job.inputDataset && <FileRow label="Input Dataset" fileId={job.inputDataset} />}
              {job.outputDataset && <FileRow label="Output Dataset" fileId={job.outputDataset} />}
              {job.errorDataset && <FileRow label="Error Dataset" fileId={job.errorDataset} />}
            </div>
          </div>
        </div>
      </div>
    </div>
  );
}

function FileRow({ label, fileId }: { label: string; fileId: string }) {
  return (
    <div>
      <div className="text-gray-500 mb-1">{label}</div>
      <div className="flex items-center gap-2">
        <code className="text-sm bg-gray-100 px-2 py-1 rounded-md">{fileId}</code>
        <a
          href={`/api/v1/files/${encodeURIComponent(fileId)}/content`}
          download
          className="inline-flex items-center gap-1 px-2 py-1 text-xs text-teal-600 hover:text-teal-700 hover:bg-teal-50 rounded-md transition-colors"
          title="Download file content"
        >
          <Download className="w-3.5 h-3.5" />
          Download
        </a>
      </div>
    </div>
  );
}
