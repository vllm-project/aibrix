#!/usr/bin/env python3
"""Repository-local issue and pull request triage bot for AIBrix."""

from __future__ import annotations

import json
import os
import re
import sys
from dataclasses import dataclass
from urllib.error import HTTPError, URLError
from urllib.parse import quote, urljoin
from urllib.request import Request, urlopen


API_ROOT = "https://api.github.com/"
GUIDE_MARKER = "<!-- aibrix-bot-guide -->"
INFO_MARKER = "<!-- aibrix-bot-needs-info -->"
GUIDE_COMMENT = f"""{GUIDE_MARKER}
Thanks for contributing to AIBrix! Please review the [contribution guide](https://github.com/vllm-project/aibrix/blob/main/CONTRIBUTING.md) and make sure this issue contains enough context for maintainers to reproduce or evaluate it.
"""

FORM_KIND = {
    "bug": "kind/bug",
    "feature": "kind/feature",
    "rfc": "kind/feature",
}

FORM_REQUIRED = {
    "bug": ["Describe the bug", "Steps to Reproduce", "Expected behavior", "Environment"],
    "feature": ["Feature Description and Motivation", "Use Case"],
    "rfc": ["Summary", "Motivation", "Proposed Change"],
}

# These are the labels this bot owns. It never creates labels and only removes
# labels from this set when reconciling a reclassified issue.
TRIAGE_LABEL = "triage/needs-information"
MANAGED_LABELS = {
    "kind/bug",
    "kind/feature",
    "kind/misc",
    "area/gateway",
    "area/orchestration",
    "area/runtime",
    "area/kv-cache",
    "area/batch",
    "area/website",
    "area/cicd",
    "area/installation",
    "area/testing",
    TRIAGE_LABEL,
    # Labels produced by earlier revisions of this bot.
    "kind/needs-info",
    "kind/needs-triage",
    "area/controller",
    "area/python",
    "area/kvcache",
    "area/docs",
    "area/ci",
    "area/console",
    "area/helm",
    "area/api",
    "area/e2e",
    "area/needs-triage",
}
PR_MANAGED_LABELS = {TRIAGE_LABEL, "kind/needs-info"}

# Keep this table explicit and conservative. A later matching rule does not
# override an earlier one, but multiple existing areas may be returned.
AREA_RULES = (
    ("area/gateway", (r"\bgateway\b", r"envoy", r"routing")),
    ("area/orchestration", (r"\bcontroller(?:s)?\b", r"reconciliation", r"modelclaim", r"modeladapter", r"crd", r"custom resource")),
    ("area/runtime", (r"\bruntime\b", r"downloader", r"metadata-service")),
    ("area/kv-cache", (r"\bkv[- ]?cache\b", r"kvcache", r"nixl", r"shfs")),
    ("area/batch", (r"\bbatch\b", r"scheduled job")),
    ("area/website", (r"\bdocs?\b", r"documentation", r"readthedocs", r"\bconsole\b", r"frontend", r"web ui")),
    ("area/cicd", (r"\bci\b", r"github actions?", r"workflow")),
    ("area/installation", (r"\bhelm\b", r"kustomize", r"chart")),
    ("area/testing", (r"\be2e\b", r"end[- ]to[- ]end", r"kind cluster")),
)

PR_PREFIXES = ("[bug]", "[ci]", "[docs]", "[api]", "[cli]", "[misc]")
PLACEHOLDER_RE = re.compile(r"\[(?:Please provide\b|Insert issue number(?:\(s\))?)", re.I)


@dataclass(frozen=True)
class IssueClassification:
    kind: str
    areas: list[str]
    form: str | None


def _clean_heading(value: str) -> str:
    value = re.sub(r"[^\w\s]", " ", value, flags=re.UNICODE)
    return re.sub(r"\s+", " ", value).strip().lower()


def _required_headings(form: str) -> set[str]:
    return {_clean_heading(value) for value in FORM_REQUIRED[form]}


def _form_name(title: str, body: str) -> str | None:
    sections = _sections(body)
    if len(set(sections) & _required_headings("bug")) >= 2:
        return "bug"
    if len(set(sections) & _required_headings("rfc")) >= 2:
        return "rfc"
    if len(set(sections) & _required_headings("feature")) >= 2:
        return "feature"
    text = title.lower()
    if "bug report" in text:
        return "bug"
    if "request for comments" in text or "[rfc]" in text:
        return "rfc"
    if "feature request" in text:
        return "feature"
    return None


def _keyword_kind(title: str, body: str) -> str:
    text = f"{title}\n{body}".lower()
    if re.search(r"\b(bug report|broken|crash(?:es|ed)?|regression|reproducible)\b", text):
        return "kind/bug"
    if re.search(r"\b(feature request|feature proposal|rfc|enhancement request)\b", text):
        return "kind/feature"
    return "kind/misc"


def classify_issue(title: str, body: str) -> IssueClassification:
    form = _form_name(title, body)
    kind = FORM_KIND[form] if form else _keyword_kind(title, body)
    haystack = f"{title}\n{body}".lower()
    areas = [label for label, patterns in AREA_RULES if any(re.search(pattern, haystack) for pattern in patterns)]
    return IssueClassification(kind, areas, form)


def _sections(body: str) -> dict[str, str]:
    found: dict[str, list[str]] = {}
    current: str | None = None
    for line in body.splitlines():
        match = re.match(r"^\s*#{1,6}\s+(.+?)\s*$", line)
        if match:
            current = _clean_heading(match.group(1))
            found.setdefault(current, [])
        elif current:
            found[current].append(line)
    return {heading: "\n".join(lines).strip() for heading, lines in found.items()}


def _looks_empty(value: str | None) -> bool:
    if not value or PLACEHOLDER_RE.search(value):
        return True
    return not re.sub(r"[\s_`*\-]", "", value)


def validate_issue(title: str, body: str) -> list[str]:
    form = _form_name(title, body)
    if not form:
        return []
    sections = _sections(body)
    missing = []
    for required in FORM_REQUIRED[form]:
        normalized = _clean_heading(required)
        if _looks_empty(sections.get(normalized)):
            missing.append(required)
    return missing


def validate_pr(title: str, body: str) -> list[str]:
    errors = []
    if not title.strip().lower().startswith(PR_PREFIXES):
        errors.append("PR title must start with one of: [Bug], [CI], [Docs], [API], [CLI], [Misc].")
    if not body.strip():
        return errors + ["PR description is empty."]
    if PLACEHOLDER_RE.search(body):
        errors.append("PR description still contains a template placeholder.")
    sections = _sections(body)
    description = sections.get(_clean_heading("Pull Request Description"), "")
    if _looks_empty(description):
        errors.append("PR description must explain the change in the Pull Request Description section.")
    related = sections.get(_clean_heading("Related Issues"), "")
    if _looks_empty(related) or ("#" not in related and not re.search(r"\b(n/?a|none|not applicable)\b", related, re.I)):
        errors.append("PR description must identify a related issue, or explicitly state N/A.")
    return errors


class GitHub:
    def __init__(self, token: str, repository: str):
        self.repository = repository
        self.token = token

    def request(self, method: str, path: str, payload: dict | None = None):
        data = json.dumps(payload).encode() if payload is not None else None
        request = Request(
            urljoin(API_ROOT, path.lstrip("/")),
            data=data,
            method=method,
            headers={
                "Accept": "application/vnd.github+json",
                "Authorization": f"Bearer {self.token}",
                "X-GitHub-Api-Version": "2022-11-28",
                "User-Agent": "aibrix-local-workflow-bot",
                **({"Content-Type": "application/json"} if data is not None else {}),
            },
        )
        try:
            with urlopen(request, timeout=20) as response:
                raw = response.read()
                return json.loads(raw) if raw else None
        except HTTPError as error:
            detail = error.read().decode(errors="replace")
            raise RuntimeError(f"GitHub API {method} {path} failed ({error.code}): {detail}") from error
        except URLError as error:
            raise RuntimeError(f"GitHub API {method} {path} failed: {error.reason}") from error

    def list_labels(self, number: int) -> list[str]:
        labels = self.request("GET", f"/repos/{self.repository}/issues/{number}/labels?per_page=100")
        return [label["name"] for label in labels or []]

    def sync_labels(self, number: int, desired: list[str], managed_labels: set[str] = MANAGED_LABELS):
        desired = list(dict.fromkeys(desired))
        current = set(self.list_labels(number))
        for name in (current & managed_labels) - set(desired):
            self.remove_label(number, name)
        additions = [name for name in desired if name not in current]
        if additions:
            self.request("POST", f"/repos/{self.repository}/issues/{number}/labels", {"labels": additions})

    def remove_label(self, number: int, name: str):
        path = f"/repos/{self.repository}/issues/{number}/labels/{quote(name, safe='')}"
        try:
            self.request("DELETE", path)
        except RuntimeError as error:
            if "(404)" not in str(error):
                raise

    def has_bot_comment(self, number: int, marker: str) -> bool:
        page = 1
        while True:
            comments = self.request("GET", f"/repos/{self.repository}/issues/{number}/comments?per_page=100&page={page}")
            if any(marker in comment.get("body", "") for comment in comments or []):
                return True
            if not comments or len(comments) < 100:
                return False
            page += 1

    def comment_once(self, number: int, body: str, marker: str):
        if not self.has_bot_comment(number, marker):
            self.request("POST", f"/repos/{self.repository}/issues/{number}/comments", {"body": body})


def _summary(lines: list[str]):
    summary_path = os.environ.get("GITHUB_STEP_SUMMARY")
    text = "\n".join(lines) + "\n"
    if summary_path:
        with open(summary_path, "a", encoding="utf-8") as output:
            output.write(text)
    print(text, end="")


def handle_issue(event: dict, github: GitHub):
    issue = event["issue"]
    title, body = issue.get("title", ""), issue.get("body") or ""
    classification = classify_issue(title, body)
    labels = [classification.kind, *classification.areas]
    missing = validate_issue(title, body)
    if missing:
        labels.append(TRIAGE_LABEL)
    github.sync_labels(issue["number"], labels)
    lines = [f"## AIBrix bot: Issue #{issue['number']}", f"Labels: {', '.join(labels)}"]
    if missing:
        lines.append("Missing required Issue Form sections: " + ", ".join(missing))
        github.comment_once(issue["number"], f"{INFO_MARKER}\nPlease complete these required sections: " + ", ".join(f"`{item}`" for item in missing) + ".", INFO_MARKER)
    else:
        lines.append("Issue Form completeness: passed")
    if event.get("action") == "opened":
        github.comment_once(issue["number"], GUIDE_COMMENT, GUIDE_MARKER)
    _summary(lines)
    return False


def handle_pull_request(event: dict, github: GitHub):
    pull_request = event["pull_request"]
    errors = validate_pr(pull_request.get("title", ""), pull_request.get("body") or "")
    labels = [TRIAGE_LABEL] if errors else []
    lines = [f"## AIBrix bot: PR #{pull_request['number']}"]
    if errors:
        lines.append("PR checks: findings (advisory)")
        lines.extend(f"- {error}" for error in errors)
        for error in errors:
            print(f"::warning::{error}")
    else:
        lines.append("PR checks: passed")
    _summary(lines)
    try:
        github.sync_labels(pull_request["number"], labels, PR_MANAGED_LABELS)
    except RuntimeError as error:
        print(f"Unable to update PR labels: {error}", file=sys.stderr)
        print("::warning::Unable to update PR labels")
        _summary(["Label update: warning (label mutation failed; validation remains advisory)"])
    # PR title and description checks are advisory and must not block CI.
    return False


def self_test() -> None:
    result = classify_issue("RFC: gateway change", "### Summary\nImprove gateway routing")
    assert result.kind == "kind/feature"
    assert result.areas == ["area/gateway"]
    result = classify_issue("Question", "No supported component mentioned")
    assert result.kind == "kind/misc"
    assert result.areas == []
    assert validate_issue("🐛 AIBrix Bug Report", "### 🐛 Describe the bug\nactual\n\n### Steps to Reproduce\nrepro") == [
        "Expected behavior",
        "Environment",
    ]
    assert validate_pr(
        "[Bug] Fix gateway",
        "## Pull Request Description\nFix gateway behavior.\n\n## Related Issues\nResolves: #123",
    ) == []
    assert validate_pr(
        "Fix gateway",
        "## Pull Request Description\n[Please provide a clear and concise description of your changes here]",
    )
    print("AIBrix bot self-test passed")


def main(argv: list[str]) -> int:
    if argv[1:] == ["--self-test"]:
        self_test()
        return 0
    if len(argv) != 2:
        print(f"usage: {argv[0]} EVENT_JSON", file=sys.stderr)
        return 2
    token = os.environ.get("GITHUB_TOKEN")
    repository = os.environ.get("GITHUB_REPOSITORY")
    if not token or not repository:
        print("GITHUB_TOKEN and GITHUB_REPOSITORY are required", file=sys.stderr)
        return 2
    try:
        with open(argv[1], encoding="utf-8") as event_file:
            event = json.load(event_file)
        github = GitHub(token, repository)
        validation_failed = False
        if "issue" in event:
            validation_failed = handle_issue(event, github)
        elif "pull_request" in event:
            validation_failed = handle_pull_request(event, github)
        else:
            raise RuntimeError("event payload is neither an issue nor a pull request")
    except (OSError, json.JSONDecodeError, RuntimeError) as error:
        print(f"::error::{error}", file=sys.stderr)
        return 1
    return 1 if validation_failed else 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv))
