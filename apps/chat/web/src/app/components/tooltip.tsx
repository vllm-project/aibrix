import { useState, useRef, useEffect, type ReactNode } from "react";

interface TooltipProps {
  content: string;
  children: ReactNode;
  position?: "top" | "bottom";
  delay?: number;
}

export function Tooltip({
  content,
  children,
  position = "top",
  delay = 400,
}: TooltipProps) {
  const [visible, setVisible] = useState(false);
  const timeoutRef = useRef<ReturnType<typeof setTimeout> | null>(null);

  const show = () => {
    timeoutRef.current = setTimeout(() => setVisible(true), delay);
  };

  const hide = () => {
    if (timeoutRef.current) clearTimeout(timeoutRef.current);
    setVisible(false);
  };

  useEffect(() => {
    return () => {
      if (timeoutRef.current) clearTimeout(timeoutRef.current);
    };
  }, []);

  return (
    <div className="relative inline-flex" onMouseEnter={show} onMouseLeave={hide} onClick={hide}>
      {children}
      {visible && (
        <div
          className={`absolute left-1/2 -translate-x-1/2 z-[300] px-2.5 py-1 rounded-lg bg-foreground text-background text-xs whitespace-nowrap pointer-events-none ${
            position === "top" ? "bottom-full mb-2" : "top-full mt-2"
          }`}
        >
          {content}
        </div>
      )}
    </div>
  );
}