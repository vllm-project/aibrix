import { useState, useRef, useEffect } from "react";
import { useParams, useNavigate } from "react-router";
import {
  ArrowLeft,
  MoreHorizontal,
  Star,
  Pencil,
  Plus,
  Upload,
  FileText,
  Github,
  AudioLines,
} from "lucide-react";
import { ModelSelector } from "./model-selector";
import { PlusMenu } from "./plus-menu";
import { SetInstructionsModal } from "./set-instructions-modal";
import { AddTextContentModal } from "./add-text-content-modal";

interface ProjectFile {
  id: string;
  name: string;
  type: "text" | "file";
}

const projectData: Record<
  string,
  {
    name: string;
    instructions: string;
    files: ProjectFile[];
  }
> = {
  "1": {
    name: "AIBrix",
    instructions:
      'You are working inside the **AIBrix** codebase. Treat this as the project\'s standing engineering instructions for all chats in this repo.\n\n# What AIBrix is (project context)\nAIBrix is an open-source, Kubernetes-native LLM inference infrastructure stack ("bricks for AI Infra") that helps teams turn large models into scalable, cost-efficient APIs on K8s. Key themes:\n- Multi-engine serving (e.g., vLLM / SGLang / TensorRT-LLM) via a consistent control-plane abstraction.\n- Production-grade orchestration via CRDs + controllers (controller-runtime): deployments, rollouts, upgrades, status, and autoscaling.\n- LLM-specific capabilities: high-density LoRA management (load/unload, scheduling, isolation), fast cold start & model loading, routing strategies, and KV-cache offloading.\n- Prefill/Decode (P/D) disaggregation orchestration (e.g., replica mode and pooled mode) with role-aware routing.',
    files: [],
  },
  "2": {
    name: "How to use AIBrix Chat",
    instructions:
      "This is an example project that shows how to use AIBrix Chat effectively. It includes tips on prompting, conversation management, and getting the most out of AI assistants.",
    files: [],
  },
};

export function ProjectDetailPage() {
  const { id } = useParams();
  const navigate = useNavigate();
  const project = id ? projectData[id] : null;

  const [showInstructionsModal, setShowInstructionsModal] = useState(false);
  const [showAddTextModal, setShowAddTextModal] = useState(false);
  const [showFilesMenu, setShowFilesMenu] = useState(false);
  const [instructions, setInstructions] = useState(
    project?.instructions || ""
  );
  const [files, setFiles] = useState<ProjectFile[]>(project?.files || []);
  const [selectedModel, setSelectedModel] = useState("Sonnet 4.6");
  const [extendedThinking, setExtendedThinking] = useState(true);

  const filesMenuRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    function handleClickOutside(event: MouseEvent) {
      if (
        filesMenuRef.current &&
        !filesMenuRef.current.contains(event.target as Node)
      ) {
        setShowFilesMenu(false);
      }
    }
    document.addEventListener("mousedown", handleClickOutside);
    return () => document.removeEventListener("mousedown", handleClickOutside);
  }, []);

  if (!project) {
    return (
      <div className="flex-1 flex items-center justify-center text-muted-foreground">
        Project not found
      </div>
    );
  }

  return (
    <div className="flex-1 flex h-full overflow-hidden">
      {/* Main content */}
      <div className="flex-1 flex flex-col px-8 py-6 overflow-y-auto">
        <button
          onClick={() => navigate("/projects")}
          className="flex items-center gap-1.5 text-sm text-muted-foreground hover:text-foreground transition-colors mb-4 w-fit"
        >
          <ArrowLeft size={14} />
          All projects
        </button>

        <div className="flex items-center gap-3 mb-8">
          <h1 style={{ fontSize: "1.4rem", fontWeight: 500 }}>
            {project.name}
          </h1>
          <button className="p-1 rounded hover:bg-accent text-foreground/40 hover:text-foreground transition-colors">
            <MoreHorizontal size={18} />
          </button>
          <button className="p-1 rounded hover:bg-accent text-foreground/40 hover:text-foreground transition-colors">
            <Star size={18} />
          </button>
        </div>

        {/* Chat input area */}
        <div className="max-w-[620px] mx-auto w-full mb-6">
          <div className="bg-card border border-border rounded-2xl overflow-hidden">
            <div className="px-4 pt-4 pb-2">
              <textarea
                placeholder="Reply..."
                rows={1}
                className="w-full bg-transparent text-foreground placeholder-foreground/30 resize-none outline-none text-[15px]"
                style={{ minHeight: "24px" }}
              />
            </div>
            <div className="flex items-center justify-between px-3 pb-3">
              <PlusMenu />
              <div className="flex items-center gap-3">
                <ModelSelector
                  selectedModel={selectedModel}
                  extendedThinking={extendedThinking}
                  onModelChange={setSelectedModel}
                  onExtendedThinkingChange={setExtendedThinking}
                />
                <button className="p-1.5 rounded-lg hover:bg-accent text-foreground/50 hover:text-foreground transition-colors">
                  <AudioLines size={20} />
                </button>
              </div>
            </div>
          </div>
        </div>

        <div className="max-w-[620px] mx-auto w-full">
          <div className="bg-card/50 border border-border rounded-xl p-4 text-center">
            <p className="text-sm text-muted-foreground">
              Start a chat to keep conversations organized and re-use project
              knowledge.
            </p>
          </div>
        </div>
      </div>

      {/* Right sidebar */}
      <div className="w-[300px] min-w-[300px] border-l border-border p-5 overflow-y-auto">
        {/* Instructions section */}
        <div className="mb-6">
          <div className="flex items-center justify-between mb-2">
            <h3 className="text-sm">Instructions</h3>
            <button
              onClick={() => setShowInstructionsModal(true)}
              className="p-1 rounded hover:bg-accent text-foreground/40 hover:text-foreground transition-colors"
            >
              <Pencil size={14} />
            </button>
          </div>
          <div
            className="bg-card border border-border rounded-xl p-3 cursor-pointer hover:border-foreground/15 transition-colors"
            onClick={() => setShowInstructionsModal(true)}
          >
            <p className="text-xs text-muted-foreground line-clamp-3">
              {instructions || "Click to add project instructions..."}
            </p>
          </div>
        </div>

        {/* Files section */}
        <div>
          <div className="flex items-center justify-between mb-2">
            <h3 className="text-sm">Files</h3>
            <div className="relative" ref={filesMenuRef}>
              <button
                onClick={() => setShowFilesMenu(!showFilesMenu)}
                className="p-1 rounded hover:bg-accent text-foreground/40 hover:text-foreground transition-colors"
              >
                <Plus size={14} />
              </button>

              {showFilesMenu && (
                <div className="absolute right-0 top-full mt-1 w-[200px] bg-popover border border-border rounded-xl shadow-xl p-1.5 z-50">
                  <button
                    onClick={() => setShowFilesMenu(false)}
                    className="flex items-center gap-2.5 w-full px-3 py-2 rounded-lg hover:bg-accent text-sm transition-colors"
                  >
                    <Upload size={14} className="text-foreground/60" />
                    Upload from device
                  </button>
                  <button
                    onClick={() => {
                      setShowFilesMenu(false);
                      setShowAddTextModal(true);
                    }}
                    className="flex items-center gap-2.5 w-full px-3 py-2 rounded-lg hover:bg-accent text-sm transition-colors"
                  >
                    <FileText size={14} className="text-foreground/60" />
                    Add text content
                  </button>
                  <div className="border-t border-border my-1" />
                  <button
                    onClick={() => setShowFilesMenu(false)}
                    className="flex items-center gap-2.5 w-full px-3 py-2 rounded-lg hover:bg-accent text-sm transition-colors"
                  >
                    <Github size={14} className="text-foreground/60" />
                    GitHub
                  </button>
                  <button
                    onClick={() => setShowFilesMenu(false)}
                    className="flex items-center gap-2.5 w-full px-3 py-2 rounded-lg hover:bg-accent text-sm transition-colors"
                  >
                    <span className="text-sm">🔗</span>
                    Google Drive
                  </button>
                </div>
              )}
            </div>
          </div>

          {files.length === 0 ? (
            <div className="bg-card border border-border rounded-xl p-6 flex flex-col items-center justify-center text-center">
              <div className="flex items-center gap-2 mb-3 opacity-30">
                <FileText size={24} />
                <FileText size={20} />
              </div>
              <p className="text-xs text-muted-foreground">
                Add PDFs, documents, or other text to reference in this project.
              </p>
            </div>
          ) : (
            <div className="space-y-2">
              {files.map((file) => (
                <div
                  key={file.id}
                  className="bg-card border border-border rounded-lg p-3 flex items-center gap-2"
                >
                  <FileText size={14} className="text-foreground/60" />
                  <span className="text-sm truncate">{file.name}</span>
                </div>
              ))}
            </div>
          )}
        </div>
      </div>

      {/* Modals */}
      <SetInstructionsModal
        isOpen={showInstructionsModal}
        onClose={() => setShowInstructionsModal(false)}
        onSave={setInstructions}
        projectName={project.name}
        initialInstructions={instructions}
      />

      <AddTextContentModal
        isOpen={showAddTextModal}
        onClose={() => setShowAddTextModal(false)}
        onAdd={(title, content) => {
          setFiles((prev) => [
            ...prev,
            { id: Date.now().toString(), name: title, type: "text" },
          ]);
        }}
      />
    </div>
  );
}
