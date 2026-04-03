import { useEffect, useState } from "react";
import { Moon, Sun } from "lucide-react";

export default function ThemeToggle() {
  const [dark, setDark] = useState(() => {
    if (typeof window === "undefined") return false;
    const stored = localStorage.getItem("portal-theme");
    if (stored) return stored === "dark";
    return window.matchMedia("(prefers-color-scheme: dark)").matches;
  });

  useEffect(() => {
    document.documentElement.classList.toggle("dark", dark);
    localStorage.setItem("portal-theme", dark ? "dark" : "light");
  }, [dark]);

  return (
    <button
      onClick={() => setDark(!dark)}
      className="p-2 rounded-md transition-colors hover:bg-[var(--muted)]"
      title={dark ? "Switch to light mode" : "Switch to dark mode"}
    >
      {dark ? <Sun size={18} className="text-[var(--foreground)]" /> : <Moon size={18} className="text-[var(--foreground)]" />}
    </button>
  );
}
