from praector.db import DoltStore


def test_retry_failed_task_creates_replacement_and_supersedes(monkeypatch):
    store = object.__new__(DoltStore)
    old = {
        "id": "T-OLD",
        "project_id": "PRJ-1",
        "collection_id": "COL-1",
        "profile": "coordinator",
        "title": "Initial coordination",
        "objective": "Plan the collection",
        "reason": "Collection entry point",
        "status": "FAILED",
        "parent_task_id": None,
        "requested_by_task_id": None,
        "workspace_mode": "collection",
        "base_commit": "abc",
        "current_head": "def",
        "priority": 50,
        "depth": 0,
    }
    replacement = dict(old, id="T-NEW", status="READY", reason="retry")
    calls = {}

    monkeypatch.setattr(store, "get_task", lambda task_id: dict(replacement if task_id == "T-NEW" else old))
    monkeypatch.setattr(store, "dependencies", lambda task_id: [{"id": "T-DEP"}])

    def create_task(**kwargs):
        calls["create"] = kwargs
        return dict(replacement), True

    monkeypatch.setattr(store, "create_task", create_task)
    monkeypatch.setattr(store, "supersede_task", lambda old_id, new_id, reason: calls.setdefault("supersede", (old_id, new_id, reason)))
    monkeypatch.setattr(store, "set_collection_status", lambda collection_id, status, commit=True: calls.setdefault("collection", (collection_id, status, commit)))
    monkeypatch.setattr(store, "event", lambda *args: calls.setdefault("event", args))

    row = store.retry_failed_task("T-OLD", "transient harness failure")

    assert row["id"] == "T-NEW"
    assert calls["create"]["profile"] == "coordinator"
    assert calls["create"]["depends_on"] == ["T-DEP"]
    assert calls["create"]["base_commit"] == "def"
    assert calls["create"]["allow_dedupe"] is False
    assert calls["supersede"][:2] == ("T-OLD", "T-NEW")
    assert calls["collection"] == ("COL-1", "RUNNING", True)


def test_retry_rejects_non_failed_task(monkeypatch):
    import pytest

    store = object.__new__(DoltStore)
    monkeypatch.setattr(store, "get_task", lambda task_id: {"id": task_id, "status": "COMPLETE"})
    with pytest.raises(ValueError, match="Only FAILED tasks can be retried"):
        store.retry_failed_task("T-DONE", "nope")
