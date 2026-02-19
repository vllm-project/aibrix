import { Home, Settings, Rocket, Boxes, Library, Gamepad2, Layers } from 'lucide-react';
import type { Page } from '../App';

interface SidebarProps {
  currentPage: Page;
  onNavigate: (page: Page) => void;
}

export function Sidebar({ currentPage, onNavigate }: SidebarProps) {
  const navItems = [
    { id: 'home' as Page, icon: Home, label: 'Home' },
    { id: 'settings' as Page, icon: Settings, label: 'Settings' },
  ];

  const apiItems = [
    { id: 'deployments' as Page, icon: Rocket, label: 'Deployments' },
    { id: 'batch-jobs' as Page, icon: Boxes, label: 'Batch API' },
    { id: 'lora' as Page, icon: Layers, label: 'LoRA' },
  ];

  const exploreItems = [
    { id: 'model-library' as Page, icon: Library, label: 'Model Library' },
    { id: 'playground' as Page, icon: Gamepad2, label: 'Playground' },
  ];

  const NavSection = ({ title, items }: { title?: string; items: typeof navItems }) => (
    <div className="mb-6">
      {title && <div className="px-3 mb-2 text-xs text-slate-400 uppercase tracking-wider">{title}</div>}
      <div className="space-y-0.5">
        {items.map((item) => {
          const Icon = item.icon;
          const isActive = currentPage === item.id
            || (item.id === 'model-library' && currentPage === 'model-detail')
            || (item.id === 'deployments' && (currentPage === 'deployment-detail' || currentPage === 'create-deployment'))
            || (item.id === 'batch-jobs' && (currentPage === 'job-detail' || currentPage === 'create-job'));
          return (
            <button
              key={item.id}
              onClick={() => onNavigate(item.id)}
              className={`w-full flex items-center gap-3 px-3 py-2 rounded-lg text-sm transition-colors ${
                isActive
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

      <div className="p-3 border-t border-slate-700/50">
        <div className="flex items-center gap-2.5 px-3 py-2">
          <div className="w-7 h-7 rounded-full bg-gradient-to-br from-teal-500 to-cyan-500 flex items-center justify-center text-white text-xs">
            S
          </div>
          <div className="text-xs text-slate-400 truncate">seedjeffwan-2hvzrk1</div>
        </div>
      </div>
    </aside>
  );
}