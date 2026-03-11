import { useState } from "react";
import { Navigate, useNavigate } from "react-router";
import { useAuth } from "@/app/context/auth-context";

export function LoginPage() {
  const { user, authMode, login, loading } = useAuth();
  const navigate = useNavigate();
  const [name, setName] = useState("");
  const [error, setError] = useState("");
  const [submitting, setSubmitting] = useState(false);

  // If auth is disabled or user is already logged in, go to home
  if (!loading && (authMode === "none" || user)) {
    return <Navigate to="/" replace />;
  }

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    const trimmed = name.trim();
    if (!trimmed) return;

    setError("");
    setSubmitting(true);
    try {
      await login(trimmed);
      navigate("/", { replace: true });
    } catch {
      setError("Something went wrong. Please try again.");
    } finally {
      setSubmitting(false);
    }
  };

  if (loading) {
    return (
      <div className="flex h-screen w-screen items-center justify-center bg-background">
        <div className="text-muted-foreground text-sm">Loading...</div>
      </div>
    );
  }

  return (
    <div className="flex h-screen w-screen items-center justify-center bg-background">
      <div className="w-full max-w-sm px-6">
        <div className="flex flex-col items-center mb-8">
          <span className="text-4xl mb-3">✺</span>
          <h1
            style={{
              fontFamily: "'Playfair Display', serif",
              fontSize: "1.8rem",
              fontWeight: 400,
              color: "var(--foreground)",
              opacity: 0.8,
            }}
          >
            AIBrix Chat
          </h1>
          <p className="text-sm text-muted-foreground mt-2">
            Enter your name to get started
          </p>
        </div>

        <form onSubmit={handleSubmit} className="space-y-4">
          <input
            type="text"
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder="Your name"
            autoFocus
            className="w-full px-4 py-3 rounded-xl bg-sidebar border border-white/10 text-foreground placeholder:text-muted-foreground focus:outline-none focus:ring-2 focus:ring-amber-500/40 text-sm"
          />

          {error && (
            <p className="text-sm text-red-400">{error}</p>
          )}

          <button
            type="submit"
            disabled={!name.trim() || submitting}
            className="w-full py-3 rounded-xl bg-[#c4a882] text-[#1a1714] font-medium text-sm hover:bg-[#d4b892] disabled:opacity-50 disabled:cursor-not-allowed transition-colors"
          >
            {submitting ? "Signing in..." : "Continue"}
          </button>
        </form>
      </div>
    </div>
  );
}
