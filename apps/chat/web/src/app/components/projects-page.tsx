import { useState } from "react";
import { useNavigate } from "react-router";
import { Search, Plus, ChevronDown } from "lucide-react";

interface Project {
  id: string;
  name: string;
  description: string;
  updatedAt: string;
  isExample?: boolean;
}

const initialProjects: Project[] = [
  {
    id: "1",
    name: "AIBrix",
    description: "",
    updatedAt: "Updated 9 minutes ago",
  },
  {
    id: "2",
    name: "How to use AIBrix Chat",
    description:
      "An example project that also doubles as a how-to guide for using AIBrix Chat. Chat with it to learn more about how to get the most out of chatting with AI!",
    updatedAt: "Updated 6 months ago",
    isExample: true,
  },
];

interface ProjectsPageProps {
  onNewProject: () => void;
  projects?: Project[];
}

export function ProjectsPage({ onNewProject, projects }: ProjectsPageProps) {
  const navigate = useNavigate();
  const [searchQuery, setSearchQuery] = useState("");
  const [sortBy, setSortBy] = useState("Activity");

  const allProjects = projects || initialProjects;
  const filtered = allProjects.filter((p) =>
    p.name.toLowerCase().includes(searchQuery.toLowerCase())
  );

  return (
    <div className="flex-1 overflow-y-auto px-8 py-8">
      <div className="max-w-[900px] mx-auto">
        <div className="flex items-center justify-between mb-6">
          <h1
            style={{
              fontFamily: "'Playfair Display', serif",
              fontSize: "1.6rem",
              fontWeight: 500,
            }}
          >
            Projects
          </h1>
          <button
            onClick={onNewProject}
            className="flex items-center gap-1.5 px-4 py-2 rounded-lg border border-border hover:bg-accent text-sm transition-colors"
          >
            <Plus size={14} />
            New project
          </button>
        </div>

        <div className="relative mb-4">
          <Search
            size={16}
            className="absolute left-3.5 top-1/2 -translate-y-1/2 text-foreground/40"
          />
          <input
            type="text"
            value={searchQuery}
            onChange={(e) => setSearchQuery(e.target.value)}
            placeholder="Search projects..."
            className="w-full pl-10 pr-4 py-2.5 bg-card border border-border rounded-xl text-sm text-foreground placeholder-foreground/30 outline-none focus:border-foreground/20 transition-colors"
          />
        </div>

        <div className="flex items-center justify-end gap-2 mb-4">
          <span className="text-xs text-muted-foreground">Sort by</span>
          <button className="flex items-center gap-1 text-xs text-foreground/70 hover:text-foreground px-2 py-1 rounded border border-border">
            {sortBy}
            <ChevronDown size={12} />
          </button>
        </div>

        <div className="grid grid-cols-1 md:grid-cols-2 gap-4">
          {filtered.map((project) => (
            <button
              key={project.id}
              onClick={() => navigate(`/projects/${project.id}`)}
              className="text-left p-5 bg-card border border-border rounded-xl hover:border-foreground/15 transition-colors group"
            >
              <div className="flex items-start gap-2 mb-2">
                <h3 className="text-sm">{project.name}</h3>
                {project.isExample && (
                  <span className="text-[10px] px-1.5 py-0.5 rounded bg-accent text-muted-foreground whitespace-nowrap">
                    Example project
                  </span>
                )}
              </div>
              {project.description && (
                <p className="text-xs text-muted-foreground mb-3 line-clamp-2">
                  {project.description}
                </p>
              )}
              <div className="flex-1" />
              <p className="text-xs text-muted-foreground/60 mt-auto pt-4">
                {project.updatedAt}
              </p>
            </button>
          ))}
        </div>
      </div>
    </div>
  );
}
