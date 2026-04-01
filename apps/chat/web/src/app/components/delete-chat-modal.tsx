interface DeleteChatModalProps {
  isOpen: boolean;
  onClose: () => void;
  onDelete: () => void;
}

export function DeleteChatModal({
  isOpen,
  onClose,
  onDelete,
}: DeleteChatModalProps) {
  if (!isOpen) return null;

  return (
    <div className="fixed inset-0 z-[200] flex items-center justify-center">
      <div className="absolute inset-0 bg-black/60" onClick={onClose} />
      <div className="relative bg-card border border-border rounded-2xl w-full max-w-[360px] p-6 shadow-2xl">
        <h2 style={{ fontSize: "1.1rem" }} className="mb-2">
          Delete chat
        </h2>
        <p className="text-sm text-foreground/50">
          Are you sure you want to delete this chat?
        </p>

        <div className="flex items-center justify-end gap-2.5 mt-6">
          <button
            onClick={onClose}
            className="px-4 py-1.5 rounded-lg text-sm text-foreground/70 hover:text-foreground hover:bg-accent transition-colors"
          >
            Cancel
          </button>
          <button
            onClick={() => {
              onDelete();
              onClose();
            }}
            className="px-4 py-1.5 rounded-lg text-sm bg-red-500 text-white hover:bg-red-600 transition-colors"
          >
            Delete
          </button>
        </div>
      </div>
    </div>
  );
}
