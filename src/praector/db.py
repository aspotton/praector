from __future__ import annotations

import contextlib
import fcntl
import json
import os
import subprocess
from importlib.resources import files
from pathlib import Path
from typing import Any, Iterator

from .config import GlobalConfig, ProjectConfig, PROFILE_NAMES, project_config_to_json
from .models import TaskStatus
from .planning_policy import (
    classify_delivery_policy,
    classify_planning_policy,
    material_plan_fingerprint,
    material_plan_fingerprint_v021,
    validate_deterministic_research_result,
    validate_external_conformance_review_result,
    validate_research_consumer_result,
    research_exception_decision_ids,
    research_consumer_exception_decision_ids,
)
from .util import dedupe_key, json_dumps, json_loads, make_id, require_binary, sql_literal, utcnow


class DependencyCycleError(ValueError):
    """A hard task dependency would create or preserve a cyclic graph."""


class DoltStore:
    """Small Dolt CLI-backed store.

    Praector currently serializes all Dolt CLI access with a filesystem lock. This keeps
    the implementation simple and reliable while still allowing multiple OpenCode
    worker processes to call the praector CLI concurrently.
    """

    def __init__(self, cfg: GlobalConfig):
        self.cfg = cfg
        self.state_dir = cfg.state_dir
        self.lock_file = cfg.lock_file
        self._dolt = require_binary("dolt")

    @contextlib.contextmanager
    def lock(self) -> Iterator[None]:
        self.cfg.home.mkdir(parents=True, exist_ok=True)
        with self.lock_file.open("a+") as fh:
            fcntl.flock(fh.fileno(), fcntl.LOCK_EX)
            try:
                yield
            finally:
                fcntl.flock(fh.fileno(), fcntl.LOCK_UN)

    def bootstrap(self) -> None:
        self.state_dir.mkdir(parents=True, exist_ok=True)
        with self.lock():
            if not (self.state_dir / ".dolt").exists():
                self._run([self._dolt, "init"], cwd=self.state_dir)
                # Local identity so semantic audit commits always work without
                # requiring the user's global Dolt config.
                self._run([self._dolt, "config", "--local", "--add", "user.name", "praector"], cwd=self.state_dir, check=False)
                self._run([self._dolt, "config", "--local", "--add", "user.email", "praector@local"], cwd=self.state_dir, check=False)
            schema = files("praector").joinpath("schema.sql").read_text(encoding="utf-8")
            self._run([self._dolt, "sql"], cwd=self.state_dir, input_text=schema)
            self._semantic_commit_locked("praector: initialize schema")
            self._repair_invariants_locked()

    def _repair_invariants_locked(self) -> None:
        """Repair invariant violations written by older Praector versions.

        These repairs are deliberately narrow and idempotent. They make durable
        history internally consistent without rewriting task/result semantics.
        """
        inconsistent = self._query_locked(
            "SELECT id FROM attempts WHERE status='SUCCEEDED' AND exit_code IS NOT NULL AND exit_code <> 0"
        )
        if inconsistent:
            self._mutate_locked(
                "UPDATE attempts SET status='FAILED' "
                "WHERE status='SUCCEEDED' AND exit_code IS NOT NULL AND exit_code <> 0"
            )
            self._semantic_commit_locked(
                f"praector: repair {len(inconsistent)} inconsistent attempt status(es)"
            )
        self._repair_revision_baseline_provenance_locked()
        self._repair_legacy_dependency_cycles_locked_all()

    @staticmethod
    def _dependency_cycle_in_edges(edges: list[tuple[str, str]]) -> list[str] | None:
        """Return one hard-dependency cycle as ``A -> ... -> A`` if present."""
        adjacency: dict[str, list[str]] = {}
        nodes: set[str] = set()
        for from_id, to_id in edges:
            adjacency.setdefault(from_id, []).append(to_id)
            nodes.add(from_id)
            nodes.add(to_id)
        visiting: set[str] = set()
        visited: set[str] = set()
        stack: list[str] = []
        stack_index: dict[str, int] = {}

        def visit(node: str) -> list[str] | None:
            if node in visited:
                return None
            if node in visiting:
                idx = stack_index[node]
                return stack[idx:] + [node]
            visiting.add(node)
            stack_index[node] = len(stack)
            stack.append(node)
            for nxt in adjacency.get(node, []):
                cycle = visit(nxt)
                if cycle is not None:
                    return cycle
            stack.pop()
            stack_index.pop(node, None)
            visiting.remove(node)
            visited.add(node)
            return None

        for node in sorted(nodes):
            cycle = visit(node)
            if cycle is not None:
                return cycle
        return None

    def _repair_legacy_dependency_cycles_locked_all(self) -> int:
        rows = self._query_locked("SELECT id FROM collections ORDER BY created_at")
        repaired = 0
        for row in rows:
            count, _ = self._repair_legacy_dependency_cycles_locked(str(row["id"]))
            repaired += count
        return repaired

    def _repair_legacy_dependency_cycles_locked(self, collection_id: str) -> tuple[int, list[str] | None]:
        """Repair only dependency-cycle shapes that older Praector versions are known to have emitted.

        Modern Praector rejects every new cyclic hard dependency. Older versions could
        persist two invalid pre-write edges: implementation -> plan_review and
        coordinator -> plan_review when the review already led back to that task.
        Those edges are semantically invalid because a final plan review evaluates
        READY implementation contracts and evidence; it must not wait for write
        execution or for a planner that is itself waiting on the review.

        Unknown cycle shapes are intentionally left untouched for collection-local
        quarantine by the scheduler rather than guessed at destructively.
        """
        task_rows = self._query_locked(
            "SELECT id,project_id,profile,status FROM tasks WHERE "
            f"collection_id={sql_literal(collection_id)}"
        )
        task_by_id = {str(row["id"]): row for row in task_rows}
        edges = self._dependency_edges_locked(collection_id)
        repairs: list[tuple[str, str, str]] = []

        # Search known-invalid edge shapes directly rather than stopping at the
        # first cycle discovered. A legacy Collection may contain both a repairable
        # planning cycle and a second unknown cycle; we still normalize the former
        # before quarantining the latter.
        while True:
            candidate: tuple[str, str, str] | None = None
            for from_id, to_id in list(edges):
                source = task_by_id.get(from_id, {})
                target = task_by_id.get(to_id, {})
                source_profile = str(source.get("profile") or "")
                target_profile = str(target.get("profile") or "")
                reason = ""
                if target_profile == "plan_review" and source_profile == "implement":
                    reason = "legacy_plan_review_waited_for_implementation"
                elif target_profile == "plan_review" and source_profile == "coordinator":
                    reason = "legacy_plan_review_waited_for_coordinator"
                if not reason:
                    continue
                # This edge is a cycle member only if the target already reaches
                # back to the source through the rest of the dependency graph.
                return_path = self._dependency_path_in_edges(edges, to_id, from_id)
                if return_path is None:
                    continue
                candidate = (from_id, to_id, reason)
                break
            if candidate is None:
                break
            from_id, to_id, reason = candidate
            edges = [edge for edge in edges if edge != (from_id, to_id)]
            repairs.append(candidate)

        unresolved_cycle = self._dependency_cycle_in_edges(edges)

        if repairs:
            now = utcnow()
            statements: list[str] = []
            project_id = str(next((row.get("project_id") for row in task_rows if row.get("project_id")), ""))
            for from_id, to_id, reason in repairs:
                statements.append(
                    "DELETE FROM task_edges WHERE "
                    f"collection_id={sql_literal(collection_id)} AND relation='depends_on' "
                    f"AND from_task_id={sql_literal(from_id)} AND to_task_id={sql_literal(to_id)}"
                )
                if project_id:
                    payload = json_dumps({
                        "from_task_id": from_id,
                        "to_task_id": to_id,
                        "reason": reason,
                    })
                    statements.append(
                        "INSERT INTO events (id,project_id,collection_id,task_id,attempt_id,event_type,payload_json,created_at) VALUES ("
                        f"{sql_literal(make_id('EVT'))},{sql_literal(project_id)},{sql_literal(collection_id)},"
                        f"{sql_literal(to_id)},NULL,'LEGACY_DEPENDENCY_CYCLE_REPAIRED',{sql_literal(payload)},{sql_literal(now)})"
                    )
            self._mutate_locked(";".join(statements))
            self._semantic_commit_locked(
                f"praector: repair {len(repairs)} legacy dependency cycle edge(s) in {collection_id}"
            )
        return len(repairs), unresolved_cycle

    def repair_legacy_dependency_cycles(self, collection_id: str) -> tuple[int, list[str] | None]:
        """Normalize recognized legacy hard-dependency cycles for one collection."""
        with self.lock():
            return self._repair_legacy_dependency_cycles_locked(collection_id)

    def _repair_revision_baseline_provenance_locked(self) -> int:
        """Correct false compatibility labels caused by the pre-0.1.24 seal race.

        0.1.22/0.1.23 could publish a plan-review row just before publishing
        its critic baseline. A concurrent scheduler could see that gap and write
        ``compatibility_plan_seal`` even for brand-new human revision work. We
        repair only cases whose lineage is provable: the critic and plan review
        were both requested by the coordinator recorded on a Collection revision.
        History stays append-only; a corrected baseline event supersedes the bad
        provenance label without deleting the original event.
        """
        rows = self._query_locked(
            "SELECT task_id,payload_json,created_at FROM events "
            "WHERE event_type='CRITIC_PLAN_BASELINE' ORDER BY created_at"
        )
        latest_by_critic: dict[str, dict[str, Any]] = {}
        for row in rows:
            critic_id = str(row.get("task_id") or "")
            if critic_id:
                latest_by_critic[critic_id] = row

        repairs: list[str] = []
        for critic_id, row in latest_by_critic.items():
            payload = json_loads(row.get("payload_json"), {})
            if not isinstance(payload, dict) or payload.get("source") != "compatibility_plan_seal":
                continue
            plan_review_id = str(payload.get("plan_review_task_id") or "")
            fingerprint = str(payload.get("fingerprint") or "")
            if not plan_review_id or not fingerprint:
                continue

            task_rows = self._query_locked(
                "SELECT id,collection_id,requested_by_task_id FROM tasks WHERE "
                f"id IN ({sql_literal(critic_id)},{sql_literal(plan_review_id)})"
            )
            tasks = {str(task.get("id") or ""): task for task in task_rows}
            critic = tasks.get(critic_id)
            plan_review = tasks.get(plan_review_id)
            if not critic or not plan_review:
                continue
            coordinator_id = str(critic.get("requested_by_task_id") or "")
            if not coordinator_id or str(plan_review.get("requested_by_task_id") or "") != coordinator_id:
                continue
            collection_id = str(critic.get("collection_id") or "")
            revision_rows = self._query_locked(
                "SELECT id,project_id,coordinator_task_id FROM collection_revisions WHERE "
                f"collection_id={sql_literal(collection_id)} AND coordinator_task_id={sql_literal(coordinator_id)} LIMIT 1"
            )
            if not revision_rows:
                continue
            revision = revision_rows[0]
            project_id = str(revision.get("project_id") or "")
            if not project_id:
                collection_rows = self._query_locked(
                    f"SELECT project_id FROM collections WHERE id={sql_literal(collection_id)} LIMIT 1"
                )
                project_id = str(collection_rows[0].get("project_id") or "") if collection_rows else ""
            if not project_id:
                continue

            now = utcnow()
            correction_payload = json_dumps({
                "critic_task_id": critic_id,
                "fingerprint": fingerprint,
                "source": "revision_plan_seal",
                "plan_review_task_id": plan_review_id,
                "corrected_from": "compatibility_plan_seal",
                "revision_id": revision.get("id"),
            })
            repairs.append(
                "INSERT INTO events (id,project_id,collection_id,task_id,attempt_id,event_type,payload_json,created_at) VALUES ("
                f"{sql_literal(make_id('EVT'))},{sql_literal(project_id)},{sql_literal(collection_id)},"
                f"{sql_literal(critic_id)},NULL,'CRITIC_PLAN_BASELINE',{sql_literal(correction_payload)},{sql_literal(now)})"
            )

        if repairs:
            self._mutate_locked(";".join(repairs))
            self._semantic_commit_locked(
                f"praector: repair {len(repairs)} revision critic baseline provenance label(s)"
            )
        return len(repairs)

    def _run(
        self,
        args: list[str],
        *,
        cwd: Path | None = None,
        input_text: str | None = None,
        check: bool = True,
    ) -> subprocess.CompletedProcess[str]:
        proc = subprocess.run(
            args,
            cwd=str(cwd or self.state_dir),
            text=True,
            input=input_text,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
        )
        if check and proc.returncode != 0:
            detail = proc.stderr.strip() or proc.stdout.strip()
            raise RuntimeError(f"Dolt command failed ({proc.returncode}): {' '.join(args)}\n{detail}")
        return proc

    def _query_locked(self, query: str) -> list[dict[str, Any]]:
        proc = self._run([self._dolt, "sql", "-r", "json", "-q", query], cwd=self.state_dir)
        text = proc.stdout.strip()
        if not text:
            return []
        try:
            payload = json.loads(text)
        except json.JSONDecodeError as exc:
            raise RuntimeError(f"Unable to parse Dolt JSON output for query:\n{query}\nOutput:\n{text}") from exc
        return list(payload.get("rows", []))

    def query(self, query: str) -> list[dict[str, Any]]:
        with self.lock():
            return self._query_locked(query)

    def _mutate_locked(self, query: str) -> None:
        self._run([self._dolt, "sql", "-q", query], cwd=self.state_dir)

    def mutate(self, query: str, commit_message: str | None = None) -> None:
        with self.lock():
            self._mutate_locked(query)
            if commit_message:
                self._semantic_commit_locked(commit_message)

    def _semantic_commit_locked(self, message: str) -> None:
        self._run([self._dolt, "add", "-A"], cwd=self.state_dir, check=False)
        proc = self._run([self._dolt, "commit", "-m", message], cwd=self.state_dir, check=False)
        if proc.returncode != 0:
            detail = (proc.stderr + "\n" + proc.stdout).lower()
            if "nothing to commit" not in detail and "no changes" not in detail:
                raise RuntimeError(f"Dolt commit failed: {proc.stderr.strip() or proc.stdout.strip()}")

    # ---------- Project ----------

    def register_project(self, cfg: ProjectConfig) -> dict[str, Any]:
        now = utcnow()
        existing = self.query(
            f"SELECT * FROM projects WHERE id={sql_literal(cfg.project_id)} OR root_path={sql_literal(str(cfg.root))} LIMIT 1"
        )
        config_json = json_dumps(project_config_to_json(cfg))
        if existing:
            current = existing[0]
            project_id = current["id"]
            if (
                current.get("name") == cfg.name
                and current.get("root_path") == str(cfg.root)
                and current.get("base_branch") == cfg.base_branch
                and current.get("config_json") == config_json
            ):
                return self.get_project(project_id)
            self.mutate(
                "UPDATE projects SET "
                f"name={sql_literal(cfg.name)}, root_path={sql_literal(str(cfg.root))}, "
                f"base_branch={sql_literal(cfg.base_branch)}, config_json={sql_literal(config_json)}, "
                f"updated_at={sql_literal(now)} WHERE id={sql_literal(project_id)}",
                f"project {project_id}: refresh configuration",
            )
            return self.get_project(project_id)
        self.mutate(
            "INSERT INTO projects (id,name,root_path,base_branch,config_json,created_at,updated_at) VALUES ("
            f"{sql_literal(cfg.project_id)},{sql_literal(cfg.name)},{sql_literal(str(cfg.root))},"
            f"{sql_literal(cfg.base_branch)},{sql_literal(config_json)},{sql_literal(now)},{sql_literal(now)})",
            f"project {cfg.project_id}: register {cfg.name}",
        )
        return self.get_project(cfg.project_id)

    def get_project(self, identifier: str) -> dict[str, Any]:
        rows = self.query(
            "SELECT * FROM projects WHERE "
            f"id={sql_literal(identifier)} OR name={sql_literal(identifier)} OR root_path={sql_literal(identifier)} LIMIT 1"
        )
        if not rows:
            raise KeyError(f"Project not found: {identifier}")
        row = rows[0]
        row["config"] = json_loads(row.get("config_json"), {})
        return row

    def list_projects(self) -> list[dict[str, Any]]:
        return self.query("SELECT * FROM projects ORDER BY created_at")

    # ---------- Collections ----------

    def create_collection(self, project_id: str, title: str, objective: str) -> dict[str, Any]:
        collection_id = make_id("COL")
        branch = f"praector/collections/{collection_id.lower()}"
        now = utcnow()
        self.mutate(
            "INSERT INTO collections "
            "(id,project_id,title,objective,status,integration_branch,created_at,updated_at) VALUES ("
            f"{sql_literal(collection_id)},{sql_literal(project_id)},{sql_literal(title)},"
            f"{sql_literal(objective)},'RUNNING',{sql_literal(branch)},{sql_literal(now)},{sql_literal(now)})",
            f"collection {collection_id}: create {title}",
        )
        self.event(project_id, collection_id, None, None, "COLLECTION_CREATED", {"title": title, "objective": objective})
        return self.get_collection(collection_id)

    def create_collection_with_coordinator(
        self,
        project_id: str,
        title: str,
        objective: str,
        coordinator_objective: str,
    ) -> tuple[dict[str, Any], dict[str, Any]]:
        """Create a collection and its required entrypoint task atomically.

        A RUNNING collection with zero tasks is invalid in Praector. Keeping these two
        inserts in one Dolt mutation prevents interrupted startup from leaving an
        orphan collection that the scheduler can never advance.
        """
        collection_id = make_id("COL")
        task_id = make_id("T")
        collection_event_id = make_id("EVT")
        task_event_id = make_id("EVT")
        branch = f"praector/collections/{collection_id.lower()}"
        now = utcnow()
        key = dedupe_key("coordinator", coordinator_objective)
        collection_payload = json_dumps({"title": title, "objective": objective})
        task_payload = json_dumps({"profile": "coordinator", "title": "Initial coordination", "reason": "Collection entry point"})
        with self.lock():
            self._mutate_locked(
                "INSERT INTO collections "
                "(id,project_id,title,objective,status,integration_branch,created_at,updated_at) VALUES ("
                f"{sql_literal(collection_id)},{sql_literal(project_id)},{sql_literal(title)},"
                f"{sql_literal(objective)},'RUNNING',{sql_literal(branch)},{sql_literal(now)},{sql_literal(now)});"
                "INSERT INTO tasks "
                "(id,project_id,collection_id,parent_task_id,requested_by_task_id,profile,title,objective,reason,status,"
                "priority,depth,workspace_mode,base_commit,dedupe_key,created_at,updated_at) VALUES ("
                f"{sql_literal(task_id)},{sql_literal(project_id)},{sql_literal(collection_id)},NULL,NULL,'coordinator',"
                f"'Initial coordination',{sql_literal(coordinator_objective)},'Collection entry point','READY',50,0,'collection',NULL,"
                f"{sql_literal(key)},{sql_literal(now)},{sql_literal(now)});"
                "INSERT INTO events (id,project_id,collection_id,task_id,attempt_id,event_type,payload_json,created_at) VALUES ("
                f"{sql_literal(collection_event_id)},{sql_literal(project_id)},{sql_literal(collection_id)},NULL,NULL,'COLLECTION_CREATED',"
                f"{sql_literal(collection_payload)},{sql_literal(now)});"
                "INSERT INTO events (id,project_id,collection_id,task_id,attempt_id,event_type,payload_json,created_at) VALUES ("
                f"{sql_literal(task_event_id)},{sql_literal(project_id)},{sql_literal(collection_id)},{sql_literal(task_id)},NULL,'TASK_CREATED',"
                f"{sql_literal(task_payload)},{sql_literal(now)})"
            )
            self._semantic_commit_locked(f"collection {collection_id}: create with coordinator {task_id}")
        return self.get_collection(collection_id), self.get_task(task_id)

    def ensure_collection_entrypoint(self, collection_id: str) -> dict[str, Any] | None:
        """Repair a legacy/interrupted RUNNING collection that has no tasks.

        pre-native releases could leave this state if collection workspace setup failed
        between collection creation and coordinator creation. The scheduler calls
        this before reconciliation so existing installations self-heal.
        """
        if self.count_tasks(collection_id) > 0:
            return None
        collection = self.get_collection(collection_id)
        if collection["status"] not in ("RUNNING", "BLOCKED"):
            return None
        coordinator_objective = (
            f"Own the initial decomposition of collection {collection_id}: {collection['objective']}. "
            "Consume the deterministic explore/critic preflight when present. Create durable plan_review/research/implement/review/advise tasks using praector as appropriate; request additional explore/critic work only when genuinely needed. "
            "Do not perform implementation or substantive specialist work yourself. Ensure implementation tasks are sequenced by dependencies; "
            "independent review is automatically enforced for implementation tasks."
        )
        task, _ = self.create_task(
            project_id=collection["project_id"], collection_id=collection_id,
            profile="coordinator", title="Initial coordination", objective=coordinator_objective,
            reason="Recovered missing collection entry point", workspace_mode="collection", depth=0, allow_dedupe=False,
        )
        self.set_collection_status(collection_id, "RUNNING", commit=True)
        self.event(collection["project_id"], collection_id, task["id"], None, "COLLECTION_ENTRYPOINT_RECOVERED", {"task_id": task["id"]})
        return task

    def get_collection(self, collection_id: str) -> dict[str, Any]:
        rows = self.query(f"SELECT * FROM collections WHERE id={sql_literal(collection_id)} LIMIT 1")
        if not rows:
            raise KeyError(f"Collection not found: {collection_id}")
        return rows[0]

    def list_collections(self, project_id: str | None = None) -> list[dict[str, Any]]:
        where = f" WHERE project_id={sql_literal(project_id)}" if project_id else ""
        return self.query(f"SELECT * FROM collections{where} ORDER BY created_at DESC")

    def list_collection_revisions(self, collection_id: str, status: str | None = None) -> list[dict[str, Any]]:
        where = f"collection_id={sql_literal(collection_id)}"
        if status:
            where += f" AND status={sql_literal(status)}"
        return self.query(
            f"SELECT * FROM collection_revisions WHERE {where} ORDER BY created_at"
        )

    def request_collection_revision(self, collection_id: str, reason: str) -> tuple[dict[str, Any], dict[str, Any]]:
        """Reopen a completed, unaccepted collection for mandatory operator-requested repair.

        Human acceptance feedback is durable work, not an informal note. The open
        revision prevents the collection from becoming COMPLETE again until at
        least one post-revision implementation is independently reviewed and all
        post-revision work is terminal.
        """
        reason = reason.strip()
        if not reason:
            raise ValueError("Revision reason must not be empty")
        collection = self.get_collection(collection_id)
        if collection["status"] != "COMPLETE":
            raise RuntimeError(f"Collection {collection_id} is {collection['status']}, not COMPLETE")
        if self.get_collection_acceptance(collection_id):
            raise RuntimeError(
                "Accepted collections are immutable. Create a new collection for follow-up changes instead of revising accepted history."
            )
        open_revisions = self.list_collection_revisions(collection_id, status="OPEN")
        if open_revisions:
            raise RuntimeError(f"Collection already has open revision {open_revisions[-1]['id']}")

        revision_id = make_id("REV")
        task_id = make_id("T")
        revision_event_id = make_id("EVT")
        task_event_id = make_id("EVT")
        now = utcnow()
        coordinator_objective = (
            f"Operator acceptance rejected completed collection {collection_id}. This is a mandatory revision, not a non-blocking suggestion. "
            f"Operator reason: {reason}\n\n"
            "Inspect the current integrated candidate and prior task/review results. Create the smallest bounded repair implementation work that "
            "directly resolves the operator reason, with executable regression coverage where practical. Do not merely document or downgrade the "
            "reported defect when it conflicts with the collection objective. Ensure repaired write work receives a fresh independent review. "
            "If the requested correction cannot be satisfied without changing the collection objective, submit a failed coordinator result explaining why."
        )
        key = dedupe_key("coordinator", coordinator_objective)
        revision_payload = json_dumps({"revision_id": revision_id, "reason": reason, "coordinator_task_id": task_id})
        task_payload = json_dumps({"profile": "coordinator", "title": "Acceptance revision", "reason": reason})
        with self.lock():
            self._mutate_locked(
                "INSERT INTO collection_revisions "
                "(id,project_id,collection_id,reason,status,coordinator_task_id,created_at) VALUES ("
                f"{sql_literal(revision_id)},{sql_literal(collection['project_id'])},{sql_literal(collection_id)},"
                f"{sql_literal(reason)},'OPEN',{sql_literal(task_id)},{sql_literal(now)});"
                "INSERT INTO tasks "
                "(id,project_id,collection_id,parent_task_id,requested_by_task_id,profile,title,objective,reason,status,"
                "priority,depth,workspace_mode,base_commit,dedupe_key,created_at,updated_at) VALUES ("
                f"{sql_literal(task_id)},{sql_literal(collection['project_id'])},{sql_literal(collection_id)},NULL,NULL,'coordinator',"
                f"'Acceptance revision',{sql_literal(coordinator_objective)},{sql_literal(reason)},'READY',50,0,'collection',NULL,"
                f"{sql_literal(key)},{sql_literal(now)},{sql_literal(now)});"
                f"UPDATE collections SET status='RUNNING',updated_at={sql_literal(now)} WHERE id={sql_literal(collection_id)};"
                "INSERT INTO events (id,project_id,collection_id,task_id,attempt_id,event_type,payload_json,created_at) VALUES ("
                f"{sql_literal(revision_event_id)},{sql_literal(collection['project_id'])},{sql_literal(collection_id)},"
                f"{sql_literal(task_id)},NULL,'COLLECTION_REVISION_REQUESTED',{sql_literal(revision_payload)},{sql_literal(now)});"
                "INSERT INTO events (id,project_id,collection_id,task_id,attempt_id,event_type,payload_json,created_at) VALUES ("
                f"{sql_literal(task_event_id)},{sql_literal(collection['project_id'])},{sql_literal(collection_id)},"
                f"{sql_literal(task_id)},NULL,'TASK_CREATED',{sql_literal(task_payload)},{sql_literal(now)})"
            )
            self._semantic_commit_locked(f"collection {collection_id}: open revision {revision_id}")
        revision = self.query(f"SELECT * FROM collection_revisions WHERE id={sql_literal(revision_id)} LIMIT 1")[0]
        return revision, self.get_task(task_id)

    def update_collection_workspace(self, collection_id: str, worktree: str) -> None:
        self.mutate(
            f"UPDATE collections SET integration_worktree={sql_literal(worktree)},updated_at={sql_literal(utcnow())} "
            f"WHERE id={sql_literal(collection_id)}"
        )

    def set_collection_status(self, collection_id: str, status: str, *, commit: bool = True) -> None:
        self.mutate(
            f"UPDATE collections SET status={sql_literal(status)},updated_at={sql_literal(utcnow())} "
            f"WHERE id={sql_literal(collection_id)}",
            f"collection {collection_id}: {status.lower()}" if commit else None,
        )

    # ---------- Tasks / graph ----------

    def count_tasks(self, collection_id: str) -> int:
        rows = self.query(f"SELECT COUNT(*) AS n FROM tasks WHERE collection_id={sql_literal(collection_id)}")
        return int(rows[0]["n"]) if rows else 0

    def child_count(self, task_id: str) -> int:
        rows = self.query(f"SELECT COUNT(*) AS n FROM tasks WHERE parent_task_id={sql_literal(task_id)}")
        return int(rows[0]["n"]) if rows else 0

    def create_task(
        self,
        *,
        project_id: str,
        collection_id: str,
        profile: str,
        title: str,
        objective: str,
        reason: str = "",
        parent_task_id: str | None = None,
        requested_by_task_id: str | None = None,
        depends_on: list[str] | None = None,
        workspace_mode: str = "collection",
        base_commit: str | None = None,
        priority: int = 50,
        depth: int = 0,
        allow_dedupe: bool = True,
        spawned_by_task_id: str | None = None,
        blocking_caller_task_id: str | None = None,
        gate_implement_tasks: bool = False,
        critic_baseline_task_id: str | None = None,
        critic_baseline_fingerprint: str | None = None,
        critic_baseline_source: str | None = None,
    ) -> tuple[dict[str, Any], bool]:
        """Create a task with its initial graph contract atomically.

        Worker processes and the scheduler run concurrently. A task must never be
        externally visible as READY before its initial depends_on/spawned_by/
        blocking-parent edges exist. Keep the row, initial edges, caller
        WAITING transition, and an optional critic-plan seal under one Dolt lock
        and one semantic commit. The critic seal must be published atomically with
        a final plan-review task so the scheduler cannot misclassify the tiny
        publication gap as an upgrade-compatibility seal.
        """
        if profile not in PROFILE_NAMES:
            raise ValueError(f"Unknown profile {profile!r}; expected one of: {', '.join(PROFILE_NAMES)}")
        key = dedupe_key(profile, objective)
        deps = list(dict.fromkeys(depends_on or []))
        baseline_parts = (critic_baseline_task_id, critic_baseline_fingerprint, critic_baseline_source)
        if any(part is not None for part in baseline_parts) and not all(part is not None for part in baseline_parts):
            raise ValueError("critic baseline task, fingerprint, and source must be provided together")
        now = utcnow()
        created = False

        with self.lock():
            dependency_refs = list(deps)
            if blocking_caller_task_id:
                dependency_refs.append(blocking_caller_task_id)
            self._assert_dependency_tasks_in_collection_locked(
                collection_id, dependency_refs
            )

            existing: list[dict[str, Any]] = []
            if allow_dedupe:
                existing = self._query_locked(
                    "SELECT * FROM tasks WHERE "
                    f"collection_id={sql_literal(collection_id)} AND dedupe_key={sql_literal(key)} "
                    "AND status NOT IN ('CANCELLED','SUPERSEDED') ORDER BY created_at LIMIT 1"
                )

            if existing:
                task_id = existing[0]["id"]
                prospective_dependencies = [(dep, task_id) for dep in deps]
                if blocking_caller_task_id and existing[0].get("status") != TaskStatus.COMPLETE.value:
                    prospective_dependencies.append((task_id, blocking_caller_task_id))
                if gate_implement_tasks:
                    implement_rows = self._query_locked(
                        "SELECT id FROM tasks WHERE "
                        f"collection_id={sql_literal(collection_id)} AND profile='implement' "
                        "AND status NOT IN ('CANCELLED','SUPERSEDED','COMPLETE')"
                    )
                    prospective_dependencies.extend(
                        (task_id, str(row["id"])) for row in implement_rows
                    )
                self._assert_dependency_edges_acyclic_locked(
                    collection_id, prospective_dependencies
                )
                statements: list[str] = []
                for dep in deps:
                    statements.append(
                        "INSERT IGNORE INTO task_edges (from_task_id,to_task_id,collection_id,relation,created_at) VALUES ("
                        f"{sql_literal(dep)},{sql_literal(task_id)},{sql_literal(collection_id)},'depends_on',{sql_literal(now)})"
                    )
                if spawned_by_task_id:
                    statements.append(
                        "INSERT IGNORE INTO task_edges (from_task_id,to_task_id,collection_id,relation,created_at) VALUES ("
                        f"{sql_literal(spawned_by_task_id)},{sql_literal(task_id)},{sql_literal(collection_id)},'spawned_by',{sql_literal(now)})"
                    )
                if blocking_caller_task_id and existing[0].get("status") != TaskStatus.COMPLETE.value:
                    statements.append(
                        "INSERT IGNORE INTO task_edges (from_task_id,to_task_id,collection_id,relation,created_at) VALUES ("
                        f"{sql_literal(task_id)},{sql_literal(blocking_caller_task_id)},{sql_literal(collection_id)},'depends_on',{sql_literal(now)})"
                    )
                    statements.append(
                        "UPDATE tasks SET status='WAITING',updated_at="
                        f"{sql_literal(now)} WHERE id={sql_literal(blocking_caller_task_id)}"
                    )
                if gate_implement_tasks:
                    statements.append(
                        "INSERT IGNORE INTO task_edges (from_task_id,to_task_id,collection_id,relation,created_at) "
                        "SELECT "
                        f"{sql_literal(task_id)},t.id,{sql_literal(collection_id)},'depends_on',{sql_literal(now)} "
                        "FROM tasks t WHERE "
                        f"t.collection_id={sql_literal(collection_id)} AND t.profile='implement' "
                        "AND t.status NOT IN ('CANCELLED','SUPERSEDED','COMPLETE')"
                    )
                if critic_baseline_task_id is not None:
                    baseline_exists = self._query_locked(
                        "SELECT 1 AS ok FROM events WHERE "
                        f"task_id={sql_literal(critic_baseline_task_id)} AND event_type='CRITIC_PLAN_BASELINE' LIMIT 1"
                    )
                    if not baseline_exists:
                        baseline_event_id = make_id("EVT")
                        baseline_payload = json_dumps({
                            "critic_task_id": critic_baseline_task_id,
                            "fingerprint": critic_baseline_fingerprint,
                            "source": critic_baseline_source,
                            "plan_review_task_id": task_id,
                        })
                        statements.append(
                            "INSERT INTO events (id,project_id,collection_id,task_id,attempt_id,event_type,payload_json,created_at) VALUES ("
                            f"{sql_literal(baseline_event_id)},{sql_literal(project_id)},{sql_literal(collection_id)},"
                            f"{sql_literal(critic_baseline_task_id)},NULL,'CRITIC_PLAN_BASELINE',{sql_literal(baseline_payload)},{sql_literal(now)})"
                        )
                if statements:
                    self._mutate_locked(";".join(statements))
                    self._semantic_commit_locked(f"task {task_id}: attach initial graph contract")
            else:
                task_id = make_id("T")
                prospective_dependencies = [(dep, task_id) for dep in deps]
                if blocking_caller_task_id:
                    prospective_dependencies.append((task_id, blocking_caller_task_id))
                if gate_implement_tasks:
                    implement_rows = self._query_locked(
                        "SELECT id FROM tasks WHERE "
                        f"collection_id={sql_literal(collection_id)} AND profile='implement' "
                        "AND status NOT IN ('CANCELLED','SUPERSEDED','COMPLETE')"
                    )
                    prospective_dependencies.extend(
                        (task_id, str(row["id"])) for row in implement_rows
                    )
                self._assert_dependency_edges_acyclic_locked(
                    collection_id, prospective_dependencies
                )
                statements = [
                    "INSERT INTO tasks "
                    "(id,project_id,collection_id,parent_task_id,requested_by_task_id,profile,title,objective,reason,status,"
                    "priority,depth,workspace_mode,base_commit,dedupe_key,created_at,updated_at) VALUES ("
                    f"{sql_literal(task_id)},{sql_literal(project_id)},{sql_literal(collection_id)},"
                    f"{sql_literal(parent_task_id)},{sql_literal(requested_by_task_id)},{sql_literal(profile)},"
                    f"{sql_literal(title)},{sql_literal(objective)},{sql_literal(reason)},'READY',{priority},{depth},"
                    f"{sql_literal(workspace_mode)},{sql_literal(base_commit)},{sql_literal(key)},"
                    f"{sql_literal(now)},{sql_literal(now)})"
                ]
                for dep in deps:
                    statements.append(
                        "INSERT IGNORE INTO task_edges (from_task_id,to_task_id,collection_id,relation,created_at) VALUES ("
                        f"{sql_literal(dep)},{sql_literal(task_id)},{sql_literal(collection_id)},'depends_on',{sql_literal(now)})"
                    )
                if spawned_by_task_id:
                    statements.append(
                        "INSERT IGNORE INTO task_edges (from_task_id,to_task_id,collection_id,relation,created_at) VALUES ("
                        f"{sql_literal(spawned_by_task_id)},{sql_literal(task_id)},{sql_literal(collection_id)},'spawned_by',{sql_literal(now)})"
                    )
                if blocking_caller_task_id:
                    statements.append(
                        "INSERT IGNORE INTO task_edges (from_task_id,to_task_id,collection_id,relation,created_at) VALUES ("
                        f"{sql_literal(task_id)},{sql_literal(blocking_caller_task_id)},{sql_literal(collection_id)},'depends_on',{sql_literal(now)})"
                    )
                    statements.append(
                        "UPDATE tasks SET status='WAITING',updated_at="
                        f"{sql_literal(now)} WHERE id={sql_literal(blocking_caller_task_id)}"
                    )
                if gate_implement_tasks:
                    statements.append(
                        "INSERT IGNORE INTO task_edges (from_task_id,to_task_id,collection_id,relation,created_at) "
                        "SELECT "
                        f"{sql_literal(task_id)},t.id,{sql_literal(collection_id)},'depends_on',{sql_literal(now)} "
                        "FROM tasks t WHERE "
                        f"t.collection_id={sql_literal(collection_id)} AND t.profile='implement' "
                        "AND t.status NOT IN ('CANCELLED','SUPERSEDED','COMPLETE')"
                    )
                if critic_baseline_task_id is not None:
                    baseline_exists = self._query_locked(
                        "SELECT 1 AS ok FROM events WHERE "
                        f"task_id={sql_literal(critic_baseline_task_id)} AND event_type='CRITIC_PLAN_BASELINE' LIMIT 1"
                    )
                    if not baseline_exists:
                        baseline_event_id = make_id("EVT")
                        baseline_payload = json_dumps({
                            "critic_task_id": critic_baseline_task_id,
                            "fingerprint": critic_baseline_fingerprint,
                            "source": critic_baseline_source,
                            "plan_review_task_id": task_id,
                        })
                        statements.append(
                            "INSERT INTO events (id,project_id,collection_id,task_id,attempt_id,event_type,payload_json,created_at) VALUES ("
                            f"{sql_literal(baseline_event_id)},{sql_literal(project_id)},{sql_literal(collection_id)},"
                            f"{sql_literal(critic_baseline_task_id)},NULL,'CRITIC_PLAN_BASELINE',{sql_literal(baseline_payload)},{sql_literal(now)})"
                        )
                self._mutate_locked(";".join(statements))
                self._semantic_commit_locked(f"task {task_id}: create {profile} task with initial graph contract")
                created = True

        if created:
            self.event(project_id, collection_id, task_id, None, "TASK_CREATED", {"profile": profile, "title": title, "reason": reason})
        return self.get_task(task_id), created

    def _attach_existing_plan_review_handoff(
        self,
        *,
        plan_review_task_id: str,
        caller_task_id: str,
        depends_on: list[str],
        critic_baseline_task_id: str | None = None,
        critic_baseline_fingerprint: str | None = None,
        critic_baseline_source: str | None = None,
    ) -> dict[str, Any]:
        """Atomically reuse one nonterminal final plan-review gate.

        A coordinator handoff must not manufacture parallel final review
        generations merely because the requested title/objective differs. The
        reviewer evaluates the current durable graph, so an existing non-failed
        nonterminal plan_review can absorb new evidence prerequisites, block the
        caller, and gate all current implementations.
        """
        review = self.get_task(plan_review_task_id)
        caller = self.get_task(caller_task_id)
        if review["collection_id"] != caller["collection_id"]:
            raise ValueError("Plan review and caller must belong to the same collection")
        if review.get("profile") != "plan_review":
            raise ValueError("Reusable final gate must be a plan_review task")
        if review.get("status") in {
            TaskStatus.COMPLETE.value, TaskStatus.FAILED.value, TaskStatus.CANCELLED.value,
            TaskStatus.SUPERSEDED.value,
        }:
            raise ValueError("Cannot reuse a terminal plan_review task")

        baseline_parts = (critic_baseline_task_id, critic_baseline_fingerprint, critic_baseline_source)
        if any(part is not None for part in baseline_parts) and not all(part is not None for part in baseline_parts):
            raise ValueError("critic baseline task, fingerprint, and source must be provided together")

        collection_id = str(review["collection_id"])
        project_id = str(review["project_id"])
        now = utcnow()
        deps = list(dict.fromkeys(depends_on))
        with self.lock():
            self._assert_dependency_tasks_in_collection_locked(collection_id, deps)
            implement_rows = self._query_locked(
                "SELECT id FROM tasks WHERE "
                f"collection_id={sql_literal(collection_id)} AND profile='implement' "
                "AND status NOT IN ('CANCELLED','SUPERSEDED','COMPLETE')"
            )
            existing_edges = self._dependency_edges_locked(collection_id)
            caller_is_review_prerequisite = (caller_task_id, plan_review_task_id) in existing_edges

            # Reusing a review that currently waits for this active coordinator
            # is the final handoff point. Flip that edge atomically: before the
            # request the review waits for the coordinator; after the request the
            # coordinator waits for the review. Validating against the graph with
            # the old direction removed prevents the temporary two-edge cycle
            # that a naive add-then-delete sequence would create.
            validation_edges = [
                edge for edge in existing_edges
                if not (caller_is_review_prerequisite and edge == (caller_task_id, plan_review_task_id))
            ]
            prospective = [(dep, plan_review_task_id) for dep in deps]
            if caller.get("status") != TaskStatus.COMPLETE.value:
                prospective.append((plan_review_task_id, caller_task_id))
            prospective.extend((plan_review_task_id, str(row["id"])) for row in implement_rows)
            for from_id, to_id in prospective:
                if (from_id, to_id) in validation_edges:
                    continue
                path = self._dependency_path_in_edges(validation_edges, to_id, from_id)
                if path is not None:
                    cycle = " -> ".join([from_id] + path)
                    raise DependencyCycleError(
                        f"Dependency {from_id}->{to_id} would create a cycle in {collection_id}: {cycle}"
                    )
                validation_edges.append((from_id, to_id))

            statements: list[str] = []
            if caller_is_review_prerequisite:
                statements.append(
                    "DELETE FROM task_edges WHERE "
                    f"collection_id={sql_literal(collection_id)} AND relation='depends_on' "
                    f"AND from_task_id={sql_literal(caller_task_id)} AND to_task_id={sql_literal(plan_review_task_id)}"
                )
            for dep in deps:
                # The caller prerequisite, if it existed, is being flipped into
                # the blocking handoff and must not be reinserted here.
                if dep == caller_task_id:
                    continue
                statements.append(
                    "INSERT IGNORE INTO task_edges (from_task_id,to_task_id,collection_id,relation,created_at) VALUES ("
                    f"{sql_literal(dep)},{sql_literal(plan_review_task_id)},{sql_literal(collection_id)},'depends_on',{sql_literal(now)})"
                )
            if caller.get("status") != TaskStatus.COMPLETE.value:
                statements.append(
                    "INSERT IGNORE INTO task_edges (from_task_id,to_task_id,collection_id,relation,created_at) VALUES ("
                    f"{sql_literal(plan_review_task_id)},{sql_literal(caller_task_id)},{sql_literal(collection_id)},'depends_on',{sql_literal(now)})"
                )
                statements.append(
                    "UPDATE tasks SET status='WAITING',updated_at="
                    f"{sql_literal(now)} WHERE id={sql_literal(caller_task_id)}"
                )
            statements.append(
                "INSERT IGNORE INTO task_edges (from_task_id,to_task_id,collection_id,relation,created_at) "
                "SELECT "
                f"{sql_literal(plan_review_task_id)},t.id,{sql_literal(collection_id)},'depends_on',{sql_literal(now)} "
                "FROM tasks t WHERE "
                f"t.collection_id={sql_literal(collection_id)} AND t.profile='implement' "
                "AND t.status NOT IN ('CANCELLED','SUPERSEDED','COMPLETE')"
            )
            if critic_baseline_task_id is not None:
                baseline_exists = self._query_locked(
                    "SELECT 1 AS ok FROM events WHERE "
                    f"task_id={sql_literal(critic_baseline_task_id)} AND event_type='CRITIC_PLAN_BASELINE' LIMIT 1"
                )
                if not baseline_exists:
                    payload = json_dumps({
                        "critic_task_id": critic_baseline_task_id,
                        "fingerprint": critic_baseline_fingerprint,
                        "source": critic_baseline_source,
                        "plan_review_task_id": plan_review_task_id,
                    })
                    statements.append(
                        "INSERT INTO events (id,project_id,collection_id,task_id,attempt_id,event_type,payload_json,created_at) VALUES ("
                        f"{sql_literal(make_id('EVT'))},{sql_literal(project_id)},{sql_literal(collection_id)},"
                        f"{sql_literal(critic_baseline_task_id)},NULL,'CRITIC_PLAN_BASELINE',{sql_literal(payload)},{sql_literal(now)})"
                    )
            if statements:
                self._mutate_locked(";".join(statements))
                self._semantic_commit_locked(
                    f"task {plan_review_task_id}: reuse final plan-review handoff for {caller_task_id}"
                )
        return self.get_task(plan_review_task_id)

    def get_task(self, task_id: str) -> dict[str, Any]:
        rows = self.query(f"SELECT * FROM tasks WHERE id={sql_literal(task_id)} LIMIT 1")
        if not rows:
            raise KeyError(f"Task not found: {task_id}")
        row = rows[0]
        row["result"] = json_loads(row.get("result_json"), {})
        return row

    def list_tasks(self, collection_id: str) -> list[dict[str, Any]]:
        rows = self.query(
            f"SELECT * FROM tasks WHERE collection_id={sql_literal(collection_id)} ORDER BY created_at"
        )
        for row in rows:
            row["result"] = json_loads(row.get("result_json"), {})
        return rows

    def _dependency_edges_locked(self, collection_id: str) -> list[tuple[str, str]]:
        rows = self._query_locked(
            "SELECT from_task_id,to_task_id FROM task_edges WHERE "
            f"collection_id={sql_literal(collection_id)} AND relation='depends_on'"
        )
        return [(str(row["from_task_id"]), str(row["to_task_id"])) for row in rows]

    @staticmethod
    def _dependency_path_in_edges(
        edges: list[tuple[str, str]], start_task_id: str, target_task_id: str
    ) -> list[str] | None:
        """Return one dependency-edge path from start to target, if present."""
        if start_task_id == target_task_id:
            return [start_task_id]
        adjacency: dict[str, list[str]] = {}
        for from_id, to_id in edges:
            adjacency.setdefault(from_id, []).append(to_id)
        queue: list[tuple[str, list[str]]] = [(start_task_id, [start_task_id])]
        seen = {start_task_id}
        while queue:
            current, path = queue.pop(0)
            for next_id in adjacency.get(current, []):
                if next_id == target_task_id:
                    return path + [next_id]
                if next_id in seen:
                    continue
                seen.add(next_id)
                queue.append((next_id, path + [next_id]))
        return None

    def _assert_dependency_edges_acyclic_locked(
        self, collection_id: str, prospective_edges: list[tuple[str, str]]
    ) -> None:
        """Reject any prospective hard dependency that would make the DAG cyclic."""
        edges = self._dependency_edges_locked(collection_id)
        for from_id, to_id in prospective_edges:
            if (from_id, to_id) in edges:
                continue
            path = self._dependency_path_in_edges(edges, to_id, from_id)
            if path is not None:
                cycle = " -> ".join([from_id] + path)
                raise DependencyCycleError(
                    f"Dependency {from_id}->{to_id} would create a cycle in {collection_id}: {cycle}"
                )
            edges.append((from_id, to_id))

    def _assert_dependency_tasks_in_collection_locked(
        self, collection_id: str, task_ids: list[str]
    ) -> None:
        """Reject unresolved or cross-Collection hard-dependency references.

        Runnable-task queries join dependency edges back to task rows. Without
        this guard, a dangling dependency edge can disappear from scheduler
        gating and make work claimable even though its intended prerequisite is
        not real.
        """
        for task_id in dict.fromkeys(task_ids):
            if not task_id:
                continue
            rows = self._query_locked(
                "SELECT id,collection_id FROM tasks WHERE "
                f"id={sql_literal(task_id)} LIMIT 1"
            )
            if not rows:
                raise ValueError(f"Dependency task not found: {task_id}")
            actual_collection = str(rows[0].get("collection_id") or "")
            if actual_collection != collection_id:
                raise ValueError(
                    f"Dependency task {task_id} belongs to collection "
                    f"{actual_collection}, not {collection_id}"
                )

    def dependency_path(self, from_task_id: str, to_task_id: str) -> list[str] | None:
        """Return one existing hard-dependency path in edge direction, if any."""
        source = self.get_task(from_task_id)
        target = self.get_task(to_task_id)
        if source["collection_id"] != target["collection_id"]:
            return None
        with self.lock():
            edges = self._dependency_edges_locked(source["collection_id"])
        return self._dependency_path_in_edges(edges, from_task_id, to_task_id)

    def add_edge(self, from_task_id: str, to_task_id: str, collection_id: str, relation: str = "depends_on") -> None:
        now = utcnow()
        if relation != "depends_on":
            self.mutate(
                "INSERT IGNORE INTO task_edges (from_task_id,to_task_id,collection_id,relation,created_at) VALUES ("
                f"{sql_literal(from_task_id)},{sql_literal(to_task_id)},{sql_literal(collection_id)},"
                f"{sql_literal(relation)},{sql_literal(now)})",
                f"graph {collection_id}: {relation} {from_task_id}->{to_task_id}",
            )
            return
        with self.lock():
            self._assert_dependency_tasks_in_collection_locked(
                collection_id, [from_task_id, to_task_id]
            )
            self._assert_dependency_edges_acyclic_locked(
                collection_id, [(from_task_id, to_task_id)]
            )
            self._mutate_locked(
                "INSERT IGNORE INTO task_edges (from_task_id,to_task_id,collection_id,relation,created_at) VALUES ("
                f"{sql_literal(from_task_id)},{sql_literal(to_task_id)},{sql_literal(collection_id)},"
                f"{sql_literal(relation)},{sql_literal(now)})"
            )
            self._semantic_commit_locked(
                f"graph {collection_id}: {relation} {from_task_id}->{to_task_id}"
            )

    def list_edges(self, collection_id: str) -> list[dict[str, Any]]:
        return self.query(
            f"SELECT * FROM task_edges WHERE collection_id={sql_literal(collection_id)} ORDER BY created_at"
        )

    def material_plan_fingerprint(self, collection_id: str, *, legacy_raw_base_commit: bool = False) -> str:
        collection = self.get_collection(collection_id)
        return material_plan_fingerprint(
            str(collection.get("objective") or ""),
            self.list_tasks(collection_id),
            self.list_edges(collection_id),
            legacy_raw_base_commit=legacy_raw_base_commit,
        )

    def critic_revalidation_state(self, collection_id: str) -> dict[str, Any]:
        """Return the current durable critic-freshness state for a collection."""
        tasks = self.list_tasks(collection_id)
        critics = [
            task for task in tasks
            if task.get("profile") == "critic"
            and task.get("status") not in {TaskStatus.CANCELLED.value, TaskStatus.SUPERSEDED.value}
        ]
        latest = critics[-1] if critics else None
        current = self.material_plan_fingerprint(collection_id)
        legacy_current = self.material_plan_fingerprint(
            collection_id, legacy_raw_base_commit=True
        )
        collection = self.get_collection(collection_id)
        edges = self.list_edges(collection_id)
        v021_current = material_plan_fingerprint_v021(
            str(collection.get("objective") or ""), tasks, edges
        )
        v021_legacy_current = material_plan_fingerprint_v021(
            str(collection.get("objective") or ""), tasks, edges, legacy_raw_base_commit=True
        )
        baseline = self.critic_plan_baseline(latest["id"]) if latest else None
        sealed = baseline.get("fingerprint") if baseline else None
        compatible_via = None
        if sealed and sealed != current:
            if sealed == legacy_current:
                compatible_via = "legacy_raw_base_commit"
            elif sealed == v021_current:
                compatible_via = "v0.2.1_material_plan"
            elif sealed == v021_legacy_current:
                compatible_via = "v0.2.1_legacy_raw_base_commit"
        stale = bool(latest and sealed and sealed != current and not compatible_via)
        return {
            "latest_critic_task_id": latest.get("id") if latest else None,
            "latest_critic_status": latest.get("status") if latest else None,
            "current_material_plan_fingerprint": current,
            "sealed_fingerprint": sealed,
            "baseline_source": baseline.get("source") if baseline else None,
            "compatible_via": compatible_via,
            "stale": stale,
        }

    def critic_plan_baseline(self, critic_task_id: str) -> dict[str, Any] | None:
        rows = self.query(
            "SELECT payload_json,created_at FROM events WHERE "
            f"task_id={sql_literal(critic_task_id)} AND event_type='CRITIC_PLAN_BASELINE' "
            "ORDER BY created_at DESC LIMIT 1"
        )
        if not rows:
            return None
        payload = json_loads(rows[0].get("payload_json"), {})
        if not isinstance(payload, dict):
            return None
        payload = dict(payload)
        payload.setdefault("created_at", rows[0].get("created_at"))
        return payload

    def record_critic_plan_baseline(
        self,
        critic_task_id: str,
        fingerprint: str,
        *,
        source: str,
        plan_review_task_id: str | None = None,
    ) -> None:
        critic = self.get_task(critic_task_id)
        self.event(
            critic["project_id"],
            critic["collection_id"],
            critic_task_id,
            None,
            "CRITIC_PLAN_BASELINE",
            {
                "critic_task_id": critic_task_id,
                "fingerprint": fingerprint,
                "source": source,
                "plan_review_task_id": plan_review_task_id,
            },
        )

    def _material_plan_fingerprint_locked(
        self, collection_id: str, objective: str, *, legacy_raw_base_commit: bool = False
    ) -> str:
        tasks = self._query_locked(
            f"SELECT * FROM tasks WHERE collection_id={sql_literal(collection_id)} ORDER BY created_at,id"
        )
        edges = self._query_locked(
            f"SELECT * FROM task_edges WHERE collection_id={sql_literal(collection_id)} ORDER BY created_at"
        )
        return material_plan_fingerprint(
            objective, tasks, edges, legacy_raw_base_commit=legacy_raw_base_commit
        )

    def _critic_plan_baseline_locked(self, critic_task_id: str) -> dict[str, Any] | None:
        rows = self._query_locked(
            "SELECT payload_json,created_at FROM events WHERE "
            f"task_id={sql_literal(critic_task_id)} AND event_type='CRITIC_PLAN_BASELINE' "
            "ORDER BY created_at DESC LIMIT 1"
        )
        if not rows:
            return None
        payload = json_loads(rows[0].get("payload_json"), {})
        if not isinstance(payload, dict):
            return None
        payload = dict(payload)
        payload.setdefault("created_at", rows[0].get("created_at"))
        return payload

    def dependencies(self, task_id: str) -> list[dict[str, Any]]:
        return self.query(
            "SELECT t.* FROM task_edges e JOIN tasks t ON t.id=e.from_task_id "
            f"WHERE e.to_task_id={sql_literal(task_id)} AND e.relation='depends_on' ORDER BY e.created_at"
        )

    def research_ancestors(self, task_id: str) -> list[dict[str, Any]]:
        """Return completed research Tasks reachable through hard dependency lineage."""
        seen = {task_id}
        queue = [task_id]
        by_id: dict[str, dict[str, Any]] = {}
        while queue:
            current = queue.pop(0)
            for dep in self.dependencies(current):
                dep_id = str(dep.get("id") or "")
                if not dep_id:
                    continue
                if dep.get("profile") == "research" and dep.get("status") == TaskStatus.COMPLETE.value:
                    dep = dict(dep)
                    dep["result"] = json_loads(dep.get("result_json") or "{}")
                    by_id[dep_id] = dep
                if dep_id not in seen:
                    seen.add(dep_id)
                    queue.append(dep_id)
        return sorted(by_id.values(), key=lambda row: (str(row.get("created_at") or ""), str(row.get("id") or "")))

    def _research_ancestors_locked(self, task_id: str) -> list[dict[str, Any]]:
        """Locked claim-time variant of research_ancestors."""
        seen = {task_id}
        queue = [task_id]
        by_id: dict[str, dict[str, Any]] = {}
        while queue:
            current = queue.pop(0)
            deps = self._query_locked(
                "SELECT t.* FROM task_edges e JOIN tasks t ON t.id=e.from_task_id "
                f"WHERE e.to_task_id={sql_literal(current)} AND e.relation='depends_on' ORDER BY e.created_at"
            )
            for dep in deps:
                dep_id = str(dep.get("id") or "")
                if not dep_id:
                    continue
                if dep.get("profile") == "research" and dep.get("status") == TaskStatus.COMPLETE.value:
                    dep = dict(dep)
                    dep["result"] = json_loads(dep.get("result_json") or "{}")
                    by_id[dep_id] = dep
                if dep_id not in seen:
                    seen.add(dep_id)
                    queue.append(dep_id)
        return sorted(by_id.values(), key=lambda row: (str(row.get("created_at") or ""), str(row.get("id") or "")))

    def dependents(self, task_id: str) -> list[dict[str, Any]]:
        return self.query(
            "SELECT t.* FROM task_edges e JOIN tasks t ON t.id=e.to_task_id "
            f"WHERE e.from_task_id={sql_literal(task_id)} AND e.relation='depends_on' ORDER BY e.created_at"
        )

    def find_related_review(self, implement_task_id: str) -> dict[str, Any] | None:
        rows = self.query(
            "SELECT t.* FROM task_edges e JOIN tasks t ON t.id=e.to_task_id "
            f"WHERE e.from_task_id={sql_literal(implement_task_id)} AND e.relation='depends_on' "
            "AND t.profile='review' AND t.status NOT IN ('CANCELLED','SUPERSEDED') LIMIT 1"
        )
        return rows[0] if rows else None

    def add_dependency(self, task_id: str, dependency_id: str) -> None:
        task = self.get_task(task_id)
        dep = self.get_task(dependency_id)
        if task["collection_id"] != dep["collection_id"]:
            raise ValueError("Dependencies must belong to the same collection in the current release")
        self.add_edge(dependency_id, task_id, task["collection_id"], "depends_on")

    def remove_dependency(self, task_id: str, dependency_id: str) -> None:
        """Remove one dependency edge while preserving all other graph relations."""
        task = self.get_task(task_id)
        dep = self.get_task(dependency_id)
        if task["collection_id"] != dep["collection_id"]:
            raise ValueError("Dependencies must belong to the same collection in the current release")
        self.mutate(
            "DELETE FROM task_edges WHERE "
            f"from_task_id={sql_literal(dependency_id)} AND to_task_id={sql_literal(task_id)} "
            "AND relation='depends_on'",
            f"graph {task['collection_id']}: remove dependency {dependency_id}->{task_id}",
        )

    def supersede_task(self, old_task_id: str, replacement_task_id: str, reason: str) -> None:
        old = self.get_task(old_task_id)
        replacement = self.get_task(replacement_task_id)
        if old["collection_id"] != replacement["collection_id"]:
            raise ValueError("Replacement task must belong to the same collection")
        if old_task_id == replacement_task_id:
            raise ValueError("A task cannot supersede itself")
        dependents = self.query(
            "SELECT to_task_id FROM task_edges "
            f"WHERE from_task_id={sql_literal(old_task_id)} AND relation='depends_on'"
        )
        # Rewire hard dependencies to the replacement, then preserve lineage.
        for row in dependents:
            dependent_id = row["to_task_id"]
            try:
                self.add_edge(replacement_task_id, dependent_id, old["collection_id"], "depends_on")
            except ValueError:
                if old.get("profile") != "plan_review" or replacement.get("profile") != "plan_review":
                    raise
                # Replacing an obsolete planning gate may encounter a dependent
                # already wired into the surviving gate. Skip only that
                # orchestration edge; execution-task supersession remains strict.
                self.event(
                    old["project_id"], old["collection_id"], old_task_id, None,
                    "DEPENDENCY_CYCLE_REJECTED",
                    {"from_task_id": replacement_task_id, "to_task_id": dependent_id, "context": "supersede_rewire"},
                )

        # A coordinator may replace a protocol-broken reviewer with a fresh
        # reviewer. Preserve both its prerequisites and its verification target
        # so collection completion/acceptance still proves which implementation
        # was independently verified. Generic task replacement intentionally does
        # not inherit prerequisites; this is specific to same-role review gates.
        if old.get("profile") == replacement.get("profile") and old.get("profile") in {"review", "plan_review"}:
            prerequisites = self.query(
                "SELECT from_task_id FROM task_edges "
                f"WHERE to_task_id={sql_literal(old_task_id)} AND relation='depends_on'"
            )
            for row in prerequisites:
                try:
                    self.add_edge(row["from_task_id"], replacement_task_id, old["collection_id"], "depends_on")
                except ValueError:
                    if old.get("profile") != "plan_review":
                        raise
                    self.event(
                        old["project_id"], old["collection_id"], old_task_id, None,
                        "DEPENDENCY_CYCLE_REJECTED",
                        {
                            "from_task_id": row["from_task_id"],
                            "to_task_id": replacement_task_id,
                            "context": "supersede_prerequisite",
                        },
                    )
            if old.get("profile") == "review":
                verification_sources = self.query(
                    "SELECT from_task_id FROM task_edges "
                    f"WHERE to_task_id={sql_literal(old_task_id)} AND relation='verifies'"
                )
                for row in verification_sources:
                    self.add_edge(row["from_task_id"], replacement_task_id, old["collection_id"], "verifies")

        self.mutate(
            f"DELETE FROM task_edges WHERE from_task_id={sql_literal(old_task_id)} AND relation='depends_on';"
            f"UPDATE tasks SET status='SUPERSEDED',updated_at={sql_literal(utcnow())} WHERE id={sql_literal(old_task_id)}",
            f"task {old_task_id}: superseded by {replacement_task_id}",
        )
        self.add_edge(old_task_id, replacement_task_id, old["collection_id"], "superseded_by")
        self.event(
            old["project_id"], old["collection_id"], old_task_id, None, "TASK_SUPERSEDED",
            {"replacement_task_id": replacement_task_id, "reason": reason},
        )

    def retry_failed_task(self, task_id: str, reason: str) -> dict[str, Any]:
        """Create a fresh durable replacement for a failed task.

        Attempts remain immutable/auditable on the failed task. The replacement
        inherits its prerequisite dependencies and is wired in wherever the
        failed task was a hard dependency. If the failed implementation had a
        checkpoint, start the replacement from that checkpoint so useful partial
        work is not discarded.
        """
        old = self.get_task(task_id)
        if old["status"] != TaskStatus.FAILED.value:
            raise ValueError(f"Only FAILED tasks can be retried; {task_id} is {old['status']}")
        deps = [row["id"] for row in self.dependencies(task_id)]
        retry_base = old.get("current_head") or old.get("base_commit")
        replacement, _ = self.create_task(
            project_id=old["project_id"],
            collection_id=old["collection_id"],
            profile=old["profile"],
            title=old["title"],
            objective=old["objective"],
            reason=f"Manual retry of {task_id}: {reason}",
            parent_task_id=old.get("parent_task_id"),
            requested_by_task_id=old.get("requested_by_task_id"),
            depends_on=deps,
            workspace_mode=old.get("workspace_mode") or "collection",
            base_commit=retry_base,
            priority=int(old.get("priority") or 50),
            depth=int(old.get("depth") or 0),
            allow_dedupe=False,
        )
        self.supersede_task(task_id, replacement["id"], reason)
        self.set_collection_status(old["collection_id"], "RUNNING", commit=True)
        self.event(
            old["project_id"], old["collection_id"], task_id, None,
            "TASK_MANUAL_RETRY_CREATED",
            {"replacement_task_id": replacement["id"], "reason": reason},
        )
        return self.get_task(replacement["id"])

    def set_task_workspace(
        self,
        task_id: str,
        *,
        worktree_path: str | None = None,
        branch_name: str | None = None,
        base_commit: str | None = None,
        current_head: str | None = None,
    ) -> None:
        assignments = [f"updated_at={sql_literal(utcnow())}"]
        if worktree_path is not None:
            assignments.append(f"worktree_path={sql_literal(worktree_path)}")
        if branch_name is not None:
            assignments.append(f"branch_name={sql_literal(branch_name)}")
        if base_commit is not None:
            assignments.append(f"base_commit={sql_literal(base_commit)}")
        if current_head is not None:
            assignments.append(f"current_head={sql_literal(current_head)}")
        self.mutate(f"UPDATE tasks SET {','.join(assignments)} WHERE id={sql_literal(task_id)}")

    def set_task_status(self, task_id: str, status: str, *, message: str | None = None) -> None:
        self.mutate(
            f"UPDATE tasks SET status={sql_literal(status)},updated_at={sql_literal(utcnow())} WHERE id={sql_literal(task_id)}",
            message,
        )

    def submit_result(self, task_id: str, result: dict[str, Any]) -> dict[str, Any]:
        before = self.get_task(task_id)
        status = str(result.get("status", "complete")).strip().lower()
        mapping = {
            # A submitted "complete" result is not dependency-visible until the
            # harness exits and the executor finishes workspace checks/integration.
            "complete": TaskStatus.VERIFYING.value,
            "completed": TaskStatus.VERIFYING.value,
            "waiting": TaskStatus.WAITING.value,
            "failed": TaskStatus.FAILED.value,
        }
        if status not in mapping:
            raise ValueError("Result status must be one of: complete, waiting, failed")
        if before.get("profile") == "research" and status in {"complete", "completed"}:
            title = str(before.get("title") or "").lower()
            reason = str(before.get("reason") or "").lower()
            strict = "fresh external research" in title or "fresh external evidence" in reason or title.startswith("research requested by")
            if strict:
                collection = self.get_collection(before["collection_id"])
                policy = classify_planning_policy(collection.get("objective") or "")
                validate_deterministic_research_result(result, require_oracle=policy.external_conformance_required)
                exception_ids = research_exception_decision_ids(result)
                if exception_ids:
                    decisions = self.list_decisions(before["project_id"], before["collection_id"])
                    known = {str(decision.get("id") or "") for decision in decisions}
                    for decision_id in exception_ids:
                        if decision_id not in known:
                            raise ValueError(
                                f"research exception_decision_id {decision_id} is not a durable decision in this Collection"
                            )

        if status in {"complete", "completed"}:
            profile = str(before.get("profile") or "")
            collection = self.get_collection(before["collection_id"])
            policy = classify_planning_policy(collection.get("objective") or "")
            if profile in {"critic", "plan_review", "review"}:
                research_deps = self.research_ancestors(task_id)
                if research_deps:
                    require_pass = profile in {"plan_review", "review"} and str(result.get("verdict") or "").strip().lower() == "pass"
                    validate_research_consumer_result(result, research_deps, require_pass=require_pass)
                    exception_ids = research_consumer_exception_decision_ids(result)
                    if exception_ids:
                        known = {row["id"] for row in self.list_decisions(before["project_id"], before["collection_id"])}
                        missing = [decision_id for decision_id in exception_ids if decision_id not in known]
                        if missing:
                            raise ValueError(
                                "research consistency exception_decision_id is not a durable decision in this Collection: "
                                + ",".join(missing)
                            )
            if profile == "review" and policy.external_conformance_required:
                validate_external_conformance_review_result(result)
                decision_id = str(result.get("oracle_exception_decision_id") or "").strip()
                if decision_id:
                    decisions = self.list_decisions(before["project_id"], before["collection_id"])
                    if not any(str(decision.get("id") or "") == decision_id for decision in decisions):
                        raise ValueError(f"oracle_exception_decision_id {decision_id} is not a durable decision in this Collection")
        task_status = mapping[status]
        payload = json_dumps(result)
        self.mutate(
            "UPDATE tasks SET "
            f"status={sql_literal(task_status)},result_json={sql_literal(payload)},updated_at={sql_literal(utcnow())} "
            f"WHERE id={sql_literal(task_id)}",
            f"task {task_id}: result {task_status.lower()}",
        )
        task = self.get_task(task_id)
        self.event(task["project_id"], task["collection_id"], task_id, None, "TASK_RESULT_SUBMITTED", result)
        return task

    def request_task(
        self,
        caller_task_id: str,
        *,
        profile: str,
        title: str,
        objective: str,
        reason: str,
        blocking: bool,
        depends_on: list[str] | None,
        workspace_mode: str,
        base_commit: str | None,
        limits: dict[str, int],
    ) -> tuple[dict[str, Any], bool]:
        caller = self.get_task(caller_task_id)
        if profile == "coordinator" and caller["profile"] != "coordinator":
            raise PermissionError("Workers may not create coordinator tasks directly; use `praector coordinator attention`.")
        total = self.count_tasks(caller["collection_id"])
        if total >= int(limits.get("max_tasks_per_collection", 100)):
            raise RuntimeError("Collection task limit reached")
        depth = int(caller.get("depth") or 0) + 1
        if depth > int(limits.get("max_task_depth", 8)):
            raise RuntimeError("Maximum task depth reached")
        if self.child_count(caller_task_id) >= int(limits.get("max_children_per_task", 10)):
            raise RuntimeError("Maximum children for this task reached")

        deps = list(dict.fromkeys(depends_on or []))
        effective_blocking = bool(blocking)

        # A coordinator cannot safely keep shaping the graph while an independent
        # plan reviewer is evaluating it. Treat coordinator->plan_review as a
        # mandatory blocking handoff even if the model omitted --blocking. Include
        # the latest non-superseded critic as durable review input when present.
        gate_implement_tasks = False
        baseline_critic: dict[str, Any] | None = None
        baseline_fingerprint: str | None = None
        baseline_source: str | None = None
        ignored_implement_dependencies: list[str] = []
        ignored_caller_dependency = False
        reusable_plan_review: dict[str, Any] | None = None
        collection_tasks: list[dict[str, Any]] | None = None
        if profile == "plan_review":
            collection_tasks = self.list_tasks(caller["collection_id"])
            implement_ids = {
                task["id"] for task in collection_tasks
                if task.get("profile") == "implement"
                and task.get("status") not in {TaskStatus.CANCELLED.value, TaskStatus.SUPERSEDED.value}
            }
            ignored_implement_dependencies = [dep for dep in deps if dep in implement_ids]
            if ignored_implement_dependencies:
                # A plan_review is a pre-write gate over durable implementation
                # contracts. Depending on implementation execution is invalid even
                # before policy gating is considered; once Praector adds the valid
                # plan_review -> implement gate, the opposite dependency would form
                # a hard cycle. The reviewer can inspect READY task contracts without
                # an execution prerequisite.
                deps = [dep for dep in deps if dep not in implement_ids]
            if caller["profile"] == "coordinator" and caller_task_id in deps:
                # Blocking handoff adds plan_review -> caller. Keeping the caller
                # itself as an explicit review prerequisite would create the
                # reverse edge in the same atomic graph contract.
                deps = [dep for dep in deps if dep != caller_task_id]
                ignored_caller_dependency = True

        if caller["profile"] == "coordinator" and profile == "plan_review":
            effective_blocking = True
            gate_implement_tasks = True
            assert collection_tasks is not None
            critics = [
                task for task in collection_tasks
                if task["profile"] == "critic"
                and task["status"] not in {TaskStatus.CANCELLED.value, TaskStatus.SUPERSEDED.value}
            ]
            if critics:
                latest_critic = critics[-1]
                if latest_critic["id"] not in deps:
                    deps.append(latest_critic["id"])
                if (
                    latest_critic.get("status") == TaskStatus.COMPLETE.value
                    and self.critic_plan_baseline(latest_critic["id"]) is None
                ):
                    baseline_critic = latest_critic
                    baseline_fingerprint = self.material_plan_fingerprint(caller["collection_id"])
                    open_revisions = self.list_collection_revisions(caller["collection_id"], status="OPEN")
                    is_revision_coordinator = any(
                        revision.get("coordinator_task_id") == caller_task_id for revision in open_revisions
                    )
                    baseline_source = "revision_plan_seal" if is_revision_coordinator else "initial_plan_seal"

            # Single-flight final plan review: a coordinator may refine the graph
            # many times, but there is only one live non-failed final review gate.
            # The reviewer consumes the current durable graph, so a new title or
            # objective string is not a reason to create another review generation.
            candidates = [
                task for task in collection_tasks
                if task.get("profile") == "plan_review"
                and task.get("status") in {
                    TaskStatus.PENDING.value, TaskStatus.READY.value, TaskStatus.RUNNING.value,
                    TaskStatus.WAITING.value, TaskStatus.VERIFYING.value,
                }
                and str((task.get("result") or {}).get("verdict") or "").lower() != "fail"
            ]
            if candidates:
                reusable_plan_review = candidates[-1]

        if reusable_plan_review is not None:
            task = self._attach_existing_plan_review_handoff(
                plan_review_task_id=reusable_plan_review["id"],
                caller_task_id=caller_task_id,
                depends_on=deps,
                critic_baseline_task_id=baseline_critic["id"] if baseline_critic is not None else None,
                critic_baseline_fingerprint=baseline_fingerprint,
                critic_baseline_source=baseline_source,
            )
            created = False
            self.event(
                caller["project_id"], caller["collection_id"], caller_task_id, None,
                "PLAN_REVIEW_REUSED",
                {
                    "plan_review_task_id": task["id"],
                    "reason": "single-flight final plan-review gate",
                },
            )
        else:
            task, created = self.create_task(
                project_id=caller["project_id"],
                collection_id=caller["collection_id"],
                profile=profile,
                title=title,
                objective=objective,
                reason=reason,
                parent_task_id=caller_task_id,
                requested_by_task_id=caller_task_id,
                depends_on=deps,
                workspace_mode=workspace_mode,
                base_commit=base_commit,
                depth=depth,
                spawned_by_task_id=caller_task_id,
                blocking_caller_task_id=caller_task_id if effective_blocking else None,
                gate_implement_tasks=gate_implement_tasks,
                critic_baseline_task_id=baseline_critic["id"] if baseline_critic is not None else None,
                critic_baseline_fingerprint=baseline_fingerprint,
                critic_baseline_source=baseline_source,
            )
        self.event(
            caller["project_id"],
            caller["collection_id"],
            caller_task_id,
            None,
            "TASK_REQUESTED",
            {"requested_task_id": task["id"], "profile": profile, "blocking": effective_blocking, "created": created},
        )
        if ignored_implement_dependencies:
            self.event(
                caller["project_id"],
                caller["collection_id"],
                caller_task_id,
                None,
                "PLAN_REVIEW_IMPLEMENT_DEPENDENCIES_IGNORED",
                {
                    "plan_review_task_id": task["id"],
                    "implement_task_ids": ignored_implement_dependencies,
                    "reason": "plan_review is a pre-write gate and cannot depend on implementation execution",
                },
            )
        if ignored_caller_dependency:
            self.event(
                caller["project_id"], caller["collection_id"], caller_task_id, None,
                "PLAN_REVIEW_CALLER_DEPENDENCY_IGNORED",
                {
                    "plan_review_task_id": task["id"],
                    "coordinator_task_id": caller_task_id,
                    "reason": "blocking plan-review handoff makes the coordinator depend on the review",
                },
            )
        return task, created

    def request_coordinator_attention(
        self,
        caller_task_id: str,
        reason: str,
        *,
        blocking: bool,
        limits: dict[str, int],
    ) -> tuple[dict[str, Any], bool]:
        caller = self.get_task(caller_task_id)
        if caller["profile"] == "coordinator":
            raise PermissionError("Coordinator already owns graph-level reasoning; it should modify/request work directly rather than spawning another coordinator.")
        total = self.count_tasks(caller["collection_id"])
        if total >= int(limits.get("max_tasks_per_collection", 100)):
            raise RuntimeError("Collection task limit reached")
        depth = int(caller.get("depth") or 0) + 1
        if depth > int(limits.get("max_task_depth", 8)):
            raise RuntimeError("Maximum task depth reached")
        task, created = self.create_task(
            project_id=caller["project_id"],
            collection_id=caller["collection_id"],
            profile="coordinator",
            title="Coordinator attention",
            objective=f"Re-evaluate the collection plan because: {reason}",
            reason=reason,
            parent_task_id=caller_task_id,
            requested_by_task_id=caller_task_id,
            workspace_mode="collection",
            depth=depth,
            allow_dedupe=False,
            spawned_by_task_id=caller_task_id,
            blocking_caller_task_id=caller_task_id if blocking else None,
        )
        self.event(
            caller["project_id"], caller["collection_id"], caller_task_id, None,
            "COORDINATOR_ATTENTION_REQUESTED", {"coordinator_task_id": task["id"], "reason": reason, "blocking": blocking}
        )
        return task, created

    def refresh_waiting_tasks(self, collection_id: str | None = None) -> list[str]:
        where = "status='WAITING'"
        if collection_id:
            where += f" AND collection_id={sql_literal(collection_id)}"
        waiting = self.query(f"SELECT id FROM tasks WHERE {where}")
        changed: list[str] = []
        for row in waiting:
            task_id = row["id"]
            unmet = self.query(
                "SELECT COUNT(*) AS n FROM task_edges e JOIN tasks t ON t.id=e.from_task_id "
                f"WHERE e.to_task_id={sql_literal(task_id)} AND e.relation='depends_on' AND t.status!='COMPLETE'"
            )
            if int(unmet[0]["n"]) == 0:
                self.set_task_status(task_id, TaskStatus.READY.value, message=f"task {task_id}: dependencies satisfied")
                changed.append(task_id)
        return changed

    def runnable_tasks(self, collection_id: str | None = None, limit: int = 100) -> list[dict[str, Any]]:
        where = "t.status IN ('READY','PENDING')"
        if collection_id:
            where += f" AND t.collection_id={sql_literal(collection_id)}"
        return self.query(
            "SELECT t.* FROM tasks t WHERE " + where + " AND NOT EXISTS ("
            "SELECT 1 FROM task_edges e JOIN tasks dep ON dep.id=e.from_task_id "
            "WHERE e.to_task_id=t.id AND e.relation='depends_on' AND dep.status!='COMPLETE'"
            ") AND NOT EXISTS ("
            # A blocking child may be created while its parent's OpenCode process
            # is still unwinding. Do not start it until that parent session exits.
            "SELECT 1 FROM task_edges eb "
            "JOIN attempts pa ON pa.task_id=eb.to_task_id "
            "JOIN sessions ps ON ps.attempt_id=pa.id "
            "WHERE eb.from_task_id=t.id AND eb.to_task_id=t.parent_task_id "
            "AND eb.relation='depends_on' AND ps.status='RUNNING'"
            ") ORDER BY t.priority DESC,t.created_at LIMIT " + str(int(limit))
        )

    def _planning_gate_allows_claim_locked(self, task: dict[str, Any]) -> bool:
        """Final claim-time safety gate for implementation work.

        Scheduler reconciliation is intentionally eventual and can race with a
        coordinator that is creating several graph nodes through separate CLI
        calls. A write task in a collection that requires plan review must never
        start merely because its dependency edge has not been published yet.
        """
        profile = task.get("profile")
        if profile not in {"implement", "plan_review"}:
            return True
        collection_rows = self._query_locked(
            f"SELECT objective FROM collections WHERE id={sql_literal(task['collection_id'])} LIMIT 1"
        )
        objective = collection_rows[0].get("objective") if collection_rows else ""
        policy = classify_planning_policy(objective or "")

        if profile == "plan_review":
            if not policy.critic_required:
                return True
            critics = self._query_locked(
                "SELECT * FROM tasks WHERE "
                f"collection_id={sql_literal(task['collection_id'])} AND profile='critic' "
                "AND status NOT IN ('CANCELLED','SUPERSEDED') ORDER BY created_at,id"
            )
            if not critics:
                return False
            critic = critics[-1]
            if critic.get("status") != TaskStatus.COMPLETE.value:
                return False
            if policy.research_required:
                research = self._research_ancestors_locked(str(critic["id"]))
                if not research:
                    return False
                try:
                    validate_research_consumer_result(
                        json_loads(critic.get("result_json") or "{}"), research, require_pass=False
                    )
                except ValueError:
                    return False
            critic_edge = self._query_locked(
                "SELECT 1 AS ok FROM task_edges WHERE "
                f"from_task_id={sql_literal(critic['id'])} AND to_task_id={sql_literal(task['id'])} "
                "AND relation='depends_on' LIMIT 1"
            )
            if not critic_edge:
                return False
            baseline = self._critic_plan_baseline_locked(str(critic["id"]))
            if not baseline or not baseline.get("fingerprint"):
                return False
            current_fingerprint = self._material_plan_fingerprint_locked(
                str(task["collection_id"]), str(objective or "")
            )
            legacy_fingerprint = self._material_plan_fingerprint_locked(
                str(task["collection_id"]), str(objective or ""), legacy_raw_base_commit=True
            )
            return str(baseline.get("fingerprint")) in {current_fingerprint, legacy_fingerprint}

        if not policy.plan_review_required:
            return True

        reviews = self._query_locked(
            "SELECT * FROM tasks WHERE "
            f"collection_id={sql_literal(task['collection_id'])} AND profile='plan_review' "
            "AND status NOT IN ('CANCELLED','SUPERSEDED') ORDER BY created_at,id"
        )
        if not reviews:
            return False
        review = reviews[-1]
        if review.get("status") != TaskStatus.COMPLETE.value:
            return False
        if str(review.get("updated_at") or "") < str(task.get("created_at") or ""):
            return False
        review_result = json_loads(review.get("result_json") or "{}")
        if str(review_result.get("verdict") or "").strip().lower() != "pass":
            return False
        if policy.research_required:
            research = self._research_ancestors_locked(str(review["id"]))
            if not research:
                return False
            try:
                validate_research_consumer_result(review_result, research, require_pass=True)
            except ValueError:
                return False

        edge = self._query_locked(
            "SELECT 1 AS ok FROM task_edges WHERE "
            f"from_task_id={sql_literal(review['id'])} AND to_task_id={sql_literal(task['id'])} "
            "AND relation='depends_on' LIMIT 1"
        )
        if not edge:
            return False

        if policy.critic_required:
            critics = self._query_locked(
                "SELECT * FROM tasks WHERE "
                f"collection_id={sql_literal(task['collection_id'])} AND profile='critic' "
                "AND status NOT IN ('CANCELLED','SUPERSEDED') ORDER BY created_at,id"
            )
            if not critics:
                return False
            critic = critics[-1]
            if critic.get("status") != TaskStatus.COMPLETE.value:
                return False
            if policy.research_required:
                research = self._research_ancestors_locked(str(critic["id"]))
                if not research:
                    return False
                try:
                    validate_research_consumer_result(
                        json_loads(critic.get("result_json") or "{}"), research, require_pass=False
                    )
                except ValueError:
                    return False
            baseline = self._critic_plan_baseline_locked(str(critic["id"]))
            if not baseline or not baseline.get("fingerprint"):
                return False
            current_fingerprint = self._material_plan_fingerprint_locked(
                str(task["collection_id"]), str(objective or "")
            )
            legacy_fingerprint = self._material_plan_fingerprint_locked(
                str(task["collection_id"]), str(objective or ""), legacy_raw_base_commit=True
            )
            if str(baseline.get("fingerprint")) not in {current_fingerprint, legacy_fingerprint}:
                return False
            if str(critic.get("updated_at") or "") > str(review.get("updated_at") or ""):
                return False
            critic_edge = self._query_locked(
                "SELECT 1 AS ok FROM task_edges WHERE "
                f"from_task_id={sql_literal(critic['id'])} AND to_task_id={sql_literal(review['id'])} "
                "AND relation='depends_on' LIMIT 1"
            )
            if not critic_edge:
                return False

        active_coordinators = self._query_locked(
            "SELECT COUNT(*) AS n FROM tasks WHERE "
            f"collection_id={sql_literal(task['collection_id'])} AND profile='coordinator' "
            "AND status IN ('PENDING','READY','RUNNING','WAITING','VERIFYING')"
        )
        if active_coordinators and int(active_coordinators[0]['n']) != 0:
            return False
        return True

    def claim_task(self, task_id: str, max_attempts: int) -> tuple[dict[str, Any], dict[str, Any]] | None:
        with self.lock():
            rows = self._query_locked(f"SELECT * FROM tasks WHERE id={sql_literal(task_id)} LIMIT 1")
            if not rows or rows[0]["status"] not in ("READY", "PENDING"):
                return None
            if not self._planning_gate_allows_claim_locked(rows[0]):
                return None
            unmet = self._query_locked(
                "SELECT COUNT(*) AS n FROM task_edges e JOIN tasks dep ON dep.id=e.from_task_id "
                f"WHERE e.to_task_id={sql_literal(task_id)} AND e.relation='depends_on' AND dep.status!='COMPLETE'"
            )
            if int(unmet[0]["n"]) != 0:
                return None
            attempts = self._query_locked(f"SELECT COUNT(*) AS n FROM attempts WHERE task_id={sql_literal(task_id)}")
            attempt_no = int(attempts[0]["n"]) + 1
            if attempt_no > max_attempts:
                self._mutate_locked(
                    f"UPDATE tasks SET status='FAILED',updated_at={sql_literal(utcnow())} WHERE id={sql_literal(task_id)}"
                )
                self._semantic_commit_locked(f"task {task_id}: exhausted attempts")
                return None
            attempt_id = make_id("A")
            now = utcnow()
            self._mutate_locked(
                f"UPDATE tasks SET status='RUNNING',updated_at={sql_literal(now)} WHERE id={sql_literal(task_id)};"
                "INSERT INTO attempts (id,task_id,attempt_no,status,started_at,created_at) VALUES ("
                f"{sql_literal(attempt_id)},{sql_literal(task_id)},{attempt_no},'RUNNING',{sql_literal(now)},{sql_literal(now)})"
            )
            self._semantic_commit_locked(f"task {task_id}: start attempt {attempt_no}")
            task = self._query_locked(f"SELECT * FROM tasks WHERE id={sql_literal(task_id)} LIMIT 1")[0]
            attempt = self._query_locked(f"SELECT * FROM attempts WHERE id={sql_literal(attempt_id)} LIMIT 1")[0]
            return task, attempt

    def set_attempt_log_path(self, attempt_id: str, log_path: str) -> None:
        self.mutate(
            "UPDATE attempts SET "
            f"log_path={sql_literal(log_path)} WHERE id={sql_literal(attempt_id)}"
        )

    def finish_attempt(
        self,
        attempt_id: str,
        *,
        status: str,
        exit_code: int | None,
        result: dict[str, Any] | None,
        external_session_id: str | None,
        log_path: str | None,
    ) -> None:
        self.mutate(
            "UPDATE attempts SET "
            f"status={sql_literal(status)},exit_code={sql_literal(exit_code)},"
            f"external_session_id={sql_literal(external_session_id)},log_path={sql_literal(log_path)},"
            f"result_json={sql_literal(json_dumps(result or {}))},ended_at={sql_literal(utcnow())} "
            f"WHERE id={sql_literal(attempt_id)}"
        )

    def create_session(self, attempt_id: str, harness: str, workdir: str) -> dict[str, Any]:
        session_id = make_id("S")
        now = utcnow()
        self.mutate(
            "INSERT INTO sessions (id,attempt_id,harness,status,workdir,started_at,created_at) VALUES ("
            f"{sql_literal(session_id)},{sql_literal(attempt_id)},{sql_literal(harness)},'RUNNING',"
            f"{sql_literal(workdir)},{sql_literal(now)},{sql_literal(now)})"
        )
        return self.query(f"SELECT * FROM sessions WHERE id={sql_literal(session_id)} LIMIT 1")[0]

    def set_session_pid(self, session_id: str, pid: int) -> None:
        self.mutate(
            f"UPDATE sessions SET pid={int(pid)} WHERE id={sql_literal(session_id)}"
        )

    def running_sessions(self) -> list[dict[str, Any]]:
        return self.query(
            "SELECT s.*,a.task_id,a.status AS attempt_status,t.status AS task_status "
            "FROM sessions s JOIN attempts a ON a.id=s.attempt_id JOIN tasks t ON t.id=a.task_id "
            "WHERE s.status='RUNNING' ORDER BY s.started_at"
        )

    def has_running_session(self, collection_id: str) -> bool:
        rows = self.query(
            "SELECT s.id FROM sessions s "
            "JOIN attempts a ON a.id=s.attempt_id "
            "JOIN tasks t ON t.id=a.task_id "
            "WHERE s.status='RUNNING' "
            f"AND t.collection_id={sql_literal(collection_id)} LIMIT 1"
        )
        return bool(rows)

    def abort_stale_session(self, session_id: str, attempt_id: str, task_id: str) -> None:
        now = utcnow()
        with self.lock():
            self._mutate_locked(
                f"UPDATE sessions SET status='ABORTED',ended_at={sql_literal(now)} WHERE id={sql_literal(session_id)};"
                f"UPDATE attempts SET status='ABORTED',ended_at={sql_literal(now)} WHERE id={sql_literal(attempt_id)};"
                f"UPDATE tasks SET status=CASE WHEN status IN ('RUNNING','VERIFYING') THEN 'READY' ELSE status END,"
                f"updated_at={sql_literal(now)} WHERE id={sql_literal(task_id)}"
            )
            self._semantic_commit_locked(f"recovery: abort stale session {session_id}")

    def finish_session(self, session_id: str, *, external_session_id: str | None, status: str) -> None:
        self.mutate(
            "UPDATE sessions SET "
            f"status={sql_literal(status)},external_session_id={sql_literal(external_session_id)},"
            f"ended_at={sql_literal(utcnow())} WHERE id={sql_literal(session_id)}"
        )

    # ---------- Context ----------

    def get_parent(self, task: dict[str, Any]) -> dict[str, Any] | None:
        parent_id = task.get("parent_task_id")
        return self.get_task(parent_id) if parent_id else None

    def list_memories(self, project_id: str, collection_id: str | None = None, state: str = "accepted") -> list[dict[str, Any]]:
        where = f"project_id={sql_literal(project_id)} AND state={sql_literal(state)}"
        if collection_id:
            where += f" AND (scope='project' OR collection_id={sql_literal(collection_id)})"
        else:
            where += " AND scope='project'"
        return self.query(f"SELECT * FROM memories WHERE {where} ORDER BY created_at")

    def add_memory(
        self,
        *,
        project_id: str,
        collection_id: str | None,
        task_id: str | None,
        scope: str,
        state: str,
        kind: str,
        text: str,
        evidence: Any = None,
    ) -> dict[str, Any]:
        memory_id = make_id("MEM")
        now = utcnow()
        self.mutate(
            "INSERT INTO memories (id,project_id,collection_id,task_id,scope,state,kind,text,evidence_json,created_at,updated_at) VALUES ("
            f"{sql_literal(memory_id)},{sql_literal(project_id)},{sql_literal(collection_id)},{sql_literal(task_id)},"
            f"{sql_literal(scope)},{sql_literal(state)},{sql_literal(kind)},{sql_literal(text)},"
            f"{sql_literal(json_dumps(evidence or []))},{sql_literal(now)},{sql_literal(now)})",
            f"memory {memory_id}: add {scope} {kind}",
        )
        return self.query(f"SELECT * FROM memories WHERE id={sql_literal(memory_id)} LIMIT 1")[0]

    def list_decisions(self, project_id: str, collection_id: str | None = None) -> list[dict[str, Any]]:
        where = f"project_id={sql_literal(project_id)}"
        if collection_id:
            where += f" AND (collection_id IS NULL OR collection_id={sql_literal(collection_id)})"
        return self.query(f"SELECT * FROM decisions WHERE {where} AND superseded_by IS NULL ORDER BY created_at")

    def add_decision(
        self,
        *,
        project_id: str,
        collection_id: str | None,
        task_id: str | None,
        statement: str,
        rationale: str,
        evidence: Any = None,
        alternatives: Any = None,
    ) -> dict[str, Any]:
        decision_id = make_id("DEC")
        self.mutate(
            "INSERT INTO decisions (id,project_id,collection_id,task_id,statement,rationale,evidence_json,alternatives_json,created_at) VALUES ("
            f"{sql_literal(decision_id)},{sql_literal(project_id)},{sql_literal(collection_id)},{sql_literal(task_id)},"
            f"{sql_literal(statement)},{sql_literal(rationale)},{sql_literal(json_dumps(evidence or []))},"
            f"{sql_literal(json_dumps(alternatives or []))},{sql_literal(utcnow())})",
            f"decision {decision_id}: {statement[:80]}",
        )
        return self.query(f"SELECT * FROM decisions WHERE id={sql_literal(decision_id)} LIMIT 1")[0]

    # ---------- Events / status ----------

    def event(
        self,
        project_id: str | None,
        collection_id: str | None,
        task_id: str | None,
        attempt_id: str | None,
        event_type: str,
        payload: Any,
    ) -> None:
        event_id = make_id("EVT")
        self.mutate(
            "INSERT INTO events (id,project_id,collection_id,task_id,attempt_id,event_type,payload_json,created_at) VALUES ("
            f"{sql_literal(event_id)},{sql_literal(project_id)},{sql_literal(collection_id)},{sql_literal(task_id)},"
            f"{sql_literal(attempt_id)},{sql_literal(event_type)},{sql_literal(json_dumps(payload))},{sql_literal(utcnow())})"
        )

    def repair_superseded_review_lineage(self, collection_id: str) -> int:
        """Backfill verifier/prerequisite edges for reviews superseded by older Praector versions."""
        tasks = {task["id"]: task for task in self.list_tasks(collection_id)}
        edges = self.list_edges(collection_id)
        edge_keys = {(edge["from_task_id"], edge["to_task_id"], edge["relation"]) for edge in edges}
        supersessions = [edge for edge in edges if edge["relation"] == "superseded_by"]
        repaired = 0

        # Iterate to a fixed point so a historical chain R1 -> R2 -> R3 can
        # inherit the original implementation verification edge in one reconcile.
        changed = True
        while changed:
            changed = False
            for edge in supersessions:
                old = tasks.get(edge["from_task_id"])
                replacement = tasks.get(edge["to_task_id"])
                if not old or not replacement:
                    continue
                if old.get("profile") != replacement.get("profile") or old.get("profile") not in {"review", "plan_review"}:
                    continue
                incoming = list(edge_keys)
                for from_task_id, to_task_id, relation in incoming:
                    if to_task_id != old["id"]:
                        continue
                    if relation == "depends_on":
                        key = (from_task_id, replacement["id"], "depends_on")
                    elif relation == "verifies" and old.get("profile") == "review":
                        key = (from_task_id, replacement["id"], "verifies")
                    else:
                        continue
                    if key in edge_keys:
                        continue
                    self.add_edge(key[0], key[1], collection_id, key[2])
                    edge_keys.add(key)
                    repaired += 1
                    changed = True
        if repaired:
            collection = self.get_collection(collection_id)
            self.event(
                collection["project_id"], collection_id, None, None,
                "REVIEW_LINEAGE_REPAIRED", {"edges_added": repaired},
            )
        return repaired

    def implementations_missing_passing_review(self, collection_id: str) -> list[str]:
        """Return completed implementation task ids without a durable passing review."""
        tasks = self.list_tasks(collection_id)
        tasks_by_id = {task["id"]: task for task in tasks}
        edges = self.list_edges(collection_id)
        verifies: dict[str, list[str]] = {}
        for edge in edges:
            if edge["relation"] == "verifies":
                verifies.setdefault(edge["from_task_id"], []).append(edge["to_task_id"])

        missing: list[str] = []
        for task in tasks:
            if task.get("profile") != "implement" or task.get("status") != TaskStatus.COMPLETE.value:
                continue
            candidates = list(verifies.get(task["id"], []))
            parent_id = task.get("parent_task_id")
            if parent_id and tasks_by_id.get(parent_id, {}).get("profile") == "review":
                candidates.append(parent_id)
            passed = False
            for review_id in candidates:
                review = tasks_by_id.get(review_id)
                if not review or review.get("status") != TaskStatus.COMPLETE.value:
                    continue
                verdict = str((review.get("result") or {}).get("verdict", "")).strip().lower()
                if verdict == "pass":
                    passed = True
                    break
            if not passed:
                missing.append(task["id"])
        return missing

    def reconcile_collection_revisions(self, collection_id: str) -> int:
        """Satisfy open operator revisions only after fresh repaired work is reviewed."""
        revisions = self.list_collection_revisions(collection_id, status="OPEN")
        if not revisions:
            return 0
        tasks = self.list_tasks(collection_id)
        tasks_by_id = {task["id"]: task for task in tasks}
        edges = self.list_edges(collection_id)
        verifies: dict[str, list[str]] = {}
        for edge in edges:
            if edge["relation"] == "verifies":
                verifies.setdefault(edge["from_task_id"], []).append(edge["to_task_id"])

        successful_terminal = {"COMPLETE", "CANCELLED", "SUPERSEDED"}
        satisfied = 0
        for revision in revisions:
            created_at = str(revision.get("created_at") or "")
            post_tasks = [task for task in tasks if str(task.get("created_at") or "") > created_at]
            implementations = [task for task in post_tasks if task.get("profile") == "implement"]
            if not implementations:
                continue
            if any(task.get("status") not in successful_terminal for task in post_tasks):
                continue
            completed_impls = [task for task in implementations if task.get("status") == "COMPLETE"]
            if not completed_impls:
                continue

            review_ids: list[str] = []
            all_verified = True
            for impl in completed_impls:
                passed_review = None
                for review_id in verifies.get(impl["id"], []):
                    review = tasks_by_id.get(review_id)
                    if not review or review.get("status") != "COMPLETE":
                        continue
                    if str(review.get("created_at") or "") <= created_at:
                        continue
                    verdict = str((review.get("result") or {}).get("verdict", "")).strip().lower()
                    if verdict == "pass":
                        passed_review = review_id
                        break
                if not passed_review:
                    all_verified = False
                    break
                review_ids.append(passed_review)
            if not all_verified:
                continue

            now = utcnow()
            self.mutate(
                "UPDATE collection_revisions SET status='SATISFIED',"
                f"satisfied_at={sql_literal(now)} WHERE id={sql_literal(revision['id'])} AND status='OPEN'",
                f"collection {collection_id}: satisfy revision {revision['id']}",
            )
            collection = self.get_collection(collection_id)
            self.event(
                collection["project_id"], collection_id, None, None,
                "COLLECTION_REVISION_SATISFIED",
                {
                    "revision_id": revision["id"],
                    "implementation_task_ids": [task["id"] for task in completed_impls],
                    "review_task_ids": review_ids,
                },
            )
            satisfied += 1
        return satisfied

    def update_collection_terminal_state(self, collection_id: str) -> str:
        """Reconcile collection status from the durable graph.

        Collection status is derived state. A collection is COMPLETE when every
        task is successfully terminal, RUNNING while work can advance, and
        BLOCKED when tasks remain but none are runnable or executing.
        """
        # Reconcile historical 0.1.13-era review replacements before deriving
        # terminal state so existing collections gain the same verification
        # invariants as newly-created work.
        self.repair_superseded_review_lineage(collection_id)
        self.reconcile_collection_revisions(collection_id)
        tasks = self.list_tasks(collection_id)
        if not tasks:
            return self.get_collection(collection_id)["status"]
        statuses = {t["status"] for t in tasks}
        successful_terminal = {"COMPLETE", "CANCELLED", "SUPERSEDED"}
        open_revisions = self.list_collection_revisions(collection_id, status="OPEN")
        if open_revisions:
            has_executing = any(t["status"] in {"RUNNING", "VERIFYING"} for t in tasks)
            has_runnable = bool(self.runnable_tasks(collection_id, limit=1))
            has_active_session = False if (has_executing or has_runnable) else self.has_running_session(collection_id)
            desired = "RUNNING" if (has_executing or has_runnable or has_active_session) else "BLOCKED"
        elif statuses <= successful_terminal and "COMPLETE" in statuses:
            # COMPLETE must mean the requested delivery actually exists and has
            # acceptance-grade verification. Direct mutation objectives may not
            # terminate after planning/critique alone.
            collection = self.get_collection(collection_id)
            delivery_policy = classify_delivery_policy(collection.get("objective") or "")
            completed_impls = [
                task for task in tasks
                if task.get("profile") == "implement" and task.get("status") == "COMPLETE"
            ]
            missing_delivery = bool(delivery_policy.implementation_required and not completed_impls)
            missing_reviews = self.implementations_missing_passing_review(collection_id)
            planning_policy = classify_planning_policy(collection.get("objective") or "")
            critic_state = self.critic_revalidation_state(collection_id)
            stale_required_critic = bool(planning_policy.critic_required and critic_state.get("stale"))
            desired = "BLOCKED" if (missing_delivery or missing_reviews or stale_required_critic) else "COMPLETE"
        else:
            has_executing = any(t["status"] in {"RUNNING", "VERIFYING"} for t in tasks)
            has_runnable = bool(self.runnable_tasks(collection_id, limit=1))
            has_active_session = False if (has_executing or has_runnable) else self.has_running_session(collection_id)
            desired = "RUNNING" if (has_executing or has_runnable or has_active_session) else "BLOCKED"
        current = self.get_collection(collection_id)["status"]
        if current != desired:
            self.set_collection_status(collection_id, desired, commit=(desired in {"COMPLETE", "BLOCKED"}))
        return desired

    # ---------- Collection acceptance ----------

    def get_collection_acceptance(
        self, collection_id: str, target_branch: str | None = None
    ) -> dict[str, Any] | None:
        where = f"collection_id={sql_literal(collection_id)}"
        if target_branch:
            where += f" AND target_branch={sql_literal(target_branch)}"
        rows = self.query(
            f"SELECT * FROM collection_acceptances WHERE {where} ORDER BY created_at DESC LIMIT 1"
        )
        return rows[0] if rows else None

    def list_collection_acceptances(self, collection_id: str) -> list[dict[str, Any]]:
        return self.query(
            "SELECT * FROM collection_acceptances WHERE collection_id="
            + sql_literal(collection_id)
            + " ORDER BY created_at"
        )

    def record_collection_acceptance(
        self,
        *,
        project_id: str,
        collection_id: str,
        target_branch: str,
        source_branch: str,
        source_commit: str,
        target_before_commit: str,
        accepted_commit: str,
        merge_mode: str,
        actor: str | None,
    ) -> dict[str, Any]:
        existing = self.get_collection_acceptance(collection_id, target_branch)
        if existing:
            return existing
        acceptance_id = make_id("ACC")
        now = utcnow()
        self.mutate(
            "INSERT INTO collection_acceptances "
            "(id,project_id,collection_id,target_branch,source_branch,source_commit,target_before_commit,"
            "accepted_commit,merge_mode,actor,created_at) VALUES ("
            f"{sql_literal(acceptance_id)},{sql_literal(project_id)},{sql_literal(collection_id)},"
            f"{sql_literal(target_branch)},{sql_literal(source_branch)},{sql_literal(source_commit)},"
            f"{sql_literal(target_before_commit)},{sql_literal(accepted_commit)},{sql_literal(merge_mode)},"
            f"{sql_literal(actor)},{sql_literal(now)})",
            f"collection {collection_id}: accepted into {target_branch} at {accepted_commit[:12]}",
        )
        self.event(
            project_id,
            collection_id,
            None,
            None,
            "COLLECTION_ACCEPTED",
            {
                "target_branch": target_branch,
                "source_branch": source_branch,
                "source_commit": source_commit,
                "target_before_commit": target_before_commit,
                "accepted_commit": accepted_commit,
                "merge_mode": merge_mode,
                "actor": actor,
            },
        )
        return self.get_collection_acceptance(collection_id, target_branch) or {}

    # ---------- Collection pull requests ----------

    def get_collection_pull_request(
        self, collection_id: str, provider: str = "github"
    ) -> dict[str, Any] | None:
        rows = self.query(
            "SELECT * FROM collection_pull_requests WHERE "
            f"collection_id={sql_literal(collection_id)} AND provider={sql_literal(provider)} "
            "ORDER BY created_at DESC LIMIT 1"
        )
        return rows[0] if rows else None

    def list_collection_pull_requests(self, collection_id: str) -> list[dict[str, Any]]:
        return self.query(
            "SELECT * FROM collection_pull_requests WHERE collection_id="
            + sql_literal(collection_id)
            + " ORDER BY created_at"
        )

    def record_collection_pull_request(
        self,
        *,
        project_id: str,
        collection_id: str,
        provider: str,
        remote_name: str,
        repository: str | None,
        pr_number: int,
        url: str,
        state: str,
        is_draft: bool,
        base_branch: str,
        head_branch: str,
        source_commit: str,
        title: str,
        actor: str | None,
    ) -> dict[str, Any]:
        existing = self.get_collection_pull_request(collection_id, provider)
        now = utcnow()
        if existing:
            self.mutate(
                "UPDATE collection_pull_requests SET "
                f"remote_name={sql_literal(remote_name)},repository={sql_literal(repository)},"
                f"pr_number={sql_literal(pr_number)},url={sql_literal(url)},state={sql_literal(state)},"
                f"is_draft={sql_literal(is_draft)},base_branch={sql_literal(base_branch)},"
                f"head_branch={sql_literal(head_branch)},source_commit={sql_literal(source_commit)},"
                f"title={sql_literal(title)},actor={sql_literal(actor)},updated_at={sql_literal(now)} "
                f"WHERE id={sql_literal(existing['id'])}",
                f"collection {collection_id}: refresh {provider} PR #{pr_number}",
            )
            event_type = "COLLECTION_PR_UPDATED"
        else:
            record_id = make_id("PR")
            self.mutate(
                "INSERT INTO collection_pull_requests "
                "(id,project_id,collection_id,provider,remote_name,repository,pr_number,url,state,is_draft,"
                "base_branch,head_branch,source_commit,title,actor,created_at,updated_at) VALUES ("
                f"{sql_literal(record_id)},{sql_literal(project_id)},{sql_literal(collection_id)},"
                f"{sql_literal(provider)},{sql_literal(remote_name)},{sql_literal(repository)},"
                f"{sql_literal(pr_number)},{sql_literal(url)},{sql_literal(state)},{sql_literal(is_draft)},"
                f"{sql_literal(base_branch)},{sql_literal(head_branch)},{sql_literal(source_commit)},"
                f"{sql_literal(title)},{sql_literal(actor)},{sql_literal(now)},{sql_literal(now)})",
                f"collection {collection_id}: record {provider} PR #{pr_number}",
            )
            event_type = "COLLECTION_PR_RECORDED"
        self.event(
            project_id,
            collection_id,
            None,
            None,
            event_type,
            {
                "provider": provider,
                "remote_name": remote_name,
                "repository": repository,
                "pr_number": pr_number,
                "url": url,
                "state": state,
                "is_draft": is_draft,
                "base_branch": base_branch,
                "head_branch": head_branch,
                "source_commit": source_commit,
                "title": title,
                "actor": actor,
            },
        )
        return self.get_collection_pull_request(collection_id, provider) or {}

    def task_input_lineage(self, task_id: str) -> dict[str, Any] | None:
        rows = self.query(
            "SELECT payload_json,attempt_id,created_at FROM events WHERE "
            f"task_id={sql_literal(task_id)} AND event_type='TASK_INPUT_LINEAGE' "
            "ORDER BY created_at DESC LIMIT 1"
        )
        if not rows:
            return None
        payload = json_loads(rows[0].get("payload_json"), {})
        if not isinstance(payload, dict):
            return None
        out = dict(payload)
        out.setdefault("attempt_id", rows[0].get("attempt_id"))
        out.setdefault("created_at", rows[0].get("created_at"))
        return out

    def events_for_collection(self, collection_id: str, limit: int = 100) -> list[dict[str, Any]]:
        return self.query(
            f"SELECT * FROM events WHERE collection_id={sql_literal(collection_id)} ORDER BY created_at DESC LIMIT {int(limit)}"
        )
