const colorMap: Record<string, { bg: string; text: string }> = {
  ready:       { bg: "var(--badge-green-bg)", text: "var(--badge-green-text)" },
  running:     { bg: "var(--badge-green-bg)", text: "var(--badge-green-text)" },
  pending:     { bg: "var(--badge-yellow-bg)", text: "var(--badge-yellow-text)" },
  progressing: { bg: "var(--badge-blue-bg)", text: "var(--badge-blue-text)" },
  failed:      { bg: "var(--badge-red-bg)", text: "var(--badge-red-text)" },
  error:       { bg: "var(--badge-red-bg)", text: "var(--badge-red-text)" },
};

const defaultColor = { bg: "var(--badge-gray-bg)", text: "var(--badge-gray-text)" };

export default function StatusBadge({ status }: { status: string }) {
  const key = status.toLowerCase();
  const c = colorMap[key] || defaultColor;
  return (
    <span
      className="inline-flex items-center px-2 py-0.5 rounded-full text-xs font-medium"
      style={{ backgroundColor: c.bg, color: c.text }}
    >
      {status || "Unknown"}
    </span>
  );
}
