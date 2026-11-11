from __future__ import annotations

from dataclasses import dataclass
import hashlib
import json
import re

from .util import normalize_text


@dataclass(frozen=True, slots=True)
class PlanningPolicy:
    critic_required: bool
    plan_review_required: bool
    categories: tuple[str, ...]
    reasons: tuple[str, ...]

    def as_dict(self) -> dict[str, object]:
        return {
            "critic_required": self.critic_required,
            "plan_review_required": self.plan_review_required,
            "categories": list(self.categories),
            "reasons": list(self.reasons),
        }


_SECURITY_PATTERNS = (
    r"\bauth(?:entication|orization)?\b",
    r"\bapi[-_ ]?key(?:s)?\b",
    r"\bcredential(?:s)?\b",
    r"\bsecret(?:s)?\b",
    r"\bpassword(?:s)?\b",
    r"\boauth\b",
    r"\btoken(?:s)?\b",
    r"\baccess[ -]?control\b",
    r"\bpermission(?:s)?\b",
    r"\bencrypt(?:ion|ed)?\b",
    r"\bsecurity\b",
)

_STATEFUL_PATTERNS = (
    r"\brate[ -]?limit(?:ing)?\b",
    r"\bstateful\b",
    r"\bsession(?:s)?\b",
    r"\bcache(?:d|s|ing)?\b",
    r"\bqueue(?:s)?\b",
    r"\bretr(?:y|ies)\b",
    r"\bidempoten(?:t|cy)\b",
    r"\bconcurren(?:t|cy)\b",
    r"\block(?:ing|s)?\b",
    r"\btransaction(?:s)?\b",
)

_MIGRATION_PATTERNS = (
    r"\bmigrat(?:e|ion|ions)\b",
    r"\bschema\b",
    r"\bdatabase\b",
    r"\bbackfill\b",
    r"\bdata conversion\b",
)

_DESTRUCTIVE_PATTERNS = (
    r"\bdelete\b",
    r"\bdrop\b",
    r"\bpurge\b",
    r"\bdestructive\b",
    r"\birreversible\b",
)

_EXTERNAL_CONTRACT_PATTERNS = (
    r"\bapi\b",
    r"\bwebhook(?:s)?\b",
    r"\bpublic endpoint\b",
    r"\bbackward compatib(?:le|ility)\b",
)

_EXTERNAL_INTEGRATION_PATTERNS = (
    r"\bgateway(?:s)?\b",
    r"\bintegration(?:s)?\b",
    r"\bthird[- ]party\b",
    r"\bexternal service\b",
    r"\bprovider(?:s)?\b",
    r"\bsdk\b",
    r"\btwilio\b",
    r"\btelephony\b",
    r"\btwiml\b",
)

# Delivery classification is intentionally deterministic, but normal user
# requests are often phrased conversationally rather than beginning with an
# imperative verb. Keep the mutation vocabulary centralized so planning and
# delivery policy classify the same intent.
_MUTATION_VERBS = (
    "add|implement|build|create|modify|change|update|fix|repair|refactor|"
    "remove|delete|migrate|extend|introduce|replace|upgrade|convert|make|"
    "enforce|support"
)
_IMPLEMENTATION_FIRST_LINE = re.compile(
    rf"^(?:please\s+)?(?:{_MUTATION_VERBS})\b",
    flags=re.IGNORECASE,
)
_IMPLEMENTATION_REQUEST_PATTERNS = (
    rf"\b(?:i(?:'|’)d|i would|we would)\s+like\s+to\s+(?:{_MUTATION_VERBS})\b",
    rf"\b(?:i|we)\s+(?:want|need)\s+(?:you\s+)?to\s+(?:{_MUTATION_VERBS})\b",
    rf"\b(?:can|could|would)\s+you\s+(?:please\s+)?(?:{_MUTATION_VERBS})\b",
    rf"\bplease\s+(?:{_MUTATION_VERBS})\b",
    rf"\blet(?:'|’)s\s+(?:{_MUTATION_VERBS})\b",
)

# Preserve a low-ceremony escape hatch for clearly documentation-only edits.
# The test is deliberately narrow: if the objective also names code/runtime
# surfaces, it is no longer considered trivial documentation work.
_DOCUMENTATION_PATTERNS = (
    r"\breadme(?:\.md)?\b",
    r"\bdocumentation\b",
    r"\bdocs?\b",
    r"\btypo\b",
    r"\bspelling\b",
    r"\bwording\b",
    r"\bexample sentence\b",
)
_CODE_OR_RUNTIME_PATTERNS = (
    r"\bfeature\b",
    r"\bbug\b",
    r"\bcode(?:base)?\b",
    r"\bfunction\b",
    r"\bmethod\b",
    r"\bclass\b",
    r"\bendpoint\b",
    r"\broute\b",
    r"\bhandler\b",
    r"\bgateway\b",
    r"\btemplate\b",
    r"\bservice\b",
    r"\bclient\b",
    r"\bserver\b",
    r"\btest(?:s|ing)?\b",
    r"\bapi\b",
    r"\bwebhook\b",
    r"\btransfer\b",
    r"\bdtmf\b",
    r"\btwiml\b",
    r"\bdial\b",
    r"\b(?:py|js|ts|java|go|rs|rb|php|cs|cpp|c|h)\b",
    r"\.(?:py|pyi|js|jsx|ts|tsx|java|go|rs|rb|php|cs|cpp|cc|cxx|c|h|hpp|sql|sh)\b",
)


def _matches_any(text: str, patterns: tuple[str, ...]) -> bool:
    return any(re.search(pattern, text, flags=re.IGNORECASE) for pattern in patterns)


def _requires_implementation_delivery(objective: str) -> bool:
    """Return whether the objective is an explicit request to mutate delivery.

    Direct imperatives are recognized from the first meaningful line, while
    common conversational forms ("I'd like to add...", "Can you fix...") are
    recognized anywhere in the objective. This avoids treating explanatory or
    research prompts that merely mention implementation verbs as write work.
    """
    text = objective or ""
    first_line = next((line.strip() for line in text.splitlines() if line.strip()), "")
    if _IMPLEMENTATION_FIRST_LINE.search(first_line):
        return True
    return _matches_any(text, _IMPLEMENTATION_REQUEST_PATTERNS)


def _is_trivial_documentation_change(objective: str) -> bool:
    text = objective or ""
    return _matches_any(text, _DOCUMENTATION_PATTERNS) and not _matches_any(
        text, _CODE_OR_RUNTIME_PATTERNS
    )


def classify_planning_policy(objective: str) -> PlanningPolicy:
    """Return deterministic planning gates for a Collection objective.

    This intentionally uses conservative lexical signals instead of an LLM so
    the planner cannot silently opt out of independent planning controls. The
    policy is a floor, not a ceiling: the coordinator may still add critic,
    advice, research, or plan review for work that is not caught here.

    Any non-trivial explicit implementation request receives an independent
    plan-review floor. Externally integrated/contract-sensitive implementation
    changes additionally require a critic. This prevents conversationally
    worded feature requests from bypassing gates merely because they do not
    begin with an imperative verb.
    """
    text = objective or ""
    categories: list[str] = []
    reasons: list[str] = []

    implementation = _requires_implementation_delivery(text)
    trivial_docs = implementation and _is_trivial_documentation_change(text)
    security = _matches_any(text, _SECURITY_PATTERNS)
    stateful = _matches_any(text, _STATEFUL_PATTERNS)
    migration = _matches_any(text, _MIGRATION_PATTERNS)
    destructive = _matches_any(text, _DESTRUCTIVE_PATTERNS)
    external_contract = _matches_any(text, _EXTERNAL_CONTRACT_PATTERNS)
    external_integration = _matches_any(text, _EXTERNAL_INTEGRATION_PATTERNS)

    if implementation and not trivial_docs:
        categories.append("implementation_change")
        reasons.append("Non-trivial implementation work requires an independent plan-review gate before write work.")
    if security:
        categories.append("security")
        reasons.append("Security/authentication/credential-sensitive behavior requires an independent pre-planning critic.")
    if stateful:
        categories.append("stateful_behavior")
        reasons.append("Stateful/concurrency/rate-limit behavior requires explicit state and verification semantics.")
    if migration:
        categories.append("migration_or_persistence")
        reasons.append("Persistence/schema/migration work requires explicit compatibility, rollback, and verification planning.")
    if destructive:
        categories.append("destructive_change")
        reasons.append("Potentially destructive or irreversible work requires an independent planning challenge and review.")
    if external_contract:
        categories.append("external_contract")
    if external_integration:
        categories.append("external_integration")
        reasons.append("External provider/gateway integration behavior requires an independent critic before write work.")

    integration_sensitive_change = bool(
        implementation and (external_contract or external_integration)
    )
    critic_required = (
        security
        or migration
        or destructive
        or (stateful and external_contract)
        or integration_sensitive_change
    )
    plan_review_required = (
        critic_required
        or stateful
        or migration
        or destructive
        or (implementation and not trivial_docs)
    )

    if plan_review_required and not any("plan-review" in reason.lower() or "plan review" in reason.lower() for reason in reasons):
        reasons.append("The objective crosses the deterministic threshold for an independent plan-review gate before write work.")

    return PlanningPolicy(
        critic_required=critic_required,
        plan_review_required=plan_review_required,
        categories=tuple(categories),
        reasons=tuple(reasons),
    )


_MATERIAL_PLAN_PROFILES = {"implement", "research", "advise"}
_MATERIAL_PLAN_RELATIONS = {"depends_on", "blocks", "supersedes", "superseded_by"}


def material_plan_fingerprint(
    objective: str,
    tasks: list[dict[str, object]],
    edges: list[dict[str, object]],
    *,
    legacy_raw_base_commit: bool = False,
) -> str:
    """Return a stable fingerprint for critic-relevant plan material.

    The initial critic is a preflight critic, so ordinary construction of the
    first plan must not immediately invalidate it. Praector seals that critic to
    the first final plan-review handoff. After that seal, this fingerprint is
    used to detect genuinely new plan material.

    Status/timestamps, mutable workspace Git pointers, coordinator/reviewer
    bookkeeping, and orchestration-only edges are deliberately excluded. New or
    replaced implementation/research/advice contracts and their meaningful
    dependencies are included.

    ``base_commit`` is only material for ``inherit`` workspaces, where it is an
    explicit delegation checkpoint. Collection/isolated workspaces historically
    had base_commit populated lazily during workspace preparation, so hashing that
    runtime value produced false critic staleness. ``legacy_raw_base_commit`` is
    retained only to recognize pre-0.1.23 seals that legitimately captured an
    explicit raw value.
    """
    material_tasks = [
        task for task in tasks
        if str(task.get("profile") or "") in _MATERIAL_PLAN_PROFILES
        and str(task.get("status") or "") not in {"CANCELLED", "SUPERSEDED"}
    ]
    material_tasks.sort(key=lambda task: (str(task.get("profile") or ""), str(task.get("id") or "")))
    material_ids = {str(task.get("id") or "") for task in material_tasks}

    task_payload = []
    for task in material_tasks:
        workspace_mode = str(task.get("workspace_mode") or "collection")
        raw_base_commit = str(task.get("base_commit") or "")
        canonical_base_commit = (
            raw_base_commit
            if legacy_raw_base_commit or workspace_mode == "inherit"
            else ""
        )
        task_payload.append({
            "id": str(task.get("id") or ""),
            "profile": str(task.get("profile") or ""),
            "title": normalize_text(str(task.get("title") or "")),
            "objective": normalize_text(str(task.get("objective") or "")),
            "reason": normalize_text(str(task.get("reason") or "")),
            "workspace_mode": workspace_mode,
            "base_commit": canonical_base_commit,
        })

    edge_payload = []
    for edge in edges:
        relation = str(edge.get("relation") or "")
        from_id = str(edge.get("from_task_id") or "")
        to_id = str(edge.get("to_task_id") or "")
        if relation not in _MATERIAL_PLAN_RELATIONS:
            continue
        if from_id not in material_ids or to_id not in material_ids:
            continue
        edge_payload.append((from_id, to_id, relation))
    edge_payload.sort()

    payload = {
        "objective": normalize_text(objective or ""),
        "tasks": task_payload,
        "edges": edge_payload,
    }
    encoded = json.dumps(payload, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode("utf-8")
    return hashlib.sha256(encoded).hexdigest()


@dataclass(frozen=True, slots=True)
class DeliveryPolicy:
    implementation_required: bool
    categories: tuple[str, ...]
    reasons: tuple[str, ...]

    def as_dict(self) -> dict[str, object]:
        return {
            "implementation_required": self.implementation_required,
            "categories": list(self.categories),
            "reasons": list(self.reasons),
        }


def classify_delivery_policy(objective: str) -> DeliveryPolicy:
    """Return a deterministic minimum delivery requirement for a Collection.

    Recognize both imperative requests ("Add...") and common conversational
    requests ("I'd like to add...", "Can you fix..."). Research/review/
    explanation objectives are not forced through a write path merely because
    they mention implementation details.
    """
    implementation_required = _requires_implementation_delivery(objective)
    categories: list[str] = []
    reasons: list[str] = []
    if implementation_required:
        categories.append("implementation_delivery")
        reasons.append(
            "The Collection objective is a direct change request; at least one completed implementation with passing independent review is required before COMPLETE."
        )
    return DeliveryPolicy(
        implementation_required=implementation_required,
        categories=tuple(categories),
        reasons=tuple(reasons),
    )
