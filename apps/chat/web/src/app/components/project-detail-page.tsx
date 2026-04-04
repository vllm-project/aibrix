import { ArrowLeft, FileText, Github, MoreHorizontal, Pencil, Plus, Star, Upload } from 'lucide-react'
import { useEffect, useRef, useState } from 'react'
import { useNavigate, useParams } from 'react-router'
import { createConversation, getProject, notifyConversationsChanged, type Project, updateProject } from '@/api/client'
import { AddTextContentModal } from './add-text-content-modal'
import { ChatInput } from './chat-input'
import { SetInstructionsModal } from './set-instructions-modal'

interface ProjectFile {
  id: string
  name: string
  type: 'text' | 'file'
}

export function ProjectDetailPage() {
  const { id } = useParams()
  const navigate = useNavigate()

  const [project, setProject] = useState<Project | null>(null)
  const [loading, setLoading] = useState(true)
  const [showInstructionsModal, setShowInstructionsModal] = useState(false)
  const [showAddTextModal, setShowAddTextModal] = useState(false)
  const [showFilesMenu, setShowFilesMenu] = useState(false)
  const [instructions, setInstructions] = useState('')
  const [files, setFiles] = useState<ProjectFile[]>([])

  const filesMenuRef = useRef<HTMLDivElement>(null)

  useEffect(() => {
    if (!id) return
    let cancelled = false
    setLoading(true)
    getProject(id)
      .then((p) => {
        if (cancelled) return
        setProject(p)
        setInstructions(p.instructions)
        setLoading(false)
      })
      .catch(() => {
        if (!cancelled) setLoading(false)
      })
    return () => {
      cancelled = true
    }
  }, [id])

  useEffect(() => {
    function handleClickOutside(event: MouseEvent) {
      if (filesMenuRef.current && !filesMenuRef.current.contains(event.target as Node)) {
        setShowFilesMenu(false)
      }
    }
    document.addEventListener('mousedown', handleClickOutside)
    return () => document.removeEventListener('mousedown', handleClickOutside)
  }, [])

  const handleSaveInstructions = async (newInstructions: string) => {
    setInstructions(newInstructions)
    if (id) {
      try {
        await updateProject(id, { instructions: newInstructions })
      } catch (err) {
        console.error('Failed to save instructions:', err)
      }
    }
  }

  const handleSend = async (message: string, model: string) => {
    try {
      const conv = await createConversation(model || undefined, undefined, id)
      notifyConversationsChanged()
      navigate(`/chat/${conv.id}`, {
        state: { firstMessage: message, model },
      })
    } catch (err) {
      console.error('Failed to create conversation:', err)
    }
  }

  if (loading) {
    return <div className="flex-1 flex items-center justify-center text-muted-foreground">Loading...</div>
  }

  if (!project) {
    return <div className="flex-1 flex items-center justify-center text-muted-foreground">Project not found</div>
  }

  return (
    <div className="flex-1 flex h-full overflow-hidden">
      {/* Main content */}
      <div className="flex-1 flex flex-col px-8 py-6 overflow-y-auto">
        <button
          onClick={() => navigate('/projects')}
          className="flex items-center gap-1.5 text-sm text-muted-foreground hover:text-foreground transition-colors mb-4 w-fit"
        >
          <ArrowLeft size={14} />
          All projects
        </button>

        <div className="flex items-center gap-3 mb-8">
          <h1 style={{ fontSize: '1.4rem', fontWeight: 500 }}>{project.name}</h1>
          <button className="p-1 rounded hover:bg-accent text-foreground/40 hover:text-foreground transition-colors">
            <MoreHorizontal size={18} />
          </button>
          <button className="p-1 rounded hover:bg-accent text-foreground/40 hover:text-foreground transition-colors">
            <Star size={18} />
          </button>
        </div>

        {/* Centered chat input area — flex-1 so it fills remaining space */}
        <div className="flex-1 flex flex-col items-center justify-center pb-12">
          <ChatInput onSend={handleSend} placeholder="Reply..." />
          <div className="mt-5 max-w-[680px] w-full">
            <div className="bg-card/50 border border-border rounded-xl p-4 text-center">
              <p className="text-sm text-muted-foreground">
                Start a chat to keep conversations organized and re-use project knowledge.
              </p>
            </div>
          </div>
        </div>
      </div>

      {/* Right sidebar */}
      <div className="w-[300px] min-w-[300px] border-l border-border p-5 overflow-y-auto">
        {/* Instructions section */}
        <div className="mb-6">
          <div className="flex items-center justify-between mb-2">
            <h3 className="text-sm">Instructions</h3>
            <button
              onClick={() => setShowInstructionsModal(true)}
              className="p-1 rounded hover:bg-accent text-foreground/40 hover:text-foreground transition-colors"
            >
              <Pencil size={14} />
            </button>
          </div>
          <div
            className="bg-card border border-border rounded-xl p-3 cursor-pointer hover:border-foreground/15 transition-colors"
            onClick={() => setShowInstructionsModal(true)}
          >
            <p className="text-xs text-muted-foreground line-clamp-3">
              {instructions || 'Click to add project instructions...'}
            </p>
          </div>
        </div>

        {/* Files section */}
        <div>
          <div className="flex items-center justify-between mb-2">
            <h3 className="text-sm">Files</h3>
            <div className="relative" ref={filesMenuRef}>
              <button
                onClick={() => setShowFilesMenu(!showFilesMenu)}
                className="p-1 rounded hover:bg-accent text-foreground/40 hover:text-foreground transition-colors"
              >
                <Plus size={14} />
              </button>

              {showFilesMenu && (
                <div className="absolute right-0 top-full mt-1 w-[200px] bg-popover border border-border rounded-xl shadow-xl p-1.5 z-50">
                  <button
                    onClick={() => setShowFilesMenu(false)}
                    className="flex items-center gap-2.5 w-full px-3 py-2 rounded-lg hover:bg-accent text-sm transition-colors"
                  >
                    <Upload size={14} className="text-foreground/60" />
                    Upload from device
                  </button>
                  <button
                    onClick={() => {
                      setShowFilesMenu(false)
                      setShowAddTextModal(true)
                    }}
                    className="flex items-center gap-2.5 w-full px-3 py-2 rounded-lg hover:bg-accent text-sm transition-colors"
                  >
                    <FileText size={14} className="text-foreground/60" />
                    Add text content
                  </button>
                  <div className="border-t border-border my-1" />
                  <button
                    onClick={() => setShowFilesMenu(false)}
                    className="flex items-center gap-2.5 w-full px-3 py-2 rounded-lg hover:bg-accent text-sm transition-colors"
                  >
                    <Github size={14} className="text-foreground/60" />
                    GitHub
                  </button>
                  <button
                    onClick={() => setShowFilesMenu(false)}
                    className="flex items-center gap-2.5 w-full px-3 py-2 rounded-lg hover:bg-accent text-sm transition-colors"
                  >
                    <span className="text-sm">🔗</span>
                    Google Drive
                  </button>
                </div>
              )}
            </div>
          </div>

          {files.length === 0 ? (
            <div className="bg-card border border-border rounded-xl p-6 flex flex-col items-center justify-center text-center">
              <div className="flex items-center gap-2 mb-3 opacity-30">
                <FileText size={24} />
                <FileText size={20} />
              </div>
              <p className="text-xs text-muted-foreground">
                Add PDFs, documents, or other text to reference in this project.
              </p>
            </div>
          ) : (
            <div className="space-y-2">
              {files.map((file) => (
                <div key={file.id} className="bg-card border border-border rounded-lg p-3 flex items-center gap-2">
                  <FileText size={14} className="text-foreground/60" />
                  <span className="text-sm truncate">{file.name}</span>
                </div>
              ))}
            </div>
          )}
        </div>
      </div>

      {/* Modals */}
      <SetInstructionsModal
        isOpen={showInstructionsModal}
        onClose={() => setShowInstructionsModal(false)}
        onSave={handleSaveInstructions}
        projectName={project.name}
        initialInstructions={instructions}
      />

      <AddTextContentModal
        isOpen={showAddTextModal}
        onClose={() => setShowAddTextModal(false)}
        onAdd={(title) => {
          setFiles((prev) => [...prev, { id: Date.now().toString(), name: title, type: 'text' }])
        }}
      />
    </div>
  )
}
