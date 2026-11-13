from __future__ import annotations

from dataclasses import dataclass
import hashlib
import json
import re
from typing import Any

from .util import normalize_text

# GateCoherenceCutoff is the UTC date (YYYY-MM-DD) from which the deterministic
# planning risk gates are derived from the recorded risk categories rather than
# being ANDed with the objective's mutation-verb signal. Collections created on
# or after this date use the coherent derivation; collections created before it
# keep the historical verb-ANDed derivation so pre-existing work is never
# retroactively re-gated. This is a forward-scope guard, not a policy_version or
# per-collection marker; it mirrors internal/praector/policy.go exactly.
GATE_COHERENCE_CUTOFF = "2026-11-12"


@dataclass(frozen=True, slots=True)
class PlanningPolicy:
    explore_required: bool
    research_required: bool
    critic_required: bool
    plan_review_required: bool
    external_conformance_required: bool
    threat_model_required: bool
    categories: tuple[str, ...]
    reasons: tuple[str, ...]

    def as_dict(self) -> dict[str, object]:
        return {
            "explore_required": self.explore_required,
            "research_required": self.research_required,
            "critic_required": self.critic_required,
            "plan_review_required": self.plan_review_required,
            "external_conformance_required": self.external_conformance_required,
            "threat_model_required": self.threat_model_required,
            "categories": list(self.categories),
            "reasons": list(self.reasons),
        }


_SECURITY_PATTERNS = (
    r"\bauth(?:entication|orization)?\b", r"\bapi[-_ ]?key(?:s)?\b", r"\bcredential(?:s)?\b",
    r"\bsecret(?:s)?\b", r"\bpassword(?:s)?\b", r"\boauth\b", r"\btoken(?:s)?\b",
    r"\baccess[ -]?control\b", r"\bpermission(?:s)?\b", r"\bencrypt(?:ion|ed)?\b", r"\bsecurity\b",
)
_STATEFUL_PATTERNS = (
    r"\brate[ -]?limit(?:ing)?\b", r"\bstateful\b", r"\bsession(?:s)?\b", r"\bcache(?:d|s|ing)?\b",
    r"\bqueue(?:s)?\b", r"\bretr(?:y|ies)\b", r"\bidempoten(?:t|cy)\b", r"\bconcurren(?:t|cy)\b",
    r"\block(?:ing|s)?\b", r"\btransaction(?:s)?\b",
)
_MIGRATION_PATTERNS = (r"\bmigrat(?:e|es|ed|ing|ion|ions)\b", r"\bschema\b", r"\bdatabase\b", r"\bbackfill\b", r"\bdata conversion\b")
_DESTRUCTIVE_PATTERNS = (r"\bdelet(?:e|es|ed|ing|ion)\b", r"\bdrop(?:s|ped|ping)?\b", r"\bpurg(?:e|es|ed|ing)\b", r"\bdestructive\b", r"\birreversible\b")
_EXTERNAL_CONTRACT_PATTERNS = (r"\bapi\b", r"\bwebhook(?:s)?\b", r"\bpublic endpoint\b", r"\bbackward compatib(?:le|ility)\b")
_EXTERNAL_INTEGRATION_PATTERNS = (
    r"\bgateway(?:s)?\b", r"\bintegration(?:s)?\b", r"\bthird[- ]party\b", r"\bexternal service\b",
    r"\bexternal api\b", r"\bprovider(?:s)?\b", r"\bsdk\b", r"\btwilio\b", r"\btelephony\b",
    r"\btwiml\b", r"\bcloud\b",
)
_PROTOCOL_PATTERNS = (r"\bprotocol\b", r"\bspecification\b", r"\bstandard\b", r"\brfc\b", r"\bwire format\b", r"\bcompatibility matrix\b", r"\bmcp\b", r"\bmodel context protocol\b", r"\bopenapi\b")
_FRESHNESS_PATTERNS = (r"\blatest\b", r"\brecent\b", r"\bas[- ]of\b", r"\btoday\b", r"\bthis week\b", r"\bnewest\b", r"\bup[- ]to[- ]date\b", r"\bnews\b", r"\bcurrent events?\b", r"\bcurrent\s+(?:version|release|specification|standard|api|sdk|law|regulation|policy|events?|news|pricing|availability|support)\b")
_REGULATION_PATTERNS = (r"\blaw\b", r"\blegal\b", r"\bregulat(?:ion|ory|ions)\b", r"\bcompliance\b", r"\bpolicy requirement\b", r"\bhipaa\b", r"\bgdpr\b", r"\bsec rule\b")
_EVOLVING_DEPENDENCY_PATTERNS = (
    r"\blibrar(?:y|ies)\b", r"\bframework\b", r"\bvendor\b", r"\bsupport matrix\b",
    r"\b(?:package|dependency|library|framework)\s+(?:version|release|compatibility|support)\b",
    r"\b(?:upgrade|update)\s+(?:a\s+)?(?:package|dependency|library|framework)\b",
)
_NETWORK_BOUNDARY_PATTERNS = (r"\blistener\b", r"\bhttp server\b", r"\bserver\b", r"\bnetwork\b", r"\bbind\b", r"\bsocket\b", r"\bremote\b", r"\bwebhook\b", r"\binbound\b", r"\bpublic endpoint\b")

_MUTATION_VERBS = (
    "add|implement|build|create|modify|change|update|fix|repair|refactor|"
    "remove|delete|migrate|extend|introduce|replace|upgrade|convert|make|enforce|support"
)
_IMPLEMENTATION_FIRST_LINE = re.compile(rf"^(?:please\s+)?(?:{_MUTATION_VERBS})\b", flags=re.IGNORECASE)
_IMPLEMENTATION_REQUEST_PATTERNS = (
    rf"\b(?:i(?:'|’)d|i would|we would)\s+like\s+to\s+(?:{_MUTATION_VERBS})\b",
    rf"\b(?:i|we)\s+(?:want|need)\s+(?:you\s+)?to\s+(?:{_MUTATION_VERBS})\b",
    rf"\b(?:can|could|would)\s+you\s+(?:please\s+)?(?:{_MUTATION_VERBS})\b",
    rf"\bplease\s+(?:{_MUTATION_VERBS})\b", rf"\blet(?:'|’)s\s+(?:{_MUTATION_VERBS})\b",
)
_DOCUMENTATION_PATTERNS = (r"\breadme(?:\.md)?\b", r"\bdocumentation\b", r"\bdocs?\b", r"\btypo\b", r"\bspelling\b", r"\bwording\b", r"\bexample sentence\b")
_CODE_OR_RUNTIME_PATTERNS = (
    r"\bfeature\b", r"\bbug\b", r"\bcode(?:base)?\b", r"\bfunction\b", r"\bmethod\b", r"\bclass\b",
    r"\bendpoint\b", r"\broute\b", r"\bhandler\b", r"\bgateway\b", r"\btemplate\b", r"\bservice\b",
    r"\bclient\b", r"\bserver\b", r"\btest(?:s|ing)?\b", r"\bapi\b", r"\bwebhook\b", r"\btransfer\b",
    r"\bdtmf\b", r"\btwiml\b", r"\bdial\b", r"\b(?:py|js|ts|java|go|rs|rb|php|cs|cpp|c|h)\b",
    r"\.(?:py|pyi|js|jsx|ts|tsx|java|go|rs|rb|php|cs|cpp|cc|cxx|c|h|hpp|sql|sh)\b",
)


def _matches_any(text: str, patterns: tuple[str, ...]) -> bool:
    return any(re.search(pattern, text, flags=re.IGNORECASE) for pattern in patterns)


def _requires_implementation_delivery(objective: str) -> bool:
    text = objective or ""
    first_line = next((line.strip() for line in text.splitlines() if line.strip()), "")
    if _IMPLEMENTATION_FIRST_LINE.search(first_line):
        return True
    return _matches_any(text, _IMPLEMENTATION_REQUEST_PATTERNS)


def _is_trivial_documentation_change(objective: str) -> bool:
    text = objective or ""
    return _matches_any(text, _DOCUMENTATION_PATTERNS) and not _matches_any(text, _CODE_OR_RUNTIME_PATTERNS)


def _classify_planning_policy(objective: str, coherent: bool) -> PlanningPolicy:
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
    protocol = _matches_any(text, _PROTOCOL_PATTERNS)
    freshness = _matches_any(text, _FRESHNESS_PATTERNS)
    regulation = _matches_any(text, _REGULATION_PATTERNS)
    evolving = _matches_any(text, _EVOLVING_DEPENDENCY_PATTERNS)
    network_boundary = _matches_any(text, _NETWORK_BOUNDARY_PATTERNS)

    if implementation:
        categories.append("implementation_change")
        reasons.append("Implementation work requires a read-only repository exploration preflight before coordination.")
        reasons.append("Implementation work always requires an independent pre-planning critic.")
        if not trivial_docs:
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
    if protocol:
        categories.append("external_protocol_or_standard")
    if freshness:
        categories.append("freshness_sensitive")
    if regulation:
        categories.append("law_regulation_or_compliance")
    if evolving:
        categories.append("evolving_dependency_or_vendor")
    if network_boundary:
        categories.append("network_trust_boundary")

    research_required = bool(protocol or freshness or regulation or external_integration or (implementation and evolving))
    if research_required:
        reasons.append("Correctness depends on fresh external evidence; current authoritative research is required before critique/planning.")
    # Category-derived RISK gates. Coherent mode derives each gate from the
    # recorded risk category; legacy (pre-cutoff) mode ANDs each verb-dependent
    # risk gate with the mutation-verb mask to reproduce the historical verb-
    # gated behavior exactly. The plan-review `implementation and not trivial`
    # term, the delivery policy, and the research freshness triggers stay
    # verb-based in both modes.
    external_conformance_required = bool(protocol or external_integration)
    threat_model_required = bool(security or network_boundary)
    integration_sensitive_change = bool(external_contract or external_integration or protocol)
    any_risk = bool(security or stateful or migration or destructive or external_contract or external_integration or protocol or freshness or regulation or evolving or network_boundary)
    explore_required = bool(implementation or any_risk)
    if not coherent:
        external_conformance_required = bool(implementation and external_conformance_required)
        threat_model_required = bool(implementation and threat_model_required)
        integration_sensitive_change = bool(implementation and integration_sensitive_change)
        explore_required = implementation
    if external_conformance_required:
        reasons.append("External protocol/provider work requires an authoritative independent verification oracle when practical.")
    if threat_model_required:
        reasons.append("A network/security trust boundary requires explicit threat-model directives before write work.")

    critic_required = bool(implementation or security or migration or destructive or (stateful and external_contract) or integration_sensitive_change)
    plan_review_required = bool(stateful or migration or destructive or integration_sensitive_change or external_conformance_required or threat_model_required or (implementation and not trivial_docs))
    if plan_review_required and not any("plan-review" in reason.lower() or "plan review" in reason.lower() for reason in reasons):
        reasons.append("The objective crosses the deterministic threshold for an independent plan-review gate before write work.")

    return PlanningPolicy(
        explore_required=explore_required,
        research_required=research_required,
        critic_required=critic_required,
        plan_review_required=plan_review_required,
        external_conformance_required=external_conformance_required,
        threat_model_required=threat_model_required,
        categories=tuple(categories), reasons=tuple(reasons),
    )


def classify_planning_policy(objective: str) -> PlanningPolicy:
    return _classify_planning_policy(objective, True)


def _planning_gate_coherent(collection: dict[str, Any]) -> bool:
    created = str((collection or {}).get("created_at") or "")
    if len(created) < 10:
        return True
    return created[:10] >= GATE_COHERENCE_CUTOFF


def effective_planning_policy(collection: dict[str, Any]) -> PlanningPolicy:
    return _classify_planning_policy((collection or {}).get("objective") or "", _planning_gate_coherent(collection))


def gate_derivation_label(collection: dict[str, Any]) -> str:
    return "category" if _planning_gate_coherent(collection) else "verb_legacy"


_MATERIAL_PLAN_PROFILES = {"implement", "research", "advise"}
_MATERIAL_PLAN_RELATIONS = {"depends_on", "blocks", "supersedes", "superseded_by"}


def _research_result_digest(task: dict[str, object]) -> str:
    if str(task.get("profile") or "") != "research":
        return ""
    value: object | None = task.get("result")
    if value is None:
        raw = str(task.get("result_json") or "").strip()
        if raw:
            try:
                value = json.loads(raw)
            except Exception:
                value = raw
    if value is None:
        return ""
    encoded = json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode("utf-8")
    return hashlib.sha256(encoded).hexdigest()




def dependency_lineage_digest(dependencies: list[dict[str, object]]) -> str:
    """Hash the direct prerequisite state a task started from.

    This complements the material-plan fingerprint by catching a prerequisite
    task that is reopened or whose integrated head/result changes in place.
    """
    payload: list[dict[str, str]] = []
    for dep in sorted(dependencies, key=lambda item: str(item.get("id") or "")):
        raw = str(dep.get("result_json") or "").strip()
        payload.append({
            "id": str(dep.get("id") or ""),
            "status": str(dep.get("status") or ""),
            "current_head": str(dep.get("current_head") or ""),
            "result_digest": hashlib.sha256(raw.encode("utf-8")).hexdigest() if raw else "",
        })
    encoded = json.dumps(payload, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode("utf-8")
    return hashlib.sha256(encoded).hexdigest()


def _material_plan_fingerprint(objective: str, tasks: list[dict[str, object]], edges: list[dict[str, object]], *, legacy_raw_base_commit: bool = False, include_research_digest: bool = True) -> str:
    material_tasks = [task for task in tasks if str(task.get("profile") or "") in _MATERIAL_PLAN_PROFILES and str(task.get("status") or "") not in {"CANCELLED", "SUPERSEDED"}]
    material_tasks.sort(key=lambda task: (str(task.get("profile") or ""), str(task.get("id") or "")))
    material_ids = {str(task.get("id") or "") for task in material_tasks}
    task_payload: list[dict[str, object]] = []
    for task in material_tasks:
        workspace_mode = str(task.get("workspace_mode") or "collection")
        raw_base_commit = str(task.get("base_commit") or "")
        canonical_base_commit = raw_base_commit if legacy_raw_base_commit or workspace_mode == "inherit" else ""
        row: dict[str, object] = {
            "id": str(task.get("id") or ""), "profile": str(task.get("profile") or ""),
            "title": normalize_text(str(task.get("title") or "")), "objective": normalize_text(str(task.get("objective") or "")),
            "reason": normalize_text(str(task.get("reason") or "")), "workspace_mode": workspace_mode, "base_commit": canonical_base_commit,
        }
        if include_research_digest:
            digest = _research_result_digest(task)
            if digest:
                row["research_result_digest"] = digest
        task_payload.append(row)
    edge_payload: list[tuple[str, str, str]] = []
    for edge in edges:
        relation = str(edge.get("relation") or "")
        from_id, to_id = str(edge.get("from_task_id") or ""), str(edge.get("to_task_id") or "")
        if relation in _MATERIAL_PLAN_RELATIONS and from_id in material_ids and to_id in material_ids:
            edge_payload.append((from_id, to_id, relation))
    edge_payload.sort()
    payload = {"objective": normalize_text(objective or ""), "tasks": task_payload, "edges": edge_payload}
    encoded = json.dumps(payload, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode("utf-8")
    return hashlib.sha256(encoded).hexdigest()


def material_plan_fingerprint(objective: str, tasks: list[dict[str, object]], edges: list[dict[str, object]], *, legacy_raw_base_commit: bool = False) -> str:
    return _material_plan_fingerprint(
        objective, tasks, edges,
        legacy_raw_base_commit=legacy_raw_base_commit,
        include_research_digest=True,
    )


def material_plan_fingerprint_v021(objective: str, tasks: list[dict[str, object]], edges: list[dict[str, object]], *, legacy_raw_base_commit: bool = False) -> str:
    """Reproduce the 0.2.1 fingerprint contract for upgrade compatibility."""
    return _material_plan_fingerprint(
        objective, tasks, edges,
        legacy_raw_base_commit=legacy_raw_base_commit,
        include_research_digest=False,
    )


def validate_deterministic_research_result(result: dict[str, Any], *, require_oracle: bool) -> None:
    if not str(result.get("as_of") or "").strip():
        raise ValueError("deterministic fresh research result requires as_of")
    sources = result.get("sources")
    if not isinstance(sources, list) or not sources:
        raise ValueError("deterministic fresh research result requires non-empty sources")
    source_ids: set[str] = set()
    allowed_authority = {
        "official_specification", "official_standard", "official_release_notes", "official_sdk",
        "first_party", "vendor_documentation", "secondary", "community",
    }
    for idx, source in enumerate(sources):
        if not isinstance(source, dict):
            raise ValueError(f"research source {idx} must be an object")
        source_id = str(source.get("id") or "").strip()
        authority_level = str(source.get("authority_level") or "").strip().lower()
        if not source_id or not str(source.get("authority") or "").strip() or not str(source.get("reference") or "").strip() or not authority_level:
            raise ValueError(f"research source {idx} requires id, authority, authority_level, and reference")
        if authority_level not in allowed_authority:
            raise ValueError(f"research source {idx} has unsupported authority_level {authority_level!r}")
        if source_id in source_ids:
            raise ValueError(f"research source id {source_id!r} is duplicated")
        source_ids.add(source_id)
    claims = result.get("claims")
    if not isinstance(claims, list) or not claims:
        raise ValueError("deterministic fresh research result requires non-empty claims")
    allowed_norm = {"MUST", "MUST_NOT", "SHOULD", "MAY", "FACT", "INFERENCE"}
    allowed_effect = {"preserve", "strengthen", "exception"}
    claim_ids: set[str] = set()
    claim_directives: set[str] = set()
    for idx, claim in enumerate(claims):
        if not isinstance(claim, dict):
            raise ValueError(f"research claim {idx} must be an object")
        for key in ("id", "claim", "source", "authority", "normative_level", "planner_directive", "directive_effect", "confidence"):
            if not str(claim.get(key) or "").strip():
                raise ValueError(f"research claim {idx} requires {key}")
        claim_id = str(claim.get("id") or "").strip()
        if claim_id in claim_ids:
            raise ValueError(f"research claim id {claim_id!r} is duplicated")
        claim_ids.add(claim_id)
        source_id = str(claim.get("source") or "").strip()
        if source_id not in source_ids:
            raise ValueError(f"research claim {idx} references unknown source {source_id!r}")
        norm = str(claim["normative_level"]).strip().upper()
        if norm not in allowed_norm:
            raise ValueError(f"research claim {idx} has unsupported normative_level {norm!r}")
        effect = str(claim["directive_effect"]).strip().lower()
        if effect not in allowed_effect:
            raise ValueError(f"research claim {idx} directive_effect must be preserve, strengthen, or exception")
        if effect == "exception" and not str(claim.get("exception_decision_id") or "").strip():
            raise ValueError(f"research claim {idx} exception requires exception_decision_id")
        claim_directives.add(normalize_text(str(claim.get("planner_directive") or "")))

    for idx, directive in enumerate(result.get("planner_directives") or []):
        normalized = normalize_text(str(directive or ""))
        if not normalized or normalized not in claim_directives:
            raise ValueError(f"research planner_directive {idx} is not backed by a structured claim")

    oracles = result.get("verification_oracles")
    if require_oracle and (not isinstance(oracles, list) or not oracles):
        raise ValueError("external-conformance research requires at least one verification_oracle")
    allowed_oracles = {"internal_test", "black_box_test", "independent_implementation", "official_reference_oracle", "live_provider_oracle"}
    authoritative_oracle = False
    if isinstance(oracles, list):
        for idx, oracle in enumerate(oracles):
            if not isinstance(oracle, dict):
                raise ValueError(f"verification_oracle {idx} must be an object")
            kind = str(oracle.get("kind") or "").strip().lower()
            if kind not in allowed_oracles or not str(oracle.get("name") or "").strip():
                raise ValueError(f"verification_oracle {idx} requires a supported kind and name")
            if kind in {"independent_implementation", "official_reference_oracle", "live_provider_oracle"}:
                authoritative_oracle = True
    if require_oracle and not authoritative_oracle and not str(result.get("authoritative_oracle_unavailable_reason") or "").strip():
        raise ValueError("external-conformance research requires an authoritative independent oracle or authoritative_oracle_unavailable_reason")


def research_claim_constraints(tasks: list[dict[str, Any]]) -> list[dict[str, str]]:
    refs: list[dict[str, str]] = []
    counts: dict[str, int] = {}
    for task in tasks:
        if str(task.get("profile") or "") != "research" or str(task.get("status") or "") != "COMPLETE":
            continue
        result = task.get("result")
        if not isinstance(result, dict):
            if task.get("result_json"):
                try:
                    result = json.loads(str(task.get("result_json")))
                except (TypeError, ValueError, json.JSONDecodeError):
                    result = {}
            else:
                result = {}
        claims = result.get("claims") or []
        for index, claim in enumerate(claims):
            if not isinstance(claim, dict):
                raise ValueError(f"research task {task.get('id')} claim {index} must be an object")
            claim_id = str(claim.get("id") or "").strip()
            directive = str(claim.get("planner_directive") or "").strip()
            if not claim_id:
                raise ValueError(f"research task {task.get('id')} claim {index} is missing id")
            if not directive:
                raise ValueError(f"research task {task.get('id')} claim {claim_id} is missing planner_directive")
            refs.append({
                "task_id": str(task.get("id") or ""),
                "claim_id": claim_id,
                "planner_directive": directive,
                "normative_level": str(claim.get("normative_level") or "").strip().upper(),
                "authority": str(claim.get("authority") or "").strip(),
                "source": str(claim.get("source") or "").strip(),
            })
            counts[claim_id] = counts.get(claim_id, 0) + 1
    out: list[dict[str, str]] = []
    for ref in refs:
        item = dict(ref)
        item["claim_ref"] = (
            f"{ref['task_id']}:{ref['claim_id']}" if counts[ref["claim_id"]] > 1 else ref["claim_id"]
        )
        out.append(item)
    return sorted(out, key=lambda item: item["claim_ref"])


def research_claim_ids(tasks: list[dict[str, Any]]) -> list[str]:
    return [item["claim_ref"] for item in research_claim_constraints(tasks)]


def research_exception_decision_ids(result: dict[str, Any]) -> list[str]:
    out: set[str] = set()
    for claim in result.get("claims") or []:
        if not isinstance(claim, dict):
            continue
        if str(claim.get("directive_effect") or "").strip().lower() != "exception":
            continue
        decision_id = str(claim.get("exception_decision_id") or "").strip()
        if decision_id:
            out.add(decision_id)
    return sorted(out)


def research_consumer_exception_decision_ids(result: dict[str, Any]) -> list[str]:
    consistency = result.get("research_consistency")
    if not isinstance(consistency, dict):
        return []
    out: set[str] = set()
    for item in consistency.get("claims") or []:
        if not isinstance(item, dict):
            continue
        if str(item.get("disposition") or "").strip().lower() != "explicit_exception":
            continue
        decision_id = str(item.get("exception_decision_id") or "").strip()
        if decision_id:
            out.add(decision_id)
    return sorted(out)


def validate_research_consumer_result(result: dict[str, Any], research_tasks: list[dict[str, Any]], *, require_pass: bool) -> None:
    constraints = research_claim_constraints(research_tasks)
    if not constraints:
        return
    consistency = result.get("research_consistency")
    if not isinstance(consistency, dict):
        raise ValueError("result requires research_consistency for fresh research evidence")
    verdict = str(consistency.get("verdict") or "").strip().lower()
    if verdict not in {"pass", "fail"}:
        raise ValueError("research_consistency.verdict must be pass or fail")

    entries = consistency.get("claims")
    if not isinstance(entries, list) or not entries:
        raise ValueError("research_consistency.claims must map every structured research claim to an inherited directive disposition")
    by_ref: dict[str, dict[str, Any]] = {}
    for index, entry in enumerate(entries):
        if not isinstance(entry, dict):
            raise ValueError(f"research_consistency.claims[{index}] must be an object")
        claim_ref = str(entry.get("claim_id") or "").strip()
        if not claim_ref:
            raise ValueError(f"research_consistency.claims[{index}] requires claim_id")
        if claim_ref in by_ref:
            raise ValueError(f"research_consistency claim_id {claim_ref!r} is duplicated")
        by_ref[claim_ref] = entry

    expected = {item["claim_ref"]: item for item in constraints}
    missing = [claim_ref for claim_ref in expected if claim_ref not in by_ref]
    if missing:
        raise ValueError(f"research_consistency did not disposition claim ids: {','.join(missing)}")
    unknown = [claim_ref for claim_ref in by_ref if claim_ref not in expected]
    if unknown:
        raise ValueError(f"research_consistency references unknown claim ids: {','.join(sorted(unknown))}")

    allowed_dispositions = {"preserved", "strengthened", "explicit_exception"}
    for claim_ref, constraint in expected.items():
        entry = by_ref[claim_ref]
        disposition = str(entry.get("disposition") or "").strip().lower()
        if disposition not in allowed_dispositions:
            raise ValueError(
                f"research_consistency claim {claim_ref} disposition must be preserved, strengthened, or explicit_exception"
            )
        inherited = str(entry.get("inherited_directive") or "").strip()
        if normalize_text(inherited) != normalize_text(constraint["planner_directive"]):
            raise ValueError(f"research_consistency claim {claim_ref} must preserve the exact inherited research directive")
        derived = entry.get("derived_directives") or []
        if not isinstance(derived, list) or any(not str(value or "").strip() for value in derived):
            raise ValueError(f"research_consistency claim {claim_ref} derived_directives must be a list of non-empty strings")
        decision_id = str(entry.get("exception_decision_id") or "").strip()
        if disposition == "explicit_exception" and not decision_id:
            raise ValueError(f"research_consistency claim {claim_ref} explicit_exception requires exception_decision_id")
        if disposition != "explicit_exception" and decision_id:
            raise ValueError(f"research_consistency claim {claim_ref} may cite exception_decision_id only for explicit_exception")

    checked_raw = consistency.get("checked_claim_ids")
    if checked_raw is not None:
        checked = {str(value or "").strip() for value in checked_raw if str(value or "").strip()}
        missing_checked = [claim_ref for claim_ref in expected if claim_ref not in checked]
        if missing_checked:
            raise ValueError(f"research_consistency did not check claim ids: {','.join(missing_checked)}")

    contradictions = list(consistency.get("contradictions") or [])
    if verdict == "fail" and not contradictions:
        raise ValueError("failed research_consistency requires contradictions")
    if verdict == "pass" and contradictions:
        raise ValueError("passing research_consistency cannot report contradictions")
    if require_pass and verdict != "pass":
        raise ValueError("passing plan/review requires research_consistency verdict=pass")


def validate_external_conformance_review_result(result: dict[str, Any]) -> None:
    if str(result.get("verdict") or "").strip().lower() != "pass":
        return
    for oracle in result.get("verification_oracles") or []:
        if not isinstance(oracle, dict):
            continue
        kind = str(oracle.get("kind") or "").strip().lower()
        used = bool(oracle.get("used", True))
        if used and kind in {"independent_implementation", "official_reference_oracle", "live_provider_oracle"}:
            return
    if str(result.get("authoritative_oracle_unavailable_reason") or "").strip() and str(result.get("oracle_exception_decision_id") or "").strip():
        return
    raise ValueError("passing external-conformance review requires an authoritative oracle actually used, or a durable oracle exception decision")


@dataclass(frozen=True, slots=True)
class DeliveryPolicy:
    implementation_required: bool
    categories: tuple[str, ...]
    reasons: tuple[str, ...]

    def as_dict(self) -> dict[str, object]:
        return {"implementation_required": self.implementation_required, "categories": list(self.categories), "reasons": list(self.reasons)}


def classify_delivery_policy(objective: str) -> DeliveryPolicy:
    implementation_required = _requires_implementation_delivery(objective)
    categories: list[str] = []
    reasons: list[str] = []
    if implementation_required:
        categories.append("implementation_delivery")
        reasons.append("The Collection objective is a direct change request; at least one completed implementation with passing independent review is required before COMPLETE.")
    return DeliveryPolicy(implementation_required=implementation_required, categories=tuple(categories), reasons=tuple(reasons))
