import { useState, useEffect, useRef, useCallback } from "react";
import {
  User,
  Loader2,
  Volume2,
  Square,
  Copy,
  Pencil,
  RefreshCw,
  Download,
  Check,
  X,
  Sparkles,
} from "lucide-react";
import { useParams, useLocation } from "react-router";
import { ChatInput,type Attachment as InputAttachment } from "./chat-input";
import { Tooltip } from "./tooltip";
import { MarkdownContent } from "./markdown-content";
import {
  getConversation,
  streamCompletion,
  generateSpeech,
  generateImage,
  editImage,
  generateVideo,
  getVideoStatus,
  getVideoContentUrl,
  notifyConversationsChanged,
  type Message as ApiMessage,
  type Conversation,
  type ImageData,
  type VideoJobResponse,
  type ChatAttachmentPayload,
} from "@/api/client";
import {
  pendingCreation,
  cacheMessages,
  getCachedMessages,
  type CreationMessage,
} from "@/app/creation-store";

// Sora video sizes: only 720x1280 (portrait) and 1280x720 (landscape)
const RATIO_TO_SIZE: Record<string, { image: string; video: string }> = {
  "1:1": { image: "1024x1024", video: "1280x720" },
  "16:9": { image: "1792x1024", video: "1280x720" },
  "9:16": { image: "1024x1792", video: "720x1280" },
  "4:3": { image: "1024x768", video: "1280x720" },
  "3:4": { image: "768x1024", video: "720x1280" },
};

interface Message {
  id: string;
  role: "user" | "assistant";
  content: string;
  model?: string;
  streaming?: boolean;
  attachments?: MessageAttachment[];
  // AI Creation fields
  mode?: "image" | "video";
  ratio?: string;
  referenceImageUrl?: string;
  images?: ImageData[];
  videoJob?: VideoJobResponse;
  loading?: boolean;
  error?: string;
}

interface MessageAttachment {
  id: string;
  name: string;
  type: string;
  kind: "image" | "file";
  blob_url?: string;
  preview_url?: string;
  file?: File;
}

export function ChatPage() {
  const { id } = useParams();
  const location = useLocation();
  const [messages, setMessages] = useState<Message[]>([]);
  const [selectedModel, setSelectedModel] = useState("gpt-4-0613");
  const [conversation, setConversation] = useState<Conversation | null>(null);
  const [loading, setLoading] = useState(true);
  const [streaming, setStreaming] = useState(false);
  const [playingMessageId, setPlayingMessageId] = useState<string | null>(null);
  const [ttsLoadingId, setTtsLoadingId] = useState<string | null>(null);
  const [editingId, setEditingId] = useState<string | null>(null);
  const [editText, setEditText] = useState("");
  const messagesEndRef = useRef<HTMLDivElement>(null);
  const audioRef = useRef<HTMLAudioElement | null>(null);
  const controllerRef = useRef<AbortController | null>(null);
  const processedFirstMsg = useRef(false);
  const lastModelRef = useRef<string>("default");
  const loadedConvId = useRef<string | null>(null);

  const messagesRef = useRef(messages);
  messagesRef.current = messages;

  useEffect(() => {
    return () => {
      messagesRef.current.forEach((msg) => {
        msg.attachments?.forEach((attachment) => {
          if (attachment.blob_url?.startsWith("blob:")) {
            URL.revokeObjectURL(attachment.blob_url);
          }
        });
      });
    };
  }, []);
  
  // Scroll to bottom on new messages
  const scrollToBottom = useCallback(() => {
    messagesEndRef.current?.scrollIntoView({ behavior: "smooth" });
  }, []);

  useEffect(() => {
    scrollToBottom();
  }, [messages, scrollToBottom]);

  // Cache creation messages (image/video) so sidebar clicks can restore them.
  // Only cache messages with mode set — text messages are persisted by the backend.
  // Guard: only cache when messages actually belong to this conversation
  // (loadedConvId is set after getConversation resolves).
  useEffect(() => {
    if (!id || id !== loadedConvId.current || messages.length === 0) return;
    const creationOnly = messages.filter((m) => m.mode);
    if (creationOnly.length > 0) {
      cacheMessages(id, creationOnly as CreationMessage[]);
    }
  }, [id, messages]);

  // Load conversation on mount or when id changes
  useEffect(() => {
    if (!id) return;
    processedFirstMsg.current = false;
    loadedConvId.current = null; // reset until load completes
    setLoading(true);
    setMessages([]);
    setConversation(null);

    getConversation(id)
      .then((conv) => {
        if (conv) {
          setConversation(conv);
          const apiMsgs: Message[] = (conv.messages ?? [])
            .filter((m: ApiMessage) => m.role !== "system")
            .map((m: ApiMessage) => ({
              id: m.id,
              role: m.role as "user" | "assistant",
              content: m.content as string,
              model: m.model ?? undefined,
              attachments: (m.attachments ?? []).map((attachment) => ({
                ...attachment,
                blob_url: attachment.blob_url ?? attachment.preview_url,
              })),
            }));
          // Prepend cached creation messages (image/video) before
          // backend messages (text). The backend only stores text
          // chat messages; creation results live in the client cache.
          const cached = getCachedMessages(id);
          const creationMsgs = (cached ?? []) as Message[];
          const cachedIds = new Set(creationMsgs.map((m) => m.id));
          const deduped = apiMsgs.filter((m) => !cachedIds.has(m.id));
          setMessages([...creationMsgs, ...deduped]);
          loadedConvId.current = id;
        }
      })
      .catch(() => {
        // Conversation may not exist yet — check creation cache
        if (id) {
          const cached = getCachedMessages(id);
          if (cached && cached.length > 0) {
            setMessages(cached as Message[]);
          }
          loadedConvId.current = id;
        }
      })
      .finally(() => setLoading(false));
  }, [id]);

  // Poll video status helper
  const pollVideoStatus = useCallback(
    async (jobId: string, assistantMsgId: string) => {
      const maxAttempts = 120;
      for (let i = 0; i < maxAttempts; i++) {
        await new Promise((r) => setTimeout(r, 5000));
        try {
          const status = await getVideoStatus(jobId);
          setMessages((prev) =>
            prev.map((m) =>
              m.id === assistantMsgId && m.role === "assistant"
                ? { ...m, videoJob: status, loading: false }
                : m
            )
          );
          if (
            status.status === "completed" ||
            status.status === "failed" ||
            status.error
          ) {
            return;
          }
        } catch {
          // Keep polling on transient errors
        }
      }
    },
    []
  );

  // Handle image generation
  const handleImageGeneration = useCallback(
    async (
      prompt: string,
      model: string,
      ratio: string,
      referenceFile?: File,
      referenceImageUrl?: string
    ) => {
      const userMsgId = crypto.randomUUID();
      const assistantMsgId = crypto.randomUUID();
      const sizeMap = RATIO_TO_SIZE[ratio] ?? RATIO_TO_SIZE["1:1"];

      const userMsg: Message = {
        id: userMsgId,
        role: "user",
        content: prompt,
        model,
        mode: "image",
        ratio,
        referenceImageUrl,
      };

      const assistantMsg: Message = {
        id: assistantMsgId,
        role: "assistant",
        content: "",
        model,
        mode: "image",
        loading: true,
      };

      setMessages((prev) => [...prev, userMsg, assistantMsg]);

      try {
        let response;
        if (referenceFile) {
          const isDallE2 = model === "dall-e-2";
          const editSize =
            isDallE2 &&
            !["256x256", "512x512", "1024x1024"].includes(sizeMap.image)
              ? "1024x1024"
              : sizeMap.image;
          response = await editImage(referenceFile, prompt, model, editSize, 1);
        } else {
          response = await generateImage(prompt, model, sizeMap.image, 1);
        }
        setMessages((prev) =>
          prev.map((m) =>
            m.id === assistantMsgId
              ? { ...m, images: response.data, loading: false }
              : m
          )
        );
      } catch (e: unknown) {
        const msg = e instanceof Error ? e.message : "Image generation failed";
        setMessages((prev) =>
          prev.map((m) =>
            m.id === assistantMsgId
              ? { ...m, error: msg, loading: false }
              : m
          )
        );
      }
    },
    []
  );

  // Handle video generation
  const handleVideoGeneration = useCallback(
    async (prompt: string, model: string, ratio: string) => {
      const userMsgId = crypto.randomUUID();
      const assistantMsgId = crypto.randomUUID();
      const sizeMap = RATIO_TO_SIZE[ratio] ?? RATIO_TO_SIZE["1:1"];

      const userMsg: Message = {
        id: userMsgId,
        role: "user",
        content: prompt,
        model,
        mode: "video",
        ratio,
      };

      const assistantMsg: Message = {
        id: assistantMsgId,
        role: "assistant",
        content: "",
        model,
        mode: "video",
        loading: true,
      };

      setMessages((prev) => [...prev, userMsg, assistantMsg]);

      try {
        const job = await generateVideo(prompt, model, sizeMap.video, "4");
        setMessages((prev) =>
          prev.map((m) =>
            m.id === assistantMsgId
              ? {
                  ...m,
                  videoJob: job,
                  loading:
                    job.status !== "completed" && job.status !== "failed",
                }
              : m
          )
        );
        if (job.status !== "completed" && job.status !== "failed") {
          pollVideoStatus(job.id, assistantMsgId);
        }
      } catch (e: unknown) {
        const msg =
          e instanceof Error ? e.message : "Video generation failed";
        setMessages((prev) =>
          prev.map((m) =>
            m.id === assistantMsgId
              ? { ...m, error: msg, loading: false }
              : m
          )
        );
      }
    },
    [pollVideoStatus]
  );

  // Handle first message from home page or AI creation navigation
  useEffect(() => {
    if (
      !id ||
      loading ||
      processedFirstMsg.current ||
      !location.state
    )
      return;

    processedFirstMsg.current = true;
    loadedConvId.current = id;
    const { firstMessage, model, mode, ratio, attachments } = location.state as {
      firstMessage: string;
      model: string;
      attachments?: InputAttachment[];
      mode?: "image" | "video";
      ratio?: string;
    };
    setSelectedModel(model);

    // Read non-serializable data from module-level store (set by AICreationPage)
    const referenceFile = pendingCreation.file;
    const referenceImageUrl = pendingCreation.blobUrl;
    pendingCreation.file = undefined;
    pendingCreation.blobUrl = undefined;

    // Clear navigation state to prevent re-send on refresh
    window.history.replaceState({}, "");

    if (mode === "image") {
      handleImageGeneration(
        firstMessage,
        model,
        ratio ?? "1:1",
        referenceFile,
        referenceImageUrl
      );
    } else if (mode === "video") {
      handleVideoGeneration(firstMessage, model, ratio ?? "1:1");
    } else {
      sendMessage(firstMessage, model, attachments);
    }
  }, [id, loading, location.state]); // eslint-disable-line react-hooks/exhaustive-deps

  /**
   * Stream an assistant response for the given content.
   * Does NOT add a user message — only creates an assistant placeholder
   * and streams the response. Use this for edit/regenerate flows.
   */
  const streamResponse = useCallback(
    (content: string, model: string) => {
      if (!id || streaming) return;

      lastModelRef.current = model;
      const assistantId = crypto.randomUUID();
      const assistantMsg: Message = {
        id: assistantId,
        role: "assistant",
        content: "",
        model,
        streaming: true,
      };

      setMessages((prev) => [...prev, assistantMsg]);
      setStreaming(true);

      controllerRef.current = streamCompletion(id, content, model, undefined, {
        onDelta: (delta) => {
          setMessages((prev) =>
            prev.map((m) =>
              m.id === assistantId
                ? { ...m, content: m.content + delta }
                : m
            )
          );
        },
        onDone: () => {
          setMessages((prev) =>
            prev.map((m) =>
              m.id === assistantId ? { ...m, streaming: false } : m
            )
          );
          setStreaming(false);
          notifyConversationsChanged();
        },
        onError: (errMsg) => {
          setMessages((prev) =>
            prev.map((m) =>
              m.id === assistantId
                ? {
                    ...m,
                    content: m.content || `Error: ${errMsg}`,
                    streaming: false,
                  }
                : m
            )
          );
          setStreaming(false);
        },
      });
    },
    [id, streaming]
  );

  const sendMessage = useCallback(
    (content: string, model: string, attachments?: InputAttachment[]) => {
      if (!id || streaming) return;

      const attachmentPayload: ChatAttachmentPayload[] | undefined = 
        attachments?.map((attachment) => ({
          id: attachment.id,
          name: attachment.name,
          type: attachment.type,
          kind: attachment.kind,
          file: attachment.file,
        }));

      lastModelRef.current = model;
      setSelectedModel(model);
      const userMsg: Message = {
        id: crypto.randomUUID(),
        role: "user",
        content,
        attachments,
      };
      const assistantId = crypto.randomUUID();
      const assistantMsg: Message = {
        id: assistantId,
        role: "assistant",
        content: "",
        model,
        streaming: true,
      };

      setMessages((prev) => [...prev, userMsg, assistantMsg]);
      setStreaming(true);

      controllerRef.current = streamCompletion(id, content, model, attachmentPayload, {
        onDelta: (delta) => {
          setMessages((prev) =>
            prev.map((m) =>
              m.id === assistantId
                ? { ...m, content: m.content + delta }
                : m
            )
          );
        },
        onDone: () => {
          setMessages((prev) =>
            prev.map((m) =>
              m.id === assistantId ? { ...m, streaming: false } : m
            )
          );
          setStreaming(false);
          notifyConversationsChanged();
        },
        onError: (errMsg) => {
          setMessages((prev) =>
            prev.map((m) =>
              m.id === assistantId
                ? {
                    ...m,
                    content: m.content || `Error: ${errMsg}`,
                    streaming: false,
                  }
                : m
            )
          );
          setStreaming(false);
        },
      });

    },
    [id, streaming]
  );

  // Note: we intentionally do NOT abort the stream on unmount.
  // If the user navigates away mid-stream, the backend continues
  // receiving tokens and saves the response via try/finally.
  // When the user returns, getConversation() reloads the saved messages.
  // React 18 safely ignores setState calls on unmounted components.

  const handleTTS = useCallback(async (messageId: string, content: string) => {
    // Toggle off if already playing this message
    if (playingMessageId === messageId) {
      audioRef.current?.pause();
      audioRef.current = null;
      setPlayingMessageId(null);
      return;
    }

    // Stop any existing playback
    audioRef.current?.pause();
    audioRef.current = null;
    setPlayingMessageId(null);
    setTtsLoadingId(messageId);

    try {
      const blob = await generateSpeech(content);
      const url = URL.createObjectURL(blob);
      const audio = new Audio(url);
      audioRef.current = audio;

      audio.onended = () => {
        setPlayingMessageId(null);
        URL.revokeObjectURL(url);
        audioRef.current = null;
      };
      audio.onerror = () => {
        setPlayingMessageId(null);
        setTtsLoadingId(null);
        URL.revokeObjectURL(url);
        audioRef.current = null;
      };

      setTtsLoadingId(null);
      setPlayingMessageId(messageId);
      await audio.play();
    } catch {
      setTtsLoadingId(null);
      setPlayingMessageId(null);
    }
  }, [playingMessageId]);

  const handleSend = (content: string, model: string,attachments?: InputAttachment[]) => {
    sendMessage(content, model,attachments);
  };

  // ── Edit handlers ──────────────────────────────────────

  const handleEdit = (msgId: string) => {
    const msg = messages.find((m) => m.id === msgId);
    if (msg) {
      setEditingId(msgId);
      setEditText(msg.content);
    }
  };

  const handleEditSave = async () => {
    if (!editingId || !editText.trim()) return;

    const idx = messages.findIndex((m) => m.id === editingId);
    if (idx === -1) return;

    // Truncate messages after the edited one and update content
    const updated = messages.slice(0, idx);
    const editedMsg = { ...messages[idx], content: editText.trim() };
    updated.push(editedMsg);
    setMessages(updated);
    setEditingId(null);
    setEditText("");

    // Determine model to use
    const model =
      messages.find((m) => m.model)?.model || lastModelRef.current;

    // Stream a new response (user message is already in the array)
    // We need a small delay so the state update settles
    setTimeout(() => {
      streamResponse(editText.trim(), model);
    }, 0);
  };

  const handleEditCancel = () => {
    setEditingId(null);
    setEditText("");
  };

  // ── Regenerate handler ─────────────────────────────────

  const handleRegenerate = (msgId: string) => {
    const idx = messages.findIndex((m) => m.id === msgId);
    if (idx <= 0) return;

    // Find the preceding user message
    let userMsgIdx = idx - 1;
    while (userMsgIdx >= 0 && messages[userMsgIdx].role !== "user") {
      userMsgIdx--;
    }
    if (userMsgIdx < 0) return;

    const userMsg = messages[userMsgIdx];
    const model =
      messages[idx].model ||
      messages.find((m) => m.model)?.model ||
      lastModelRef.current;

    // Remove the assistant message (and anything after it)
    setMessages((prev) => prev.slice(0, idx));

    // Stream a new response
    setTimeout(() => {
      streamResponse(userMsg.content, model);
    }, 0);
  };

  // ── Rendering helpers for AI creation messages ─────────

  const renderCreationUserMessage = (msg: Message) => (
    <div className="flex-1 min-w-0">
      <p className="text-xs text-muted-foreground mb-1">You</p>
      <p className="text-sm text-foreground/90 leading-relaxed">
        {msg.content}
      </p>
      {msg.referenceImageUrl && (
        <div className="mt-2">
          <img
            src={msg.referenceImageUrl}
            alt="Reference"
            className="w-16 h-16 rounded-lg object-cover border border-border"
          />
        </div>
      )}
      <div className="flex items-center gap-2 mt-1.5">
        <span className="text-xs text-muted-foreground">{msg.model}</span>
        <span className="text-xs text-muted-foreground">&middot;</span>
        <span className="text-xs text-muted-foreground">{msg.ratio}</span>
        <span className="text-xs text-muted-foreground">&middot;</span>
        <span className="text-xs text-muted-foreground capitalize">
          {msg.mode}
        </span>
      </div>
    </div>
  );

  const renderUserAttachments = (attachments?: MessageAttachment[]) => {
    if (!attachments || attachments.length === 0) return null;

    return (
      <div className="mt-3 flex flex-wrap gap-2">
        {attachments.map((attachment) => (
          <div
            key={attachment.id}
            className="w-[120px] rounded-xl overflow-hidden border border-border bg-accent/50"
          >
            {attachment.kind === "image" && attachment.blob_url ? (
              <img
                src={attachment.blob_url}
                alt={attachment.name}
                className="w-full h-[90px] object-cover"
              />
            ) : (
              <div className="h-[90px] flex flex-col items-center justify-center px-2 text-center">
                <div className="text-xs text-foreground/50 mb-1">FILE</div>
                <div className="text-xs text-foreground/80 line-clamp-2 break-all">
                  {attachment.name}
                </div>
              </div>
            )}
          </div>
        ))}
      </div>
    );
  };
  
  const renderCreationAssistantMessage = (msg: Message) => (
    <div className="flex-1 min-w-0">
      <p className="text-xs text-muted-foreground mb-1">AI Assistant</p>

      {/* Loading state */}
      {msg.loading && (
        <div className="w-full max-w-[320px] aspect-square rounded-lg bg-accent/50 border border-border flex items-center justify-center">
          <div className="text-center px-6">
            <div className="w-8 h-8 rounded-full border-2 border-foreground/20 border-t-amber-400/80 animate-spin mx-auto mb-2" />
            <p className="text-xs text-muted-foreground">
              Creating your {msg.mode}...
            </p>
          </div>
        </div>
      )}

      {/* Error state */}
      {msg.error && (
        <div className="rounded-lg bg-red-500/10 border border-red-500/20 p-3">
          <p className="text-sm text-red-400">{msg.error}</p>
        </div>
      )}

      {/* Image results */}
      {msg.images && msg.images.length > 0 && (
        <div className="flex gap-3 flex-wrap">
          {msg.images.map((img, idx) => (
            <div key={idx} className="relative group">
              <img
                src={
                  img.url ||
                  (img.b64_json
                    ? `data:image/png;base64,${img.b64_json}`
                    : "")
                }
                alt={img.revised_prompt || "Generated image"}
                className="max-w-[320px] rounded-lg border border-border"
              />
              {img.url && (
                <a
                  href={img.url}
                  download
                  target="_blank"
                  rel="noopener noreferrer"
                  className="absolute top-2 right-2 p-1.5 rounded-lg bg-black/50 text-white opacity-0 group-hover:opacity-100 transition-opacity"
                >
                  <Download size={14} />
                </a>
              )}
              {img.revised_prompt && (
                <p className="text-xs text-muted-foreground mt-1.5 max-w-[320px]">
                  {img.revised_prompt}
                </p>
              )}
            </div>
          ))}
        </div>
      )}

      {/* Video job status / player */}
      {msg.videoJob && (
        <div className="rounded-lg bg-accent/50 border border-border p-4">
          {msg.videoJob.status === "completed" ? (
            <div className="space-y-3">
              <video
                src={getVideoContentUrl(msg.videoJob.id)}
                controls
                className="max-w-[480px] rounded-lg"
              />
              <div className="flex items-center gap-3">
                <Check size={16} className="text-green-400" />
                <span className="text-sm text-green-400">Video ready</span>
                <button
                  onClick={async () => {
                    try {
                      const res = await fetch(
                        getVideoContentUrl(msg.videoJob!.id)
                      );
                      if (!res.ok) throw new Error("Download failed");
                      const blob = await res.blob();
                      const url = URL.createObjectURL(blob);
                      const a = document.createElement("a");
                      a.href = url;
                      a.download = `${msg.videoJob!.id}.mp4`;
                      document.body.appendChild(a);
                      a.click();
                      a.remove();
                      URL.revokeObjectURL(url);
                    } catch (err) {
                      console.error("Video download failed:", err);
                    }
                  }}
                  className="ml-auto flex items-center gap-1.5 px-3 py-1.5 rounded-lg bg-accent hover:bg-accent/80 text-sm text-foreground transition-colors"
                >
                  <Download size={14} />
                  Download
                </button>
              </div>
            </div>
          ) : (
            <>
              <div className="flex items-center gap-2 mb-2">
                {msg.videoJob.status === "failed" ? (
                  <X size={16} className="text-red-400" />
                ) : (
                  <Loader2
                    size={16}
                    className="text-amber-400 animate-spin"
                  />
                )}
                <span className="text-sm text-foreground capitalize">
                  {msg.videoJob.status}
                </span>
                {msg.videoJob.progress != null &&
                  msg.videoJob.progress > 0 && (
                    <span className="text-xs text-muted-foreground">
                      {Math.round(msg.videoJob.progress)}%
                    </span>
                  )}
              </div>
              {msg.videoJob.error && (
                <p className="text-sm text-red-400">{msg.videoJob.error}</p>
              )}
            </>
          )}
        </div>
      )}
    </div>
  );

  if (loading) {
    return (
      <div className="flex-1 flex items-center justify-center">
        <Loader2 size={24} className="animate-spin text-foreground/40" />
      </div>
    );
  }

  return (
    <div className="flex-1 flex flex-col h-full">
      {/* Project context badge */}
      {conversation?.project_id && (
        <div className="px-4 pt-3 pb-0">
          <div className="max-w-[680px] mx-auto">
            <span className="text-xs text-amber-400/70">
              (Project context active)
            </span>
          </div>
        </div>
      )}

      <div className="flex-1 overflow-y-auto px-4 py-6">
        <div className="max-w-[680px] mx-auto space-y-6">
          {messages.map((msg, idx) => {
            const isCreation = !!msg.mode;

            return (
              <div key={msg.id} className="group flex gap-3">
                <div
                  className={`flex-shrink-0 w-7 h-7 rounded-full flex items-center justify-center mt-0.5 ${
                    msg.role === "user"
                      ? "bg-accent"
                      : isCreation
                        ? "bg-amber-400/15"
                        : "bg-orange-800/30"
                  }`}
                >
                  {msg.role === "user" ? (
                    <User size={14} className="text-foreground/70" />
                  ) : isCreation ? (
                    <Sparkles size={14} className="text-amber-400/80" />
                  ) : (
                    <span className="text-xs">&#10042;</span>
                  )}
                </div>

                {/* Creation messages use specialized renderers */}
                {isCreation ? (
                  msg.role === "user" ? (
                    renderCreationUserMessage(msg)
                  ) : (
                    renderCreationAssistantMessage(msg)
                  )
                ) : (
                  /* Regular text chat messages */
                  <div className="flex-1 min-w-0">
                    <p className="text-xs text-muted-foreground mb-1">
                      {msg.role === "user" ? "You" : "AI Assistant"}
                    </p>
                    {msg.role === "user" ? (
                      <>
                        {editingId === msg.id ? (
                          <div className="space-y-2">
                            <textarea
                              value={editText}
                              onChange={(e) => setEditText(e.target.value)}
                              className="w-full p-2 rounded-lg bg-black/20 border border-white/10 text-sm text-foreground resize-none focus:outline-none focus:ring-1 focus:ring-amber-500/40"
                              rows={3}
                              autoFocus
                            />
                            <div className="flex gap-2">
                              <button
                                onClick={handleEditSave}
                                className="px-3 py-1 rounded-lg bg-amber-600 text-white text-xs hover:bg-amber-500 transition-colors"
                              >
                                Save &amp; Resend
                              </button>
                              <button
                                onClick={handleEditCancel}
                                className="px-3 py-1 rounded-lg bg-white/10 text-foreground/70 text-xs hover:bg-white/20 transition-colors"
                              >
                                Cancel
                              </button>
                            </div>
                          </div>
                        ) : (
                          <>
                            {msg.content ? (
                              <div className="text-sm text-foreground/90 whitespace-pre-wrap leading-relaxed">
                                {msg.content}
                              </div>
                            ) : null}
                            {renderUserAttachments(msg.attachments)}
                          </>
                        )}
                        {/* Edit button — visible on hover, hidden while streaming or editing */}
                        {!streaming && editingId !== msg.id && (
                          <div className="opacity-0 group-hover:opacity-100 transition-opacity mt-1">
                            <Tooltip content="Edit message">
                              <button
                                onClick={() => handleEdit(msg.id)}
                                className="p-1 rounded hover:bg-accent text-foreground/40 hover:text-foreground/70 transition-colors"
                              >
                                <Pencil size={14} />
                              </button>
                            </Tooltip>
                          </div>
                        )}
                      </>
                    ) : (
                      <div className="text-sm text-foreground/90 leading-relaxed">
                        {msg.content ? (
                          <MarkdownContent content={msg.content} />
                        ) : null}
                        {msg.streaming && !msg.content && (
                          <span className="inline-flex items-center gap-1 text-foreground/40">
                            <Loader2 size={14} className="animate-spin" />
                            Thinking...
                          </span>
                        )}
                        {msg.streaming && msg.content && (
                          <span className="inline-block w-1.5 h-4 bg-foreground/50 animate-pulse ml-0.5 align-text-bottom" />
                        )}
                      </div>
                    )}
                    {msg.role === "assistant" && !msg.streaming && msg.content && (
                      <div className={`mt-1.5 flex items-center gap-0.5 ${
                        playingMessageId === msg.id ? "opacity-100" : "opacity-0 group-hover:opacity-100"
                      } transition-opacity`}>
                        <Tooltip content="Copy message">
                          <button
                            onClick={() => navigator.clipboard.writeText(msg.content)}
                            className="p-1 rounded hover:bg-accent text-foreground/40 hover:text-foreground/70 transition-colors"
                          >
                            <Copy size={14} />
                          </button>
                        </Tooltip>
                        <Tooltip content={playingMessageId === msg.id ? "Stop" : "Read aloud"}>
                          <button
                            onClick={() => handleTTS(msg.id, msg.content)}
                            className="p-1 rounded hover:bg-accent text-foreground/40 hover:text-foreground/70 transition-colors"
                          >
                            {ttsLoadingId === msg.id ? (
                              <Loader2 size={14} className="animate-spin" />
                            ) : playingMessageId === msg.id ? (
                              <Square size={14} />
                            ) : (
                              <Volume2 size={14} />
                            )}
                          </button>
                        </Tooltip>
                        {/* Regenerate — only on the last assistant message */}
                        {idx === messages.length - 1 && !streaming && (
                          <Tooltip content="Regenerate response">
                            <button
                              onClick={() => handleRegenerate(msg.id)}
                              className="p-1 rounded hover:bg-accent text-foreground/40 hover:text-foreground/70 transition-colors"
                            >
                              <RefreshCw size={14} />
                            </button>
                          </Tooltip>
                        )}
                      </div>
                    )}
                  </div>
                )}
              </div>
            );
          })}
          <div ref={messagesEndRef} />
        </div>
      </div>

      {streaming && (
        <div className="flex justify-center py-2">
          <button
            onClick={() => {
              controllerRef.current?.abort();
              setStreaming(false);
            }}
            className="flex items-center gap-2 px-4 py-2 rounded-full border border-white/10 bg-background hover:bg-accent text-sm text-foreground/70 hover:text-foreground transition-colors"
          >
            <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="currentColor"><rect x="4" y="4" width="16" height="16" rx="2"/></svg>
            Stop generating
          </button>
        </div>
      )}

      <div className="px-4 pb-6 pt-2">
        <ChatInput
          placeholder="Reply..."
          disabled={streaming}
          selectedModel={selectedModel}
          onModelChange={setSelectedModel}
          onSend={handleSend}
        />
      </div>
    </div>
  );
}
