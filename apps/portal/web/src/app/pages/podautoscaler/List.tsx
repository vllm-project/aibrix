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

  useEffect(() => {
    let active = true;
    const poll = async () => {
      try {
        const result = await podAutoscalerApi.list();
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

  const handleDelete = async (row: PodAutoscalerListItem) => {
    if (!confirm(`Delete PodAutoscaler "${row.name}"?`)) return;
    await podAutoscalerApi.delete(row.namespace, row.name);
    const result = await podAutoscalerApi.list();
    setItems(result.items);
  };

  return (
    <div>
      <div className="flex items-center justify-between mb-6">
        <h2 className="text-2xl font-bold text-[var(--foreground)]">PodAutoscalers</h2>
        <Link to="/podautoscalers/create" className="px-4 py-2 bg-[var(--primary)] text-[var(--primary-foreground)] text-sm font-medium rounded-md hover:opacity-90">
          Create
        </Link>
      </div>
      {error && <div className="text-[var(--destructive)] mb-4">{error}</div>}
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
