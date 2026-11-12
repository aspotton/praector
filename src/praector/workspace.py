from __future__ import annotations

import os
import re
import shutil
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from .config import GlobalConfig, READ_ONLY_PROFILES
from .db import DoltStore
from .util import run


@dataclass(frozen=True)
class WorkspaceSnapshot:
    """Git identity a read-only attempt must preserve exactly."""

    head: str
    branch: str
    status: str
    head_reflog: str


class GitWorkspace:
    def __init__(self, store: DoltStore, cfg: GlobalConfig):
        self.store = store
        self.cfg = cfg

    def _git(self, repo: Path, *args: str, check: bool = True):
        return run(["git", "-C", str(repo), *args], check=check)

    def repo_root(self, project: dict[str, Any]) -> Path:
        return Path(project["root_path"]).resolve()

    def workspace_snapshot(self, worktree: Path) -> WorkspaceSnapshot:
        branch_proc = self._git(
            worktree, "symbolic-ref", "--quiet", "--short", "HEAD", check=False
        )
        if branch_proc.returncode not in {0, 1}:
            raise RuntimeError(
                f"Cannot resolve worktree branch in {worktree}: "
                f"{branch_proc.stderr.strip() or branch_proc.stdout.strip()}"
            )
        reflog_proc = self._git(
            worktree, "reflog", "show", "--format=%H", "HEAD", check=False
        )
        if reflog_proc.returncode != 0:
            raise RuntimeError(
                f"Cannot read worktree HEAD reflog in {worktree}: "
                f"{reflog_proc.stderr.strip() or reflog_proc.stdout.strip()}"
            )
        return WorkspaceSnapshot(
            head=self._git(worktree, "rev-parse", "HEAD").stdout.strip(),
            branch=branch_proc.stdout.strip(),
            status=self._git(
                worktree, "status", "--porcelain=v1", "--untracked-files=all"
            ).stdout.rstrip("\r\n"),
            head_reflog=reflog_proc.stdout.rstrip("\r\n"),
        )

    def _validate_attached_branch(
        self, worktree: Path, expected: str, label: str
    ) -> WorkspaceSnapshot:
        state = self.workspace_snapshot(worktree)
        if state.branch != expected:
            actual = state.branch or "detached HEAD"
            raise RuntimeError(
                f"{label} must remain on Praector-assigned branch {expected}; "
                f"found {actual} at {state.head}"
            )
        return state

    def validate_read_only_workspace(
        self, worktree: Path, baseline: WorkspaceSnapshot
    ) -> None:
        """Reject file, HEAD, branch, and transient checkout mutations."""
        current = self.workspace_snapshot(worktree)
        if current.branch != baseline.branch:
            raise RuntimeError(
                f"Read-only worktree branch identity changed from "
                f"{baseline.branch!r} to {current.branch!r}"
            )
        if current.head != baseline.head:
            raise RuntimeError(
                f"Read-only worktree HEAD changed from {baseline.head} to {current.head}"
            )
        if current.status != baseline.status:
            raise RuntimeError("Read-only worktree file status changed")
        if current.head_reflog != baseline.head_reflog:
            raise RuntimeError(
                "Read-only worktree HEAD history changed; checkout, switch, reset, "
                "or commit operations are not allowed"
            )

    def restore_read_only_workspace(
        self, worktree: Path, baseline: WorkspaceSnapshot
    ) -> None:
        self._git(worktree, "checkout", "--detach", "--force", baseline.head)
        self._git(worktree, "clean", "-fd")

    def ensure_collection(self, project: dict[str, Any], collection: dict[str, Any]) -> Path:
        repo = self.repo_root(project)
        branch = collection["integration_branch"]
        path = self.cfg.worktrees_dir / project["id"] / collection["id"] / "integration"
        path.parent.mkdir(parents=True, exist_ok=True)

        branch_exists = self._git(repo, "show-ref", "--verify", "--quiet", f"refs/heads/{branch}", check=False).returncode == 0
        if not branch_exists:
            base_branch = project["base_branch"]
            base = self._git(repo, "rev-parse", base_branch).stdout.strip()
            self._git(repo, "branch", branch, base)

        if not (path / ".git").exists():
            if path.exists() and any(path.iterdir()):
                raise RuntimeError(f"Collection worktree path is not empty: {path}")
            path.parent.mkdir(parents=True, exist_ok=True)
            self._git(repo, "worktree", "add", str(path), branch)
        self._validate_attached_branch(path, branch, "Collection integration worktree")
        self.store.update_collection_workspace(collection["id"], str(path))
        return path

    def _task_path(self, project: dict[str, Any], collection: dict[str, Any], task: dict[str, Any]) -> Path:
        return self.cfg.worktrees_dir / project["id"] / collection["id"] / task["id"]

    def prepare_task(self, project: dict[str, Any], collection: dict[str, Any], task: dict[str, Any]) -> Path:
        repo = self.repo_root(project)
        integration = self.ensure_collection(project, collection)
        task_path = self._task_path(project, collection, task)
        task_path.parent.mkdir(parents=True, exist_ok=True)

        if task["profile"] in READ_ONLY_PROFILES:
            if not (task_path / ".git").exists():
                if task_path.exists() and any(task_path.iterdir()):
                    raise RuntimeError(f"Task worktree path is not empty: {task_path}")
                ref = task.get("base_commit") or (project["base_branch"] if task.get("workspace_mode") == "isolated" else collection["integration_branch"])
                self._git(repo, "worktree", "add", "--detach", str(task_path), ref)
            else:
                ref = task.get("base_commit") or (project["base_branch"] if task.get("workspace_mode") == "isolated" else collection["integration_branch"])
                # Detach before restoring the candidate. If a prior read-only
                # worker attached this worktree to another branch, reset --hard
                # would otherwise move and potentially destroy that branch.
                self._git(task_path, "checkout", "--detach", "--force", ref)
                self._git(task_path, "clean", "-fd")
            head = self._git(task_path, "rev-parse", "HEAD").stdout.strip()
            state = self.workspace_snapshot(task_path)
            if state.branch:
                raise RuntimeError(
                    f"Read-only task {task['id']} must use detached HEAD; "
                    f"found branch {state.branch}"
                )
            workspace_update = {"worktree_path": str(task_path), "current_head": head}
            if task.get("base_commit"):
                workspace_update["base_commit"] = task["base_commit"]
            self.store.set_task_workspace(task["id"], **workspace_update)
            return task_path

        branch = task.get("branch_name") or f"praector/tasks/{collection['id'].lower()}/{task['id'].lower()}"
        branch_exists = self._git(repo, "show-ref", "--verify", "--quiet", f"refs/heads/{branch}", check=False).returncode == 0
        if not (task_path / ".git").exists():
            if task_path.exists() and any(task_path.iterdir()):
                raise RuntimeError(f"Task worktree path is not empty: {task_path}")
            base_ref = task.get("base_commit") or (project["base_branch"] if task.get("workspace_mode") == "isolated" else collection["integration_branch"])
            if branch_exists:
                self._git(repo, "worktree", "add", str(task_path), branch)
            else:
                self._git(repo, "worktree", "add", "-b", branch, str(task_path), base_ref)
        state = self._validate_attached_branch(
            task_path, branch, f"Implementation task {task['id']}"
        )
        head = state.head
        workspace_update = {
            "worktree_path": str(task_path),
            "branch_name": branch,
            "current_head": head,
        }
        if task.get("base_commit"):
            workspace_update["base_commit"] = task["base_commit"]
        self.store.set_task_workspace(task["id"], **workspace_update)
        return task_path

    @staticmethod
    def _task_git_title(task: dict[str, Any]) -> str:
        title = " ".join(str(task.get("title") or "").split())
        if not title:
            title = " ".join(str(task.get("objective") or "").split())
        if not title:
            title = f"Praector task {task.get('id') or ''}".strip()
        return title if len(title) <= 100 else title[:97].rstrip() + "..."

    @classmethod
    def _task_git_commit_message(
        cls,
        task: dict[str, Any],
        *,
        operation: str,
        attempt_no: int | None = None,
        subject_prefix: str | None = None,
    ) -> str:
        title = cls._task_git_title(task)
        subject = f"{subject_prefix}: {title}" if subject_prefix else title
        result = task.get("result") or {}
        summary = " ".join(str(result.get("pr_summary") or result.get("summary") or "").split())
        if len(summary) > 700:
            summary = summary[:697].rstrip() + "..."
        lines = [subject]
        if summary and summary.casefold() != title.casefold():
            lines.extend(["", summary])
        lines.extend([
            "",
            f"Praector-Collection: {task.get('collection_id') or ''}",
            f"Praector-Task: {task.get('id') or ''}",
        ])
        if attempt_no:
            lines.append(f"Praector-Attempt: {attempt_no}")
        if operation:
            lines.append(f"Praector-Operation: {operation}")
        return "\n".join(lines)

    def is_dirty(self, worktree: Path) -> bool:
        return bool(self._git(worktree, "status", "--porcelain").stdout.strip())

    def checkpoint(
        self,
        task: dict[str, Any],
        *,
        message: str | None = None,
        attempt_no: int | None = None,
        operation: str = "attempt-checkpoint",
    ) -> str | None:
        path_value = task.get("worktree_path")
        if not path_value:
            return task.get("current_head")
        worktree = Path(path_value)
        if not worktree.exists():
            return task.get("current_head")
        if task["profile"] in READ_ONLY_PROFILES:
            return self._git(worktree, "rev-parse", "HEAD").stdout.strip()
        branch = task.get("branch_name")
        if not branch:
            raise RuntimeError(f"Task {task['id']} has no assigned branch to checkpoint")
        self._validate_attached_branch(
            worktree, branch, f"Implementation task {task['id']}"
        )
        if self.is_dirty(worktree):
            self._git(worktree, "add", "-A")
            commit_message = message or self._task_git_commit_message(
                task, operation=operation, attempt_no=attempt_no
            )
            self._git(
                worktree, "-c", "user.name=praector", "-c", "user.email=praector@local",
                "commit", "-m", commit_message
            )
        head = self._validate_attached_branch(
            worktree, branch, f"Implementation task {task['id']}"
        ).head
        self.store.set_task_workspace(task["id"], current_head=head)
        return head

    def rollback_read_only_changes(self, task: dict[str, Any]) -> None:
        path_value = task.get("worktree_path")
        if not path_value:
            return
        worktree = Path(path_value)
        if worktree.exists():
            head = task.get("current_head") or self._git(
                worktree, "rev-parse", "HEAD"
            ).stdout.strip()
            self._git(worktree, "checkout", "--detach", "--force", head)
            self._git(worktree, "clean", "-fd")

    def integrate_task(self, project: dict[str, Any], collection: dict[str, Any], task: dict[str, Any]) -> str:
        if task["profile"] in READ_ONLY_PROFILES:
            integration = self.ensure_collection(project, collection)
            return self._git(integration, "rev-parse", "HEAD").stdout.strip()
        branch = task.get("branch_name")
        if not branch:
            raise RuntimeError(f"Task {task['id']} has no branch to integrate")
        current_head = task.get("current_head")
        if not current_head:
            raise RuntimeError(f"Task {task['id']} has no checkpoint commit to integrate")
        worktree_value = task.get("worktree_path")
        if not worktree_value or not Path(worktree_value).exists():
            raise RuntimeError(
                f"Task {task['id']} has no available worktree to validate before integration"
            )
        task_state = self._validate_attached_branch(
            Path(worktree_value), branch, f"Implementation task {task['id']}"
        )
        if task_state.head != current_head:
            raise RuntimeError(
                f"Task {task['id']} worktree HEAD {task_state.head} does not match "
                f"durable checkpoint {current_head}"
            )
        repo = self.repo_root(project)
        branch_head = self._git(repo, "rev-parse", f"refs/heads/{branch}").stdout.strip()
        if branch_head != current_head:
            raise RuntimeError(
                f"Task {task['id']} assigned branch {branch} points to {branch_head}, "
                f"not durable checkpoint {current_head}"
            )

        # An inherited implementation is a contribution to its blocked parent,
        # not an independent collection-level change. Merge it back into the
        # parent's durable task branch so a fresh parent attempt resumes with it.
        target_path: Path
        target_label: str
        parent_id = task.get("parent_task_id")
        if task.get("workspace_mode") == "inherit" and parent_id:
            parent = self.store.get_task(parent_id)
            if parent.get("profile") == "implement" and parent.get("worktree_path"):
                target_path = Path(parent["worktree_path"])
                target_label = parent_id
                parent_branch = parent.get("branch_name")
                if not parent_branch:
                    raise RuntimeError(
                        f"Parent implementation task {parent_id} has no assigned branch"
                    )
                self._validate_attached_branch(
                    target_path,
                    parent_branch,
                    f"Parent implementation task {parent_id}",
                )
            else:
                target_path = self.ensure_collection(project, collection)
                target_label = collection["id"]
        else:
            target_path = self.ensure_collection(project, collection)
            target_label = collection["id"]

        if self.is_dirty(target_path):
            raise RuntimeError(
                f"Integration target {target_path} is dirty; refusing to merge task {task['id']}"
            )

        proc = self._git(
            target_path, "-c", "user.name=praector", "-c", "user.email=praector@local", "merge", "--no-ff", current_head,
            "-m", self._task_git_commit_message(
                task, operation="integration", subject_prefix="Integrate"
            ),
            check=False,
        )
        if proc.returncode != 0:
            self._git(target_path, "merge", "--abort", check=False)
            raise RuntimeError(f"Integration conflict for {task['id']}: {proc.stderr.strip() or proc.stdout.strip()}")
        head = self._git(target_path, "rev-parse", "HEAD").stdout.strip()
        if not self._is_ancestor(target_path, current_head, head):
            raise RuntimeError(
                f"Integration verification failed for {task['id']}: checkpoint "
                f"{current_head} is not reachable from target HEAD {head}"
            )
        if target_label != collection["id"]:
            self.store.set_task_workspace(target_label, current_head=head)
        return head

    def checkpoint_for_delegation(self, task_id: str) -> str | None:
        task = self.store.get_task(task_id)
        if task["profile"] in READ_ONLY_PROFILES:
            return task.get("current_head") or task.get("base_commit")
        return self.checkpoint(task, operation="delegation-checkpoint")

    def collection_diff(self, project: dict[str, Any], collection: dict[str, Any]) -> str:
        repo = self.repo_root(project)
        base = project["base_branch"]
        branch = collection["integration_branch"]
        return self._git(repo, "diff", "--stat", f"{base}...{branch}").stdout

    def _branch_worktree(self, repo: Path, branch: str) -> Path | None:
        proc = self._git(repo, "worktree", "list", "--porcelain")
        current_path: Path | None = None
        for line in proc.stdout.splitlines():
            if line.startswith("worktree "):
                current_path = Path(line.split(" ", 1)[1]).resolve()
            elif line == f"branch refs/heads/{branch}" and current_path is not None:
                return current_path
            elif not line.strip():
                current_path = None
        return None

    def _branch_exists(self, repo: Path, branch: str) -> bool:
        return self._git(repo, "show-ref", "--verify", "--quiet", f"refs/heads/{branch}", check=False).returncode == 0

    def _is_ancestor(self, repo: Path, ancestor: str, descendant: str) -> bool:
        return self._git(repo, "merge-base", "--is-ancestor", ancestor, descendant, check=False).returncode == 0

    def acceptance_plan(
        self,
        project: dict[str, Any],
        collection: dict[str, Any],
        *,
        target_branch: str | None = None,
    ) -> dict[str, Any]:
        repo = self.repo_root(project)
        source = str(collection["integration_branch"])
        target = target_branch or str(project["base_branch"])
        if not self._branch_exists(repo, source):
            raise RuntimeError(f"Collection branch does not exist: {source}")
        if not self._branch_exists(repo, target):
            raise RuntimeError(f"Target branch does not exist: {target}")
        source_commit = self._git(repo, "rev-parse", source).stdout.strip()
        target_commit = self._git(repo, "rev-parse", target).stdout.strip()
        return {
            "source_branch": source,
            "source_commit": source_commit,
            "target_branch": target,
            "target_commit": target_commit,
            "already_contained": self._is_ancestor(repo, source_commit, target_commit),
            "fast_forward": self._is_ancestor(repo, target_commit, source_commit),
            "diff_stat": self._git(repo, "diff", "--stat", f"{target}...{source}").stdout.rstrip(),
        }

    def accept_collection(
        self,
        project: dict[str, Any],
        collection: dict[str, Any],
        *,
        target_branch: str | None = None,
        no_ff: bool = False,
    ) -> dict[str, str]:
        """Merge a completed collection branch into an explicit target branch.

        Safe default is fast-forward only. The merge executes in the worktree that
        already owns the target branch, or in a temporary Praector worktree when the
        target is not checked out anywhere. This avoids silently switching the
        user's checkout.
        """
        repo = self.repo_root(project)
        source_branch = str(collection["integration_branch"])
        target = target_branch or str(project["base_branch"])
        if not self._branch_exists(repo, source_branch):
            raise RuntimeError(f"Collection branch does not exist: {source_branch}")
        if not self._branch_exists(repo, target):
            raise RuntimeError(f"Target branch does not exist: {target}")

        source_commit = self._git(repo, "rev-parse", source_branch).stdout.strip()
        target_before = self._git(repo, "rev-parse", target).stdout.strip()

        integration_path = collection.get("integration_worktree")
        if integration_path and Path(integration_path).exists():
            if self._git(Path(integration_path), "status", "--porcelain").stdout.strip():
                raise RuntimeError(
                    f"Collection integration worktree is dirty: {integration_path}. "
                    "Resolve or checkpoint those changes before acceptance."
                )

        # Idempotent: target already contains the exact collection result.
        if self._is_ancestor(repo, source_commit, target_before):
            return {
                "source_branch": source_branch,
                "source_commit": source_commit,
                "target_branch": target,
                "target_before_commit": target_before,
                "accepted_commit": target_before,
                "merge_mode": "already-contained",
            }

        if not no_ff and not self._is_ancestor(repo, target_before, source_commit):
            raise RuntimeError(
                f"Target branch {target} has diverged from {source_branch}; refusing the safe fast-forward. "
                f"Review the divergence and retry with `praector collection accept {collection['id']} --target {target} --no-ff` "
                "only if a merge commit is intended."
            )

        target_worktree = self._branch_worktree(repo, target)
        temporary = False
        if target_worktree is None:
            safe_target = re.sub(r"[^A-Za-z0-9._-]+", "-", target).strip("-") or "target"
            target_worktree = self.cfg.worktrees_dir / project["id"] / collection["id"] / f"accept-{safe_target}"
            target_worktree.parent.mkdir(parents=True, exist_ok=True)
            if target_worktree.exists() and any(target_worktree.iterdir()):
                raise RuntimeError(f"Acceptance worktree path is not empty: {target_worktree}")
            self._git(repo, "worktree", "add", str(target_worktree), target)
            temporary = True

        try:
            dirty = self._git(target_worktree, "status", "--porcelain").stdout.strip()
            if dirty:
                raise RuntimeError(
                    f"Target branch {target} has uncommitted changes in {target_worktree}; "
                    "acceptance will not modify a dirty worktree."
                )
            if no_ff:
                proc = self._git(
                    target_worktree,
                    "merge", "--no-ff", source_branch,
                    "-m", f"Accept {collection['id']}: {collection['title']}",
                    check=False,
                )
                if proc.returncode != 0:
                    self._git(target_worktree, "merge", "--abort", check=False)
                    raise RuntimeError(
                        f"Acceptance merge conflict for {collection['id']} into {target}: "
                        f"{proc.stderr.strip() or proc.stdout.strip()}"
                    )
                mode = "no-ff"
            else:
                self._git(target_worktree, "merge", "--ff-only", source_branch)
                mode = "ff-only"
            accepted_commit = self._git(target_worktree, "rev-parse", "HEAD").stdout.strip()
        finally:
            if temporary:
                self._git(repo, "worktree", "remove", "--force", str(target_worktree), check=False)
                self._git(repo, "worktree", "prune", check=False)

        return {
            "source_branch": source_branch,
            "source_commit": source_commit,
            "target_branch": target,
            "target_before_commit": target_before,
            "accepted_commit": accepted_commit,
            "merge_mode": mode,
        }

    def cleanup_collection(
        self,
        project: dict[str, Any],
        collection: dict[str, Any],
        *,
        delete_branches: bool = False,
    ) -> dict[str, list[str]]:
        """Remove completed collection/task worktrees and optionally merged branches."""
        repo = self.repo_root(project)
        removed_worktrees: list[str] = []
        removed_branches: list[str] = []

        # Remove task worktrees first, then the collection integration worktree.
        tasks = self.store.list_tasks(collection["id"])
        paths: list[Path] = []
        for task in tasks:
            value = task.get("worktree_path")
            if value:
                paths.append(Path(value))
        if collection.get("integration_worktree"):
            paths.append(Path(collection["integration_worktree"]))
        seen: set[str] = set()
        for path in paths:
            key = str(path.resolve()) if path.exists() else str(path)
            if key in seen:
                continue
            seen.add(key)
            if path.exists():
                self._git(repo, "worktree", "remove", "--force", str(path), check=False)
                if path.exists():
                    shutil.rmtree(path, ignore_errors=True)
                removed_worktrees.append(str(path))
        self._git(repo, "worktree", "prune", check=False)

        if delete_branches:
            acceptance = self.store.get_collection_acceptance(collection["id"])
            if not acceptance:
                raise RuntimeError("Refusing branch cleanup before the collection has been accepted")
            target = acceptance["target_branch"]
            branches = [t.get("branch_name") for t in tasks if t.get("branch_name")]
            branches.append(collection.get("integration_branch"))
            for branch in branches:
                if not branch or branch == target or not self._branch_exists(repo, branch):
                    continue
                # Only delete branches whose tip is reachable from the accepted target.
                tip = self._git(repo, "rev-parse", branch).stdout.strip()
                target_head = self._git(repo, "rev-parse", target).stdout.strip()
                if self._is_ancestor(repo, tip, target_head):
                    proc = self._git(repo, "branch", "-d", branch, check=False)
                    if proc.returncode == 0:
                        removed_branches.append(branch)

        return {"worktrees": removed_worktrees, "branches": removed_branches}

