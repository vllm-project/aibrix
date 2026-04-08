"""In-memory project store."""

from __future__ import annotations

from datetime import datetime, timezone

from models.schemas import Project, ProjectSummary


class ProjectStore:
    """Thread-safe in-memory project storage."""

    def __init__(self) -> None:
        self._projects: dict[str, Project] = {}
        self._seed()

    def _seed(self) -> None:
        """Create two example projects."""
        examples = [
            Project(
                name="AIBrix",
                description="",
                instructions=(
                    "You are working inside the **AIBrix** codebase. "
                    "AIBrix is an open-source, Kubernetes-native LLM inference "
                    "infrastructure stack."
                ),
            ),
            Project(
                name="How to use AIBrix Chat",
                description=("An example project that also doubles as a how-to guide for using AIBrix Chat."),
                instructions=("This is an example project that shows how to use AIBrix Chat effectively."),
            ),
        ]
        for p in examples:
            self._projects[p.id] = p

    def create(self, name: str, description: str = "", user_id: str = "") -> Project:
        project = Project(name=name, description=description, user_id=user_id)
        self._projects[project.id] = project
        return project

    def get(self, project_id: str) -> Project | None:
        return self._projects.get(project_id)

    def list_all(self, user_id: str = "") -> list[ProjectSummary]:
        summaries = []
        for p in self._projects.values():
            if user_id and p.user_id != user_id:
                continue
            summaries.append(
                ProjectSummary(
                    id=p.id,
                    name=p.name,
                    description=p.description,
                    updated_at=p.updated_at,
                )
            )
        summaries.sort(key=lambda s: s.updated_at, reverse=True)
        return summaries

    def update(
        self,
        project_id: str,
        name: str | None = None,
        description: str | None = None,
        instructions: str | None = None,
    ) -> Project | None:
        project = self._projects.get(project_id)
        if project is None:
            return None
        if name is not None:
            project.name = name
        if description is not None:
            project.description = description
        if instructions is not None:
            project.instructions = instructions
        project.updated_at = datetime.now(timezone.utc).isoformat()
        return project

    def delete(self, project_id: str) -> bool:
        return self._projects.pop(project_id, None) is not None


# Singleton store
project_store = ProjectStore()
