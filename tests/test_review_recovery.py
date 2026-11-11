from types import SimpleNamespace

from praector.db import DoltStore
from praector.scheduler import Scheduler


class ProtocolStore:
    def __init__(self):
        self.calls = []

    def set_task_status(self, task_id, status, message=None):
        self.calls.append(("status", task_id, status, message))

    def event(self, *args):
        self.calls.append(("event",) + args)

    def submit_result(self, task_id, result):
        self.calls.append(("result", task_id, result))


def _scheduler_with_store(store):
    scheduler = object.__new__(Scheduler)
    scheduler.store = store
    return scheduler


def test_missing_review_verdict_retries_same_task_before_coordinator_repair():
    store = ProtocolStore()
    scheduler = _scheduler_with_store(store)
    cfg = SimpleNamespace(limits=SimpleNamespace(max_attempts_per_task=3))
    task = {"id": "T-R", "project_id": "PRJ-1", "collection_id": "COL-1"}
    attempt = {"id": "A-1", "attempt_no": 1}

    message = scheduler._handle_missing_review_verdict(cfg, task, attempt)

    assert "without submitting" in message
    assert any(call[:3] == ("status", "T-R", "READY") for call in store.calls)
    assert any(call[0] == "event" and call[-2] == "REVIEW_PROTOCOL_RETRY" for call in store.calls)
    assert not any(call[0] == "result" for call in store.calls)


def test_missing_review_verdict_fails_after_retry_budget_exhausted():
    store = ProtocolStore()
    scheduler = _scheduler_with_store(store)
    cfg = SimpleNamespace(limits=SimpleNamespace(max_attempts_per_task=3))
    task = {"id": "T-R", "project_id": "PRJ-1", "collection_id": "COL-1"}
    attempt = {"id": "A-3", "attempt_no": 3}

    scheduler._handle_missing_review_verdict(cfg, task, attempt)

    result_calls = [call for call in store.calls if call[0] == "result"]
    assert len(result_calls) == 1
    assert result_calls[0][2]["status"] == "failed"
    assert "retry budget exhausted" in result_calls[0][2]["risks"][0]


def test_superseding_review_preserves_prerequisite_and_verifies_lineage(monkeypatch):
    store = object.__new__(DoltStore)
    old = {"id": "T-R1", "project_id": "PRJ-1", "collection_id": "COL-1", "profile": "review"}
    new = {"id": "T-R2", "project_id": "PRJ-1", "collection_id": "COL-1", "profile": "review"}
    edges_added = []
    mutations = []

    monkeypatch.setattr(store, "get_task", lambda task_id: old if task_id == "T-R1" else new)

    def query(sql):
        if "from_task_id='T-R1'" in sql and "relation='depends_on'" in sql:
            return []
        if "to_task_id='T-R1'" in sql and "relation='depends_on'" in sql:
            return [{"from_task_id": "T-I"}]
        if "to_task_id='T-R1'" in sql and "relation='verifies'" in sql:
            return [{"from_task_id": "T-I"}]
        return []

    monkeypatch.setattr(store, "query", query)
    monkeypatch.setattr(store, "add_edge", lambda f, t, c, r="depends_on": edges_added.append((f, t, c, r)))
    monkeypatch.setattr(store, "mutate", lambda sql, message=None: mutations.append((sql, message)))
    monkeypatch.setattr(store, "event", lambda *args: None)

    store.supersede_task("T-R1", "T-R2", "replace protocol-broken review")

    assert ("T-I", "T-R2", "COL-1", "depends_on") in edges_added
    assert ("T-I", "T-R2", "COL-1", "verifies") in edges_added
    assert ("T-R1", "T-R2", "COL-1", "superseded_by") in edges_added
    assert mutations


def test_completed_implementation_requires_passing_review_for_terminal_complete(monkeypatch):
    store = object.__new__(DoltStore)
    tasks = [
        {"id": "T-I", "profile": "implement", "status": "COMPLETE", "parent_task_id": None, "result": {}},
        {"id": "T-R1", "profile": "review", "status": "SUPERSEDED", "parent_task_id": "T-I", "result": {"status": "failed"}},
        {"id": "T-R2", "profile": "review", "status": "COMPLETE", "parent_task_id": None, "result": {"verdict": "pass"}},
    ]
    edges = [{"from_task_id": "T-I", "to_task_id": "T-R1", "relation": "verifies"}]
    monkeypatch.setattr(store, "list_tasks", lambda collection_id: tasks)
    monkeypatch.setattr(store, "list_edges", lambda collection_id: edges)

    assert store.implementations_missing_passing_review("COL-1") == ["T-I"]

    edges.append({"from_task_id": "T-I", "to_task_id": "T-R2", "relation": "verifies"})
    assert store.implementations_missing_passing_review("COL-1") == []


def test_reconcile_repairs_historical_superseded_review_lineage(monkeypatch):
    store = object.__new__(DoltStore)
    tasks = [
        {"id": "T-I", "project_id": "PRJ-1", "collection_id": "COL-1", "profile": "implement", "status": "COMPLETE"},
        {"id": "T-R1", "project_id": "PRJ-1", "collection_id": "COL-1", "profile": "review", "status": "SUPERSEDED"},
        {"id": "T-R2", "project_id": "PRJ-1", "collection_id": "COL-1", "profile": "review", "status": "COMPLETE", "result": {"verdict": "pass"}},
    ]
    edges = [
        {"from_task_id": "T-I", "to_task_id": "T-R1", "relation": "depends_on"},
        {"from_task_id": "T-I", "to_task_id": "T-R1", "relation": "verifies"},
        {"from_task_id": "T-R1", "to_task_id": "T-R2", "relation": "superseded_by"},
    ]
    added = []
    monkeypatch.setattr(store, "list_tasks", lambda collection_id: tasks)
    monkeypatch.setattr(store, "list_edges", lambda collection_id: edges)
    monkeypatch.setattr(store, "get_collection", lambda collection_id: {"id": collection_id, "project_id": "PRJ-1"})
    monkeypatch.setattr(store, "add_edge", lambda f, t, c, r="depends_on": added.append((f, t, c, r)))
    monkeypatch.setattr(store, "event", lambda *args: None)

    repaired = store.repair_superseded_review_lineage("COL-1")

    assert repaired == 2
    assert ("T-I", "T-R2", "COL-1", "depends_on") in added
    assert ("T-I", "T-R2", "COL-1", "verifies") in added


def test_reconcile_review_lineage_propagates_through_supersession_chain(monkeypatch):
    store = object.__new__(DoltStore)
    tasks = [
        {"id": "T-I", "project_id": "PRJ-1", "collection_id": "COL-1", "profile": "implement", "status": "COMPLETE"},
        {"id": "T-R1", "project_id": "PRJ-1", "collection_id": "COL-1", "profile": "review", "status": "SUPERSEDED"},
        {"id": "T-R2", "project_id": "PRJ-1", "collection_id": "COL-1", "profile": "review", "status": "SUPERSEDED"},
        {"id": "T-R3", "project_id": "PRJ-1", "collection_id": "COL-1", "profile": "review", "status": "COMPLETE", "result": {"verdict": "pass"}},
    ]
    edges = [
        {"from_task_id": "T-I", "to_task_id": "T-R1", "relation": "depends_on"},
        {"from_task_id": "T-I", "to_task_id": "T-R1", "relation": "verifies"},
        {"from_task_id": "T-R1", "to_task_id": "T-R2", "relation": "superseded_by"},
        {"from_task_id": "T-R2", "to_task_id": "T-R3", "relation": "superseded_by"},
    ]
    added = []
    monkeypatch.setattr(store, "list_tasks", lambda collection_id: tasks)
    monkeypatch.setattr(store, "list_edges", lambda collection_id: edges)
    monkeypatch.setattr(store, "get_collection", lambda collection_id: {"id": collection_id, "project_id": "PRJ-1"})
    monkeypatch.setattr(store, "add_edge", lambda f, t, c, r="depends_on": added.append((f, t, c, r)))
    monkeypatch.setattr(store, "event", lambda *args: None)

    store.repair_superseded_review_lineage("COL-1")

    assert ("T-I", "T-R3", "COL-1", "depends_on") in added
    assert ("T-I", "T-R3", "COL-1", "verifies") in added


def test_terminal_state_blocks_direct_change_collection_with_no_completed_implementation(monkeypatch):
    store = object.__new__(DoltStore)
    tasks = [
        {"id": "T-C", "profile": "coordinator", "status": "COMPLETE", "result": {}},
        {"id": "T-K", "profile": "critic", "status": "COMPLETE", "result": {}},
    ]
    collection = {
        "id": "COL-1",
        "project_id": "PRJ-1",
        "objective": "Add an authenticated GET /usage endpoint.",
        "status": "RUNNING",
    }
    states = []
    monkeypatch.setattr(store, "repair_superseded_review_lineage", lambda collection_id: 0)
    monkeypatch.setattr(store, "reconcile_collection_revisions", lambda collection_id: 0)
    monkeypatch.setattr(store, "list_tasks", lambda collection_id: tasks)
    monkeypatch.setattr(store, "list_collection_revisions", lambda collection_id, status=None: [])
    monkeypatch.setattr(store, "implementations_missing_passing_review", lambda collection_id: [])
    monkeypatch.setattr(store, "critic_revalidation_state", lambda collection_id: {"stale": False})
    monkeypatch.setattr(store, "runnable_tasks", lambda collection_id, limit=1: [])
    monkeypatch.setattr(store, "has_running_session", lambda collection_id: False)
    monkeypatch.setattr(store, "get_collection", lambda collection_id: dict(collection))
    monkeypatch.setattr(
        store,
        "set_collection_status",
        lambda collection_id, status, commit=True: states.append(status),
    )

    assert store.update_collection_terminal_state("COL-1") == "BLOCKED"
    assert states == ["BLOCKED"]


def test_terminal_state_blocks_complete_collection_with_stale_required_critic(monkeypatch):
    store = object.__new__(DoltStore)
    tasks = [
        {"id": "T-K", "profile": "critic", "status": "COMPLETE", "result": {}},
        {"id": "T-P", "profile": "plan_review", "status": "COMPLETE", "result": {"verdict": "pass"}},
        {"id": "T-I", "profile": "implement", "status": "COMPLETE", "result": {}},
        {"id": "T-R", "profile": "review", "status": "COMPLETE", "result": {"verdict": "pass"}},
    ]
    collection = {
        "id": "COL-1",
        "project_id": "PRJ-1",
        "objective": "Add API-key authentication and per-key rate limiting",
        "status": "COMPLETE",
    }
    states = []
    monkeypatch.setattr(store, "repair_superseded_review_lineage", lambda collection_id: 0)
    monkeypatch.setattr(store, "reconcile_collection_revisions", lambda collection_id: 0)
    monkeypatch.setattr(store, "list_tasks", lambda collection_id: tasks)
    monkeypatch.setattr(store, "list_collection_revisions", lambda collection_id, status=None: [])
    monkeypatch.setattr(store, "implementations_missing_passing_review", lambda collection_id: [])
    monkeypatch.setattr(store, "critic_revalidation_state", lambda collection_id: {"stale": True})
    monkeypatch.setattr(store, "runnable_tasks", lambda collection_id, limit=1: [])
    monkeypatch.setattr(store, "has_running_session", lambda collection_id: False)
    monkeypatch.setattr(store, "get_collection", lambda collection_id: dict(collection))
    monkeypatch.setattr(
        store, "set_collection_status",
        lambda collection_id, status, commit=True: states.append(status),
    )

    assert store.update_collection_terminal_state("COL-1") == "BLOCKED"
    assert states == ["BLOCKED"]


def test_read_only_capability_violation_retries_same_task_before_coordinator_repair():
    store = ProtocolStore()
    scheduler = _scheduler_with_store(store)
    cfg = SimpleNamespace(limits=SimpleNamespace(max_attempts_per_task=3))
    task = {"id": "T-R", "project_id": "PRJ-1", "collection_id": "COL-1"}
    attempt = {"id": "A-1", "attempt_no": 1}

    message = scheduler._handle_read_only_capability_violation(cfg, task, attempt)

    assert "Read-only task mutated" in message
    assert any(call[:3] == ("status", "T-R", "READY") for call in store.calls)
    assert any(call[0] == "event" and call[-2] == "READ_ONLY_CAPABILITY_RETRY" for call in store.calls)
    assert not any(call[0] == "result" for call in store.calls)


def test_read_only_capability_violation_fails_after_retry_budget_exhausted():
    store = ProtocolStore()
    scheduler = _scheduler_with_store(store)
    cfg = SimpleNamespace(limits=SimpleNamespace(max_attempts_per_task=3))
    task = {"id": "T-R", "project_id": "PRJ-1", "collection_id": "COL-1"}
    attempt = {"id": "A-3", "attempt_no": 3}

    scheduler._handle_read_only_capability_violation(cfg, task, attempt)

    result_calls = [call for call in store.calls if call[0] == "result"]
    assert len(result_calls) == 1
    assert result_calls[0][2]["status"] == "failed"
    assert "retry budget exhausted" in result_calls[0][2]["risks"][0]


class SemanticRepairStore:
    def __init__(self):
        self.calls = []
        self.tasks = {
            "T-R1": {
                "id": "T-R1", "project_id": "PRJ-1", "collection_id": "COL-1",
                "profile": "review", "status": "VERIFYING",
                "result": {"status": "complete", "verdict": "fail", "summary": "auth defect"},
            },
            "T-I2": {
                "id": "T-I2", "project_id": "PRJ-1", "collection_id": "COL-1",
                "profile": "implement", "status": "COMPLETE", "title": "Repair auth defect",
                "parent_task_id": "T-R1", "depth": 3, "workspace_mode": "collection",
            },
        }
        self.edges = [
            {"from_task_id": "T-I1", "to_task_id": "T-R1", "collection_id": "COL-1", "relation": "verifies"},
        ]

    def get_task(self, task_id):
        return dict(self.tasks[task_id])

    def request_task(self, caller_task_id, **kwargs):
        self.calls.append(("request_task", caller_task_id, kwargs))
        repair = {"id": "T-I2", "project_id": "PRJ-1", "collection_id": "COL-1", "profile": "implement"}
        return repair, True

    def event(self, *args):
        self.calls.append(("event",) + args)

    def find_related_review(self, task_id):
        return None

    def list_edges(self, collection_id):
        return list(self.edges)

    def create_task(self, **kwargs):
        self.calls.append(("create_task", kwargs))
        review = {
            "id": "T-R2", "project_id": kwargs["project_id"], "collection_id": kwargs["collection_id"],
            "profile": "review", "status": "READY", "title": kwargs["title"], "objective": kwargs["objective"],
        }
        self.tasks["T-R2"] = review
        return review, True

    def add_edge(self, from_task_id, to_task_id, collection_id, relation="depends_on"):
        self.calls.append(("add_edge", from_task_id, to_task_id, collection_id, relation))


def test_semantic_review_failure_completes_review_and_creates_nonblocking_repair_dependency():
    store = SemanticRepairStore()
    scheduler = _scheduler_with_store(store)
    scheduler._limits_dict = lambda project: {"max_tasks_per_collection": 100, "max_task_depth": 8, "max_children_per_task": 10}

    scheduler._handle_failed_review({}, store.get_task("T-R1"))

    request = next(call for call in store.calls if call[0] == "request_task")
    kwargs = request[2]
    assert kwargs["blocking"] is False
    assert kwargs["depends_on"] == ["T-R1"]
    assert "fresh independent review task" in kwargs["reason"]


def test_repair_implementation_gets_fresh_review_that_reverifies_original_implementation():
    store = SemanticRepairStore()
    # Simulate the failed review having completed as durable historical evidence.
    store.tasks["T-R1"]["status"] = "COMPLETE"
    scheduler = _scheduler_with_store(store)

    scheduler._ensure_review({}, {"id": "COL-1"}, store.get_task("T-I2"))

    create = next(call for call in store.calls if call[0] == "create_task")[1]
    assert create["profile"] == "review"
    assert create["depends_on"] == ["T-I2", "T-I1"]
    assert create["parent_task_id"] == "T-I2"
    assert "fresh independent post-repair review" in create["objective"].lower()
    assert ("add_edge", "T-I2", "T-R2", "COL-1", "verifies") in store.calls
    assert ("add_edge", "T-I1", "T-R2", "COL-1", "verifies") in store.calls
    assert any(call[0] == "event" and "POST_REPAIR_REVIEW_CREATED" in call for call in store.calls)
