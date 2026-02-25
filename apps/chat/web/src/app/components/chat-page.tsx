import { useState } from "react";
import { User } from "lucide-react";
import { useParams } from "react-router";
import { ChatInput } from "./chat-input";

interface Message {
  id: string;
  role: "user" | "assistant";
  content: string;
}

const sampleChats: Record<string, { title: string; messages: Message[] }> = {
  "1": {
    title: "Multi-modality AI application",
    messages: [
      {
        id: "1",
        role: "user",
        content: "I want to build a multi-modality AI application that can handle text, images, and audio. What architecture would you recommend?",
      },
      {
        id: "2",
        role: "assistant",
        content: "Great question! For a multi-modality AI application, I'd recommend a modular architecture with the following components:\n\n1. **Input Processing Layer**: Separate encoders for each modality (text tokenizer, image encoder like ViT, audio encoder like Whisper)\n\n2. **Fusion Layer**: A cross-attention mechanism or multimodal transformer that combines representations from different modalities\n\n3. **Reasoning Layer**: A large language model backbone (like LLaMA or Mistral) that processes the fused representations\n\n4. **Output Layer**: Decoders for generating text, and optionally images or audio\n\nWould you like me to dive deeper into any of these components?",
      },
    ],
  },
  "2": {
    title: "LLM Inference Infrastructure",
    messages: [
      {
        id: "1",
        role: "user",
        content: "Can you give me an overview of the key challenges in LLM inference infrastructure?",
      },
      {
        id: "2",
        role: "assistant",
        content: "Here's an overview of the key challenges in LLM inference infrastructure:\n\n**Scalability:**\n- **Auto-scaling** - Dynamically adjusting resources based on traffic patterns\n- **Multi-model serving** - Efficiently serving multiple models on shared infrastructure\n- **LoRA adapters** - Hot-loading and managing fine-tuned adapters at scale\n\n**Performance:**\n- **KV cache management** - Optimizing memory usage for long context windows\n- **Prefill/Decode disaggregation** - Separating compute-heavy prefill from memory-bound decode\n- **Request routing** - Intelligent load balancing across model replicas\n\n**Operations:**\n- **Model deployment** - Kubernetes-native orchestration for model lifecycle\n- **Observability** - Metrics, logging, and tracing for inference workloads\n- **Cost optimization** - Maximizing GPU utilization and minimizing waste",
      },
    ],
  },
};

export function ChatPage() {
  const { id } = useParams();
  const chat = id ? sampleChats[id] : null;
  const [messages, setMessages] = useState<Message[]>(chat?.messages || []);

  const handleSend = (content: string) => {
    const userMsg: Message = {
      id: crypto.randomUUID(),
      role: "user",
      content,
    };
    setMessages((prev) => [
      ...prev,
      userMsg,
      {
        id: crypto.randomUUID(),
        role: "assistant",
        content: "I'm a demo interface. Connect your own model API to enable real responses! This UI is ready to be connected to any LLM backend.",
      },
    ]);
  };

  return (
    <div className="flex-1 flex flex-col h-full">
      <div className="flex-1 overflow-y-auto px-4 py-6">
        <div className="max-w-[680px] mx-auto space-y-6">
          {messages.map((msg) => (
            <div key={msg.id} className="flex gap-3">
              <div
                className={`flex-shrink-0 w-7 h-7 rounded-full flex items-center justify-center mt-0.5 ${
                  msg.role === "user" ? "bg-accent" : "bg-orange-800/30"
                }`}
              >
                {msg.role === "user" ? (
                  <User size={14} className="text-foreground/70" />
                ) : (
                  <span className="text-xs">✺</span>
                )}
              </div>
              <div className="flex-1 min-w-0">
                <p className="text-xs text-muted-foreground mb-1">
                  {msg.role === "user" ? "You" : "AI Assistant"}
                </p>
                <div className="text-sm text-foreground/90 whitespace-pre-wrap leading-relaxed">
                  {msg.content}
                </div>
              </div>
            </div>
          ))}
        </div>
      </div>

      <div className="px-4 pb-6 pt-2">
        <ChatInput placeholder="Reply..." onSend={handleSend} />
      </div>
    </div>
  );
}