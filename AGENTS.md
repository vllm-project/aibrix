# AIBrix

AIBrix is a Kubernetes-native platform for building scalable GenAI inference
infrastructure. This repository contains Go controllers, gateway plugins,
custom resources, deployment manifests, tests, and Python runtime components.
This file gives coding agents the repository-specific context and safety rules
needed to make focused changes.

## Scope and repository layout

| Path | Role |
|------|------|
| `api/` | Kubernetes API types and CRD definitions |
| `pkg/controller/` | Kubernetes controllers and reconciliation logic |
| `pkg/plugins/` | Gateway plugins and request processing |
| `pkg/` | Shared Go libraries, clients, caches, metrics, and utilities |
| `cmd/` | Go binary entrypoints |
| `config/` | Kustomize configuration, CRDs, RBAC, and deployment manifests |
| `test/integration/` | Ginkgo-based integration tests |
| `test/e2e/` | Kind and cluster-level end-to-end tests |
| `hack/` | Code generation, verification, and CI scripts |
| `python/aibrix/` | Python runtime, downloader, batch, metadata, and optimizer |
| `python/aibrix_kvcache/` | Python distributed KV-cache components |
| `apps/` | Application services and the console |
| `docs/` | Project documentation and generated documentation assets |
| `samples/` | Example deployments and feature samples |

Rules in a deeper `AGENTS.md` take precedence for files under that directory.
The Python runtime has additional guidance in
[`python/aibrix/AGENTS.md`](python/aibrix/AGENTS.md).

## Working rules

- State the intended scope and success criteria before making a non-trivial
  change.
- Read the closest existing implementation and its tests before introducing a
  new pattern.
- Keep changes focused. Do not combine unrelated refactors, formatting, or
  generated output with a feature or bug fix.
- Verify behavior from code and tests rather than inferring it from filenames.
- Preserve unrelated user changes in the working tree.
- Do not push branches, rewrite history, modify remote GitHub state, or send
  external messages without explicit authorization for that action.
- Ask before upgrading dependencies or changing `.github/` workflow policy.

## API and compatibility surfaces

Treat the following as compatibility surfaces:

- Kubernetes API types, CRD schemas, JSON/YAML tags, defaults, validation,
  list semantics, subresources, and printer columns.
- Public HTTP endpoints, request and response fields, headers, status codes,
  and gateway plugin contracts.
- CLI flags, configuration keys, environment variables, labels, and
  annotations used by deployment or controller logic.

Do not rename, remove, or repurpose an existing field or key without an
explicit migration and compatibility plan. Preserve pointer and `omitempty`
choices when absent, zero, and explicit values have different meanings.
Keep status fields observational; status transitions must remain consistent
with every controller that writes or consumes them.

Keep API types declarative. Admission behavior belongs in webhooks,
reconciliation policy belongs in controllers, and transport compatibility
belongs in the relevant API or gateway layer.

## Build and verification

Run commands from the repository root. Start with the narrowest relevant check
and run broader checks for changes that cross package or language boundaries.

| Command | Purpose |
|---------|---------|
| `make fmt` | Format Go code |
| `make vet` | Run `go vet ./...` |
| `make generate` | Regenerate Go and Kubernetes generated artifacts |
| `make manifests` | Regenerate webhook, RBAC, and CRD manifests |
| `make verify` | Verify generated code and CRD synchronization |
| `make lint` | Run Go linting |
| `make lint-all` | Run license and Go lint checks |
| `make test` | Run Go unit tests, excluding integration and E2E packages |
| `make test-race-condition` | Run Go unit tests with the race detector |
| `make test-integration` | Run integration tests |
| `make python-ci` | Run Python Ruff, mypy, and tests for `python/aibrix` |
| `make test-e2e` | Run Kind-based end-to-end tests |

For a Go package, a focused check such as `go test ./pkg/<package>/...` is
appropriate while iterating. For cluster-level behavior, use the existing
helpers and test organization under `test/e2e/`; do not replace polling with
arbitrary sleeps.

When API types, controller-gen markers, or CRDs change, run the applicable
generation and verification targets and include the resulting generated files
in the change. Do not hand-edit generated files unless the generator explicitly
requires it.

## Coding conventions

- Follow existing Go package structure and standard Go style; use `gofmt` or
  `make fmt`.
- Put reusable implementation in `pkg/`; keep `cmd/` entrypoints thin.
- Use table-driven tests where they match the surrounding package style.
- Add or update tests for behavior changes, including failure and compatibility
  cases when applicable.
- Comments should explain non-obvious reasons or invariants, not restate code.
- Preserve existing logging, error handling, retry, timeout, and context
  propagation patterns unless the change specifically addresses them.
- For Python changes, follow the more specific rules in
  [`python/aibrix/AGENTS.md`](python/aibrix/AGENTS.md) and run checks from that
  subtree as documented there.

## Pull requests and documentation

- Keep a PR focused on one issue or coherent behavior change.
- Use the repository [PR template](.github/PULL_REQUEST_TEMPLATE.md), link the
  relevant issue, and describe the tests actually run.
- Update user or developer documentation when behavior, configuration, API,
  deployment, or CLI usage changes.
- Large design changes should be discussed before implementation and recorded
  in the appropriate documentation area.
- Review every generated or AI-assisted change yourself; do not submit code
  that you cannot explain and verify.

## Further reading

- [Contributing guide](CONTRIBUTING.md)
- [Development guide](development/README.md)
- [Test guide](test/README.md)
- [Project documentation](docs/README.md)
