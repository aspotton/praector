from __future__ import annotations

import contextlib
import fcntl
import json
import os
import signal
import time
from concurrent.futures import Future, ThreadPoolExecutor
from pathlib import Path
from typing import Any, Iterator

from .config import GlobalConfig, READ_ONLY_PROFILES, project_config_from_row
from .db import DependencyCycleError, DoltStore
from .envelope import build_task_envelope
from .harness import create_harness
from .models import AttemptStatus, TaskStatus
from .planning_policy import (
    classify_delivery_policy, classify_planning_policy, dependency_lineage_digest, material_plan_fingerprint, material_plan_fingerprint_v021,
    validate_research_consumer_result,
)
from .workspace import GitWorkspace


class ResearchCapabilityError(RuntimeError):
    def __init__(self, collection_id: str):
        super().__init__(
            f"collection {collection_id} requires fresh external research, but project capability current_external_research=false"
        )
        self.collection_id = collection_id


class Scheduler:
    def __init__(self, store: DoltStore, cfg: GlobalConfig, *, max_workers: int | None = None):
        self.store = store
        self.cfg = cfg
        self.workspace = GitWorkspace(store, cfg)
        self.max_workers_override = max_workers
        self._quarantined_collections: set[str] = set()
        self._reported_cycle_quarantines: set[str] = set()

    @contextlib.contextmanager
    def exclusive_scheduler(self) -> Iterator[None]:
        self.cfg.scheduler_lock.parent.mkdir(parents=True, exist_ok=True)
        with self.cfg.scheduler_lock.open("a+") as fh:
            try:
                fcntl.flock(fh.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError as exc:
                raise RuntimeError("Another praector scheduler/daemon is already running") from exc
            try:
                yield
            finally:
                fcntl.flock(fh.fileno(), fcntl.LOCK_UN)

    def recover_stale_sessions(self) -> int:
        recovered = 0
        for row in self.store.running_sessions():
            pid_raw = row.get("pid")
            pid = int(pid_raw) if pid_raw not in (None, "") else None
            if pid:
                try:
                    # OpenCode is started as its own process group. Terminate the
                    # group because a previous scheduler cannot safely collect it.
                    os.killpg(pid, signal.SIGTERM)
                    time.sleep(0.1)
                    try:
                        os.killpg(pid, 0)
                    except ProcessLookupError:
                        pass
                    else:
                        os.killpg(pid, signal.SIGKILL)
                except (ProcessLookupError, PermissionError):
                    pass
            self.store.abort_stale_session(row["id"], row["attempt_id"], row["task_id"])
            recovered += 1
        return recovered

    def _limits_dict(self, project: dict[str, Any]) -> dict[str, int]:
        cfg = project_config_from_row(project)
        return {
            "max_task_depth": cfg.limits.max_task_depth,
            "max_tasks_per_collection": cfg.limits.max_tasks_per_collection,
            "max_children_per_task": cfg.limits.max_children_per_task,
            "max_attempts_per_task": cfg.limits.max_attempts_per_task,
        }

    def _record_task_input_lineage(self, task: dict[str, Any], attempt: dict[str, Any]) -> None:
        fingerprint = self.store.material_plan_fingerprint(task["collection_id"])
        dependencies = self.store.dependencies(task["id"])
        self.store.event(
            task["project_id"], task["collection_id"], task["id"], attempt["id"],
            "TASK_INPUT_LINEAGE",
            {
                "material_plan_fingerprint": fingerprint,
                "dependency_lineage_digest": dependency_lineage_digest(dependencies),
                "dependency_ids": [str(dep.get("id") or "") for dep in dependencies],
                "profile": str(task.get("profile") or ""),
            },
        )

    def _task_input_lineage_changed(self, task: dict[str, Any]) -> tuple[dict[str, Any] | None, bool]:
        lineage_reader = getattr(self.store, "task_input_lineage", None)
        if not callable(lineage_reader):
            return None, False
        start = lineage_reader(task["id"])
        if not start:
            return None, False
        fingerprint = self.store.material_plan_fingerprint(task["collection_id"])
        dependencies = self.store.dependencies(task["id"])
        current_dep = dependency_lineage_digest(dependencies)
        changed = (
            str(start.get("material_plan_fingerprint") or "") != fingerprint
            or str(start.get("dependency_lineage_digest") or "") != current_dep
        )
        if not changed:
            return start, False
        return {
            "start_material_plan_fingerprint": str(start.get("material_plan_fingerprint") or ""),
            "current_material_plan_fingerprint": fingerprint,
            "start_dependency_lineage_digest": str(start.get("dependency_lineage_digest") or ""),
            "current_dependency_lineage_digest": current_dep,
            "attempt_id": str(start.get("attempt_id") or ""),
        }, True

    def _material_plan_fingerprint(
        self, collection: dict[str, Any], tasks: list[dict[str, Any]], *, legacy_raw_base_commit: bool = False
    ) -> str:
        return material_plan_fingerprint(
            str(collection.get("objective") or ""),
            tasks,
            self.store.list_edges(collection["id"]),
            legacy_raw_base_commit=legacy_raw_base_commit,
        )

    def _ensure_material_critic_revalidation(
        self,
        collection: dict[str, Any],
        active: list[dict[str, Any]],
        policy: Any,
        critic: dict[str, Any] | None,
        plan_review: dict[str, Any] | None,
    ) -> tuple[dict[str, Any] | None, bool]:
        """Require a fresh critic when a sealed material plan changes.

        The initial critic is intentionally pre-plan. It is sealed to the first
        critic-informed plan when final plan review is requested. After that, a
        material contract/dependency change invalidates the seal. Praector then runs
        a fresh critic against the changed graph and a fresh coordinator consumes
        those findings before plan review may proceed.
        """
        if not policy.critic_required or critic is None:
            return critic, False
        if critic.get("status") != TaskStatus.COMPLETE.value:
            return critic, False
        baseline = self.store.critic_plan_baseline(critic["id"])
        if not baseline or not baseline.get("fingerprint"):
            return critic, False

        current_fingerprint = self._material_plan_fingerprint(collection, active)
        legacy_fingerprint = self._material_plan_fingerprint(
            collection, active, legacy_raw_base_commit=True
        )
        edges = self.store.list_edges(collection["id"])
        v021_fingerprint = material_plan_fingerprint_v021(
            str(collection.get("objective") or ""), active, edges
        )
        v021_legacy_fingerprint = material_plan_fingerprint_v021(
            str(collection.get("objective") or ""), active, edges, legacy_raw_base_commit=True
        )
        if str(baseline.get("fingerprint")) in {
            current_fingerprint, legacy_fingerprint, v021_fingerprint, v021_legacy_fingerprint
        }:
            return critic, False

        # Let a currently-running/ready coordinator finish publishing its repair
        # coherently. Claim-time gates keep plan review and implementation from
        # running against the stale critic in the meantime. WAITING coordinators
        # are not shaping the graph and do not suppress revalidation.
        shaping_coordinators = [
            task for task in active
            if task["profile"] == "coordinator"
            and task["status"] in {
                TaskStatus.PENDING.value, TaskStatus.READY.value, TaskStatus.RUNNING.value,
                TaskStatus.VERIFYING.value,
            }
        ]
        if shaping_coordinators:
            return critic, False

        parent = next((task for task in reversed(active) if task["profile"] == "coordinator"), None)
        research_deps = [task["id"] for task in active if task["profile"] == "research"]
        fresh_critic, created = self.store.create_task(
            project_id=collection["project_id"],
            collection_id=collection["id"],
            profile="critic",
            title="Revalidate materially changed plan",
            objective=(
                "Re-run the independent planning preflight because the durable material plan changed after the previous critic was sealed. "
                "Inspect the CURRENT implementation/research/advice contracts and dependencies. Consume all current research dependencies and explicitly "
                "check their structured claim ids for evidence-to-directive consistency. Identify any new hidden requirements, "
                "security/state/concurrency hazards, scope changes, assumptions, and executable QA changes introduced by the new plan. "
                "Return concrete MUST/MUST NOT/VERIFY guidance. Do not implement or mutate the graph."
            ),
            reason=(
                f"Material plan fingerprint changed after critic {critic['id']}: "
                f"{baseline.get('fingerprint')} -> {current_fingerprint}"
            ),
            parent_task_id=parent["id"] if parent else None,
            requested_by_task_id=None,
            depends_on=research_deps,
            workspace_mode="collection",
            depth=(int(parent.get("depth") or 0) + 1) if parent else 1,
            allow_dedupe=False,
        )
        if not created:
            return fresh_critic, False
        self.store.record_critic_plan_baseline(
            fresh_critic["id"], current_fingerprint, source="material_revalidation"
        )
        if parent:
            self.store.add_edge(parent["id"], fresh_critic["id"], collection["id"], "spawned_by")

        # A coordinator must consume the fresh critic before review. This is a
        # new coordinator rather than the original final-handoff coordinator,
        # which may already be WAITING on plan_review and cannot safely resume.
        repair_coordinator, _ = self.store.create_task(
            project_id=collection["project_id"],
            collection_id=collection["id"],
            profile="coordinator",
            title="Apply critic revalidation",
            objective=(
                f"Consume critic {fresh_critic['id']} after material plan change. Re-evaluate the current graph using its findings. "
                "If the findings require material changes, make them; Praector will require another critic round. "
                "If no material change is required, preserve the material contracts. Ensure there is exactly one final blocking plan-review gate; "
                "if an existing nonterminal plan_review already gates the graph, do not create a duplicate."
            ),
            reason=f"Fresh critic {fresh_critic['id']} must be incorporated before final plan review.",
            parent_task_id=fresh_critic["id"],
            requested_by_task_id=None,
            depends_on=[fresh_critic["id"]],
            workspace_mode="collection",
            depth=int(fresh_critic.get("depth") or 0) + 1,
            allow_dedupe=False,
        )
        self.store.add_edge(fresh_critic["id"], repair_coordinator["id"], collection["id"], "spawned_by")

        if plan_review is not None and plan_review.get("status") not in {
            TaskStatus.COMPLETE.value, TaskStatus.CANCELLED.value, TaskStatus.SUPERSEDED.value,
        }:
            self.store.add_dependency(plan_review["id"], fresh_critic["id"])
            self.store.add_dependency(plan_review["id"], repair_coordinator["id"])

        self.store.event(
            collection["project_id"], collection["id"], fresh_critic["id"], None,
            "CRITIC_REVALIDATION_REQUIRED",
            {
                "stale_critic_task_id": critic["id"],
                "fresh_critic_task_id": fresh_critic["id"],
                "coordinator_task_id": repair_coordinator["id"],
                "previous_fingerprint": baseline.get("fingerprint"),
                "current_fingerprint": current_fingerprint,
                "plan_review_task_id": plan_review["id"] if plan_review else None,
            },
        )
        return fresh_critic, True

    @staticmethod
    def _research_request_question(value: Any) -> str:
        if isinstance(value, str):
            return value.strip()
        if isinstance(value, dict):
            return str(value.get("question") or value.get("query") or "").strip()
        return ""

    def _enforce_research_requests(self, collection_id: str) -> bool:
        tasks = self.store.list_tasks(collection_id)
        collection = self.store.get_collection(collection_id)
        policy = classify_planning_policy(collection.get("objective") or "")
        changed = False
        for caller in tasks:
            if caller.get("status") != TaskStatus.WAITING.value or caller.get("profile") not in {"critic", "review", "plan_review", "advise"}:
                continue
            result = caller.get("result") or {}
            requests = result.get("research_required")
            if not isinstance(requests, list) or not requests:
                continue
            if any(task.get("profile") == "research" and task.get("requested_by_task_id") == caller["id"] and task.get("status") not in {TaskStatus.CANCELLED.value, TaskStatus.SUPERSEDED.value} for task in tasks):
                continue
            project_cfg = project_config_from_row(self.store.get_project(collection["project_id"]))
            if not project_cfg.capabilities.current_external_research:
                raise ResearchCapabilityError(collection_id)
            question = self._research_request_question(requests[0]) or "Resolve the externally-verifiable uncertainty identified by the requesting task."
            self.store.create_task(
                project_id=collection["project_id"], collection_id=collection_id,
                profile="research", title=f"Research requested by {caller['id']}",
                objective=(
                    f"Resolve this bounded fresh external research question before {caller['profile']} {caller['id']} resumes: {question} "
                    "Use current authoritative retrieval rather than model memory. Return structured as_of, sources, claims, verification_oracles, risks, unresolved, summary. "
                    "Each source must include id, authority, authority_level, reference, and version/publication_date when available; authority_level must be one of "
                    "official_specification, official_standard, official_release_notes, official_sdk, first_party, vendor_documentation, secondary, community. "
                    "Each material claim must have a stable id, reference one source id, and include claim, authority, normative_level, planner_directive, directive_effect, confidence. "
                    "Any legacy top-level planner_directives must exactly match structured claim planner_directives. Classify verification oracles and surface contradictions explicitly."
                ),
                reason=f"Structured RESEARCH_REQUIRED escalation from {caller['profile']}",
                parent_task_id=caller["id"], requested_by_task_id=caller["id"], workspace_mode="collection",
                depth=int(caller.get("depth") or 0)+1, allow_dedupe=False,
                spawned_by_task_id=caller["id"], blocking_caller_task_id=caller["id"],
            )
            self.store.event(collection["project_id"], collection_id, caller["id"], None, "RESEARCH_REQUIRED", {
                "requesting_profile": caller["profile"], "question": question,
                "external_conformance_required": policy.external_conformance_required,
            })
            changed = True
        return changed

    def _enforce_planning_policy(self, collection_id: str) -> bool:
        """Ensure deterministic discovery, critique, and pre-write planning gates.

        Implementation Collections receive an exploration and critic preflight
        before coordinator planning. Plan review remains the independent final
        pre-write gate for objectives that require it.
        """
        collection = self.store.get_collection(collection_id)
        policy = classify_planning_policy(collection.get("objective") or "")
        if not (policy.explore_required or policy.research_required or policy.critic_required or policy.plan_review_required):
            return False

        tasks = self.store.list_tasks(collection_id)
        active = [
            task for task in tasks
            if task["status"] not in {TaskStatus.CANCELLED.value, TaskStatus.SUPERSEDED.value}
        ]
        changed = False
        reason_text = "; ".join(policy.reasons) or "Deterministic planning policy requires pre-write gates."

        # Implementation Collections begin with deterministic discovery and
        # critique before the coordinator shapes a material plan. Because this
        # runs during reconciliation it also applies to acceptance revisions.
        def select_generation_coordinator(rows):
            if collection.get("status") == "COMPLETE":
                return None
            return next(
                (
                    task for task in reversed(rows)
                    if task["profile"] == "coordinator"
                    and task.get("title") in {"Initial coordination", "Acceptance revision"}
                    and (
                        task["status"] in {TaskStatus.PENDING.value, TaskStatus.READY.value, TaskStatus.WAITING.value}
                        or (policy.research_required and task["status"] == TaskStatus.COMPLETE.value)
                    )
                ),
                None,
            )

        latest_coordinator = select_generation_coordinator(active)
        def latest_for_coordinator(profile: str):
            for task in reversed(active):
                if task["profile"] != profile:
                    continue
                if latest_coordinator is None or task.get("requested_by_task_id") == latest_coordinator["id"]:
                    return task
            return None

        latest_explore = latest_for_coordinator("explore")
        if policy.explore_required and latest_coordinator is not None and latest_explore is None:
            parent_id = latest_coordinator["id"] if latest_coordinator else None
            depth = int(latest_coordinator.get("depth") or 0) + 1 if latest_coordinator else 1
            blocking = None
            if latest_coordinator and latest_coordinator["status"] in {
                TaskStatus.PENDING.value, TaskStatus.READY.value, TaskStatus.WAITING.value,
            }:
                blocking = latest_coordinator["id"]
            self.store.create_task(
                project_id=collection["project_id"], collection_id=collection_id,
                profile="explore", title="Explore current implementation surface",
                objective=(
                    "Before planning, inspect the existing repository/system surface relevant to the Collection objective. "
                    "Map relevant files/modules, entry points, callers/dependencies, shared state, configuration/runtime boundaries, "
                    "tests, documentation, similar existing patterns, and compatibility/concurrency/platform constraints. Return a concise "
                    "evidence-grounded current-state map for the critic and coordinator. Do not design the solution or implement changes; "
                    "for a greenfield repository, say so explicitly and stop early."
                ),
                reason="Deterministic planning policy: repository exploration is required before implementation planning.",
                parent_task_id=parent_id, requested_by_task_id=parent_id,
                workspace_mode="collection", depth=depth, allow_dedupe=False,
                spawned_by_task_id=parent_id, blocking_caller_task_id=blocking,
            )
            changed = True
            tasks = self.store.list_tasks(collection_id)
            active = [
                task for task in tasks
                if task["status"] not in {TaskStatus.CANCELLED.value, TaskStatus.SUPERSEDED.value}
            ]
            latest_coordinator = select_generation_coordinator(active)
            latest_explore = latest_for_coordinator("explore")

        latest_research = latest_for_coordinator("research")
        if policy.research_required and latest_coordinator is not None and latest_research is None:
            project_cfg = project_config_from_row(self.store.get_project(collection["project_id"]))
            if not project_cfg.capabilities.current_external_research:
                raise ResearchCapabilityError(collection_id)
            parent_id = latest_coordinator["id"]
            depth = int(latest_coordinator.get("depth") or 0) + 1
            blocking = parent_id if latest_coordinator["status"] in {
                TaskStatus.PENDING.value, TaskStatus.READY.value, TaskStatus.WAITING.value,
            } else None
            oracle_text = (
                "Classify verification oracles as internal_test, black_box_test, independent_implementation, "
                "official_reference_oracle, or live_provider_oracle."
            )
            if policy.external_conformance_required:
                oracle_text += (
                    " At least one authoritative independent oracle is required when practical; an implementation-owned "
                    "hand-written client is black_box_test, not official conformance."
                )
            self.store.create_task(
                project_id=collection["project_id"], collection_id=collection_id,
                profile="research", title="Fresh external research preflight",
                objective=(
                    "Before critique/material planning, research the CURRENT external facts that can change correctness for this Collection. "
                    "Use fresh retrieval from authoritative first-party/standards sources rather than model memory. Deterministic policy categories: "
                    f"{', '.join(policy.categories)}. Return structured JSON with as_of, sources, claims, verification_oracles, risks, unresolved, and summary. "
                    "Each source MUST be an object with id, authority, authority_level, reference, and version/publication_date when available; authority_level must be one of "
                    "official_specification, official_standard, official_release_notes, official_sdk, first_party, vendor_documentation, secondary, community. "
                    "Each material claim MUST have a stable id, MUST reference one source id, and MUST include claim, authority, normative_level (MUST/MUST_NOT/SHOULD/MAY/FACT/INFERENCE), "
                    "planner_directive, directive_effect (preserve/strengthen/exception), confidence, and exception_decision_id when directive_effect=exception. "
                    "Structured claims are authoritative; any legacy top-level planner_directives MUST exactly match claim planner_directives. "
                    "Prefer current official specifications/standards, official release notes, official SDK/reference implementations, then other primary sources. "
                    f"Explicitly surface source conflicts and version/date scope. {oracle_text} If no authoritative independent oracle is practical, state "
                    "authoritative_oracle_unavailable_reason rather than misclassifying an implementation-owned black-box test. Do not implement or mutate the graph."
                ),
                reason="Deterministic planning policy: fresh external evidence is required before critique/planning.",
                parent_task_id=parent_id, requested_by_task_id=parent_id,
                workspace_mode="collection", depth=depth, allow_dedupe=False,
                spawned_by_task_id=parent_id, blocking_caller_task_id=blocking,
            )
            changed = True
            tasks = self.store.list_tasks(collection_id)
            active = [task for task in tasks if task["status"] not in {TaskStatus.CANCELLED.value, TaskStatus.SUPERSEDED.value}]
            latest_coordinator = select_generation_coordinator(active)
            latest_explore = latest_for_coordinator("explore")
            latest_research = latest_for_coordinator("research")

        critic_objective = (
            "Independently preflight the Collection objective before material planning. Consume completed repository exploration and fresh external research "
            "as primary evidence when present; explicitly compare local reality to authoritative external requirements and challenge evidence->directive contradictions. "
            "Deterministic policy categories: "
            f"{', '.join(policy.categories) or 'implementation'}. Treat inherited_research_constraints as canonical: for every research claim, preserve the exact "
            "inherited directive, strengthen it without replacing it, or cite a durable explicit exception. research_consistency.claims must record one disposition "
            "per inherited claim. Identify hidden requirements, scope hazards, security/state/concurrency semantics, assumptions, executable QA directives, and any "
            "acceptance contradiction. For network/security trust boundaries, explicitly threat-model exposure/bind behavior, authn/authz, origin/rebinding/CSRF-style "
            "threats where applicable, secrets/non-disclosure, privilege boundaries, input validation, rate/bounds, and auditability. For external protocol/provider work, "
            "require an authoritative independent oracle when practical and distinguish official reference evidence from implementation-owned black-box tests. Return "
            "concrete MUST/MUST NOT/VERIFY guidance for the coordinator; do not implement or mutate the graph."
        )

        latest_critic = latest_for_coordinator("critic")
        if policy.research_required and latest_coordinator is not None and latest_critic is not None and latest_critic["status"] == TaskStatus.COMPLETE.value:
            research_deps = self.store.research_ancestors(latest_critic["id"])
            if research_deps:
                result = latest_critic.get("result")
                if not isinstance(result, dict):
                    try:
                        result = json.loads(latest_critic.get("result_json") or "{}")
                    except (TypeError, ValueError, json.JSONDecodeError):
                        result = {}
                try:
                    validate_research_consumer_result(result or {}, research_deps, require_pass=False)
                except ValueError as consistency_error:
                    deps = []
                    if latest_explore:
                        deps.append(latest_explore["id"])
                    if latest_research:
                        deps.append(latest_research["id"])
                    replacement_critic, _ = self.store.create_task(
                        project_id=collection["project_id"], collection_id=collection_id,
                        profile="critic", title="Revalidate inherited research constraints", objective=critic_objective,
                        reason=(
                            "Current planning policy requires per-claim inherited research dispositions; prior critic result is not valid under "
                            f"the active generation contract: {consistency_error}"
                        ),
                        parent_task_id=latest_coordinator["id"], requested_by_task_id=latest_coordinator["id"],
                        depends_on=deps, workspace_mode="collection", depth=int(latest_coordinator.get("depth") or 0) + 1, allow_dedupe=False,
                    )
                    self.store.supersede_task(
                        latest_critic["id"], replacement_critic["id"],
                        "Revalidate active-generation critic under the current inherited research-constraint contract.",
                    )
                    repair_coordinator_id = None
                    if latest_coordinator["status"] == TaskStatus.COMPLETE.value:
                        repair_coordinator, _ = self.store.create_task(
                            project_id=collection["project_id"], collection_id=collection_id,
                            profile="coordinator", title="Apply inherited research revalidation",
                            objective=(
                                f"Consume replacement critic {replacement_critic['id']} after engine-contract revalidation. "
                                "Reconcile the CURRENT material task contracts against inherited_research_constraints. Remove or correct any "
                                "task-objective prose that conflicts with canonical research directives. Preserve scope otherwise. Ensure a "
                                "fresh final plan-review gate evaluates the reconciled graph before any write task runs."
                            ),
                            reason=(
                                "The original planning coordinator had already completed before the stronger research-constraint contract was applied; "
                                "a fresh coordinator must reconcile its material contracts."
                            ),
                            parent_task_id=replacement_critic["id"], requested_by_task_id=replacement_critic["id"],
                            depends_on=[replacement_critic["id"]], workspace_mode="collection",
                            depth=int(replacement_critic.get("depth") or 0) + 1, allow_dedupe=False,
                        )
                        repair_coordinator_id = repair_coordinator["id"]
                    self.store.event(
                        collection["project_id"], collection_id, latest_critic["id"], None,
                        "RESEARCH_CONSISTENCY_REVALIDATION_REQUIRED",
                        {
                            "replacement_critic_task_id": replacement_critic["id"],
                            "repair_coordinator_task_id": repair_coordinator_id,
                            "reason": str(consistency_error),
                        },
                    )
                    changed = True
                    tasks = self.store.list_tasks(collection_id)
                    active = [task for task in tasks if task["status"] not in {TaskStatus.CANCELLED.value, TaskStatus.SUPERSEDED.value}]
                    latest_critic = next(
                        (task for task in reversed(active) if task["profile"] == "critic" and task["status"] not in {TaskStatus.CANCELLED.value, TaskStatus.SUPERSEDED.value}),
                        None,
                    )

        if policy.critic_required and latest_coordinator is not None and latest_critic is None:
            parent_id = latest_coordinator["id"] if latest_coordinator else None
            depth = int(latest_coordinator.get("depth") or 0) + 1 if latest_coordinator else 1
            blocking = None
            if latest_coordinator and latest_coordinator["status"] in {
                TaskStatus.PENDING.value, TaskStatus.READY.value, TaskStatus.WAITING.value,
            }:
                blocking = latest_coordinator["id"]
            deps = []
            if latest_explore:
                deps.append(latest_explore["id"])
            if latest_research:
                deps.append(latest_research["id"])
            self.store.create_task(
                project_id=collection["project_id"], collection_id=collection_id,
                profile="critic", title="Preflight implementation plan",
                objective=critic_objective,
                reason="Deterministic planning policy: every implementation Collection receives an independent critic.",
                parent_task_id=parent_id, requested_by_task_id=parent_id,
                depends_on=deps, workspace_mode="collection", depth=depth, allow_dedupe=False,
                spawned_by_task_id=parent_id, blocking_caller_task_id=blocking,
            )
            changed = True
            tasks = self.store.list_tasks(collection_id)
            active = [
                task for task in tasks
                if task["status"] not in {TaskStatus.CANCELLED.value, TaskStatus.SUPERSEDED.value}
            ]

        write_tasks = [
            task for task in active
            if task["profile"] == "implement"
            and task["status"] in {TaskStatus.PENDING.value, TaskStatus.READY.value}
        ]
        if not write_tasks:
            return changed

        # Repair the specific impossible graph produced when a coordinator made
        # final plan_review depend on an implement task while Praector also gated the
        # implement on that same plan_review. A plan reviewer evaluates the
        # durable implementation contract before execution; it must not wait for
        # implementation completion. Restrict automatic repair to reciprocal
        # edges so unrelated historical/post-write graph shapes are not rewritten.
        plan_review_tasks = [task for task in active if task["profile"] == "plan_review"]
        write_ids = {task["id"] for task in write_tasks}

        # A pre-write plan review may wait for evidence, but never for a
        # coordinator that already waits (directly or transitively) on that same
        # review. 0.1.27 only checked the direct case; the Twilio recovery graph
        # exposed a four-node cycle through two coordinators and two plan reviews.
        # Remove only the coordinator->plan_review edge that closes such a cycle.
        coordinator_cycle_repairs: list[tuple[dict[str, Any], dict[str, Any], list[str]]] = []
        for review_task in plan_review_tasks:
            review_deps = self.store.dependencies(review_task["id"])
            for dep in review_deps:
                if dep.get("profile") != "coordinator":
                    continue
                path = self.store.dependency_path(review_task["id"], dep["id"])
                if path is not None:
                    coordinator_cycle_repairs.append((review_task, dep, path))
        # Compute against the same pre-repair graph so removing one edge cannot
        # hide another edge that independently participates in the same SCC.
        for review_task, dep, path in coordinator_cycle_repairs:
            self.store.remove_dependency(review_task["id"], dep["id"])
            self.store.event(
                collection["project_id"], collection_id, review_task["id"], None,
                "PLAN_REVIEW_COORDINATOR_CYCLE_REPAIRED",
                {
                    "plan_review_task_id": review_task["id"],
                    "coordinator_task_id": dep["id"],
                    "removed_edge": f"{dep['id']}->{review_task['id']}",
                    "existing_return_path": path,
                },
            )
            changed = True

        # There is one final pre-write plan-review gate for the current material
        # graph. If repair activity produced multiple unstarted gates, keep the
        # newest and supersede older READY/PENDING/WAITING gates. Supersession
        # rewires waiting coordinators and implementations to the surviving gate;
        # the DB cycle invariant rejects any obsolete prerequisite that would
        # recreate a loop. Running/verifying reviews are never collapsed here.
        unstarted_reviews = [
            task for task in plan_review_tasks
            if task["status"] in {
                TaskStatus.PENDING.value, TaskStatus.READY.value, TaskStatus.WAITING.value,
            }
        ]
        if len(unstarted_reviews) > 1:
            surviving_review = unstarted_reviews[-1]
            for obsolete_review in unstarted_reviews[:-1]:
                self.store.supersede_task(
                    obsolete_review["id"],
                    surviving_review["id"],
                    "Collapse redundant unstarted final plan-review gates after graph repair.",
                )
                self.store.event(
                    collection["project_id"], collection_id, obsolete_review["id"], None,
                    "REDUNDANT_PLAN_REVIEW_COLLAPSED",
                    {
                        "obsolete_plan_review_task_id": obsolete_review["id"],
                        "surviving_plan_review_task_id": surviving_review["id"],
                    },
                )
                changed = True
            tasks = self.store.list_tasks(collection_id)
            active = [
                task for task in tasks
                if task["status"] not in {TaskStatus.CANCELLED.value, TaskStatus.SUPERSEDED.value}
            ]
            write_tasks = [
                task for task in active
                if task["profile"] == "implement"
                and task["status"] in {TaskStatus.PENDING.value, TaskStatus.READY.value}
            ]
            write_ids = {task["id"] for task in write_tasks}
            plan_review_tasks = [task for task in active if task["profile"] == "plan_review"]

        for review_task in plan_review_tasks:
            review_dep_ids = {dep["id"] for dep in self.store.dependencies(review_task["id"])}
            for implement_id in sorted(review_dep_ids & write_ids):
                implement_dep_ids = {dep["id"] for dep in self.store.dependencies(implement_id)}
                if review_task["id"] not in implement_dep_ids:
                    continue
                self.store.remove_dependency(review_task["id"], implement_id)
                self.store.event(
                    collection["project_id"], collection_id, review_task["id"], None,
                    "PLAN_REVIEW_DEPENDENCY_CYCLE_REPAIRED",
                    {
                        "plan_review_task_id": review_task["id"],
                        "implement_task_id": implement_id,
                        "removed_edge": f"{implement_id}->{review_task['id']}",
                        "preserved_gate": f"{review_task['id']}->{implement_id}",
                    },
                )
                changed = True

        critics = [task for task in active if task["profile"] == "critic"]
        critic = critics[-1] if critics else None
        researches = [task for task in active if task["profile"] == "research"]
        research_task = researches[-1] if researches else None
        if policy.research_required and research_task is None:
            project_cfg = project_config_from_row(self.store.get_project(collection["project_id"]))
            if not project_cfg.capabilities.current_external_research:
                raise ResearchCapabilityError(collection_id)
            parent = next((task for task in reversed(active) if task["profile"] == "coordinator"), None)
            research_task, _ = self.store.create_task(
                project_id=collection["project_id"], collection_id=collection_id,
                profile="research", title="Policy fresh external research",
                objective=(
                    f"Research current authoritative external facts required by this Collection before write work. Categories: {', '.join(policy.categories)}. "
                    "Use fresh retrieval, not model memory. Return structured as_of, sources, claims, verification_oracles, risks, unresolved, summary. "
                    "Sources require id, authority, authority_level, reference; claims require stable id, source, authority, normative_level, planner_directive, directive_effect, confidence. "
                    "Structured claims are authoritative; any top-level planner_directives must exactly match claim directives. Distinguish black-box tests "
                    "from official_reference_oracle/live_provider_oracle and surface contradictions explicitly. If no authoritative independent oracle is practical, "
                    "state authoritative_oracle_unavailable_reason."
                ),
                reason="Deterministic planning policy: fresh external evidence required",
                parent_task_id=parent["id"] if parent else None, requested_by_task_id=None,
                workspace_mode="collection", depth=(int(parent.get("depth") or 0)+1) if parent else 1,
                allow_dedupe=False,
            )
            researches.append(research_task)
            changed = True
        if policy.critic_required and critic is None:
            parent = next((task for task in reversed(active) if task["profile"] == "coordinator"), None)
            critic_deps = [task["id"] for task in researches]
            if research_task is not None and research_task["id"] not in critic_deps:
                critic_deps.append(research_task["id"])
            critic, critic_created = self.store.create_task(
                project_id=collection["project_id"],
                collection_id=collection_id,
                profile="critic",
                title="Policy preflight critic",
                objective=(
                    "Independently preflight the Collection objective and current work graph before write work begins. "
                    f"Deterministic policy categories: {', '.join(policy.categories) or 'risk'}. Consume fresh research when required and trace normative external requirements into MUST/MUST NOT/VERIFY guidance. "
                    "Challenge evidence-to-directive contradictions and unsupported assumptions. For network/security work, perform an explicit threat-model challenge. "
                    "For external protocols/providers, distinguish implementation-owned black-box tests from authoritative official/live conformance oracles. "
                    "Identify hidden requirements, security/state semantics, scope hazards, assumptions, executable QA directives, and any task contract that knowingly violates a Collection MUST/MUST NOT or acceptance requirement. "
                    "Treat known acceptance violations as planning blockers; do not accept a plan merely because a later review could catch or repair them. "
                    "Return concrete MUST/MUST NOT/VERIFY guidance for the planner and plan reviewer; do not implement or mutate the graph."
                ),
                reason=f"Deterministic planning policy: {reason_text}",
                parent_task_id=parent["id"] if parent else None,
                requested_by_task_id=None,
                depends_on=critic_deps,
                workspace_mode="collection",
                depth=(int(parent.get("depth") or 0) + 1) if parent else 1,
                allow_dedupe=False,
            )
            if critic_created and parent:
                self.store.add_edge(parent["id"], critic["id"], collection_id, "spawned_by")
            changed = True

        plan_reviews = [task for task in active if task["profile"] == "plan_review"]
        plan_review = plan_reviews[-1] if plan_reviews else None

        # Upgrade compatibility only: pre-baseline collections may already have
        # a critic-informed plan-review gate but no durable critic baseline event.
        # Fresh plan-review publication writes its seal atomically with the task,
        # so this branch should only be needed for graphs created by older Praector
        # versions (or externally reconstructed legacy state).
        if (
            policy.critic_required
            and critic is not None
            and critic.get("status") == TaskStatus.COMPLETE.value
            and plan_review is not None
            and self.store.critic_plan_baseline(critic["id"]) is None
        ):
            self.store.record_critic_plan_baseline(
                critic["id"],
                self._material_plan_fingerprint(collection, active),
                source="compatibility_plan_seal",
                plan_review_task_id=plan_review["id"],
            )
            changed = True

        critic, revalidation_changed = self._ensure_material_critic_revalidation(
            collection, active, policy, critic, plan_review
        )
        if revalidation_changed:
            changed = True
            # Refresh the local graph view so the new critic/coordinator and
            # their dependencies participate in the remainder of this pass.
            tasks = self.store.list_tasks(collection_id)
            active = [
                task for task in tasks
                if task["status"] not in {TaskStatus.CANCELLED.value, TaskStatus.SUPERSEDED.value}
            ]
            critics = [task for task in active if task["profile"] == "critic"]
            critic = critics[-1] if critics else critic
            researches = [task for task in active if task["profile"] == "research"]
            research_task = researches[-1] if researches else research_task
            plan_reviews = [task for task in active if task["profile"] == "plan_review"]
            plan_review = plan_reviews[-1] if plan_reviews else plan_review
            write_tasks = [
                task for task in active
                if task["profile"] == "implement"
                and task["status"] in {TaskStatus.PENDING.value, TaskStatus.READY.value}
            ]

        # A completed plan review is stale if write work or coordinator graph
        # reasoning changed after it ran, or if a mandatory critic was added
        # later and therefore could not have informed the verdict.
        if plan_review and plan_review["status"] == TaskStatus.COMPLETE.value:
            reviewed_at = str(plan_review.get("updated_at") or "")
            newest_write_created = max(str(task.get("created_at") or "") for task in write_tasks)
            newest_coordinator_update = max(
                (
                    str(task.get("updated_at") or "")
                    for task in active
                    if task["profile"] == "coordinator"
                    and task["status"] not in {TaskStatus.COMPLETE.value, TaskStatus.CANCELLED.value, TaskStatus.SUPERSEDED.value}
                ),
                default="",
            )
            critic_not_included = bool(
                policy.critic_required and critic is not None
                and (critic["status"] != TaskStatus.COMPLETE.value or str(critic.get("updated_at") or "") > reviewed_at)
            )
            research_not_included = bool(
                policy.research_required
                and any(
                    task["status"] != TaskStatus.COMPLETE.value or str(task.get("updated_at") or "") > reviewed_at
                    for task in researches
                )
            )
            if reviewed_at < newest_write_created or reviewed_at < newest_coordinator_update or critic_not_included or research_not_included:
                plan_review = None

        # A plan review that depends on implementation is post-write review, not
        # a valid pre-write gate. Do not introduce a dependency cycle; create a
        # fresh policy review instead.
        if plan_review:
            review_deps = {dep["id"] for dep in self.store.dependencies(plan_review["id"])}
            if any(task["id"] in review_deps for task in write_tasks):
                plan_review = None

        # If a suitable plan review already exists, strengthen its prerequisites
        # instead of creating duplicates. A mandatory critic must finish first.
        # Likewise, an actively-changing coordinator should finish first unless
        # it is itself blocked on this plan review.
        if plan_review and plan_review["status"] != TaskStatus.COMPLETE.value:
            review_deps = {dep["id"] for dep in self.store.dependencies(plan_review["id"])}
            if policy.research_required:
                for research in researches:
                    if research["id"] not in review_deps:
                        self.store.add_dependency(plan_review["id"], research["id"])
                        review_deps.add(research["id"])
                        changed = True
            if policy.critic_required and critic is not None and critic["id"] not in review_deps:
                self.store.add_dependency(plan_review["id"], critic["id"])
                review_deps.add(critic["id"])
                changed = True
            # Only coordinators that are actively shaping the graph may be
            # prerequisites of final plan review. A WAITING coordinator has
            # already handed control to a blocking child/review and must never
            # be wired back as a prerequisite of that review generation. Doing
            # so creates the exact reverse-edge cycle exposed by the Twilio
            # revalidation recovery path (coordinator waits on review while
            # policy simultaneously makes review wait on coordinator).
            shaping_coordinators = [
                task for task in active
                if task["profile"] == "coordinator"
                and task["status"] in {
                    TaskStatus.PENDING.value, TaskStatus.READY.value, TaskStatus.RUNNING.value,
                    TaskStatus.VERIFYING.value,
                }
            ]
            for coordinator in shaping_coordinators:
                # If the review already reaches this coordinator, then the
                # coordinator is waiting (possibly through several orchestration
                # tasks) on the review. Making the review depend on it would close
                # a cycle. This is a transitive check, not the former one-hop check.
                if self.store.dependency_path(plan_review["id"], coordinator["id"]) is not None:
                    continue
                if coordinator["id"] not in review_deps:
                    try:
                        self.store.add_dependency(plan_review["id"], coordinator["id"])
                    except ValueError as exc:
                        self.store.event(
                            collection["project_id"], collection_id, plan_review["id"], None,
                            "DEPENDENCY_CYCLE_REJECTED",
                            {
                                "from_task_id": coordinator["id"],
                                "to_task_id": plan_review["id"],
                                "context": "planning_policy_strengthen",
                                "error": str(exc),
                            },
                        )
                        continue
                    review_deps.add(coordinator["id"])
                    changed = True

        active_coordinators = [
            task for task in active
            if task["profile"] == "coordinator"
            and task["status"] in {
                TaskStatus.PENDING.value, TaskStatus.READY.value, TaskStatus.RUNNING.value,
                TaskStatus.WAITING.value, TaskStatus.VERIFYING.value,
            }
        ]

        # Do not manufacture a policy plan review while a coordinator is still
        # shaping the graph. The claim-time implementation barrier already blocks
        # write work, so the coordinator owns creation of the final blocking
        # plan-review gate. If planning terminates without one, this deterministic
        # backstop creates the policy review on the next reconciliation pass.
        if policy.plan_review_required and plan_review is None and not active_coordinators:
            deps: list[str] = []
            if critic is not None:
                deps.append(critic["id"])
            for research in researches:
                if research["id"] not in deps:
                    deps.append(research["id"])
            parent = next(
                (task for task in reversed(active) if task["profile"] == "coordinator"), None
            )
            policy_baseline_critic = (
                critic
                if critic is not None
                and critic.get("status") == TaskStatus.COMPLETE.value
                and self.store.critic_plan_baseline(critic["id"]) is None
                else None
            )
            policy_baseline_fingerprint = (
                self._material_plan_fingerprint(collection, active)
                if policy_baseline_critic is not None
                else None
            )
            plan_review, plan_review_created = self.store.create_task(
                project_id=collection["project_id"],
                collection_id=collection_id,
                profile="plan_review",
                title="Policy plan review",
                objective=(
                    "Independently review the current durable work graph before any implementation runs. "
                    f"Deterministic policy categories: {', '.join(policy.categories) or 'risk'}. "
                    "Use completed critic directives and required fresh research as authoritative planning inputs. Trace material external-contract directives back to research claims/sources; "
                    "independently challenge contradictions, dropped MUST/MUST_NOT requirements, unsupported assumptions, stale evidence, and misclassified conformance oracles. "
                    "Verify that implementation tasks are decision-complete, security/threat-model semantics are explicit, dependencies are coherent, verification is executable, and executing "
                    "the graph exactly as written can satisfy every Collection MUST/MUST NOT and acceptance requirement without knowingly requiring a later repair for an already-visible defect. "
                    "Return verdict pass/fail; a known acceptance violation is a true blocker. Name at most three actionable blockers."
                ),
                reason=f"Deterministic planning policy: {reason_text}",
                parent_task_id=parent["id"] if parent else None,
                requested_by_task_id=None,
                depends_on=deps,
                workspace_mode="collection",
                depth=(int(parent.get("depth") or 0) + 1) if parent else 1,
                allow_dedupe=False,
                gate_implement_tasks=True,
                critic_baseline_task_id=(policy_baseline_critic["id"] if policy_baseline_critic else None),
                critic_baseline_fingerprint=policy_baseline_fingerprint,
                critic_baseline_source=("initial_policy_plan_seal" if policy_baseline_critic else None),
            )
            if plan_review_created and parent:
                self.store.add_edge(parent["id"], plan_review["id"], collection_id, "spawned_by")
            changed = True

        if policy.plan_review_required and plan_review is not None:
            for write_task in write_tasks:
                deps = {dep["id"] for dep in self.store.dependencies(write_task["id"])}
                if plan_review["id"] not in deps:
                    self.store.add_dependency(write_task["id"], plan_review["id"]
                    )
                    changed = True

        if changed:
            self.store.event(
                collection["project_id"], collection_id, None, None,
                "PLANNING_POLICY_ENFORCED",
                {
                    "explore_required": policy.explore_required,
                    "research_required": policy.research_required,
                    "critic_required": policy.critic_required,
                    "plan_review_required": policy.plan_review_required,
                    "external_conformance_required": policy.external_conformance_required,
                    "threat_model_required": policy.threat_model_required,
                    "categories": list(policy.categories),
                    "critic_task_id": critic["id"] if critic else None,
                    "plan_review_task_id": plan_review["id"] if plan_review else None,
                    "gated_implement_tasks": [task["id"] for task in write_tasks],
                },
            )
            if collection.get("status") == "BLOCKED":
                self.store.set_collection_status(collection_id, "RUNNING", commit=False)
        return changed

    def _enforce_delivery_policy(self, collection_id: str) -> bool:
        """Ensure direct change requests cannot terminate as planning-only work.

        This is deliberately narrower than planning policy: it activates when
        the Collection objective is an explicit mutation request, including
        common conversational forms such as "I'd like to add...". One automatic repair coordinator is allowed. If that
        coordinator still creates no implementation, terminal reconciliation
        leaves the Collection BLOCKED instead of looping indefinitely.
        """
        collection = self.store.get_collection(collection_id)
        policy = classify_delivery_policy(collection.get("objective") or "")
        if not policy.implementation_required:
            return False

        tasks = self.store.list_tasks(collection_id)
        active = [
            task for task in tasks
            if task["status"] not in {TaskStatus.CANCELLED.value, TaskStatus.SUPERSEDED.value}
        ]
        if any(task["profile"] == "implement" for task in active):
            return False

        nonterminal_planning_tasks = [
            task for task in active
            if task["profile"] in {"coordinator", "explore", "critic", "plan_review", "research", "advise"}
            and task["status"] in {
                TaskStatus.PENDING.value, TaskStatus.READY.value, TaskStatus.RUNNING.value,
                TaskStatus.WAITING.value, TaskStatus.VERIFYING.value,
            }
        ]
        # Delivery repair is a terminal backstop, not parallel planning. Wait
        # for all active planning/specialist work to settle before deciding the
        # graph truly lacks an implementation path.
        if nonterminal_planning_tasks:
            return False

        # Give the system one deterministic repair opportunity, but do not
        # manufacture coordinators forever if the planner repeatedly refuses to
        # create write work.
        if any(
            task["profile"] == "coordinator"
            and str(task.get("reason") or "").startswith("Deterministic delivery policy")
            for task in tasks
        ):
            return False

        latest_coordinator = next(
            (task for task in reversed(active) if task["profile"] == "coordinator"),
            None,
        )
        latest_critic = next(
            (task for task in reversed(active) if task["profile"] == "critic" and task["status"] == TaskStatus.COMPLETE.value),
            None,
        )
        deps: list[str] = []
        if latest_critic:
            deps.append(latest_critic["id"])
        if latest_coordinator and latest_coordinator["status"] == TaskStatus.COMPLETE.value:
            deps.append(latest_coordinator["id"])

        repair, created = self.store.create_task(
            project_id=collection["project_id"],
            collection_id=collection_id,
            profile="coordinator",
            title="Delivery policy repair",
            objective=(
                "The Collection objective is a direct change request, but the durable graph contains no implementation task. "
                "Use completed critic/coordinator evidence to create at least one decision-complete implement task and any required pre-write gates. "
                "Do not declare the Collection delivered until durable implementation work exists and can receive independent review."
            ),
            reason="Deterministic delivery policy: implementation-required objective has no implementation task",
            parent_task_id=latest_coordinator["id"] if latest_coordinator else None,
            requested_by_task_id=None,
            depends_on=deps,
            workspace_mode="collection",
            depth=(int(latest_coordinator.get("depth") or 0) + 1) if latest_coordinator else 1,
            allow_dedupe=False,
        )
        if created and latest_coordinator:
            self.store.add_edge(latest_coordinator["id"], repair["id"], collection_id, "spawned_by")
        self.store.event(
            collection["project_id"], collection_id, repair["id"], None,
            "DELIVERY_POLICY_ENFORCED",
            {
                "implementation_required": True,
                "categories": list(policy.categories),
                "repair_coordinator_task_id": repair["id"],
            },
        )
        if collection.get("status") == "BLOCKED":
            self.store.set_collection_status(collection_id, "RUNNING", commit=False)
        return True

    def _defer_completion_for_unmet_dependencies(
        self, task_id: str, *, attempt_id: str | None = None
    ) -> bool:
        """Keep blocking-delegation parents WAITING until hard deps complete."""
        current = self.store.get_task(task_id)
        if current["status"] != TaskStatus.VERIFYING.value:
            return False
        unmet_dependencies = [
            dep for dep in self.store.dependencies(task_id)
            if dep.get("status") != TaskStatus.COMPLETE.value
        ]
        if not unmet_dependencies:
            return False
        unmet_ids = [dep["id"] for dep in unmet_dependencies]
        self.store.set_task_status(
            task_id, TaskStatus.WAITING.value,
            message=f"task {task_id}: completion deferred for dependencies {','.join(unmet_ids)}",
        )
        self.store.event(
            current["project_id"], current["collection_id"], task_id, attempt_id,
            "TASK_COMPLETION_DEFERRED",
            {"unmet_dependencies": unmet_ids},
        )
        return True

    def _handle_missing_review_verdict(
        self, project_cfg: Any, task: dict[str, Any], attempt: dict[str, Any]
    ) -> str:
        """Retry reviewer protocol failures before escalating graph repair."""
        message = "Review task exited without submitting an explicit structured pass/fail verdict."
        if int(attempt["attempt_no"]) < project_cfg.limits.max_attempts_per_task:
            self.store.set_task_status(
                task["id"], TaskStatus.READY.value,
                message=f"task {task['id']}: retry after missing review verdict",
            )
            self.store.event(
                task["project_id"], task["collection_id"], task["id"], attempt["id"],
                "REVIEW_PROTOCOL_RETRY",
                {"attempt_no": int(attempt["attempt_no"]), "reason": message},
            )
        else:
            self.store.submit_result(task["id"], {
                "status": "failed",
                "summary": message,
                "risks": ["Missing review verdict after retry budget exhausted"],
            })
        return message

    def _handle_read_only_capability_violation(
        self, project_cfg: Any, task: dict[str, Any], attempt: dict[str, Any]
    ) -> str:
        """Retry read-only capability violations before graph-level escalation."""
        message = "Read-only task mutated its workspace; changes were discarded."
        if int(attempt["attempt_no"]) < project_cfg.limits.max_attempts_per_task:
            self.store.set_task_status(
                task["id"], TaskStatus.READY.value,
                message=f"task {task['id']}: retry after read-only capability violation",
            )
            self.store.event(
                task["project_id"], task["collection_id"], task["id"], attempt["id"],
                "READ_ONLY_CAPABILITY_RETRY",
                {"attempt_no": int(attempt["attempt_no"]), "reason": message},
            )
        else:
            self.store.submit_result(task["id"], {
                "status": "failed",
                "summary": message,
                "artifacts": [],
                "risks": ["Capability boundary violation after retry budget exhausted"],
            })
        return message

    def _process_result_metadata(self, task: dict[str, Any]) -> None:
        refreshed = self.store.get_task(task["id"])
        result = refreshed.get("result") or {}
        for item in result.get("memory_candidates", []) or []:
            if isinstance(item, str):
                text, kind = item, "finding"
            elif isinstance(item, dict):
                # Be tolerant of common structured-result aliases while the
                # public schema is still intentionally stable. The canonical keys are text/kind.
                text = str(item.get("text") or item.get("content") or "").strip()
                kind = str(item.get("kind") or item.get("type") or "finding")
            else:
                continue
            if text:
                self.store.add_memory(
                    project_id=task["project_id"], collection_id=task["collection_id"], task_id=task["id"],
                    scope="collection", state="candidate", kind=kind, text=text,
                )
        for item in result.get("decisions", []) or []:
            if isinstance(item, str):
                statement, rationale = item, ""
            elif isinstance(item, dict):
                statement = str(item.get("statement", "")).strip()
                rationale = str(item.get("rationale", "")).strip()
            else:
                continue
            if statement:
                self.store.add_decision(
                    project_id=task["project_id"], collection_id=task["collection_id"], task_id=task["id"],
                    statement=statement, rationale=rationale,
                    evidence=item.get("evidence", []) if isinstance(item, dict) else [],
                    alternatives=item.get("alternatives", []) if isinstance(item, dict) else [],
                )

    def _ensure_review(self, project: dict[str, Any], collection: dict[str, Any], implement_task: dict[str, Any]) -> None:
        failed_parent_review: dict[str, Any] | None = None
        parent_id = implement_task.get("parent_task_id")
        if parent_id:
            parent = self.store.get_task(parent_id)
            if (
                parent.get("profile") == "review"
                and str((parent.get("result") or {}).get("verdict") or "").lower() == "fail"
            ):
                failed_parent_review = parent
        existing = self.store.find_related_review(implement_task["id"])
        if existing:
            return
        lineage, reconciliation_required = self._task_input_lineage_changed(implement_task)
        dependencies = [implement_task["id"]]
        policy = classify_planning_policy(collection.get("objective") or "")
        if policy.research_required:
            for task in self.store.list_tasks(collection["id"]):
                if (
                    task.get("profile") == "research"
                    and task.get("status") == TaskStatus.COMPLETE.value
                    and task["id"] not in dependencies
                ):
                    dependencies.append(task["id"])
        title = f"Review {implement_task['title']}"
        objective = (
            f"Independently review implementation task {implement_task['id']} against its objective and acceptance intent. "
            "Inspect the integrated code, run appropriate checks, and return verdict pass/fail. If repair is required, "
            "request a blocking implement task rather than changing code yourself."
        )
        reason = "Praector policy requires independent review of every implementation task."
        original_verification_sources: list[str] = []
        if failed_parent_review is not None:
            for edge in self.store.list_edges(implement_task["collection_id"]):
                if edge.get("relation") == "verifies" and edge.get("to_task_id") == failed_parent_review["id"]:
                    source = str(edge.get("from_task_id") or "")
                    if source and source not in dependencies:
                        dependencies.append(source)
                        original_verification_sources.append(source)
            title = f"Fresh review after repair {implement_task['title']}"
            objective = (
                f"Perform a fresh independent post-repair review. Repair implementation {implement_task['id']} was created "
                f"because review {failed_parent_review['id']} returned verdict=fail. Re-evaluate the repaired integrated "
                "candidate from scratch against the original acceptance intent and the repair objective; do not rely on "
                "the prior review's conclusions. Return verdict pass/fail."
            )
            reason = "A semantic review failure was repaired; policy requires a fresh independent review task/context."
        if reconciliation_required and lineage is not None:
            prior_title = title
            title = f"Reconcile and review {implement_task['title']}"
            objective = (
                f"Input lineage changed while implementation {implement_task['id']} was running. Reconcile the completed/integrated "
                "work against the CURRENT accepted material plan, fresh research evidence, and prerequisite state before returning "
                "verdict pass/fail. Do not assume the implementation's starting evidence is still valid. "
                f"Starting material fingerprint={lineage['start_material_plan_fingerprint']}; "
                f"current={lineage['current_material_plan_fingerprint']}; "
                f"starting dependency digest={lineage['start_dependency_lineage_digest']}; "
                f"current={lineage['current_dependency_lineage_digest']}. Re-run relevant verification against the current integrated "
                "candidate. If compatibility requires code changes, request a blocking repair implementation rather than changing code "
                f"yourself. This reconciliation also satisfies the normal independent review obligation. Prior review framing: {prior_title}"
            )
            reason = (
                "The implementation ran against prerequisite/planning evidence that changed before integration; durable reconciliation "
                "is required before acceptance."
            )
        review, _ = self.store.create_task(
            project_id=implement_task["project_id"],
            collection_id=implement_task["collection_id"],
            profile="review",
            title=title,
            objective=objective,
            reason=reason,
            parent_task_id=implement_task["id"],
            requested_by_task_id=None,
            depends_on=dependencies,
            workspace_mode="collection",
            depth=int(implement_task.get("depth") or 0) + 1,
        )
        self.store.add_edge(implement_task["id"], review["id"], collection["id"], "verifies")
        for source in original_verification_sources:
            self.store.add_edge(source, review["id"], collection["id"], "verifies")
        if failed_parent_review is not None:
            self.store.event(
                implement_task["project_id"], implement_task["collection_id"], review["id"], None,
                "POST_REPAIR_REVIEW_CREATED",
                {
                    "failed_review_task_id": failed_parent_review["id"],
                    "repair_task_id": implement_task["id"],
                    "original_implementation_task_ids": original_verification_sources,
                },
            )
        if reconciliation_required and lineage is not None:
            self.store.event(
                implement_task["project_id"], implement_task["collection_id"], implement_task["id"], None,
                "RECONCILIATION_REQUIRED",
                {**lineage, "implementation_task_id": implement_task["id"], "reconciliation_review_task_id": review["id"]},
            )
        # If this implementation was a blocking inherited child, the parent must
        # wait for the review too, not resume immediately after code integration.
        if implement_task.get("workspace_mode") == "inherit" and implement_task.get("parent_task_id"):
            parent = self.store.get_task(implement_task["parent_task_id"])
            if parent["status"] == TaskStatus.WAITING.value:
                self.store.add_edge(review["id"], parent["id"], collection["id"], "depends_on")

    def _handle_failed_review(self, project: dict[str, Any], task: dict[str, Any]) -> None:
        current = self.store.get_task(task["id"])
        result = current.get("result") or {}
        if current["profile"] != "review" or current["status"] != TaskStatus.VERIFYING.value:
            return
        if str(result.get("verdict", "")).lower() != "fail":
            return
        summary = str(result.get("summary", "Review found defects requiring repair."))
        limits = self._limits_dict(project)
        repair, _ = self.store.request_task(
            current["id"],
            profile="implement",
            title=f"Repair findings from {current['id']}",
            objective=f"Address the review findings from {current['id']}. Review summary: {summary}",
            reason=(
                "Independent review returned verdict=fail. The failed review will complete as historical evidence; "
                "this repair must receive a fresh independent review task after integration."
            ),
            blocking=False,
            depends_on=[current["id"]],
            workspace_mode="collection",
            base_commit=None,
            limits=limits,
        )
        self.store.event(
            current["project_id"], current["collection_id"], current["id"], None,
            "REVIEW_REPAIR_CREATED", {"repair_task_id": repair["id"]}
        )

    def _handle_failed_plan_review(self, project: dict[str, Any], task: dict[str, Any]) -> None:
        current = self.store.get_task(task["id"])
        result = current.get("result") or {}
        if current["profile"] != "plan_review" or current["status"] != TaskStatus.VERIFYING.value:
            return
        if str(result.get("verdict", "")).lower() != "fail":
            return
        summary = str(result.get("summary", "Plan review found blocking gaps."))
        coordinator, _ = self.store.request_coordinator_attention(
            current["id"],
            f"Plan review {current['id']} rejected the current graph. Repair the plan before execution continues. Summary: {summary}",
            blocking=True,
            limits=self._limits_dict(project),
        )
        self.store.event(
            current["project_id"], current["collection_id"], current["id"], None,
            "PLAN_REVIEW_REPAIR_REQUESTED", {"coordinator_task_id": coordinator["id"]}
        )

    def _finalize_blocking_coordinator_after_plan_review(self, plan_review: dict[str, Any]) -> bool:
        """Complete waiting coordinators whose final planning handoff passed.

        The requester/parent is the common case, but graph repair may rewire an
        older waiting coordinator onto a newer surviving plan-review gate. Any
        WAITING coordinator that directly depends on this passing review and has
        no other unmet hard dependency is therefore a terminal planning waiter
        and should be finalized rather than awakened to reshape an already
        approved graph.
        """
        candidate_ids: list[str] = []
        parent_id = plan_review.get("requested_by_task_id") or plan_review.get("parent_task_id")
        if parent_id:
            candidate_ids.append(str(parent_id))
        for dependent in self.store.dependents(plan_review["id"]):
            if dependent.get("profile") == "coordinator":
                candidate_ids.append(str(dependent["id"]))

        finalized = False
        for coordinator_id in dict.fromkeys(candidate_ids):
            try:
                coordinator = self.store.get_task(coordinator_id)
            except KeyError:
                continue
            if coordinator.get("profile") != "coordinator" or coordinator.get("status") != TaskStatus.WAITING.value:
                continue
            deps = self.store.dependencies(coordinator["id"])
            if not any(dep["id"] == plan_review["id"] for dep in deps):
                continue
            if any(dep.get("status") != TaskStatus.COMPLETE.value for dep in deps):
                continue

            if not coordinator.get("result"):
                self.store.submit_result(coordinator["id"], {
                    "status": "complete",
                    "summary": f"Planning graph accepted by plan review {plan_review['id']}.",
                    "findings": [],
                    "evidence": [f"plan_review {plan_review['id']} verdict=pass"],
                    "artifacts": [],
                    "risks": [],
                    "unresolved": [],
                    "memory_candidates": [],
                    "decisions": [],
                })
            self.store.set_task_status(
                coordinator["id"], TaskStatus.COMPLETE.value,
                message=f"task {coordinator['id']}: finalized after passing plan review {plan_review['id']}",
            )
            self.store.event(
                coordinator["project_id"], coordinator["collection_id"], coordinator["id"], None,
                "COORDINATOR_FINALIZED_AFTER_PLAN_REVIEW",
                {"plan_review_task_id": plan_review["id"]},
            )
            finalized = True
        return finalized

    def execute_task(self, task_id: str) -> None:
        initial = self.store.get_task(task_id)
        project = self.store.get_project(initial["project_id"])
        project_cfg = project_config_from_row(project)
        claim = self.store.claim_task(task_id, project_cfg.limits.max_attempts_per_task)
        if not claim:
            return
        task, attempt = claim
        collection = self.store.get_collection(task["collection_id"])
        log_path = self.cfg.logs_dir / collection["id"] / f"{attempt['id']}.log"
        # Persist the path before launching OpenCode so `praector logs --tail` can
        # inspect an in-flight attempt rather than waiting for finish_attempt.
        self.store.set_attempt_log_path(attempt["id"], str(log_path))
        session_row: dict[str, Any] | None = None
        protocol_failure: str | None = None

        try:
            self._record_task_input_lineage(task, attempt)
            workdir = self.workspace.prepare_task(project, collection, task)
            task = self.store.get_task(task_id)
            session_row = self.store.create_session(attempt["id"], project_cfg.harness, str(workdir))
            prompt = build_task_envelope(self.store, project, collection, task, attempt)
            harness = create_harness(project_cfg.harness, opencode_command=project_cfg.opencode_command)
            env = {
                "PRAECTOR_HOME": str(self.cfg.home),
                "PRAECTOR_PROJECT_ID": task["project_id"],
                "PRAECTOR_COLLECTION_ID": task["collection_id"],
                "PRAECTOR_TASK_ID": task["id"],
                "PRAECTOR_ATTEMPT_ID": attempt["id"],
                "PRAECTOR_SESSION_ID": session_row["id"],
                "PRAECTOR_PROFILE": task["profile"],
                "PRAECTOR_WORKTREE": str(workdir),
            }
            self.store.event(task["project_id"], task["collection_id"], task["id"], attempt["id"], "SESSION_STARTED", {"workdir": str(workdir)})
            result = harness.run_task(
                prompt=prompt,
                workdir=workdir,
                profile=task["profile"],
                model=project_cfg.model_for(task["profile"]),
                env=env,
                log_path=log_path,
                timeout_seconds=project_cfg.limits.task_timeout_seconds,
                on_start=lambda pid: self.store.set_session_pid(session_row["id"], pid),
            )
            self.store.finish_session(
                session_row["id"],
                external_session_id=result.external_session_id,
                status="TIMED_OUT" if result.timed_out else ("COMPLETE" if result.exit_code == 0 else "FAILED"),
            )

            current = self.store.get_task(task_id)
            # Read-only profiles get a second, deterministic enforcement layer.
            if task["profile"] in READ_ONLY_PROFILES and self.workspace.is_dirty(workdir):
                self.workspace.rollback_read_only_changes(current)
                protocol_failure = self._handle_read_only_capability_violation(
                    project_cfg, task, attempt
                )
                current = self.store.get_task(task_id)

            # If the agent did not submit a durable result, preserve its final
            # response as a fallback result. Process failures are retried.
            if current["status"] == TaskStatus.RUNNING.value:
                if result.exit_code == 0:
                    if task["profile"] in {"review", "plan_review"}:
                        # Missing verdict is a reviewer protocol failure, not a
                        # substantive rejection of the implementation/plan. Retry
                        # the same durable review task before graph-level repair.
                        protocol_failure = self._handle_missing_review_verdict(project_cfg, task, attempt)
                    else:
                        self.store.submit_result(task_id, {
                            "status": "complete",
                            "summary": result.final_text or "OpenCode task completed without an explicit structured result.",
                            "findings": [], "evidence": [], "artifacts": [], "risks": [], "unresolved": [],
                            "memory_candidates": [], "decisions": [],
                        })
                else:
                    if int(attempt["attempt_no"]) < project_cfg.limits.max_attempts_per_task:
                        self.store.set_task_status(task_id, TaskStatus.READY.value, message=f"task {task_id}: retry after harness failure")
                    else:
                        self.store.submit_result(task_id, {
                            "status": "failed",
                            "summary": f"OpenCode exited with code {result.exit_code}.",
                            "risks": [result.stderr[-4000:] if result.stderr else "Harness failure"],
                        })

            current = self.store.get_task(task_id)

            # A model can violate the blocking-delegation protocol by requesting
            # a blocking child (which moves this task to WAITING) and then
            # submitting a "complete" result before its process exits. That
            # submission temporarily moves the task to VERIFYING. Durable graph
            # dependencies are authoritative: no task may become dependency-
            # visible COMPLETE while any hard dependency is still incomplete.
            if self._defer_completion_for_unmet_dependencies(task_id, attempt_id=attempt["id"]):
                current = self.store.get_task(task_id)

            if task["profile"] == "implement":
                # Preserve physical workspace state at every attempt boundary.
                head = self.workspace.checkpoint(
                    current,
                    attempt_no=int(attempt["attempt_no"]),
                    operation="attempt-checkpoint",
                )
                if head:
                    self.store.set_task_workspace(task_id, current_head=head)
                current = self.store.get_task(task_id)
                if current["status"] == TaskStatus.VERIFYING.value:
                    try:
                        integrated_head = self.workspace.integrate_task(project, collection, current)
                        self.store.event(
                            current["project_id"], current["collection_id"], task_id, attempt["id"],
                            "TASK_INTEGRATED", {"head": integrated_head}
                        )
                        self._ensure_review(project, collection, current)
                        self.store.set_task_status(task_id, TaskStatus.COMPLETE.value, message=f"task {task_id}: integration complete")
                    except Exception as exc:
                        self.store.submit_result(task_id, {
                            "status": "failed",
                            "summary": f"Implementation completed but integration failed: {exc}",
                            "risks": ["Git integration conflict"],
                        })
                        try:
                            self.store.request_coordinator_attention(
                                task_id, f"Integration failed for {task_id}: {exc}", blocking=False,
                                limits=self._limits_dict(project),
                            )
                        except Exception:
                            pass
            elif task["profile"] == "review":
                self._handle_failed_review(project, current)
                current = self.store.get_task(task_id)
                # A semantic fail creates repair work that depends on this
                # review; the failed review itself becomes terminal historical
                # evidence. Repair integration later gets a fresh review task.
                if current["status"] == TaskStatus.VERIFYING.value:
                    self.store.set_task_status(task_id, TaskStatus.COMPLETE.value, message=f"task {task_id}: review complete")
            elif task["profile"] == "plan_review":
                self._handle_failed_plan_review(project, current)
                current = self.store.get_task(task_id)
                # A failed plan review remains WAITING on coordinator repair. A
                # pass becomes dependency-visible and can release gated writes.
                if current["status"] == TaskStatus.VERIFYING.value:
                    self.store.set_task_status(task_id, TaskStatus.COMPLETE.value, message=f"task {task_id}: plan review complete")
                    self._finalize_blocking_coordinator_after_plan_review(self.store.get_task(task_id))
            else:
                if current["status"] == TaskStatus.VERIFYING.value:
                    self.store.set_task_status(task_id, TaskStatus.COMPLETE.value, message=f"task {task_id}: complete")

            self._process_result_metadata(self.store.get_task(task_id))
            final_task = self.store.get_task(task_id)
            if final_task["status"] == TaskStatus.FAILED.value and final_task["profile"] != "coordinator":
                try:
                    self.store.request_coordinator_attention(
                        task_id,
                        f"Task {task_id} ({final_task['title']}) failed and may require replacement or graph repair.",
                        blocking=False,
                        limits=self._limits_dict(project),
                    )
                except Exception:
                    pass
            # Attempt outcome is about this concrete harness invocation, not
            # merely the eventual task state. A non-zero process exit must never
            # be recorded as SUCCEEDED even when the task is being retried and
            # has already been moved back to READY.
            attempt_status = (
                AttemptStatus.TIMED_OUT.value if result.timed_out else
                AttemptStatus.FAILED.value if result.exit_code != 0 else
                AttemptStatus.FAILED.value if protocol_failure else
                AttemptStatus.FAILED.value if final_task["status"] == TaskStatus.FAILED.value else
                AttemptStatus.SUCCEEDED.value
            )
            attempt_result = final_task.get("result") or ({"error": protocol_failure} if protocol_failure else {})
            self.store.finish_attempt(
                attempt["id"], status=attempt_status, exit_code=result.exit_code,
                result=attempt_result, external_session_id=result.external_session_id,
                log_path=str(log_path),
            )
            self.store.event(
                final_task["project_id"], final_task["collection_id"], task_id, attempt["id"],
                "ATTEMPT_FINISHED", {"task_status": final_task["status"], "attempt_status": attempt_status}
            )
        except Exception as exc:
            if session_row:
                try:
                    self.store.finish_session(session_row["id"], external_session_id=None, status="FAILED")
                except Exception:
                    pass
            try:
                current = self.store.get_task(task_id)
                if current["status"] == TaskStatus.RUNNING.value:
                    if int(attempt["attempt_no"]) < project_cfg.limits.max_attempts_per_task:
                        self.store.set_task_status(task_id, TaskStatus.READY.value, message=f"task {task_id}: retry after executor error")
                    else:
                        self.store.submit_result(task_id, {"status": "failed", "summary": f"Executor error: {exc}"})
                self.store.finish_attempt(
                    attempt["id"], status=AttemptStatus.FAILED.value, exit_code=None,
                    result={"error": str(exc)}, external_session_id=None, log_path=str(log_path),
                )
                self.store.event(task["project_id"], task["collection_id"], task_id, attempt["id"], "EXECUTOR_ERROR", {"error": str(exc)})
            except Exception:
                pass

    def _max_workers_for(self, collection_id: str | None) -> int:
        if self.max_workers_override:
            return self.max_workers_override
        if collection_id:
            collection = self.store.get_collection(collection_id)
            project = self.store.get_project(collection["project_id"])
            return project_config_from_row(project).limits.max_parallel_tasks
        projects = self.store.list_projects()
        if not projects:
            return 4
        return max(project_config_from_row(p).limits.max_parallel_tasks for p in projects)

    def _reconcile_collection(self, collection_id: str) -> str:
        _, unresolved_cycle = self.store.repair_legacy_dependency_cycles(collection_id)
        if unresolved_cycle is not None:
            cycle_text = " -> ".join(unresolved_cycle)
            raise DependencyCycleError(
                f"Existing hard-dependency cycle in {collection_id}: {cycle_text}"
            )
        self.store.ensure_collection_entrypoint(collection_id)
        self._enforce_research_requests(collection_id)
        self.store.refresh_waiting_tasks(collection_id)
        self._enforce_planning_policy(collection_id)
        self._enforce_research_requests(collection_id)
        self.store.refresh_waiting_tasks(collection_id)
        self._enforce_delivery_policy(collection_id)
        self.store.refresh_waiting_tasks(collection_id)
        return self.store.update_collection_terminal_state(collection_id)

    def _reconcile_collection_safely(self, collection_id: str) -> str:
        """Contain dependency-cycle failures to one Collection.

        New cycles are rejected at write time, but a persistent store upgraded
        from an older Praector may already contain an unrecognized cycle. Such a
        graph must not terminate the global daemon or allow unrelated runnable
        tasks in that collection to execute around the cycle.
        """
        try:
            state = self._reconcile_collection(collection_id)
            self._quarantined_collections.discard(collection_id)
            return state
        except ResearchCapabilityError as exc:
            self._quarantined_collections.add(collection_id)
            collection = self.store.get_collection(collection_id)
            if collection.get("status") != "BLOCKED":
                self.store.set_collection_status(collection_id, "BLOCKED", commit=True)
            prior = self.store.events_for_collection(collection_id, limit=50)
            if not any(event.get("event_type") == "RESEARCH_CAPABILITY_REQUIRED" and json.loads(event.get("payload_json") or "{}").get("error") == str(exc) for event in prior):
                self.store.event(collection["project_id"], collection_id, None, None, "RESEARCH_CAPABILITY_REQUIRED", {"error": str(exc), "required_capability": "current_external_research"})
            return "BLOCKED"
        except DependencyCycleError as exc:
            self._quarantined_collections.add(collection_id)
            collection = self.store.get_collection(collection_id)
            if collection.get("status") != "BLOCKED":
                self.store.set_collection_status(collection_id, "BLOCKED", commit=True)
            prior = self.store.events_for_collection(collection_id, limit=50)
            already_recorded = False
            for event in prior:
                if event.get("event_type") != "DEPENDENCY_CYCLE_QUARANTINED":
                    continue
                try:
                    payload = json.loads(event.get("payload_json") or "{}")
                except Exception:
                    payload = {}
                if payload.get("error") == str(exc):
                    already_recorded = True
                    break
            if not already_recorded:
                self.store.event(
                    collection["project_id"], collection_id, None, None,
                    "DEPENDENCY_CYCLE_QUARANTINED",
                    {"error": str(exc)},
                )
            if collection_id not in self._reported_cycle_quarantines:
                print(f"praector: collection {collection_id} quarantined by dependency cycle: {exc}")
                self._reported_cycle_quarantines.add(collection_id)
            return "BLOCKED"

    def run_collection(self, collection_id: str, *, poll_seconds: float = 0.5) -> str:
        max_workers = self._max_workers_for(collection_id)
        with self.exclusive_scheduler(), ThreadPoolExecutor(max_workers=max_workers, thread_name_prefix="praector") as pool:
            self.recover_stale_sessions()
            futures: dict[str, Future[None]] = {}
            while True:
                state = self._reconcile_collection_safely(collection_id)
                if state == "COMPLETE":
                    return state
                if collection_id in self._quarantined_collections:
                    return "BLOCKED"
                for task_id, future in list(futures.items()):
                    if future.done():
                        future.result()
                        futures.pop(task_id, None)
                capacity = max_workers - len(futures)
                if capacity > 0:
                    for task in self.store.runnable_tasks(collection_id, limit=capacity):
                        if task["id"] not in futures:
                            futures[task["id"]] = pool.submit(self.execute_task, task["id"])
                if not futures and not self.store.runnable_tasks(collection_id, limit=1):
                    tasks = self.store.list_tasks(collection_id)
                    statuses = {t["status"] for t in tasks}
                    # Nothing can currently advance. Leave enough state for a user
                    # or later coordinator invocation to inspect and repair.
                    if state != "COMPLETE":
                        self.store.set_collection_status(collection_id, "BLOCKED", commit=True)
                        return "BLOCKED"
                time.sleep(poll_seconds)

    def daemon(self, *, once: bool = False, poll_seconds: float = 1.0) -> None:
        max_workers = self._max_workers_for(None)
        with self.exclusive_scheduler(), ThreadPoolExecutor(max_workers=max_workers, thread_name_prefix="praector") as pool:
            self.recover_stale_sessions()
            futures: dict[str, Future[None]] = {}
            while True:
                active_collection_ids: list[str] = []
                for collection in self.store.list_collections():
                    if collection["status"] not in ("RUNNING", "BLOCKED"):
                        continue
                    state = self._reconcile_collection_safely(collection["id"])
                    if collection["id"] in self._quarantined_collections:
                        continue
                    active_collection_ids.append(collection["id"])
                    if self.store.runnable_tasks(collection["id"], 1) and collection["status"] == "BLOCKED":
                        self.store.set_collection_status(collection["id"], "RUNNING", commit=False)
                for task_id, future in list(futures.items()):
                    if future.done():
                        future.result()
                        futures.pop(task_id, None)
                capacity = max_workers - len(futures)
                if capacity > 0:
                    for collection_id in active_collection_ids:
                        if capacity <= 0:
                            break
                        for task in self.store.runnable_tasks(collection_id, limit=capacity):
                            if task["id"] in futures:
                                continue
                            futures[task["id"]] = pool.submit(self.execute_task, task["id"])
                            capacity -= 1
                            if capacity <= 0:
                                break
                if once and not futures:
                    has_runnable = any(
                        self.store.runnable_tasks(collection_id, 1)
                        for collection_id in active_collection_ids
                    )
                    if not has_runnable:
                        return
                time.sleep(poll_seconds)
