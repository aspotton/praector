from contextlib import contextmanager

from praector.config import GlobalConfig
from praector.db import DependencyCycleError, DoltStore
from praector.scheduler import Scheduler


class LegacyCycleHarness:
    def __init__(self, tasks, edges):
        self.tasks = list(tasks)
        self.edges = list(edges)
        self.mutations = []
        self.commits = []

    def _query_locked(self, query):
        if query.startswith("SELECT id,project_id,profile,status FROM tasks"):
            return list(self.tasks)
        if query.startswith("SELECT from_task_id,to_task_id FROM task_edges"):
            return [
                {"from_task_id": from_id, "to_task_id": to_id}
                for from_id, to_id in self.edges
            ]
        raise AssertionError(query)

    def _dependency_edges_locked(self, collection_id):
        return DoltStore._dependency_edges_locked(self, collection_id)

    def _dependency_cycle_in_edges(self, edges):
        return DoltStore._dependency_cycle_in_edges(edges)

    def _dependency_path_in_edges(self, edges, start_task_id, target_task_id):
        return DoltStore._dependency_path_in_edges(edges, start_task_id, target_task_id)

    def _mutate_locked(self, query):
        self.mutations.append(query)

    def _semantic_commit_locked(self, message):
        self.commits.append(message)


def _task(task_id, profile):
    return {
        "id": task_id,
        "project_id": "PRJ-1",
        "profile": profile,
        "status": "READY",
    }


def test_legacy_repair_removes_plan_review_waiting_on_implementation_cycle():
    store = LegacyCycleHarness(
        [_task("T-I", "implement"), _task("T-P", "plan_review")],
        [("T-P", "T-I"), ("T-I", "T-P")],
    )

    repaired, unresolved = DoltStore._repair_legacy_dependency_cycles_locked(store, "COL-1")

    assert repaired == 1
    assert unresolved is None
    assert len(store.mutations) == 1
    sql = store.mutations[0]
    assert "from_task_id='T-I'" in sql
    assert "to_task_id='T-P'" in sql
    assert "LEGACY_DEPENDENCY_CYCLE_REPAIRED" in sql
    assert "legacy_plan_review_waited_for_implementation" in sql
    assert store.commits == ["praector: repair 1 legacy dependency cycle edge(s) in COL-1"]


def test_legacy_repair_removes_plan_review_waiting_on_coordinator_cycle():
    store = LegacyCycleHarness(
        [_task("T-C", "coordinator"), _task("T-P", "plan_review")],
        [("T-P", "T-C"), ("T-C", "T-P")],
    )

    repaired, unresolved = DoltStore._repair_legacy_dependency_cycles_locked(store, "COL-1")

    assert repaired == 1
    assert unresolved is None
    sql = store.mutations[0]
    assert "from_task_id='T-C'" in sql
    assert "to_task_id='T-P'" in sql
    assert "legacy_plan_review_waited_for_coordinator" in sql


def test_unknown_legacy_cycle_is_not_destructively_guessed():
    store = LegacyCycleHarness(
        [_task("T-A", "critic"), _task("T-B", "research")],
        [("T-A", "T-B"), ("T-B", "T-A")],
    )

    repaired, unresolved = DoltStore._repair_legacy_dependency_cycles_locked(store, "COL-1")

    assert repaired == 0
    assert unresolved is not None
    assert unresolved[0] == unresolved[-1]
    assert set(unresolved[:-1]) == {"T-A", "T-B"}
    assert store.mutations == []
    assert store.commits == []


def test_legacy_repair_commits_known_bad_edge_even_if_an_unknown_cycle_remains():
    store = LegacyCycleHarness(
        [
            _task("T-I", "implement"), _task("T-P", "plan_review"),
            _task("T-A", "critic"), _task("T-B", "research"),
        ],
        [
            ("T-P", "T-I"), ("T-I", "T-P"),
            ("T-A", "T-B"), ("T-B", "T-A"),
        ],
    )

    repaired, unresolved = DoltStore._repair_legacy_dependency_cycles_locked(store, "COL-1")

    assert repaired == 1
    assert unresolved is not None
    assert len(store.mutations) == 1
    assert "from_task_id='T-I'" in store.mutations[0]
    assert "to_task_id='T-P'" in store.mutations[0]



class QuarantineStore:
    def __init__(self):
        self.status = "RUNNING"
        self.events = []

    def repair_legacy_dependency_cycles(self, collection_id):
        return 0, ["T-A", "T-B", "T-A"]

    def get_collection(self, collection_id):
        return {"id": collection_id, "project_id": "PRJ-1", "status": self.status}

    def set_collection_status(self, collection_id, status, commit=True):
        self.status = status

    def events_for_collection(self, collection_id, limit=50):
        return list(self.events)

    def event(self, project_id, collection_id, task_id, attempt_id, event_type, payload):
        self.events.append({"event_type": event_type, "payload_json": __import__("json").dumps(payload)})


def test_safe_reconcile_quarantines_unknown_cycle_without_raising(tmp_path):
    store = QuarantineStore()
    scheduler = Scheduler(store, GlobalConfig(home=tmp_path), max_workers=1)

    assert scheduler._reconcile_collection_safely("COL-BAD") == "BLOCKED"
    assert "COL-BAD" in scheduler._quarantined_collections
    assert store.status == "BLOCKED"
    assert [event["event_type"] for event in store.events] == ["DEPENDENCY_CYCLE_QUARANTINED"]

    # Repeated polling is idempotent and does not spam duplicate durable events.
    assert scheduler._reconcile_collection_safely("COL-BAD") == "BLOCKED"
    assert len(store.events) == 1


class DaemonIsolationStore:
    def __init__(self):
        self.statuses = {"COL-BAD": "RUNNING", "COL-GOOD": "RUNNING"}
        self.events = []

    def list_projects(self):
        return []

    def running_sessions(self):
        return []

    def list_collections(self):
        return [
            {"id": "COL-BAD", "project_id": "PRJ-1", "status": self.statuses["COL-BAD"]},
            {"id": "COL-GOOD", "project_id": "PRJ-1", "status": self.statuses["COL-GOOD"]},
        ]

    def get_collection(self, collection_id):
        return {"id": collection_id, "project_id": "PRJ-1", "status": self.statuses[collection_id]}

    def set_collection_status(self, collection_id, status, commit=True):
        self.statuses[collection_id] = status

    def events_for_collection(self, collection_id, limit=50):
        return [event for event in self.events if event["collection_id"] == collection_id]

    def event(self, project_id, collection_id, task_id, attempt_id, event_type, payload):
        import json
        self.events.append({
            "collection_id": collection_id,
            "event_type": event_type,
            "payload_json": json.dumps(payload),
        })

    def runnable_tasks(self, collection_id, limit=1):
        return []


class DaemonIsolationScheduler(Scheduler):
    @contextmanager
    def exclusive_scheduler(self):
        yield

    def recover_stale_sessions(self):
        return 0

    def _reconcile_collection(self, collection_id):
        if collection_id == "COL-BAD":
            raise DependencyCycleError("legacy cycle")
        return "RUNNING"


def test_daemon_once_continues_past_quarantined_collection(tmp_path):
    store = DaemonIsolationStore()
    scheduler = DaemonIsolationScheduler(store, GlobalConfig(home=tmp_path), max_workers=1)

    scheduler.daemon(once=True, poll_seconds=0)

    assert store.statuses["COL-BAD"] == "BLOCKED"
    assert store.statuses["COL-GOOD"] == "RUNNING"
    assert "COL-BAD" in scheduler._quarantined_collections
    assert len(store.events) == 1
