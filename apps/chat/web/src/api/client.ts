/**
 * AIBrix Chat API client.
 *
 * All calls go through the Vite proxy (/api/* -> backend:8000).
 * The backend proxies to any OpenAI-compatible endpoint (AIBrix gateway,
 * vLLM, OpenAI cloud, etc.) configured via environment variables.
 */

// ── Auth helpers ─────────────────────────────────────────

const TOKEN_KEY = "aibrix-chat-token";

function getAuthToken(): string | null {
  try {
    return localStorage.getItem(TOKEN_KEY);
  } catch {
    return null;
  }
}

function authHeaders(): Record<string, string> {
  const token = getAuthToken();
  return token ? { Authorization: `Bearer ${token}` } : {};
}

/** Handle 401 responses by clearing token and redirecting to login. */
function handle401(res: Response): void {
  if (res.status === 401) {
    localStorage.removeItem(TOKEN_KEY);
    if (!window.location.pathname.startsWith("/login")) {
      window.location.href = "/login";
    }
  }
}

// ── Types ────────────────────────────────────────────────

export interface ModelInfo {
  id: string;
  name: string | null;
  capabilities: string[];
  owned_by: string | null;
}

export interface ConversationSummary {
  id: string;
  title: string;
  model: string | null;
  message_count: number;
  created_at: string;
  updated_at: string;
}

export interface Message {
  id: string;
  role: "user" | "assistant" | "system";
  content: string;
  parent_id: string | null;
  model: string | null;
  created_at: string;
}

export interface Conversation {
  id: string;
  title: string;
  messages: Message[];
  model: string | null;
  project_id: string | null;
  created_at: string;
  updated_at: string;
}

export interface StreamCallbacks {
  onDelta: (text: string) => void;
  onDone: () => void;
  onError: (message: string) => void;
}

export interface ImageData {
  b64_json?: string;
  url?: string;
  revised_prompt?: string;
}

export interface ImageGenerateResponse {
  created: number;
  data: ImageData[];
}

export interface VideoJobResponse {
  id: string;
  status: string;
  prompt?: string;
  model?: string;
  progress?: number;
  error?: string;
  generations?: Array<Record<string, unknown>>;
}

export interface ProjectSummary {
  id: string;
  name: string;
  description: string;
  updated_at: string;
}

export interface Project {
  id: string;
  name: string;
  description: string;
  instructions: string;
  created_at: string;
  updated_at: string;
}

// ── Notification ─────────────────────────────────────────

/** Dispatch a custom event so sidebar/chats-page can re-fetch. */
export function notifyConversationsChanged() {
  window.dispatchEvent(new CustomEvent("conversations-changed"));
}

// ── Models ───────────────────────────────────────────────

export async function fetchModels(capability?: string): Promise<ModelInfo[]> {
  const url = capability
    ? `/api/models?capability=${encodeURIComponent(capability)}`
    : "/api/models";
  const res = await fetch(url, { headers: authHeaders() });
  if (!res.ok) return [];
  const data = await res.json();
  return data.models ?? [];
}

// ── Conversations ────────────────────────────────────────

export async function createConversation(
  model?: string,
  title?: string,
  projectId?: string,
): Promise<Conversation> {
  const res = await fetch("/api/conversations", {
    method: "POST",
    headers: { "Content-Type": "application/json", ...authHeaders() },
    body: JSON.stringify({
      model: model ?? null,
      title: title ?? "New Chat",
      project_id: projectId ?? null,
    }),
  });
  handle401(res);
  if (!res.ok) throw new Error("Failed to create conversation");
  return res.json();
}

export async function listConversations(): Promise<ConversationSummary[]> {
  const res = await fetch("/api/conversations", { headers: authHeaders() });
  handle401(res);
  if (!res.ok) return [];
  return res.json();
}

export async function getConversation(id: string): Promise<Conversation> {
  const res = await fetch(`/api/conversations/${id}`, {
    headers: authHeaders(),
  });
  handle401(res);
  if (!res.ok) throw new Error("Conversation not found");
  return res.json();
}

export async function renameConversation(
  id: string,
  title: string
): Promise<Conversation> {
  const res = await fetch(`/api/conversations/${id}`, {
    method: "PATCH",
    headers: { "Content-Type": "application/json", ...authHeaders() },
    body: JSON.stringify({ title }),
  });
  handle401(res);
  if (!res.ok) throw new Error("Failed to rename conversation");
  return res.json();
}

export async function deleteConversation(id: string): Promise<void> {
  const res = await fetch(`/api/conversations/${id}`, {
    method: "DELETE",
    headers: authHeaders(),
  });
  handle401(res);
  if (!res.ok) throw new Error("Failed to delete conversation");
}

// ── Chat Completion (SSE streaming) ──────────────────────

export function streamCompletion(
  conversationId: string,
  message: string,
  model: string,
  callbacks: StreamCallbacks,
  opts?: { temperature?: number; maxTokens?: number; systemPrompt?: string }
): AbortController {
  const controller = new AbortController();

  const body = {
    message,
    model,
    stream: true,
    temperature: opts?.temperature ?? 0.7,
    max_tokens: opts?.maxTokens ?? 2048,
    system_prompt: opts?.systemPrompt ?? null,
  };

  fetch(`/api/conversations/${conversationId}/completions`, {
    method: "POST",
    headers: { "Content-Type": "application/json", ...authHeaders() },
    body: JSON.stringify(body),
    signal: controller.signal,
  })
    .then(async (res) => {
      if (!res.ok) {
        callbacks.onError(`HTTP ${res.status}: ${res.statusText}`);
        return;
      }
      const reader = res.body?.getReader();
      if (!reader) {
        callbacks.onError("No response body");
        return;
      }

      const decoder = new TextDecoder();
      let buffer = "";

      while (true) {
        const { done, value } = await reader.read();
        if (done) break;

        buffer += decoder.decode(value, { stream: true });
        const lines = buffer.split("\n");
        buffer = lines.pop() ?? "";

        for (const line of lines) {
          const trimmed = line.trim();
          if (!trimmed || trimmed.startsWith(":")) continue;
          if (trimmed.startsWith("data:")) {
            const jsonStr = trimmed.slice(5).trim();
            if (jsonStr === "[DONE]") {
              callbacks.onDone();
              return;
            }
            try {
              const parsed = JSON.parse(jsonStr);
              if (parsed.event === "text_delta" && parsed.delta) {
                callbacks.onDelta(parsed.delta);
              } else if (parsed.event === "done") {
                callbacks.onDone();
                return;
              } else if (parsed.event === "error") {
                callbacks.onError(parsed.message || "Unknown error");
                return;
              }
            } catch {
              // Skip malformed JSON lines
            }
          }
        }
      }
      callbacks.onDone();
    })
    .catch((err) => {
      if (err.name !== "AbortError") {
        callbacks.onError(err.message || "Network error");
      }
    });

  return controller;
}

// ── Image Generation ─────────────────────────────────────

export async function generateImage(
  prompt: string,
  model: string = "dall-e-3",
  size: string = "1024x1024",
  n: number = 1,
): Promise<ImageGenerateResponse> {
  const res = await fetch("/api/image/generate", {
    method: "POST",
    headers: { "Content-Type": "application/json", ...authHeaders() },
    body: JSON.stringify({ prompt, model, size, n }),
  });
  if (!res.ok) {
    const detail = await res.text();
    throw new Error(`Image generation failed: ${detail}`);
  }
  return res.json();
}

// ── Image Edit ───────────────────────────────────────────

export async function editImage(
  image: File,
  prompt: string,
  model: string = "dall-e-2",
  size: string = "1024x1024",
  n: number = 1,
): Promise<ImageGenerateResponse> {
  const form = new FormData();
  form.append("image", image);
  form.append("prompt", prompt);
  form.append("model", model);
  form.append("size", size);
  form.append("n", String(n));

  const res = await fetch("/api/image/edit", {
    method: "POST",
    headers: authHeaders(),
    body: form,
  });
  if (!res.ok) {
    const detail = await res.text();
    throw new Error(`Image edit failed: ${detail}`);
  }
  return res.json();
}

// ── Audio Transcription ──────────────────────────────────

export async function transcribeAudio(
  file: File,
  model: string = "whisper-1",
  language?: string,
): Promise<{ text: string }> {
  const form = new FormData();
  form.append("file", file);
  form.append("model", model);
  if (language) form.append("language", language);

  const res = await fetch("/api/audio/transcribe", {
    method: "POST",
    headers: authHeaders(),
    body: form,
  });
  if (!res.ok) {
    const detail = await res.text();
    throw new Error(`Transcription failed: ${detail}`);
  }
  return res.json();
}

// ── Audio Speech (TTS) ──────────────────────────────────

export async function generateSpeech(
  input: string,
  model: string = "tts-1",
  voice: string = "alloy",
): Promise<Blob> {
  const res = await fetch("/api/audio/speech", {
    method: "POST",
    headers: { "Content-Type": "application/json", ...authHeaders() },
    body: JSON.stringify({ input, model, voice, response_format: "mp3" }),
  });
  if (!res.ok) {
    const detail = await res.text();
    throw new Error(`Speech generation failed: ${detail}`);
  }
  return res.blob();
}

// ── Video Generation ─────────────────────────────────────

export async function generateVideo(
  prompt: string,
  model: string = "sora-2",
  size: string = "1280x720",
  seconds: string = "4",
): Promise<VideoJobResponse> {
  const res = await fetch("/api/video/generate", {
    method: "POST",
    headers: { "Content-Type": "application/json", ...authHeaders() },
    body: JSON.stringify({ prompt, model, size, seconds }),
  });
  if (!res.ok) {
    const detail = await res.text();
    throw new Error(`Video generation failed: ${detail}`);
  }
  return res.json();
}

export async function getVideoStatus(jobId: string): Promise<VideoJobResponse> {
  const res = await fetch(`/api/video/status/${jobId}`, {
    headers: authHeaders(),
  });
  if (!res.ok) throw new Error("Failed to get video status");
  return res.json();
}

/** Returns the URL to stream/download the finished video MP4. */
export function getVideoContentUrl(jobId: string): string {
  return `/api/video/content/${jobId}`;
}

// ── Projects ─────────────────────────────────────────────

export async function createProject(
  name: string,
  description: string = ""
): Promise<Project> {
  const res = await fetch("/api/projects", {
    method: "POST",
    headers: { "Content-Type": "application/json", ...authHeaders() },
    body: JSON.stringify({ name, description }),
  });
  handle401(res);
  if (!res.ok) throw new Error("Failed to create project");
  return res.json();
}

export async function listProjects(): Promise<ProjectSummary[]> {
  const res = await fetch("/api/projects", { headers: authHeaders() });
  handle401(res);
  if (!res.ok) return [];
  return res.json();
}

export async function getProject(id: string): Promise<Project> {
  const res = await fetch(`/api/projects/${id}`, { headers: authHeaders() });
  handle401(res);
  if (!res.ok) throw new Error("Project not found");
  return res.json();
}

export async function updateProject(
  id: string,
  data: { name?: string; description?: string; instructions?: string }
): Promise<Project> {
  const res = await fetch(`/api/projects/${id}`, {
    method: "PATCH",
    headers: { "Content-Type": "application/json", ...authHeaders() },
    body: JSON.stringify(data),
  });
  handle401(res);
  if (!res.ok) throw new Error("Failed to update project");
  return res.json();
}

export async function deleteProject(id: string): Promise<void> {
  const res = await fetch(`/api/projects/${id}`, {
    method: "DELETE",
    headers: authHeaders(),
  });
  handle401(res);
  if (!res.ok) throw new Error("Failed to delete project");
}
