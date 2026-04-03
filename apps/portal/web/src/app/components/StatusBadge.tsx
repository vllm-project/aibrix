const colors: Record<string, string> = {
  ready: "bg-green-100 text-green-800",
  running: "bg-green-100 text-green-800",
  pending: "bg-yellow-100 text-yellow-800",
  progressing: "bg-blue-100 text-blue-800",
  failed: "bg-red-100 text-red-800",
  error: "bg-red-100 text-red-800",
  "": "bg-gray-100 text-gray-600",
};

export default function StatusBadge({ status }: { status: string }) {
  const key = status.toLowerCase();
  const cls = colors[key] || colors[""];
  return (
    <span className={`inline-flex items-center px-2 py-0.5 rounded-full text-xs font-medium ${cls}`}>
      {status || "Unknown"}
    </span>
  );
}
