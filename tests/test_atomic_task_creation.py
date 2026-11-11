from contextlib import contextmanager

from praector.db import DoltStore
from praector.models import TaskStatus


class AtomicCreateHarness:
    def __init__(self):
        self.queries = []
        self.commits = []
        self.events = []
        self.task_id = None
        self.task_collections = {
            "T-PARENT": "COL-1",
            "T-DEP": "COL-1",
            "T-CRITIC": "COL-1",
            "T-IMPL": "COL-1",
        }

    @contextmanager
    def lock(self):
        yield

    def _query_locked(self, query):
        # No dedupe hit, no existing critic baseline, no prior hard edges, and
        # one existing implementation when the plan-review gate asks for it.
        if query.startswith("SELECT id,collection_id FROM tasks WHERE id="):
            task_id = query.split("id=", 1)[1].split(" LIMIT", 1)[0].strip("'")
            collection_id = self.task_collections.get(task_id)
            return ([{"id": task_id, "collection_id": collection_id}]
                    if collection_id is not None else [])
        if query.startswith("SELECT * FROM tasks WHERE"):
            return []
        if query.startswith("SELECT 1 AS ok FROM events WHERE"):
            return []
        if query.startswith("SELECT from_task_id,to_task_id FROM task_edges"):
            return []
        if query.startswith("SELECT id FROM tasks WHERE") and "profile='implement'" in query:
            return [{"id": "T-IMPL"}]
        raise AssertionError(f"unexpected query: {query}")


    def _dependency_path_in_edges(self, edges, start_task_id, target_task_id):
        return DoltStore._dependency_path_in_edges(edges, start_task_id, target_task_id)

    def _dependency_edges_locked(self, collection_id):
        return DoltStore._dependency_edges_locked(self, collection_id)

    def _assert_dependency_tasks_in_collection_locked(self, collection_id, task_ids):
        return DoltStore._assert_dependency_tasks_in_collection_locked(self, collection_id, task_ids)

    def _assert_dependency_tasks_in_collection_locked(self, collection_id, task_ids):
        return DoltStore._assert_dependency_tasks_in_collection_locked(self, collection_id, task_ids)

    def _assert_dependency_edges_acyclic_locked(self, collection_id, prospective_edges):
        return DoltStore._assert_dependency_edges_acyclic_locked(self, collection_id, prospective_edges)

    def _mutate_locked(self, query):
        self.queries.append(query)
        marker = "INSERT INTO tasks (id,"
        assert marker in query
        # Extracting the generated ID is unnecessary; get_task can return a stub.

    def _semantic_commit_locked(self, message):
        self.commits.append(message)

    def event(self, project_id, collection_id, task_id, attempt_id, event_type, payload):
        self.events.append((event_type, payload))

    def get_task(self, task_id):
        return {"id": task_id, "status": TaskStatus.READY.value, "result": {}}


def test_create_task_publishes_row_dependencies_and_blocking_edge_in_one_mutation():
    store = AtomicCreateHarness()

    task, created = DoltStore.create_task(
        store,
        project_id="PRJ-1",
        collection_id="COL-1",
        profile="critic",
        title="Critique",
        objective="Inspect plan",
        reason="test",
        parent_task_id="T-PARENT",
        requested_by_task_id="T-PARENT",
        depends_on=["T-DEP"],
        workspace_mode="collection",
        depth=1,
        spawned_by_task_id="T-PARENT",
        blocking_caller_task_id="T-PARENT",
        allow_dedupe=False,
    )

    assert created is True
    assert task["status"] == TaskStatus.READY.value
    assert len(store.queries) == 1
    sql = store.queries[0]
    assert "INSERT INTO tasks" in sql
    assert "'T-DEP'" in sql and "'depends_on'" in sql
    assert "'T-PARENT'" in sql and "'spawned_by'" in sql
    assert "UPDATE tasks SET status='WAITING'" in sql
    assert len(store.commits) == 1


def test_create_task_rejects_missing_dependency_before_publishing_row():
    import pytest

    store = AtomicCreateHarness()

    with pytest.raises(ValueError, match="Dependency task not found: T-MISSING"):
        DoltStore.create_task(
            store,
            project_id="PRJ-1",
            collection_id="COL-1",
            profile="implement",
            title="Invalid dependency",
            objective="Do not publish this task",
            reason="test",
            parent_task_id="T-PARENT",
            requested_by_task_id="T-PARENT",
            depends_on=["T-MISSING"],
            workspace_mode="collection",
            depth=1,
            allow_dedupe=False,
        )

    assert store.queries == []
    assert store.commits == []


def test_create_plan_review_can_gate_existing_implement_tasks_in_same_mutation():
    store = AtomicCreateHarness()

    DoltStore.create_task(
        store,
        project_id="PRJ-1",
        collection_id="COL-1",
        profile="plan_review",
        title="Review graph",
        objective="Review implementation graph",
        reason="test",
        parent_task_id="T-PARENT",
        requested_by_task_id="T-PARENT",
        depends_on=["T-CRITIC"],
        workspace_mode="collection",
        depth=1,
        spawned_by_task_id="T-PARENT",
        blocking_caller_task_id="T-PARENT",
        gate_implement_tasks=True,
        critic_baseline_task_id="T-CRITIC",
        critic_baseline_fingerprint="fp-sealed",
        critic_baseline_source="revision_plan_seal",
        allow_dedupe=False,
    )

    assert len(store.queries) == 1
    sql = store.queries[0]
    assert "SELECT" in sql
    assert "t.profile='implement'" in sql
    assert "AND t.status NOT IN ('CANCELLED','SUPERSEDED','COMPLETE')" in sql
    assert "'depends_on'" in sql
    assert "'CRITIC_PLAN_BASELINE'" in sql
    assert "revision_plan_seal" in sql
    assert "fp-sealed" in sql


class RequestHarness:
    def __init__(self, *, revision=False):
        self.revision = revision
        self.created_kwargs = None
        self.events = []
        self.baselines = {}
        self.tasks = [
            {
                "id": "T-CALLER",
                "project_id": "PRJ-1",
                "collection_id": "COL-1",
                "profile": "coordinator",
                "status": TaskStatus.RUNNING.value,
                "depth": 0,
            },
            {
                "id": "T-CRITIC",
                "project_id": "PRJ-1",
                "collection_id": "COL-1",
                "profile": "critic",
                "status": TaskStatus.COMPLETE.value,
                "depth": 1,
            },
            {
                "id": "T-IMPL",
                "project_id": "PRJ-1",
                "collection_id": "COL-1",
                "profile": "implement",
                "status": TaskStatus.READY.value,
                "depth": 1,
            },
            {
                "id": "T-RESEARCH",
                "project_id": "PRJ-1",
                "collection_id": "COL-1",
                "profile": "research",
                "status": TaskStatus.COMPLETE.value,
                "depth": 1,
            },
        ]

    def get_task(self, task_id):
        return next(dict(t) for t in self.tasks if t["id"] == task_id)

    def count_tasks(self, collection_id):
        return len(self.tasks)

    def child_count(self, task_id):
        return 0

    def list_tasks(self, collection_id):
        return [dict(t) for t in self.tasks]

    def critic_plan_baseline(self, critic_task_id):
        return self.baselines.get(critic_task_id)

    def list_collection_revisions(self, collection_id, status=None):
        if self.revision and status == "OPEN":
            return [{"id": "REV-1", "coordinator_task_id": "T-CALLER", "status": "OPEN"}]
        return []

    def material_plan_fingerprint(self, collection_id):
        return "fp-initial"

    def record_critic_plan_baseline(self, critic_task_id, fingerprint, *, source, plan_review_task_id=None):
        self.baselines[critic_task_id] = {
            "fingerprint": fingerprint,
            "source": source,
            "plan_review_task_id": plan_review_task_id,
        }

    def create_task(self, **kwargs):
        self.created_kwargs = kwargs
        critic_id = kwargs.get("critic_baseline_task_id")
        if critic_id is not None:
            self.baselines[critic_id] = {
                "fingerprint": kwargs["critic_baseline_fingerprint"],
                "source": kwargs["critic_baseline_source"],
                "plan_review_task_id": "T-PLAN",
            }
        return {
            "id": "T-PLAN",
            "project_id": kwargs["project_id"],
            "collection_id": kwargs["collection_id"],
            "profile": kwargs["profile"],
            "status": TaskStatus.READY.value,
        }, True

    def event(self, project_id, collection_id, task_id, attempt_id, event_type, payload):
        self.events.append((event_type, payload))


def test_coordinator_plan_review_request_is_forced_blocking_and_inherits_latest_critic():
    store = RequestHarness()

    task, created = DoltStore.request_task(
        store,
        "T-CALLER",
        profile="plan_review",
        title="Review graph",
        objective="Check plan",
        reason="gate",
        blocking=False,
        depends_on=[],
        workspace_mode="collection",
        base_commit=None,
        limits={"max_tasks_per_collection": 100, "max_task_depth": 8, "max_children_per_task": 10},
    )

    assert created is True
    assert task["id"] == "T-PLAN"
    kwargs = store.created_kwargs
    assert kwargs["depends_on"] == ["T-CRITIC"]
    assert kwargs["spawned_by_task_id"] == "T-CALLER"
    assert kwargs["blocking_caller_task_id"] == "T-CALLER"
    assert kwargs["gate_implement_tasks"] is True
    assert kwargs["critic_baseline_task_id"] == "T-CRITIC"
    assert kwargs["critic_baseline_fingerprint"] == "fp-initial"
    assert kwargs["critic_baseline_source"] == "initial_plan_seal"
    assert store.baselines["T-CRITIC"]["fingerprint"] == "fp-initial"
    assert store.baselines["T-CRITIC"]["source"] == "initial_plan_seal"
    assert store.baselines["T-CRITIC"]["plan_review_task_id"] == "T-PLAN"
    assert store.events[-1][1]["blocking"] is True


def test_coordinator_plan_review_strips_implement_dependency_but_keeps_prerequisite_evidence():
    store = RequestHarness()

    task, created = DoltStore.request_task(
        store,
        "T-CALLER",
        profile="plan_review",
        title="Review graph",
        objective="Check the implementation contract",
        reason="gate",
        blocking=False,
        depends_on=["T-IMPL", "T-RESEARCH"],
        workspace_mode="collection",
        base_commit=None,
        limits={"max_tasks_per_collection": 100, "max_task_depth": 8, "max_children_per_task": 10},
    )

    assert created is True
    assert task["id"] == "T-PLAN"
    # The reviewer may wait for evidence, but not for the implementation it gates.
    assert store.created_kwargs["depends_on"] == ["T-RESEARCH", "T-CRITIC"]
    assert store.created_kwargs["gate_implement_tasks"] is True
    ignored = [event for event in store.events if event[0] == "PLAN_REVIEW_IMPLEMENT_DEPENDENCIES_IGNORED"]
    assert len(ignored) == 1
    assert ignored[0][1]["implement_task_ids"] == ["T-IMPL"]


def test_revision_coordinator_plan_review_uses_revision_plan_seal_provenance():
    store = RequestHarness(revision=True)

    task, created = DoltStore.request_task(
        store,
        "T-CALLER",
        profile="plan_review",
        title="Review revision graph",
        objective="Check revised plan",
        reason="revision gate",
        blocking=False,
        depends_on=[],
        workspace_mode="collection",
        base_commit=None,
        limits={"max_tasks_per_collection": 100, "max_task_depth": 8, "max_children_per_task": 10},
    )

    assert created is True
    assert task["id"] == "T-PLAN"
    assert store.created_kwargs["critic_baseline_source"] == "revision_plan_seal"
    assert store.baselines["T-CRITIC"]["source"] == "revision_plan_seal"


def test_create_task_rejects_atomic_dependency_cycle_before_publishing_row():
    import pytest

    store = AtomicCreateHarness()

    with pytest.raises(ValueError, match="would create a cycle"):
        DoltStore.create_task(
            store,
            project_id="PRJ-1",
            collection_id="COL-1",
            profile="plan_review",
            title="Invalid blocking review",
            objective="Review graph",
            reason="test",
            parent_task_id="T-PARENT",
            requested_by_task_id="T-PARENT",
            depends_on=["T-PARENT"],
            workspace_mode="collection",
            depth=1,
            blocking_caller_task_id="T-PARENT",
            allow_dedupe=False,
        )

    assert store.queries == []
    assert store.commits == []


class ExistingReviewRequestHarness(RequestHarness):
    def __init__(self):
        super().__init__()
        self.attach_kwargs = None
        self.tasks.append({
            "id": "T-EXISTING-PLAN",
            "project_id": "PRJ-1",
            "collection_id": "COL-1",
            "profile": "plan_review",
            "status": TaskStatus.READY.value,
            "depth": 1,
            "result": {},
        })

    def _attach_existing_plan_review_handoff(self, **kwargs):
        self.attach_kwargs = kwargs
        return self.get_task(kwargs["plan_review_task_id"])


def test_coordinator_reuses_existing_nonfailed_plan_review_single_flight():
    store = ExistingReviewRequestHarness()

    task, created = DoltStore.request_task(
        store,
        "T-CALLER",
        profile="plan_review",
        title="Another final review title",
        objective="Review the newest durable graph",
        reason="revalidation handoff",
        blocking=False,
        depends_on=["T-RESEARCH"],
        workspace_mode="collection",
        base_commit=None,
        limits={"max_tasks_per_collection": 100, "max_task_depth": 8, "max_children_per_task": 10},
    )

    assert created is False
    assert task["id"] == "T-EXISTING-PLAN"
    assert store.created_kwargs is None
    assert store.attach_kwargs["plan_review_task_id"] == "T-EXISTING-PLAN"
    assert store.attach_kwargs["caller_task_id"] == "T-CALLER"
    assert store.attach_kwargs["depends_on"] == ["T-RESEARCH", "T-CRITIC"]
    assert any(event[0] == "PLAN_REVIEW_REUSED" for event in store.events)


def test_plan_review_request_drops_caller_as_explicit_prerequisite_before_blocking_handoff():
    store = ExistingReviewRequestHarness()

    task, created = DoltStore.request_task(
        store,
        "T-CALLER",
        profile="plan_review",
        title="Final review",
        objective="Review graph",
        reason="handoff",
        blocking=True,
        depends_on=["T-CALLER", "T-RESEARCH"],
        workspace_mode="collection",
        base_commit=None,
        limits={"max_tasks_per_collection": 100, "max_task_depth": 8, "max_children_per_task": 10},
    )

    assert created is False
    assert task["id"] == "T-EXISTING-PLAN"
    assert "T-CALLER" not in store.attach_kwargs["depends_on"]
    ignored = [event for event in store.events if event[0] == "PLAN_REVIEW_CALLER_DEPENDENCY_IGNORED"]
    assert len(ignored) == 1


class ExistingPlanReviewAttachHarness:
    def __init__(self):
        self.queries = []
        self.commits = []
        self.tasks = {
            "T-PLAN": {
                "id": "T-PLAN", "project_id": "PRJ-1", "collection_id": "COL-1",
                "profile": "plan_review", "status": TaskStatus.READY.value,
            },
            "T-CALLER": {
                "id": "T-CALLER", "project_id": "PRJ-1", "collection_id": "COL-1",
                "profile": "coordinator", "status": TaskStatus.RUNNING.value,
            },
            "T-CRITIC": {
                "id": "T-CRITIC", "project_id": "PRJ-1", "collection_id": "COL-1",
                "profile": "critic", "status": TaskStatus.COMPLETE.value,
            },
            "T-RESEARCH": {
                "id": "T-RESEARCH", "project_id": "PRJ-1", "collection_id": "COL-1",
                "profile": "research", "status": TaskStatus.COMPLETE.value,
            },
            "T-IMPL": {
                "id": "T-IMPL", "project_id": "PRJ-1", "collection_id": "COL-1",
                "profile": "implement", "status": TaskStatus.READY.value,
            },
        }
        self.edges = [("T-CRITIC", "T-PLAN")]

    def get_task(self, task_id):
        task = dict(self.tasks[task_id])
        task.setdefault("result", {})
        return task

    @contextmanager
    def lock(self):
        yield

    def _query_locked(self, query):
        if query.startswith("SELECT id,collection_id FROM tasks WHERE id="):
            task_id = query.split("id=", 1)[1].split(" LIMIT", 1)[0].strip("'")
            task = self.tasks.get(task_id)
            return ([{"id": task_id, "collection_id": task["collection_id"]}]
                    if task is not None else [])
        if query.startswith("SELECT id FROM tasks WHERE") and "profile='implement'" in query:
            return [{"id": "T-IMPL"}]
        if query.startswith("SELECT from_task_id,to_task_id FROM task_edges"):
            return [
                {"from_task_id": from_id, "to_task_id": to_id}
                for from_id, to_id in self.edges
            ]
        if query.startswith("SELECT 1 AS ok FROM events WHERE"):
            return []
        raise AssertionError(query)

    def _dependency_edges_locked(self, collection_id):
        return DoltStore._dependency_edges_locked(self, collection_id)

    def _dependency_path_in_edges(self, edges, start_task_id, target_task_id):
        return DoltStore._dependency_path_in_edges(edges, start_task_id, target_task_id)

    def _assert_dependency_tasks_in_collection_locked(self, collection_id, task_ids):
        return DoltStore._assert_dependency_tasks_in_collection_locked(self, collection_id, task_ids)

    def _assert_dependency_edges_acyclic_locked(self, collection_id, prospective_edges):
        return DoltStore._assert_dependency_edges_acyclic_locked(self, collection_id, prospective_edges)

    def _mutate_locked(self, query):
        self.queries.append(query)

    def _semantic_commit_locked(self, message):
        self.commits.append(message)


def test_reuse_plan_review_handoff_is_atomic_and_gates_caller_and_implementation():
    store = ExistingPlanReviewAttachHarness()

    task = DoltStore._attach_existing_plan_review_handoff(
        store,
        plan_review_task_id="T-PLAN",
        caller_task_id="T-CALLER",
        depends_on=["T-RESEARCH", "T-CRITIC"],
        critic_baseline_task_id="T-CRITIC",
        critic_baseline_fingerprint="fp-current",
        critic_baseline_source="material_revalidation",
    )

    assert task["id"] == "T-PLAN"
    assert len(store.queries) == 1
    sql = store.queries[0]
    assert "'T-RESEARCH','T-PLAN'" in sql
    assert "'T-PLAN','T-CALLER'" in sql
    assert "UPDATE tasks SET status='WAITING'" in sql
    assert "t.profile='implement'" in sql
    assert "'CRITIC_PLAN_BASELINE'" in sql
    assert "material_revalidation" in sql
    assert len(store.commits) == 1


def test_reuse_plan_review_handoff_atomically_flips_existing_caller_prerequisite():
    store = ExistingPlanReviewAttachHarness()
    # Scheduler strengthening may have made the active coordinator a prerequisite
    # of the review before the coordinator explicitly performs final handoff.
    store.edges.append(("T-CALLER", "T-PLAN"))

    task = DoltStore._attach_existing_plan_review_handoff(
        store,
        plan_review_task_id="T-PLAN",
        caller_task_id="T-CALLER",
        depends_on=["T-CRITIC"],
    )

    assert task["id"] == "T-PLAN"
    assert len(store.queries) == 1
    sql = store.queries[0]
    assert "DELETE FROM task_edges" in sql
    assert "from_task_id='T-CALLER'" in sql
    assert "to_task_id='T-PLAN'" in sql
    assert "'T-PLAN','T-CALLER'" in sql
    assert "UPDATE tasks SET status='WAITING'" in sql
    assert len(store.commits) == 1
