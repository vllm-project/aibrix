import { useEffect, useState } from "react";
import { Link } from "react-router";
import { stormServiceApi, type StormServiceListItem } from "@/api/stormservice";
import ResourceTable, { type Column } from "@/app/components/ResourceTable";

const columns: Column<StormServiceListItem>[] = [
  { header: "Name", accessor: (r) => r.name },
  { header: "Namespace", accessor: (r) => r.namespace },
  { header: "Roles Count", accessor: (r) => String(r.rolesCount) },
  { header: "Replicas", accessor: (r) => `${r.readyReplicas}/${r.replicas}` },
  { header: "Stateful", accessor: (r) => r.stateful ? "Yes" : "No" },
  { header: "Age", accessor: (r) => new Date(r.createdAt).toLocaleDateString() },
];

export default function StormServiceList() {
  const [items, setItems] = useState<StormServiceListItem[]>([]);
  const [error, setError] = useState("");

  const load = () => stormServiceApi.list().then((r) => setItems(r.items)).catch((e) => setError(e.message));

  useEffect(() => { load(); const t = setInterval(load, 10000); return () => clearInterval(t); }, []);

  const handleDelete = async (row: StormServiceListItem) => {
    if (!confirm(`Delete StormService "${row.name}"?`)) return;
    await stormServiceApi.delete(row.namespace, row.name);
    load();
  };

  return (
    <div>
      <div className="flex items-center justify-between mb-6">
        <h2 className="text-2xl font-bold text-[var(--foreground)]">StormServices</h2>
        <Link to="/stormservices/create" className="px-4 py-2 bg-[var(--primary)] text-[var(--primary-foreground)] text-sm font-medium rounded-md hover:opacity-90">
          Create
        </Link>
      </div>
      {error && <div className="text-[var(--destructive)] mb-4">{error}</div>}
      <ResourceTable
        columns={columns}
        data={items}
        getKey={(r) => `${r.namespace}/${r.name}`}
        getLink={(r) => `/stormservices/${r.namespace}/${r.name}`}
        onDelete={handleDelete}
      />
    </div>
  );
}
