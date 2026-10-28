# Agent Guidance Modularization Plan

> **For agentic workers:** Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox syntax for tracking.

**Goal:** Keep root AGENTS.md short while making detailed guidance discoverable, consistent, and owned by the appropriate contributor document.

**Architecture:** Root guidance contains durable principles and conditional links. Existing contributor guides own procedures; no additional AGENTS.md files or new documentation hierarchy are needed for this round.

**Tech Stack:** Markdown and repository-local links.

**Spec:** The conversation's approved direction: split documentation and retain only principles in AGENTS.md. The first migration is already present in the working tree.

## Global Constraints

- Preserve unrelated user changes.
- Keep AGENTS.md around its current 32 lines; line count is a maintenance target, not a correctness requirement.
- Migrate existing requirements without silently changing their scope or exceptions.
- Keep workflows conditional on the task; do not require reading every guide.
- Do not modify code, dependencies, CI policy, or remote GitHub state.
- Upstream policy reconciliation is separate from structural cleanup.

## Review Focus

- A contributor can identify the relevant guide directly from AGENTS.md.
- API compatibility and code-generation requirements remain discoverable for API changes.
- Verification guidance does not contradict existing integration and E2E instructions.
- Any upstream contribution-policy migration preserves maintainer and mechanical-change exceptions.
- Relative links and heading anchors resolve after moving text.

## Current State

The initial migration modified AGENTS.md, CONTRIBUTING.md, development/README.md,
test/README.md, and docs/README.md. Root guidance is now 32 lines. Some test
requirements still appear in both the development and test guides. Local
contribution guidance does not yet contain the policy observed on upstream main.

### Task 1: Give each requirement one authoritative home

**Files:** development/README.md, test/README.md, CONTRIBUTING.md, docs/README.md.

- [x] Keep repository layout, Go conventions, API boundaries, and code generation in development/README.md.
- [x] Make test/README.md own test selection, regression coverage, polling, and test execution details; replace duplicated development-guide instructions with a targeted link.
- [x] Keep documentation translation/build requirements in docs/README.md and submission requirements in CONTRIBUTING.md.
- [x] Reconcile the existing broad E2E recommendation with scope-based verification, preserving explicitly required checks.
- [x] Review the diff against the original AGENTS.md to confirm every substantive requirement has a destination.

**Validation:** Read the final guides together, search for duplicated requirements, and run git diff --check.

### Task 2: Reconcile upstream contribution policy separately

**Files:** CONTRIBUTING.md; AGENTS.md only if its contribution-guide link needs clarification.

- [x] Fetch current upstream AGENTS.md and CONTRIBUTING.md without changing the local branch.
- [x] Compare policy against local guidance and record additions separately from relocated text.
- [x] Propose the policy reconciliation for review before incorporating upstream requirements absent locally.
- [x] Incorporate the policy into CONTRIBUTING.md after the user continued the policy-reconciliation task; retain the root contribution-guide entry point.

**Validation:** Compare all conditions and exceptions with the fetched source. Verify that local implementation and external submission boundaries remain explicit.

### Task 3: Check the reading paths and finalize

**Files:** AGENTS.md and the four contributor guides above.

- [x] Walk through Go fix, API change, Python runtime change, RST translation change, and PR preparation scenarios; verify each leads directly to the relevant requirements.
- [x] Check added relative links and heading anchors, then run git diff --check.
- [x] Report root-file size, changed files, checks performed, and any unresolved policy differences.

**Validation:** No broken added links; no lost requirements; no unnecessary mandatory reading. Markdown-only edits do not require Go tests or Sphinx builds. If RST changes enter scope, follow docs/README.md, including translation and bilingual build checks.

## Deferred

Do not add per-directory AGENTS.md files, agent-specific skills, automated link-check CI,
or a new architecture-document hierarchy without a concrete recurring need.

## Execution notes

Structural cleanup completed in the existing working tree because the first
migration is already there. No commits, branch moves, or remote writes were
needed. This is a Markdown-only change; verification uses link checks and a
requirement-by-requirement review rather than implementation tests.

Test instructions now belong to test/README.md. The broad E2E recommendation
is scoped to cluster behavior, deployment, and interactions that narrower tests
cannot verify; component-specific requirements still apply. Runtime logging,
error handling, retries, timeouts, and context conventions remain in the
development guide.

## Policy reconciliation (implemented)

Compared with upstream main on 2026-10-28:
[AGENTS.md](https://github.com/vllm-project/aibrix/blob/main/AGENTS.md#contribution-intake-and-evidence)
and [CONTRIBUTING.md](https://github.com/vllm-project/aibrix/blob/main/CONTRIBUTING.md#issue-first-policy).
The additions formerly absent locally are now incorporated into CONTRIBUTING.md.
The user requested continuation after the separate proposal was presented.

Policy content owned by CONTRIBUTING.md:

- External contributors check for duplicate issues/PRs and obtain `triage/accepted`
  before implementation or PR preparation; investigation and reproduction may proceed.
- Issues establish affected versions/environments, expected and actual behavior,
  evidence, users, and direction. Acceptance does not approve a patch; expanded
  scope returns to the issue.
- Maintainers may self-triage routine fixes. Sensitive or broad changes still
  require prior discussion and independent review.
- Documentation corrections and mechanical fixes qualify for an exception only
  without behavior, configuration, dependency, workflow, API, or generated-file
  changes. Test-only work has no automatic exemption.
- PRs link the accepted issue, give reproduction/root-cause/impact evidence and
  actual verification results, and add a failing-then-passing regression when practical.
- Humans remain accountable; agent review is not maintainer approval.
- Maintainers may close duplicate, unsupported, out-of-scope, process-bypassing,
  or low-value submissions; compliant resubmission remains possible.

The implementation preserves upstream's detailed sensitive-change categories
and its community-page maintainer definition. It also carries over the agent
guide's investigation boundary, root-cause/impact evidence, and human accountability.

## Verification results

- Added local links and anchors: 20 checked, all resolve.
- Root guidance: 32 lines; all five task entry points present.
- API compatibility, layer ownership, and generated-artifact requirements checked.
- Independent read-only review: no significant findings; original requirements retain destinations.
- `git diff --check`: passed. No Go tests or Sphinx builds run (Markdown only).
- Policy reconciliation subsequently applied on user continuation; root entry now points to issue-first policy before implementation or PR preparation.

Final policy review found the general sensitive-or-broad-change condition was
narrower than upstream AGENTS.md when only specific categories were listed.
Restored the general condition and retained the categories as examples.
Policy conditions and local links were rechecked before commit.
