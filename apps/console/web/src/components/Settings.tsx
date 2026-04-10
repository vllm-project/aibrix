import { Key, Shield, Gauge, ChevronLeft } from 'lucide-react';

export type SettingsTab = 'api-keys' | 'secrets' | 'quotas';

const tabs = [
  { id: 'api-keys' as SettingsTab, icon: Key, label: 'API Keys' },
  { id: 'secrets' as SettingsTab, icon: Shield, label: 'Secrets' },
  { id: 'quotas' as SettingsTab, icon: Gauge, label: 'Quotas' },
];

interface SettingsSidebarProps {
  activeTab: SettingsTab;
  onTabChange: (tab: SettingsTab) => void;
  onBack: () => void;
}

export function SettingsSidebar({ activeTab, onTabChange, onBack }: SettingsSidebarProps) {
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
        <div className="mb-4">
          <button
            onClick={onBack}
            className="flex items-center gap-2 text-sm text-slate-400 hover:text-white transition-colors px-3 py-2"
          >
            <ChevronLeft className="w-4 h-4" />
            <span>Back</span>
          </button>
        </div>

        <div className="mb-2 px-3 text-xs text-slate-400 uppercase tracking-wider">Settings</div>
        <nav className="space-y-0.5">
          {tabs.map((tab) => {
            const Icon = tab.icon;
            const isActive = activeTab === tab.id;
            return (
              <button
                key={tab.id}
                onClick={() => onTabChange(tab.id)}
                className={`w-full flex items-center gap-3 px-3 py-2 rounded-lg text-sm transition-colors ${
                  isActive
                    ? 'bg-teal-500/15 text-teal-400'
                    : 'text-slate-300 hover:bg-slate-800 hover:text-white'
                }`}
              >
                <Icon className="w-4 h-4" />
                {tab.label}
              </button>
            );
          })}
        </nav>
      </div>
    </aside>
  );
}