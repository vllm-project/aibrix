import { Blocks, Camera, Check, ChevronRight, Github, Globe, Paintbrush, Paperclip, Plus, Search } from 'lucide-react'
import { useCallback, useEffect, useRef, useState } from 'react'
import { Tooltip } from './tooltip'

interface PlusMenuProps {
  onAddFilesOrPhotos?: () => void
}

export function PlusMenu({ onAddFilesOrPhotos }: PlusMenuProps) {
  const [isOpen, setIsOpen] = useState(false)
  const [activeSubmenu, setActiveSubmenu] = useState<'style' | null>(null)
  const [webSearchEnabled, setWebSearchEnabled] = useState(true)
  const containerRef = useRef<HTMLDivElement>(null)

  const closeMenu = useCallback(() => {
    setIsOpen(false)
    setActiveSubmenu(null)
  }, [])

  useEffect(() => {
    function handleClickOutside(event: MouseEvent) {
      if (containerRef.current && !containerRef.current.contains(event.target as Node)) {
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

  return (
    <div className="relative" ref={containerRef}>
      <Tooltip content="Add files, connectors, and more" position="bottom">
        <button
          type="button"
          onClick={() => {
            setIsOpen(!isOpen)
            setActiveSubmenu(null)
          }}
          className="p-1.5 rounded-lg hover:bg-accent text-foreground/50 hover:text-foreground transition-colors"
        >
          <Plus size={20} />
        </button>
      </Tooltip>

      {isOpen && (
        <div className="absolute bottom-full mb-2 left-0 w-[240px] bg-popover border border-border rounded-xl shadow-xl z-50">
          {/* File section */}
          <div className="p-1.5">
            <MenuItem
              icon={<Paperclip size={16} />}
              label="Add files or photos"
              onClick={() => {
                closeMenu()
                setTimeout(() => onAddFilesOrPhotos?.(), 0)
              }}
              onMouseEnter={() => setActiveSubmenu(null)}
            />
            <MenuItem
              icon={<Camera size={16} />}
              label="Take a screenshot"
              onClick={closeMenu}
              onMouseEnter={() => setActiveSubmenu(null)}
            />
            <MenuItem
              icon={<Github size={16} />}
              label="Add from GitHub"
              onClick={closeMenu}
              onMouseEnter={() => setActiveSubmenu(null)}
            />
          </div>

          <div className="border-t border-border mx-1.5" />

          {/* Tools section */}
          <div className="p-1.5">
            <MenuItem
              icon={<Search size={16} />}
              label="Research"
              onClick={closeMenu}
              onMouseEnter={() => setActiveSubmenu(null)}
            />
            <MenuItem
              icon={<Globe size={16} />}
              label="Web search"
              isToggleActive={webSearchEnabled}
              onClick={() => setWebSearchEnabled(!webSearchEnabled)}
              onMouseEnter={() => setActiveSubmenu(null)}
            />
            <div className="relative">
              <MenuItem
                icon={<Paintbrush size={16} />}
                label="Use style"
                hasSubmenu
                isActive={activeSubmenu === 'style'}
                onMouseEnter={() => setActiveSubmenu('style')}
              />
              {/* Style Submenu */}
              {activeSubmenu === 'style' && (
                <div className="absolute left-full top-0 ml-1 w-[180px] bg-popover border border-border rounded-xl shadow-xl z-50">
                  <div className="p-1.5">
                    {['Normal', 'Concise', 'Formal', 'Explanatory'].map((style) => (
                      <button
                        type="button"
                        key={style}
                        onClick={closeMenu}
                        className="w-full text-left px-3 py-2 rounded-lg hover:bg-accent text-sm text-foreground transition-colors"
                      >
                        {style}
                      </button>
                    ))}
                  </div>
                </div>
              )}
            </div>
            <MenuItem
              icon={<Blocks size={16} />}
              label="Add connectors"
              onClick={closeMenu}
              onMouseEnter={() => setActiveSubmenu(null)}
            />
          </div>
        </div>
      )}
    </div>
  )
}

/* ---- Reusable Menu Item ---- */
interface MenuItemProps {
  icon: React.ReactNode
  label: string
  hasSubmenu?: boolean
  isActive?: boolean
  isToggleActive?: boolean
  onClick?: () => void
  onMouseEnter?: () => void
}

function MenuItem({ icon, label, hasSubmenu, isActive, isToggleActive, onClick, onMouseEnter }: MenuItemProps) {
  const isGreen = isToggleActive !== undefined && isToggleActive

  return (
    <button
      type="button"
      onClick={onClick}
      onMouseEnter={onMouseEnter}
      className={`flex items-center justify-between w-full px-3 py-2 rounded-lg transition-colors ${
        isActive ? 'bg-accent' : 'hover:bg-accent'
      }`}
    >
      <div className="flex items-center gap-2.5">
        <span className={isGreen ? 'text-emerald-400' : 'text-foreground/60'}>{icon}</span>
        <span className={`text-sm ${isGreen ? 'text-emerald-400' : 'text-foreground'}`}>{label}</span>
      </div>
      {isToggleActive && <Check size={14} className="text-emerald-400" />}
      {hasSubmenu && <ChevronRight size={14} className="text-foreground/40" />}
    </button>
  )
}
