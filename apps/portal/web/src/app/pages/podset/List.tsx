import { useEffect, useState } from "react";
import { Link } from "react-router";
import { podSetApi, type PodSetListItem } from "@/api/podset";
import ResourceTable, { type Column } from "@/app/components/ResourceTable";
import StatusBadge from "@/app/components/StatusBadge";

const columns: Column<PodSetListItem>[] = [
  { header: "Name", accessor: (r) => r.name },
  { header: "Namespace", accessor: (r) => r.namespace },
  { header: "Pod Group Size", accessor: (r) => String(r.podGroupSize) },
  { header: "Ready/Total Pods", accessor: (r) => `${r.readyPods}/${r.totalPods}` },
  { header: "Phase", accessor: (r) => <StatusBadge status={r.phase} /> },
  { header: "Age", accessor: (r) => new Date(r.createdAt).toLocaleDateString() },
];

export default function PodSetList() {
  const [items, setItems] = useState<PodSetListItem[]>([]);
  const [error, setError] = useState("");

  const load = () => podSetApi.list().then((r) => setItems(r.items)).catch((e) => setError(e.message));

  useEffect(() => { load(); const t = setInterval(load, 10000); return () => clearInterval(t); }, []);

  const handleDelete = async (row: PodSetListItem) => {
    if (!confirm(`Delete PodSet "${row.name}"?`)) return;
    await podSetApi.delete(row.namespace, row.name);
    load();
  };

  return (
    <div>
      <div className="flex items-center justify-between mb-6">
        <h2 className="text-2xl font-bold text-gray-900">PodSets</h2>
        <Link to="/podsets/create" className="px-4 py-2 bg-blue-600 text-white text-sm font-medium rounded-md hover:bg-blue-700">
          Create
        </Link>
      </div>
      {error && <div className="text-red-600 mb-4">{error}</div>}
      <ResourceTable
        columns={columns}
        data={items}
        getKey={(r) => `${r.namespace}/${r.name}`}
        getLink={(r) => `/podsets/${r.namespace}/${r.name}`}
        onDelete={handleDelete}
      />
    </div>
  );
}
