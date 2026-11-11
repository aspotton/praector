from pathlib import Path

from praector.config import GlobalConfig
from praector.models import TaskStatus
from praector.planning_policy import classify_planning_policy
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
            "status": TaskStatus.READY.value,
            "depth": kwargs.get("depth", 0),
            "created_at": f"2026-11-10 10:02:0{self.counter}",
            "updated_at": f"2026-11-10 10:02:0{self.counter}",
        }
        self.tasks.append(task)
        for dep in kwargs.get("depends_on") or []:
            self.add_edge(dep, task["id"], task["collection_id"], "depends_on")
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
    assert policy.critic_required is True
    assert policy.plan_review_required is True
    assert "security" in policy.categories
    assert "stateful_behavior" in policy.categories


def test_simple_bounded_change_does_not_force_extra_planning_gates():
    policy = classify_planning_policy("Fix a typo in README.md and update one example sentence.")
    assert policy.critic_required is False
    assert policy.plan_review_required is False


def test_scheduler_inserts_critic_and_plan_review_before_write(tmp_path: Path):
    store = FakePolicyStore(
        "Add API-key authentication and per-key rate limiting to the existing /hello endpoint."
    )
    scheduler = Scheduler(store, GlobalConfig(home=tmp_path), max_workers=1)

    changed = scheduler._enforce_planning_policy("COL-1")
    assert changed is True

    critics = [task for task in store.tasks if task["profile"] == "critic"]
    plan_reviews = [task for task in store.tasks if task["profile"] == "plan_review"]
    assert len(critics) == 1
    assert len(plan_reviews) == 1

    critic = critics[0]
    plan_review = plan_reviews[0]
    plan_deps = {task["id"] for task in store.dependencies(plan_review["id"])}
    impl_deps = {task["id"] for task in store.dependencies("T-IMPL")}

    assert critic["id"] in plan_deps
    assert plan_review["id"] in impl_deps
    assert any(event[0] == "PLANNING_POLICY_ENFORCED" for event in store.events)

    # Reconciliation is idempotent: don't manufacture another gate set.
    changed_again = scheduler._enforce_planning_policy("COL-1")
    assert changed_again is False
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

    assert delivery.implementation_required is False
    assert planning.plan_review_required is False
    assert planning.critic_required is False


def test_conversational_nontrivial_code_change_requires_plan_review_without_forcing_critic():
    from praector.planning_policy import classify_delivery_policy

    objective = "The CSV parser is intentionally small. I'd like to add support for escaped commas while preserving existing parsing behavior."
    delivery = classify_delivery_policy(objective)
    planning = classify_planning_policy(objective)

    assert delivery.implementation_required is True
    assert planning.plan_review_required is True
    assert planning.critic_required is False
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
