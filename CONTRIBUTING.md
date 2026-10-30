## About AIBrix

For instructions on building, installing, deploying, and testing this project, 
please see the [Development Guide](development/README.md) in the `development` directory.

## How to Contribute

### Scope and review

- State the intended scope and success criteria before a non-trivial change.
- Keep each change focused on one issue or coherent behavior change. Do not
  combine unrelated refactors, formatting, or generated output.
- Discuss large design changes before implementation and record the design in
  the appropriate documentation area.
- Update user or developer documentation when behavior, configuration, APIs,
  deployment, or CLI usage changes. Follow the [documentation guide](docs/README.md).
- Review every generated or AI-assisted change yourself; submit only changes
  you can explain and verify. Agent-generated analysis or review does not
  constitute maintainer approval; the human contributor remains accountable.

### 0. Coding Guidelines

Follow the [implementation guidelines](development/README.md#implementation-guidelines)
for Go conventions, API compatibility, and generated artifacts. The
[Go style guide](https://google.github.io/styleguide/go/) is an additional reference.

### 1. Understanding the Repository Structure

Start with the [repository layout](development/README.md#repository-layout).
Feature walkthroughs are available in [development tutorials](development/tutorials/).

### 2. Picking an Issue

Start by picking an [issue](https://github.com/vllm-project/aibrix/issues) tagged with "good first issue" to get familiar with the project. Claim the issue by commenting to avoid duplicate efforts.

#### Issue-first policy

Contributors who are not listed as project maintainers on the
[community page](docs/source/community/community.rst) must obtain maintainer
agreement before implementing a feature or bug fix, except for the documentation
and mechanical corrections described below. Start by searching open issues and
pull requests. Open an issue if the work is not already tracked. Whether the
issue is new or existing, wait for a maintainer to apply `triage/accepted` before
starting implementation or preparing a pull request. Until then, limit work to
investigation, reproduction, and improving the issue.

A useful issue establishes why AIBrix should maintain the change. Strong
examples include:

- a problem observed while running AIBrix in production;
- an RFC for an upstream or downstream integration;
- a defect with repeatable steps and observable results; or
- an error demonstrated by a clear logic or interface-contract violation.

Include the affected version and environment, actual and expected behavior,
reproduction evidence, affected users or use cases, and a proposed direction.
Maintainer acceptance means that the problem is worth solving and that the
direction is reasonable. It does not reserve the issue or pre-approve a
particular patch.

Maintainers may self-triage routine fixes and may open an issue and pull request
together because they remain responsible for the resulting quality and
maintenance. Sensitive or broad changes still require prior discussion and
independent maintainer review. This includes changes to APIs, CRDs, defaults,
environment variables, admission behavior, autoscaling algorithms, security
boundaries, or CI and build policy.

Pure documentation corrections and obvious mechanical fixes may be submitted
without an accepted issue when they do not change behavior, configuration,
dependencies, workflows, APIs, or generated files. A test-only change is not
automatically exempt: it must identify the real behavior or risk it protects.

### 3. Submitting Codes

- **Creating Branches**: Use the following naming convention: `/<YOUR_NAME>/[feat|patch|bug_fix] description`
- **Making Commits**: Write detailed commit messages that explain your changes.
- **Pull Requests**:
  - Create a pull request on [PR Page](https://github.com/vllm-project/aibrix/pulls) against the main branch.
  - Use the [PR template](.github/PULL_REQUEST_TEMPLATE.md) to describe your changes.
  - Link the accepted issue with `Fixes #<issue-number>` unless the change
    qualifies for the small-change exception above.
  - Record affected versions or environments, reproduction steps, root cause,
    and user-visible or operational impact.
  - For a bug fix, describe how to reproduce the failure and, when practical,
    add a regression test that fails for the expected reason before the fix and
    passes afterward.
  - Keep the implementation within the direction and scope accepted in the
    issue. Return to the issue for agreement before expanding that scope.
  - Test or validate the change before submitting the PR, following the
    [verification guidance](test/README.md#verification-principles). Report the
    commands or manual steps performed and their results. Explain any relevant
    checks that could not be completed and what remains unverified. Never
    report an unexecuted check as passing.
- **Code Review Process**: Your PR will be reviewed by related contributors. Address feedback to improve and finalize your contribution.

Maintainers may close a pull request without completing a code-level review
when it bypasses the issue-first policy, duplicates existing work, lacks the
required reproduction or validation evidence after feedback, or materially
exceeds the accepted issue scope. Maintainers may also close low-value
mechanical or generated changes whose maintenance cost exceeds their benefit.
After satisfying the requirements, the contributor may submit a new pull
request.

## Advanced Contributions

- **Feature Proposals and Bug Reporting**: Report bugs or propose new features by creating an issue on our GitHub page. Provide as much detail as possible to facilitate discussions.
- **Contributing to Documentation**: Help improve our documentation by proposing changes to issue list. Our documentation is at [AIBrix Doc](https://github.com/vllm-project/aibrix/tree/main/development/tutorials)
- **Organizing Community Events**: If you are interested in organizing meetups or webinars, contact us through our [Slack channel](https://vllm-dev.slack.com/archives/C08EQ883CSV) or [GitHub Discussions](https://github.com/vllm-project/aibrix/discussions).

## Community and Communication

Join our community through various channels:

- **Slack Channel**: [#aibrix on vLLM Slack](https://vllm-dev.slack.com/archives/C08EQ883CSV)
- **Forums**: [GitHub Discussions](https://github.com/vllm-project/aibrix/discussions)
- **Issues**: [GitHub Issues](https://github.com/vllm-project/aibrix/issues)

## FAQ

For frequently asked questions, check our [GitHub Discussions](https://github.com/vllm-project/aibrix/discussions) or ask in the [Slack channel](https://vllm-dev.slack.com/archives/C08EQ883CSV).

## Contact

- **GitHub Issues**: [Report bugs or request features](https://github.com/vllm-project/aibrix/issues)
- **GitHub Discussions**: [Ask questions and share ideas](https://github.com/vllm-project/aibrix/discussions)
- **Slack**: [#aibrix channel](https://vllm-dev.slack.com/archives/C08EQ883CSV)
