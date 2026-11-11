import json

from praector.db import DoltStore


class RepairHarness:
    def __init__(self, *, matching_revision=True):
        self.matching_revision = matching_revision
        self.mutations = []
        self.commits = []

    def _query_locked(self, query):
        if "FROM events" in query and "CRITIC_PLAN_BASELINE" in query:
            return [{
                "task_id": "T-CRITIC",
                "created_at": "2026-11-10 20:50:00",
                "payload_json": json.dumps({
                    "critic_task_id": "T-CRITIC",
                    "fingerprint": "fp-revision",
                    "source": "compatibility_plan_seal",
                    "plan_review_task_id": "T-PLAN",
                }),
            }]
        if query.startswith("SELECT id,collection_id,requested_by_task_id FROM tasks"):
            return [
                {"id": "T-CRITIC", "collection_id": "COL-1", "requested_by_task_id": "T-REV-COORD"},
                {"id": "T-PLAN", "collection_id": "COL-1", "requested_by_task_id": "T-REV-COORD"},
            ]
        if query.startswith("SELECT id,project_id,coordinator_task_id FROM collection_revisions"):
            if not self.matching_revision:
                return []
            return [{"id": "REV-1", "project_id": "PRJ-1", "coordinator_task_id": "T-REV-COORD"}]
        raise AssertionError(f"unexpected query: {query}")

    def _mutate_locked(self, query):
        self.mutations.append(query)

    def _semantic_commit_locked(self, message):
        self.commits.append(message)


def test_bootstrap_repair_relabels_provable_revision_compatibility_seal_append_only():
    store = RepairHarness(matching_revision=True)

    repaired = DoltStore._repair_revision_baseline_provenance_locked(store)

    assert repaired == 1
    assert len(store.mutations) == 1
    sql = store.mutations[0]
    assert "'CRITIC_PLAN_BASELINE'" in sql
    assert "revision_plan_seal" in sql
    assert "compatibility_plan_seal" in sql
    assert "REV-1" in sql
    assert "fp-revision" in sql
    assert store.commits == ["praector: repair 1 revision critic baseline provenance label(s)"]


def test_bootstrap_repair_preserves_genuine_legacy_compatibility_seal():
    store = RepairHarness(matching_revision=False)

    repaired = DoltStore._repair_revision_baseline_provenance_locked(store)

    assert repaired == 0
    assert store.mutations == []
    assert store.commits == []
