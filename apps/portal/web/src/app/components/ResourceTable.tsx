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
      <div className="text-center py-12 text-gray-500">
        {emptyMessage || "No resources found."}
      </div>
    );
  }

  return (
    <div className="overflow-x-auto bg-white rounded-lg border border-gray-200">
      <table className="min-w-full divide-y divide-gray-200">
        <thead className="bg-gray-50">
          <tr>
            {columns.map((col, i) => (
              <th key={i} className={`px-4 py-3 text-left text-xs font-medium text-gray-500 uppercase tracking-wider ${col.className || ""}`}>
                {col.header}
              </th>
            ))}
            {onDelete && <th className="px-4 py-3 text-right text-xs font-medium text-gray-500 uppercase">Actions</th>}
          </tr>
        </thead>
        <tbody className="divide-y divide-gray-200">
          {data.map((row) => (
            <tr key={getKey(row)} className="hover:bg-gray-50">
              {columns.map((col, i) => (
                <td key={i} className={`px-4 py-3 text-sm ${col.className || ""}`}>
                  {i === 0 && getLink ? (
                    <Link to={getLink(row)} className="text-blue-600 hover:text-blue-800 font-medium">
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
                    className="text-red-600 hover:text-red-800 text-sm font-medium"
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
