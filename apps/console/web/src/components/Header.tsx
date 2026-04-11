import { useState, useEffect } from 'react';
import { Bell, Search } from 'lucide-react';
import { getUserInfo } from '../utils/api';
import type { UserInfo } from '../utils/api';
import { handleLogout } from '../utils/auth';

export function Header() {
  const [user, setUser] = useState<UserInfo | null>(null);
  const [showDropdown, setShowDropdown] = useState(false);

  useEffect(() => {
    getUserInfo()
      .then(u => setUser(u))
      .catch(err => console.error('Failed to fetch user info:', err));
  }, []);

  const displayName = user?.name || user?.email || 'User';
  const initial = displayName.charAt(0).toUpperCase();

  return (
    <header className="bg-white border-b border-gray-200 px-6 py-2.5">
      <div className="flex items-center justify-between">
        <div className="relative w-72">
          <Search className="absolute left-3 top-1/2 -translate-y-1/2 w-4 h-4 text-gray-400" />
          <input
            type="text"
            placeholder="Search..."
            className="w-full pl-9 pr-4 py-1.5 bg-gray-50 border border-gray-200 rounded-lg text-sm focus:outline-none focus:ring-2 focus:ring-teal-500/30 focus:border-teal-500"
          />
        </div>
        <div className="flex items-center gap-3">
          <button className="relative p-2 text-gray-400 hover:text-gray-600 hover:bg-gray-100 rounded-lg transition-colors">
            <Bell className="w-4 h-4" />
            <span className="absolute top-1.5 right-1.5 w-1.5 h-1.5 bg-teal-500 rounded-full" />
          </button>
          <div className="w-px h-6 bg-gray-200" />
          <div className="relative">
            <button
              onClick={() => setShowDropdown(!showDropdown)}
              className="flex items-center gap-2 px-2 py-1.5 hover:bg-gray-50 rounded-lg transition-colors"
            >
              <div className="w-7 h-7 rounded-full bg-gradient-to-br from-teal-500 to-cyan-500 flex items-center justify-center text-white text-xs">
                {initial}
              </div>
              <span className="text-sm text-gray-600">{displayName}</span>
              <svg className="w-3.5 h-3.5 text-gray-400" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M19 9l-7 7-7-7" />
              </svg>
            </button>
            {showDropdown && (
              <div className="absolute right-0 mt-1 w-48 bg-white rounded-lg shadow-lg border border-gray-100 py-1 z-50">
                {user?.email && (
                  <div className="px-4 py-2 text-xs text-gray-400 border-b border-gray-100">
                    {user.email}
                  </div>
                )}
                <button
                  onClick={() => {
                    setShowDropdown(false);
                    handleLogout();
                  }}
                  className="w-full text-left px-4 py-2 text-sm text-gray-700 hover:bg-gray-50"
                >
                  Log out
                </button>
              </div>
            )}
          </div>
        </div>
      </div>
    </header>
  );
}
