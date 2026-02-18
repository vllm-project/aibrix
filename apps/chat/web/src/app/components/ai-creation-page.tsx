import { useState, useRef, useCallback } from "react";
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

type CreationMode = "image" | "video";

interface UploadedFile {
  id: string;
  name: string;
  url: string;
  type: string;
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

const imageModels = [
  { name: "Flux Pro", description: "High quality image generation" },
  { name: "SDXL 1.0", description: "Fast and versatile" },
  { name: "DALL-E 3", description: "Creative and detailed" },
];

const videoModels = [
  { name: "Seedance 2.0 Fast", description: "Fast video generation" },
  { name: "Seedance 2.0", description: "High quality video" },
  { name: "Kling 1.5", description: "Realistic motion" },
];

export function AICreationPage() {
  const [mode, setMode] = useState<CreationMode>("image");
  const [prompt, setPrompt] = useState("");
  const [uploadedFiles, setUploadedFiles] = useState<UploadedFile[]>([]);
  const [referenceImages, setReferenceImages] = useState<UploadedFile[]>([]);
  const [selectedModel, setSelectedModel] = useState("Flux Pro");
  const [selectedRatio, setSelectedRatio] = useState("1:1");
  const [showModelDropdown, setShowModelDropdown] = useState(false);
  const [showRatioDropdown, setShowRatioDropdown] = useState(false);
  const [isGenerating, setIsGenerating] = useState(false);
  const [generatedResults, setGeneratedResults] = useState<string[]>([]);

  const fileInputRef = useRef<HTMLInputElement>(null);
  const refImageInputRef = useRef<HTMLInputElement>(null);
  const modelDropdownRef = useRef<HTMLDivElement>(null);
  const ratioDropdownRef = useRef<HTMLDivElement>(null);

  const models = mode === "image" ? imageModels : videoModels;

  // Switch model default when mode changes
  const handleModeChange = (newMode: CreationMode) => {
    setMode(newMode);
    if (newMode === "image") {
      setSelectedModel("Flux Pro");
    } else {
      setSelectedModel("Seedance 2.0 Fast");
    }
  };

  const handleFileUpload = useCallback(
    (e: React.ChangeEvent<HTMLInputElement>, isReference = false) => {
      const files = e.target.files;
      if (!files) return;

      const newFiles: UploadedFile[] = Array.from(files).map((file) => ({
        id: Date.now().toString() + Math.random().toString(36).slice(2),
        name: file.name,
        url: URL.createObjectURL(file),
        type: file.type,
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

  const handleGenerate = () => {
    if (!prompt.trim() && uploadedFiles.length === 0) return;
    setIsGenerating(true);
    // Simulate generation
    setTimeout(() => {
      setGeneratedResults((prev) => [
        ...prev,
        `Generated ${mode} for: "${prompt || "uploaded image"}" | Model: ${selectedModel} | Ratio: ${selectedRatio}`,
      ]);
      setIsGenerating(false);
    }, 2000);
  };

  const canGenerate =
    (prompt.trim().length > 0 || uploadedFiles.length > 0) && !isGenerating;

  return (
    <div className="flex-1 flex flex-col h-full overflow-y-auto">
      {/* Header area */}
      <div className="flex-1 flex flex-col items-center justify-center px-4 pb-10 min-h-0">
        <div className="flex flex-col items-center mb-10">
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
          <p className="text-sm text-muted-foreground">
            Let creation come with inspiration
          </p>
        </div>

        {/* Creation Input */}
        <div className="w-full max-w-[680px] mx-auto">
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
                            <Video size={20} className="text-foreground/50" />
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
                    <ImageIcon
                      size={20}
                      className="text-foreground/30"
                    />
                  </button>
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

                {/* Separator */}
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

                {/* Separator */}
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
                    <div className="absolute top-full mt-1 left-0 w-[220px] bg-popover border border-border rounded-xl shadow-xl z-50">
                      <div className="p-1.5">
                        {models.map((model) => (
                          <button
                            key={model.name}
                            onClick={() => {
                              setSelectedModel(model.name);
                              setShowModelDropdown(false);
                            }}
                            className="flex items-center justify-between w-full px-3 py-2 rounded-lg hover:bg-accent transition-colors text-left"
                          >
                            <div>
                              <p className="text-sm text-foreground">
                                {model.name}
                              </p>
                              <p className="text-xs text-muted-foreground">
                                {model.description}
                              </p>
                            </div>
                            {selectedModel === model.name && (
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
                    <div className="absolute top-full mt-1 left-0 w-[140px] bg-popover border border-border rounded-xl shadow-xl z-50">
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

          {/* Reference images preview */}
          {referenceImages.length > 0 && (
            <div className="mt-3 px-1">
              <p className="text-xs text-muted-foreground mb-2">
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

          {/* Upload area when no files */}
          {uploadedFiles.length === 0 && (
            <div className="mt-4">
              <button
                onClick={() => fileInputRef.current?.click()}
                className="w-full border border-dashed border-foreground/15 hover:border-foreground/30 rounded-xl py-8 flex flex-col items-center gap-2 transition-colors group"
              >
                <div className="w-10 h-10 rounded-full bg-accent flex items-center justify-center group-hover:bg-accent/80 transition-colors">
                  <ImageIcon
                    size={20}
                    className="text-foreground/50"
                  />
                </div>
                <p className="text-sm text-foreground/50 group-hover:text-foreground/70 transition-colors">
                  Upload images for{" "}
                  {mode === "image" ? "editing" : "video generation"}
                </p>
                <p className="text-xs text-muted-foreground">
                  Drag & drop or click to browse
                </p>
              </button>
            </div>
          )}

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

      {/* Generated results */}
      {(generatedResults.length > 0 || isGenerating) && (
        <div className="px-4 pb-8">
          <div className="max-w-[680px] mx-auto space-y-4">
            {generatedResults.map((result, idx) => (
              <div
                key={idx}
                className="bg-card border border-border rounded-xl p-4"
              >
                <div className="flex items-center gap-2 mb-3">
                  <Sparkles size={14} className="text-amber-400/80" />
                  <span className="text-xs text-muted-foreground">
                    AI Creation
                  </span>
                </div>
                {/* Placeholder for generated content */}
                <div className="w-full aspect-square max-w-[320px] rounded-lg bg-accent/50 border border-border flex items-center justify-center">
                  <div className="text-center px-6">
                    <ImageIcon
                      size={32}
                      className="text-foreground/20 mx-auto mb-2"
                    />
                    <p className="text-xs text-muted-foreground">{result}</p>
                  </div>
                </div>
              </div>
            ))}

            {isGenerating && (
              <div className="bg-card border border-border rounded-xl p-4">
                <div className="flex items-center gap-2 mb-3">
                  <Sparkles size={14} className="text-amber-400/80 animate-pulse" />
                  <span className="text-xs text-muted-foreground">
                    Generating...
                  </span>
                </div>
                <div className="w-full aspect-square max-w-[320px] rounded-lg bg-accent/50 border border-border flex items-center justify-center animate-pulse">
                  <div className="text-center px-6">
                    <div className="w-8 h-8 rounded-full border-2 border-foreground/20 border-t-amber-400/80 animate-spin mx-auto mb-2" />
                    <p className="text-xs text-muted-foreground">
                      Creating your {mode}...
                    </p>
                  </div>
                </div>
              </div>
            )}
          </div>
        </div>
      )}
    </div>
  );
}
