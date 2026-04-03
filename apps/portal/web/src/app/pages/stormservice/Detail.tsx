import { useEffect, useState } from "react";
import { useParams, useNavigate } from "react-router";
import { stormServiceApi, type StormServiceDetail as DetailType } from "@/api/stormservice";

export default function StormServiceDetail() {
  const { namespace, name } = useParams<{ namespace: string; name: string }>();
  const navigate = useNavigate();
  const [data, setData] = useState<DetailType | null>(null);
  const [error, setError] = useState("");

  useEffect(() => {
    if (namespace && name) {
      stormServiceApi.get(namespace, name).then(setData).catch((e) => setError(e.message));
    }
  }, [namespace, name]);

  const handleDelete = async () => {
    if (!namespace || !name) return;
    if (!confirm(`Delete StormService "${name}"?`)) return;
    await stormServiceApi.delete(namespace, name);
    navigate("/stormservices");
  };

  if (error) return <div className="text-[var(--destructive)]">Error: {error}</div>;
  if (!data) return <div className="text-[var(--muted-foreground)]">Loading...</div>;

  return (
    <div>
      <div className="flex items-center justify-between mb-6">
        <div className="flex items-center gap-3">
          <h2 className="text-2xl font-bold text-[var(--foreground)]">{data.name}</h2>
        </div>
        <button onClick={handleDelete} className="px-4 py-2 bg-[var(--destructive)] text-[var(--destructive-foreground)] text-sm font-medium rounded-md hover:opacity-90">
          Delete
        </button>
      </div>

      <div className="grid grid-cols-1 lg:grid-cols-2 gap-6">
        <div className="bg-[var(--card)] rounded-lg border border-[var(--border)] p-5">
          <h3 className="text-lg font-semibold text-[var(--foreground)] mb-4">Spec</h3>
          <dl className="space-y-3 text-sm">
            <div className="flex justify-between"><dt className="text-[var(--muted-foreground)]">Namespace</dt><dd>{data.namespace}</dd></div>
            <div className="flex justify-between"><dt className="text-[var(--muted-foreground)]">Stateful</dt><dd>{data.stateful ? "Yes" : "No"}</dd></div>
          </dl>
        </div>

        <div className="bg-[var(--card)] rounded-lg border border-[var(--border)] p-5">
          <h3 className="text-lg font-semibold text-[var(--foreground)] mb-4">Status</h3>
          <dl className="space-y-3 text-sm">
            <div className="flex justify-between"><dt className="text-[var(--muted-foreground)]">Replicas</dt><dd>{data.status.replicas}</dd></div>
            <div className="flex justify-between"><dt className="text-[var(--muted-foreground)]">Ready Replicas</dt><dd>{data.status.readyReplicas}</dd></div>
          </dl>
        </div>
      </div>

      <p className="mt-4 text-xs text-[var(--muted-foreground)]">Created: {new Date(data.createdAt).toLocaleString()}</p>
    </div>
  );
}
