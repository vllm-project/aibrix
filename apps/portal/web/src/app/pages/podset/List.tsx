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

  useEffect(() => {
    let active = true;
    const poll = async () => {
      try {
        const result = await podSetApi.list();
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

  const handleDelete = async (row: PodSetListItem) => {
    if (!confirm(`Delete PodSet "${row.name}"?`)) return;
    await podSetApi.delete(row.namespace, row.name);
    const result = await podSetApi.list();
    setItems(result.items);
  };

  return (
    <div>
      <div className="flex items-center justify-between mb-6">
        <h2 className="text-2xl font-bold text-[var(--foreground)]">PodSets</h2>
        <Link to="/podsets/create" className="px-4 py-2 bg-[var(--primary)] text-[var(--primary-foreground)] text-sm font-medium rounded-md hover:opacity-90">
          Create
        </Link>
      </div>
      {error && <div className="text-[var(--destructive)] mb-4">{error}</div>}
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
