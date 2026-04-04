import { Folder, Loader2, Search, X } from 'lucide-react'
import { useEffect, useState } from 'react'
import { listProjects, type ProjectSummary } from '@/api/client'

interface MoveChatModalProps {
  isOpen: boolean
  onClose: () => void
  onMove: (projectId: string) => void
}

export function MoveChatModal({ isOpen, onClose, onMove }: MoveChatModalProps) {
  const [searchQuery, setSearchQuery] = useState('')
  const [isMoving, setIsMoving] = useState(false)
  const [projects, setProjects] = useState<ProjectSummary[]>([])
  const [loading, setLoading] = useState(false)

  // Fetch projects from API when modal opens
  useEffect(() => {
    if (!isOpen) return
    let cancelled = false
    setLoading(true)
    listProjects()
      .then((data) => {
        if (!cancelled) setProjects(data)
      })
      .finally(() => {
        if (!cancelled) setLoading(false)
      })
    return () => {
      cancelled = true
    }
  }, [isOpen])

  if (!isOpen) return null

  const filtered = projects.filter((p) => p.name.toLowerCase().includes(searchQuery.toLowerCase()))

  const handleMove = (projectId: string) => {
    setIsMoving(true)
    setTimeout(() => {
      onMove(projectId)
      setIsMoving(false)
      setSearchQuery('')
      onClose()
    }, 1200)
  }

  return (
    <div className="fixed inset-0 z-[200] flex items-center justify-center">
      <div className="absolute inset-0 bg-black/60" onClick={onClose} />
      <div
        className="relative bg-card border border-border rounded-2xl w-full max-w-[440px] shadow-2xl flex flex-col"
        style={{ maxHeight: '420px' }}
      >
        {/* Header */}
        <div className="flex items-start justify-between px-6 pt-5 pb-1">
          <div>
            <h2 style={{ fontSize: '1.1rem' }}>Move chat</h2>
            <p className="text-sm text-foreground/50 mt-0.5">Select a project to move this chat into.</p>
          </div>
          <button
            onClick={() => {
              setSearchQuery('')
              onClose()
            }}
            className="p-1 rounded-md text-foreground/40 hover:text-foreground hover:bg-accent transition-colors -mt-0.5"
          >
            <X size={18} />
          </button>
        </div>

        {/* Body */}
        <div className="flex-1 min-h-0 px-6 pb-5 pt-3 flex flex-col">
          <div className="border border-border rounded-xl overflow-hidden flex flex-col flex-1 min-h-0 relative">
            {/* Search */}
            <div className="flex items-center gap-2.5 px-3 py-2.5 border-b border-border bg-accent/30">
              <Search size={16} className="text-foreground/40 flex-shrink-0" />
              <input
                type="text"
                placeholder="Search or create a project"
                value={searchQuery}
                onChange={(e) => setSearchQuery(e.target.value)}
                className="flex-1 bg-transparent text-sm text-foreground placeholder-foreground/30 outline-none"
                disabled={isMoving}
              />
            </div>

            {/* Project list */}
            <div className="flex-1 overflow-y-auto">
              {isMoving ? (
                <div className="flex flex-col items-center justify-center py-12 text-foreground/30">
                  <Folder size={32} className="mb-2 animate-pulse" />
                  <span className="text-sm">Moving...</span>
                </div>
              ) : loading ? (
                <div className="flex flex-col items-center justify-center py-12 text-foreground/30">
                  <Loader2 size={24} className="mb-2 animate-spin" />
                  <span className="text-sm">Loading projects...</span>
                </div>
              ) : filtered.length > 0 ? (
                filtered.map((project) => (
                  <button
                    key={project.id}
                    onClick={() => handleMove(project.id)}
                    className="flex items-center gap-3 w-full px-3 py-2.5 text-sm text-foreground hover:bg-accent transition-colors"
                  >
                    <Folder size={16} className="text-foreground/50 flex-shrink-0" />
                    <span className="flex-1 text-left truncate">{project.name}</span>
                  </button>
                ))
              ) : (
                <div className="flex flex-col items-center justify-center py-12 text-foreground/30">
                  <span className="text-sm">No projects found</span>
                </div>
              )}
            </div>
          </div>
        </div>
      </div>
    </div>
  )
}
