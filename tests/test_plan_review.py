from pathlib import Path

from praector.config import GlobalConfig
from praector.models import TaskStatus
from praector.scheduler import Scheduler


class FakePlanReviewStore:
    def __init__(self):
        self.task = {
            "id": "T-PLAN",
            "project_id": "PRJ-1",
            "collection_id": "COL-1",
            "profile": "plan_review",
            "status": TaskStatus.VERIFYING.value,
            "title": "Review plan",
            "result": {"status": "complete", "verdict": "fail", "summary": "Missing rollback strategy"},
        }
        self.attention = None
        self.events = []

    def get_task(self, task_id):
        assert task_id == "T-PLAN"
        return dict(self.task)

    def request_coordinator_attention(self, caller_task_id, reason, *, blocking, limits):
        self.attention = {
            "caller_task_id": caller_task_id,
            "reason": reason,
            "blocking": blocking,
            "limits": limits,
        }
        self.task["status"] = TaskStatus.WAITING.value
        return ({"id": "T-COORD"}, True)

    def event(self, project_id, collection_id, task_id, attempt_id, event_type, payload):
        self.events.append((project_id, collection_id, task_id, attempt_id, event_type, payload))


def test_failed_plan_review_blocks_on_coordinator_repair(tmp_path: Path):
    store = FakePlanReviewStore()
    scheduler = Scheduler(store, GlobalConfig(home=tmp_path), max_workers=1)
    project = {
        "id": "PRJ-1",
        "name": "demo",
        "root_path": str(tmp_path / "repo"),
        "base_branch": "main",
        "config": {"limits": {
            "max_task_depth": 8,
            "max_tasks_per_collection": 100,
            "max_children_per_task": 10,
            "max_parallel_tasks": 4,
            "max_attempts_per_task": 3,
            "task_timeout_seconds": 3600,
        }},
    }

    scheduler._handle_failed_plan_review(project, store.task)

    assert store.attention is not None
    assert store.attention["caller_task_id"] == "T-PLAN"
    assert store.attention["blocking"] is True
    assert "Missing rollback strategy" in store.attention["reason"]
    assert store.task["status"] == TaskStatus.WAITING.value
    assert any(event[4] == "PLAN_REVIEW_REPAIR_REQUESTED" for event in store.events)
