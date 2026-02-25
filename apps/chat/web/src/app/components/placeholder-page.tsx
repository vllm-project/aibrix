import { MessageSquare, Blocks, Code } from "lucide-react";

interface PlaceholderPageProps {
  title: string;
  description: string;
  icon: "chats" | "artifacts" | "code";
}

const icons = {
  chats: MessageSquare,
  artifacts: Blocks,
  code: Code,
};

export function PlaceholderPage({ title, description, icon }: PlaceholderPageProps) {
  const Icon = icons[icon];

  return (
    <div className="flex-1 flex flex-col items-center justify-center px-4 text-center">
      <div className="w-14 h-14 rounded-2xl bg-accent flex items-center justify-center mb-4">
        <Icon size={24} className="text-foreground/50" />
      </div>
      <h2
        style={{
          fontFamily: "'Playfair Display', serif",
          fontSize: "1.4rem",
          fontWeight: 500,
          marginBottom: "0.5rem",
        }}
      >
        {title}
      </h2>
      <p className="text-sm text-muted-foreground max-w-[400px]">{description}</p>
    </div>
  );
}
