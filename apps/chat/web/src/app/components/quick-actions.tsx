import { Pencil, GraduationCap, Code, LayoutGrid, Sparkles, Image as ImageIcon } from "lucide-react";
import { useNavigate } from "react-router";

const actions = [
  { icon: Pencil, label: "Write" },
  { icon: GraduationCap, label: "Learn" },
  { icon: Code, label: "Code" },
  { icon: ImageIcon, label: "AI Creation", path: "/ai-creation" },
  { icon: Sparkles, label: "AI's choice" },
];

export function QuickActions() {
  const navigate = useNavigate();

  return (
    <div className="flex items-center justify-center gap-2 flex-wrap">
      {actions.map((action) => (
        <button
          key={action.label}
          onClick={() => {
            if ("path" in action && action.path) navigate(action.path);
          }}
          className={`flex items-center gap-1.5 px-3.5 py-1.5 rounded-full border border-border hover:bg-accent text-sm text-foreground/70 hover:text-foreground transition-colors ${
            "path" in action && action.path ? "border-amber-500/30 hover:border-amber-500/50" : ""
          }`}
        >
          <action.icon size={14} />
          <span>{action.label}</span>
        </button>
      ))}
    </div>
  );
}