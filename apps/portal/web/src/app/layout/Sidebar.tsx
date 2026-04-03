import { Link, useLocation } from "react-router";
import { LayoutDashboard, Puzzle, Server, Workflow, Gauge, Database, Boxes } from "lucide-react";
import ThemeToggle from "@/app/components/ThemeToggle";

const nav = [
  { label: "Dashboard", path: "/", icon: LayoutDashboard },
  { group: "Model", items: [
    { label: "ModelAdapters", path: "/modeladapters", icon: Puzzle },
  ]},
  { group: "Orchestration", items: [
    { label: "RayClusterFleets", path: "/rayclusterfleets", icon: Server },
    { label: "StormServices", path: "/stormservices", icon: Workflow },
    { label: "KVCaches", path: "/kvcaches", icon: Database },
    { label: "PodSets", path: "/podsets", icon: Boxes },
  ]},
  { group: "Autoscaling", items: [
    { label: "PodAutoscalers", path: "/podautoscalers", icon: Gauge },
  ]},
];

export default function Sidebar() {
  const location = useLocation();

  const linkClass = (path: string) => {
    const active = location.pathname === path;
    return `flex items-center gap-2 px-3 py-2 rounded-md text-sm transition-colors ${
      active
        ? "bg-[var(--sidebar-active-bg)] text-[var(--sidebar-active-text)] font-medium"
        : "text-[var(--sidebar-text)] hover:bg-[var(--muted)]"
    }`;
  };

  return (
    <aside className="w-60 border-r border-[var(--sidebar-border)] bg-[var(--sidebar-bg)] h-screen flex flex-col overflow-y-auto">
      <div className="px-4 py-5 border-b border-[var(--sidebar-border)] flex items-center justify-between">
        <h1 className="text-lg font-bold text-[var(--foreground)]">AIBrix Portal</h1>
        <ThemeToggle />
      </div>
      <nav className="flex-1 px-3 py-4 space-y-1">
        {nav.map((item, i) => {
          if ("path" in item) {
            const Icon = item.icon;
            return (
              <Link key={i} to={item.path} className={linkClass(item.path)}>
                <Icon size={16} />
                {item.label}
              </Link>
            );
          }
          return (
            <div key={i} className="pt-4">
              <p className="px-3 text-xs font-semibold text-[var(--sidebar-group)] uppercase tracking-wider mb-1">{item.group}</p>
              {item.items.map((sub) => {
                const Icon = sub.icon;
                return (
                  <Link key={sub.path} to={sub.path} className={linkClass(sub.path)}>
                    <Icon size={16} />
                    {sub.label}
                  </Link>
                );
              })}
            </div>
          );
        })}
      </nav>
    </aside>
  );
}
