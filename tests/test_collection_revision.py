from pathlib import Path

from praector.cli import build_parser
from praector.db import DoltStore
from praector.profile_prompts import load_profile_prompt


def test_collection_revise_parser_requires_reason():
    args = build_parser().parse_args([
        "collection", "revise", "COL-1", "--reason", "Fix the concurrent limiter race"
    ])
    assert args.collection_id == "COL-1"
    assert args.reason == "Fix the concurrent limiter race"
    assert args.detach is False


def test_open_revision_prevents_terminal_complete(monkeypatch):
    store = object.__new__(DoltStore)
    tasks = [{"id": "T-I", "profile": "implement", "status": "COMPLETE", "parent_task_id": None, "result": {}}]
    statuses = []

    monkeypatch.setattr(store, "repair_superseded_review_lineage", lambda collection_id: 0)
    monkeypatch.setattr(store, "reconcile_collection_revisions", lambda collection_id: 0)
    monkeypatch.setattr(store, "list_tasks", lambda collection_id: tasks)
    monkeypatch.setattr(store, "list_collection_revisions", lambda collection_id, status=None: [{"id": "REV-1", "status": "OPEN"}] if status == "OPEN" else [])
    monkeypatch.setattr(store, "runnable_tasks", lambda collection_id, limit=1: [])
    monkeypatch.setattr(store, "has_running_session", lambda collection_id: False)
    monkeypatch.setattr(store, "get_collection", lambda collection_id: {"id": collection_id, "status": "COMPLETE"})
    monkeypatch.setattr(store, "set_collection_status", lambda collection_id, status, commit=True: statuses.append(status))

    state = store.update_collection_terminal_state("COL-1")

    assert state == "BLOCKED"
    assert statuses == ["BLOCKED"]


def test_revision_satisfied_only_by_fresh_reviewed_implementation(monkeypatch):
    store = object.__new__(DoltStore)
    revision = {"id": "REV-1", "created_at": "2026-11-10 10:00:00.000000", "status": "OPEN"}
    tasks = [
        {"id": "T-C", "profile": "coordinator", "status": "COMPLETE", "created_at": "2026-11-10 10:00:01.000000", "result": {}},
        {"id": "T-I", "profile": "implement", "status": "COMPLETE", "created_at": "2026-11-10 10:00:02.000000", "result": {}},
        {"id": "T-R", "profile": "review", "status": "COMPLETE", "created_at": "2026-11-10 10:00:03.000000", "result": {"verdict": "pass"}},
    ]
    edges = [{"from_task_id": "T-I", "to_task_id": "T-R", "relation": "verifies"}]
    mutations = []
    events = []

    monkeypatch.setattr(store, "list_collection_revisions", lambda collection_id, status=None: [revision] if status == "OPEN" else [])
    monkeypatch.setattr(store, "list_tasks", lambda collection_id: tasks)
    monkeypatch.setattr(store, "list_edges", lambda collection_id: edges)
    monkeypatch.setattr(store, "mutate", lambda sql, message=None: mutations.append((sql, message)))
    monkeypatch.setattr(store, "get_collection", lambda collection_id: {"id": collection_id, "project_id": "PRJ-1"})
    monkeypatch.setattr(store, "event", lambda *args: events.append(args))

    assert store.reconcile_collection_revisions("COL-1") == 1
    assert any("SATISFIED" in sql for sql, _ in mutations)
    assert any(args[4] == "COLLECTION_REVISION_SATISFIED" for args in events)


def test_revision_not_satisfied_without_fresh_passing_review(monkeypatch):
    store = object.__new__(DoltStore)
    revision = {"id": "REV-1", "created_at": "2026-11-10 10:00:00.000000", "status": "OPEN"}
    tasks = [
        {"id": "T-I", "profile": "implement", "status": "COMPLETE", "created_at": "2026-11-10 10:00:02.000000", "result": {}},
        {"id": "T-R", "profile": "review", "status": "COMPLETE", "created_at": "2026-11-10 10:00:03.000000", "result": {"verdict": "fail"}},
    ]
    edges = [{"from_task_id": "T-I", "to_task_id": "T-R", "relation": "verifies"}]
    mutations = []

    monkeypatch.setattr(store, "list_collection_revisions", lambda collection_id, status=None: [revision] if status == "OPEN" else [])
    monkeypatch.setattr(store, "list_tasks", lambda collection_id: tasks)
    monkeypatch.setattr(store, "list_edges", lambda collection_id: edges)
    monkeypatch.setattr(store, "mutate", lambda sql, message=None: mutations.append((sql, message)))
    monkeypatch.setattr(store, "get_collection", lambda collection_id: {"id": collection_id, "project_id": "PRJ-1"})
    monkeypatch.setattr(store, "event", lambda *args: None)

    assert store.reconcile_collection_revisions("COL-1") == 0
    assert mutations == []


def test_review_prompt_treats_explicit_requirement_violation_as_blocker():
    prompt = load_profile_prompt("review")
    assert "Merely documenting the limitation does not waive an explicit requirement" in prompt
    assert "single process" in prompt


def test_terminal_state_stays_running_while_parent_session_is_active(monkeypatch):
    store = object.__new__(DoltStore)
    tasks = [
        {"id": "T-C", "profile": "coordinator", "status": "WAITING", "result": {}},
        {"id": "T-K", "profile": "critic", "status": "READY", "result": {}},
    ]
    statuses = []

    monkeypatch.setattr(store, "repair_superseded_review_lineage", lambda collection_id: 0)
    monkeypatch.setattr(store, "reconcile_collection_revisions", lambda collection_id: 0)
    monkeypatch.setattr(store, "list_tasks", lambda collection_id: tasks)
    monkeypatch.setattr(store, "list_collection_revisions", lambda collection_id, status=None: [])
    monkeypatch.setattr(store, "runnable_tasks", lambda collection_id, limit=1: [])
    monkeypatch.setattr(store, "has_running_session", lambda collection_id: True)
    monkeypatch.setattr(store, "get_collection", lambda collection_id: {"id": collection_id, "status": "RUNNING"})
    monkeypatch.setattr(store, "set_collection_status", lambda collection_id, status, commit=True: statuses.append(status))

    assert store.update_collection_terminal_state("COL-1") == "RUNNING"
    assert statuses == []
