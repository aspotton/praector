from pathlib import Path

from praector.config import GlobalConfig
from praector.models import TaskStatus
from praector.planning_policy import (
    classify_planning_policy, effective_planning_policy, gate_derivation_label, GATE_COHERENCE_CUTOFF,
    _classify_planning_policy,
    material_plan_fingerprint, validate_deterministic_research_result,
    validate_external_conformance_review_result, validate_research_consumer_result,
    research_claim_ids, research_claim_constraints, research_exception_decision_ids,
)
from praector.scheduler import Scheduler


class FakePolicyStore:
    def __init__(self, objective: str):
        self.collection = {
            "id": "COL-1",
            "project_id": "PRJ-1",
            "objective": objective,
            "status": "RUNNING",
        }
        self.tasks = [
            {
                "id": "T-COORD",
                "project_id": "PRJ-1",
                "collection_id": "COL-1",
                "profile": "coordinator",
                "title": "Initial coordination",
                "objective": "Plan work",
                "reason": "entrypoint",
                "status": TaskStatus.COMPLETE.value,
                "depth": 0,
                "created_at": "2026-11-10 10:00:00",
                "updated_at": "2026-11-10 10:01:00",
            },
            {
                "id": "T-IMPL",
                "project_id": "PRJ-1",
                "collection_id": "COL-1",
                "profile": "implement",
                "title": "Implement",
                "objective": "Do the work",
                "reason": "planned",
                "status": TaskStatus.READY.value,
                "depth": 1,
                "created_at": "2026-11-10 10:01:01",
                "updated_at": "2026-11-10 10:01:01",
            },
        ]
        self.edges = []
        self.events = []
        self.baselines = {}
        self.counter = 0

    def get_collection(self, collection_id):
        assert collection_id == "COL-1"
        return dict(self.collection)

    def get_project(self, project_id):
        assert project_id == "PRJ-1"
        return {
            "id": "PRJ-1",
            "name": "fake",
            "root_path": ".",
            "base_branch": "main",
            "config": {"capabilities": {"current_external_research": True}},
        }

    def list_tasks(self, collection_id):
        assert collection_id == "COL-1"
        return [dict(task) for task in self.tasks]

    def create_task(self, **kwargs):
        self.counter += 1
        task = {
            "id": f"T-AUTO-{self.counter}",
            "project_id": kwargs["project_id"],
            "collection_id": kwargs["collection_id"],
            "profile": kwargs["profile"],
            "title": kwargs["title"],
            "objective": kwargs["objective"],
            "reason": kwargs.get("reason", ""),
            "requested_by_task_id": kwargs.get("requested_by_task_id"),
            "parent_task_id": kwargs.get("parent_task_id"),
            "status": TaskStatus.READY.value,
            "depth": kwargs.get("depth", 0),
            "created_at": f"2026-11-10 10:02:0{self.counter}",
            "updated_at": f"2026-11-10 10:02:0{self.counter}",
        }
        self.tasks.append(task)
        for dep in kwargs.get("depends_on") or []:
            self.add_edge(dep, task["id"], task["collection_id"], "depends_on")
        if kwargs.get("spawned_by_task_id"):
            self.add_edge(kwargs["spawned_by_task_id"], task["id"], task["collection_id"], "spawned_by")
        if kwargs.get("blocking_caller_task_id"):
            self.add_edge(task["id"], kwargs["blocking_caller_task_id"], task["collection_id"], "depends_on")
            for existing in self.tasks:
                if existing["id"] == kwargs["blocking_caller_task_id"]:
                    existing["status"] = TaskStatus.WAITING.value
        critic_id = kwargs.get("critic_baseline_task_id")
        if critic_id is not None and critic_id not in self.baselines:
            self.record_critic_plan_baseline(
                critic_id,
                kwargs["critic_baseline_fingerprint"],
                source=kwargs["critic_baseline_source"],
                plan_review_task_id=task["id"],
            )
        return dict(task), True

    def list_edges(self, collection_id):
        assert collection_id == "COL-1"
        return [dict(edge) for edge in self.edges]

    def critic_plan_baseline(self, critic_task_id):
        value = self.baselines.get(critic_task_id)
        return dict(value) if value else None

    def record_critic_plan_baseline(self, critic_task_id, fingerprint, *, source, plan_review_task_id=None):
        self.baselines[critic_task_id] = {
            "fingerprint": fingerprint,
            "source": source,
            "plan_review_task_id": plan_review_task_id,
        }
        self.events.append(("CRITIC_PLAN_BASELINE", dict(self.baselines[critic_task_id])))

    def dependencies(self, task_id):
        dep_ids = [
            edge["from_task_id"]
            for edge in self.edges
            if edge["to_task_id"] == task_id and edge["relation"] == "depends_on"
        ]
        return [dict(task) for task in self.tasks if task["id"] in dep_ids]

    def research_ancestors(self, task_id):
        seen = {task_id}
        queue = [task_id]
        found = {}
        while queue:
            current = queue.pop(0)
            for dep in self.dependencies(current):
                dep_id = dep["id"]
                if dep.get("profile") == "research" and dep.get("status") == TaskStatus.COMPLETE.value:
                    found[dep_id] = dict(dep)
                if dep_id not in seen:
                    seen.add(dep_id)
                    queue.append(dep_id)
        return sorted(found.values(), key=lambda task: (task.get("created_at", ""), task["id"]))

    def add_dependency(self, task_id, dependency_id):
        self.add_edge(dependency_id, task_id, "COL-1", "depends_on")

    def remove_dependency(self, task_id, dependency_id):
        self.edges = [
            edge for edge in self.edges
            if not (
                edge["from_task_id"] == dependency_id
                and edge["to_task_id"] == task_id
                and edge["relation"] == "depends_on"
            )
        ]

    def add_edge(self, from_task_id, to_task_id, collection_id, relation="depends_on"):
        edge = {
            "from_task_id": from_task_id,
            "to_task_id": to_task_id,
            "collection_id": collection_id,
            "relation": relation,
        }
        if edge not in self.edges:
            self.edges.append(edge)

    def dependency_path(self, from_task_id, to_task_id):
        edges = [
            (edge["from_task_id"], edge["to_task_id"])
            for edge in self.edges if edge["relation"] == "depends_on"
        ]
        from praector.db import DoltStore
        return DoltStore._dependency_path_in_edges(edges, from_task_id, to_task_id)

    def dependents(self, task_id):
        dependent_ids = [
            edge["to_task_id"] for edge in self.edges
            if edge["from_task_id"] == task_id and edge["relation"] == "depends_on"
        ]
        return [dict(task) for task in self.tasks if task["id"] in dependent_ids]

    def supersede_task(self, old_task_id, replacement_task_id, reason):
        old = next(task for task in self.tasks if task["id"] == old_task_id)
        replacement = next(task for task in self.tasks if task["id"] == replacement_task_id)
        outgoing = [
            edge["to_task_id"] for edge in self.edges
            if edge["from_task_id"] == old_task_id and edge["relation"] == "depends_on"
        ]
        incoming = [
            edge["from_task_id"] for edge in self.edges
            if edge["to_task_id"] == old_task_id and edge["relation"] == "depends_on"
        ]
        for dependent_id in outgoing:
            if self.dependency_path(dependent_id, replacement_task_id) is None and dependent_id != replacement_task_id:
                self.add_edge(replacement_task_id, dependent_id, old["collection_id"], "depends_on")
        if old.get("profile") == replacement.get("profile") == "plan_review":
            for dependency_id in incoming:
                if self.dependency_path(replacement_task_id, dependency_id) is None and dependency_id != replacement_task_id:
                    self.add_edge(dependency_id, replacement_task_id, old["collection_id"], "depends_on")
        self.edges = [
            edge for edge in self.edges
            if not (edge["from_task_id"] == old_task_id and edge["relation"] == "depends_on")
        ]
        old["status"] = TaskStatus.SUPERSEDED.value
        self.add_edge(old_task_id, replacement_task_id, old["collection_id"], "superseded_by")

    def get_task(self, task_id):
        for task in self.tasks:
            if task["id"] == task_id:
                return dict(task)
        raise KeyError(task_id)

    def set_task_status(self, task_id, status, *, message=None):
        for task in self.tasks:
            if task["id"] == task_id:
                task["status"] = status
                return
        raise KeyError(task_id)

    def event(self, project_id, collection_id, task_id, attempt_id, event_type, payload):
        self.events.append((event_type, payload))

    def set_collection_status(self, collection_id, status, *, commit=True):
        self.collection["status"] = status


def test_security_and_stateful_objective_requires_critic_and_plan_review():
    policy = classify_planning_policy(
        "Add API-key authentication and per-key rate limiting to the public API endpoint."
    )
    assert policy.explore_required is True
    assert policy.critic_required is True
    assert policy.plan_review_required is True
    assert "security" in policy.categories
    assert "stateful_behavior" in policy.categories


def test_trivial_implementation_still_gets_explore_and_critic_but_skips_plan_review():
    policy = classify_planning_policy("Fix a typo in README.md and update one example sentence.")
    assert policy.explore_required is True
    assert policy.critic_required is True
    assert policy.plan_review_required is False


def test_scheduler_injects_explore_then_critic_before_coordinator_and_plan_review(tmp_path: Path):
    store = FakePolicyStore(
        "Add API-key authentication and per-key rate limiting to the existing /hello endpoint."
    )
    for task in store.tasks:
        if task["id"] == "T-COORD":
            task["status"] = TaskStatus.READY.value
    scheduler = Scheduler(store, GlobalConfig(home=tmp_path), max_workers=1)

    changed = scheduler._enforce_planning_policy("COL-1")
    assert changed is True

    explores = [task for task in store.tasks if task["profile"] == "explore"]
    critics = [task for task in store.tasks if task["profile"] == "critic"]
    plan_reviews = [task for task in store.tasks if task["profile"] == "plan_review"]
    assert len(explores) == 1
    assert len(critics) == 1
    assert len(plan_reviews) == 0

    explore = explores[0]
    critic = critics[0]
    coord = next(task for task in store.tasks if task["id"] == "T-COORD")
    assert coord["status"] == TaskStatus.WAITING.value
    critic_deps = {task["id"] for task in store.dependencies(critic["id"])}
    coord_deps = {task["id"] for task in store.dependencies(coord["id"])}
    assert explore["id"] in critic_deps
    assert explore["id"] in coord_deps
    assert critic["id"] in coord_deps

    # Once preflight and coordination complete, policy installs the final plan review.
    store.set_task_status(explore["id"], TaskStatus.COMPLETE.value)
    store.set_task_status(critic["id"], TaskStatus.COMPLETE.value)
    store.set_task_status(coord["id"], TaskStatus.COMPLETE.value)
    changed = scheduler._enforce_planning_policy("COL-1")
    assert changed is True
    plan_reviews = [task for task in store.tasks if task["profile"] == "plan_review"]
    assert len(plan_reviews) == 1
    plan_review = plan_reviews[0]
    plan_deps = {task["id"] for task in store.dependencies(plan_review["id"])}
    impl_deps = {task["id"] for task in store.dependencies("T-IMPL")}
    assert critic["id"] in plan_deps
    assert plan_review["id"] in impl_deps
    assert any(event[0] == "PLANNING_POLICY_ENFORCED" for event in store.events)

    changed_again = scheduler._enforce_planning_policy("COL-1")
    assert changed_again is False
    assert len([task for task in store.tasks if task["profile"] == "explore"]) == 1
    assert len([task for task in store.tasks if task["profile"] == "critic"]) == 1
    assert len([task for task in store.tasks if task["profile"] == "plan_review"]) == 1


def test_existing_plan_review_is_made_to_wait_for_required_critic(tmp_path: Path):
    store = FakePolicyStore("Add API-key authentication to the public API endpoint.")
    store.tasks.append({
        "id": "T-PLAN",
        "project_id": "PRJ-1",
        "collection_id": "COL-1",
        "profile": "plan_review",
        "title": "Planner-created review",
        "objective": "Review current graph",
        "reason": "planner chose review",
        "status": TaskStatus.READY.value,
        "depth": 1,
        "created_at": "2026-11-10 10:01:02",
        "updated_at": "2026-11-10 10:01:02",
    })
    scheduler = Scheduler(store, GlobalConfig(home=tmp_path), max_workers=1)

    scheduler._enforce_planning_policy("COL-1")

    critics = [task for task in store.tasks if task["profile"] == "critic"]
    assert len(critics) == 1
    plan_deps = {task["id"] for task in store.dependencies("T-PLAN")}
    impl_deps = {task["id"] for task in store.dependencies("T-IMPL")}
    assert critics[0]["id"] in plan_deps
    assert "T-PLAN" in impl_deps
    assert len([task for task in store.tasks if task["profile"] == "plan_review"]) == 1


def test_scheduler_repairs_reciprocal_plan_review_implementation_cycle(tmp_path: Path):
    store = FakePolicyStore(
        "In the Twilio gateway I'd like to add support for transferring to +12125552211,6655."
    )
    store.tasks.append({
        "id": "T-CRITIC",
        "project_id": "PRJ-1",
        "collection_id": "COL-1",
        "profile": "critic",
        "title": "Preflight",
        "objective": "Check Twilio transfer semantics",
        "reason": "required",
        "status": TaskStatus.COMPLETE.value,
        "depth": 1,
        "created_at": "2026-11-10 10:00:30",
        "updated_at": "2026-11-10 10:01:00",
    })
    store.tasks.append({
        "id": "T-PLAN",
        "project_id": "PRJ-1",
        "collection_id": "COL-1",
        "profile": "plan_review",
        "title": "Review Twilio work graph",
        "objective": "Review the READY implementation contract",
        "reason": "gate",
        "status": TaskStatus.READY.value,
        "depth": 1,
        "created_at": "2026-11-10 10:01:10",
        "updated_at": "2026-11-10 10:01:10",
    })
    # Reproduce COL-f575af95a4: implement waits for plan review, while the
    # plan review incorrectly waits for that same implementation.
    store.add_dependency("T-IMPL", "T-PLAN")
    store.add_dependency("T-PLAN", "T-IMPL")
    store.add_dependency("T-PLAN", "T-CRITIC")
    store.record_critic_plan_baseline(
        "T-CRITIC",
        Scheduler(store, GlobalConfig(home=tmp_path), max_workers=1)._material_plan_fingerprint(
            store.get_collection("COL-1"), store.list_tasks("COL-1")
        ),
        source="initial_plan_seal",
        plan_review_task_id="T-PLAN",
    )
    scheduler = Scheduler(store, GlobalConfig(home=tmp_path), max_workers=1)

    assert scheduler._enforce_planning_policy("COL-1") is True

    plan_deps = {task["id"] for task in store.dependencies("T-PLAN")}
    impl_deps = {task["id"] for task in store.dependencies("T-IMPL")}
    assert "T-IMPL" not in plan_deps
    assert "T-CRITIC" in plan_deps
    assert "T-PLAN" in impl_deps
    repaired = [event for event in store.events if event[0] == "PLAN_REVIEW_DEPENDENCY_CYCLE_REPAIRED"]
    assert len(repaired) == 1
    assert repaired[0][1]["implement_task_id"] == "T-IMPL"


def test_direct_change_objective_requires_implementation_delivery():
    from praector.planning_policy import classify_delivery_policy

    policy = classify_delivery_policy("Add an authenticated GET /usage endpoint.")
    assert policy.implementation_required is True
    assert "implementation_delivery" in policy.categories

    research = classify_delivery_policy("Analyze the current authentication implementation and report risks.")
    assert research.implementation_required is False


def test_scheduler_creates_one_delivery_repair_coordinator_when_planning_finishes_without_implementation(tmp_path: Path):
    store = FakePolicyStore("Add an authenticated GET /usage endpoint.")
    store.tasks = [task for task in store.tasks if task["profile"] != "implement"]
    store.tasks.append({
        "id": "T-CRITIC",
        "project_id": "PRJ-1",
        "collection_id": "COL-1",
        "profile": "critic",
        "title": "Preflight",
        "objective": "Check plan",
        "reason": "required",
        "status": TaskStatus.COMPLETE.value,
        "depth": 1,
        "created_at": "2026-11-10 10:01:01",
        "updated_at": "2026-11-10 10:01:02",
    })
    scheduler = Scheduler(store, GlobalConfig(home=tmp_path), max_workers=1)

    assert scheduler._enforce_delivery_policy("COL-1") is True
    repairs = [
        task for task in store.tasks
        if task["profile"] == "coordinator" and task["title"] == "Delivery policy repair"
    ]
    assert len(repairs) == 1
    repair_deps = {task["id"] for task in store.dependencies(repairs[0]["id"])}
    assert "T-CRITIC" in repair_deps
    assert "T-COORD" in repair_deps
    assert any(event[0] == "DELIVERY_POLICY_ENFORCED" for event in store.events)

    # One deterministic repair opportunity only; no infinite coordinator loop.
    repairs[0]["status"] = TaskStatus.COMPLETE.value
    for task in store.tasks:
        if task["id"] == repairs[0]["id"]:
            task["status"] = TaskStatus.COMPLETE.value
    assert scheduler._enforce_delivery_policy("COL-1") is False
    assert len([task for task in store.tasks if task["title"] == "Delivery policy repair"]) == 1


def test_verifying_parent_with_unmet_blocking_dependency_is_deferred_to_waiting(tmp_path: Path):
    store = FakePolicyStore("Add API-key authentication to the endpoint.")
    store.tasks = [task for task in store.tasks if task["id"] != "T-IMPL"]
    for task in store.tasks:
        if task["id"] == "T-COORD":
            task["status"] = TaskStatus.VERIFYING.value
    store.tasks.append({
        "id": "T-CRITIC",
        "project_id": "PRJ-1",
        "collection_id": "COL-1",
        "profile": "critic",
        "title": "Blocking critic",
        "objective": "Critique",
        "reason": "blocking",
        "status": TaskStatus.RUNNING.value,
        "depth": 1,
        "created_at": "2026-11-10 10:01:01",
        "updated_at": "2026-11-10 10:01:01",
    })
    store.add_dependency("T-COORD", "T-CRITIC")
    scheduler = Scheduler(store, GlobalConfig(home=tmp_path), max_workers=1)

    assert scheduler._defer_completion_for_unmet_dependencies("T-COORD", attempt_id="A-1") is True
    assert store.get_task("T-COORD")["status"] == TaskStatus.WAITING.value
    assert any(event[0] == "TASK_COMPLETION_DEFERRED" for event in store.events)


def test_delivery_repair_waits_for_running_critic_even_if_coordinator_is_complete(tmp_path: Path):
    store = FakePolicyStore("Add an authenticated GET /usage endpoint.")
    store.tasks = [task for task in store.tasks if task["profile"] != "implement"]
    store.tasks.append({
        "id": "T-CRITIC",
        "project_id": "PRJ-1",
        "collection_id": "COL-1",
        "profile": "critic",
        "title": "Preflight",
        "objective": "Check plan",
        "reason": "required",
        "status": TaskStatus.RUNNING.value,
        "depth": 1,
        "created_at": "2026-11-10 10:01:01",
        "updated_at": "2026-11-10 10:01:02",
    })
    scheduler = Scheduler(store, GlobalConfig(home=tmp_path), max_workers=1)

    assert scheduler._enforce_delivery_policy("COL-1") is False
    assert not [task for task in store.tasks if task["title"] == "Delivery policy repair"]


class FinalizeCoordinatorStore:
    def __init__(self):
        self.tasks = {
            "T-PLAN": {
                "id": "T-PLAN", "project_id": "PRJ-1", "collection_id": "COL-1",
                "profile": "plan_review", "status": TaskStatus.COMPLETE.value,
                "requested_by_task_id": "T-COORD", "parent_task_id": "T-COORD",
                "result": {"status": "complete", "verdict": "pass"},
            },
            "T-COORD": {
                "id": "T-COORD", "project_id": "PRJ-1", "collection_id": "COL-1",
                "profile": "coordinator", "status": TaskStatus.WAITING.value,
                "result": {},
            },
            "T-CRITIC": {
                "id": "T-CRITIC", "project_id": "PRJ-1", "collection_id": "COL-1",
                "profile": "critic", "status": TaskStatus.COMPLETE.value,
                "result": {"status": "complete"},
            },
        }
        self.events = []

    def get_task(self, task_id):
        return dict(self.tasks[task_id])

    def dependencies(self, task_id):
        assert task_id == "T-COORD"
        return [dict(self.tasks["T-CRITIC"]), dict(self.tasks["T-PLAN"])]

    def dependents(self, task_id):
        if task_id == "T-PLAN":
            return [dict(self.tasks["T-COORD"])]
        return []

    def submit_result(self, task_id, result):
        self.tasks[task_id]["result"] = dict(result)
        self.tasks[task_id]["status"] = TaskStatus.VERIFYING.value
        return dict(self.tasks[task_id])

    def set_task_status(self, task_id, status, *, message=None):
        self.tasks[task_id]["status"] = status

    def event(self, project_id, collection_id, task_id, attempt_id, event_type, payload):
        self.events.append((event_type, payload))


def test_passing_blocking_plan_review_finalizes_coordinator_without_resuming_planner(tmp_path: Path):
    store = FinalizeCoordinatorStore()
    scheduler = Scheduler(store, GlobalConfig(home=tmp_path), max_workers=1)

    assert scheduler._finalize_blocking_coordinator_after_plan_review(store.get_task("T-PLAN")) is True
    assert store.get_task("T-COORD")["status"] == TaskStatus.COMPLETE.value
    assert store.get_task("T-COORD")["result"]["status"] == "complete"
    assert any(event[0] == "COORDINATOR_FINALIZED_AFTER_PLAN_REVIEW" for event in store.events)


def test_policy_defers_auto_plan_review_while_coordinator_is_active(tmp_path: Path):
    store = FakePolicyStore(
        "Add API-key authentication and per-key rate limiting to the existing /hello endpoint."
    )
    # Reproduce the live race: the resumed coordinator has created implementation
    # work but has not yet issued its final blocking plan-review request.
    for task in store.tasks:
        if task["id"] == "T-COORD":
            task["status"] = TaskStatus.RUNNING.value
            task["updated_at"] = "2026-11-10 10:02:00"
    store.tasks.append({
        "id": "T-CRITIC",
        "project_id": "PRJ-1",
        "collection_id": "COL-1",
        "profile": "critic",
        "title": "Preflight",
        "objective": "Critique plan",
        "reason": "required",
        "status": TaskStatus.COMPLETE.value,
        "depth": 1,
        "created_at": "2026-11-10 10:00:30",
        "updated_at": "2026-11-10 10:01:30",
    })
    scheduler = Scheduler(store, GlobalConfig(home=tmp_path), max_workers=1)

    changed = scheduler._enforce_planning_policy("COL-1")
    assert changed is False
    assert [task for task in store.tasks if task["profile"] == "plan_review"] == []

    # If the coordinator terminates without creating the required gate, policy
    # becomes the backstop and creates exactly one plan review afterward.
    for task in store.tasks:
        if task["id"] == "T-COORD":
            task["status"] = TaskStatus.COMPLETE.value
            task["updated_at"] = "2026-11-10 10:03:00"

    changed = scheduler._enforce_planning_policy("COL-1")
    assert changed is True
    reviews = [task for task in store.tasks if task["profile"] == "plan_review"]
    assert len(reviews) == 1
    assert reviews[0]["title"] == "Policy plan review"
    assert reviews[0]["id"] in {dep["id"] for dep in store.dependencies("T-IMPL")}


def test_forced_stale_safety_net_inserts_revalidation_without_proactive_critic(tmp_path: Path):
    from praector.planning_policy import material_plan_fingerprint

    store = FakePolicyStore(
        "Add API-key authentication and per-key rate limiting to the existing /hello endpoint."
    )
    # The initial critic-informed plan has already been sealed and has a final
    # plan review waiting to run.
    store.tasks.append({
        "id": "T-CRITIC",
        "project_id": "PRJ-1",
        "collection_id": "COL-1",
        "profile": "critic",
        "title": "Preflight",
        "objective": "Critique the initial plan",
        "reason": "required",
        "status": TaskStatus.COMPLETE.value,
        "depth": 1,
        "created_at": "2026-11-10 10:00:30",
        "updated_at": "2026-11-10 10:01:30",
    })
    store.tasks.append({
        "id": "T-PLAN",
        "project_id": "PRJ-1",
        "collection_id": "COL-1",
        "profile": "plan_review",
        "title": "Final plan review",
        "objective": "Review the graph",
        "reason": "gate",
        "status": TaskStatus.READY.value,
        "depth": 1,
        "created_at": "2026-11-10 10:02:00",
        "updated_at": "2026-11-10 10:02:00",
    })
    store.add_dependency("T-PLAN", "T-CRITIC")
    store.add_dependency("T-IMPL", "T-PLAN")

    # Seal critic 1 to the original plan, then materially change the
    # implementation contract after the seal.
    original_fp = material_plan_fingerprint(store.collection["objective"], store.list_tasks("COL-1"), store.list_edges("COL-1"))
    store.record_critic_plan_baseline("T-CRITIC", original_fp, source="initial_plan_seal", plan_review_task_id="T-PLAN")
    assert len([task for task in store.tasks if task["profile"] == "critic"]) == 1
    assert not [
        task for task in store.tasks
        if task["profile"] == "coordinator" and task["status"] != TaskStatus.COMPLETE.value
    ]
    for task in store.tasks:
        if task["id"] == "T-IMPL":
            task["objective"] = "Do the work and introduce a new persistent security state model"
    changed_fp = material_plan_fingerprint(
        store.collection["objective"], store.list_tasks("COL-1"), store.list_edges("COL-1")
    )
    assert changed_fp != original_fp
    # No model/coordinator requested another critic. The scheduler must detect
    # the sealed-plan mismatch and manufacture revalidation itself.
    assert len([task for task in store.tasks if task["profile"] == "critic"]) == 1

    scheduler = Scheduler(store, GlobalConfig(home=tmp_path), max_workers=1)
    assert scheduler._enforce_planning_policy("COL-1") is True

    critics = [task for task in store.tasks if task["profile"] == "critic"]
    assert len(critics) == 2
    fresh = critics[-1]
    assert fresh["title"] == "Revalidate materially changed plan"
    assert store.critic_plan_baseline(fresh["id"])["source"] == "material_revalidation"

    repairs = [
        task for task in store.tasks
        if task["profile"] == "coordinator" and task["title"] == "Apply critic revalidation"
    ]
    assert len(repairs) == 1
    repair = repairs[0]
    assert fresh["id"] in {dep["id"] for dep in store.dependencies(repair["id"])}
    plan_deps = {dep["id"] for dep in store.dependencies("T-PLAN")}
    assert fresh["id"] in plan_deps
    assert repair["id"] in plan_deps
    assert any(event[0] == "CRITIC_REVALIDATION_REQUIRED" for event in store.events)


def test_revalidation_repeats_if_coordinator_makes_another_material_change(tmp_path: Path):
    from praector.planning_policy import material_plan_fingerprint

    store = FakePolicyStore(
        "Add API-key authentication and per-key rate limiting to the existing /hello endpoint."
    )
    store.tasks.append({
        "id": "T-CRITIC-1", "project_id": "PRJ-1", "collection_id": "COL-1",
        "profile": "critic", "title": "Preflight", "objective": "Critique", "reason": "required",
        "status": TaskStatus.COMPLETE.value, "depth": 1,
        "created_at": "2026-11-10 10:00:30", "updated_at": "2026-11-10 10:01:00",
    })
    initial_fp = material_plan_fingerprint(store.collection["objective"], store.list_tasks("COL-1"), store.list_edges("COL-1"))
    store.record_critic_plan_baseline("T-CRITIC-1", initial_fp, source="initial_plan_seal")
    # First material change.
    for task in store.tasks:
        if task["id"] == "T-IMPL":
            task["objective"] = "Changed implementation contract"

    scheduler = Scheduler(store, GlobalConfig(home=tmp_path), max_workers=1)
    assert scheduler._enforce_planning_policy("COL-1") is True
    fresh1 = [task for task in store.tasks if task["profile"] == "critic"][-1]
    repair1 = [task for task in store.tasks if task["title"] == "Apply critic revalidation"][-1]

    # Simulate critic 2 completing and its coordinator consuming findings, then
    # making another material change before final review.
    for task in store.tasks:
        if task["id"] == fresh1["id"]:
            task["status"] = TaskStatus.COMPLETE.value
        if task["id"] == repair1["id"]:
            task["status"] = TaskStatus.COMPLETE.value
    store.tasks.append({
        "id": "T-IMPL-2", "project_id": "PRJ-1", "collection_id": "COL-1",
        "profile": "implement", "title": "Second implementation slice",
        "objective": "New material scope introduced after critic revalidation",
        "reason": "critic findings", "status": TaskStatus.READY.value, "depth": 2,
        "workspace_mode": "collection", "created_at": "2026-11-10 10:05:00", "updated_at": "2026-11-10 10:05:00",
    })

    assert scheduler._enforce_planning_policy("COL-1") is True
    critics = [task for task in store.tasks if task["profile"] == "critic"]
    assert len(critics) == 3
    assert critics[-1]["id"] != fresh1["id"]
    assert store.critic_plan_baseline(critics[-1]["id"])["source"] == "material_revalidation"


def test_fresh_revision_seal_is_not_relabelled_as_compatibility(tmp_path: Path):
    from praector.planning_policy import material_plan_fingerprint

    store = FakePolicyStore(
        "Add API-key authentication and per-key rate limiting to the existing /hello endpoint."
    )
    store.tasks.append({
        "id": "T-CRITIC-REV", "project_id": "PRJ-1", "collection_id": "COL-1",
        "profile": "critic", "title": "Revision critic", "objective": "Critique revision",
        "reason": "revision", "status": TaskStatus.COMPLETE.value, "depth": 1,
        "created_at": "2026-11-10 10:00:30", "updated_at": "2026-11-10 10:01:30",
    })
    store.tasks.append({
        "id": "T-PLAN-REV", "project_id": "PRJ-1", "collection_id": "COL-1",
        "profile": "plan_review", "title": "Review revision", "objective": "Review revised graph",
        "reason": "gate", "status": TaskStatus.READY.value, "depth": 1,
        "created_at": "2026-11-10 10:02:00", "updated_at": "2026-11-10 10:02:00",
    })
    store.add_dependency("T-PLAN-REV", "T-CRITIC-REV")
    store.add_dependency("T-IMPL", "T-PLAN-REV")
    fp = material_plan_fingerprint(
        store.collection["objective"], store.list_tasks("COL-1"), store.list_edges("COL-1")
    )
    store.record_critic_plan_baseline(
        "T-CRITIC-REV", fp, source="revision_plan_seal", plan_review_task_id="T-PLAN-REV"
    )

    scheduler = Scheduler(store, GlobalConfig(home=tmp_path), max_workers=1)
    scheduler._enforce_planning_policy("COL-1")

    baseline = store.critic_plan_baseline("T-CRITIC-REV")
    assert baseline["source"] == "revision_plan_seal"
    compat_events = [
        payload for event_type, payload in store.events
        if event_type == "CRITIC_PLAN_BASELINE" and payload.get("source") == "compatibility_plan_seal"
    ]
    assert compat_events == []


def test_pre_022_plan_review_gets_one_compatibility_critic_seal(tmp_path: Path):
    store = FakePolicyStore(
        "Add API-key authentication and per-key rate limiting to the existing /hello endpoint."
    )
    store.tasks.append({
        "id": "T-CRITIC-COMPAT",
        "project_id": "PRJ-1",
        "collection_id": "COL-1",
        "profile": "critic",
        "title": "Old critic",
        "objective": "Preflight",
        "reason": "required",
        "status": TaskStatus.COMPLETE.value,
        "depth": 1,
        "created_at": "2026-11-10 10:00:30",
        "updated_at": "2026-11-10 10:01:30",
    })
    store.tasks.append({
        "id": "T-PLAN-COMPAT",
        "project_id": "PRJ-1",
        "collection_id": "COL-1",
        "profile": "plan_review",
        "title": "Old final plan review",
        "objective": "Review graph",
        "reason": "gate",
        "status": TaskStatus.READY.value,
        "depth": 1,
        "created_at": "2026-11-10 10:02:00",
        "updated_at": "2026-11-10 10:02:00",
    })
    store.add_dependency("T-PLAN-COMPAT", "T-CRITIC-COMPAT")
    store.add_dependency("T-IMPL", "T-PLAN-COMPAT")

    scheduler = Scheduler(store, GlobalConfig(home=tmp_path), max_workers=1)
    assert store.critic_plan_baseline("T-CRITIC-COMPAT") is None
    assert scheduler._enforce_planning_policy("COL-1") is True
    baseline = store.critic_plan_baseline("T-CRITIC-COMPAT")
    assert baseline is not None
    assert baseline["source"] == "compatibility_plan_seal"
    assert len([task for task in store.tasks if task["profile"] == "critic"]) == 1

    # A second reconciliation is stable and does not manufacture a revalidation.
    assert scheduler._enforce_planning_policy("COL-1") is False
    assert len([task for task in store.tasks if task["profile"] == "critic"]) == 1

TWILIO_CONVERSATIONAL_OBJECTIVE = """In the Twilio gateway we make use of templates. I'd like to add a new feature, likely a new template, that when we initiate a transfer to a phone number and it has commas like +12125552211,6655 it would initial a call transfer much like it does now using the Dial verb. It would need to dial these other digits, 6655 in this case, at answer. This is how we will support transferring calls in Twilio to other internal extensions.

This only applies to bots that make use of Twilio."""


def test_conversational_direct_change_requires_delivery():
    from praector.planning_policy import classify_delivery_policy

    policy = classify_delivery_policy(TWILIO_CONVERSATIONAL_OBJECTIVE)
    assert policy.implementation_required is True
    assert "implementation_delivery" in policy.categories


def test_conversational_twilio_change_requires_critic_and_plan_review():
    policy = classify_planning_policy(TWILIO_CONVERSATIONAL_OBJECTIVE)

    assert policy.critic_required is True
    assert policy.plan_review_required is True
    assert "implementation_change" in policy.categories
    assert "external_integration" in policy.categories


def test_scheduler_gates_conversational_twilio_change_before_implementation(tmp_path: Path):
    store = FakePolicyStore(TWILIO_CONVERSATIONAL_OBJECTIVE)
    scheduler = Scheduler(store, GlobalConfig(home=tmp_path), max_workers=1)

    changed = scheduler._enforce_planning_policy("COL-1")
    assert changed is True

    critics = [task for task in store.tasks if task["profile"] == "critic"]
    plan_reviews = [task for task in store.tasks if task["profile"] == "plan_review"]
    assert len(critics) == 1
    assert len(plan_reviews) == 1

    plan_deps = {task["id"] for task in store.dependencies(plan_reviews[0]["id"])}
    impl_deps = {task["id"] for task in store.dependencies("T-IMPL")}
    assert critics[0]["id"] in plan_deps
    assert plan_reviews[0]["id"] in impl_deps


def test_conversational_research_does_not_become_write_delivery():
    from praector.planning_policy import classify_delivery_policy

    objective = "Analyze how the Twilio gateway would need to change if we later wanted to add comma-separated extension dialing. Do not modify code."
    delivery = classify_delivery_policy(objective)
    planning = classify_planning_policy(objective)

    # A read-only analysis request must never be forced into write delivery.
    assert delivery.implementation_required is False
    # Intentional widening: because the objective names an external integration
    # (Twilio gateway), the category-derived risk gates fire regardless of the
    # "analyze" verb phrasing. The delivery classification above is the invariant
    # this test protects, not the absence of these gates.
    assert "external_integration" in planning.categories
    assert planning.explore_required is True
    assert planning.plan_review_required is True
    assert planning.critic_required is True


def test_conversational_nontrivial_code_change_requires_explore_critic_and_plan_review():
    from praector.planning_policy import classify_delivery_policy

    objective = "The CSV parser is intentionally small. I'd like to add support for escaped commas while preserving existing parsing behavior."
    delivery = classify_delivery_policy(objective)
    planning = classify_planning_policy(objective)

    assert delivery.implementation_required is True
    assert planning.explore_required is True
    assert planning.plan_review_required is True
    assert planning.critic_required is True
    assert "implementation_change" in planning.categories


def test_twilio_repair_graph_collapses_transitive_coordinator_plan_review_cycle(tmp_path: Path):
    """Regression for COL-f575af95a4 after the 0.1.27 reciprocal-cycle repair.

    The bad recovery graph had two waiting coordinators and two plan reviews:
    initial coordinator -> old plan review -> attention coordinator -> new plan
    review -> initial coordinator. The reconciler must remove coordinator->review
    back-edges, collapse the obsolete old review, and leave one final gate.
    """
    from praector.planning_policy import material_plan_fingerprint

    store = FakePolicyStore(
        "In the Twilio gateway I'd like to add support for transferring to +12125552211,6655."
    )
    # Replace the minimal fixture with the live-recovery shape.
    store.tasks = [
        {
            "id": "T-INIT", "project_id": "PRJ-1", "collection_id": "COL-1",
            "profile": "coordinator", "title": "Initial coordination", "objective": "Plan",
            "reason": "entry", "status": TaskStatus.WAITING.value, "depth": 0,
            "created_at": "2026-11-10 10:00:00", "updated_at": "2026-11-10 10:05:00",
            "result": {},
        },
        {
            "id": "T-CRITIC", "project_id": "PRJ-1", "collection_id": "COL-1",
            "profile": "critic", "title": "Preflight", "objective": "Critique",
            "reason": "required", "status": TaskStatus.COMPLETE.value, "depth": 1,
            "created_at": "2026-11-10 10:00:10", "updated_at": "2026-11-10 10:01:00",
            "result": {"status": "complete"},
        },
        {
            "id": "T-RESEARCH", "project_id": "PRJ-1", "collection_id": "COL-1",
            "profile": "research", "title": "Mechanism research", "objective": "Research Twilio",
            "reason": "evidence", "status": TaskStatus.COMPLETE.value, "depth": 1,
            "created_at": "2026-11-10 10:00:20", "updated_at": "2026-11-10 10:02:00",
            "result": {"status": "complete"},
        },
        {
            "id": "T-OLD-PLAN", "project_id": "PRJ-1", "collection_id": "COL-1",
            "profile": "plan_review", "title": "Old plan review", "objective": "Review old graph",
            "reason": "gate", "status": TaskStatus.WAITING.value, "depth": 1,
            "created_at": "2026-11-10 10:02:10", "updated_at": "2026-11-10 10:04:00",
            "result": {},
        },
        {
            "id": "T-ATTN", "project_id": "PRJ-1", "collection_id": "COL-1",
            "profile": "coordinator", "title": "Coordinator attention", "objective": "Repair graph",
            "reason": "repair", "status": TaskStatus.WAITING.value, "depth": 2,
            "created_at": "2026-11-10 10:03:00", "updated_at": "2026-11-10 10:05:00",
            "result": {},
        },
        {
            "id": "T-NEW-PLAN", "project_id": "PRJ-1", "collection_id": "COL-1",
            "profile": "plan_review", "title": "Review repaired graph", "objective": "Review new graph",
            "reason": "replacement gate", "status": TaskStatus.READY.value, "depth": 3,
            "created_at": "2026-11-10 10:04:00", "updated_at": "2026-11-10 10:04:00",
            "result": {},
        },
        {
            "id": "T-NEW-IMPL", "project_id": "PRJ-1", "collection_id": "COL-1",
            "profile": "implement", "title": "Implement sendDigits", "objective": "Use Number sendDigits",
            "reason": "repaired plan", "status": TaskStatus.READY.value, "depth": 3,
            "workspace_mode": "collection", "created_at": "2026-11-10 10:04:10", "updated_at": "2026-11-10 10:04:10",
            "result": {},
        },
    ]
    # initial -> old plan -> attention -> new plan -> initial is a hard cycle.
    store.add_dependency("T-INIT", "T-CRITIC")
    store.add_dependency("T-INIT", "T-OLD-PLAN")
    store.add_dependency("T-OLD-PLAN", "T-CRITIC")
    store.add_dependency("T-OLD-PLAN", "T-RESEARCH")
    store.add_dependency("T-OLD-PLAN", "T-ATTN")
    store.add_dependency("T-ATTN", "T-NEW-PLAN")
    store.add_dependency("T-NEW-PLAN", "T-CRITIC")
    store.add_dependency("T-NEW-PLAN", "T-RESEARCH")
    store.add_dependency("T-NEW-PLAN", "T-INIT")
    store.add_dependency("T-NEW-IMPL", "T-RESEARCH")
    store.add_dependency("T-NEW-IMPL", "T-OLD-PLAN")
    store.add_dependency("T-NEW-IMPL", "T-NEW-PLAN")

    # Keep critic freshness stable so this test isolates graph convergence.
    fp = material_plan_fingerprint(
        store.collection["objective"], store.list_tasks("COL-1"), store.list_edges("COL-1")
    )
    store.record_critic_plan_baseline("T-CRITIC", fp, source="initial_plan_seal", plan_review_task_id="T-OLD-PLAN")

    scheduler = Scheduler(store, GlobalConfig(home=tmp_path), max_workers=1)
    assert scheduler._enforce_planning_policy("COL-1") is True

    assert store.get_task("T-OLD-PLAN")["status"] == TaskStatus.SUPERSEDED.value
    active_reviews = [
        task for task in store.tasks
        if task["profile"] == "plan_review" and task["status"] != TaskStatus.SUPERSEDED.value
    ]
    assert [task["id"] for task in active_reviews] == ["T-NEW-PLAN"]

    new_plan_deps = {task["id"] for task in store.dependencies("T-NEW-PLAN")}
    assert "T-INIT" not in new_plan_deps
    assert "T-ATTN" not in new_plan_deps
    assert {"T-CRITIC", "T-RESEARCH"}.issubset(new_plan_deps)

    # Both waiting coordinators and the implementation converge on the one gate.
    assert "T-NEW-PLAN" in {task["id"] for task in store.dependencies("T-INIT")}
    assert "T-NEW-PLAN" in {task["id"] for task in store.dependencies("T-ATTN")}
    impl_deps = {task["id"] for task in store.dependencies("T-NEW-IMPL")}
    assert "T-OLD-PLAN" not in impl_deps
    assert "T-NEW-PLAN" in impl_deps

    assert store.dependency_path("T-NEW-PLAN", "T-INIT") is not None
    assert store.dependency_path("T-INIT", "T-NEW-PLAN") is None
    assert any(event[0] == "REDUNDANT_PLAN_REVIEW_COLLAPSED" for event in store.events)
    assert len([event for event in store.events if event[0] == "PLAN_REVIEW_COORDINATOR_CYCLE_REPAIRED"]) == 2


class StrictCyclePolicyStore(FakePolicyStore):
    """Fake policy store that enforces the same DAG rule as DoltStore."""

    def add_edge(self, from_task_id, to_task_id, collection_id, relation="depends_on"):
        edge = {
            "from_task_id": from_task_id,
            "to_task_id": to_task_id,
            "collection_id": collection_id,
            "relation": relation,
        }
        if edge in self.edges:
            return
        if relation == "depends_on":
            path = self.dependency_path(to_task_id, from_task_id)
            if path is not None:
                cycle = " -> ".join([from_task_id] + path)
                raise ValueError(
                    f"Dependency {from_task_id}->{to_task_id} would create a cycle in {collection_id}: {cycle}"
                )
        self.edges.append(edge)


def test_waiting_coordinator_is_never_added_as_final_plan_review_prerequisite(tmp_path: Path):
    """Regression for the 0.1.29 COL-f575af95a4 quarantine.

    A revalidation coordinator had already handed off to the final plan review,
    so it was WAITING on that review. Planning policy must not then strengthen
    the review by adding the WAITING coordinator as a prerequisite, which would
    create the reverse edge and a hard dependency cycle.
    """
    from praector.planning_policy import material_plan_fingerprint

    store = StrictCyclePolicyStore(TWILIO_CONVERSATIONAL_OBJECTIVE)
    store.tasks = [
        {
            "id": "T-INIT", "project_id": "PRJ-1", "collection_id": "COL-1",
            "profile": "coordinator", "title": "Initial coordination", "objective": "Plan",
            "reason": "entry", "status": TaskStatus.WAITING.value, "depth": 0,
            "created_at": "2026-11-10 10:00:00", "updated_at": "2026-11-10 10:10:00", "result": {},
        },
        {
            "id": "T-CRITIC-1", "project_id": "PRJ-1", "collection_id": "COL-1",
            "profile": "critic", "title": "Initial critic", "objective": "Critique",
            "reason": "required", "status": TaskStatus.COMPLETE.value, "depth": 1,
            "created_at": "2026-11-10 10:00:10", "updated_at": "2026-11-10 10:01:00", "result": {},
        },
        {
            "id": "T-RESEARCH", "project_id": "PRJ-1", "collection_id": "COL-1",
            "profile": "research", "title": "Twilio mechanism", "objective": "Research sendDigits",
            "reason": "evidence", "status": TaskStatus.COMPLETE.value, "depth": 1,
            "workspace_mode": "collection", "created_at": "2026-11-10 10:00:20", "updated_at": "2026-11-10 10:02:00", "result": {},
        },
        {
            "id": "T-CRITIC-2", "project_id": "PRJ-1", "collection_id": "COL-1",
            "profile": "critic", "title": "Revalidate materially changed plan", "objective": "Revalidate",
            "reason": "material change", "status": TaskStatus.COMPLETE.value, "depth": 2,
            "created_at": "2026-11-10 10:03:00", "updated_at": "2026-11-10 10:04:00", "result": {},
        },
        {
            "id": "T-APPLY-OLD", "project_id": "PRJ-1", "collection_id": "COL-1",
            "profile": "coordinator", "title": "Apply critic revalidation", "objective": "Apply critic",
            "reason": "repair", "status": TaskStatus.WAITING.value, "depth": 3,
            "created_at": "2026-11-10 10:04:10", "updated_at": "2026-11-10 10:08:00", "result": {},
        },
        {
            "id": "T-CRITIC-3", "project_id": "PRJ-1", "collection_id": "COL-1",
            "profile": "critic", "title": "Revalidate materially changed plan", "objective": "Revalidate newest plan",
            "reason": "second material change", "status": TaskStatus.READY.value, "depth": 4,
            "created_at": "2026-11-10 10:08:10", "updated_at": "2026-11-10 10:08:10", "result": {},
        },
        {
            "id": "T-APPLY-NEW", "project_id": "PRJ-1", "collection_id": "COL-1",
            "profile": "coordinator", "title": "Apply critic revalidation", "objective": "Apply newest critic",
            "reason": "repair", "status": TaskStatus.READY.value, "depth": 5,
            "created_at": "2026-11-10 10:08:20", "updated_at": "2026-11-10 10:08:20", "result": {},
        },
        {
            "id": "T-PLAN", "project_id": "PRJ-1", "collection_id": "COL-1",
            "profile": "plan_review", "title": "Final plan review: Twilio comma-transfer",
            "objective": "Review current durable sendDigits graph", "reason": "gate",
            "status": TaskStatus.READY.value, "depth": 4,
            "created_at": "2026-11-10 10:07:00", "updated_at": "2026-11-10 10:08:30", "result": {},
        },
        {
            "id": "T-IMPL", "project_id": "PRJ-1", "collection_id": "COL-1",
            "profile": "implement", "title": "Implement Twilio comma transfer",
            "objective": "Implement via Number sendDigits", "reason": "approved mechanism",
            "status": TaskStatus.READY.value, "depth": 4, "workspace_mode": "collection",
            "created_at": "2026-11-10 10:07:10", "updated_at": "2026-11-10 10:07:10", "result": {},
        },
    ]
    # The WAITING coordinators already depend on the final review. The newest
    # active revalidation coordinator is a legitimate prerequisite of the review.
    store.add_dependency("T-INIT", "T-PLAN")
    store.add_dependency("T-APPLY-OLD", "T-CRITIC-2")
    store.add_dependency("T-APPLY-OLD", "T-PLAN")
    store.add_dependency("T-APPLY-NEW", "T-CRITIC-3")
    store.add_dependency("T-PLAN", "T-RESEARCH")
    store.add_dependency("T-PLAN", "T-CRITIC-1")
    store.add_dependency("T-PLAN", "T-CRITIC-2")
    store.add_dependency("T-PLAN", "T-CRITIC-3")
    store.add_dependency("T-PLAN", "T-APPLY-NEW")
    store.add_dependency("T-IMPL", "T-RESEARCH")
    store.add_dependency("T-IMPL", "T-PLAN")

    # Latest critic is still running/ready, so no new revalidation round should
    # be manufactured during this reconciliation pass.
    old_fp = material_plan_fingerprint(
        store.collection["objective"], store.list_tasks("COL-1"), store.list_edges("COL-1")
    )
    store.record_critic_plan_baseline("T-CRITIC-2", old_fp, source="material_revalidation")

    scheduler = Scheduler(store, GlobalConfig(home=tmp_path), max_workers=1)
    # The strict fake would raise if policy tried T-APPLY-OLD -> T-PLAN.
    scheduler._enforce_planning_policy("COL-1")

    deps = {task["id"] for task in store.dependencies("T-PLAN")}
    assert "T-APPLY-OLD" not in deps
    assert "T-INIT" not in deps
    assert "T-APPLY-NEW" in deps
    assert store.dependency_path("T-PLAN", "T-APPLY-OLD") is not None
    assert store.dependency_path("T-APPLY-OLD", "T-PLAN") is None


def test_revision_generation_gets_fresh_explore_and_critic(tmp_path: Path):
    store = FakePolicyStore(
        "Add API-key authentication and per-key rate limiting to the existing /hello endpoint."
    )
    # Prior generation already completed its deterministic preflight.
    store.tasks.extend([
        {
            "id": "T-OLD-EXPLORE", "project_id": "PRJ-1", "collection_id": "COL-1",
            "profile": "explore", "title": "Explore current implementation surface",
            "objective": "Map repo", "reason": "policy", "status": TaskStatus.COMPLETE.value,
            "depth": 1, "requested_by_task_id": "T-COORD", "parent_task_id": "T-COORD",
            "created_at": "2026-11-10 10:00:10", "updated_at": "2026-11-10 10:00:20",
        },
        {
            "id": "T-OLD-CRITIC", "project_id": "PRJ-1", "collection_id": "COL-1",
            "profile": "critic", "title": "Preflight implementation plan",
            "objective": "Critique", "reason": "policy", "status": TaskStatus.COMPLETE.value,
            "depth": 1, "requested_by_task_id": "T-COORD", "parent_task_id": "T-COORD",
            "created_at": "2026-11-10 10:00:21", "updated_at": "2026-11-10 10:00:30",
        },
        {
            "id": "T-REV", "project_id": "PRJ-1", "collection_id": "COL-1",
            "profile": "coordinator", "title": "Acceptance revision",
            "objective": "Revise accepted work", "reason": "operator revision",
            "status": TaskStatus.READY.value, "depth": 0,
            "created_at": "2026-11-10 11:00:00", "updated_at": "2026-11-10 11:00:00",
        },
    ])
    scheduler = Scheduler(store, GlobalConfig(home=tmp_path), max_workers=1)

    assert scheduler._enforce_planning_policy("COL-1") is True

    revision_explores = [
        task for task in store.tasks
        if task["profile"] == "explore" and task.get("requested_by_task_id") == "T-REV"
    ]
    revision_critics = [
        task for task in store.tasks
        if task["profile"] == "critic" and task.get("requested_by_task_id") == "T-REV"
    ]
    assert len(revision_explores) == 1
    assert len(revision_critics) == 1
    assert revision_explores[0]["id"] != "T-OLD-EXPLORE"
    assert revision_critics[0]["id"] != "T-OLD-CRITIC"
    critic_deps = {task["id"] for task in store.dependencies(revision_critics[0]["id"])}
    assert revision_explores[0]["id"] in critic_deps
    revision_deps = {task["id"] for task in store.dependencies("T-REV")}
    assert revision_explores[0]["id"] in revision_deps
    assert revision_critics[0]["id"] in revision_deps


def _valid_fresh_research_result():
    return {
        "status": "complete",
        "summary": "current external contract verified",
        "as_of": "2026-11-12",
        "sources": [{"id": "spec", "authority": "official", "authority_level": "official_specification", "reference": "https://example.test/spec", "version": "current"}],
        "claims": [{
            "id": "claim-x",
            "claim": "The current specification requires behavior X.",
            "source": "spec",
            "authority": "official specification",
            "normative_level": "MUST",
            "planner_directive": "Implementation MUST preserve behavior X.",
            "directive_effect": "preserve",
            "confidence": "high",
        }],
        "verification_oracles": [{"kind": "official_reference_oracle", "name": "official SDK"}],
        "risks": [],
        "unresolved": [],
    }


def _research_consistency_for(tasks):
    constraints = research_claim_constraints(tasks)
    return {
        "verdict": "pass",
        "checked_claim_ids": [item["claim_ref"] for item in constraints],
        "claims": [
            {
                "claim_id": item["claim_ref"],
                "disposition": "preserved",
                "inherited_directive": item["planner_directive"],
                "derived_directives": [],
            }
            for item in constraints
        ],
        "contradictions": [],
    }


def test_v022_external_protocol_policy_requires_fresh_research_and_conformance():
    policy = classify_planning_policy("Implement support for the current MCP protocol specification in an HTTP server.")
    assert policy.explore_required
    assert policy.research_required
    assert policy.critic_required
    assert policy.plan_review_required
    assert policy.external_conformance_required
    assert policy.threat_model_required

    local = classify_planning_policy("Refactor local parser helper to remove duplicate code and update its tests.")
    assert local.research_required is False

    dependency_graph = classify_planning_policy("Refactor the local task dependency graph reconciliation and add unit tests.")
    assert dependency_graph.research_required is False

    current_local = classify_planning_policy("Inspect the current implementation and refactor its local parser tests.")
    assert current_local.research_required is False

    current_news = classify_planning_policy("Research current news and recent events affecting the vendor before making a recommendation.")
    assert current_news.research_required is True


def test_v022_research_result_schema_and_fingerprint_include_evidence():
    result = _valid_fresh_research_result()
    validate_deterministic_research_result(result, require_oracle=True)
    bad = _valid_fresh_research_result()
    bad["claims"] = [{
        "id": "claim-x", "claim": "x", "source": "spec", "authority": "official", "normative_level": "MUST",
        "planner_directive": "ignore x", "directive_effect": "weaken", "confidence": "high",
    }]
    import pytest
    with pytest.raises(ValueError):
        validate_deterministic_research_result(bad, require_oracle=True)

    task = {
        "id": "T-R", "profile": "research", "title": "Fresh external research preflight",
        "objective": "research", "reason": "fresh external evidence", "workspace_mode": "collection",
        "result": result,
    }
    fp1 = material_plan_fingerprint("Implement current MCP protocol support", [task], [])
    changed = _valid_fresh_research_result()
    changed["as_of"] = "2026-11-13"
    task2 = dict(task, result=changed)
    fp2 = material_plan_fingerprint("Implement current MCP protocol support", [task2], [])
    assert fp1 != fp2


def test_v022_research_trace_and_oracle_validation():
    import pytest

    unbacked = _valid_fresh_research_result()
    unbacked["planner_directives"] = ["A contradictory free-form directive"]
    with pytest.raises(ValueError):
        validate_deterministic_research_result(unbacked, require_oracle=True)

    black_box_only = _valid_fresh_research_result()
    black_box_only["verification_oracles"] = [{"kind": "black_box_test", "name": "implementation-owned client"}]
    with pytest.raises(ValueError):
        validate_deterministic_research_result(black_box_only, require_oracle=True)
    black_box_only["authoritative_oracle_unavailable_reason"] = "No official oracle is available."
    validate_deterministic_research_result(black_box_only, require_oracle=True)

    research_task = {
        "id": "T-R", "profile": "research", "status": TaskStatus.COMPLETE.value,
        "result": _valid_fresh_research_result(),
    }
    with pytest.raises(ValueError):
        validate_research_consumer_result(
            {"research_consistency": {"verdict": "pass", "checked_claim_ids": [], "contradictions": []}},
            [research_task], require_pass=True,
        )
    validate_research_consumer_result(
        {"research_consistency": _research_consistency_for([research_task])},
        [research_task], require_pass=True,
    )

    with pytest.raises(ValueError):
        validate_external_conformance_review_result({
            "verdict": "pass",
            "verification_oracles": [{"kind": "black_box_test", "name": "same-team client", "used": True}],
        })
    validate_external_conformance_review_result({
        "verdict": "pass",
        "verification_oracles": [{"kind": "official_reference_oracle", "name": "official SDK", "used": True}],
    })


def test_v022_scheduler_stages_explore_and_research_before_critic(tmp_path: Path):
    store = FakePolicyStore("Implement support for the current MCP protocol specification in an HTTP server.")
    # The initial coordinator must still be active for deterministic generation preflight.
    store.tasks[0]["status"] = TaskStatus.READY.value
    scheduler = Scheduler(store, GlobalConfig(home=tmp_path), max_workers=1)
    assert scheduler._enforce_planning_policy("COL-1") is True
    explores = [task for task in store.tasks if task["profile"] == "explore"]
    research = [task for task in store.tasks if task["profile"] == "research"]
    critics = [task for task in store.tasks if task["profile"] == "critic"]
    assert len(explores) == len(research) == len(critics) == 1
    assert store.dependencies(research[0]["id"]) == []
    critic_deps = {task["id"] for task in store.dependencies(critics[0]["id"])}
    assert explores[0]["id"] in critic_deps
    assert research[0]["id"] in critic_deps
    coord_deps = {task["id"] for task in store.dependencies("T-COORD")}
    assert {explores[0]["id"], research[0]["id"], critics[0]["id"]} <= coord_deps


def test_v022_external_protocol_revision_gets_fresh_research_preflight(tmp_path: Path):
    store = FakePolicyStore("Implement support for the current MCP protocol specification in an HTTP server.")
    store.tasks[0]["status"] = TaskStatus.COMPLETE.value
    store.tasks.append({
        "id": "T-REV-EXT", "project_id": "PRJ-1", "collection_id": "COL-1",
        "profile": "coordinator", "title": "Acceptance revision", "objective": "Revise protocol behavior",
        "reason": "operator revision", "status": TaskStatus.READY.value, "depth": 0,
        "created_at": "2026-11-12 12:00:00", "updated_at": "2026-11-12 12:00:00",
    })
    scheduler = Scheduler(store, GlobalConfig(home=tmp_path), max_workers=1)
    assert scheduler._enforce_planning_policy("COL-1") is True
    children = [task for task in store.tasks if task.get("requested_by_task_id") == "T-REV-EXT"]
    explore = next(task for task in children if task["profile"] == "explore")
    research = next(task for task in children if task["profile"] == "research")
    critic = next(task for task in children if task["profile"] == "critic")
    assert store.dependencies(research["id"]) == []
    critic_deps = {task["id"] for task in store.dependencies(critic["id"])}
    assert {explore["id"], research["id"]} <= critic_deps
    revision_deps = {task["id"] for task in store.dependencies("T-REV-EXT")}
    assert {explore["id"], research["id"], critic["id"]} <= revision_deps


def test_v022_advise_can_request_blocking_fresh_research(tmp_path: Path):
    store = FakePolicyStore("Analyze a local design choice.")
    advise = {
        "id": "T-ADVISE", "project_id": "PRJ-1", "collection_id": "COL-1",
        "profile": "advise", "title": "Advise", "objective": "Recommend",
        "reason": "decision", "status": TaskStatus.WAITING.value, "depth": 1,
        "created_at": "2026-11-12 12:00:00", "updated_at": "2026-11-12 12:00:00",
        "result": {"status": "waiting", "research_required": [{"question": "What does the current external standard require?"}]},
    }
    store.tasks.append(advise)
    scheduler = Scheduler(store, GlobalConfig(home=tmp_path), max_workers=1)
    assert scheduler._enforce_research_requests("COL-1") is True
    research = next(task for task in store.tasks if task["profile"] == "research" and task.get("requested_by_task_id") == "T-ADVISE")
    deps = {task["id"] for task in store.dependencies("T-ADVISE")}
    assert research["id"] in deps
    assert any(event[0] == "RESEARCH_REQUIRED" for event in store.events)


def test_v022_external_plan_review_depends_on_all_current_research(tmp_path: Path):
    store = FakePolicyStore("Implement support for the current MCP protocol specification in an HTTP server.")
    store.tasks[0]["status"] = TaskStatus.COMPLETE.value
    store.tasks[1]["status"] = TaskStatus.READY.value

    research_ids = []
    claim_ids = []
    for index, claim_id in enumerate(("claim-a", "claim-b"), start=1):
        result = _valid_fresh_research_result()
        result["claims"][0] = dict(result["claims"][0], id=claim_id, claim=f"Requirement {claim_id}", planner_directive=f"Preserve {claim_id}")
        task = {
            "id": f"T-RESEARCH-{index}", "project_id": "PRJ-1", "collection_id": "COL-1",
            "profile": "research", "title": f"Research {index}", "objective": "fresh evidence",
            "reason": "fresh external evidence", "status": TaskStatus.COMPLETE.value, "depth": 1,
            "created_at": f"2026-11-12 10:00:0{index}", "updated_at": f"2026-11-12 10:00:0{index}",
            "requested_by_task_id": "T-COORD", "result": result,
        }
        store.tasks.append(task)
        research_ids.append(task["id"])
        claim_ids.append(claim_id)

    critic = {
        "id": "T-CRITIC-EXT", "project_id": "PRJ-1", "collection_id": "COL-1",
        "profile": "critic", "title": "External critic", "objective": "challenge protocol plan",
        "reason": "policy", "status": TaskStatus.COMPLETE.value, "depth": 1,
        "created_at": "2026-11-12 10:00:10", "updated_at": "2026-11-12 10:00:10",
        "requested_by_task_id": "T-COORD",
        "result": {"status": "complete", "summary": "checked", "research_consistency": _research_consistency_for(
            [task for task in store.tasks if task["id"] in research_ids]
        )},
    }
    store.tasks.append(critic)
    for research_id in research_ids:
        store.add_dependency(critic["id"], research_id)

    scheduler = Scheduler(store, GlobalConfig(home=tmp_path), max_workers=1)
    assert scheduler._enforce_planning_policy("COL-1") is True

    plan_reviews = [task for task in store.tasks if task["profile"] == "plan_review" and task["status"] != TaskStatus.SUPERSEDED.value]
    assert len(plan_reviews) == 1
    plan_deps = {task["id"] for task in store.dependencies(plan_reviews[0]["id"])}
    assert set(research_ids) <= plan_deps
    assert critic["id"] in plan_deps
    impl_deps = {task["id"] for task in store.dependencies("T-IMPL")}
    assert plan_reviews[0]["id"] in impl_deps


def test_v022_duplicate_claim_ids_are_task_qualified():
    first = {"id": "T-R1", "profile": "research", "status": TaskStatus.COMPLETE.value, "result": _valid_fresh_research_result()}
    second = {"id": "T-R2", "profile": "research", "status": TaskStatus.COMPLETE.value, "result": _valid_fresh_research_result()}
    assert research_claim_ids([first, second]) == ["T-R1:claim-x", "T-R2:claim-x"]
    import pytest
    with pytest.raises(ValueError):
        validate_research_consumer_result(
            {"research_consistency": {"verdict": "pass", "checked_claim_ids": ["claim-x"], "contradictions": []}},
            [first, second], require_pass=True,
        )
    validate_research_consumer_result(
        {"research_consistency": _research_consistency_for([first, second])},
        [first, second], require_pass=True,
    )


def test_v022_research_exception_decision_id_extraction():
    result = _valid_fresh_research_result()
    result["claims"][0]["directive_effect"] = "exception"
    result["claims"][0]["exception_decision_id"] = "DEC-123"
    assert research_exception_decision_ids(result) == ["DEC-123"]


def test_v022_current_news_objective_gets_research_without_implementation_ceremony(tmp_path: Path):
    store = FakePolicyStore("Research current news and recent events affecting the vendor and summarize the implications.")
    store.tasks = [store.tasks[0]]
    store.tasks[0]["status"] = TaskStatus.READY.value
    scheduler = Scheduler(store, GlobalConfig(home=tmp_path), max_workers=1)
    assert scheduler._enforce_planning_policy("COL-1") is True
    research = [task for task in store.tasks if task["profile"] == "research"]
    assert len(research) == 1
    # Intentional widening: the recorded freshness/evolving risk categories now
    # enable the read-only explore preflight verb-independently. The heavy
    # critic/plan_review/implement ceremony must still be absent.
    assert not [task for task in store.tasks if task["profile"] in {"critic", "plan_review", "implement"}]
    coordinator_deps = {task["id"] for task in store.dependencies("T-COORD")}
    assert research[0]["id"] in coordinator_deps


def test_v024_rejects_changed_inherited_research_directive():
    import pytest

    research_result = _valid_fresh_research_result()
    research_result["claims"][0] = dict(
        research_result["claims"][0],
        id="C1",
        claim="initialize MUST include protocolVersion",
        planner_directive="Missing, non-string, or malformed protocolVersion MUST return Invalid Params.",
    )
    research = {
        "id": "T-R", "profile": "research", "status": TaskStatus.COMPLETE.value,
        "result": research_result,
    }
    consistency = _research_consistency_for([research])
    consistency["claims"][0]["inherited_directive"] = "Absent params may succeed with the default protocol version."
    with pytest.raises(ValueError):
        validate_research_consumer_result(
            {"research_consistency": consistency}, [research], require_pass=True
        )


def test_v024_active_generation_legacy_critic_is_revalidated(tmp_path: Path):
    store = FakePolicyStore("Implement support for the current MCP protocol specification in an HTTP server.")
    store.tasks = [
        {
            "id": "T-COORD", "project_id": "PRJ-1", "collection_id": "COL-1",
            "profile": "coordinator", "title": "Acceptance revision", "objective": "Revise MCP",
            "reason": "operator revision", "status": TaskStatus.READY.value, "depth": 0,
            "created_at": "2026-11-12 12:00:00", "updated_at": "2026-11-12 12:00:00",
        },
        {
            "id": "T-EXPLORE", "project_id": "PRJ-1", "collection_id": "COL-1",
            "profile": "explore", "title": "Explore current implementation surface", "objective": "map",
            "reason": "policy", "status": TaskStatus.COMPLETE.value, "depth": 1,
            "requested_by_task_id": "T-COORD", "parent_task_id": "T-COORD",
            "created_at": "2026-11-12 12:00:01", "updated_at": "2026-11-12 12:00:02",
            "result": {"status": "complete", "summary": "mapped"},
        },
        {
            "id": "T-RESEARCH", "project_id": "PRJ-1", "collection_id": "COL-1",
            "profile": "research", "title": "Fresh external research preflight", "objective": "research",
            "reason": "fresh external evidence", "status": TaskStatus.COMPLETE.value, "depth": 1,
            "requested_by_task_id": "T-COORD", "parent_task_id": "T-COORD",
            "created_at": "2026-11-12 12:00:01", "updated_at": "2026-11-12 12:00:03",
            "result": _valid_fresh_research_result(),
        },
        {
            "id": "T-CRITIC", "project_id": "PRJ-1", "collection_id": "COL-1",
            "profile": "critic", "title": "Preflight implementation plan", "objective": "critique",
            "reason": "policy", "status": TaskStatus.COMPLETE.value, "depth": 1,
            "requested_by_task_id": "T-COORD", "parent_task_id": "T-COORD",
            "created_at": "2026-11-12 12:00:04", "updated_at": "2026-11-12 12:00:05",
            "result": {
                "status": "complete", "summary": "legacy 0.2.3 critic",
                "research_consistency": {
                    "verdict": "pass", "checked_claim_ids": ["claim-x"], "contradictions": []
                },
            },
        },
    ]
    store.add_dependency("T-CRITIC", "T-EXPLORE")
    store.add_dependency("T-CRITIC", "T-RESEARCH")
    store.add_dependency("T-COORD", "T-EXPLORE")
    store.add_dependency("T-COORD", "T-RESEARCH")
    store.add_dependency("T-COORD", "T-CRITIC")

    scheduler = Scheduler(store, GlobalConfig(home=tmp_path), max_workers=1)
    assert scheduler._enforce_planning_policy("COL-1") is True

    assert store.get_task("T-CRITIC")["status"] == TaskStatus.SUPERSEDED.value
    replacements = [
        task for task in store.tasks
        if task["profile"] == "critic"
        and task["title"] == "Revalidate inherited research constraints"
        and task["status"] != TaskStatus.SUPERSEDED.value
    ]
    assert len(replacements) == 1
    replacement = replacements[0]
    replacement_deps = {task["id"] for task in store.dependencies(replacement["id"])}
    assert {"T-EXPLORE", "T-RESEARCH"} <= replacement_deps
    coord_deps = {task["id"] for task in store.dependencies("T-COORD")}
    assert replacement["id"] in coord_deps
    assert "T-CRITIC" not in coord_deps
    assert any(event[0] == "RESEARCH_CONSISTENCY_REVALIDATION_REQUIRED" for event in store.events)


def test_v025_running_collection_revalidates_legacy_critic_after_generation_coordinator_completed(tmp_path: Path):
    store = FakePolicyStore("Implement support for the current MCP protocol specification in an HTTP server.")
    store.collection["status"] = "RUNNING"
    store.tasks = [
        {
            "id": "T-COORD", "project_id": "PRJ-1", "collection_id": "COL-1",
            "profile": "coordinator", "title": "Acceptance revision", "objective": "Revise MCP",
            "reason": "operator revision", "status": TaskStatus.COMPLETE.value, "depth": 0,
            "created_at": "2026-11-12 12:00:00", "updated_at": "2026-11-12 12:00:10",
            "result": {"status": "complete", "summary": "old planning complete"},
        },
        {
            "id": "T-EXPLORE", "project_id": "PRJ-1", "collection_id": "COL-1",
            "profile": "explore", "title": "Explore current implementation surface", "objective": "map",
            "reason": "policy", "status": TaskStatus.COMPLETE.value, "depth": 1,
            "requested_by_task_id": "T-COORD", "parent_task_id": "T-COORD",
            "created_at": "2026-11-12 12:00:01", "updated_at": "2026-11-12 12:00:02",
            "result": {"status": "complete", "summary": "mapped"},
        },
        {
            "id": "T-RESEARCH", "project_id": "PRJ-1", "collection_id": "COL-1",
            "profile": "research", "title": "Fresh external research preflight", "objective": "research",
            "reason": "fresh external evidence", "status": TaskStatus.COMPLETE.value, "depth": 1,
            "requested_by_task_id": "T-COORD", "parent_task_id": "T-COORD",
            "created_at": "2026-11-12 12:00:01", "updated_at": "2026-11-12 12:00:03",
            "result": _valid_fresh_research_result(),
        },
        {
            "id": "T-CRITIC", "project_id": "PRJ-1", "collection_id": "COL-1",
            "profile": "critic", "title": "Preflight implementation plan", "objective": "critique",
            "reason": "policy", "status": TaskStatus.COMPLETE.value, "depth": 1,
            "requested_by_task_id": "T-COORD", "parent_task_id": "T-COORD",
            "created_at": "2026-11-12 12:00:04", "updated_at": "2026-11-12 12:00:05",
            "result": {
                "status": "complete", "summary": "legacy critic",
                "research_consistency": {"verdict": "pass", "checked_claim_ids": ["claim-x"], "contradictions": []},
            },
        },
    ]
    store.add_dependency("T-CRITIC", "T-EXPLORE")
    store.add_dependency("T-CRITIC", "T-RESEARCH")
    store.add_dependency("T-COORD", "T-EXPLORE")
    store.add_dependency("T-COORD", "T-RESEARCH")
    store.add_dependency("T-COORD", "T-CRITIC")

    scheduler = Scheduler(store, GlobalConfig(home=tmp_path), max_workers=1)
    assert scheduler._enforce_planning_policy("COL-1") is True
    assert store.get_task("T-CRITIC")["status"] == TaskStatus.SUPERSEDED.value
    replacements = [task for task in store.tasks if task["title"] == "Revalidate inherited research constraints" and task["status"] != TaskStatus.SUPERSEDED.value]
    repairs = [task for task in store.tasks if task["title"] == "Apply inherited research revalidation" and task["status"] != TaskStatus.SUPERSEDED.value]
    assert len(replacements) == 1
    assert len(repairs) == 1
    assert replacements[0]["id"] in {task["id"] for task in store.dependencies(repairs[0]["id"])}


def test_v024_historical_completed_legacy_consumer_is_not_reopened(tmp_path: Path):
    store = FakePolicyStore("Implement support for the current MCP protocol specification in an HTTP server.")
    store.collection["status"] = "COMPLETE"
    store.tasks = [
        {
            "id": "T-COORD", "project_id": "PRJ-1", "collection_id": "COL-1",
            "profile": "coordinator", "title": "Initial coordination", "objective": "Plan",
            "reason": "entry", "status": TaskStatus.COMPLETE.value, "depth": 0,
            "created_at": "2026-11-10 10:00:00", "updated_at": "2026-11-10 10:01:00",
        },
        {
            "id": "T-RESEARCH", "project_id": "PRJ-1", "collection_id": "COL-1",
            "profile": "research", "title": "Fresh external research preflight", "objective": "research",
            "reason": "fresh external evidence", "status": TaskStatus.COMPLETE.value, "depth": 1,
            "requested_by_task_id": "T-COORD", "parent_task_id": "T-COORD",
            "created_at": "2026-11-10 10:00:10", "updated_at": "2026-11-10 10:00:20",
            "result": _valid_fresh_research_result(),
        },
        {
            "id": "T-CRITIC", "project_id": "PRJ-1", "collection_id": "COL-1",
            "profile": "critic", "title": "Preflight implementation plan", "objective": "critique",
            "reason": "policy", "status": TaskStatus.COMPLETE.value, "depth": 1,
            "requested_by_task_id": "T-COORD", "parent_task_id": "T-COORD",
            "created_at": "2026-11-10 10:00:21", "updated_at": "2026-11-10 10:00:30",
            "result": {
                "status": "complete", "summary": "historical legacy critic",
                "research_consistency": {"verdict": "pass", "checked_claim_ids": ["claim-x"], "contradictions": []},
            },
        },
    ]
    store.add_dependency("T-CRITIC", "T-RESEARCH")
    scheduler = Scheduler(store, GlobalConfig(home=tmp_path), max_workers=1)
    scheduler._enforce_planning_policy("COL-1")
    assert store.get_task("T-CRITIC")["status"] == TaskStatus.COMPLETE.value
    assert not [task for task in store.tasks if task["title"] == "Revalidate inherited research constraints"]


# --- Planning-gate coherence regression oracles (mirror internal/praector) ---

PRE_CUTOFF = "2020-01-01T00:00:00.000000"
POST_CUTOFF = "2026-11-12T00:00:00.000000"


def _gates(p):
    return (p.explore_required, p.research_required, p.critic_required,
            p.plan_review_required, p.external_conformance_required, p.threat_model_required)


def test_gate_coherence_parity_vectors():
    # Identical table asserted in internal/praector/policy_gate_coherence_test.go;
    # scripts/parity_check.py never compares classifier verdicts, so this is the
    # machine-checked Go/Python parity for the coherent floor.
    table = {
        "Update the transport to comply with the MCP protocol specification.": (1, 1, 1, 1, 1, 0),
        "MCP protocol specification compliance for the transport.": (1, 1, 1, 1, 1, 0),
        "Fix the auth bypass in the token check.": (1, 0, 1, 1, 0, 1),
        "The auth bypass in the token check requires correction.": (1, 0, 1, 1, 0, 1),
        "Migrate the ledger to the new format.": (1, 0, 1, 1, 0, 0),
        "Migrating the ledger to the new format.": (1, 0, 1, 1, 0, 0),
        "Delete the stale records.": (1, 0, 1, 1, 0, 0),
        "Deletion of the stale records.": (1, 0, 1, 1, 0, 0),
        "Integrate the Twilio gateway.": (1, 1, 1, 1, 1, 0),
        "Twilio gateway integration.": (1, 1, 1, 1, 1, 0),
        "The vendor library needs assessment.": (1, 0, 0, 0, 0, 0),
        "Summarize current news and recent events.": (1, 1, 0, 0, 0, 0),
        "Assess GDPR compliance implications.": (1, 1, 0, 0, 0, 0),
        "Review the public API endpoint backward compatibility.": (1, 0, 1, 1, 0, 0),
        "The cache layer behavior under load.": (1, 0, 0, 1, 0, 0),
        "Harden the HTTP server listener.": (1, 0, 0, 1, 0, 1),
        "Update a typo in README.md.": (1, 0, 1, 0, 0, 0),
        "Fix documentation wording in the docs.": (1, 0, 1, 0, 0, 0),
        "A minor wording correction in the README.": (0, 0, 0, 0, 0, 0),
        "The removal of stale records.": (0, 0, 0, 0, 0, 0),
        "Add API-key authentication and per-key rate limiting to the public API endpoint.": (1, 0, 1, 1, 0, 1),
        "Fix a typo in README.md and update one example sentence.": (1, 0, 1, 0, 0, 0),
    }
    for obj, expected in table.items():
        got = tuple(int(x) for x in _gates(classify_planning_policy(obj)))
        assert got == expected, f"parity vector mismatch {obj!r}: got {got} want {expected}"


def test_gate_coherence_phrasing_matrix():
    rows = [
        ("external_protocol_or_standard", "Update the transport to comply with the MCP protocol specification.", "MCP protocol specification compliance for the transport.", "x", True, False),
        ("security", "Fix the auth bypass in the token check.", "The auth bypass in the token check requires correction.", "t", True, False),
        ("migration_or_persistence", "Migrate the ledger to the new format.", "Migrating the ledger to the new format.", "p", True, True),
        ("destructive_change", "Delete the stale records.", "Deletion of the stale records.", "c", True, True),
        ("external_integration", "Add Twilio gateway integration.", "Twilio gateway integration.", "x", True, False),
    ]
    pick = {"x": lambda p: p.external_conformance_required, "t": lambda p: p.threat_model_required,
            "p": lambda p: p.plan_review_required, "c": lambda p: p.critic_required}
    for category, verb, noun, g, verb_legacy, noun_legacy in rows:
        gate = pick[g]
        assert category in classify_planning_policy(noun).categories
        assert gate(classify_planning_policy(verb)) == gate(classify_planning_policy(noun))
        assert gate(classify_planning_policy(noun)) is True
        assert gate(_classify_planning_policy(verb, False)) is verb_legacy
        assert gate(_classify_planning_policy(noun, False)) is noun_legacy


def test_gate_coherence_all_twelve_categories():
    table = [
        ("implementation_change", "Add a helper function to the module.", lambda p: p.explore_required or p.critic_required),
        ("security", "Fix the authentication token check.", lambda p: p.critic_required),
        ("stateful_behavior", "The cache layer behavior under load.", lambda p: p.plan_review_required),
        ("migration_or_persistence", "Migrating the ledger to the new format.", lambda p: p.critic_required),
        ("destructive_change", "Deletion of the stale records.", lambda p: p.critic_required),
        ("external_contract", "Review the public API endpoint backward compatibility.", lambda p: p.critic_required),
        ("external_integration", "Twilio gateway integration.", lambda p: p.external_conformance_required),
        ("external_protocol_or_standard", "MCP protocol specification compliance for the transport.", lambda p: p.external_conformance_required),
        ("freshness_sensitive", "Summarize current news and recent events.", lambda p: p.research_required),
        ("law_regulation_or_compliance", "Assess GDPR compliance implications.", lambda p: p.research_required),
        ("network_trust_boundary", "Harden the HTTP server listener.", lambda p: p.threat_model_required),
    ]
    for category, obj, witness in table:
        p = classify_planning_policy(obj)
        assert category in p.categories, f"{category} not recorded for {obj!r}"
        assert witness(p), f"{category} recorded for {obj!r} but its gate is disabled: {_gates(p)}"
    evolving = classify_planning_policy("The vendor library needs assessment.")
    assert "evolving_dependency_or_vendor" in evolving.categories
    assert evolving.explore_required is True
    assert evolving.research_required is False


def test_gate_coherence_destructive_noun_known_gap():
    p = classify_planning_policy("The removal of stale records.")
    assert "destructive_change" not in p.categories and "migration_or_persistence" not in p.categories
    assert p.critic_required is False and p.plan_review_required is False and p.explore_required is False


def test_gate_coherence_dual_separator():
    for created in ("2020-01-01T10:00:00.000000", "2020-01-01 10:00:00.000000"):
        assert effective_planning_policy({"objective": "MCP protocol specification compliance", "created_at": created}) is not None
        assert gate_derivation_label({"created_at": created}) == "verb_legacy"
    for created in ("2026-11-12T00:00:01.000000", "2026-11-12 23:59:59.000000"):
        assert gate_derivation_label({"created_at": created}) == "category"
    assert gate_derivation_label({}) == "category"  # fail closed to coherent


def test_gate_coherence_forward_scope_effective_policy():
    for obj in ("MCP protocol specification compliance for the transport.",
                "Twilio gateway integration.",
                "Deletion of the stale records.",
                "The vendor library needs assessment."):
        coherent = classify_planning_policy(obj)
        legacy = _classify_planning_policy(obj, False)
        assert _gates(effective_planning_policy({"objective": obj, "created_at": POST_CUTOFF})) == _gates(coherent)
        assert _gates(effective_planning_policy({"objective": obj, "created_at": PRE_CUTOFF})) == _gates(legacy)
