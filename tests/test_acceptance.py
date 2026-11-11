import argparse

import pytest

from praector.cli import _validate_collection_for_acceptance, build_parser


class FakeStore:
    def __init__(self, tasks, edges=(), state="COMPLETE", *, stale_critic=False):
        self.tasks = tasks
        self.edges = list(edges)
        self.state = state
        self.stale_critic = stale_critic

    def update_collection_terminal_state(self, collection_id):
        return self.state

    def get_collection(self, collection_id):
        return {
            "id": collection_id,
            "status": self.state,
            "objective": "Add API-key authentication and per-key rate limiting",
        }

    def list_tasks(self, collection_id):
        return self.tasks

    def list_edges(self, collection_id):
        return self.edges

    def critic_revalidation_state(self, collection_id):
        return {
            "latest_critic_task_id": "T-K",
            "latest_critic_status": "COMPLETE",
            "current_material_plan_fingerprint": "fp-new" if self.stale_critic else "fp",
            "sealed_fingerprint": "fp",
            "baseline_source": "initial_plan_seal",
            "stale": self.stale_critic,
        }


def test_acceptance_requires_explicit_passing_review_for_implementation():
    tasks = [
        {"id": "T-I", "profile": "implement", "status": "COMPLETE", "result": {}, "parent_task_id": None},
        {"id": "T-R", "profile": "review", "status": "COMPLETE", "result": {"verdict": "pass"}, "parent_task_id": "T-I"},
    ]
    edges = [{"from_task_id": "T-I", "to_task_id": "T-R", "relation": "verifies"}]
    result = _validate_collection_for_acceptance(FakeStore(tasks, edges), {"id": "COL-1"})
    assert len(result["tasks"]) == 2


def test_acceptance_rejects_review_without_verdict():
    tasks = [
        {"id": "T-I", "profile": "implement", "status": "COMPLETE", "result": {}, "parent_task_id": None},
        {"id": "T-R", "profile": "review", "status": "COMPLETE", "result": {}, "parent_task_id": "T-I"},
    ]
    edges = [{"from_task_id": "T-I", "to_task_id": "T-R", "relation": "verifies"}]
    with pytest.raises(RuntimeError, match="explicit pass/fail"):
        _validate_collection_for_acceptance(FakeStore(tasks, edges), {"id": "COL-1"})


def test_collection_accept_parser_defaults_to_safe_ff_only():
    args = build_parser().parse_args(["collection", "accept", "COL-1"])
    assert args.collection_id == "COL-1"
    assert args.no_ff is False
    assert args.dry_run is False


def test_result_show_parser_allows_current_task_default():
    args = build_parser().parse_args(["result", "show"])
    assert args.task_id is None


def test_acceptance_rejects_stale_required_critic_even_if_collection_says_complete():
    tasks = [
        {"id": "T-I", "profile": "implement", "status": "COMPLETE", "result": {}, "parent_task_id": None},
        {"id": "T-R", "profile": "review", "status": "COMPLETE", "result": {"verdict": "pass"}, "parent_task_id": "T-I"},
    ]
    edges = [{"from_task_id": "T-I", "to_task_id": "T-R", "relation": "verifies"}]
    with pytest.raises(RuntimeError, match="stale required critic"):
        _validate_collection_for_acceptance(
            FakeStore(tasks, edges, stale_critic=True), {"id": "COL-1"}
        )


def test_acceptance_allows_historical_failed_review_after_fresh_post_repair_pass():
    tasks = [
        {"id": "T-I1", "profile": "implement", "status": "COMPLETE", "result": {}, "parent_task_id": None},
        {"id": "T-R1", "profile": "review", "status": "COMPLETE", "result": {"verdict": "fail"}, "parent_task_id": "T-I1"},
        {"id": "T-I2", "profile": "implement", "status": "COMPLETE", "result": {}, "parent_task_id": "T-R1"},
        {"id": "T-R2", "profile": "review", "status": "COMPLETE", "result": {"verdict": "pass"}, "parent_task_id": "T-I2"},
    ]
    edges = [
        {"from_task_id": "T-I1", "to_task_id": "T-R1", "relation": "verifies"},
        {"from_task_id": "T-I1", "to_task_id": "T-R2", "relation": "verifies"},
        {"from_task_id": "T-I2", "to_task_id": "T-R2", "relation": "verifies"},
    ]
    result = _validate_collection_for_acceptance(FakeStore(tasks, edges), {"id": "COL-1"})
    assert len(result["tasks"]) == 4
