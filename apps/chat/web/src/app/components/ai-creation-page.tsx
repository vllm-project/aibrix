import { useState, useRef, useCallback, useEffect } from "react";
import {
  Image as ImageIcon,
  Video,
  Paperclip,
  ChevronDown,
  Check,
  X,
  ArrowUp,
  RatioIcon,
  Sparkles,
} from "lucide-react";
import { useNavigate } from "react-router";
import {
  fetchModels,
  createConversation,
  notifyConversationsChanged,
  type ModelInfo,
} from "../../api/client";
import { pendingCreation } from "../creation-store";

type CreationMode = "image" | "video";

interface UploadedFile {
  id: string;
  name: string;
  url: string;
  type: string;
  rawFile?: File;
}

interface AspectRatio {
  label: string;
  value: string;
  icon: string;
}

const aspectRatios: AspectRatio[] = [
  { label: "1:1", value: "1:1", icon: "⬜" },
  { label: "16:9", value: "16:9", icon: "▬" },
  { label: "9:16", value: "9:16", icon: "▯" },
  { label: "4:3", value: "4:3", icon: "⬜" },
  { label: "3:4", value: "3:4", icon: "▯" },
];

// Fallbacks when no models are returned from the API
const FALLBACK_IMAGE_MODELS: ModelInfo[] = [
  { id: "gpt-image-1", name: "gpt-image-1", capabilities: ["image"], owned_by: null },
  { id: "dall-e-3", name: "dall-e-3", capabilities: ["image"], owned_by: null },
  { id: "dall-e-2", name: "dall-e-2", capabilities: ["image"], owned_by: null },
];

const FALLBACK_VIDEO_MODELS: ModelInfo[] = [
  { id: "sora-2", name: "sora-2", capabilities: ["video"], owned_by: null },
];

export function AICreationPage() {
  const navigate = useNavigate();
  const [mode, setMode] = useState<CreationMode>("image");
  const [prompt, setPrompt] = useState("");
  const [uploadedFiles, setUploadedFiles] = useState<UploadedFile[]>([]);
  const [referenceImages, setReferenceImages] = useState<UploadedFile[]>([]);
  const [selectedModel, setSelectedModel] = useState("gpt-image-1");
  const [selectedRatio, setSelectedRatio] = useState("1:1");
  const [showModelDropdown, setShowModelDropdown] = useState(false);
  const [showRatioDropdown, setShowRatioDropdown] = useState(false);
  const [imageModels, setImageModels] = useState<ModelInfo[]>([]);
  const [videoModels, setVideoModels] = useState<ModelInfo[]>([]);
  const [generating, setGenerating] = useState(false);

  const fileInputRef = useRef<HTMLInputElement>(null);
  const refImageInputRef = useRef<HTMLInputElement>(null);
  const modelDropdownRef = useRef<HTMLDivElement>(null);
  const ratioDropdownRef = useRef<HTMLDivElement>(null);

  // Fetch image and video models from API
  useEffect(() => {
    let cancelled = false;
    Promise.all([fetchModels("image"), fetchModels("video")]).then(
      ([img, vid]) => {
        if (cancelled) return;
        setImageModels(img.length > 0 ? img : FALLBACK_IMAGE_MODELS);
        setVideoModels(vid.length > 0 ? vid : FALLBACK_VIDEO_MODELS);
        if (img.length > 0) setSelectedModel(img[0].id);
      }
    );
    return () => { cancelled = true; };
  }, []);

  const models = mode === "image" ? imageModels : videoModels;

  // Close dropdowns on outside click
  useEffect(() => {
    if (!showModelDropdown && !showRatioDropdown) return;

    function handleClickOutside(event: MouseEvent) {
      if (
        modelDropdownRef.current &&
        !modelDropdownRef.current.contains(event.target as Node)
      ) {
        setShowModelDropdown(false);
      }
      if (
        ratioDropdownRef.current &&
        !ratioDropdownRef.current.contains(event.target as Node)
      ) {
        setShowRatioDropdown(false);
      }
    }

    document.addEventListener("mousedown", handleClickOutside);
    return () => document.removeEventListener("mousedown", handleClickOutside);
  }, [showModelDropdown, showRatioDropdown]);

  // Switch model default when mode changes
  const handleModeChange = (newMode: CreationMode) => {
    setMode(newMode);
    const list = newMode === "image" ? imageModels : videoModels;
    const fallback = newMode === "image" ? FALLBACK_IMAGE_MODELS : FALLBACK_VIDEO_MODELS;
    const effectiveList = list.length > 0 ? list : fallback;
    if (!effectiveList.some((m) => m.id === selectedModel)) {
      setSelectedModel(effectiveList[0]?.id ?? "");
    }
  };

  const handleFileUpload = useCallback(
    (e: React.ChangeEvent<HTMLInputElement>, isReference = false) => {
      const files = e.target.files;
      if (!files) return;

      const newFiles: UploadedFile[] = Array.from(files).map((file) => ({
        id: crypto.randomUUID(),
        name: file.name,
        url: URL.createObjectURL(file),
        type: file.type,
        rawFile: file,
      }));

      if (isReference) {
        setReferenceImages((prev) => [...prev, ...newFiles]);
      } else {
        setUploadedFiles((prev) => [...prev, ...newFiles]);
      }
      e.target.value = "";
    },
    []
  );

  const removeFile = (id: string, isReference = false) => {
    if (isReference) {
      setReferenceImages((prev) => {
        const file = prev.find((f) => f.id === id);
        if (file) URL.revokeObjectURL(file.url);
        return prev.filter((f) => f.id !== id);
      });
    } else {
      setUploadedFiles((prev) => {
        const file = prev.find((f) => f.id === id);
        if (file) URL.revokeObjectURL(file.url);
        return prev.filter((f) => f.id !== id);
      });
    }
  };

  const handleGenerate = async () => {
    if (!prompt.trim() && uploadedFiles.length === 0) return;
    if (generating) return;

    setGenerating(true);
    try {
      const title = prompt.trim().slice(0, 40) + (prompt.trim().length > 40 ? "..." : "");
      const conv = await createConversation(selectedModel, title || "AI Creation");
      notifyConversationsChanged();

      // Store non-serializable data (File, blob URL) in module-level ref
      const refImage = referenceImages[0] || uploadedFiles[0];
      pendingCreation.file = (mode === "image" && refImage?.rawFile) ? refImage.rawFile : undefined;
      pendingCreation.blobUrl = (mode === "image" && refImage?.url) ? refImage.url : undefined;

      navigate(`/chat/${conv.id}`, {
        state: {
          firstMessage: prompt,
          model: selectedModel,
          mode,
          ratio: selectedRatio,
        },
      });
    } catch (err) {
      console.error("Failed to create conversation:", err);
      setGenerating(false);
    }
  };

  const canGenerate =
    (prompt.trim().length > 0 || uploadedFiles.length > 0) && !generating;

  return (
    <div className="flex-1 flex flex-col h-full">
      {/* Empty state: welcome + input centered together */}
      <div className="flex-1 flex flex-col items-center justify-center px-4 pb-20">
        <div className="flex items-center gap-2.5 mb-3">
          <Sparkles size={28} className="text-amber-400/80" />
          <h1
            style={{
              fontFamily: "'Playfair Display', serif",
              fontSize: "2rem",
              fontWeight: 400,
              color: "var(--foreground)",
              opacity: 0.85,
            }}
          >
            AI Creation
          </h1>
        </div>
        <p className="text-sm text-muted-foreground mb-8">
          Let creation come with inspiration
        </p>

        {/* Input card */}
        <div className="max-w-[680px] mx-auto w-full">
          <div className="bg-card border border-border rounded-2xl">
            {/* Uploaded files preview */}
            {uploadedFiles.length > 0 && (
              <div className="px-4 pt-4 pb-2">
                <div className="flex gap-2 flex-wrap">
                  {uploadedFiles.map((file) => (
                    <div key={file.id} className="relative group">
                      <div className="w-20 h-20 rounded-lg overflow-hidden border border-border">
                        {file.type.startsWith("image/") ? (
                          <img
                            src={file.url}
                            alt={file.name}
                            className="w-full h-full object-cover"
                          />
                        ) : (
                          <div className="w-full h-full bg-accent flex items-center justify-center">
                            <Video
                              size={20}
                              className="text-foreground/50"
                            />
                          </div>
                        )}
                      </div>
                      <button
                        onClick={() => removeFile(file.id)}
                        className="absolute -top-1.5 -right-1.5 w-5 h-5 rounded-full bg-foreground/80 text-background flex items-center justify-center opacity-0 group-hover:opacity-100 transition-opacity"
                      >
                        <X size={12} />
                      </button>
                    </div>
                  ))}
                  <button
                    onClick={() => fileInputRef.current?.click()}
                    className="w-20 h-20 rounded-lg border border-dashed border-foreground/20 hover:border-foreground/40 flex items-center justify-center transition-colors"
                  >
                    <ImageIcon size={20} className="text-foreground/30" />
                  </button>
                </div>
              </div>
            )}

            {/* Reference images preview */}
            {referenceImages.length > 0 && (
              <div className="px-4 pt-3 pb-1">
                <p className="text-xs text-muted-foreground mb-1.5">
                  Reference images
                </p>
                <div className="flex gap-2 flex-wrap">
                  {referenceImages.map((file) => (
                    <div key={file.id} className="relative group">
                      <div className="w-16 h-16 rounded-lg overflow-hidden border border-border">
                        <img
                          src={file.url}
                          alt={file.name}
                          className="w-full h-full object-cover"
                        />
                      </div>
                      <button
                        onClick={() => removeFile(file.id, true)}
                        className="absolute -top-1.5 -right-1.5 w-5 h-5 rounded-full bg-foreground/80 text-background flex items-center justify-center opacity-0 group-hover:opacity-100 transition-opacity"
                      >
                        <X size={12} />
                      </button>
                    </div>
                  ))}
                </div>
              </div>
            )}

            {/* Text input */}
            <div className="px-4 pt-4 pb-2">
              <textarea
                value={prompt}
                onChange={(e) => setPrompt(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === "Enter" && !e.shiftKey) {
                    e.preventDefault();
                    if (canGenerate) handleGenerate();
                  }
                }}
                placeholder={
                  mode === "image"
                    ? "Describe the image you want to create..."
                    : "Add photos, describe the video you want to generate..."
                }
                rows={3}
                className="w-full bg-transparent text-foreground placeholder-foreground/30 resize-none outline-none text-[15px]"
                style={{ minHeight: "72px", maxHeight: "200px" }}
                onInput={(e) => {
                  const target = e.target as HTMLTextAreaElement;
                  target.style.height = "auto";
                  target.style.height =
                    Math.min(target.scrollHeight, 200) + "px";
                }}
              />
            </div>

            {/* Toolbar */}
            <div className="flex items-center justify-between px-3 pb-3">
              <div className="flex items-center gap-1">
                {/* Image / Video toggle */}
                <div className="flex items-center bg-accent/60 rounded-lg p-0.5">
                  <button
                    onClick={() => handleModeChange("image")}
                    className={`flex items-center gap-1.5 px-2.5 py-1 rounded-md text-sm transition-colors ${
                      mode === "image"
                        ? "bg-card text-foreground shadow-sm"
                        : "text-foreground/50 hover:text-foreground/70"
                    }`}
                  >
                    <ImageIcon size={14} />
                    Image
                  </button>
                  <button
                    onClick={() => handleModeChange("video")}
                    className={`flex items-center gap-1.5 px-2.5 py-1 rounded-md text-sm transition-colors ${
                      mode === "video"
                        ? "bg-card text-foreground shadow-sm"
                        : "text-foreground/50 hover:text-foreground/70"
                    }`}
                  >
                    <Video size={14} />
                    Video
                  </button>
                </div>

                <div className="w-px h-5 bg-border mx-1" />

                {/* Reference Image */}
                <button
                  onClick={() => refImageInputRef.current?.click()}
                  className={`flex items-center gap-1.5 px-2.5 py-1 rounded-lg text-sm transition-colors ${
                    referenceImages.length > 0
                      ? "text-amber-400 bg-amber-400/10"
                      : "text-foreground/50 hover:text-foreground hover:bg-accent"
                  }`}
                >
                  <Paperclip size={14} />
                  <span>Ref image</span>
                  {referenceImages.length > 0 && (
                    <span className="text-xs bg-amber-400/20 px-1.5 rounded-full">
                      {referenceImages.length}
                    </span>
                  )}
                </button>

                <div className="w-px h-5 bg-border mx-1" />

                {/* Model selector */}
                <div className="relative" ref={modelDropdownRef}>
                  <button
                    onClick={() => {
                      setShowModelDropdown(!showModelDropdown);
                      setShowRatioDropdown(false);
                    }}
                    className="flex items-center gap-1.5 px-2.5 py-1 rounded-lg text-sm text-foreground/50 hover:text-foreground hover:bg-accent transition-colors"
                  >
                    <Sparkles size={14} />
                    <span>{selectedModel}</span>
                    <ChevronDown size={12} />
                  </button>

                  {showModelDropdown && (
                    <div className="absolute bottom-full mb-1 left-0 w-[220px] bg-popover border border-border rounded-xl shadow-xl z-50">
                      <div className="p-1.5 max-h-[320px] overflow-y-auto">
                        {models.map((model) => (
                          <button
                            key={model.id}
                            onClick={() => {
                              setSelectedModel(model.id);
                              setShowModelDropdown(false);
                            }}
                            className="flex items-center justify-between w-full px-3 py-2 rounded-lg hover:bg-accent transition-colors text-left"
                          >
                            <div>
                              <p className="text-sm text-foreground">
                                {model.id}
                              </p>
                              {model.owned_by && (
                                <p className="text-xs text-muted-foreground">
                                  {model.owned_by}
                                </p>
                              )}
                            </div>
                            {selectedModel === model.id && (
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

                {/* Aspect Ratio */}
                <div className="relative" ref={ratioDropdownRef}>
                  <button
                    onClick={() => {
                      setShowRatioDropdown(!showRatioDropdown);
                      setShowModelDropdown(false);
                    }}
                    className="flex items-center gap-1.5 px-2.5 py-1 rounded-lg text-sm text-foreground/50 hover:text-foreground hover:bg-accent transition-colors"
                  >
                    <RatioIcon size={14} />
                    <span>{selectedRatio}</span>
                    <ChevronDown size={12} />
                  </button>

                  {showRatioDropdown && (
                    <div className="absolute bottom-full mb-1 left-0 w-[140px] bg-popover border border-border rounded-xl shadow-xl z-50">
                      <div className="p-1.5">
                        {aspectRatios.map((ratio) => (
                          <button
                            key={ratio.value}
                            onClick={() => {
                              setSelectedRatio(ratio.value);
                              setShowRatioDropdown(false);
                            }}
                            className="flex items-center justify-between w-full px-3 py-1.5 rounded-lg hover:bg-accent transition-colors text-left"
                          >
                            <span className="text-sm text-foreground">
                              {ratio.label}
                            </span>
                            {selectedRatio === ratio.value && (
                              <Check
                                size={14}
                                className="text-foreground/70"
                              />
                            )}
                          </button>
                        ))}
                      </div>
                    </div>
                  )}
                </div>
              </div>

              {/* Generate button */}
              <button
                onClick={handleGenerate}
                disabled={!canGenerate}
                className={`p-2 rounded-full transition-colors ${
                  canGenerate
                    ? "bg-foreground text-background hover:bg-foreground/90"
                    : "bg-foreground/15 text-foreground/30 cursor-not-allowed"
                }`}
              >
                <ArrowUp size={16} />
              </button>
            </div>
          </div>

          {/* Hidden file inputs */}
          <input
            ref={fileInputRef}
            type="file"
            accept="image/*"
            multiple
            className="hidden"
            onChange={(e) => handleFileUpload(e, false)}
          />
          <input
            ref={refImageInputRef}
            type="file"
            accept="image/*"
            multiple
            className="hidden"
            onChange={(e) => handleFileUpload(e, true)}
          />
        </div>
      </div>
    </div>
  );
}
