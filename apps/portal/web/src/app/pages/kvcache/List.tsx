import { useEffect, useState } from "react";
import { Link } from "react-router";
import { kvCacheApi, type KVCacheListItem } from "@/api/kvcache";
import ResourceTable, { type Column } from "@/app/components/ResourceTable";

const columns: Column<KVCacheListItem>[] = [
  { header: "Name", accessor: (r) => r.name },
  { header: "Namespace", accessor: (r) => r.namespace },
  { header: "Mode", accessor: (r) => r.mode || "-" },
  { header: "Ready Replicas", accessor: (r) => String(r.readyReplicas) },
  { header: "Age", accessor: (r) => new Date(r.createdAt).toLocaleDateString() },
];

export default function KVCacheList() {
  const [items, setItems] = useState<KVCacheListItem[]>([]);
  const [error, setError] = useState("");

  const load = () => kvCacheApi.list().then((r) => setItems(r.items)).catch((e) => setError(e.message));

  useEffect(() => { load(); const t = setInterval(load, 10000); return () => clearInterval(t); }, []);

  const handleDelete = async (row: KVCacheListItem) => {
    if (!confirm(`Delete KVCache "${row.name}"?`)) return;
    await kvCacheApi.delete(row.namespace, row.name);
    load();
  };

  return (
    <div>
      <div className="flex items-center justify-between mb-6">
        <h2 className="text-2xl font-bold text-gray-900">KVCaches</h2>
        <Link to="/kvcaches/create" className="px-4 py-2 bg-blue-600 text-white text-sm font-medium rounded-md hover:bg-blue-700">
          Create
        </Link>
      </div>
      {error && <div className="text-red-600 mb-4">{error}</div>}
      <ResourceTable
        columns={columns}
        data={items}
        getKey={(r) => `${r.namespace}/${r.name}`}
        getLink={(r) => `/kvcaches/${r.namespace}/${r.name}`}
        onDelete={handleDelete}
      />
    </div>
  );
}
