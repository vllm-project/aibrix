import { useEffect, useState } from "react";
import { Link } from "react-router";
import { podAutoscalerApi, type PodAutoscalerListItem } from "@/api/podautoscaler";
import ResourceTable, { type Column } from "@/app/components/ResourceTable";

const columns: Column<PodAutoscalerListItem>[] = [
  { header: "Name", accessor: (r) => r.name },
  { header: "Namespace", accessor: (r) => r.namespace },
  { header: "Strategy", accessor: (r) => r.scalingStrategy },
  { header: "Min/Max", accessor: (r) => `${r.minReplicas ?? "-"}/${r.maxReplicas}` },
  { header: "Desired/Actual Scale", accessor: (r) => `${r.desiredScale}/${r.actualScale}` },
  { header: "Age", accessor: (r) => new Date(r.createdAt).toLocaleDateString() },
];

export default function PodAutoscalerList() {
  const [items, setItems] = useState<PodAutoscalerListItem[]>([]);
  const [error, setError] = useState("");

  const load = () => podAutoscalerApi.list().then((r) => setItems(r.items)).catch((e) => setError(e.message));

  useEffect(() => { load(); const t = setInterval(load, 10000); return () => clearInterval(t); }, []);

  const handleDelete = async (row: PodAutoscalerListItem) => {
    if (!confirm(`Delete PodAutoscaler "${row.name}"?`)) return;
    await podAutoscalerApi.delete(row.namespace, row.name);
    load();
  };

  return (
    <div>
      <div className="flex items-center justify-between mb-6">
        <h2 className="text-2xl font-bold text-gray-900">PodAutoscalers</h2>
        <Link to="/podautoscalers/create" className="px-4 py-2 bg-blue-600 text-white text-sm font-medium rounded-md hover:bg-blue-700">
          Create
        </Link>
      </div>
      {error && <div className="text-red-600 mb-4">{error}</div>}
      <ResourceTable
        columns={columns}
        data={items}
        getKey={(r) => `${r.namespace}/${r.name}`}
        getLink={(r) => `/podautoscalers/${r.namespace}/${r.name}`}
        onDelete={handleDelete}
      />
    </div>
  );
}
