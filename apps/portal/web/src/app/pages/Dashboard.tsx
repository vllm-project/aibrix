import { useEffect, useState } from "react";
import { Link } from "react-router";
import { overviewApi, type OverviewResponse, type OverviewCount } from "@/api/overview";

const cards: { label: string; key: keyof OverviewResponse; path: string }[] = [
  { label: "ModelAdapters", key: "modelAdapters", path: "/modeladapters" },
  { label: "RayClusterFleets", key: "rayClusterFleets", path: "/rayclusterfleets" },
  { label: "StormServices", key: "stormServices", path: "/stormservices" },
  { label: "PodAutoscalers", key: "podAutoscalers", path: "/podautoscalers" },
  { label: "KVCaches", key: "kvCaches", path: "/kvcaches" },
  { label: "PodSets", key: "podSets", path: "/podsets" },
];

function CountCard({ label, count, path }: { label: string; count: OverviewCount; path: string }) {
  return (
    <Link to={path} className="block bg-[var(--card)] rounded-lg border border-[var(--border)] p-5 hover:shadow-md transition-shadow">
      <p className="text-sm font-medium text-[var(--muted-foreground)]">{label}</p>
      <p className="mt-2 text-3xl font-bold text-[var(--foreground)]">{count.total}</p>
      <div className="mt-2 flex gap-3 text-xs">
        <span style={{ color: "var(--badge-green-text)" }}>{count.ready} ready</span>
        {count.notReady > 0 && <span style={{ color: "var(--badge-red-text)" }}>{count.notReady} not ready</span>}
      </div>
    </Link>
  );
}

export default function Dashboard() {
  const [data, setData] = useState<OverviewResponse | null>(null);
  const [error, setError] = useState("");

  useEffect(() => {
    overviewApi.get().then(setData).catch((e) => setError(e.message));
    const timer = setInterval(() => { overviewApi.get().then(setData); }, 10000);
    return () => clearInterval(timer);
  }, []);

  if (error) return <div className="text-[var(--destructive)]">Error: {error}</div>;
  if (!data) return <div className="text-[var(--muted-foreground)]">Loading...</div>;

  return (
    <div>
      <h2 className="text-2xl font-bold text-[var(--foreground)] mb-6">Dashboard</h2>
      <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 gap-4">
        {cards.map((c) => (
          <CountCard key={c.key} label={c.label} count={data[c.key]} path={c.path} />
        ))}
      </div>
    </div>
  );
}
