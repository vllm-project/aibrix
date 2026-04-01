import { useState } from "react";

interface AddTextContentModalProps {
  isOpen: boolean;
  onClose: () => void;
  onAdd: (title: string, content: string) => void;
}

export function AddTextContentModal({
  isOpen,
  onClose,
  onAdd,
}: AddTextContentModalProps) {
  const [title, setTitle] = useState("");
  const [content, setContent] = useState("");

  if (!isOpen) return null;

  const handleAdd = () => {
    if (title.trim()) {
      onAdd(title, content);
      setTitle("");
      setContent("");
      onClose();
    }
  };

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center">
      <div className="absolute inset-0 bg-black/60" onClick={onClose} />
      <div className="relative bg-card border border-border rounded-2xl w-full max-w-[560px] p-8 shadow-2xl">
        <h2 className="mb-5" style={{ fontSize: "1.25rem", fontWeight: 500 }}>
          Add text content
        </h2>

        <div className="mb-4">
          <label className="block mb-1.5 text-sm text-foreground/70">Title</label>
          <input
            type="text"
            value={title}
            onChange={(e) => setTitle(e.target.value)}
            placeholder="Name your content"
            className="w-full px-4 py-3 bg-accent/50 border border-blue-500/40 rounded-xl text-foreground placeholder-foreground/30 outline-none focus:border-blue-500/60 transition-colors"
          />
        </div>

        <div className="mb-6">
          <label className="block mb-1.5 text-sm text-foreground/70">Content</label>
          <textarea
            value={content}
            onChange={(e) => setContent(e.target.value)}
            placeholder="Type or paste in content..."
            rows={8}
            className="w-full px-4 py-3 bg-accent/50 border border-border rounded-xl text-foreground placeholder-foreground/30 outline-none resize-none focus:border-foreground/20 transition-colors"
          />
        </div>

        <div className="flex items-center justify-end gap-3">
          <button
            onClick={onClose}
            className="px-5 py-2 rounded-lg text-sm text-foreground/70 hover:text-foreground hover:bg-accent transition-colors"
          >
            Cancel
          </button>
          <button
            onClick={handleAdd}
            disabled={!title.trim()}
            className="px-5 py-2 rounded-lg text-sm bg-foreground text-background hover:opacity-90 transition-opacity disabled:opacity-40"
          >
            Add Content
          </button>
        </div>
      </div>
    </div>
  );
}
