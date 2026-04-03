import { useEffect, useState } from "react";
import { useParams, useNavigate } from "react-router";
import { modelAdapterApi, type ModelAdapterDetail as DetailType } from "@/api/modeladapter";
import StatusBadge from "@/app/components/StatusBadge";

export default function ModelAdapterDetail() {
  const { namespace, name } = useParams<{ namespace: string; name: string }>();
  const navigate = useNavigate();
  const [data, setData] = useState<DetailType | null>(null);
  const [error, setError] = useState("");

  useEffect(() => {
    if (namespace && name) {
      modelAdapterApi.get(namespace, name).then(setData).catch((e) => setError(e.message));
    }
  }, [namespace, name]);

  const handleDelete = async () => {
    if (!namespace || !name) return;
    if (!confirm(`Delete ModelAdapter "${name}"?`)) return;
    await modelAdapterApi.delete(namespace, name);
    navigate("/modeladapters");
  };

  if (error) return <div className="text-[var(--destructive)]">Error: {error}</div>;
  if (!data) return <div className="text-[var(--muted-foreground)]">Loading...</div>;

  return (
    <div>
      <div className="flex items-center justify-between mb-6">
        <div className="flex items-center gap-3">
          <h2 className="text-2xl font-bold text-[var(--foreground)]">{data.name}</h2>
          <StatusBadge status={data.status.phase} />
        </div>
        <button onClick={handleDelete} className="px-4 py-2 bg-[var(--destructive)] text-[var(--destructive-foreground)] text-sm font-medium rounded-md hover:opacity-90">
          Delete
        </button>
      </div>

      <div className="grid grid-cols-1 lg:grid-cols-2 gap-6">
        <div className="bg-[var(--card)] rounded-lg border border-[var(--border)] p-5">
          <h3 className="text-lg font-semibold text-[var(--foreground)] mb-4">Spec</h3>
          <dl className="space-y-3 text-sm">
            <div className="flex justify-between"><dt className="text-[var(--muted-foreground)]">Namespace</dt><dd className="text-[var(--foreground)]">{data.namespace}</dd></div>
            <div className="flex justify-between"><dt className="text-[var(--muted-foreground)]">Base Model</dt><dd className="text-[var(--foreground)]">{data.spec.baseModel || "-"}</dd></div>
            <div className="flex justify-between"><dt className="text-[var(--muted-foreground)]">Artifact URL</dt><dd className="text-[var(--foreground)] break-all">{data.spec.artifactURL}</dd></div>
            <div className="flex justify-between"><dt className="text-[var(--muted-foreground)]">Replicas</dt><dd className="text-[var(--foreground)]">{data.spec.replicas ?? "-"}</dd></div>
            {data.spec.podSelector && Object.keys(data.spec.podSelector).length > 0 && (
              <div>
                <dt className="text-[var(--muted-foreground)] mb-1">Pod Selector</dt>
                <dd className="flex flex-wrap gap-1">
                  {Object.entries(data.spec.podSelector).map(([k, v]) => (
                    <span key={k} className="px-2 py-0.5 bg-[var(--muted)] rounded text-xs text-[var(--foreground)]">{k}={v}</span>
                  ))}
                </dd>
              </div>
            )}
          </dl>
        </div>

        <div className="bg-[var(--card)] rounded-lg border border-[var(--border)] p-5">
          <h3 className="text-lg font-semibold text-[var(--foreground)] mb-4">Status</h3>
          <dl className="space-y-3 text-sm">
            <div className="flex justify-between"><dt className="text-[var(--muted-foreground)]">Phase</dt><dd><StatusBadge status={data.status.phase} /></dd></div>
            <div className="flex justify-between"><dt className="text-[var(--muted-foreground)]">Ready Replicas</dt><dd className="text-[var(--foreground)]">{data.status.readyReplicas}/{data.status.desiredReplicas}</dd></div>
            {data.status.instances && data.status.instances.length > 0 && (
              <div>
                <dt className="text-[var(--muted-foreground)] mb-1">Instances</dt>
                <dd className="space-y-1">
                  {data.status.instances.map((inst) => (
                    <div key={inst} className="px-2 py-1 bg-[var(--muted)] rounded text-xs font-mono text-[var(--foreground)]">{inst}</div>
                  ))}
                </dd>
              </div>
            )}
          </dl>
        </div>
      </div>

      <p className="mt-4 text-xs text-[var(--muted-foreground)]">Created: {new Date(data.createdAt).toLocaleString()}</p>
    </div>
  );
}
