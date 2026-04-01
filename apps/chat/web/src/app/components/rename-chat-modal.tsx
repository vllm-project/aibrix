import { useState, useRef, useEffect } from "react";

interface RenameChatModalProps {
  isOpen: boolean;
  currentTitle: string;
  onClose: () => void;
  onSave: (newTitle: string) => void;
}

export function RenameChatModal({
  isOpen,
  currentTitle,
  onClose,
  onSave,
}: RenameChatModalProps) {
  const [value, setValue] = useState(currentTitle);
  const inputRef = useRef<HTMLInputElement>(null);

  useEffect(() => {
    if (isOpen) {
      setValue(currentTitle);
      setTimeout(() => {
        inputRef.current?.focus();
        inputRef.current?.select();
      }, 50);
    }
  }, [isOpen, currentTitle]);

  if (!isOpen) return null;

  const handleSave = () => {
    const trimmed = value.trim();
    if (trimmed) {
      onSave(trimmed);
    }
    onClose();
  };

  return (
    <div className="fixed inset-0 z-[200] flex items-center justify-center">
      <div className="absolute inset-0 bg-black/60" onClick={onClose} />
      <div className="relative bg-card border border-border rounded-2xl w-full max-w-[400px] p-6 shadow-2xl">
        <h2
          className="mb-5"
          style={{ fontSize: "1.1rem" }}
        >
          Rename chat
        </h2>

        <input
          ref={inputRef}
          value={value}
          onChange={(e) => setValue(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter") handleSave();
            if (e.key === "Escape") onClose();
          }}
          className="w-full px-3 py-2.5 bg-accent/50 border border-border rounded-xl text-foreground text-sm placeholder-foreground/30 outline-none focus:border-blue-500/60 focus:ring-1 focus:ring-blue-500/40 transition-all"
        />

        <div className="flex items-center justify-end gap-2.5 mt-5">
          <button
            onClick={onClose}
            className="px-4 py-1.5 rounded-lg text-sm text-foreground/70 hover:text-foreground hover:bg-accent transition-colors"
          >
            Cancel
          </button>
          <button
            onClick={handleSave}
            disabled={!value.trim()}
            className="px-4 py-1.5 rounded-lg text-sm bg-foreground text-background hover:opacity-90 transition-opacity disabled:opacity-40"
          >
            Save
          </button>
        </div>
      </div>
    </div>
  );
}
