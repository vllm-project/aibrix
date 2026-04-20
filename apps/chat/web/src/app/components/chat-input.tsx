import { ArrowUp, AudioLines, Loader2, Square, X } from 'lucide-react'
import { useCallback, useEffect, useRef, useState } from 'react'
import { transcribeAudio } from '../../api/client'
import { useAudioRecording } from '../hooks/use-audio-recording'
import { ModelSelector } from './model-selector'
import { PlusMenu } from './plus-menu'
import { Tooltip } from './tooltip'

export interface Attachment {
  id: string
  name: string
  type: string
  file: File
  uploading: boolean
  progress: number
  blob_url?: string
  kind: 'image' | 'file'
}

interface ChatInputProps {
  placeholder?: string
  disabled?: boolean
  selectedModel?: string
  onModelChange?: (model: string) => void
  onSend?: (message: string, model: string, attachments?: Attachment[]) => void
  onStartNewProject?: () => void
}

export function ChatInput({
  placeholder = 'How can I help you today?',
  disabled = false,
  selectedModel,
  onModelChange,
  onSend,
  onStartNewProject,
}: ChatInputProps) {
  const [message, setMessage] = useState('')
  const [internalSelectedModel, setInternalSelectedModel] = useState('')
  const currentModel = selectedModel ?? internalSelectedModel
  const handleModelChange = onModelChange ?? setInternalSelectedModel
  const [attachments, setAttachments] = useState<Attachment[]>([])
  const [isDragOver, setIsDragOver] = useState(false)
  const [isTranscribing, setIsTranscribing] = useState(false)
  const [audioError, setAudioError] = useState<string | null>(null)
  const textareaRef = useRef<HTMLTextAreaElement>(null)
  const fileInputRef = useRef<HTMLInputElement>(null)

  const { isRecording, duration, start, stop, cancel } = useAudioRecording()

  const hasContent = message.trim().length > 0 || attachments.some((a) => !a.uploading)

  const formatDuration = (secs: number) => {
    const m = Math.floor(secs / 60)
    const s = secs % 60
    return `${m}:${s.toString().padStart(2, '0')}`
  }

  const handleAddFilesOrPhotos = useCallback(() => {
    fileInputRef.current?.click()
  }, [])

  const handleAudioClick = async () => {
    setAudioError(null)
    try {
      await start()
    } catch {
      setAudioError('Microphone access denied')
    }
  }

  const handleAudioStop = async () => {
    setIsTranscribing(true)
    setAudioError(null)
    try {
      const file = await stop()
      const result = await transcribeAudio(file)
      setMessage((prev) => (prev ? `${prev} ${result.text}` : result.text))
      textareaRef.current?.focus()
    } catch (err) {
      setAudioError(err instanceof Error ? err.message : 'Transcription failed')
    } finally {
      setIsTranscribing(false)
    }
  }

  const handleAudioCancel = () => {
    cancel()
    setAudioError(null)
  }

  const simulateUpload = useCallback((img: Attachment) => {
    const duration = 800 + Math.random() * 600
    const steps = 12
    const stepTime = duration / steps
    let step = 0

    const interval = setInterval(() => {
      step++
      const progress = Math.min((step / steps) * 100, 100)
      setAttachments((prev) => prev.map((i) => (i.id === img.id ? { ...i, progress } : i)))
      if (step >= steps) {
        clearInterval(interval)
        setAttachments((prev) => prev.map((i) => (i.id === img.id ? { ...i, uploading: false, progress: 100 } : i)))
      }
    }, stepTime)
  }, [])

  const addFiles = useCallback(
    (files: File[]) => {
      files.forEach((file) => {
        const isImage = file.type.startsWith('image/')
        const base: Attachment = {
          id: crypto.randomUUID(),
          name: file.name,
          type: file.type,
          file,
          uploading: true,
          progress: 0,
          kind: isImage ? 'image' : 'file',
        }

        if (isImage) {
          const blob_url = URL.createObjectURL(file)
          const attachment: Attachment = { ...base, blob_url }
          setAttachments((prev) => [...prev, attachment])
          simulateUpload(attachment)
        } else {
          setAttachments((prev) => [...prev, base])
          simulateUpload(base)
        }
      })
    },
    [simulateUpload],
  )

  const handleFileInputChange = useCallback(
    (e: React.ChangeEvent<HTMLInputElement>) => {
      const files = Array.from(e.target.files ?? [])
      if (files.length === 0) return
      addFiles(files)
      e.target.value = ''
    },
    [addFiles],
  )

  const handlePaste = useCallback(
    (e: React.ClipboardEvent) => {
      const items = Array.from(e.clipboardData.items)
      const imageItems = items.filter((item) => item.type.startsWith('image/'))
      if (imageItems.length > 0) {
        e.preventDefault()
        const files = imageItems.map((item) => item.getAsFile()).filter(Boolean) as File[]
        addFiles(files)
      }
    },
    [addFiles],
  )

  const handleDrop = useCallback(
    (e: React.DragEvent) => {
      e.preventDefault()
      setIsDragOver(false)
      const files = Array.from(e.dataTransfer.files)
      addFiles(files)
    },
    [addFiles],
  )

  const handleDragOver = useCallback((e: React.DragEvent) => {
    e.preventDefault()
    setIsDragOver(true)
  }, [])

  const handleDragLeave = useCallback((e: React.DragEvent) => {
    e.preventDefault()
    setIsDragOver(false)
  }, [])

  const removeAttachment = (id: string) => {
    setAttachments((prev) => {
      const attachmentToRemove = prev.find((a) => a.id === id)
      if (attachmentToRemove?.blob_url?.startsWith('blob:')) {
        URL.revokeObjectURL(attachmentToRemove.blob_url)
      }
      return prev.filter((a) => a.id !== id)
    })
  }

  const attachmentsRef = useRef(attachments)
  attachmentsRef.current = attachments

  useEffect(() => {
    return () => {
      attachmentsRef.current.forEach((a) => {
        if (a.blob_url?.startsWith('blob:')) {
          URL.revokeObjectURL(a.blob_url)
        }
      })
    }
  }, [])

  const handleSubmit = () => {
    if (!hasContent || disabled) return
    onSend?.(
      message,
      currentModel,
      attachments.filter((a) => !a.uploading),
    )
    setMessage('')
    setAttachments([])
  }

  const handleKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault()
      handleSubmit()
    }
  }

  return (
    <div className="w-full max-w-[680px] mx-auto">
      <div
        className={`bg-card border rounded-2xl transition-colors ${
          isDragOver ? 'border-blue-500/50 bg-blue-500/5' : 'border-border'
        }`}
        onDrop={handleDrop}
        onDragOver={handleDragOver}
        onDragLeave={handleDragLeave}
      >
        {/* Attachment previews */}
        {attachments.length > 0 && (
          <div className="flex flex-wrap gap-2 px-4 pt-3">
            {attachments.map((attachment) => (
              <div key={attachment.id} className="relative group/attachment">
                <div className="w-[120px] h-[90px] rounded-xl overflow-hidden border border-border bg-accent/50">
                  {attachment.uploading ? (
                    <div className="w-full h-full flex flex-col items-center justify-center gap-2">
                      <Loader2 size={20} className="text-foreground/40 animate-spin" />
                      <div className="w-16 h-1 rounded-full bg-foreground/10 overflow-hidden">
                        <div
                          className="h-full bg-foreground/40 rounded-full transition-all duration-150"
                          style={{ width: `${attachment.progress}%` }}
                        />
                      </div>
                    </div>
                  ) : attachment.kind === 'image' && attachment.blob_url ? (
                    <img src={attachment.blob_url} alt={attachment.name} className="w-full h-full object-cover" />
                  ) : (
                    <div className="w-full h-full flex flex-col items-center justify-center px-2 text-center">
                      <div className="text-xs text-foreground/50 mb-1">FILE</div>
                      <div className="text-xs text-foreground/80 line-clamp-2 break-all">{attachment.name}</div>
                    </div>
                  )}
                </div>

                {!attachment.uploading && (
                  <button
                    onClick={() => removeAttachment(attachment.id)}
                    className="absolute -top-1.5 -right-1.5 w-5 h-5 rounded-full bg-foreground/80 text-background flex items-center justify-center opacity-0 group-hover/attachment:opacity-100 transition-opacity"
                  >
                    <X size={12} />
                  </button>
                )}
              </div>
            ))}
          </div>
        )}

        <div className="px-4 pt-4 pb-2">
          <textarea
            ref={textareaRef}
            value={message}
            onChange={(e) => setMessage(e.target.value)}
            onKeyDown={handleKeyDown}
            onPaste={handlePaste}
            placeholder={placeholder}
            disabled={disabled || isRecording || isTranscribing}
            rows={1}
            className="w-full bg-transparent text-foreground placeholder-foreground/30 resize-none outline-none text-[15px] disabled:opacity-50"
            style={{ minHeight: '24px', maxHeight: '200px' }}
            onInput={(e) => {
              const target = e.target as HTMLTextAreaElement
              target.style.height = 'auto'
              target.style.height = `${Math.min(target.scrollHeight, 200)}px`
            }}
          />
        </div>
        <div className="flex items-center justify-between px-3 pb-3">
          <PlusMenu onStartNewProject={onStartNewProject} onAddFilesOrPhotos={handleAddFilesOrPhotos} />
          <div className="flex items-center gap-3">
            <ModelSelector selectedModel={currentModel} onModelChange={handleModelChange} />

            {/* Recording mode */}
            {isRecording ? (
              <div className="flex items-center gap-2">
                <span className="flex items-center gap-1.5 text-sm text-red-500">
                  <span className="w-2 h-2 rounded-full bg-red-500 animate-pulse" />
                  {formatDuration(duration)}
                </span>
                <Tooltip content="Cancel recording">
                  <button
                    onClick={handleAudioCancel}
                    className="p-1.5 rounded-lg hover:bg-accent text-foreground/50 hover:text-foreground transition-colors"
                  >
                    <X size={18} />
                  </button>
                </Tooltip>
                <Tooltip content="Stop and transcribe">
                  <button
                    onClick={handleAudioStop}
                    className="w-8 h-8 rounded-full bg-red-600 hover:bg-red-500 text-white flex items-center justify-center transition-colors"
                  >
                    <Square size={14} fill="currentColor" />
                  </button>
                </Tooltip>
              </div>
            ) : isTranscribing ? (
              <div className="flex items-center gap-2 text-sm text-foreground/50">
                <Loader2 size={16} className="animate-spin" />
                <span>Transcribing...</span>
              </div>
            ) : hasContent ? (
              <button
                onClick={handleSubmit}
                disabled={disabled}
                className="w-8 h-8 rounded-full bg-amber-700 hover:bg-amber-600 disabled:opacity-50 text-white flex items-center justify-center transition-colors"
              >
                <ArrowUp size={18} />
              </button>
            ) : (
              <Tooltip content="Use voice mode">
                <button
                  onClick={handleAudioClick}
                  className="p-1.5 rounded-lg hover:bg-accent text-foreground/50 hover:text-foreground transition-colors"
                >
                  <AudioLines size={20} />
                </button>
              </Tooltip>
            )}
          </div>
        </div>

        {/* Audio error message */}
        {audioError && (
          <div className="px-4 pb-3">
            <p className="text-xs text-red-500">{audioError}</p>
          </div>
        )}
      </div>
      <input
        ref={fileInputRef}
        type="file"
        accept="image/*,.pdf,.txt,.doc,.docx,.md"
        multiple
        className="hidden"
        onChange={handleFileInputChange}
      />
    </div>
  )
}
