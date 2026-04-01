import { useState } from "react";
import { X } from "lucide-react";

interface CreateProjectModalProps {
  isOpen: boolean;
  onClose: () => void;
  onCreate: (name: string, description: string) => void;
}

export function CreateProjectModal({
  isOpen,
  onClose,
  onCreate,
}: CreateProjectModalProps) {
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");

  if (!isOpen) return null;

  const handleCreate = () => {
    if (name.trim()) {
      onCreate(name, description);
      setName("");
      setDescription("");
      onClose();
    }
  };

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center">
      <div className="absolute inset-0 bg-black/60" onClick={onClose} />
      <div className="relative bg-card border border-border rounded-2xl w-full max-w-[560px] p-8 shadow-2xl">
        <h2
          className="mb-6"
          style={{
            fontFamily: "'Playfair Display', serif",
            fontSize: "1.75rem",
            fontWeight: 400,
            color: "var(--foreground)",
            opacity: 0.85,
          }}
        >
          Create a personal project
        </h2>

        <div className="mb-5">
          <label className="block mb-1.5 text-sm text-foreground/70">
            What are you working on?
          </label>
          <input
            type="text"
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder="Name your project"
            className="w-full px-4 py-3 bg-accent/50 border border-border rounded-xl text-foreground placeholder-foreground/30 outline-none focus:border-foreground/20 transition-colors"
          />
        </div>

        <div className="mb-6">
          <label className="block mb-1.5 text-sm text-foreground/70">
            What are you trying to achieve?
          </label>
          <textarea
            value={description}
            onChange={(e) => setDescription(e.target.value)}
            placeholder="Describe your project, goals, subject, etc..."
            rows={4}
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
            onClick={handleCreate}
            disabled={!name.trim()}
            className="px-5 py-2 rounded-lg text-sm bg-foreground text-background hover:opacity-90 transition-opacity disabled:opacity-40"
          >
            Create project
          </button>
        </div>
      </div>
    </div>
  );
}
