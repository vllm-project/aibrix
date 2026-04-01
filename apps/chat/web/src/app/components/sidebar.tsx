import { useState, useEffect, useCallback } from "react";
import { useNavigate, useLocation } from "react-router";
import {
  Plus,
  MessageSquare,
  FolderKanban,
  Blocks,
  Code,
  PanelLeftClose,
  PanelLeft,
  Sparkles,
} from "lucide-react";
import { useAuth } from "@/app/context/auth-context";
import { UserMenu } from "./user-menu";
import { ChatItem, type ChatItemData } from "./chat-item";
import { RenameChatModal } from "./rename-chat-modal";
import { MoveChatModal } from "./move-chat-modal";
import { DeleteChatModal } from "./delete-chat-modal";
import {
  listConversations,
  renameConversation,
  deleteConversation,
} from "@/api/client";

const navItems = [
  { icon: MessageSquare, label: "Chats", path: "/chats" },
  { icon: Sparkles, label: "AI Creation", path: "/ai-creation", badge: "New" },
  { icon: FolderKanban, label: "Projects", path: "/projects" },
  { icon: Blocks, label: "Artifacts", path: "/artifacts" },
  { icon: Code, label: "Code", path: "/code" },
];

interface SidebarProps {
  collapsed: boolean;
  onToggle: () => void;
}

export function Sidebar({ collapsed, onToggle }: SidebarProps) {
  const navigate = useNavigate();
  const location = useLocation();
  const { user } = useAuth();
  const [chats, setChats] = useState<ChatItemData[]>([]);

  // Modal state
  const [renameTarget, setRenameTarget] = useState<ChatItemData | null>(null);
  const [moveTarget, setMoveTarget] = useState<ChatItemData | null>(null);
  const [deleteTarget, setDeleteTarget] = useState<ChatItemData | null>(null);

  // Fetch conversations from API
  const loadChats = useCallback(async () => {
    try {
      const conversations = await listConversations();
      setChats(
        conversations.map((c) => ({
          id: c.id,
          title: c.title,
          starred: false,
        }))
      );
    } catch {
      // Keep current state on error
    }
  }, []);

  // Load on mount and listen for updates
  useEffect(() => {
    loadChats();
    const handler = () => loadChats();
    window.addEventListener("conversations-changed", handler);
    return () => window.removeEventListener("conversations-changed", handler);
  }, [loadChats]);

  // Re-fetch when navigating (e.g. after creating a chat)
  useEffect(() => {
    loadChats();
  }, [location.pathname, loadChats]);

  const starredChats = chats.filter((c) => c.starred);
  const recentChats = chats.filter((c) => !c.starred);

  // ── Handlers ──────────────────────────────────────────

  const handleStar = (id: string) => {
    setChats((prev) =>
      prev.map((c) => (c.id === id ? { ...c, starred: !c.starred } : c))
    );
  };

  const handleRequestRename = (id: string) => {
    const chat = chats.find((c) => c.id === id);
    if (chat) setRenameTarget(chat);
  };

  const handleRenameConfirm = async (newTitle: string) => {
    if (renameTarget) {
      try {
        await renameConversation(renameTarget.id, newTitle);
        setChats((prev) =>
          prev.map((c) =>
            c.id === renameTarget.id ? { ...c, title: newTitle } : c
          )
        );
      } catch {
        // Ignore rename errors
      }
    }
    setRenameTarget(null);
  };

  const handleRequestMove = (id: string) => {
    const chat = chats.find((c) => c.id === id);
    if (chat) setMoveTarget(chat);
  };

  const handleMoveConfirm = (projectId: string) => {
    if (moveTarget) {
      console.log(`Moved "${moveTarget.title}" to project ${projectId}`);
    }
    setMoveTarget(null);
  };

  const handleRequestDelete = (id: string) => {
    const chat = chats.find((c) => c.id === id);
    if (chat) setDeleteTarget(chat);
  };

  const handleDeleteConfirm = async () => {
    if (deleteTarget) {
      try {
        await deleteConversation(deleteTarget.id);
        setChats((prev) => prev.filter((c) => c.id !== deleteTarget.id));
        if (location.pathname === `/chat/${deleteTarget.id}`) {
          navigate("/");
        }
      } catch {
        // Ignore delete errors
      }
    }
    setDeleteTarget(null);
  };

  // ── Render helpers ────────────────────────────────────

  const renderChatList = (list: ChatItemData[]) =>
    list.map((chat) => (
      <ChatItem
        key={chat.id}
        chat={chat}
        isActive={location.pathname === `/chat/${chat.id}`}
        onClick={() => navigate(`/chat/${chat.id}`)}
        onStar={handleStar}
        onRequestRename={handleRequestRename}
        onRequestMove={handleRequestMove}
        onRequestDelete={handleRequestDelete}
      />
    ));

  return (
    <>
      <div
        className={`flex flex-col h-full bg-sidebar text-sidebar-foreground transition-all duration-300 ${
          collapsed
            ? "w-0 overflow-hidden"
            : "w-[220px] min-w-[220px] overflow-visible"
        }`}
      >
        <div className="flex items-center justify-between px-4 pt-3 pb-1">
          <span
            className="cursor-pointer text-[15px] font-semibold text-sidebar-foreground"
            onClick={() => navigate("/")}
          >
            AIBrix Chat
          </span>
          <button
            onClick={onToggle}
            className="p-1 rounded-md hover:bg-sidebar-accent text-sidebar-foreground/60 hover:text-sidebar-foreground transition-colors"
          >
            <PanelLeftClose size={18} />
          </button>
        </div>

        <button
          onClick={() => navigate("/")}
          className="flex items-center gap-2 mx-3 mt-3 mb-1 px-2 py-1.5 rounded-lg hover:bg-sidebar-accent transition-colors text-sm"
        >
          <Plus size={16} />
          <span>New chat</span>
        </button>

        <nav className="flex flex-col gap-0.5 px-3 mt-1">
          {navItems.map((item) => {
            const isActive =
              location.pathname === item.path ||
              location.pathname.startsWith(item.path + "/");
            return (
              <button
                key={item.path}
                onClick={() => navigate(item.path)}
                className={`flex items-center gap-2.5 px-2 py-1.5 rounded-lg transition-colors text-sm ${
                  isActive
                    ? "bg-sidebar-accent text-sidebar-foreground"
                    : "text-sidebar-foreground/80 hover:bg-sidebar-accent hover:text-sidebar-foreground"
                }`}
              >
                <item.icon size={16} />
                <span>{item.label}</span>
                {"badge" in item && item.badge && (
                  <span className="ml-auto text-[10px] px-1.5 py-0.5 rounded-full bg-amber-500/20 text-amber-400">
                    {item.badge}
                  </span>
                )}
              </button>
            );
          })}
        </nav>

        {/* Scrollable chat lists */}
        <div className="flex-1 min-h-0 overflow-y-auto mt-4 px-3 space-y-4">
          {starredChats.length > 0 && (
            <div>
              <p className="px-2 mb-1.5 text-xs text-sidebar-foreground/40 tracking-wide uppercase">
                Starred
              </p>
              <div className="flex flex-col gap-0.5">
                {renderChatList(starredChats)}
              </div>
            </div>
          )}

          {recentChats.length > 0 && (
            <div>
              <p className="px-2 mb-1.5 text-xs text-sidebar-foreground/40 tracking-wide uppercase">
                Recents
              </p>
              <div className="flex flex-col gap-0.5">
                {renderChatList(recentChats)}
              </div>
            </div>
          )}
        </div>

        <UserMenu
          userName={user?.name ?? "User"}
          userEmail=""
          planName=""
        />
      </div>

      {/* ── Modals ── */}
      <RenameChatModal
        isOpen={!!renameTarget}
        currentTitle={renameTarget?.title ?? ""}
        onClose={() => setRenameTarget(null)}
        onSave={handleRenameConfirm}
      />

      <MoveChatModal
        isOpen={!!moveTarget}
        onClose={() => setMoveTarget(null)}
        onMove={handleMoveConfirm}
      />

      <DeleteChatModal
        isOpen={!!deleteTarget}
        onClose={() => setDeleteTarget(null)}
        onDelete={handleDeleteConfirm}
      />
    </>
  );
}

export function SidebarToggle({ onClick }: { onClick: () => void }) {
  return (
    <button
      onClick={onClick}
      className="fixed top-3 left-3 z-50 p-1.5 rounded-md bg-background/80 backdrop-blur hover:bg-accent text-foreground/60 hover:text-foreground transition-colors"
    >
      <PanelLeft size={18} />
    </button>
  );
}
