import { useState } from "react";
import { useNavigate } from "react-router";
import { podAutoscalerApi, type PodAutoscalerCreateRequest } from "@/api/podautoscaler";

const SCALING_STRATEGIES = ["HPA", "KPA", "APA"];

export default function PodAutoscalerCreate() {
  const navigate = useNavigate();
  const [form, setForm] = useState<PodAutoscalerCreateRequest>({
    name: "",
    namespace: "default",
    scaleTargetRef: { apiVersion: "apps/v1", kind: "Deployment", name: "" },
    minReplicas: 1,
    maxReplicas: 10,
    scalingStrategy: "HPA",
  });
  const [error, setError] = useState("");
  const [submitting, setSubmitting] = useState(false);

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    setSubmitting(true);
    setError("");
    try {
      const resp = await podAutoscalerApi.create(form);
      navigate(`/podautoscalers/${resp.namespace}/${resp.name}`);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to create");
    } finally {
      setSubmitting(false);
    }
  };

  const setRef = (field: keyof typeof form.scaleTargetRef, value: string) =>
    setForm({ ...form, scaleTargetRef: { ...form.scaleTargetRef, [field]: value } });

  return (
    <div className="max-w-2xl">
      <h2 className="text-2xl font-bold text-gray-900 mb-6">Create PodAutoscaler</h2>
      {error && <div className="bg-red-50 text-red-700 px-4 py-3 rounded-md mb-4">{error}</div>}
      <form onSubmit={handleSubmit} className="space-y-4 bg-white p-6 rounded-lg border border-gray-200">
        <div>
          <label className="block text-sm font-medium text-gray-700 mb-1">Name *</label>
          <input type="text" required value={form.name} onChange={(e) => setForm({ ...form, name: e.target.value })}
            className="w-full px-3 py-2 border border-gray-300 rounded-md text-sm focus:ring-blue-500 focus:border-blue-500" />
        </div>
        <div>
          <label className="block text-sm font-medium text-gray-700 mb-1">Namespace *</label>
          <input type="text" required value={form.namespace} onChange={(e) => setForm({ ...form, namespace: e.target.value })}
            className="w-full px-3 py-2 border border-gray-300 rounded-md text-sm focus:ring-blue-500 focus:border-blue-500" />
        </div>

        <div className="border border-gray-200 rounded-md p-4 space-y-3">
          <p className="text-sm font-medium text-gray-700">Scale Target Ref</p>
          <div>
            <label className="block text-xs font-medium text-gray-600 mb-1">API Version *</label>
            <input type="text" required value={form.scaleTargetRef.apiVersion} onChange={(e) => setRef("apiVersion", e.target.value)}
              placeholder="e.g. apps/v1"
              className="w-full px-3 py-2 border border-gray-300 rounded-md text-sm focus:ring-blue-500 focus:border-blue-500" />
          </div>
          <div>
            <label className="block text-xs font-medium text-gray-600 mb-1">Kind *</label>
            <input type="text" required value={form.scaleTargetRef.kind} onChange={(e) => setRef("kind", e.target.value)}
              placeholder="e.g. Deployment"
              className="w-full px-3 py-2 border border-gray-300 rounded-md text-sm focus:ring-blue-500 focus:border-blue-500" />
          </div>
          <div>
            <label className="block text-xs font-medium text-gray-600 mb-1">Name *</label>
            <input type="text" required value={form.scaleTargetRef.name} onChange={(e) => setRef("name", e.target.value)}
              className="w-full px-3 py-2 border border-gray-300 rounded-md text-sm focus:ring-blue-500 focus:border-blue-500" />
          </div>
        </div>

        <div className="grid grid-cols-2 gap-4">
          <div>
            <label className="block text-sm font-medium text-gray-700 mb-1">Min Replicas</label>
            <input type="number" min={0} value={form.minReplicas ?? 1} onChange={(e) => setForm({ ...form, minReplicas: parseInt(e.target.value) || 1 })}
              className="w-full px-3 py-2 border border-gray-300 rounded-md text-sm focus:ring-blue-500 focus:border-blue-500" />
          </div>
          <div>
            <label className="block text-sm font-medium text-gray-700 mb-1">Max Replicas *</label>
            <input type="number" min={1} required value={form.maxReplicas} onChange={(e) => setForm({ ...form, maxReplicas: parseInt(e.target.value) || 1 })}
              className="w-full px-3 py-2 border border-gray-300 rounded-md text-sm focus:ring-blue-500 focus:border-blue-500" />
          </div>
        </div>

        <div>
          <label className="block text-sm font-medium text-gray-700 mb-1">Scaling Strategy *</label>
          <select required value={form.scalingStrategy} onChange={(e) => setForm({ ...form, scalingStrategy: e.target.value })}
            className="w-full px-3 py-2 border border-gray-300 rounded-md text-sm focus:ring-blue-500 focus:border-blue-500">
            {SCALING_STRATEGIES.map((s) => (
              <option key={s} value={s}>{s}</option>
            ))}
          </select>
        </div>

        <div className="flex gap-3 pt-4">
          <button type="submit" disabled={submitting}
            className="px-4 py-2 bg-blue-600 text-white text-sm font-medium rounded-md hover:bg-blue-700 disabled:opacity-50">
            {submitting ? "Creating..." : "Create"}
          </button>
          <button type="button" onClick={() => navigate("/podautoscalers")}
            className="px-4 py-2 bg-white text-gray-700 text-sm font-medium rounded-md border border-gray-300 hover:bg-gray-50">
            Cancel
          </button>
        </div>
      </form>
    </div>
  );
}
