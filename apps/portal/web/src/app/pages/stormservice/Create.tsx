import { useState } from "react";
import { useNavigate } from "react-router";
import { stormServiceApi, type StormServiceCreateRequest, type StormRoleSpec } from "@/api/stormservice";

const emptyRole = (): StormRoleSpec => ({ name: "", replicas: 1, image: "", cpu: "", memory: "", gpu: 0 });

export default function StormServiceCreate() {
  const navigate = useNavigate();
  const [form, setForm] = useState<StormServiceCreateRequest>({
    name: "", namespace: "default", replicas: 1, stateful: false, roles: [emptyRole()],
  });
  const [error, setError] = useState("");
  const [submitting, setSubmitting] = useState(false);

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    setSubmitting(true);
    setError("");
    try {
      const resp = await stormServiceApi.create(form);
      navigate(`/stormservices/${resp.namespace}/${resp.name}`);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to create");
    } finally {
      setSubmitting(false);
    }
  };

  const updateRole = (index: number, field: keyof StormRoleSpec, value: string | number) => {
    const roles = form.roles.map((r, i) => i === index ? { ...r, [field]: value } : r);
    setForm({ ...form, roles });
  };

  const addRole = () => setForm({ ...form, roles: [...form.roles, emptyRole()] });

  const removeRole = (index: number) => setForm({ ...form, roles: form.roles.filter((_, i) => i !== index) });

  return (
    <div className="max-w-2xl">
      <h2 className="text-2xl font-bold text-gray-900 mb-6">Create StormService</h2>
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
          <label className="block text-sm font-medium text-gray-700 mb-1">Replicas</label>
          <input type="number" min={1} value={form.replicas ?? 1} onChange={(e) => setForm({ ...form, replicas: parseInt(e.target.value) || 1 })}
            className="w-24 px-3 py-2 border border-gray-300 rounded-md text-sm focus:ring-blue-500 focus:border-blue-500" />
        </div>
        <div className="flex items-center gap-3">
          <label className="block text-sm font-medium text-gray-700">Stateful</label>
          <input type="checkbox" checked={form.stateful ?? false} onChange={(e) => setForm({ ...form, stateful: e.target.checked })}
            className="h-4 w-4 text-blue-600 border-gray-300 rounded" />
        </div>

        <div>
          <div className="flex items-center justify-between mb-2">
            <label className="block text-sm font-medium text-gray-700">Roles</label>
            <button type="button" onClick={addRole} className="px-3 py-1 bg-gray-100 border border-gray-300 rounded-md text-sm hover:bg-gray-200">
              Add Role
            </button>
          </div>
          <div className="space-y-4">
            {form.roles.map((role, index) => (
              <div key={index} className="p-4 border border-gray-200 rounded-md space-y-3">
                <div className="flex items-center justify-between">
                  <span className="text-sm font-medium text-gray-700">Role {index + 1}</span>
                  {form.roles.length > 1 && (
                    <button type="button" onClick={() => removeRole(index)} className="text-red-500 hover:text-red-700 text-sm">Remove</button>
                  )}
                </div>
                <div className="grid grid-cols-2 gap-3">
                  <div>
                    <label className="block text-xs font-medium text-gray-600 mb-1">Name *</label>
                    <input type="text" required value={role.name} onChange={(e) => updateRole(index, "name", e.target.value)}
                      className="w-full px-3 py-2 border border-gray-300 rounded-md text-sm focus:ring-blue-500 focus:border-blue-500" />
                  </div>
                  <div>
                    <label className="block text-xs font-medium text-gray-600 mb-1">Replicas</label>
                    <input type="number" min={1} value={role.replicas} onChange={(e) => updateRole(index, "replicas", parseInt(e.target.value) || 1)}
                      className="w-full px-3 py-2 border border-gray-300 rounded-md text-sm focus:ring-blue-500 focus:border-blue-500" />
                  </div>
                  <div className="col-span-2">
                    <label className="block text-xs font-medium text-gray-600 mb-1">Image *</label>
                    <input type="text" required value={role.image} onChange={(e) => updateRole(index, "image", e.target.value)}
                      placeholder="e.g. nginx:latest"
                      className="w-full px-3 py-2 border border-gray-300 rounded-md text-sm focus:ring-blue-500 focus:border-blue-500" />
                  </div>
                  <div>
                    <label className="block text-xs font-medium text-gray-600 mb-1">CPU</label>
                    <input type="text" value={role.cpu || ""} onChange={(e) => updateRole(index, "cpu", e.target.value)}
                      placeholder="e.g. 500m"
                      className="w-full px-3 py-2 border border-gray-300 rounded-md text-sm focus:ring-blue-500 focus:border-blue-500" />
                  </div>
                  <div>
                    <label className="block text-xs font-medium text-gray-600 mb-1">Memory</label>
                    <input type="text" value={role.memory || ""} onChange={(e) => updateRole(index, "memory", e.target.value)}
                      placeholder="e.g. 512Mi"
                      className="w-full px-3 py-2 border border-gray-300 rounded-md text-sm focus:ring-blue-500 focus:border-blue-500" />
                  </div>
                  <div>
                    <label className="block text-xs font-medium text-gray-600 mb-1">GPU</label>
                    <input type="number" min={0} value={role.gpu ?? 0} onChange={(e) => updateRole(index, "gpu", parseInt(e.target.value) || 0)}
                      className="w-full px-3 py-2 border border-gray-300 rounded-md text-sm focus:ring-blue-500 focus:border-blue-500" />
                  </div>
                </div>
              </div>
            ))}
          </div>
        </div>

        <div className="flex gap-3 pt-4">
          <button type="submit" disabled={submitting}
            className="px-4 py-2 bg-blue-600 text-white text-sm font-medium rounded-md hover:bg-blue-700 disabled:opacity-50">
            {submitting ? "Creating..." : "Create"}
          </button>
          <button type="button" onClick={() => navigate("/stormservices")}
            className="px-4 py-2 bg-white text-gray-700 text-sm font-medium rounded-md border border-gray-300 hover:bg-gray-50">
            Cancel
          </button>
        </div>
      </form>
    </div>
  );
}
