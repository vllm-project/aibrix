import { Link } from "react-router";

export interface Column<T> {
  header: string;
  accessor: (row: T) => React.ReactNode;
  className?: string;
}

interface Props<T> {
  columns: Column<T>[];
  data: T[];
  getKey: (row: T) => string;
  getLink?: (row: T) => string;
  onDelete?: (row: T) => void;
  emptyMessage?: string;
}

export default function ResourceTable<T>({ columns, data, getKey, getLink, onDelete, emptyMessage }: Props<T>) {
  if (data.length === 0) {
    return (
      <div className="text-center py-12 text-[var(--muted-foreground)]">
        {emptyMessage || "No resources found."}
      </div>
    );
  }

  return (
    <div className="overflow-x-auto bg-[var(--card)] rounded-lg border border-[var(--table-border)]">
      <table className="min-w-full divide-y divide-[var(--table-border)]">
        <thead style={{ backgroundColor: "var(--table-header-bg)" }}>
          <tr>
            {columns.map((col, i) => (
              <th key={i} className={`px-4 py-3 text-left text-xs font-medium text-[var(--muted-foreground)] uppercase tracking-wider ${col.className || ""}`}>
                {col.header}
              </th>
            ))}
            {onDelete && <th className="px-4 py-3 text-right text-xs font-medium text-[var(--muted-foreground)] uppercase">Actions</th>}
          </tr>
        </thead>
        <tbody className="divide-y divide-[var(--table-border)]">
          {data.map((row) => (
            <tr key={getKey(row)} className="hover:bg-[var(--table-hover)] transition-colors">
              {columns.map((col, i) => (
                <td key={i} className={`px-4 py-3 text-sm text-[var(--foreground)] ${col.className || ""}`}>
                  {i === 0 && getLink ? (
                    <Link to={getLink(row)} className="text-[var(--primary)] hover:underline font-medium">
                      {col.accessor(row)}
                    </Link>
                  ) : (
                    col.accessor(row)
                  )}
                </td>
              ))}
              {onDelete && (
                <td className="px-4 py-3 text-right">
                  <button
                    onClick={() => onDelete(row)}
                    className="text-[var(--destructive)] hover:underline text-sm font-medium"
                  >
                    Delete
                  </button>
                </td>
              )}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
