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
    <Link to={path} className="block bg-white rounded-lg border border-gray-200 p-5 hover:shadow-md transition-shadow">
      <p className="text-sm font-medium text-gray-500">{label}</p>
      <p className="mt-2 text-3xl font-bold text-gray-900">{count.total}</p>
      <div className="mt-2 flex gap-3 text-xs">
        <span className="text-green-600">{count.ready} ready</span>
        {count.notReady > 0 && <span className="text-red-600">{count.notReady} not ready</span>}
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

  if (error) return <div className="text-red-600">Error: {error}</div>;
  if (!data) return <div className="text-gray-500">Loading...</div>;

  return (
    <div>
      <h2 className="text-2xl font-bold text-gray-900 mb-6">Dashboard</h2>
      <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 gap-4">
        {cards.map((c) => (
          <CountCard key={c.key} label={c.label} count={data[c.key]} path={c.path} />
        ))}
      </div>
    </div>
  );
}
