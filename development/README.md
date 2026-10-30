# Development

Before changing code, read the [implementation guidelines](#implementation-guidelines)
for repository layout, API compatibility, coding conventions, and generation
requirements. For contribution gates and submission requirements, see
[CONTRIBUTING.md](../CONTRIBUTING.md#issue-first-policy).

## Important Links and Resources

Here are some essential resources for anyone interested in AIBrix:

- **Documentation and Tutorials**: [View Tutorials](https://github.com/vllm-project/aibrix/tree/main/development/tutorials)
- **Issue Tracker**: [View Issues](https://github.com/vllm-project/aibrix/issues)
- **Project Roadmap**: TODO

Additional resources for contributors:

- **Kubebuilder Tutorial**: [Learn Kubebuilder](https://book.kubebuilder.io/) - A comprehensive, step-by-step guide to developing with Kubebuilder.
- **Kubernetes Documentation**: [Explore Kubernetes Docs](https://kubernetes.io/docs/home/) - Detailed explanations of Kubernetes concepts.

## Prerequisites

Before you start, ensure the following are installed on your system:

- Go version 1.21.0 or higher
- Docker version 26.x or higher
- kubectl version 1.25.x or higher
- Access to a Kubernetes cluster, version 1.25.x or higher

For Mac developers, it is recommended to use [Docker Desktop](https://www.docker.com/products/docker-desktop/) which includes built-in support for Kubernetes.

Alternatively, you can use [Kind](https://kind.sigs.k8s.io/) or [Minikube](https://minikube.sigs.k8s.io/docs/start/) to quickly set up a localized, small-scale Kubernetes cluster for development purposes.

## Start your Coding

### Fork and Clone the Repository

- Fork the AIBrix repository on GitHub.

- Clone your fork locally:

  ```sh
  git clone https://github.com/vllm-project/aibrix.git
  cd aibrix
  ```

### Build and Deploy

Navigate to the cloned directory and run:

```
make build
```

Build and push your images to the registry location specified by `AIBRIX_CONTAINER_REGISTRY_NAMESPACE`:

```sh
make docker-build-all docker-push-all AIBRIX_CONTAINER_REGISTRY_NAMESPACE=<some-registry>
```

> **NOTE:** This image ought to be published in the personal registry you specified.
And it is required to have access to pull the image from the working environment.
Make sure you have the proper permission to the registry if the above commands don’t work.

Install the CRDs into the cluster:

```sh
make install
```

> **NOTE**: once you modify the project code, like change the api definition, you need to redo this step. Some auxiliary code will be auto-generated and the installed resources on K8S will be refreshed.

### Deploy the Manager to Kubernetes

Deploy the Manager to your cluster using the `AIBRIX_CONTAINER_REGISTRY_NAMESPACE` environment variable to specify the image:

```sh
make deploy AIBRIX_CONTAINER_REGISTRY_NAMESPACE=<some-registry>
```

If you prefer local testing and do not want to push the image to a remote DockerHub, you can specify any name, such as:

```sh
make deploy AIBRIX_CONTAINER_REGISTRY_NAMESPACE=example.com
```

> **NOTE**: If you encounter RBAC errors, you may need to grant yourself cluster-admin privileges or be logged in as admin.

### Start the Manager through an IDE

Alternatively, for better debugging, you can start AIBrix in debug mode using an IDE like [Goland](https://www.jetbrains.com/go/) or [VSCode](https://code.visualstudio.com/). The main entry point of the project is located at `cmd/controllers/main.go`.

### Create AIBrix Sample Instances

Apply the samples from the `config/samples` directory. These samples create a list of AIBrix CRD instances, such as `PodAutoscaler` and `ModelAdapter`:

```sh
kubectl apply -k config/samples/
```

> **NOTE**: Ensure that the samples have default values for testing purposes.

### Run Demo App to Verify AIBrix Functionality

Start a demo application on Kubernetes and verify the functionality and effectiveness of the AIBrix suite. We provide some demo application YAMLs at `TODO: link to demo application YAMLs`.

### Uninstall and Undeploy

To remove the deployed instances (CRs) from your cluster:

```sh
kubectl delete -k config/samples/
```

To remove the Custom Resource Definitions (CRDs) from the cluster:

```sh
make uninstall
```

To undeploy the controller from the cluster:

```sh
make undeploy
```

## Run Test

See the [test guide](../test/README.md) for test selection, commands, and
cluster setup.

## Project Distribution

Following are the steps to build the installer and distribute this project to users.

1. Build the installer for the image built and published in the registry:

```sh
make build-installer AIBRIX_CONTAINER_REGISTRY_NAMESPACE=<some-registry>
```

NOTE: The makefile target mentioned above generates an 'install.yaml' file in the dist directory. This file contains all the resources built with Kustomize, which are necessary to install this project without its dependencies.

1. Using the installer: Users can just run `kubectl apply -f <URL for YAML BUNDLE>` to install the project, i.e.:

```
kubectl apply -f https://raw.githubusercontent.com/<org>/aibrix/<tag or branch>/dist/install.yaml
```

## Implementation Guidelines

### Repository layout

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

### API and compatibility surfaces

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

### Coding conventions

- Follow existing Go package structure and standard Go style. Format Go changes
  with `gofmt` or `make fmt`, and run `make lint` before submitting the PR.
- Put reusable implementation in `pkg/`; keep `cmd/` entrypoints thin.
- Use table-driven tests where they match the surrounding package style.
- Follow the [test guidance](../test/README.md#verification-principles) for
  behavior changes and regression coverage.
- Comments should explain non-obvious reasons or invariants, not restate code.
- Preserve existing logging, error handling, retry, timeout, and context
  propagation patterns unless the change specifically addresses them.
- For Python changes, follow the more specific rules in
  [`python/aibrix/AGENTS.md`](../python/aibrix/AGENTS.md) and run checks from that
  subtree as documented there.

### Build and verification

Run commands from the repository root. Test selection and execution are covered
in the [test guide](../test/README.md#verification-principles).

| Command | Purpose |
|---------|---------|
| `make fmt` | Format Go code |
| `make vet` | Run `go vet ./...` |
| `make generate` | Regenerate Go and Kubernetes generated artifacts |
| `make manifests` | Regenerate webhook, RBAC, and CRD manifests |
| `make verify` | Verify generated code and CRD synchronization |
| `make lint` | Run Go linting |
| `make lint-all` | Run license and Go lint checks |

When API types, controller-gen markers, or CRDs change, run the applicable
generation and verification targets and include the resulting generated files
in the change. Do not hand-edit generated files unless the generator explicitly
requires it.

See the [test guide](../test/README.md) for test setup and execution, and the
[documentation guide](../docs/README.md) for translation and HTML checks.
