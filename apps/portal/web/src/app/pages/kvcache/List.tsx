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

  useEffect(() => {
    let active = true;
    const poll = async () => {
      try {
        const result = await kvCacheApi.list();
        if (active) setItems(result.items);
      } catch (e) {
        if (active) setError(e instanceof Error ? e.message : "Failed to load");
      } finally {
        if (active) setTimeout(poll, 10000);
      }
    };
    poll();
    return () => { active = false; };
  }, []);

  const handleDelete = async (row: KVCacheListItem) => {
    if (!confirm(`Delete KVCache "${row.name}"?`)) return;
    await kvCacheApi.delete(row.namespace, row.name);
    const result = await kvCacheApi.list();
    setItems(result.items);
  };

  return (
    <div>
      <div className="flex items-center justify-between mb-6">
        <h2 className="text-2xl font-bold text-[var(--foreground)]">KVCaches</h2>
        <Link to="/kvcaches/create" className="px-4 py-2 bg-[var(--primary)] text-[var(--primary-foreground)] text-sm font-medium rounded-md hover:opacity-90">
          Create
        </Link>
      </div>
      {error && <div className="text-[var(--destructive)] mb-4">{error}</div>}
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
