import { PlaceholderPage } from "./placeholder-page";

export function ChatsPlaceholder() {
  return (
    <PlaceholderPage
      title="Chats"
      description="Your conversations will appear here. Start a new chat to begin."
      icon="chats"
    />
  );
}

export function ArtifactsPlaceholder() {
  return (
    <PlaceholderPage
      title="Artifacts"
      description="Generated artifacts from your conversations will appear here."
      icon="artifacts"
    />
  );
}

export function CodePlaceholder() {
  return (
    <PlaceholderPage
      title="Code"
      description="Code snippets and projects from your conversations will appear here."
      icon="code"
    />
  );
}
