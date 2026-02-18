import { useState, useEffect } from "react";

interface SetInstructionsModalProps {
  isOpen: boolean;
  onClose: () => void;
  onSave: (instructions: string) => void;
  projectName: string;
  initialInstructions: string;
}

export function SetInstructionsModal({
  isOpen,
  onClose,
  onSave,
  projectName,
  initialInstructions,
}: SetInstructionsModalProps) {
  const [instructions, setInstructions] = useState(initialInstructions);

  useEffect(() => {
    setInstructions(initialInstructions);
  }, [initialInstructions]);

  if (!isOpen) return null;

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center">
      <div className="absolute inset-0 bg-black/60" onClick={onClose} />
      <div className="relative bg-card border border-border rounded-2xl w-full max-w-[680px] p-8 shadow-2xl">
        <h2
          className="mb-2"
          style={{ fontSize: "1.25rem", fontWeight: 500 }}
        >
          Set project instructions
        </h2>
        <p className="text-sm text-muted-foreground mb-5">
          Provide relevant instructions and information for chats within {projectName}.
          This will work alongside user preferences and the selected style in a chat.
        </p>

        <textarea
          value={instructions}
          onChange={(e) => setInstructions(e.target.value)}
          rows={12}
          className="w-full px-4 py-3 bg-accent/30 border border-blue-500/40 rounded-xl text-foreground text-sm placeholder-foreground/30 outline-none resize-none focus:border-blue-500/60 transition-colors"
          placeholder="Enter project instructions..."
        />

        <div className="flex items-center justify-end gap-3 mt-5">
          <button
            onClick={onClose}
            className="px-5 py-2 rounded-lg text-sm text-foreground/70 hover:text-foreground hover:bg-accent transition-colors"
          >
            Cancel
          </button>
          <button
            onClick={() => {
              onSave(instructions);
              onClose();
            }}
            className="px-5 py-2 rounded-lg text-sm bg-foreground text-background hover:opacity-90 transition-opacity"
          >
            Save instructions
          </button>
        </div>
      </div>
    </div>
  );
}
