import { useEffect, useState } from "react";
import { Link } from "react-router";
import { modelAdapterApi, type ModelAdapterListItem } from "@/api/modeladapter";
import ResourceTable, { type Column } from "@/app/components/ResourceTable";
import StatusBadge from "@/app/components/StatusBadge";

const columns: Column<ModelAdapterListItem>[] = [
  { header: "Name", accessor: (r) => r.name },
  { header: "Namespace", accessor: (r) => r.namespace },
  { header: "Base Model", accessor: (r) => r.baseModel || "-" },
  { header: "Status", accessor: (r) => <StatusBadge status={r.phase} /> },
  { header: "Replicas", accessor: (r) => `${r.readyReplicas}/${r.desiredReplicas}` },
  { header: "Age", accessor: (r) => new Date(r.createdAt).toLocaleDateString() },
];

export default function ModelAdapterList() {
  const [items, setItems] = useState<ModelAdapterListItem[]>([]);
  const [error, setError] = useState("");

  useEffect(() => {
    let active = true;
    const poll = async () => {
      try {
        const result = await modelAdapterApi.list();
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

  const handleDelete = async (row: ModelAdapterListItem) => {
    if (!confirm(`Delete ModelAdapter "${row.name}"?`)) return;
    await modelAdapterApi.delete(row.namespace, row.name);
    const result = await modelAdapterApi.list();
    setItems(result.items);
  };

  return (
    <div>
      <div className="flex items-center justify-between mb-6">
        <h2 className="text-2xl font-bold text-[var(--foreground)]">ModelAdapters</h2>
        <Link to="/modeladapters/create" className="px-4 py-2 bg-[var(--primary)] text-[var(--primary-foreground)] text-sm font-medium rounded-md hover:opacity-90">
          Create
        </Link>
      </div>
      {error && <div className="text-[var(--destructive)] mb-4">{error}</div>}
      <ResourceTable
        columns={columns}
        data={items}
        getKey={(r) => `${r.namespace}/${r.name}`}
        getLink={(r) => `/modeladapters/${r.namespace}/${r.name}`}
        onDelete={handleDelete}
      />
    </div>
  );
}
