import { useState, useMemo } from 'react';
import { Search, Zap, Monitor, AudioLines, ImageIcon, Eye, Grid3X3, Link2, X, Video } from 'lucide-react';
import { mockModels } from '../data/mockData';
import type { ModelCategory, Model } from '../data/mockData';

type FilterTab = 'Featured' | 'All' | ModelCategory;

interface ModelLibraryProps {
  onSelectModel: (id: string) => void;
}

function TabIcon({ tab, isActive }: { tab: FilterTab; isActive: boolean }) {
  const activeColor = 'text-white';
  const inactiveColors: Record<FilterTab, string> = {
    Featured: 'text-amber-500',
    All: '',
    LLM: 'text-blue-600',
    Audio: 'text-green-600',
    Image: 'text-orange-500',
    Video: 'text-red-500',
    Vision: 'text-violet-600',
    Embedding: 'text-cyan-600',
    Reranks: 'text-pink-600',
  };
  const color = isActive ? activeColor : (inactiveColors[tab] || 'text-gray-500');
  const cls = `w-4 h-4 ${color}`;

  switch (tab) {
    case 'Featured': return <Zap className={cls} />;
    case 'LLM': return <Monitor className={cls} />;
    case 'Audio': return <AudioLines className={cls} />;
    case 'Image': return <ImageIcon className={cls} />;
    case 'Video': return <Video className={cls} />;
    case 'Vision': return <Eye className={cls} />;
    case 'Embedding': return <Grid3X3 className={cls} />;
    case 'Reranks': return <Link2 className={cls} />;
    default: return null;
  }
}

const tabs: FilterTab[] = ['Featured', 'All', 'LLM', 'Audio', 'Image', 'Video', 'Vision', 'Embedding', 'Reranks'];

function getPricingSummary(model: Model): string {
  const parts: string[] = [];
  const p = model.pricing;
  if (p.uncachedInput) parts.push(`${p.uncachedInput} Uncached Input`);
  if (p.cachedInput) parts.push(`${p.cachedInput} Cached Input`);
  if (p.output) parts.push(`${p.output} Output`);
  if (p.perMinute) parts.push(p.perMinute);
  if (p.perStep) parts.push(p.perStep);
  if (p.perEa) parts.push(p.perEa);
  if (p.perTokens) parts.push(p.perTokens);
  if (model.contextLength) parts.push(model.contextLength);
  return parts.join(' • ');
}

function ModelIcon({ model, size = 'md' }: { model: Model; size?: 'sm' | 'md' }) {
  const sizeClass = size === 'sm' ? 'w-8 h-8 text-sm' : 'w-10 h-10 text-base';
  return (
    <div className={`${sizeClass} ${model.iconBg} rounded-lg flex items-center justify-center ${model.iconTextColor} font-semibold flex-shrink-0`}>
      {model.iconText}
    </div>
  );
}

function CategoryBadge({ category }: { category: string }) {
  const colorMap: Record<string, string> = {
    LLM: 'text-blue-700 border-blue-200 bg-blue-50',
    Audio: 'text-green-700 border-green-200 bg-green-50',
    Image: 'text-orange-700 border-orange-200 bg-orange-50',
    Video: 'text-red-700 border-red-200 bg-red-50',
    Vision: 'text-violet-700 border-violet-200 bg-violet-50',
    Embedding: 'text-cyan-700 border-cyan-200 bg-cyan-50',
    Reranks: 'text-pink-700 border-pink-200 bg-pink-50',
  };
  const iconMap: Record<string, React.ReactNode> = {
    LLM: <Monitor className="w-3 h-3" />,
    Audio: <AudioLines className="w-3 h-3" />,
    Image: <ImageIcon className="w-3 h-3" />,
    Video: <Video className="w-3 h-3" />,
    Vision: <Eye className="w-3 h-3" />,
    Embedding: <Grid3X3 className="w-3 h-3" />,
    Reranks: <Link2 className="w-3 h-3" />,
  };
  return (
    <span className={`inline-flex items-center gap-1 px-2.5 py-1 rounded-full border text-xs ${colorMap[category] || 'text-gray-600 border-gray-200'}`}>
      {iconMap[category]}
      {category}
    </span>
  );
}

export function ModelLibrary({ onSelectModel }: ModelLibraryProps) {
  const [activeTab, setActiveTab] = useState<FilterTab>('Featured');
  const [searchQuery, setSearchQuery] = useState('');

  const filteredModels = useMemo(() => {
    let models = mockModels;

    if (searchQuery.trim()) {
      const q = searchQuery.toLowerCase();
      models = models.filter(m =>
        m.name.toLowerCase().includes(q) || m.provider.toLowerCase().includes(q)
      );
    }

    if (activeTab !== 'Featured' && activeTab !== 'All') {
      models = models.filter(m => m.categories.includes(activeTab as ModelCategory));
    }

    return models;
  }, [activeTab, searchQuery]);

  // For Featured view, group by category
  const featuredGroups = useMemo(() => {
    if (activeTab !== 'Featured') return {};
    const groups: Record<string, Model[]> = {};
    const categoryOrder: ModelCategory[] = ['LLM', 'Audio', 'Image', 'Video', 'Vision', 'Embedding', 'Reranks'];
    for (const cat of categoryOrder) {
      const catModels = filteredModels.filter(m => m.categories.includes(cat));
      if (catModels.length > 0) {
        groups[cat] = catModels.slice(0, 4); // Show top 4 per category
      }
    }
    return groups;
  }, [activeTab, filteredModels]);

  return (
    <div className="p-8">
      {/* Hero */}
      {activeTab === 'Featured' && (
        <div className="text-center mb-8">
          <h1 className="text-3xl mb-6">Open Source GenAI Models Library</h1>
          <div className="max-w-2xl mx-auto relative mb-4">
            <Search className="absolute left-4 top-1/2 transform -translate-y-1/2 w-4 h-4 text-gray-400" />
            <input
              type="text"
              placeholder="Search"
              value={searchQuery}
              onChange={(e) => setSearchQuery(e.target.value)}
              className="w-full pl-11 pr-4 py-3 bg-white rounded-full text-sm focus:outline-none focus:ring-2 focus:ring-teal-500/30 border border-gray-200 shadow-sm"
            />
          </div>
          <a href="#" className="text-sm text-gray-500 hover:text-gray-700 inline-flex items-center gap-1">
            Recommend a model ↗
          </a>
        </div>
      )}

      {/* Non-featured search */}
      {activeTab !== 'Featured' && (
        <div className="mb-6 max-w-md relative">
          <Search className="absolute left-3 top-1/2 transform -translate-y-1/2 w-4 h-4 text-gray-400" />
          <input
            type="text"
            placeholder="Search"
            value={searchQuery}
            onChange={(e) => setSearchQuery(e.target.value)}
            className="w-full pl-10 pr-4 py-2 bg-white rounded-full text-sm focus:outline-none focus:ring-2 focus:ring-teal-500/30 border border-gray-200"
          />
        </div>
      )}

      {/* Category Tabs */}
      <div className="flex flex-wrap items-center gap-2 mb-6">
        {tabs.map((tab) => {
          const isActive = activeTab === tab;
          return (
            <button
              key={tab}
              onClick={() => setActiveTab(tab)}
              className={`inline-flex items-center gap-1.5 px-4 py-2 rounded-full text-sm border transition-colors ${
                isActive
                  ? 'bg-slate-800 text-white border-slate-800'
                  : 'bg-white text-gray-600 border-gray-200 hover:bg-gray-50'
              }`}
            >
              <TabIcon tab={tab} isActive={isActive} />
              {tab}
            </button>
          );
        })}
      </div>

      {/* Category header for specific category */}
      {activeTab !== 'Featured' && activeTab !== 'All' && (
        <div className="flex items-center gap-2 mb-4">
          <h2 className="text-lg">
            {activeTab} ({filteredModels.length})
          </h2>
          <button
            onClick={() => setActiveTab('Featured')}
            className="text-gray-400 hover:text-gray-600"
          >
            <X className="w-4 h-4" />
          </button>
        </div>
      )}

      {/* "All" view header */}
      {activeTab === 'All' && (
        <div className="mb-4">
          <h2 className="text-lg">
            All Models ({filteredModels.length})
          </h2>
        </div>
      )}

      {/* Featured Grid View */}
      {activeTab === 'Featured' && (
        <div className="space-y-8">
          {Object.entries(featuredGroups).map(([category, models]) => (
            <div key={category}>
              <div className="flex items-center justify-between mb-4">
                <h2 className="text-lg">{category}</h2>
                <button
                  onClick={() => setActiveTab(category as FilterTab)}
                  className="text-sm text-gray-400 hover:text-gray-600"
                >
                  View All
                </button>
              </div>
              <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 xl:grid-cols-4 gap-4">
                {models.map((model) => (
                  <div
                    key={model.id}
                    onClick={() => onSelectModel(model.id)}
                    className="bg-white rounded-xl shadow-sm border border-gray-100 p-5 cursor-pointer hover:shadow-md hover:border-gray-200 transition-all group"
                  >
                    <div className="flex items-start justify-between mb-3">
                      <ModelIcon model={model} />
                      {model.isNew && (
                        <span className="px-2 py-0.5 bg-teal-500 text-white text-xs rounded-full">
                          NEW
                        </span>
                      )}
                    </div>
                    <h3 className="text-sm mb-2 group-hover:text-teal-700 transition-colors">
                      {model.name}
                    </h3>
                    <p className="text-xs text-gray-400 line-clamp-2">
                      {getPricingSummary(model)}
                    </p>
                  </div>
                ))}
              </div>
            </div>
          ))}
        </div>
      )}

      {/* List View (category-specific or All) */}
      {activeTab !== 'Featured' && (
        <div className="bg-white rounded-xl shadow-sm border border-gray-100 overflow-hidden divide-y divide-gray-50">
          {filteredModels.length === 0 ? (
            <div className="p-8 text-center text-gray-400 text-sm">
              No models found matching your criteria.
            </div>
          ) : (
            filteredModels.map((model) => (
              <div
                key={model.id}
                onClick={() => onSelectModel(model.id)}
                className="flex items-center gap-4 px-6 py-4 hover:bg-gray-50/50 cursor-pointer transition-colors"
              >
                <ModelIcon model={model} size="sm" />
                <div className="flex-1 min-w-0 flex items-center gap-4">
                  <span className="text-sm min-w-[180px] max-w-[260px] truncate">
                    {model.name}
                  </span>
                  {model.isNew && (
                    <span className="px-2 py-0.5 bg-teal-500 text-white text-xs rounded-full flex-shrink-0">
                      NEW
                    </span>
                  )}
                  <span className="text-sm text-gray-400 truncate flex-1">
                    {getPricingSummary(model)}
                  </span>
                </div>
                <div className="flex items-center gap-2 flex-shrink-0">
                  {model.categories.map((cat) => (
                    <CategoryBadge key={cat} category={cat} />
                  ))}
                </div>
              </div>
            ))
          )}
        </div>
      )}
    </div>
  );
}
