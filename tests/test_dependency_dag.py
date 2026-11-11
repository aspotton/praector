from contextlib import contextmanager

import pytest

from praector.db import DoltStore


class CycleGuardHarness:
    def __init__(self, edges, task_collections=None):
        self.edges = list(edges)
        self.mutations = []
        self.commits = []
        self.task_collections = task_collections or {
            "T-A": "COL-1",
            "T-B": "COL-1",
            "T-C": "COL-1",
        }

    @contextmanager
    def lock(self):
        yield

    def _query_locked(self, query):
        if query.startswith("SELECT id,collection_id FROM tasks WHERE id="):
            task_id = query.split("id=", 1)[1].split(" LIMIT", 1)[0].strip("'")
            collection_id = self.task_collections.get(task_id)
            return ([{"id": task_id, "collection_id": collection_id}]
                    if collection_id is not None else [])
        if query.startswith("SELECT from_task_id,to_task_id FROM task_edges"):
            return [
                {"from_task_id": from_id, "to_task_id": to_id}
                for from_id, to_id in self.edges
            ]
        raise AssertionError(query)


    def _dependency_path_in_edges(self, edges, start_task_id, target_task_id):
        return DoltStore._dependency_path_in_edges(edges, start_task_id, target_task_id)

    def _dependency_edges_locked(self, collection_id):
        return DoltStore._dependency_edges_locked(self, collection_id)

    def _assert_dependency_tasks_in_collection_locked(self, collection_id, task_ids):
        return DoltStore._assert_dependency_tasks_in_collection_locked(self, collection_id, task_ids)

    def _assert_dependency_edges_acyclic_locked(self, collection_id, prospective_edges):
        return DoltStore._assert_dependency_edges_acyclic_locked(self, collection_id, prospective_edges)

    def _mutate_locked(self, query):
        self.mutations.append(query)

    def _semantic_commit_locked(self, message):
        self.commits.append(message)


def test_add_edge_rejects_transitive_dependency_cycle_before_mutation():
    store = CycleGuardHarness([("T-A", "T-B"), ("T-B", "T-C")])

    with pytest.raises(ValueError, match="would create a cycle"):
        DoltStore.add_edge(store, "T-C", "T-A", "COL-1", "depends_on")

    assert store.mutations == []
    assert store.commits == []


def test_add_edge_accepts_acyclic_dependency():
    store = CycleGuardHarness([("T-A", "T-B")])

    DoltStore.add_edge(store, "T-C", "T-B", "COL-1", "depends_on")

    assert len(store.mutations) == 1
    assert len(store.commits) == 1


def test_add_edge_rejects_missing_dependency_before_mutation():
    store = CycleGuardHarness([])

    with pytest.raises(ValueError, match="Dependency task not found"):
        DoltStore.add_edge(store, "T-MISSING", "T-A", "COL-1", "depends_on")

    assert store.mutations == []
    assert store.commits == []


def test_add_edge_rejects_cross_collection_dependency_before_mutation():
    store = CycleGuardHarness([], {"T-A": "COL-1", "T-X": "COL-2"})

    with pytest.raises(ValueError, match="belongs to collection COL-2, not COL-1"):
        DoltStore.add_edge(store, "T-X", "T-A", "COL-1", "depends_on")

    assert store.mutations == []
    assert store.commits == []
