import { useState, useRef, useEffect } from "react";
import {
  MoreHorizontal,
  Star,
  Pencil,
  FolderInput,
  Trash2,
} from "lucide-react";

export interface ChatItemData {
  id: string;
  title: string;
  starred: boolean;
}

interface ChatItemProps {
  chat: ChatItemData;
  isActive: boolean;
  onClick: () => void;
  onStar: (id: string) => void;
  onRequestRename: (id: string) => void;
  onRequestMove: (id: string) => void;
  onRequestDelete: (id: string) => void;
}

export function ChatItem({
  chat,
  isActive,
  onClick,
  onStar,
  onRequestRename,
  onRequestMove,
  onRequestDelete,
}: ChatItemProps) {
  const [menuOpen, setMenuOpen] = useState(false);
  const menuRef = useRef<HTMLDivElement>(null);

  // Close menu on outside click
  useEffect(() => {
    if (!menuOpen) return;
    function handleClickOutside(event: MouseEvent) {
      if (menuRef.current && !menuRef.current.contains(event.target as Node)) {
        setMenuOpen(false);
      }
    }
    document.addEventListener("mousedown", handleClickOutside);
    return () => document.removeEventListener("mousedown", handleClickOutside);
  }, [menuOpen]);

  const menuItems = [
    {
      icon: Star,
      label: chat.starred ? "Unstar" : "Star",
      onClick: () => {
        onStar(chat.id);
        setMenuOpen(false);
      },
      iconFill: chat.starred ? "currentColor" : "none",
      iconClass: chat.starred ? "text-amber-400" : "text-foreground/50",
    },
    {
      icon: Pencil,
      label: "Rename",
      onClick: () => {
        onRequestRename(chat.id);
        setMenuOpen(false);
      },
      iconClass: "text-foreground/50",
    },
    {
      icon: FolderInput,
      label: "Move to project",
      onClick: () => {
        onRequestMove(chat.id);
        setMenuOpen(false);
      },
      iconClass: "text-foreground/50",
    },
    {
      icon: Trash2,
      label: "Delete",
      onClick: () => {
        onRequestDelete(chat.id);
        setMenuOpen(false);
      },
      isDanger: true,
      iconClass: "text-red-400",
    },
  ];

  return (
    <div className="relative group">
      <button
        onClick={onClick}
        className={`flex items-center w-full text-left pl-2 pr-7 py-1.5 rounded-lg text-sm transition-colors truncate ${
          isActive
            ? "bg-sidebar-accent text-sidebar-foreground"
            : "text-sidebar-foreground/70 hover:bg-sidebar-accent hover:text-sidebar-foreground"
        }`}
      >
        {chat.starred && (
          <Star
            size={12}
            className="text-amber-400 mr-1.5 flex-shrink-0"
            fill="currentColor"
          />
        )}
        <span className="truncate">{chat.title}</span>
      </button>

      {/* Three-dot button */}
      <div ref={menuRef}>
        <button
          onClick={(e) => {
            e.stopPropagation();
            setMenuOpen(!menuOpen);
          }}
          className={`absolute right-1 top-1/2 -translate-y-1/2 p-0.5 rounded-md transition-all ${
            menuOpen
              ? "opacity-100 bg-sidebar-accent"
              : "opacity-0 group-hover:opacity-100 hover:bg-sidebar-accent"
          }`}
        >
          <MoreHorizontal size={16} className="text-sidebar-foreground/60" />
        </button>

        {/* Context menu dropdown */}
        {menuOpen && (
          <div className="absolute right-0 top-full mt-1 w-[175px] bg-popover border border-border rounded-xl shadow-xl z-[100] py-1.5">
            {menuItems.map((item) => (
              <div key={item.label}>
                {item.isDanger && (
                  <div className="border-t border-border mx-2 my-1" />
                )}
                <button
                  onClick={(e) => {
                    e.stopPropagation();
                    item.onClick();
                  }}
                  className={`flex items-center gap-2.5 w-full px-3 py-1.5 text-sm transition-colors ${
                    item.isDanger
                      ? "text-red-400 hover:bg-red-500/10"
                      : "text-foreground hover:bg-accent"
                  }`}
                >
                  <item.icon
                    size={15}
                    fill={
                      "iconFill" in item
                        ? (item.iconFill as string)
                        : "none"
                    }
                    className={item.iconClass}
                  />
                  <span>{item.label}</span>
                </button>
              </div>
            ))}
          </div>
        )}
      </div>
    </div>
  );
}
