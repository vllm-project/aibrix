import { useState, useEffect } from 'react';
import { Copy, MoreVertical, X, Key } from 'lucide-react';
import { listAPIKeys, createAPIKey, deleteAPIKey } from '../../utils/api';
import type { APIKey } from '../../utils/api';
import { copyToClipboard } from '../../utils/clipboard';

interface ApiKeysPageProps {
  onToast: (message: string, subtitle?: string) => void;
}

export function ApiKeysPage({ onToast }: ApiKeysPageProps) {
  const [apiKeys, setApiKeys] = useState<APIKey[]>([]);
  const [loading, setLoading] = useState(true);
  const [showCreateModal, setShowCreateModal] = useState(false);
  const [showCopyModal, setShowCopyModal] = useState(false);
  const [newKeyName, setNewKeyName] = useState('');
  const [generatedKey, setGeneratedKey] = useState('');
  const [creating, setCreating] = useState(false);

  const fetchKeys = () => {
    setLoading(true);
    listAPIKeys()
      .then(keys => setApiKeys(keys))
      .catch(err => console.error('Failed to fetch API keys:', err))
      .finally(() => setLoading(false));
  };

  useEffect(() => {
    fetchKeys();
  }, []);

  const handleCreateKey = () => {
    if (!newKeyName.trim()) return;

    setCreating(true);
    createAPIKey(newKeyName)
      .then(result => {
        setApiKeys(prev => [...prev, result.apiKey]);
        setGeneratedKey(result.fullKey);
        setShowCreateModal(false);
        setShowCopyModal(true);
        setNewKeyName('');
      })
      .catch(err => {
        console.error('Failed to create API key:', err);
        onToast('Failed to create API key');
      })
      .finally(() => setCreating(false));
  };

  const handleDeleteKey = (id: string) => {
    deleteAPIKey(id)
      .then(() => {
        setApiKeys(prev => prev.filter(k => k.id !== id));
        onToast('API key deleted');
      })
      .catch(err => {
        console.error('Failed to delete API key:', err);
        onToast('Failed to delete API key');
      });
  };

  const handleCopyKey = () => {
    copyToClipboard(generatedKey).then(() => {
      onToast('Copied to clipboard');
    }).catch(() => {
      onToast('Copied to clipboard');
    });
  };

  return (
    <div className="p-8">
      {/* Header */}
      <div className="flex items-start justify-between mb-8">
        <div>
          <h1 className="text-2xl mb-1">API Keys</h1>
          <p className="text-sm text-gray-500">Authenticate programmatically with AIBrix</p>
        </div>
        <button
          onClick={() => setShowCreateModal(true)}
          className="flex items-center gap-2 px-5 py-2.5 bg-teal-600 text-white rounded-lg text-sm hover:bg-teal-700 transition-colors"
        >
          + Create API Key
        </button>
      </div>

      {/* Table */}
      <div className="bg-white rounded-xl shadow-sm border border-gray-100 overflow-hidden">
        <table className="w-full">
          <thead>
            <tr className="border-b border-gray-100 bg-gray-50/80">
              <th className="text-left px-6 py-3 text-sm text-gray-500">
                <div className="flex items-center gap-1">
                  Name
                  <svg className="w-3 h-3 text-gray-400" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                    <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M7 16V4m0 0L3 8m4-4l4 4m6 0v12m0 0l4-4m-4 4l-4-4" />
                  </svg>
                </div>
              </th>
              <th className="text-left px-6 py-3 text-sm text-gray-500">
                <div className="flex items-center gap-1">
                  Secret key
                  <svg className="w-3 h-3 text-gray-400" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                    <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M7 16V4m0 0L3 8m4-4l4 4m6 0v12m0 0l4-4m-4 4l-4-4" />
                  </svg>
                </div>
              </th>
              <th className="text-left px-6 py-3 text-sm text-gray-500">
                <div className="flex items-center gap-1">
                  Create time
                  <svg className="w-3 h-3 text-gray-400" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                    <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M7 16V4m0 0L3 8m4-4l4 4m6 0v12m0 0l4-4m-4 4l-4-4" />
                  </svg>
                </div>
              </th>
              <th className="w-10"></th>
            </tr>
          </thead>
          <tbody>
            {loading ? (
              <tr>
                <td colSpan={4} className="px-6 py-8 text-center text-sm text-gray-400">
                  Loading API keys...
                </td>
              </tr>
            ) : apiKeys.length === 0 ? (
              <tr>
                <td colSpan={4} className="px-6 py-8 text-center text-sm text-gray-400">
                  No API keys found. Create one to get started.
                </td>
              </tr>
            ) : (
              apiKeys.map((key) => (
              <tr key={key.id} className="border-b border-gray-50 hover:bg-gray-50/50 transition-colors">
                <td className="px-6 py-4">
                  <div className="flex items-center gap-3">
                    <div className="w-8 h-8 rounded-full bg-teal-50 flex items-center justify-center">
                      <Key className="w-4 h-4 text-teal-600" />
                    </div>
                    <div>
                      <div className="text-sm">{key.name}</div>
                      <div className="text-xs text-gray-400 flex items-center gap-1">
                        ID: {key.id}
                        <button
                          className="hover:text-gray-600"
                          onClick={() => {
                            copyToClipboard(key.id);
                            onToast('Copied to clipboard');
                          }}
                        >
                          <Copy className="w-3 h-3" />
                        </button>
                      </div>
                    </div>
                  </div>
                </td>
                <td className="px-6 py-4 text-sm text-gray-500">{key.secretKey}</td>
                <td className="px-6 py-4">
                  <div className="text-sm text-gray-900">
                    {key.createdAt.split(' ').slice(0, 3).join(' ')}
                  </div>
                  <div className="text-xs text-gray-400">
                    {key.createdAt.split(' ').slice(3).join(' ')}
                  </div>
                </td>
                <td className="px-4 py-4">
                  <button
                    className="text-gray-300 hover:text-gray-500"
                    onClick={() => handleDeleteKey(key.id)}
                  >
                    <MoreVertical className="w-4 h-4" />
                  </button>
                </td>
              </tr>
            )))}
          </tbody>
        </table>
      </div>

      {/* Create API Key Modal */}
      {showCreateModal && (
        <div className="fixed inset-0 bg-black/40 flex items-center justify-center z-50">
          <div className="bg-white rounded-xl shadow-2xl w-full max-w-md p-6">
            <div className="flex items-start justify-between mb-1">
              <div className="flex items-center gap-2">
                <Key className="w-5 h-5 text-gray-700" />
                <h2 className="text-lg">Create API Key</h2>
              </div>
              <button
                onClick={() => { setShowCreateModal(false); setNewKeyName(''); }}
                className="text-gray-400 hover:text-gray-600"
              >
                <X className="w-5 h-5" />
              </button>
            </div>
            <p className="text-sm text-gray-500 mb-6">
              Add a name to your API key to help you identify it later.
            </p>

            <div className="mb-6">
              <label className="block text-sm mb-2">
                API Key Name<span className="text-red-500">*</span>
              </label>
              <input
                type="text"
                value={newKeyName}
                onChange={(e) => setNewKeyName(e.target.value)}
                placeholder="Enter a name"
                className="w-full px-4 py-2.5 border-2 border-teal-500 rounded-lg text-sm focus:outline-none focus:ring-2 focus:ring-teal-200 placeholder-gray-400"
                autoFocus
              />
            </div>

            <div className="flex justify-end">
              <button
                onClick={handleCreateKey}
                disabled={!newKeyName.trim() || creating}
                className="px-6 py-2.5 bg-teal-600 text-white rounded-lg text-sm hover:bg-teal-700 transition-colors disabled:opacity-50 disabled:cursor-not-allowed"
              >
                {creating ? 'Generating...' : 'Generate Key'}
              </button>
            </div>
          </div>
        </div>
      )}

      {/* Copy API Key Modal */}
      {showCopyModal && (
        <div className="fixed inset-0 bg-black/40 flex items-center justify-center z-50">
          <div className="bg-white rounded-xl shadow-2xl w-full max-w-md p-6">
            <div className="flex items-start justify-between mb-4">
              <h2 className="text-lg">Copy your API Key</h2>
              <button
                onClick={() => setShowCopyModal(false)}
                className="text-gray-400 hover:text-gray-600"
              >
                <X className="w-5 h-5" />
              </button>
            </div>

            <div className="border-l-4 border-red-500 pl-4 mb-6">
              <p className="text-sm text-red-600">This value is viewable one time only</p>
              <p className="text-sm text-gray-500">Copy the below key to your clipboard and store it somewhere safe</p>
            </div>

            <div className="flex items-center gap-3 mb-6">
              <span className="text-sm text-gray-600">API Key:</span>
              <code className="px-3 py-1.5 bg-gray-100 rounded-md text-sm font-mono text-gray-800 border border-gray-200">
                {generatedKey}
              </code>
              <button
                onClick={handleCopyKey}
                className="text-gray-400 hover:text-gray-600 p-1"
              >
                <Copy className="w-4 h-4" />
              </button>
            </div>

            <div className="flex justify-end">
              <button
                onClick={() => setShowCopyModal(false)}
                className="px-6 py-2.5 bg-teal-600 text-white rounded-lg text-sm hover:bg-teal-700 transition-colors"
              >
                Close
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}
