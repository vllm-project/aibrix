import { createBrowserRouter } from 'react-router'
import { AICreationPage } from './components/ai-creation-page'
import { AuthGuard } from './components/auth-guard'
import { AuthLayout } from './components/auth-layout'
import { ChatPage } from './components/chat-page'
import { ChatsPage } from './components/chats-page'
import { HomeWrapper } from './components/home-wrapper'
import { Layout } from './components/layout'
import { LoginPage } from './components/login-page'
import { ArtifactsPlaceholder, CodePlaceholder } from './components/placeholder-pages'
import { ProjectDetailPage } from './components/project-detail-page'
import { ProjectsWrapper } from './components/projects-wrapper'

export const router = createBrowserRouter([
  {
    Component: AuthLayout,
    children: [
      { path: '/login', Component: LoginPage },
      {
        Component: AuthGuard,
        children: [
          {
            path: '/',
            Component: Layout,
            children: [
              { index: true, Component: HomeWrapper },
              { path: 'chat/:id', Component: ChatPage },
              { path: 'chats', Component: ChatsPage },
              { path: 'ai-creation', Component: AICreationPage },
              { path: 'projects', Component: ProjectsWrapper },
              { path: 'projects/:id', Component: ProjectDetailPage },
              { path: 'artifacts', Component: ArtifactsPlaceholder },
              { path: 'code', Component: CodePlaceholder },
            ],
          },
        ],
      },
    ],
  },
])
