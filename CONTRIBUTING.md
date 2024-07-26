## About AIBricks

AIBricks is a cloud-native solution designed to deploy, manage, and scale large language models (LLMs) efficiently, catering specifically to enterprise needs. For more information about AIBricks and its features, see [AIBricks Doc](https://github.com/aibrix/aibrix/tree/main/docs/tutorial) .

## Important Links and Resources

Here are some essential resources for anyone interested in AIBricks:

- **Documentation and Tutorials**: [View Tutorials](https://github.com/aibrix/aibrix/tree/main/docs/tutorial)
- **Issue Tracker**: [View Issues](https://github.com/aibrix/aibrix/issues)
- **Project Roadmap**: TODO

Additional resources for contributors:

- **Kubebuilder Tutorial**: [Learn Kubebuilder](https://book.kubebuilder.io/) - A comprehensive, step-by-step guide to developing with Kubebuilder.
- **Kubernetes Documentation**: [Explore Kubernetes Docs](https://kubernetes.io/docs/home/) - Detailed explanations of Kubernetes concepts.

## Prerequisites

Before you start, ensure the following are installed on your system:

- Go version 1.21.0 or higher
- Docker version 17.03 or higher
- kubectl version 1.11.3 or higher
- Access to a Kubernetes cluster, version 1.11.3 or higher

For Mac developers, it is recommended to use [Docker Desktop](https://www.docker.com/products/docker-desktop/) which includes built-in support for Kubernetes.

Alternatively, you can use [Kind](https://kind.sigs.k8s.io/) or [Minikube](https://minikube.sigs.k8s.io/docs/start/) to quickly set up a localized, small-scale Kubernetes cluster for development purposes.

## Start your Coding

### Fork and Clone the Repository

- Fork the AIBricks repository on GitHub.

- Clone your fork locally:

  ```sh
  git clone https://github.com/aibrix/aibrix.git
  cd aibrix
  ```

### Build and Deploy

Navigate to the cloned directory and run:

```  
make build
```

Build and push your image to the location specified by `IMG`:

```sh
make docker-build docker-push IMG=<some-registry>/aibrix:tag
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

Deploy the Manager to your cluster using the `IMG` environment variable to specify the image:

```sh
make deploy IMG=<some-registry>/aibrix:tag
```

If you prefer local testing and do not want to push the image to a remote DockerHub, you can specify any name, such as:

```sh
make deploy IMG=example.com/aibrix:v1
```

> **NOTE**: If you encounter RBAC errors, you may need to grant yourself cluster-admin privileges or be logged in as admin.

### Start the Manager through an IDE

Alternatively, for better debugging, you can start AIBricks in debug mode using an IDE like [Goland](https://www.jetbrains.com/go/) or [VSCode](https://code.visualstudio.com/). The main entry point of the project is located at `cmd/main.go`.

### Create AIBricks Sample Instances

Apply the samples from the `config/samples` directory. These samples create a list of AIBricks CRD instances, such as `PodAutoscaler` and `ModelAdapter`:

```sh
kubectl apply -k config/samples/
```

> **NOTE**: Ensure that the samples have default values for testing purposes.

### Run Demo App to Verify AIBricks Functionality

Start a demo application on Kubernetes and verify the functionality and effectiveness of the AIBricks suite. We provide some demo application YAMLs at `TODO: link to demo application YAMLs`.

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

To test the build, run:

  ```
  make test
  ```

## Project Distribution

Following are the steps to build the installer and distribute this project to users.

1. Build the installer for the image built and published in the registry:

```sh
make build-installer IMG=<some-registry>/aibrix:tag
```

NOTE: The makefile target mentioned above generates an 'install.yaml' file in the dist directory. This file contains all the resources built with Kustomize, which are necessary to install this project without its dependencies.

1. Using the installer: Users can just run `kubectl apply -f <URL for YAML BUNDLE>` to install the project, i.e.:

```
kubectl apply -f https://raw.githubusercontent.com/<org>/aibrix/<tag or branch>/dist/install.yaml
```

## How to Contribute

### 0. Coding Guidelines

- **Style Guide**: The [Go style](https://google.github.io/styleguide/go/) guide is a good standard to follow.
- **Commenting Code**: Write clear comments that explain the "why" not just the "how".

### 1. Understanding the Repository Structure

You can refer to the [Design Doc](https://github.com/aibrix/aibrix/tree/main/docs/tutorial) to quickly understand the layout of the AIBricks directory structure.

### 2. Picking an Issue

Start by picking an [issue](https://github.com/aibrix/aibrix/issues) tagged with "good first issue" to get familiar with the project. Claim the issue by commenting to avoid duplicate efforts.

### 3. Submitting Codes

- **Creating Branches**: Use the following naming convention: `/<YOUR_NAME>/[feat|patch|bug_fix] description`
- **Making Commits**: Write detailed commit messages that explain your changes.
- **Pull Requests**:
  - Create a pull request on [PR Page](https://github.com/aibrix/aibrix/pulls) against the master branch.
  - Use the provided PR template to describe your changes.
- **Code Review Process**: Your PR will be reviewed by related contributors. Address feedback to improve and finalize your contribution.

## Advanced Contributions

- **Feature Proposals and Bug Reporting**: Report bugs or propose new features by creating an issue on our GitHub page. Provide as much detail as possible to facilitate discussions.
- **Contributing to Documentation**: Help improve our documentation by proposing changes to issue list. Our documentation is at [AIBricks Doc](https://github.com/aibrix/aibrix/tree/main/docs/tutorial)
- **Organizing Community Events**: If you are interested in organizing meetups or webinars, contact our community manager (TODO: contact information).

## Community and Communication

Join our community through various channels:

- **Slack Channl**: TODO
- **Wechat User Groups**: TODO
- **Email**: TODO
- **Forums**: TODO

## FAQ

TODO: Frequently asked questions and answers for new contributors.

## Contact

- **Maintainers**: TODO
