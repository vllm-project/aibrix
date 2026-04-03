import { useEffect, useState } from "react";
import { Link } from "react-router";
import { rayClusterFleetApi, type RayClusterFleetListItem } from "@/api/rayclusterfleet";
import ResourceTable, { type Column } from "@/app/components/ResourceTable";

const columns: Column<RayClusterFleetListItem>[] = [
  { header: "Name", accessor: (r) => r.name },
  { header: "Namespace", accessor: (r) => r.namespace },
  { header: "Replicas", accessor: (r) => `${r.readyReplicas}/${r.replicas}` },
  { header: "Available", accessor: (r) => String(r.availableReplicas) },
  { header: "Age", accessor: (r) => new Date(r.createdAt).toLocaleDateString() },
];

export default function RayClusterFleetList() {
  const [items, setItems] = useState<RayClusterFleetListItem[]>([]);
  const [error, setError] = useState("");

  const load = () => rayClusterFleetApi.list().then((r) => setItems(r.items)).catch((e) => setError(e.message));

  useEffect(() => { load(); const t = setInterval(load, 10000); return () => clearInterval(t); }, []);

  const handleDelete = async (row: RayClusterFleetListItem) => {
    if (!confirm(`Delete RayClusterFleet "${row.name}"?`)) return;
    await rayClusterFleetApi.delete(row.namespace, row.name);
    load();
  };

  return (
    <div>
      <div className="flex items-center justify-between mb-6">
        <h2 className="text-2xl font-bold text-[var(--foreground)]">RayClusterFleets</h2>
        <Link to="/rayclusterfleets/create" className="px-4 py-2 bg-[var(--primary)] text-[var(--primary-foreground)] text-sm font-medium rounded-md hover:opacity-90">
          Create
        </Link>
      </div>
      {error && <div className="text-[var(--destructive)] mb-4">{error}</div>}
      <ResourceTable
        columns={columns}
        data={items}
        getKey={(r) => `${r.namespace}/${r.name}`}
        getLink={(r) => `/rayclusterfleets/${r.namespace}/${r.name}`}
        onDelete={handleDelete}
      />
    </div>
  );
}
