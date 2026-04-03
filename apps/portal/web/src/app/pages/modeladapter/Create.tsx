import { useState } from "react";
import { useNavigate } from "react-router";
import { modelAdapterApi, type ModelAdapterCreateRequest } from "@/api/modeladapter";

export default function ModelAdapterCreate() {
  const navigate = useNavigate();
  const [form, setForm] = useState<ModelAdapterCreateRequest>({
    name: "", namespace: "default", artifactURL: "", podSelector: {}, replicas: 1,
  });
  const [selectorKey, setSelectorKey] = useState("app");
  const [selectorVal, setSelectorVal] = useState("");
  const [error, setError] = useState("");
  const [submitting, setSubmitting] = useState(false);

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    setSubmitting(true);
    setError("");
    try {
      const resp = await modelAdapterApi.create(form);
      navigate(`/modeladapters/${resp.namespace}/${resp.name}`);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to create");
    } finally {
      setSubmitting(false);
    }
  };

  const addSelector = () => {
    if (selectorKey && selectorVal) {
      setForm({ ...form, podSelector: { ...form.podSelector, [selectorKey]: selectorVal } });
      setSelectorKey("");
      setSelectorVal("");
    }
  };

  return (
    <div className="max-w-2xl">
      <h2 className="text-2xl font-bold text-gray-900 mb-6">Create ModelAdapter</h2>
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
        <div>
          <label className="block text-sm font-medium text-gray-700 mb-1">Base Model</label>
          <input type="text" value={form.baseModel || ""} onChange={(e) => setForm({ ...form, baseModel: e.target.value })}
            placeholder="e.g. meta-llama/Llama-2-7b-hf"
            className="w-full px-3 py-2 border border-gray-300 rounded-md text-sm focus:ring-blue-500 focus:border-blue-500" />
        </div>
        <div>
          <label className="block text-sm font-medium text-gray-700 mb-1">Artifact URL *</label>
          <input type="text" required value={form.artifactURL} onChange={(e) => setForm({ ...form, artifactURL: e.target.value })}
            placeholder="s3://bucket/path"
            className="w-full px-3 py-2 border border-gray-300 rounded-md text-sm focus:ring-blue-500 focus:border-blue-500" />
        </div>
        <div>
          <label className="block text-sm font-medium text-gray-700 mb-1">Pod Selector *</label>
          <div className="flex gap-2 mb-2">
            <input type="text" placeholder="key" value={selectorKey} onChange={(e) => setSelectorKey(e.target.value)}
              className="flex-1 px-3 py-2 border border-gray-300 rounded-md text-sm" />
            <input type="text" placeholder="value" value={selectorVal} onChange={(e) => setSelectorVal(e.target.value)}
              className="flex-1 px-3 py-2 border border-gray-300 rounded-md text-sm" />
            <button type="button" onClick={addSelector} className="px-3 py-2 bg-gray-100 border border-gray-300 rounded-md text-sm hover:bg-gray-200">Add</button>
          </div>
          <div className="flex flex-wrap gap-1">
            {Object.entries(form.podSelector).map(([k, v]) => (
              <span key={k} className="inline-flex items-center gap-1 px-2 py-1 bg-gray-100 rounded-md text-xs">
                {k}={v}
                <button type="button" onClick={() => { const s = { ...form.podSelector }; delete s[k]; setForm({ ...form, podSelector: s }); }}
                  className="text-gray-400 hover:text-red-500">&times;</button>
              </span>
            ))}
          </div>
        </div>
        <div>
          <label className="block text-sm font-medium text-gray-700 mb-1">Replicas</label>
          <input type="number" min={1} value={form.replicas || 1} onChange={(e) => setForm({ ...form, replicas: parseInt(e.target.value) || 1 })}
            className="w-24 px-3 py-2 border border-gray-300 rounded-md text-sm focus:ring-blue-500 focus:border-blue-500" />
        </div>
        <div className="flex gap-3 pt-4">
          <button type="submit" disabled={submitting}
            className="px-4 py-2 bg-blue-600 text-white text-sm font-medium rounded-md hover:bg-blue-700 disabled:opacity-50">
            {submitting ? "Creating..." : "Create"}
          </button>
          <button type="button" onClick={() => navigate("/modeladapters")}
            className="px-4 py-2 bg-white text-gray-700 text-sm font-medium rounded-md border border-gray-300 hover:bg-gray-50">
            Cancel
          </button>
        </div>
      </form>
    </div>
  );
}
