import { BookOpen, ChevronUp, ExternalLink, Github, LogOut } from 'lucide-react'
import { useCallback, useEffect, useRef, useState } from 'react'
import { useAuth } from '@/app/context/auth-context'

interface UserMenuProps {
  userName?: string
  userEmail?: string
}

const links = [
  { label: 'Documentation', href: 'https://aibrix.readthedocs.io/latest/', icon: <BookOpen size={16} /> },
  { label: 'GitHub', href: 'https://github.com/aibrix/aibrix', icon: <Github size={16} /> },
]

export function UserMenu({ userName = 'User', userEmail = '' }: UserMenuProps) {
  const { authMode, logout: authLogout } = useAuth()
  const [isOpen, setIsOpen] = useState(false)
  const containerRef = useRef<HTMLDivElement>(null)

  const closeMenu = useCallback(() => setIsOpen(false), [])

  useEffect(() => {
    function handleClickOutside(e: MouseEvent) {
      if (containerRef.current && !containerRef.current.contains(e.target as Node)) {
        closeMenu()
      }
    }
    function handleEscape(e: KeyboardEvent) {
      if (e.key === 'Escape') closeMenu()
    }
    if (isOpen) {
      document.addEventListener('mousedown', handleClickOutside)
      document.addEventListener('keydown', handleEscape)
    }
    return () => {
      document.removeEventListener('mousedown', handleClickOutside)
      document.removeEventListener('keydown', handleEscape)
    }
  }, [isOpen, closeMenu])

  const userInitial = userName.charAt(0).toUpperCase()

  return (
    <div ref={containerRef} className="relative px-3 pb-3">
      {/* Popup Menu */}
      {isOpen && (
        <div className="absolute bottom-full left-2 right-2 mb-1 z-50">
          <div className="rounded-xl border border-white/10 shadow-2xl" style={{ backgroundColor: '#352f29' }}>
            {userEmail && (
              <div className="px-4 py-2.5 text-xs text-sidebar-foreground/50 truncate border-b border-white/5">
                {userEmail}
              </div>
            )}

            <div className="py-1">
              {links.map((item) => (
                <a
                  key={item.label}
                  href={item.href}
                  target="_blank"
                  rel="noopener noreferrer"
                  onClick={closeMenu}
                  className="flex items-center gap-3 w-full px-4 py-2 text-sm text-sidebar-foreground/80 hover:bg-white/5 hover:text-sidebar-foreground transition-colors"
                >
                  <span className="text-sidebar-foreground/60">{item.icon}</span>
                  <span className="flex-1 text-left">{item.label}</span>
                  <ExternalLink size={14} className="text-sidebar-foreground/40" />
                </a>
              ))}
            </div>

            {authMode === 'simple' && (
              <div className="border-t border-white/5 py-1">
                <button
                  type="button"
                  onClick={() => {
                    closeMenu()
                    authLogout()
                  }}
                  className="flex items-center gap-3 w-full px-4 py-2 text-sm text-sidebar-foreground/80 hover:bg-white/5 hover:text-sidebar-foreground transition-colors"
                >
                  <span className="text-sidebar-foreground/60">
                    <LogOut size={16} />
                  </span>
                  <span className="flex-1 text-left">Log out</span>
                </button>
              </div>
            )}
          </div>
        </div>
      )}

      {/* User Profile Button */}
      <button
        type="button"
        onClick={() => setIsOpen(!isOpen)}
        className="flex items-center gap-2.5 w-full px-2 py-2 rounded-lg hover:bg-sidebar-accent transition-colors"
      >
        <div className="w-7 h-7 rounded-full bg-[#c4a882] flex items-center justify-center shrink-0">
          <span className="text-sm text-[#1a1714]" style={{ lineHeight: 1 }}>
            {userInitial}
          </span>
        </div>
        <div className="flex-1 min-w-0 text-left">
          <div className="text-sm text-sidebar-foreground truncate leading-tight">{userName}</div>
        </div>
        <ChevronUp
          size={14}
          className={`text-sidebar-foreground/40 transition-transform ${isOpen ? '' : 'rotate-180'}`}
        />
      </button>
    </div>
  )
}
