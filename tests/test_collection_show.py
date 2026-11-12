import argparse
import json

import praector.cli as cli


class FakeStore:
    def get_collection(self, collection_id):
        return {
            "id": collection_id,
            "project_id": "PRJ-1",
            "objective": "Add API-key authentication and per-key rate limiting",
            "status": "COMPLETE",
        }

    def list_collection_acceptances(self, collection_id):
        return []

    def list_collection_pull_requests(self, collection_id):
        return [{"provider": "github", "pr_number": 42, "state": "OPEN"}]

    def list_collection_revisions(self, collection_id):
        return [{"id": "REV-1", "status": "SATISFIED"}]

    def list_tasks(self, collection_id):
        return []

    def critic_revalidation_state(self, collection_id):
        return {
            "latest_critic_task_id": None,
            "latest_critic_status": None,
            "current_material_plan_fingerprint": "fp-current",
            "sealed_fingerprint": None,
            "baseline_source": None,
            "stale": False,
        }


def test_collection_show_includes_planning_policy(monkeypatch, capsys):
    monkeypatch.setattr(cli, "_store", lambda: FakeStore())
    args = argparse.Namespace(collection_id="COL-1")

    assert cli.cmd_collection_show(args) == 0

    payload = json.loads(capsys.readouterr().out)
    assert payload["id"] == "COL-1"
    assert payload["planning_policy"]["explore_required"] is True
    assert payload["planning_policy"]["critic_required"] is True
    assert payload["planning_policy"]["plan_review_required"] is True
    assert payload["revisions"] == [{"id": "REV-1", "status": "SATISFIED"}]
    assert payload["pull_requests"] == [{"provider": "github", "pr_number": 42, "state": "OPEN"}]
    assert payload["critic_revalidation"]["current_material_plan_fingerprint"] == "fp-current"
    assert payload["critic_revalidation"]["stale"] is False
