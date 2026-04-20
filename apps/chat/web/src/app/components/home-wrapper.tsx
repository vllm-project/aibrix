import { useState } from 'react'
import { useNavigate } from 'react-router'
import { createConversation, notifyConversationsChanged } from '@/api/client'
import { useAuth } from '@/app/context/auth-context'
import type { Attachment } from './chat-input'
import { CreateProjectModal } from './create-project-modal'
import { HomePage } from './home-page'

export function HomeWrapper() {
  const navigate = useNavigate()
  const { user } = useAuth()
  const [showCreateProject, setShowCreateProject] = useState(false)

  const handleSend = async (message: string, model: string, attachments?: Attachment[]) => {
    try {
      const conv = await createConversation(model || undefined)
      notifyConversationsChanged()
      // Navigate to chat page with the first message as state
      navigate(`/chat/${conv.id}`, {
        state: { firstMessage: message, model, attachments },
      })
    } catch (err) {
      console.error('Failed to create conversation:', err)
    }
  }

  return (
    <>
      <HomePage
        userName={user?.name ?? 'there'}
        onSend={handleSend}
        onStartNewProject={() => setShowCreateProject(true)}
      />
      <CreateProjectModal
        isOpen={showCreateProject}
        onClose={() => setShowCreateProject(false)}
        onCreate={(name) => {
          setShowCreateProject(false)
          navigate(`/projects/${encodeURIComponent(name)}`)
        }}
      />
    </>
  )
}
