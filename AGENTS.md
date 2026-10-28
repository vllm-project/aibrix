# AIBrix

AIBrix is a Kubernetes-native platform for scalable GenAI inference infrastructure.

## Principles

- Understand the problem and define success before making a non-trivial change.
  Ground decisions in existing implementations, tests, and verified evidence.
- Make the smallest coherent change. Preserve unrelated work and existing
  conventions; avoid speculative abstractions and unrelated cleanup.
- Treat public contracts and compatibility as design constraints. Keep API
  definitions declarative and policy in the layer that owns it.
- Verify observable behavior at the narrowest useful level, then expand checks
  to match the change's scope. Report what ran, what passed, and what remains
  unverified.
- Keep documentation and generated artifacts consistent with the implementation.
  Review every generated or AI-assisted change; submit only what you can explain.
- Respect authorization boundaries. Do not push, rewrite history, change remote
  GitHub state, or send external messages without explicit authorization. Ask
  before upgrading dependencies or changing workflow policy under `.github/`.

## Task guides

Read the relevant guide before changing that area; use the sections needed for
this task. Rules in a deeper `AGENTS.md` take precedence for that directory.

- Before implementation or PRs: [Contribution policy](CONTRIBUTING.md#issue-first-policy).
- Go, APIs, controllers, and code generation:
  [Development guide](development/README.md#implementation-guidelines).
- Tests: [Test guide](test/README.md).
- Documentation and translations: [Documentation guide](docs/README.md#documentation-changes).
- Python runtime: [python/aibrix/AGENTS.md](python/aibrix/AGENTS.md).
