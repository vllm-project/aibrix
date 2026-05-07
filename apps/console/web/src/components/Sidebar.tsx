import { Home, Rocket, Boxes, Library, Gamepad2, Layers } from 'lucide-react';
import { useEffect, useState } from 'react';
import { useLocation, useNavigate } from 'react-router-dom';
import type { ComponentType, SVGProps } from 'react';
import { getUserInfo } from '../utils/api';
import type { UserInfo } from '../utils/api';

interface NavItem {
  path: string;          // primary route
  matches?: string[];    // additional pathname prefixes that should highlight this item
  icon: ComponentType<SVGProps<SVGSVGElement> & { className?: string }>;
  label: string;
}

export function Sidebar() {
  const location = useLocation();
  const navigate = useNavigate();
  const [user, setUser] = useState<UserInfo | null>(null);

  useEffect(() => {
    getUserInfo()
      .then((u) => setUser(u))
      .catch(() => {});
  }, []);

  const displayName = user?.name || user?.email || 'User';
  const initial = displayName.charAt(0).toUpperCase();

  // Settings entry intentionally omitted — API Keys / Secrets pages exist but
  // are not yet wired to backend behavior.
  const navItems: NavItem[] = [
    { path: '/home', icon: Home, label: 'Home' },
  ];

  const apiItems: NavItem[] = [
    { path: '/batch', icon: Boxes, label: 'Batch API' },
    { path: '/deployments', icon: Rocket, label: 'Deployments' },
    { path: '/lora', icon: Layers, label: 'LoRA' },
  ];

  const exploreItems: NavItem[] = [
    { path: '/models', icon: Library, label: 'Model Library' },
    { path: '/playground', icon: Gamepad2, label: 'Playground' },
  ];

  const isActive = (item: NavItem) => {
    const prefixes = [item.path, ...(item.matches ?? [])];
    return prefixes.some(
      (p) => location.pathname === p || location.pathname.startsWith(p + '/'),
    );
  };

  const NavSection = ({ title, items }: { title?: string; items: NavItem[] }) => (
    <div className="mb-6">
      {title && <div className="px-3 mb-2 text-xs text-slate-400 uppercase tracking-wider">{title}</div>}
      <div className="space-y-0.5">
        {items.map((item) => {
          const Icon = item.icon;
          const active = isActive(item);
          return (
            <button
              key={item.path}
              onClick={() => navigate(item.path)}
              className={`w-full flex items-center gap-3 px-3 py-2 rounded-lg text-sm transition-colors ${
                active
                  ? 'bg-teal-500/15 text-teal-400'
                  : 'text-slate-300 hover:bg-slate-800 hover:text-white'
              }`}
            >
              <Icon className="w-4 h-4" />
              {item.label}
            </button>
          );
        })}
      </div>
    </div>
  );

  return (
    <aside className="w-60 bg-slate-900 flex flex-col">
      <div className="p-4 border-b border-slate-700/50">
        <div className="flex items-center gap-2.5">
          <div className="w-8 h-8 bg-gradient-to-br from-teal-400 to-emerald-600 rounded-lg flex items-center justify-center">
            <svg className="w-5 h-5 text-white" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
              <polygon points="12 2 22 8.5 22 15.5 12 22 2 15.5 2 8.5 12 2" />
              <line x1="12" y1="22" x2="12" y2="15.5" />
              <polyline points="22 8.5 12 15.5 2 8.5" />
            </svg>
          </div>
          <span className="text-white text-sm tracking-wide">AIBrix</span>
        </div>
      </div>

      <div className="flex-1 overflow-y-auto p-3 pt-4">
        <NavSection items={navItems} />
        <NavSection title="API" items={apiItems} />
        <NavSection title="Explore" items={exploreItems} />
      </div>

      {user && (
        <div className="p-3 border-t border-slate-700/50">
          <div className="flex items-center gap-2.5 px-3 py-2">
            <div className="w-7 h-7 rounded-full bg-gradient-to-br from-teal-500 to-cyan-500 flex items-center justify-center text-white text-xs">
              {initial}
            </div>
            <div className="text-xs text-slate-400 truncate">{displayName}</div>
          </div>
        </div>
      )}
    </aside>
  );
}
