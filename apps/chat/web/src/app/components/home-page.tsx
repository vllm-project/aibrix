import { ChatInput } from "./chat-input";
import { QuickActions } from "./quick-actions";

interface HomePageProps {
  userName?: string;
  onStartNewProject?: () => void;
}

export function HomePage({ userName = "there", onStartNewProject }: HomePageProps) {
  const getGreeting = () => {
    const hour = new Date().getHours();
    if (hour < 12) return "Good morning";
    if (hour < 18) return "Good afternoon";
    return "Good evening";
  };

  return (
    <div className="flex-1 flex flex-col items-center justify-center px-4 pb-20">
      <div className="flex items-center gap-3 mb-10">
        <span className="text-3xl">✺</span>
        <h1
          style={{
            fontFamily: "'Playfair Display', serif",
            fontSize: "2.2rem",
            fontWeight: 400,
            color: "var(--foreground)",
            opacity: 0.8,
          }}
        >
          {getGreeting()}, {userName}
        </h1>
      </div>

      <ChatInput onStartNewProject={onStartNewProject} />

      <div className="mt-5">
        <QuickActions />
      </div>
    </div>
  );
}
