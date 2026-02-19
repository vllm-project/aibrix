import React from "react";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";

function extractTextFromChildren(children: React.ReactNode): string {
  if (typeof children === "string") return children;
  if (Array.isArray(children))
    return children.map(extractTextFromChildren).join("");
  if (children && typeof children === "object" && "props" in children) {
    return extractTextFromChildren(
      (children as React.ReactElement).props.children
    );
  }
  return "";
}

interface MarkdownContentProps {
  content: string;
}

export function MarkdownContent({ content }: MarkdownContentProps) {
  return (
    <ReactMarkdown
      remarkPlugins={[remarkGfm]}
      components={{
        pre: ({ children }) => {
          const textContent = extractTextFromChildren(children);
          return (
            <div className="relative group my-3">
              <pre className="bg-black/30 rounded-lg p-4 overflow-x-auto text-sm">
                {children}
              </pre>
              <button
                onClick={() => navigator.clipboard.writeText(textContent)}
                className="absolute top-2 right-2 p-1.5 rounded-md bg-white/10 opacity-0 group-hover:opacity-100 transition-opacity text-foreground/60 hover:text-foreground hover:bg-white/20"
                title="Copy code"
              >
                <svg
                  xmlns="http://www.w3.org/2000/svg"
                  width="14"
                  height="14"
                  viewBox="0 0 24 24"
                  fill="none"
                  stroke="currentColor"
                  strokeWidth="2"
                  strokeLinecap="round"
                  strokeLinejoin="round"
                >
                  <rect width="14" height="14" x="8" y="8" rx="2" ry="2" />
                  <path d="M4 16c-1.1 0-2-.9-2-2V4c0-1.1.9-2 2-2h10c1.1 0 2 .9 2 2" />
                </svg>
              </button>
            </div>
          );
        },
        code: ({ className, children, ...props }) => {
          const isInline = !className;
          return isInline ? (
            <code
              className="bg-black/20 px-1.5 py-0.5 rounded text-sm"
              {...props}
            >
              {children}
            </code>
          ) : (
            <code className={className} {...props}>
              {children}
            </code>
          );
        },
        table: ({ children }) => (
          <div className="overflow-x-auto my-3">
            <table className="min-w-full border border-white/10 text-sm">
              {children}
            </table>
          </div>
        ),
        th: ({ children }) => (
          <th className="border border-white/10 px-3 py-1.5 text-left bg-white/5">
            {children}
          </th>
        ),
        td: ({ children }) => (
          <td className="border border-white/10 px-3 py-1.5">{children}</td>
        ),
        a: ({ href, children }) => (
          <a
            href={href}
            target="_blank"
            rel="noopener noreferrer"
            className="text-amber-400 hover:underline"
          >
            {children}
          </a>
        ),
        ul: ({ children }) => (
          <ul className="list-disc list-inside my-2 space-y-1">{children}</ul>
        ),
        ol: ({ children }) => (
          <ol className="list-decimal list-inside my-2 space-y-1">
            {children}
          </ol>
        ),
        blockquote: ({ children }) => (
          <blockquote className="border-l-2 border-amber-500/40 pl-4 my-3 text-foreground/70 italic">
            {children}
          </blockquote>
        ),
        h1: ({ children }) => (
          <h1 className="text-xl font-semibold mt-4 mb-2">{children}</h1>
        ),
        h2: ({ children }) => (
          <h2 className="text-lg font-semibold mt-3 mb-2">{children}</h2>
        ),
        h3: ({ children }) => (
          <h3 className="text-base font-semibold mt-3 mb-1">{children}</h3>
        ),
        p: ({ children }) => <p className="my-1.5">{children}</p>,
        hr: () => <hr className="my-4 border-white/10" />,
      }}
    >
      {content}
    </ReactMarkdown>
  );
}
