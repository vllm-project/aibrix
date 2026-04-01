import { createBrowserRouter } from "react-router";
import { AuthLayout } from "./components/auth-layout";
import { Layout } from "./components/layout";
import { HomeWrapper } from "./components/home-wrapper";
import { ChatPage } from "./components/chat-page";
import { ProjectsWrapper } from "./components/projects-wrapper";
import { ProjectDetailPage } from "./components/project-detail-page";
import { ArtifactsPlaceholder, CodePlaceholder } from "./components/placeholder-pages";
import { AICreationPage } from "./components/ai-creation-page";
import { ChatsPage } from "./components/chats-page";
import { LoginPage } from "./components/login-page";
import { AuthGuard } from "./components/auth-guard";

export const router = createBrowserRouter([
  {
    Component: AuthLayout,
    children: [
      { path: "/login", Component: LoginPage },
      {
        Component: AuthGuard,
        children: [
          {
            path: "/",
            Component: Layout,
            children: [
              { index: true, Component: HomeWrapper },
              { path: "chat/:id", Component: ChatPage },
              { path: "chats", Component: ChatsPage },
              { path: "ai-creation", Component: AICreationPage },
              { path: "projects", Component: ProjectsWrapper },
              { path: "projects/:id", Component: ProjectDetailPage },
              { path: "artifacts", Component: ArtifactsPlaceholder },
              { path: "code", Component: CodePlaceholder },
            ],
          },
        ],
      },
    ],
  },
]);
