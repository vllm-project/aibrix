import { useEffect, useState } from 'react';
import { APIError, getFileSummary, type FileSummary } from '../utils/api';
import { MAX_REQUESTS_PER_BATCH } from '../utils/batchValidation';

interface DatasetSummaryProps {
  expectedModel: string;
  supportedEndpoints: string[];
}

export function DatasetSummaryDetails({
  summary,
  expectedModel,
  supportedEndpoints,
}: DatasetSummaryProps & { summary: FileSummary }) {
  const warnings: string[] = [];
  if (summary.requestCount === 0 && !summary.truncated) warnings.push('No requests found in this dataset.');
  if (summary.invalidLines > 0) {
    const noun = summary.invalidLines === 1 ? 'row' : 'rows';
    warnings.push(`${summary.invalidLines} invalid ${noun} found (invalid JSON or missing POST method, model, or endpoint).`);
  }
  if (expectedModel && summary.models.some((model) => model !== expectedModel)) {
    warnings.push('The dataset model does not match the selected serving model.');
  }
  if (supportedEndpoints.length > 0 && summary.endpoints.some((endpoint) => !supportedEndpoints.includes(endpoint))) {
    warnings.push('Some dataset endpoints are not supported by the selected template.');
  }
  // Batch validation rejects these too, and they are the mismatches that
  // actually fail at submit rather than merely looking suspicious.
  if (summary.models.length > 1) {
    warnings.push('The dataset uses more than one model. A batch file must use one.');
  }
  if (summary.endpoints.length > 1) {
    warnings.push('The dataset uses more than one endpoint. A batch file must use one.');
  }
  if (summary.requestCount > MAX_REQUESTS_PER_BATCH) {
    warnings.push(`The dataset has ${summary.requestCount.toLocaleString()} requests; the per-batch limit is ${MAX_REQUESTS_PER_BATCH.toLocaleString()}.`);
  }

  return (
    <div className="mt-3 text-sm space-y-2" aria-label="Dataset details">
      <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1 text-xs">
        <dt className="text-gray-500">Requests</dt>
        <dd>{summary.truncated ? 'At least ' : ''}{summary.requestCount.toLocaleString()}</dd>
        <dt className="text-gray-500">Models</dt>
        <dd className="break-all">{summary.models.join(', ') || 'Not detected'}</dd>
        <dt className="text-gray-500">Endpoints</dt>
        <dd className="break-all">{summary.endpoints.join(', ') || 'Not detected'}</dd>
      </dl>
      {summary.truncated && (
        <p className="text-xs text-gray-500">Preview is incomplete. Counts and detected models/endpoints cover only the scanned portion.</p>
      )}
      {warnings.length > 0 && (
        <ul role="alert" className="text-xs text-amber-800 space-y-1">
          {warnings.map((warning) => <li key={warning}>{warning}</li>)}
        </ul>
      )}
      <p className="text-xs text-gray-500">This preview does not replace batch validation when the job is submitted.</p>
    </div>
  );
}

export function ExistingDatasetSummary({
  fileId,
  ...props
}: DatasetSummaryProps & { fileId: string }) {
  const [summary, setSummary] = useState<FileSummary | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<{ message: string; retryable: boolean } | null>(null);
  const [attempt, setAttempt] = useState(0);

  useEffect(() => {
    const controller = new AbortController();
    setSummary(null);
    setLoading(true);
    setError(null);
    getFileSummary(fileId, controller.signal)
      .then((result) => { if (!controller.signal.aborted) setSummary(result); })
      .catch((err: unknown) => {
        if (!controller.signal.aborted) {
          setError(err instanceof APIError && err.status === 413
            // Retrying a file that is over the limit can never succeed.
            ? { message: 'This dataset exceeds the 8 MiB preview limit.', retryable: false }
            : { message: 'Dataset details are unavailable.', retryable: true });
        }
      })
      .finally(() => { if (!controller.signal.aborted) setLoading(false); });
    return () => controller.abort();
  }, [fileId, attempt]);

  if (loading) return <p role="status" className="mt-3 text-xs text-gray-500">Loading dataset details...</p>;
  if (error) return (
    <div className="mt-3 text-xs text-gray-500" role="status">
      {error.message} You can still use this dataset.
      {error.retryable && (
        <button type="button" className="ml-2 text-teal-700 underline" onClick={() => setAttempt((value) => value + 1)}>Retry</button>
      )}
    </div>
  );
  return summary ? <DatasetSummaryDetails summary={summary} {...props} /> : null;
}
