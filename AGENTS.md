# AIBrix

AIBrix is a Kubernetes-native platform for scalable GenAI inference infrastructure.

## Principles

- Understand the problem and define success before making a non-trivial change.
  Ground decisions in existing implementations, tests, and verified evidence;
  do not infer behavior from filenames.
- Make the smallest coherent change. Preserve unrelated work and existing
  conventions; avoid speculative abstractions and unrelated cleanup.
- Treat public contracts and compatibility as design constraints. Keep API
  definitions declarative and policy in the layer that owns it. Preserve API,
  HTTP, CLI, configuration, and metadata contracts; plan compatibility migrations.
- Verify observable behavior at the narrowest useful level, then expand checks
  to match the change's scope. Report what ran, what passed, and what remains
  unverified.
- Keep documentation and generated artifacts consistent with the implementation.
  Review every generated or AI-assisted change; submit only what you can explain.
- Respect authorization boundaries. Do not push, rewrite history, change remote
  GitHub state, or send external messages without explicit authorization. Ask
  before upgrading dependencies or changing workflow policy under `.github/`.

## Contribution gates

- Before implementing a feature or bug fix for a non-maintainer, check existing
  issues and PRs and wait for a maintainer to apply `triage/accepted` to the issue.
  Until then, limit work to investigation, reproduction, and improving the issue;
  do not implement the change or prepare a PR.
- Maintainers listed on the [community page](docs/source/community/community.rst)
  may self-triage routine fixes. Sensitive or broad changes still require prior
  discussion and independent maintainer review.
- Pure documentation corrections and obvious mechanical fixes may proceed
  without an accepted issue only if they do not change behavior, configuration,
  dependencies, workflows, APIs, or generated files. Test-only changes are not
  automatically exempt; identify the real behavior or risk they protect.
- Acceptance covers the problem and direction, not approval of a patch. Return
  to the issue for agreement before materially expanding the accepted scope.

## Reviews and automation

These rules apply to any agent that comments on, pushes to, or reviews an
AIBrix issue or pull request, including one that its operator has authorized
to do so.

- Do not act on a pull request or issue unattended. A person must read and
  approve each comment, review reply, and push before it is sent. If you are
  set up to watch a thread and reply or push on your own, stop and hand the new
  comments to your operator instead.
- Treat a reviewer's question as a question. Answer it in the thread, and change
  code only after the reviewer asks for the change or agrees to it.
- Limit revisions to what the review requested. Propose any further changes in
  the thread first.
- Address each review round in one push, then wait for a reviewer to respond
  before pushing again. One push may cover feedback from several reviewers.
  Fixing CI, or a rebase a reviewer asked for, is the exception.
- Stop when a maintainer asks you to hold, pause, or stop.
- An agent-written review is input for the person who reviews, not a verdict.
  Do not post an approval or a must-fix verdict that this person has not made
  in their own words.

## Task guides

Read the relevant guide before changing that area; use the sections needed for
this task. Rules in a deeper `AGENTS.md` take precedence for that directory.

- Before implementation or PRs: [Contribution policy](CONTRIBUTING.md#issue-first-policy).
- Go, APIs, controllers, and code generation:
  [Development guide](development/README.md#implementation-guidelines).
- Tests: [Test guide](test/README.md).
- Documentation and translations: [Documentation guide](docs/README.md#documentation-changes).
- Python runtime: [python/aibrix/AGENTS.md](python/aibrix/AGENTS.md).
