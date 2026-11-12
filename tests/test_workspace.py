from __future__ import annotations

import subprocess
from pathlib import Path

import pytest

from praector.config import GlobalConfig
from praector.workspace import GitWorkspace


class FakeStore:
    def __init__(self, tasks):
        self.tasks = tasks

    def update_collection_workspace(self, collection_id, worktree):
        self.collection_worktree = worktree

    def set_task_workspace(self, task_id, **kwargs):
        self.tasks[task_id].update({k: v for k, v in kwargs.items() if v is not None})

    def get_task(self, task_id):
        return dict(self.tasks[task_id])


def git(repo: Path, *args: str):
    return subprocess.run(["git", "-C", str(repo), *args], text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, check=True)


def init_repo(tmp_path: Path) -> Path:
    repo = tmp_path / "repo"
    repo.mkdir()
    git(repo, "init", "-b", "main")
    git(repo, "config", "user.name", "Test")
    git(repo, "config", "user.email", "test@example.com")
    (repo / "base.txt").write_text("base\n")
    git(repo, "add", ".")
    git(repo, "commit", "-m", "initial")
    return repo


def test_inherited_implementation_merges_back_to_parent(tmp_path):
    repo = init_repo(tmp_path)
    cfg = GlobalConfig(home=tmp_path / "praector-home")
    cfg.worktrees_dir.mkdir(parents=True)
    tasks = {
        "T-PARENT": {
            "id": "T-PARENT", "profile": "implement", "parent_task_id": None,
            "workspace_mode": "collection", "branch_name": None, "base_commit": None,
            "worktree_path": None, "current_head": None,
        },
        "T-CHILD": {
            "id": "T-CHILD", "profile": "implement", "parent_task_id": "T-PARENT",
            "workspace_mode": "inherit", "branch_name": None, "base_commit": None,
            "worktree_path": None, "current_head": None,
        },
    }
    store = FakeStore(tasks)
    ws = GitWorkspace(store, cfg)
    project = {"id": "PRJ-1", "root_path": str(repo), "base_branch": "main"}
    collection = {"id": "COL-1", "integration_branch": "praector/collections/col-1", "integration_worktree": None}

    parent_path = ws.prepare_task(project, collection, tasks["T-PARENT"])
    (parent_path / "parent.txt").write_text("parent\n")
    parent_head = ws.checkpoint(store.get_task("T-PARENT"), message="parent checkpoint")
    tasks["T-CHILD"]["base_commit"] = parent_head

    child_path = ws.prepare_task(project, collection, tasks["T-CHILD"])
    assert (child_path / "parent.txt").read_text() == "parent\n"
    (child_path / "child.txt").write_text("child\n")
    ws.checkpoint(store.get_task("T-CHILD"), message="child checkpoint")
    ws.integrate_task(project, collection, store.get_task("T-CHILD"))

    assert (parent_path / "child.txt").read_text() == "child\n"
    integration = Path(store.collection_worktree)
    assert not (integration / "parent.txt").exists()
    assert not (integration / "child.txt").exists()

    ws.integrate_task(project, collection, store.get_task("T-PARENT"))
    assert (integration / "parent.txt").read_text() == "parent\n"
    assert (integration / "child.txt").read_text() == "child\n"


def test_implementation_branch_escape_fails_closed_before_integration(tmp_path):
    repo = init_repo(tmp_path)
    cfg = GlobalConfig(home=tmp_path / "praector-home")
    cfg.worktrees_dir.mkdir(parents=True)
    task = {
        "id": "T-I", "profile": "implement", "parent_task_id": None,
        "workspace_mode": "collection", "branch_name": None, "base_commit": None,
        "worktree_path": None, "current_head": None,
    }
    store = FakeStore({"T-I": task})
    ws = GitWorkspace(store, cfg)
    project = {"id": "PRJ-1", "root_path": str(repo), "base_branch": "main"}
    collection = {
        "id": "COL-1", "integration_branch": "praector/collections/col-1",
        "integration_worktree": None,
    }

    task_path = ws.prepare_task(project, collection, task)
    prepared = store.get_task("T-I")
    assigned = prepared["branch_name"]
    base_head = git(repo, "rev-parse", "HEAD").stdout.strip()
    git(task_path, "checkout", "-b", "fix/escaped-worker-branch")
    (task_path / "feature.txt").write_text("candidate\n")
    git(task_path, "add", ".")
    git(task_path, "commit", "-m", "candidate on escaped branch")
    escaped_head = git(task_path, "rev-parse", "HEAD").stdout.strip()

    with pytest.raises(RuntimeError, match="must remain on Praector-assigned branch"):
        ws.checkpoint(prepared, message="old runtime would accept this")
    assert git(repo, "rev-parse", assigned).stdout.strip() == base_head

    # Reproduce the old inconsistent durable state: current_head followed the
    # escaped worktree while branch_name remained on the untouched task branch.
    store.set_task_workspace("T-I", current_head=escaped_head)
    with pytest.raises(RuntimeError, match="must remain on Praector-assigned branch"):
        ws.integrate_task(project, collection, store.get_task("T-I"))
    assert (
        git(repo, "rev-parse", collection["integration_branch"]).stdout.strip()
        == base_head
    )


def test_read_only_snapshot_detects_checkout_away_and_back(tmp_path):
    repo = init_repo(tmp_path)
    (repo / "base.txt").write_text("base\nsecond\n")
    git(repo, "add", ".")
    git(repo, "commit", "-m", "second base")
    cfg = GlobalConfig(home=tmp_path / "praector-home")
    cfg.worktrees_dir.mkdir(parents=True)
    task = {
        "id": "T-R", "profile": "review", "parent_task_id": None,
        "workspace_mode": "collection", "base_commit": None,
        "worktree_path": None, "current_head": None,
    }
    store = FakeStore({"T-R": task})
    ws = GitWorkspace(store, cfg)
    project = {"id": "PRJ-1", "root_path": str(repo), "base_branch": "main"}
    collection = {
        "id": "COL-1", "integration_branch": "praector/collections/col-1",
        "integration_worktree": None,
    }

    path = ws.prepare_task(project, collection, task)
    baseline = ws.workspace_snapshot(path)
    git(path, "checkout", "--detach", "HEAD^")
    git(path, "checkout", "--detach", baseline.head)

    with pytest.raises(RuntimeError, match="HEAD history changed"):
        ws.validate_read_only_workspace(path, baseline)
    ws.restore_read_only_workspace(path, baseline)
    restored = ws.workspace_snapshot(path)
    assert restored.head == baseline.head
    assert restored.branch == ""
    assert restored.status == ""


def test_read_only_changes_can_be_rolled_back(tmp_path):
    repo = init_repo(tmp_path)
    cfg = GlobalConfig(home=tmp_path / "praector-home")
    cfg.worktrees_dir.mkdir(parents=True)
    task = {
        "id": "T-R", "profile": "research", "parent_task_id": None,
        "workspace_mode": "collection", "base_commit": None,
        "worktree_path": None, "current_head": None,
    }
    store = FakeStore({"T-R": task})
    ws = GitWorkspace(store, cfg)
    project = {"id": "PRJ-1", "root_path": str(repo), "base_branch": "main"}
    collection = {"id": "COL-1", "integration_branch": "praector/collections/col-1", "integration_worktree": None}

    path = ws.prepare_task(project, collection, task)
    (path / "unauthorized.txt").write_text("oops\n")
    assert ws.is_dirty(path)
    ws.rollback_read_only_changes(store.get_task("T-R"))
    assert not (path / "unauthorized.txt").exists()
    assert not ws.is_dirty(path)


def test_accept_collection_fast_forwards_target_without_switching_user_checkout(tmp_path):
    repo = init_repo(tmp_path)
    cfg = GlobalConfig(home=tmp_path / "praector-home")
    cfg.worktrees_dir.mkdir(parents=True)
    tasks = {
        "T-I": {
            "id": "T-I", "profile": "implement", "parent_task_id": None,
            "workspace_mode": "collection", "branch_name": None, "base_commit": None,
            "worktree_path": None, "current_head": None,
        }
    }
    store = FakeStore(tasks)
    store.list_tasks = lambda collection_id: [dict(tasks["T-I"])]
    store.get_collection_acceptance = lambda collection_id: None
    ws = GitWorkspace(store, cfg)
    project = {"id": "PRJ-1", "root_path": str(repo), "base_branch": "main"}
    collection = {
        "id": "COL-1", "title": "Demo", "integration_branch": "praector/collections/col-1",
        "integration_worktree": None,
    }

    task_path = ws.prepare_task(project, collection, tasks["T-I"])
    (task_path / "feature.txt").write_text("done\n")
    ws.checkpoint(store.get_task("T-I"), message="feature")
    ws.integrate_task(project, collection, store.get_task("T-I"))

    before = git(repo, "rev-parse", "main").stdout.strip()
    source = git(repo, "rev-parse", collection["integration_branch"]).stdout.strip()
    assert before != source
    result = ws.accept_collection(project, collection)
    assert result["merge_mode"] == "ff-only"
    assert git(repo, "rev-parse", "main").stdout.strip() == source
    assert git(repo, "branch", "--show-current").stdout.strip() == "main"
    assert (repo / "feature.txt").read_text() == "done\n"


def test_accept_collection_refuses_diverged_target_without_no_ff(tmp_path):
    repo = init_repo(tmp_path)
    cfg = GlobalConfig(home=tmp_path / "praector-home")
    cfg.worktrees_dir.mkdir(parents=True)
    tasks = {
        "T-I": {
            "id": "T-I", "profile": "implement", "parent_task_id": None,
            "workspace_mode": "collection", "branch_name": None, "base_commit": None,
            "worktree_path": None, "current_head": None,
        }
    }
    store = FakeStore(tasks)
    store.list_tasks = lambda collection_id: [dict(tasks["T-I"])]
    store.get_collection_acceptance = lambda collection_id: None
    ws = GitWorkspace(store, cfg)
    project = {"id": "PRJ-1", "root_path": str(repo), "base_branch": "main"}
    collection = {
        "id": "COL-1", "title": "Demo", "integration_branch": "praector/collections/col-1",
        "integration_worktree": None,
    }

    task_path = ws.prepare_task(project, collection, tasks["T-I"])
    (task_path / "feature.txt").write_text("done\n")
    ws.checkpoint(store.get_task("T-I"), message="feature")
    ws.integrate_task(project, collection, store.get_task("T-I"))

    # Advance main independently after the collection was based.
    (repo / "other.txt").write_text("main change\n")
    git(repo, "add", ".")
    git(repo, "commit", "-m", "main moved")

    import pytest
    with pytest.raises(RuntimeError, match="diverged"):
        ws.accept_collection(project, collection)

    result = ws.accept_collection(project, collection, no_ff=True)
    assert result["merge_mode"] == "no-ff"
    assert (repo / "feature.txt").read_text() == "done\n"
    assert (repo / "other.txt").read_text() == "main change\n"


def test_accept_collection_is_idempotent_when_target_already_contains_source(tmp_path):
    repo = init_repo(tmp_path)
    cfg = GlobalConfig(home=tmp_path / "praector-home")
    cfg.worktrees_dir.mkdir(parents=True)
    tasks = {
        "T-I": {
            "id": "T-I", "profile": "implement", "parent_task_id": None,
            "workspace_mode": "collection", "branch_name": None, "base_commit": None,
            "worktree_path": None, "current_head": None,
        }
    }
    store = FakeStore(tasks)
    store.list_tasks = lambda collection_id: [dict(tasks["T-I"])]
    store.get_collection_acceptance = lambda collection_id: None
    ws = GitWorkspace(store, cfg)
    project = {"id": "PRJ-1", "root_path": str(repo), "base_branch": "main"}
    collection = {
        "id": "COL-1", "title": "Demo", "integration_branch": "praector/collections/col-1",
        "integration_worktree": None,
    }
    task_path = ws.prepare_task(project, collection, tasks["T-I"])
    (task_path / "feature.txt").write_text("done\n")
    ws.checkpoint(store.get_task("T-I"), message="feature")
    ws.integrate_task(project, collection, store.get_task("T-I"))
    first = ws.accept_collection(project, collection)
    second = ws.accept_collection(project, collection)
    assert first["accepted_commit"] == second["accepted_commit"]
    assert second["merge_mode"] == "already-contained"


def test_prepare_collection_task_does_not_retroactively_fill_base_commit(tmp_path):
    repo = init_repo(tmp_path)
    cfg = GlobalConfig(home=tmp_path / "praector-home")
    cfg.worktrees_dir.mkdir(parents=True)
    task = {
        "id": "T-I", "profile": "implement", "parent_task_id": None,
        "workspace_mode": "collection", "branch_name": None, "base_commit": None,
        "worktree_path": None, "current_head": None,
    }
    store = FakeStore({"T-I": task})
    ws = GitWorkspace(store, cfg)
    project = {"id": "PRJ-1", "root_path": str(repo), "base_branch": "main"}
    collection = {
        "id": "COL-1", "integration_branch": "praector/collections/col-1",
        "integration_worktree": None,
    }

    ws.prepare_task(project, collection, task)

    prepared = store.get_task("T-I")
    assert prepared["base_commit"] is None
    assert prepared["current_head"]
