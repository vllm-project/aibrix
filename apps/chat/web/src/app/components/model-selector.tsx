import { Check, ChevronDown, Loader2 } from 'lucide-react'
import { useCallback, useEffect, useLayoutEffect, useRef, useState } from 'react'
import { fetchModels, type ModelInfo } from '@/api/client'

interface ModelSelectorProps {
  selectedModel: string
  onModelChange: (model: string) => void
}

export function ModelSelector({ selectedModel, onModelChange }: ModelSelectorProps) {
  const [isOpen, setIsOpen] = useState(false)
  const [openUpward, setOpenUpward] = useState(false)
  const [models, setModels] = useState<ModelInfo[]>([])
  const [loading, setLoading] = useState(true)
  const dropdownRef = useRef<HTMLDivElement>(null)
  const menuRef = useRef<HTMLDivElement>(null)

  // Fetch models from API on mount
  useEffect(() => {
    let cancelled = false
    setLoading(true)
    fetchModels()
      .then((m) => {
        if (!cancelled) {
          setModels(m)
          // Auto-select first model if current selection is empty or not in list
          if (m.length > 0 && !m.find((x) => x.id === selectedModel)) {
            onModelChange(m[0].id)
          }
        }
      })
      .finally(() => {
        if (!cancelled) setLoading(false)
      })
    return () => {
      cancelled = true
    }
  }, [onModelChange, selectedModel]) // eslint-disable-line react-hooks/exhaustive-deps

  const closeMenu = useCallback(() => setIsOpen(false), [])

  useEffect(() => {
    function handleClickOutside(event: MouseEvent) {
      if (dropdownRef.current && !dropdownRef.current.contains(event.target as Node)) {
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

  useLayoutEffect(() => {
    if (!isOpen) return

    const updatePlacement = () => {
      const triggerEl = dropdownRef.current
      const menuEl = menuRef.current
      if (!triggerEl || !menuEl) return

      const triggerRect = triggerEl.getBoundingClientRect()
      const menuHeight = menuEl.offsetHeight
      const spaceBelow = window.innerHeight - triggerRect.bottom
      const spaceAbove = triggerRect.top

      setOpenUpward(spaceBelow < menuHeight + 8 && spaceAbove > spaceBelow)
    }

    updatePlacement()
    window.addEventListener('resize', updatePlacement)
    window.addEventListener('scroll', updatePlacement, true)

    return () => {
      window.removeEventListener('resize', updatePlacement)
      window.removeEventListener('scroll', updatePlacement, true)
    }
  }, [isOpen])

  const displayName = selectedModel || 'Select model'

  return (
    <div className="relative" ref={dropdownRef}>
      <button
        onClick={() => setIsOpen(!isOpen)}
        className="flex items-center gap-1.5 text-sm text-foreground/60 hover:text-foreground transition-colors"
      >
        {loading ? <Loader2 size={14} className="animate-spin" /> : <span>{displayName}</span>}
        <ChevronDown size={14} />
      </button>

      {isOpen && (
        <div
          ref={menuRef}
          className={`absolute right-0 w-[260px] bg-popover border border-border rounded-xl shadow-xl z-50 ${
            openUpward ? 'bottom-full mb-2' : 'top-full mt-2'
          }`}
        >
          <div className="p-1.5 max-h-[320px] overflow-y-auto">
            {loading ? (
              <div className="flex items-center justify-center py-4 text-sm text-muted-foreground">
                <Loader2 size={16} className="animate-spin mr-2" />
                Loading models...
              </div>
            ) : models.length === 0 ? (
              <div className="px-3 py-4 text-sm text-muted-foreground text-center">
                No models available.
                <br />
                <span className="text-xs">Check gateway connection.</span>
              </div>
            ) : (
              models.map((model) => (
                <button
                  key={model.id}
                  onClick={() => {
                    onModelChange(model.id)
                    setIsOpen(false)
                  }}
                  className="flex items-center justify-between w-full px-3 py-2.5 rounded-lg hover:bg-accent transition-colors text-left"
                >
                  <div>
                    <p className="text-sm text-foreground">{model.id}</p>
                    {model.owned_by && <p className="text-xs text-muted-foreground">{model.owned_by}</p>}
                  </div>
                  {selectedModel === model.id && <Check size={16} className="text-foreground/70 flex-shrink-0" />}
                </button>
              ))
            )}
          </div>
        </div>
      )}
    </div>
  )
}
