import {
  useState,
  useMemo,
  useEffect,
  useCallback,
} from "react";
import { useNavigate } from "react-router";
import {
  Search,
  Plus,
  Check,
  Trash2,
  FolderInput,
  X,
  Info,
} from "lucide-react";
import { MoveChatModal } from "./move-chat-modal";

interface ChatEntry {
  id: string;
  title: string;
  lastMessage: string;
}

const initialChats: ChatEntry[] = [
  {
    id: "2",
    title: "Casual greeting",
    lastMessage: "3 hours ago",
  },
  {
    id: "1",
    title:
      "Multi-modality AI application with aibrix orchestration",
    lastMessage: "3 hours ago",
  },
  {
    id: "3",
    title: "LLM Inference Infrastructure",
    lastMessage: "5 hours ago",
  },
  {
    id: "4",
    title: "React performance optimization",
    lastMessage: "1 day ago",
  },
  {
    id: "5",
    title: "Kubernetes deployment strategy",
    lastMessage: "2 days ago",
  },
];

/* ── Toast ─────────────────────────────────────────────── */
function Toast({
  message,
  onDismiss,
}: {
  message: string;
  onDismiss: () => void;
}) {
  useEffect(() => {
    const t = setTimeout(onDismiss, 4000);
    return () => clearTimeout(t);
  }, [onDismiss]);

  return (
    <div className="fixed top-4 right-4 z-[300] flex items-center gap-2.5 px-4 py-2.5 rounded-xl bg-card border border-border shadow-2xl text-sm text-foreground animate-in slide-in-from-top-2 fade-in duration-200">
      <Info
        size={16}
        className="text-foreground/50 flex-shrink-0"
      />
      <span>{message}</span>
      <button
        onClick={onDismiss}
        className="p-0.5 rounded-md text-foreground/40 hover:text-foreground transition-colors"
      >
        <X size={14} />
      </button>
    </div>
  );
}

/* ── Delete Confirm Modal ──────────────────────────────── */
function BulkDeleteModal({
  count,
  onClose,
  onDelete,
}: {
  count: number;
  onClose: () => void;
  onDelete: () => void;
}) {
  return (
    <div className="fixed inset-0 z-[200] flex items-center justify-center">
      <div
        className="absolute inset-0 bg-black/60"
        onClick={onClose}
      />
      <div className="relative bg-card border border-border rounded-2xl w-full max-w-[380px] p-6 shadow-2xl">
        <h2
          style={{ fontSize: "1.1rem", fontWeight: 600 }}
          className="mb-2"
        >
          Delete {count} chat{count !== 1 ? "s" : ""}
        </h2>
        <p className="text-sm text-foreground/50">
          Are you sure you want to permanently delete{" "}
          {count === 1 ? "this chat" : `these ${count} chats`}?
          This cannot be undone.
        </p>

        <div className="flex items-center justify-end gap-2.5 mt-6">
          <button
            onClick={onClose}
            className="px-4 py-1.5 rounded-lg text-sm text-foreground/70 hover:text-foreground hover:bg-accent transition-colors"
          >
            Cancel
          </button>
          <button
            onClick={onDelete}
            className="px-4 py-1.5 rounded-lg text-sm bg-red-500 text-white hover:bg-red-600 transition-colors"
          >
            Delete
          </button>
        </div>
      </div>
    </div>
  );
}

/* ── Checkbox ──────────────────────────────────────────── */
function Checkbox({
  checked,
  indeterminate,
  onChange,
  className = "",
}: {
  checked: boolean;
  indeterminate?: boolean;
  onChange: () => void;
  className?: string;
}) {
  return (
    <div
      role="checkbox"
      aria-checked={indeterminate ? "mixed" : checked}
      onClick={(e) => {
        e.stopPropagation();
        onChange();
      }}
      className={`w-[18px] h-[18px] rounded flex-shrink-0 flex items-center justify-center border transition-colors cursor-pointer ${
        checked || indeterminate
          ? "bg-blue-500 border-blue-500"
          : "border-foreground/25 hover:border-foreground/45"
      } ${className}`}
    >
      {checked && (
        <Check
          size={12}
          className="text-white"
          strokeWidth={3}
        />
      )}
      {indeterminate && !checked && (
        <div className="w-2 h-0.5 rounded-full bg-white" />
      )}
    </div>
  );
}

/* ── Main Page ─────────────────────────────────────────── */
export function ChatsPage() {
  const navigate = useNavigate();
  const [chats, setChats] = useState<ChatEntry[]>(initialChats);
  const [search, setSearch] = useState("");
  const [selectMode, setSelectMode] = useState(false);
  const [selected, setSelected] = useState<Set<string>>(
    new Set(),
  );
  const [showDeleteModal, setShowDeleteModal] = useState(false);
  const [showMoveModal, setShowMoveModal] = useState(false);
  const [toast, setToast] = useState<string | null>(null);

  const filteredChats = useMemo(
    () =>
      chats.filter((c) =>
        c.title.toLowerCase().includes(search.toLowerCase()),
      ),
    [search, chats],
  );

  const toggleSelect = (id: string) => {
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  };

  const toggleSelectAll = () => {
    if (selected.size === filteredChats.length) {
      setSelected(new Set());
    } else {
      setSelected(new Set(filteredChats.map((c) => c.id)));
    }
  };

  const exitSelectMode = () => {
    setSelectMode(false);
    setSelected(new Set());
  };

  const handleBulkDelete = () => {
    const count = selected.size;
    setChats((prev) => prev.filter((c) => !selected.has(c.id)));
    setSelected(new Set());
    setSelectMode(false);
    setShowDeleteModal(false);
    setToast(`${count} chat${count !== 1 ? "s" : ""} deleted`);
  };

  const handleBulkMove = (projectId: string) => {
    const count = selected.size;
    console.log(`Moved ${count} chats to project ${projectId}`);
    setSelected(new Set());
    setSelectMode(false);
    setShowMoveModal(false);
    setToast(`${count} chat${count !== 1 ? "s" : ""} moved`);
  };

  const dismissToast = useCallback(() => setToast(null), []);

  const allSelected =
    filteredChats.length > 0 &&
    selected.size === filteredChats.length;
  const someSelected = selected.size > 0 && !allSelected;

  return (
    <div className="flex-1 overflow-y-auto">
      <div className="max-w-[800px] mx-auto px-6 py-8">
        {/* Header */}
        <div className="flex items-center justify-between mb-6">
          <h1
            style={{
              fontFamily: "'Playfair Display', serif",
              fontSize: "1.8rem",
              fontWeight: 400,
              color: "var(--foreground)",
            }}
          >
            Chats
          </h1>
          <button
            onClick={() => navigate("/")}
            className="flex items-center gap-1.5 px-4 py-2 rounded-full bg-foreground text-background text-sm hover:bg-foreground/90 transition-colors"
          >
            <Plus size={16} />
            <span>New chat</span>
          </button>
        </div>

        {/* Search */}
        <div className="relative mb-4">
          <Search
            size={16}
            className="absolute left-3.5 top-1/2 -translate-y-1/2 text-foreground/30"
          />
          <input
            type="text"
            value={search}
            onChange={(e) => setSearch(e.target.value)}
            placeholder="Search your chats..."
            className="w-full pl-10 pr-4 py-2.5 rounded-xl bg-card border border-border text-foreground placeholder-foreground/30 outline-none focus:border-foreground/20 transition-colors text-sm"
          />
        </div>

        {/* Count / Select toolbar */}
        {!selectMode ? (
          <div className="flex items-center gap-3 mb-1 px-1">
            <span className="text-sm text-muted-foreground">
              {filteredChats.length} chat
              {filteredChats.length !== 1 ? "s" : ""} with AIBrix Chat
            </span>
            <button
              onClick={() => setSelectMode(true)}
              className="text-sm text-blue-400 hover:text-blue-300 underline underline-offset-2 transition-colors"
            >
              Select
            </button>
          </div>
        ) : (
          /* ── Select‑mode toolbar ── */
          <div className="flex items-center gap-3 mb-1 px-1">
            <Checkbox
              checked={allSelected}
              indeterminate={someSelected}
              onChange={toggleSelectAll}
            />
            <span className="text-sm text-muted-foreground">
              {selected.size} selected
            </span>
            <button
              onClick={() => {
                if (selected.size > 0) setShowMoveModal(true);
              }}
              className={`p-1.5 rounded-md transition-colors ${
                selected.size > 0
                  ? "text-foreground/60 hover:text-foreground hover:bg-accent"
                  : "text-foreground/20 cursor-default"
              }`}
              title="Move to project"
            >
              <FolderInput size={17} />
            </button>
            <button
              onClick={() => {
                if (selected.size > 0) setShowDeleteModal(true);
              }}
              className={`p-1.5 rounded-md transition-colors ${
                selected.size > 0
                  ? "text-foreground/60 hover:text-foreground hover:bg-accent"
                  : "text-foreground/20 cursor-default"
              }`}
              title="Delete"
            >
              <Trash2 size={17} />
            </button>
            <div className="flex-1" />
            <button
              onClick={exitSelectMode}
              className="p-1.5 rounded-md text-foreground/50 hover:text-foreground hover:bg-accent transition-colors"
              title="Exit select mode"
            >
              <X size={18} />
            </button>
          </div>
        )}

        {/* Divider */}
        <div className="border-t border-border mt-2" />

        {/* Chat list */}
        <div>
          {filteredChats.length === 0 ? (
            <div className="py-12 text-center text-muted-foreground text-sm">
              No chats found
            </div>
          ) : (
            filteredChats.map((chat) => {
              const isSelected = selected.has(chat.id);
              return (
                <div key={chat.id}>
                  <button
                    onClick={() => {
                      if (selectMode) {
                        toggleSelect(chat.id);
                      } else {
                        navigate(`/chat/${chat.id}`);
                      }
                    }}
                    className={`flex items-center gap-3 w-full text-left px-3 py-4 rounded-lg transition-colors group ${
                      isSelected
                        ? "bg-blue-500/15"
                        : "hover:bg-accent/50"
                    }`}
                  >
                    {selectMode && (
                      <Checkbox
                        checked={isSelected}
                        onChange={() => toggleSelect(chat.id)}
                      />
                    )}
                    <div className="min-w-0 flex-1">
                      <p className="text-sm text-foreground truncate">
                        {chat.title}
                      </p>
                      <p className="text-xs text-muted-foreground mt-0.5">
                        Last message {chat.lastMessage}
                      </p>
                    </div>
                  </button>
                  <div className="border-t border-border mx-3" />
                </div>
              );
            })
          )}
        </div>
      </div>

      {/* ── Delete confirmation modal ── */}
      {showDeleteModal && selected.size > 0 && (
        <BulkDeleteModal
          count={selected.size}
          onClose={() => setShowDeleteModal(false)}
          onDelete={handleBulkDelete}
        />
      )}

      {/* ── Move modal ── */}
      <MoveChatModal
        isOpen={showMoveModal}
        onClose={() => setShowMoveModal(false)}
        onMove={handleBulkMove}
      />

      {/* ── Toast notification ── */}
      {toast && (
        <Toast message={toast} onDismiss={dismissToast} />
      )}
    </div>
  );
}