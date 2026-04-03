import { useEffect, useState } from "react";
import { useParams, useNavigate } from "react-router";
import { podAutoscalerApi, type PodAutoscalerDetail as DetailType } from "@/api/podautoscaler";

export default function PodAutoscalerDetail() {
  const { namespace, name } = useParams<{ namespace: string; name: string }>();
  const navigate = useNavigate();
  const [data, setData] = useState<DetailType | null>(null);
  const [error, setError] = useState("");

  useEffect(() => {
    if (namespace && name) {
      podAutoscalerApi.get(namespace, name).then(setData).catch((e) => setError(e.message));
    }
  }, [namespace, name]);

  const handleDelete = async () => {
    if (!namespace || !name) return;
    if (!confirm(`Delete PodAutoscaler "${name}"?`)) return;
    await podAutoscalerApi.delete(namespace, name);
    navigate("/podautoscalers");
  };

  if (error) return <div className="text-red-600">Error: {error}</div>;
  if (!data) return <div className="text-gray-500">Loading...</div>;

  return (
    <div>
      <div className="flex items-center justify-between mb-6">
        <div className="flex items-center gap-3">
          <h2 className="text-2xl font-bold text-gray-900">{data.name}</h2>
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
          </dl>
        </div>

        <div className="bg-white rounded-lg border border-gray-200 p-5">
          <h3 className="text-lg font-semibold text-gray-900 mb-4">Status</h3>
          <dl className="space-y-3 text-sm">
            <div className="flex justify-between"><dt className="text-gray-500">Desired Scale</dt><dd>{data.status.desiredScale}</dd></div>
            <div className="flex justify-between"><dt className="text-gray-500">Actual Scale</dt><dd>{data.status.actualScale}</dd></div>
          </dl>
        </div>
      </div>

      <p className="mt-4 text-xs text-gray-400">Created: {new Date(data.createdAt).toLocaleString()}</p>
    </div>
  );
}
