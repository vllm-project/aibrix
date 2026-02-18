import { useState } from "react";
import { useNavigate } from "react-router";
import { HomePage } from "./home-page";
import { CreateProjectModal } from "./create-project-modal";

export function HomeWrapper() {
  const navigate = useNavigate();
  const [showCreateProject, setShowCreateProject] = useState(false);

  return (
    <>
      <HomePage
        userName="Test User"
        onStartNewProject={() => setShowCreateProject(true)}
      />
      <CreateProjectModal
        isOpen={showCreateProject}
        onClose={() => setShowCreateProject(false)}
        onCreate={(name, description) => {
          setShowCreateProject(false);
          navigate("/projects/new");
        }}
      />
    </>
  );
}