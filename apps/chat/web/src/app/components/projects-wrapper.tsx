import { useState } from "react";
import { useNavigate } from "react-router";
import { ProjectsPage } from "./projects-page";
import { CreateProjectModal } from "./create-project-modal";
import { createProject } from "@/api/client";

export function ProjectsWrapper() {
  const navigate = useNavigate();
  const [showCreateProject, setShowCreateProject] = useState(false);

  const handleCreate = async (name: string, description: string) => {
    setShowCreateProject(false);
    try {
      const project = await createProject(name, description);
      navigate(`/projects/${project.id}`);
    } catch (err) {
      console.error("Failed to create project:", err);
    }
  };

  return (
    <>
      <ProjectsPage onNewProject={() => setShowCreateProject(true)} />
      <CreateProjectModal
        isOpen={showCreateProject}
        onClose={() => setShowCreateProject(false)}
        onCreate={handleCreate}
      />
    </>
  );
}
