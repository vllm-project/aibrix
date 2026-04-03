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

  if (error) return <div className="text-red-600">Error: {error}</div>;
  if (!data) return <div className="text-gray-500">Loading...</div>;

  return (
    <div>
      <div className="flex items-center justify-between mb-6">
        <div className="flex items-center gap-3">
          <h2 className="text-2xl font-bold text-gray-900">{data.name}</h2>
          <StatusBadge status={data.status.phase} />
        </div>
        <button onClick={handleDelete} className="px-4 py-2 bg-red-600 text-white text-sm font-medium rounded-md hover:bg-red-700">
          Delete
        </button>
      </div>

      <div className="grid grid-cols-1 lg:grid-cols-2 gap-6">
        <div className="bg-white rounded-lg border border-gray-200 p-5">
          <h3 className="text-lg font-semibold text-gray-900 mb-4">Spec</h3>
          <dl className="space-y-3 text-sm">
            <div className="flex justify-between"><dt className="text-gray-500">Namespace</dt><dd>{data.namespace}</dd></div>
            <div className="flex justify-between"><dt className="text-gray-500">Base Model</dt><dd>{data.spec.baseModel || "-"}</dd></div>
            <div className="flex justify-between"><dt className="text-gray-500">Artifact URL</dt><dd className="break-all">{data.spec.artifactURL}</dd></div>
            <div className="flex justify-between"><dt className="text-gray-500">Replicas</dt><dd>{data.spec.replicas ?? "-"}</dd></div>
            {data.spec.podSelector && Object.keys(data.spec.podSelector).length > 0 && (
              <div>
                <dt className="text-gray-500 mb-1">Pod Selector</dt>
                <dd className="flex flex-wrap gap-1">
                  {Object.entries(data.spec.podSelector).map(([k, v]) => (
                    <span key={k} className="px-2 py-0.5 bg-gray-100 rounded text-xs">{k}={v}</span>
                  ))}
                </dd>
              </div>
            )}
          </dl>
        </div>

        <div className="bg-white rounded-lg border border-gray-200 p-5">
          <h3 className="text-lg font-semibold text-gray-900 mb-4">Status</h3>
          <dl className="space-y-3 text-sm">
            <div className="flex justify-between"><dt className="text-gray-500">Phase</dt><dd><StatusBadge status={data.status.phase} /></dd></div>
            <div className="flex justify-between"><dt className="text-gray-500">Ready Replicas</dt><dd>{data.status.readyReplicas}/{data.status.desiredReplicas}</dd></div>
            {data.status.instances && data.status.instances.length > 0 && (
              <div>
                <dt className="text-gray-500 mb-1">Instances</dt>
                <dd className="space-y-1">
                  {data.status.instances.map((inst) => (
                    <div key={inst} className="px-2 py-1 bg-gray-50 rounded text-xs font-mono">{inst}</div>
                  ))}
                </dd>
              </div>
            )}
          </dl>
        </div>
      </div>

      <p className="mt-4 text-xs text-gray-400">Created: {new Date(data.createdAt).toLocaleString()}</p>
    </div>
  );
}
