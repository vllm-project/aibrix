import { useState, useRef, useCallback } from "react";
import { AudioLines, ArrowUp, X, Loader2 } from "lucide-react";
import { ModelSelector } from "./model-selector";
import { PlusMenu } from "./plus-menu";
import { Tooltip } from "./tooltip";

interface AttachedImage {
  id: string;
  dataUrl: string;
  file: File;
  uploading: boolean;
  progress: number;
}

interface ChatInputProps {
  placeholder?: string;
  onSend?: (message: string, images?: AttachedImage[]) => void;
  onStartNewProject?: () => void;
}

export function ChatInput({
  placeholder = "How can I help you today?",
  onSend,
  onStartNewProject,
}: ChatInputProps) {
  const [message, setMessage] = useState("");
  const [selectedModel, setSelectedModel] = useState("Sonnet 4.6");
  const [extendedThinking, setExtendedThinking] = useState(true);
  const [images, setImages] = useState<AttachedImage[]>([]);
  const [isDragOver, setIsDragOver] = useState(false);
  const textareaRef = useRef<HTMLTextAreaElement>(null);

  const hasContent = message.trim().length > 0 || images.some((i) => !i.uploading);

  const simulateUpload = useCallback((img: AttachedImage) => {
    const duration = 800 + Math.random() * 600;
    const steps = 12;
    const stepTime = duration / steps;
    let step = 0;

    const interval = setInterval(() => {
      step++;
      const progress = Math.min((step / steps) * 100, 100);
      setImages((prev) =>
        prev.map((i) =>
          i.id === img.id ? { ...i, progress } : i
        )
      );
      if (step >= steps) {
        clearInterval(interval);
        setImages((prev) =>
          prev.map((i) =>
            i.id === img.id ? { ...i, uploading: false, progress: 100 } : i
          )
        );
      }
    }, stepTime);
  }, []);

  const addImageFiles = useCallback(
    (files: File[]) => {
      const imageFiles = files.filter((f) => f.type.startsWith("image/"));
      imageFiles.forEach((file) => {
        const reader = new FileReader();
        reader.onload = (e) => {
          const dataUrl = e.target?.result as string;
          const newImg: AttachedImage = {
            id: crypto.randomUUID(),
            dataUrl,
            file,
            uploading: true,
            progress: 0,
          };
          setImages((prev) => [...prev, newImg]);
          simulateUpload(newImg);
        };
        reader.readAsDataURL(file);
      });
    },
    [simulateUpload]
  );

  const handlePaste = useCallback(
    (e: React.ClipboardEvent) => {
      const items = Array.from(e.clipboardData.items);
      const imageItems = items.filter((item) => item.type.startsWith("image/"));
      if (imageItems.length > 0) {
        e.preventDefault();
        const files = imageItems
          .map((item) => item.getAsFile())
          .filter(Boolean) as File[];
        addImageFiles(files);
      }
    },
    [addImageFiles]
  );

  const handleDrop = useCallback(
    (e: React.DragEvent) => {
      e.preventDefault();
      setIsDragOver(false);
      const files = Array.from(e.dataTransfer.files);
      addImageFiles(files);
    },
    [addImageFiles]
  );

  const handleDragOver = useCallback((e: React.DragEvent) => {
    e.preventDefault();
    setIsDragOver(true);
  }, []);

  const handleDragLeave = useCallback((e: React.DragEvent) => {
    e.preventDefault();
    setIsDragOver(false);
  }, []);

  const removeImage = (id: string) => {
    setImages((prev) => prev.filter((i) => i.id !== id));
  };

  const handleSubmit = () => {
    if (!hasContent) return;
    onSend?.(message, images.filter((i) => !i.uploading));
    setMessage("");
    setImages([]);
  };

  const handleKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === "Enter" && !e.shiftKey) {
      e.preventDefault();
      handleSubmit();
    }
  };

  return (
    <div className="w-full max-w-[680px] mx-auto">
      <div
        className={`bg-card border rounded-2xl transition-colors ${
          isDragOver ? "border-blue-500/50 bg-blue-500/5" : "border-border"
        }`}
        onDrop={handleDrop}
        onDragOver={handleDragOver}
        onDragLeave={handleDragLeave}
      >
        {/* Image previews */}
        {images.length > 0 && (
          <div className="flex flex-wrap gap-2 px-4 pt-3">
            {images.map((img) => (
              <div key={img.id} className="relative group/img">
                <div className="w-[120px] h-[90px] rounded-xl overflow-hidden border border-border bg-accent/50">
                  {img.uploading ? (
                    <div className="w-full h-full flex flex-col items-center justify-center gap-2">
                      <Loader2
                        size={20}
                        className="text-foreground/40 animate-spin"
                      />
                      <div className="w-16 h-1 rounded-full bg-foreground/10 overflow-hidden">
                        <div
                          className="h-full bg-foreground/40 rounded-full transition-all duration-150"
                          style={{ width: `${img.progress}%` }}
                        />
                      </div>
                    </div>
                  ) : (
                    <img
                      src={img.dataUrl}
                      alt="Attached"
                      className="w-full h-full object-cover"
                    />
                  )}
                </div>
                {/* Remove button */}
                {!img.uploading && (
                  <button
                    onClick={() => removeImage(img.id)}
                    className="absolute -top-1.5 -right-1.5 w-5 h-5 rounded-full bg-foreground/80 text-background flex items-center justify-center opacity-0 group-hover/img:opacity-100 transition-opacity"
                  >
                    <X size={12} />
                  </button>
                )}
              </div>
            ))}
          </div>
        )}

        <div className="px-4 pt-4 pb-2">
          <textarea
            ref={textareaRef}
            value={message}
            onChange={(e) => setMessage(e.target.value)}
            onKeyDown={handleKeyDown}
            onPaste={handlePaste}
            placeholder={placeholder}
            rows={1}
            className="w-full bg-transparent text-foreground placeholder-foreground/30 resize-none outline-none text-[15px]"
            style={{ minHeight: "24px", maxHeight: "200px" }}
            onInput={(e) => {
              const target = e.target as HTMLTextAreaElement;
              target.style.height = "auto";
              target.style.height = Math.min(target.scrollHeight, 200) + "px";
            }}
          />
        </div>
        <div className="flex items-center justify-between px-3 pb-3">
          <PlusMenu onStartNewProject={onStartNewProject} />
          <div className="flex items-center gap-3">
            <ModelSelector
              selectedModel={selectedModel}
              extendedThinking={extendedThinking}
              onModelChange={setSelectedModel}
              onExtendedThinkingChange={setExtendedThinking}
            />
            {hasContent ? (
              <button
                onClick={handleSubmit}
                className="w-8 h-8 rounded-full bg-amber-700 hover:bg-amber-600 text-white flex items-center justify-center transition-colors"
              >
                <ArrowUp size={18} />
              </button>
            ) : (
              <Tooltip content="Use voice mode">
                <button className="p-1.5 rounded-lg hover:bg-accent text-foreground/50 hover:text-foreground transition-colors">
                  <AudioLines size={20} />
                </button>
              </Tooltip>
            )}
          </div>
        </div>
      </div>
    </div>
  );
}
