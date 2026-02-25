import { useState, useRef, useEffect, useCallback } from "react";
import { Check, ChevronDown, ChevronRight } from "lucide-react";

interface Model {
  name: string;
  description: string;
}

const mainModels: Model[] = [
  { name: "Opus 4.6", description: "Most capable for ambitious work" },
  { name: "Sonnet 4.6", description: "Most efficient for everyday tasks" },
  { name: "Haiku 4.5", description: "Fastest for quick answers" },
];

const moreModels: string[] = [
  "Opus 4.5",
  "Opus 3",
  "Sonnet 4.5",
  "Haiku 3.5",
];

interface ModelSelectorProps {
  selectedModel: string;
  extendedThinking: boolean;
  onModelChange: (model: string) => void;
  onExtendedThinkingChange: (enabled: boolean) => void;
}

export function ModelSelector({
  selectedModel,
  extendedThinking,
  onModelChange,
  onExtendedThinkingChange,
}: ModelSelectorProps) {
  const [isOpen, setIsOpen] = useState(false);
  const [showMoreModels, setShowMoreModels] = useState(false);
  const dropdownRef = useRef<HTMLDivElement>(null);

  const closeMenu = useCallback(() => {
    setIsOpen(false);
    setShowMoreModels(false);
  }, []);

  useEffect(() => {
    function handleClickOutside(event: MouseEvent) {
      if (
        dropdownRef.current &&
        !dropdownRef.current.contains(event.target as Node)
      ) {
        closeMenu();
      }
    }
    function handleEscape(e: KeyboardEvent) {
      if (e.key === "Escape") closeMenu();
    }
    if (isOpen) {
      document.addEventListener("mousedown", handleClickOutside);
      document.addEventListener("keydown", handleEscape);
    }
    return () => {
      document.removeEventListener("mousedown", handleClickOutside);
      document.removeEventListener("keydown", handleEscape);
    };
  }, [isOpen, closeMenu]);

  return (
    <div className="relative" ref={dropdownRef}>
      <button
        onClick={() => {
          setIsOpen(!isOpen);
          setShowMoreModels(false);
        }}
        className="flex items-center gap-1.5 text-sm text-foreground/60 hover:text-foreground transition-colors"
      >
        <span>{selectedModel}</span>
        {extendedThinking && (
          <span className="text-foreground/40">Extended</span>
        )}
        <ChevronDown size={14} />
      </button>

      {isOpen && (
        <div className="absolute top-full mt-2 right-0 w-[260px] bg-popover border border-border rounded-xl shadow-xl z-50">
          {/* Main models */}
          <div className="p-1.5">
            {mainModels.map((model) => (
              <button
                key={model.name}
                onClick={() => {
                  onModelChange(model.name);
                  setIsOpen(false);
                  setShowMoreModels(false);
                }}
                onMouseEnter={() => setShowMoreModels(false)}
                className="flex items-center justify-between w-full px-3 py-2.5 rounded-lg hover:bg-accent transition-colors text-left"
              >
                <div>
                  <p className="text-sm text-foreground">{model.name}</p>
                  <p className="text-xs text-muted-foreground">
                    {model.description}
                  </p>
                </div>
                {selectedModel === model.name && (
                  <Check
                    size={16}
                    className="text-foreground/70 flex-shrink-0"
                  />
                )}
              </button>
            ))}
          </div>

          <div className="border-t border-border mx-1.5" />

          {/* Extended thinking */}
          <div className="p-1.5">
            <div
              className="flex items-center justify-between px-3 py-2.5 rounded-lg hover:bg-accent transition-colors cursor-pointer"
              onClick={() => onExtendedThinkingChange(!extendedThinking)}
              onMouseEnter={() => setShowMoreModels(false)}
            >
              <div>
                <p className="text-sm text-foreground">Extended thinking</p>
                <p className="text-xs text-muted-foreground">
                  Think longer for complex tasks
                </p>
              </div>
              <div
                className={`w-[38px] h-[22px] rounded-full transition-colors relative flex-shrink-0 ${
                  extendedThinking ? "bg-blue-500" : "bg-foreground/20"
                }`}
              >
                <span
                  className={`absolute top-[3px] w-[16px] h-[16px] rounded-full bg-white transition-all duration-200 ${
                    extendedThinking ? "left-[19px]" : "left-[3px]"
                  }`}
                />
              </div>
            </div>
          </div>

          <div className="border-t border-border mx-1.5" />

          {/* More models */}
          <div className="p-1.5">
            <div
              className="relative"
              onMouseEnter={() => setShowMoreModels(true)}
            >
              <button
                onClick={() => setShowMoreModels(!showMoreModels)}
                className={`flex items-center justify-between w-full px-3 py-2.5 rounded-lg transition-colors ${
                  showMoreModels ? "bg-accent" : "hover:bg-accent"
                }`}
              >
                <span className="text-sm text-foreground">More models</span>
                <ChevronRight size={16} className="text-foreground/50" />
              </button>

              {showMoreModels && (
                <div className="absolute left-full top-0 ml-1 w-[180px] bg-popover border border-border rounded-xl shadow-xl z-50">
                  <div className="p-1.5">
                    {moreModels.map((model) => (
                      <button
                        key={model}
                        onClick={() => {
                          onModelChange(model);
                          closeMenu();
                        }}
                        className="flex items-center justify-between w-full text-left px-3 py-2 rounded-lg hover:bg-accent text-sm text-foreground transition-colors"
                      >
                        <span>{model}</span>
                        {selectedModel === model && (
                          <Check
                            size={14}
                            className="text-foreground/70 flex-shrink-0"
                          />
                        )}
                      </button>
                    ))}
                  </div>
                </div>
              )}
            </div>
          </div>
        </div>
      )}
    </div>
  );
}
