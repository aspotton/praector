from praector.db import DoltStore
from praector.models import TaskStatus


class GateHarness:
    def __init__(self, *, objective="Add API-key authentication and per-key rate limiting to a public API endpoint.", collection_created_at="2026-11-12 09:00:00", review_status=TaskStatus.COMPLETE.value, review_created="2026-11-10 10:02:00", review_updated="2026-11-10 10:03:00", include_review_edge=True, critic_status=TaskStatus.COMPLETE.value, critic_updated="2026-11-10 10:01:00", include_critic_edge=True, active_coordinators=0, include_review=True, include_critic=True, baseline_matches=True):
        self.objective = objective
        self.collection_created_at = collection_created_at
        self.review_status = review_status
        self.review_created = review_created
        self.review_updated = review_updated
        self.include_review_edge = include_review_edge
        self.critic_status = critic_status
        self.critic_updated = critic_updated
        self.include_critic_edge = include_critic_edge
        self.active_coordinators = active_coordinators
        self.include_review = include_review
        self.include_critic = include_critic
        self.baseline_matches = baseline_matches

    def _critic_plan_baseline_locked(self, critic_task_id):
        return {"fingerprint": "fp-current" if self.baseline_matches else "fp-stale"}

    def _material_plan_fingerprint_locked(self, collection_id, objective, *, legacy_raw_base_commit=False):
        return "fp-current"

    def _query_locked(self, query):
        if query.startswith("SELECT objective, created_at FROM collections") or query.startswith("SELECT objective FROM collections"):
            return [{"objective": self.objective, "created_at": self.collection_created_at}]
        if "profile='plan_review'" in query and query.startswith("SELECT * FROM tasks"):
            if not self.include_review:
                return []
            return [{
                "id": "T-PLAN",
                "status": self.review_status,
                "created_at": self.review_created,
                "updated_at": self.review_updated,
                "result_json": '{"verdict":"pass"}',
            }]
        if "from_task_id='T-PLAN'" in query and "to_task_id='T-IMPL'" in query:
            return [{"ok": 1}] if self.include_review_edge else []
        if "profile='critic'" in query and query.startswith("SELECT * FROM tasks"):
            if not self.include_critic:
                return []
            return [{
                "id": "T-CRITIC",
                "status": self.critic_status,
                "created_at": "2026-11-10 10:00:00",
                "updated_at": self.critic_updated,
            }]
        if "from_task_id='T-CRITIC'" in query and "to_task_id='T-PLAN'" in query:
            return [{"ok": 1}] if self.include_critic_edge else []
        if query.startswith("SELECT COUNT(*) AS n FROM tasks WHERE") and "profile='coordinator'" in query:
            return [{"n": self.active_coordinators}]
        raise AssertionError(f"unexpected query: {query}")


def _impl(created_at="2026-11-10 10:01:30"):
    return {
        "id": "T-IMPL",
        "collection_id": "COL-1",
        "profile": "implement",
        "created_at": created_at,
    }


def test_claim_gate_blocks_implement_before_plan_review_exists():
    store = GateHarness(include_review=False)
    assert DoltStore._planning_gate_allows_claim_locked(store, _impl()) is False


def test_claim_gate_blocks_implement_while_latest_plan_review_is_not_complete():
    store = GateHarness(review_status=TaskStatus.RUNNING.value)
    assert DoltStore._planning_gate_allows_claim_locked(store, _impl()) is False


def test_claim_gate_requires_explicit_plan_review_to_implementation_edge():
    store = GateHarness(include_review_edge=False)
    assert DoltStore._planning_gate_allows_claim_locked(store, _impl()) is False


def test_claim_gate_rejects_review_that_predates_implementation_contract():
    store = GateHarness(review_updated="2026-11-10 10:01:00")
    assert DoltStore._planning_gate_allows_claim_locked(store, _impl("2026-11-10 10:01:30")) is False


def test_claim_gate_requires_required_critic_to_be_included_and_complete():
    assert DoltStore._planning_gate_allows_claim_locked(
        GateHarness(critic_status=TaskStatus.RUNNING.value), _impl()
    ) is False
    assert DoltStore._planning_gate_allows_claim_locked(
        GateHarness(include_critic_edge=False), _impl()
    ) is False


def test_claim_gate_blocks_while_coordinator_is_still_active():
    store = GateHarness(active_coordinators=1)
    assert DoltStore._planning_gate_allows_claim_locked(store, _impl()) is False


def test_claim_gate_allows_implementation_after_fresh_passing_graph_gate():
    store = GateHarness()
    assert DoltStore._planning_gate_allows_claim_locked(store, _impl()) is True


def _plan_review():
    return {
        "id": "T-PLAN",
        "collection_id": "COL-1",
        "profile": "plan_review",
        "created_at": "2026-11-10 10:02:00",
    }


def test_claim_gate_blocks_plan_review_when_latest_critic_baseline_is_stale():
    store = GateHarness(baseline_matches=False)
    assert DoltStore._planning_gate_allows_claim_locked(store, _plan_review()) is False


def test_claim_gate_allows_plan_review_when_latest_critic_baseline_matches():
    store = GateHarness()
    assert DoltStore._planning_gate_allows_claim_locked(store, _plan_review()) is True


def test_claim_gate_blocks_implementation_when_latest_critic_baseline_is_stale():
    store = GateHarness(baseline_matches=False)
    assert DoltStore._planning_gate_allows_claim_locked(store, _impl()) is False


def test_claim_gate_blocks_exact_conversational_twilio_feature_before_planning_gates():
    objective = (
        "In the Twilio gateway we make use of templates. I'd like to add a new feature, "
        "likely a new template, that when we initiate a transfer to a phone number and it "
        "has commas like +12125552211,6655 it would initiate a call transfer using the Dial verb. "
        "It would need to dial these other digits, 6655, at answer. This only applies to bots "
        "that make use of Twilio."
    )
    store = GateHarness(objective=objective, include_review=False, include_critic=False)
    assert DoltStore._planning_gate_allows_claim_locked(store, _impl()) is False
