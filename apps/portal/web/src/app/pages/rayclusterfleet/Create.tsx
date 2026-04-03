import { useState } from "react";
import { useNavigate } from "react-router";
import { rayClusterFleetApi, type RayClusterFleetCreateRequest } from "@/api/rayclusterfleet";

export default function RayClusterFleetCreate() {
  const navigate = useNavigate();
  const [form, setForm] = useState<RayClusterFleetCreateRequest>({
    name: "", namespace: "default", replicas: 1, rayVersion: "",
  });
  const [error, setError] = useState("");
  const [submitting, setSubmitting] = useState(false);

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    setSubmitting(true);
    setError("");
    try {
      const resp = await rayClusterFleetApi.create(form);
      navigate(`/rayclusterfleets/${resp.namespace}/${resp.name}`);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to create");
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <div className="max-w-2xl">
      <h2 className="text-2xl font-bold text-[var(--foreground)] mb-6">Create RayClusterFleet</h2>
      {error && <div className="bg-[var(--badge-red-bg)] text-[var(--badge-red-text)] px-4 py-3 rounded-md mb-4">{error}</div>}
      <form onSubmit={handleSubmit} className="space-y-4 bg-[var(--card)] p-6 rounded-lg border border-[var(--border)]">
        <div>
          <label className="block text-sm font-medium text-[var(--foreground)] mb-1">Name *</label>
          <input type="text" required value={form.name} onChange={(e) => setForm({ ...form, name: e.target.value })}
            className="w-full px-3 py-2 border border-[var(--input-border)] rounded-md text-sm bg-[var(--background)] text-[var(--foreground)] focus:ring-[var(--input-focus)] focus:border-[var(--input-focus)]" />
        </div>
        <div>
          <label className="block text-sm font-medium text-[var(--foreground)] mb-1">Namespace *</label>
          <input type="text" required value={form.namespace} onChange={(e) => setForm({ ...form, namespace: e.target.value })}
            className="w-full px-3 py-2 border border-[var(--input-border)] rounded-md text-sm bg-[var(--background)] text-[var(--foreground)] focus:ring-[var(--input-focus)] focus:border-[var(--input-focus)]" />
        </div>
        <div>
          <label className="block text-sm font-medium text-[var(--foreground)] mb-1">Replicas</label>
          <input type="number" min={1} value={form.replicas ?? 1} onChange={(e) => setForm({ ...form, replicas: parseInt(e.target.value) || 1 })}
            className="w-24 px-3 py-2 border border-[var(--input-border)] rounded-md text-sm bg-[var(--background)] text-[var(--foreground)] focus:ring-[var(--input-focus)] focus:border-[var(--input-focus)]" />
        </div>
        <div>
          <label className="block text-sm font-medium text-[var(--foreground)] mb-1">Ray Version</label>
          <input type="text" value={form.rayVersion || ""} onChange={(e) => setForm({ ...form, rayVersion: e.target.value })}
            placeholder="e.g. 2.9.0"
            className="w-full px-3 py-2 border border-[var(--input-border)] rounded-md text-sm bg-[var(--background)] text-[var(--foreground)] focus:ring-[var(--input-focus)] focus:border-[var(--input-focus)]" />
        </div>
        <div className="flex gap-3 pt-4">
          <button type="submit" disabled={submitting}
            className="px-4 py-2 bg-[var(--primary)] text-[var(--primary-foreground)] text-sm font-medium rounded-md hover:opacity-90 disabled:opacity-50">
            {submitting ? "Creating..." : "Create"}
          </button>
          <button type="button" onClick={() => navigate("/rayclusterfleets")}
            className="px-4 py-2 bg-[var(--card)] text-[var(--foreground)] text-sm font-medium rounded-md border border-[var(--input-border)] hover:bg-[var(--muted)]">
            Cancel
          </button>
        </div>
      </form>
    </div>
  );
}
