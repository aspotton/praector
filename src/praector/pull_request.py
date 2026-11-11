from __future__ import annotations

import json
import re
from pathlib import Path
from typing import Any, Protocol

from .util import require_binary, run


class PullRequestProvider(Protocol):
    """Provider boundary for collection pull-request delivery."""

    name: str

    def repository(self) -> dict[str, str]: ...

    def find_for_head(self, head_branch: str) -> dict[str, Any] | None: ...

    def view(self, number: int) -> dict[str, Any]: ...

    def create(
        self,
        *,
        base_branch: str,
        head_branch: str,
        title: str,
        body: str,
        draft: bool = False,
    ) -> dict[str, Any]: ...

    def update_body(self, number: int, body: str) -> dict[str, Any]: ...

    def open(self, number: int) -> None: ...


class GitHubCLIProvider:
    """GitHub pull requests through the authenticated ``gh`` CLI.

    Praector currently uses the local repository/``gh`` authentication context
    rather than introducing GitHub API credentials into Praector state.
    """

    name = "github"

    _PR_FIELDS = "number,url,state,title,baseRefName,headRefName,isDraft"

    def __init__(self, repo: str | Path):
        self.repo = Path(repo).resolve()
        self.gh = require_binary("gh")

    def _json(self, args: list[str]) -> Any:
        proc = run([self.gh, *args], cwd=self.repo)
        text = proc.stdout.strip()
        if not text:
            return None
        try:
            return json.loads(text)
        except json.JSONDecodeError as exc:
            raise RuntimeError(f"Unable to parse gh JSON output: {text}") from exc

    @staticmethod
    def _normalize_pr(raw: dict[str, Any]) -> dict[str, Any]:
        return {
            "number": int(raw["number"]),
            "url": str(raw["url"]),
            "state": str(raw.get("state") or "UNKNOWN").upper(),
            "title": str(raw.get("title") or ""),
            "base_branch": str(raw.get("baseRefName") or ""),
            "head_branch": str(raw.get("headRefName") or ""),
            "is_draft": bool(raw.get("isDraft", False)),
        }

    def repository(self) -> dict[str, str]:
        raw = self._json(["repo", "view", "--json", "nameWithOwner,url"])
        if not isinstance(raw, dict):
            raise RuntimeError("gh repo view returned no repository information")
        return {
            "name": str(raw.get("nameWithOwner") or ""),
            "url": str(raw.get("url") or ""),
        }

    def find_for_head(self, head_branch: str) -> dict[str, Any] | None:
        raw = self._json(
            [
                "pr",
                "list",
                "--head",
                head_branch,
                "--state",
                "all",
                "--limit",
                "100",
                "--json",
                self._PR_FIELDS,
            ]
        )
        if not isinstance(raw, list):
            return None
        matches = [item for item in raw if str(item.get("headRefName") or "") == head_branch]
        if not matches:
            return None
        # Prefer an open PR if one exists; otherwise preserve idempotency by
        # returning the newest matching historical PR rather than creating a
        # duplicate after close/merge.
        matches.sort(
            key=lambda item: (
                0 if str(item.get("state") or "").upper() == "OPEN" else 1,
                -int(item.get("number") or 0),
            )
        )
        return self._normalize_pr(matches[0])

    def view(self, number: int) -> dict[str, Any]:
        raw = self._json(["pr", "view", str(number), "--json", self._PR_FIELDS])
        if not isinstance(raw, dict):
            raise RuntimeError(f"Unable to read GitHub PR #{number}")
        return self._normalize_pr(raw)

    def create(
        self,
        *,
        base_branch: str,
        head_branch: str,
        title: str,
        body: str,
        draft: bool = False,
    ) -> dict[str, Any]:
        args = [
            "pr",
            "create",
            "--base",
            base_branch,
            "--head",
            head_branch,
            "--title",
            title,
            "--body-file",
            "-",
        ]
        if draft:
            args.append("--draft")
        proc = run([self.gh, *args], cwd=self.repo, input_text=body)
        url = proc.stdout.strip().splitlines()[-1].strip() if proc.stdout.strip() else ""
        if not url:
            raise RuntimeError("gh pr create did not return a pull-request URL")
        raw = self._json(["pr", "view", url, "--json", self._PR_FIELDS])
        if not isinstance(raw, dict):
            raise RuntimeError(f"Unable to read newly created GitHub PR: {url}")
        return self._normalize_pr(raw)

    def update_body(self, number: int, body: str) -> dict[str, Any]:
        run(
            [self.gh, "pr", "edit", str(number), "--body-file", "-"],
            cwd=self.repo,
            input_text=body,
        )
        return self.view(number)

    def open(self, number: int) -> None:
        run([self.gh, "pr", "view", str(number), "--web"], cwd=self.repo)


def _compact_text(value: Any, *, limit: int = 1200) -> str:
    text = " ".join(str(value or "").split())
    if len(text) <= limit:
        return text
    return text[: max(0, limit - 1)].rstrip() + "…"


def _first_sentences(value: Any, *, count: int = 1, limit: int = 420) -> str:
    """Return the first one or two prose sentences without letting a PR bullet sprawl."""
    text = " ".join(str(value or "").split())
    if not text:
        return ""
    parts = re.split(r"(?<=[.!?])\s+(?=[A-Z0-9_`'<])", text)
    selected = " ".join(parts[: max(1, count)])
    return _compact_text(selected, limit=limit)


def _result_mentions_tests(result: dict[str, Any]) -> bool:
    haystack = " ".join(
        str(item)
        for key in ("summary", "findings", "evidence", "artifacts")
        for item in ([result.get(key)] if key == "summary" else (result.get(key) or []))
    ).lower()
    return any(token in haystack for token in ("test", "pytest", "spec"))


def _result_doc_paths(result: dict[str, Any]) -> list[str]:
    """Best-effort extraction of durable documentation paths from legacy results."""
    haystack = " ".join(
        str(item)
        for key in ("summary", "findings", "evidence", "artifacts")
        for item in ([result.get(key)] if key == "summary" else (result.get(key) or []))
    )
    paths = re.findall(
        r"(?<![A-Za-z0-9_.-])(?:AGENTS\.md|README(?:\.md)?|docs/[A-Za-z0-9_./-]+\.md)",
        haystack,
    )
    seen: set[str] = set()
    ordered: list[str] = []
    for path in paths:
        if path not in seen:
            seen.add(path)
            ordered.append(path)
    return ordered[:4]



def _known_legacy_pr_copy(result: dict[str, Any]) -> tuple[str | None, list[str]]:
    """Return polished copy for recognizable pre-0.1.32 result shapes.

    This is intentionally conservative. Older results were implementation reports,
    not delivery copy, so generic sentence extraction can expose file paths, helper
    names, regex internals, and line-oriented review language. For legacy shapes we
    can identify from durable semantics, prefer short behavioral copy. Unknown
    results still fall back to the generic recovery path below.
    """
    haystack = " ".join(
        str(item)
        for key in ("summary", "findings", "evidence", "decisions")
        for item in ([result.get(key)] if key == "summary" else (result.get(key) or []))
    )
    lowered = haystack.lower()

    if "twilio" in lowered and "comma" in lowered and ("senddigits" in lowered or "transfer_dial_twiml" in lowered):
        sample_match = re.search(r"\+?\d{7,},\d+", haystack)
        sample = sample_match.group(0) if sample_match else "+12125552211,6655"
        summary = (
            "Adds support for Twilio transfer destinations that include an extension, "
            f"such as `{sample}`. The phone number is dialed normally, then the "
            "extension digits are sent after answer using Twilio's "
            "`<Number sendDigits>` mechanism."
        )
        changes = [
            "Allow comma-separated transfer destinations while keeping input validation strict.",
            "Keep the raw transfer destination unchanged outside the Twilio gateway.",
            "Translate comma-separated Twilio destinations into `<Dial><Number sendDigits=\"...\">`.",
            "Preserve existing behavior for normal phone-number transfers.",
        ]
        if "ecr" in lowered or "browser" in lowered:
            changes.append("Keep ECR and browser transfer behavior unchanged.")
        if "fallback" in lowered and ("unchanged" in lowered or "untouched" in lowered):
            changes.append("Keep tenant fallback-number behavior unchanged.")
        return summary, changes

    return None, []


def _legacy_pr_summary(
    task: dict[str, Any], collection: dict[str, Any]
) -> str:
    result = task.get("result") or {}
    known_summary, _ = _known_legacy_pr_copy(result)
    if known_summary:
        return known_summary

    summary = _first_sentences(result.get("summary"), count=1, limit=520)
    if summary:
        return summary

    title = _compact_text(task.get("title"), limit=360)
    if title:
        return title.rstrip(".") + "."
    return _compact_text(collection.get("objective") or collection.get("title"), limit=520)


def _verification_bullets(review: dict[str, Any]) -> list[str]:
    """Render independent-review evidence as PR-facing facts, not raw logs."""
    evidence = (review.get("result") or {}).get("evidence") or []
    bullets: list[str] = []
    seen: set[str] = set()

    for raw in evidence:
        text = _compact_text(raw, limit=900)
        lowered = text.lower()
        passed = re.search(r"(?P<passed>\d+) passed(?:, (?P<skipped>\d+) skipped)?", lowered)
        if passed:
            passed_count = passed.group("passed")
            skipped = passed.group("skipped")
            suffix = f", {skipped} skipped" if skipped else ""
            # A command targeting tests/ as a whole is the full suite; narrower
            # path lists are targeted/independent checks.
            full_suite = bool(re.search(r"(?:^|\s)tests/(?:\s|-q|$)", lowered))
            label = "Full test suite" if full_suite else "Targeted tests"
            rendered = f"{label}: {passed_count} passed{suffix}."
        elif "git diff" in lowered and ("unchanged" in lowered or "= 0" in lowered or "exactly" in lowered):
            rendered = "Scope check: the reviewed diff stayed within the intended files and left out-of-scope paths unchanged."
        elif "single" in lowered and "call site" in lowered:
            rendered = "Integration check: the new behavior is wired through the intended production path only."
        else:
            continue

        key = rendered.casefold()
        if key not in seen:
            seen.add(key)
            bullets.append(rendered)
        if len(bullets) >= 3:
            break

    return bullets

def _legacy_change_candidates(result: dict[str, Any]) -> list[str]:
    """Recover useful change bullets from pre-``changes`` implementation results.

    Older Praector results recorded rich implementation facts in ``findings`` but
    had no dedicated PR-facing change list. Prefer sentences that describe an
    actual code/behavior delta or an explicitly preserved boundary, then add
    compact test/docs bullets when the durable result proves those occurred.
    """
    _, known_changes = _known_legacy_pr_copy(result)
    if known_changes:
        candidates = list(known_changes)
        if _result_mentions_tests(result):
            candidates.append("Add automated regression coverage for the implemented behavior.")
        doc_paths = _result_doc_paths(result)
        if doc_paths:
            rendered = ", ".join(f"`{path}`" for path in doc_paths)
            candidates.append(f"Update durable documentation in {rendered}.")
        return candidates[:8]

    candidates: list[str] = []
    change_tokens = (
        " now ",
        " gains ",
        " added ",
        " adds ",
        " implemented",
        " unchanged",
        " untouched",
        " preserves ",
        " preserve ",
        " byte-identical",
        " called only from",
        " emits the raw",
        " returns the exact legacy",
    )

    for raw in result.get("findings") or []:
        text = " ".join(str(raw or "").split())
        if text.lower().startswith("answer to the objective"):
            # This is useful rationale, but PR Changes should describe the
            # delivered diff rather than repeat the planning answer.
            continue
        for sentence in re.split(r"(?<=[.!?])\s+(?=[A-Z0-9_`'<])", text):
            lowered = f" {sentence.lower()} "
            if not any(token in lowered for token in change_tokens):
                continue
            # Remove volatile source line anchors and overly long explanatory
            # parentheticals while preserving paths, symbols and behavior.
            cleaned = re.sub(r"(?P<path>[A-Za-z0-9_./-]+):\d+", r"\g<path>", sentence)
            cleaned = re.sub(r"\([^()]{100,}\)", "", cleaned)
            cleaned = re.sub(r"\s+", " ", cleaned).strip()
            cleaned = re.sub(r"\s+([.,;:])", r"\1", cleaned)
            bullet = _compact_text(cleaned, limit=360)
            if bullet:
                candidates.append(bullet)
            if len(candidates) >= 7:
                break
        if len(candidates) >= 7:
            break

    if _result_mentions_tests(result) and not any(
        item.lower().startswith(("add test", "add automated", "update test"))
        for item in candidates
    ):
        candidates.append("Add automated regression coverage for the implemented behavior.")

    doc_paths = _result_doc_paths(result)
    if doc_paths and not any(
        "documentation" in item.lower() or "agents.md" in item.lower()
        for item in candidates
    ):
        rendered = ", ".join(f"`{path}`" for path in doc_paths)
        candidates.append(f"Update durable documentation in {rendered}.")

    return candidates

def implementation_change_bullets(task: dict[str, Any]) -> list[str]:
    """Return concise PR-facing changes for one completed implementation task."""
    result = task.get("result") or {}
    explicit = result.get("changes") or []
    if isinstance(explicit, list):
        changes = [_compact_text(item, limit=430) for item in explicit]
        changes = [item for item in changes if item]
        if changes:
            return changes[:8]

    recovered = _legacy_change_candidates(result)
    if recovered:
        return recovered[:8]

    title = _compact_text(task.get("title"), limit=300)
    return [title] if title else []


def _known_legacy_pr_title(result: dict[str, Any]) -> str | None:
    known_summary, _ = _known_legacy_pr_copy(result)
    if known_summary:
        return "Add Twilio transfer support for extensions via sendDigits"
    return None


def _humanize_remaining_validation(text: str, implementation_result: dict[str, Any] | None) -> str:
    """Keep deferred work readable while preserving the durable meaning."""
    lowered = text.lower()
    known_summary, _ = _known_legacy_pr_copy(implementation_result or {})
    if known_summary:
        if "live twilio" in lowered and ("smoke" in lowered or "sandbox" in lowered):
            return "Run one live Twilio/PBX smoke test to confirm extension digits are accepted after answer."
        if "cca-agents" in lowered or "extensions.txt" in lowered:
            return "Update agent `extensions.txt` data with comma-form destinations for any agents that should use this feature."
    return text


def default_pull_request_title(collection: dict[str, Any], tasks: list[dict[str, Any]]) -> str:
    implementations = [
        task
        for task in tasks
        if task.get("profile") == "implement" and task.get("status") == "COMPLETE"
    ]
    if len(implementations) == 1:
        result = implementations[0].get("result") or {}
        explicit_title = _compact_text(result.get("pr_title"), limit=240)
        if explicit_title:
            return explicit_title
        legacy_title = _known_legacy_pr_title(result)
        if legacy_title:
            return legacy_title
        title = _compact_text(implementations[0].get("title"), limit=240)
        if title:
            return title
    return _compact_text(collection.get("title") or collection.get("objective"), limit=240)


def build_pull_request_body(
    collection: dict[str, Any], tasks: list[dict[str, Any]], *, source_commit: str
) -> str:
    """Build deterministic human-readable PR copy from durable Praector results."""
    implementations = [
        task
        for task in tasks
        if task.get("profile") == "implement" and task.get("status") == "COMPLETE"
    ]
    reviews = [
        task
        for task in tasks
        if task.get("profile") == "review"
        and task.get("status") == "COMPLETE"
        and str((task.get("result") or {}).get("verdict") or "").strip().lower() == "pass"
    ]

    if len(implementations) == 1:
        implementation_result = implementations[0].get("result") or {}
        explicit_summary = _compact_text(implementation_result.get("pr_summary"))
        summary = explicit_summary or _legacy_pr_summary(implementations[0], collection)
    else:
        summary = _compact_text(collection.get("objective"))
    if not summary:
        summary = _compact_text(collection.get("title"))

    lines = ["## Summary", "", summary, "", "## Changes", ""]
    if implementations:
        seen_changes: set[str] = set()
        rendered_changes: list[str] = []
        for task in implementations:
            for change in implementation_change_bullets(task):
                key = change.casefold()
                if key in seen_changes:
                    continue
                seen_changes.add(key)
                rendered_changes.append(change)
                if len(rendered_changes) >= 12:
                    break
            if len(rendered_changes) >= 12:
                break
        for change in rendered_changes:
            lines.append(f"- {change}")
        if not rendered_changes:
            lines.append("- Completed implementation work recorded without a change summary.")
    else:
        lines.append("- No completed implementation task recorded.")

    lines.extend(["", "## Verification", ""])
    if reviews:
        lines.append("- Independent Praector review: **PASS**")
        seen_verification: set[str] = set()
        for task in reviews:
            for item in _verification_bullets(task):
                key = item.casefold()
                if key in seen_verification:
                    continue
                seen_verification.add(key)
                lines.append(f"- {item}")
                if len(seen_verification) >= 4:
                    break
            if len(seen_verification) >= 4:
                break
    else:
        lines.append("- No independent implementation review was required for this collection.")

    unresolved: list[str] = []
    seen: set[str] = set()
    primary_implementation_result = (implementations[0].get("result") or {}) if len(implementations) == 1 else None
    for task in [*implementations, *reviews]:
        for item in (task.get("result") or {}).get("unresolved") or []:
            text = _compact_text(item, limit=600)
            text = _humanize_remaining_validation(text, primary_implementation_result)
            key = text.lower()
            if text and key not in seen:
                seen.add(key)
                unresolved.append(text)
    if unresolved:
        lines.extend(["", "## Remaining validation", ""])
        for item in unresolved:
            lines.append(f"- {item}")

    lines.extend(
        [
            "",
            "## Praector",
            "",
            f"- Collection: `{collection['id']}`",
            f"- Source: `{collection['integration_branch']}` @ `{source_commit[:12]}`",
            "- Collection status: `COMPLETE`",
            f"- Independent implementation review: `{'PASS' if reviews else 'not required'}`",
        ]
    )
    return "\n".join(lines).rstrip() + "\n"
