import { useState, useEffect } from 'react';
import { Search, Eye, EyeOff, X, ChevronLeft, ChevronRight, SlidersHorizontal, ChevronsUpDown } from 'lucide-react';
import { listSecrets, createSecret, deleteSecret } from '../../utils/api';
import type { Secret } from '../../utils/api';

interface SecretsPageProps {
  onToast: (message: string, subtitle?: string) => void;
}

export function SecretsPage({ onToast }: SecretsPageProps) {
  const [secrets, setSecrets] = useState<Secret[]>([]);
  const [loading, setLoading] = useState(true);
  const [searchQuery, setSearchQuery] = useState('');
  const [showCreateModal, setShowCreateModal] = useState(false);
  const [newSecretName, setNewSecretName] = useState('');
  const [newSecretValue, setNewSecretValue] = useState('');
  const [showSecretValue, setShowSecretValue] = useState(false);
  const [creating, setCreating] = useState(false);

  const fetchSecrets = () => {
    setLoading(true);
    listSecrets(searchQuery || undefined)
      .then(s => setSecrets(s))
      .catch(err => console.error('Failed to fetch secrets:', err))
      .finally(() => setLoading(false));
  };

  useEffect(() => {
    fetchSecrets();
  }, [searchQuery]);

  const filteredSecrets = secrets;

  const handleCreateSecret = () => {
    if (!newSecretName.trim()) return;

    setCreating(true);
    createSecret(newSecretName, newSecretValue)
      .then((newSecret) => {
        setSecrets(prev => [...prev, newSecret]);
        onToast('Secret created successfully', `Secret "${newSecretName}" has been created`);
        setShowCreateModal(false);
        setNewSecretName('');
        setNewSecretValue('');
        setShowSecretValue(false);
      })
      .catch(err => {
        console.error('Failed to create secret:', err);
        onToast('Failed to create secret');
      })
      .finally(() => setCreating(false));
  };

  return (
    <div className="p-8">
      {/* Header */}
      <div className="flex items-start justify-between mb-8">
        <div>
          <h1 className="text-2xl mb-1">Secrets</h1>
          <p className="text-sm text-gray-500">View your secrets or create a new one</p>
        </div>
        <button
          onClick={() => setShowCreateModal(true)}
          className="px-5 py-2.5 bg-teal-600 text-white rounded-lg text-sm hover:bg-teal-700 transition-colors"
        >
          Create Secret
        </button>
      </div>

      {/* Search */}
      <div className="relative mb-6 w-56">
        <Search className="absolute left-3 top-1/2 transform -translate-y-1/2 w-4 h-4 text-gray-400" />
        <input
          type="text"
          value={searchQuery}
          onChange={(e) => setSearchQuery(e.target.value)}
          placeholder="Search secrets"
          className="w-full pl-10 pr-4 py-2 border border-gray-200 rounded-lg text-sm focus:outline-none focus:ring-2 focus:ring-teal-500/30 focus:border-teal-500 bg-white"
        />
      </div>

      {/* Table */}
      <div className="bg-white rounded-xl shadow-sm border border-gray-100 overflow-hidden">
        <table className="w-full">
          <thead>
            <tr className="border-b border-gray-100 bg-gray-50/80">
              <th className="text-left px-6 py-3 text-sm text-gray-500">Secrets</th>
              <th className="text-right px-6 py-3">
                <div className="flex items-center justify-end gap-2">
                  <button className="text-gray-400 hover:text-gray-600">
                    <SlidersHorizontal className="w-4 h-4" />
                  </button>
                  <button className="text-gray-400 hover:text-gray-600">
                    <ChevronsUpDown className="w-4 h-4" />
                  </button>
                </div>
              </th>
            </tr>
          </thead>
          <tbody>
            {loading ? (
              <tr>
                <td colSpan={2} className="px-6 py-8 text-center text-sm text-gray-400">
                  Loading secrets...
                </td>
              </tr>
            ) : filteredSecrets.length === 0 ? (
              <tr>
                <td colSpan={2} className="px-6 py-8 text-center text-sm text-gray-400">
                  No secrets found
                </td>
              </tr>
            ) : (
              filteredSecrets.map((secret) => (
              <tr key={secret.id} className="border-b border-gray-50 hover:bg-gray-50/50 transition-colors">
                <td className="px-6 py-4 text-sm">{secret.name}</td>
                <td className="px-6 py-4"></td>
              </tr>
            )))}
          </tbody>
        </table>
      </div>

      {/* Pagination */}
      <div className="flex items-center justify-end gap-2 mt-4">
        <button className="w-8 h-8 border border-gray-200 rounded-lg flex items-center justify-center text-gray-400 hover:bg-gray-50">
          <ChevronLeft className="w-4 h-4" />
        </button>
        <button className="w-8 h-8 border border-gray-200 rounded-lg flex items-center justify-center text-sm bg-white text-gray-700">
          1
        </button>
        <button className="w-8 h-8 border border-gray-200 rounded-lg flex items-center justify-center text-gray-400 hover:bg-gray-50">
          <ChevronRight className="w-4 h-4" />
        </button>
      </div>

      {/* Create Secret Modal */}
      {showCreateModal && (
        <div className="fixed inset-0 bg-black/40 flex items-center justify-center z-50">
          <div className="bg-white rounded-xl shadow-2xl w-full max-w-md p-6">
            <div className="flex items-start justify-between mb-1">
              <h2 className="text-lg">Create New Secret</h2>
              <button
                onClick={() => {
                  setShowCreateModal(false);
                  setNewSecretName('');
                  setNewSecretValue('');
                  setShowSecretValue(false);
                }}
                className="text-gray-400 hover:text-gray-600"
              >
                <X className="w-5 h-5" />
              </button>
            </div>
            <p className="text-sm text-gray-500 mb-6">
              Add a new secret key that can be used securely in your applications.
            </p>

            <div className="mb-4">
              <label className="block text-sm mb-2">Name</label>
              <input
                type="text"
                value={newSecretName}
                onChange={(e) => setNewSecretName(e.target.value)}
                placeholder="Enter a name for the secret"
                className="w-full px-4 py-2.5 border-2 border-teal-500 rounded-lg text-sm focus:outline-none focus:ring-2 focus:ring-teal-200 placeholder-gray-400"
                autoFocus
              />
            </div>

            <div className="mb-6">
              <label className="block text-sm mb-2">Value</label>
              <div className="relative">
                <input
                  type={showSecretValue ? 'text' : 'password'}
                  value={newSecretValue}
                  onChange={(e) => setNewSecretValue(e.target.value)}
                  placeholder="Enter secret value"
                  className="w-full px-4 py-2.5 pr-10 border border-gray-200 rounded-lg text-sm focus:outline-none focus:ring-2 focus:ring-teal-200 placeholder-gray-400"
                />
                <button
                  type="button"
                  onClick={() => setShowSecretValue(!showSecretValue)}
                  className="absolute right-3 top-1/2 transform -translate-y-1/2 text-gray-400 hover:text-gray-600"
                >
                  {showSecretValue ? (
                    <EyeOff className="w-4 h-4" />
                  ) : (
                    <Eye className="w-4 h-4" />
                  )}
                </button>
              </div>
            </div>

            <div className="flex justify-end gap-3">
              <button
                onClick={() => {
                  setShowCreateModal(false);
                  setNewSecretName('');
                  setNewSecretValue('');
                  setShowSecretValue(false);
                }}
                className="px-5 py-2.5 border border-gray-200 rounded-lg text-sm text-gray-600 hover:bg-gray-50 transition-colors"
              >
                Cancel
              </button>
              <button
                onClick={handleCreateSecret}
                disabled={!newSecretName.trim() || creating}
                className="px-5 py-2.5 bg-teal-600 text-white rounded-lg text-sm hover:bg-teal-700 transition-colors disabled:opacity-50 disabled:cursor-not-allowed"
              >
                {creating ? 'Creating...' : 'Create Secret'}
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}
