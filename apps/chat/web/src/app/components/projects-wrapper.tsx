import { useState } from "react";
import { ProjectsPage } from "./projects-page";
import { CreateProjectModal } from "./create-project-modal";

export function ProjectsWrapper() {
  const [showCreateProject, setShowCreateProject] = useState(false);

  return (
    <>
      <ProjectsPage onNewProject={() => setShowCreateProject(true)} />
      <CreateProjectModal
        isOpen={showCreateProject}
        onClose={() => setShowCreateProject(false)}
        onCreate={(name, description) => {
          setShowCreateProject(false);
        }}
      />
    </>
  );
}
