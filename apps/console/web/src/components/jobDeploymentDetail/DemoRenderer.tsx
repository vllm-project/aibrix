import { useState, useEffect, useCallback, useRef } from 'react';
import {
  ChevronDown,
  ChevronRight,
  Copy,
  CheckCircle,
  XCircle,
  RefreshCw,
  Search,
  Filter,
  Monitor,
  FileCode2,
  X,
} from 'lucide-react';
import * as Dialog from '@radix-ui/react-dialog';
import * as Tooltip from '@radix-ui/react-tooltip';
import { Prism as SyntaxHighlighter } from 'react-syntax-highlighter';
import { oneDark } from 'react-syntax-highlighter/dist/esm/styles/prism';
import { copyToClipboard } from '../../utils/clipboard';
import { registerDeploymentDetailRenderer, DeploymentDetailProps } from './registry';
import type { JobDeploymentDetail, JobDeploymentWorkload, JobDeploymentPod } from '../../data/mockData';

function formatAge(isoOrUnix: string | number | undefined): string {
  if (!isoOrUnix) return '';
  const date = typeof isoOrUnix === 'number'
    ? new Date(isoOrUnix * 1000)
    : new Date(isoOrUnix);
  if (isNaN(date.getTime())) return '';
  const diffMs = Date.now() - date.getTime();
  if (diffMs < 0) return '0s';
  const sec = Math.floor(diffMs / 1000);
  if (sec < 60) return `${sec}s`;
  const min = Math.floor(sec / 60);
  if (min < 60) return `${min}m`;
  const hr = Math.floor(min / 60);
  if (hr < 24) return `${hr}h`;
  const day = Math.floor(hr / 24);
  return `${day}d`;
}

function parseDeploymentFromJob(extraBody: Record<string, string> | undefined): JobDeploymentDetail | null {
  if (!extraBody) return null;
  const deploymentRaw = extraBody['deployment'];
  if (deploymentRaw) {
    try {
      const parsed = JSON.parse(deploymentRaw);
      if (parsed && typeof parsed === 'object' && parsed.type) return parsed;
    } catch { /* ignore */ }
  }
  const aibrixRaw = extraBody['aibrix'];
  if (aibrixRaw) {
    try {
      const aibrix = JSON.parse(aibrixRaw);
      if (aibrix.deployment) {
        const parsed = typeof aibrix.deployment === 'string' ? JSON.parse(aibrix.deployment) : aibrix.deployment;
        if (parsed && typeof parsed === 'object' && parsed.type) return parsed;
      }
    } catch { /* ignore */ }
  }
  return null;
}

function GrafanaIcon({ className }: { className?: string }) {
  return (
    <svg className={className} viewBox="0 0 16 16" xmlns="http://www.w3.org/2000/svg" fill="none">
      <path fill="url(#grafana-gradient)" d="M13.985 7.175a4.408 4.408 0 00-.138-.802 5.035 5.035 0 00-1.054-1.998 2.96 2.96 0 00-.366-.393c.198-.787-.245-1.468-.245-1.468-.764-.046-1.237.227-1.42.363-.031-.015-.062-.03-.092-.03-.122-.046-.26-.106-.397-.137-.138-.045-.275-.075-.413-.12-.137-.031-.29-.061-.443-.092-.03 0-.046 0-.076-.015C9.005 1.44 8.058 1 8.058 1 7.004 1.666 6.79 2.604 6.79 2.604s0 .015-.016.06l-.183.046c-.076.03-.168.06-.244.076-.077.03-.168.06-.245.09-.153.076-.32.152-.473.228-.153.09-.306.181-.443.272-.016-.015-.03-.015-.03-.015-1.467-.545-2.766.136-2.766.136-.122 1.544.58 2.528.733 2.71-.03.09-.06.196-.091.287a8.104 8.104 0 00-.245 1.09c0 .06-.015.106-.015.166C1.397 8.386 1 9.748 1 9.748c1.13 1.287 2.46 1.377 2.46 1.377.167.303.366.575.58.848.092.106.183.212.29.318a3.014 3.014 0 00.061 2.149c1.268.045 2.093-.545 2.261-.681.122.045.26.076.382.106.382.106.78.151 1.176.181h.49c.595.848 1.634.954 1.634.954.748-.772.779-1.544.779-1.71v-.015-.03-.03c.153-.107.305-.228.443-.35a5.37 5.37 0 00.779-.892c.015-.03.046-.06.061-.09.84.045 1.436-.515 1.436-.515-.138-.863-.642-1.287-.749-1.378l-.015-.015h-.015s-.015 0-.015-.015c0-.045.015-.106.015-.151 0-.091.015-.182.015-.288V9.4v-.166-.076-.152l-.015-.075c-.015-.091-.03-.197-.061-.288a3.506 3.506 0 00-.428-1.044 3.856 3.856 0 00-.718-.848 3.784 3.784 0 00-.901-.575 3.347 3.347 0 00-.993-.272c-.168-.015-.336-.03-.504-.03H9.37 9.204c-.092.015-.169.015-.26.03-.336.06-.642.181-.932.348-.275.166-.52.363-.718.605a2.579 2.579 0 00-.459.757 2.63 2.63 0 00-.183.817v.393c.015.137.046.273.077.394.076.258.183.485.336.666.137.197.32.348.504.485.183.12.382.212.58.272.199.06.382.076.565.076h.244c.031 0 .047 0 .062-.015.015 0 .046-.015.061-.015.046-.016.076-.016.122-.03l.23-.092a.869.869 0 00.198-.12c.015-.016.03-.03.046-.03a.129.129 0 00.015-.198c-.046-.06-.122-.075-.183-.03-.015.015-.03.015-.046.03-.046.03-.107.046-.168.06l-.183.046c-.03 0-.061.015-.092.015H8.73a1.519 1.519 0 01-.825-.378 1.452 1.452 0 01-.306-.378 1.655 1.655 0 01-.168-.485c-.015-.09-.015-.166-.015-.257v-.106-.03c0-.046.015-.091.015-.136.061-.364.26-.727.55-1 .077-.075.153-.136.23-.181.076-.06.167-.106.259-.151.092-.046.183-.076.29-.106a.993.993 0 01.306-.046h.321c.107.015.229.03.336.046.214.045.427.12.626.242.397.212.733.56.947.969.107.211.183.423.214.65.015.06.015.121.015.167v.363c0 .06-.015.121-.015.182 0 .06-.015.12-.03.181l-.046.182c-.03.121-.077.242-.123.363a3.183 3.183 0 01-.366.666 3.002 3.002 0 01-1.91 1.18c-.122.016-.26.03-.382.046h-.198c-.061 0-.138 0-.199-.015a3.637 3.637 0 01-.81-.151 4.068 4.068 0 01-.748-.303 4.098 4.098 0 01-1.696-1.695 4.398 4.398 0 01-.29-.742c-.076-.257-.107-.514-.137-.772v-.302-.091c0-.136.015-.258.03-.394s.046-.272.061-.393c.03-.137.061-.258.092-.394a5.33 5.33 0 01.275-.741c.214-.47.504-.893.855-1.226.092-.091.184-.167.275-.243.092-.075.184-.136.29-.211a5.39 5.39 0 01.306-.182c.046-.03.107-.045.153-.076a.26.26 0 01.076-.03.26.26 0 01.077-.03c.107-.046.229-.091.336-.121.03-.015.06-.015.091-.03.03-.016.061-.016.092-.03.061-.016.122-.031.168-.046.03-.015.061-.015.092-.015.03 0 .06-.016.091-.016.03 0 .061-.015.092-.015l.046-.015h.046c.03 0 .06-.015.091-.015.03 0 .061-.015.107-.015.03 0 .077-.015.107-.015h.764c.23.015.443.03.657.075.428.076.84.212 1.207.394.366.182.702.393.977.636l.046.045.046.045c.03.03.061.061.107.091l.092.091.091.09c.123.122.23.258.336.394.199.258.367.515.49.772.014.015.014.03.03.046.015.015.015.03.015.045l.046.09.046.092.045.09c.046.122.092.228.123.333.06.167.107.318.137.455.015.045.061.09.122.075a.104.104 0 00.107-.106c.092-.227.092-.393.077-.575z" />
      <defs>
        <linearGradient id="grafana-gradient" x1="7.502" x2="7.502" y1="18.142" y2="5.356" gradientUnits="userSpaceOnUse">
          <stop stopColor="#FFF200" />
          <stop offset="1" stopColor="#F15A29" />
        </linearGradient>
      </defs>
    </svg>
  );
}

function CopyButton({ text }: { text: string }) {
  const [copied, setCopied] = useState(false);
  const handleClick = async (e: React.MouseEvent) => {
    e.stopPropagation();
    await copyToClipboard(text);
    setCopied(true);
    setTimeout(() => setCopied(false), 1500);
  };
  return (
    <span
      role="button"
      tabIndex={0}
      onClick={handleClick}
      onKeyDown={(e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); handleClick(e as unknown as React.MouseEvent); } }}
      className="inline-flex items-center gap-1 text-gray-400 hover:text-gray-600 ml-1 cursor-pointer"
      title={`Copy ${text}`}
    >
      <Copy className="w-3.5 h-3.5" />
      {copied && <span className="text-xs text-emerald-600">Copied</span>}
    </span>
  );
}

function toPascalCase(s: string): string {
  return s.replace(/(^|[-_\s])./g, (m) => m.replace(/[-_\s]/, '').toUpperCase());
}

function PhaseBadge({ phase }: { phase?: string }) {
  if (!phase) return null;
  const colorMap: Record<string, string> = {
    synced: 'bg-emerald-50 text-emerald-700',
    failed: 'bg-red-50 text-red-700',
  };
  const cls = colorMap[phase.toLowerCase()] ?? 'bg-amber-50 text-amber-700';
  return (
    <span className={`text-xs px-1.5 py-0.5 rounded font-medium ${cls}`}>
      {toPascalCase(phase)}
    </span>
  );
}

function HealthBadge({ healthy }: { healthy?: boolean }) {
  if (healthy === undefined) return null;
  const label = healthy ? 'Healthy' : 'Unhealthy';
  const tip = healthy ? 'All pods are running and ready' : 'One or more pods are not ready';
  const bgCls = healthy ? 'bg-blue-50 text-blue-600' : 'bg-red-50 text-red-600';
  return (
    <Tooltip.Provider delayDuration={300}>
      <Tooltip.Root>
        <Tooltip.Trigger asChild>
          <span className={`inline-flex items-center gap-1 text-xs shrink-0 cursor-default px-1.5 py-0.5 rounded font-medium ${bgCls}`}>
            {healthy ? <CheckCircle className="w-3.5 h-3.5 shrink-0" /> : <XCircle className="w-3.5 h-3.5 shrink-0" />}
            <span className="whitespace-nowrap">{label}</span>
          </span>
        </Tooltip.Trigger>
        <Tooltip.Portal>
          <Tooltip.Content sideOffset={4} className="rounded-md bg-gray-900 px-2.5 py-1.5 text-xs text-white shadow-md z-50">
            {tip}
            <Tooltip.Arrow className="fill-gray-900" />
          </Tooltip.Content>
        </Tooltip.Portal>
      </Tooltip.Root>
    </Tooltip.Provider>
  );
}

function PortsDisplay({ ports }: { ports: Array<{ nodePort?: number; port?: number }> | undefined }) {
  const [expanded, setExpanded] = useState(false);
  if (!ports || ports.length === 0) return null;
  const displayPorts = expanded ? ports : ports.slice(0, 2);
  const remaining = ports.length - 2;
  const allPortsStr = ports
    .map((p) => `${p.port}${p.nodePort && p.nodePort !== p.port ? `:${p.nodePort}` : ''}`)
    .join(', ');
  return (
    <span title={remaining > 0 && !expanded ? allPortsStr : undefined}>
      <span className="text-gray-500">Ports:</span>{' '}
      {displayPorts.map((p, i) => (
        <span key={i} className="text-gray-700">
          {p.port}{p.nodePort && p.nodePort !== p.port ? `:${p.nodePort}` : ''}
          {i < displayPorts.length - 1 ? ', ' : ''}
        </span>
      ))}
      {remaining > 0 && (
        <span
          onClick={() => setExpanded(!expanded)}
          className="text-gray-400 hover:text-gray-600 cursor-pointer"
        >
          {expanded ? ' less' : ` +${remaining}`}
        </span>
      )}
    </span>
  );
}

function YamlPopup({
  name,
  yaml: yamlText,
  open,
  onClose,
}: {
  name: string;
  yaml: string;
  open: boolean;
  onClose: () => void;
}) {
  return (
    <Dialog.Root open={open} onOpenChange={(v) => { if (!v) onClose(); }}>
      <Dialog.Portal>
        <Dialog.Overlay className="fixed inset-0 z-50 bg-black/40 data-[state=open]:animate-in data-[state=open]:fade-in-0 data-[state=closed]:animate-out data-[state=closed]:fade-out-0" />
        <Dialog.Content className="fixed left-1/2 top-1/2 z-50 -translate-x-1/2 -translate-y-1/2 bg-white rounded-xl shadow-xl max-w-4xl w-[calc(100%-2rem)] max-h-[85vh] flex flex-col focus:outline-none">
          <div className="flex items-center justify-between px-4 py-3 border-b border-gray-100">
            <Dialog.Title className="text-sm font-medium text-gray-900">{name} — YAML</Dialog.Title>
            <div className="flex items-center gap-2">
              <CopyButton text={yamlText} />
              <Dialog.Close asChild>
                <button type="button" className="text-gray-400 hover:text-gray-600">
                  <X className="w-4 h-4" />
                </button>
              </Dialog.Close>
            </div>
          </div>
          <div className="flex-1 overflow-auto">
            <SyntaxHighlighter
              language="yaml"
              style={oneDark}
              showLineNumbers
              wrapLines
              customStyle={{ margin: 0, borderRadius: 0, fontSize: '0.75rem' }}
              lineNumberStyle={{ minWidth: '3em', paddingRight: '1em', color: '#6b7280' }}
            >
              {yamlText}
            </SyntaxHighlighter>
          </div>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  );
}

function WorkloadSection({
  workload,
  searchQuery,
  hideHealthy,
}: {
  workload: JobDeploymentWorkload;
  searchQuery: string;
  hideHealthy: boolean;
}) {
  const [expanded, setExpanded] = useState(false);
  const [showYaml, setShowYaml] = useState(false);
  const cluster = workload.cluster;
  const isFedCluster = (cluster?.physical_cluster ?? '').toLowerCase().includes('fed');

  const filteredPods = (workload.pods || []).filter((pod) => {
    if (hideHealthy && pod.healthy) return false;
    if (!searchQuery) return true;
    const q = searchQuery.toLowerCase();
    const matchesPodName = pod.name ? pod.name.toLowerCase().includes(q) : false;
    const matchesNode = pod.node ? pod.node.toLowerCase().includes(q) : false;
    const matchesPort = pod.ports?.some(
      (p) => (p.port !== undefined && String(p.port).includes(q)) || (p.nodePort !== undefined && String(p.nodePort).includes(q)),
    );
    return matchesPodName || matchesNode || matchesPort;
  });

  if (filteredPods.length === 0 && (searchQuery || hideHealthy)) return null;

  return (
    <div className="border border-gray-100 rounded-lg">
      <button
        type="button"
        onClick={() => setExpanded(!expanded)}
        className="w-full flex items-center gap-2 p-3 text-left hover:bg-gray-50 rounded-lg"
      >
        {expanded ? (
          <ChevronDown className="w-4 h-4 text-gray-400 shrink-0" />
        ) : (
          <ChevronRight className="w-4 h-4 text-gray-400 shrink-0" />
        )}
        <div className="flex-1 min-w-0">
          <div className="flex items-center gap-2 flex-wrap">
            <span className="text-sm font-medium text-gray-900 truncate">
              {workload.name}
            </span>
            <CopyButton text={workload.name} />
            {workload.yaml && (
              <span
                role="button"
                tabIndex={0}
                onClick={(e) => { e.stopPropagation(); setShowYaml(true); }}
                onKeyDown={(e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); e.stopPropagation(); setShowYaml(true); } }}
                className="inline-flex items-center text-gray-400 hover:text-gray-600 shrink-0 cursor-pointer"
                title="View YAML"
              >
                <FileCode2 className="w-3.5 h-3.5" />
              </span>
            )}
            {workload.sale_mode && (
              <span className="text-xs px-1.5 py-0.5 rounded bg-blue-50 text-blue-600">
                {toPascalCase(workload.sale_mode)}
              </span>
            )}
            {workload.role && workload.role !== 'default' && (
              <span className="text-xs px-1.5 py-0.5 rounded bg-purple-50 text-purple-600">
                {workload.role}
              </span>
            )}
          </div>
          <div className="flex items-center gap-3 mt-1 text-xs text-gray-500">
            {cluster && (
              <>
                {cluster.zone && <span>{cluster.zone}</span>}
                {cluster.physical_cluster && <span>{cluster.physical_cluster}</span>}
                {cluster.idc && <span>{cluster.idc}</span>}
                {cluster.logical_cluster && <span>{cluster.logical_cluster}</span>}
                {workload.replicas !== undefined && (
                  <span className="text-xs text-gray-500">
                    {workload.replicas} {workload.replicas === 1 ? 'replica' : 'replicas'}
                  </span>
                )}
              </>
            )}
          </div>
        </div>
        <div className="flex flex-col items-end gap-1 shrink-0">
          <PhaseBadge phase={workload.phase} />
          <HealthBadge healthy={workload.healthy} />
        </div>
      </button>

      {showYaml && workload.yaml && (
        <YamlPopup name={workload.name} yaml={workload.yaml} open={showYaml} onClose={() => setShowYaml(false)} />
      )}

      {expanded && filteredPods.length > 0 && (
        <div className="border-t border-gray-100 px-3 pb-3">
          <div className="space-y-2 mt-2">
            {filteredPods.map((pod: JobDeploymentPod) => (
              <PodRow key={pod.name} pod={pod} showCluster={isFedCluster} />
            ))}
          </div>
        </div>
      )}

      {expanded && filteredPods.length === 0 && (
        <div className="border-t border-gray-100 px-3 pb-3 pt-2">
          <p className="text-xs text-gray-400">No matching pods.</p>
        </div>
      )}
    </div>
  );
}

function PodRow({ pod, showCluster }: { pod: JobDeploymentPod; showCluster?: boolean }) {
  return (
    <div className="flex items-start text-xs py-1.5 border-b border-gray-50 last:border-b-0">
      <div className="flex-1 min-w-0">
        <div className="flex items-center gap-1 flex-wrap">
          <span className="text-gray-500">Pod:</span>
          <code className="text-gray-800 bg-gray-50 px-1.5 py-0.5 rounded break-all">
            {pod.name}
          </code>
          <CopyButton text={pod.name} />
          {pod.monitoring && (
            <a
              href={pod.monitoring}
              target="_blank"
              rel="noopener noreferrer"
              title="Pod Monitoring"
              className="inline-flex items-center text-blue-500 hover:text-blue-600"
            >
              <Monitor className="w-4 h-4" />
            </a>
          )}
        </div>

        {pod.node && (
          <div className="flex items-center gap-1 mt-1">
            <span className="text-gray-500">Node:</span>
            <code className="text-gray-800 bg-gray-50 px-1.5 py-0.5 rounded">{pod.node}</code>
            <CopyButton text={pod.node} />
          </div>
        )}

        <div className="flex items-center gap-3 mt-1 flex-wrap">
          {pod.phase && (
            <span>
              <span className="text-gray-500">Phase:</span>{' '}
              <span className={pod.phase.toLowerCase() === 'running' ? 'text-emerald-600' : 'text-red-700'}>
                {pod.phase}
              </span>
            </span>
          )}
          {pod.role && pod.role !== 'default' && (
            <span className="inline-flex items-center gap-1">
              <span className="text-gray-500">Role:</span>{' '}
              <span className="px-1.5 py-0.5 rounded bg-purple-50 text-purple-600">{pod.role}</span>
            </span>
          )}
          <PortsDisplay ports={pod.ports} />
          {pod.created_at && (
            <>
              <span>
                <span className="text-gray-500">Created:</span>{' '}
                <span className="text-gray-700">{new Date(pod.created_at).toLocaleString()}</span>
              </span>
              <span title={new Date(pod.created_at).toLocaleString()}>
                <span className="text-gray-500">Age:</span>{' '}
                <span className="text-gray-700">{formatAge(pod.created_at)}</span>
              </span>
            </>
          )}
        </div>

        {showCluster && pod.cluster && (
          <div className="flex items-center gap-3 mt-1 flex-wrap text-gray-500">
            {pod.cluster.zone && <span>{pod.cluster.zone}</span>}
            {pod.cluster.physical_cluster && <span>{pod.cluster.physical_cluster}</span>}
            {pod.cluster.idc && <span>{pod.cluster.idc}</span>}
            {pod.cluster.logical_cluster && <span>{pod.cluster.logical_cluster}</span>}
          </div>
        )}
      </div>
      <HealthBadge healthy={pod.healthy} />
    </div>
  );
}

function DemoRenderer({ data, onRefresh }: DeploymentDetailProps) {
  const detail = data as JobDeploymentDetail;
  const jobId = detail.job_id || '';

  const [currentData, setCurrentData] = useState<JobDeploymentDetail>(detail);
  const [searchQuery, setSearchQuery] = useState('');
  const [hideHealthy, setHideHealthy] = useState(false);
  const [refreshing, setRefreshing] = useState(false);
  const [lastRefreshedAt, setLastRefreshedAt] = useState<Date>(new Date());
  const [refreshedAgo, setRefreshedAgo] = useState('');

  const intervalRef = useRef<ReturnType<typeof setInterval> | null>(null);
  const restartRef = useRef<() => void>(() => {});

  useEffect(() => {
    setCurrentData(detail);
  }, [detail]);

  const updateRefreshedAgo = useCallback(() => {
    const sec = Math.floor((Date.now() - lastRefreshedAt.getTime()) / 1000);
    if (sec < 60) setRefreshedAgo(`${sec}s ago`);
    else if (sec < 3600) setRefreshedAgo(`${Math.floor(sec / 60)}m ago`);
    else if (sec < 86400) setRefreshedAgo(`${Math.floor(sec / 3600)}h ago`);
    else setRefreshedAgo(`${Math.floor(sec / 86400)}d ago`);
  }, [lastRefreshedAt]);

  useEffect(() => {
    updateRefreshedAgo();
    const interval = setInterval(updateRefreshedAgo, 10000);
    return () => clearInterval(interval);
  }, [updateRefreshedAgo]);

  const handleRefresh = async () => {
    if (!jobId) return;
    setRefreshing(true);
    try {
      const job = onRefresh ? await onRefresh() : null;
      if (job) {
        const refreshed = parseDeploymentFromJob(job.extraBody);
        setCurrentData(refreshed || { ...currentData, workloads: [] });
      }
    } catch {
      // Silently keep current data on refresh failure
    } finally {
      setRefreshing(false);
      setLastRefreshedAt(new Date());
      setRefreshedAgo('0s ago');
      restartRef.current();
    }
  };

  const handleRefreshRef = useRef(handleRefresh);
  handleRefreshRef.current = handleRefresh;

  useEffect(() => {
    const start = () => {
      if (intervalRef.current) clearInterval(intervalRef.current);
      intervalRef.current = setInterval(() => {
        handleRefreshRef.current?.();
      }, 60000);
    };
    restartRef.current = start;
    start();
    return () => {
      if (intervalRef.current) clearInterval(intervalRef.current);
    };
  }, [jobId]);

  const displayWorkloads = currentData.workloads || [];

  return (
    <div className="space-y-3">
      {/* Toolbar */}
      <div className="flex items-center gap-2 flex-wrap">
        {detail.monitoring && (
          <a
            href={detail.monitoring}
            target="_blank"
            rel="noopener noreferrer"
            title="Deployment Monitoring"
            className="inline-flex items-center text-blue-500 hover:text-blue-600"
          >
            <Monitor className="w-5 h-5" />
          </a>
        )}
        {detail.grafana && (
          <a
            href={detail.grafana}
            target="_blank"
            rel="noopener noreferrer"
            title="Grafana Dashboard"
            className="inline-flex items-center"
          >
            <GrafanaIcon className="w-5 h-5" />
          </a>
        )}
        <button
          type="button"
          onClick={handleRefresh}
          disabled={refreshing || !jobId}
          className="inline-flex items-center gap-1.5 text-xs text-gray-500 hover:text-gray-700 px-2 py-1 rounded border border-gray-200 hover:bg-gray-50 disabled:opacity-50 disabled:cursor-not-allowed"
        >
          <RefreshCw className={`w-3.5 h-3.5 ${refreshing ? 'animate-spin' : ''}`} />
          Refreshed {refreshedAgo}
        </button>

        <button
          type="button"
          onClick={() => setHideHealthy(!hideHealthy)}
          className={`inline-flex items-center gap-1.5 text-xs px-2 py-1 rounded border ${
            hideHealthy
              ? 'text-teal-700 border-teal-300 bg-teal-50'
              : 'text-gray-500 border-gray-200 hover:bg-gray-50'
          }`}
        >
          <Filter className="w-3.5 h-3.5" />
          {hideHealthy ? 'Show healthy' : 'Hide healthy'}
        </button>

        <div className="relative flex-1 min-w-[180px]">
          <Search className="absolute left-2.5 top-1/2 -translate-y-1/2 w-3.5 h-3.5 text-gray-400 pointer-events-none" />
          <input
            type="text"
            value={searchQuery}
            onChange={(e) => setSearchQuery(e.target.value)}
            placeholder="Search node, pod, port..."
            className="w-full pl-8 pr-3 py-1 text-xs border border-gray-200 rounded focus:outline-none focus:ring-2 focus:ring-teal-500/30 focus:border-teal-500 bg-white"
          />
        </div>
      </div>

      {/* Workloads */}
      {displayWorkloads.length === 0 ? (
        <p className="text-sm text-gray-500">No workloads.</p>
      ) : (
        <div className="space-y-2">
          {displayWorkloads.map((wl) => (
            <WorkloadSection
              key={wl.id || wl.name}
              workload={wl}
              searchQuery={searchQuery}
              hideHealthy={hideHealthy}
            />
          ))}
        </div>
      )}
    </div>
  );
}

registerDeploymentDetailRenderer('demo', DemoRenderer);

export default DemoRenderer;
