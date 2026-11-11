from __future__ import annotations

import argparse
import getpass
import json
import os
import shutil
import sys
from pathlib import Path
from typing import Any

from . import __version__
from .config import (
    PROFILE_NAMES,
    ProjectConfig,
    find_project_marker,
    global_config,
    project_config_from_row,
    read_project_marker,
    write_project_marker,
)
from .db import DoltStore
from .scheduler import Scheduler
from .profile_prompts import PROFILE_TITLES, load_global_prompt, load_profile_prompt
from .planning_policy import classify_delivery_policy, classify_planning_policy
from .pull_request import GitHubCLIProvider, build_pull_request_body, default_pull_request_title
from .util import make_id, require_binary, run, sql_literal
from .workspace import GitWorkspace


def _store() -> DoltStore:
    cfg = global_config()
    store = DoltStore(cfg)
    store.bootstrap()
    return store


def _preflight_project_runtime(project: dict[str, Any]) -> None:
    """Fail before creating durable work if the local runtime is incomplete."""
    require_binary("git")
    require_binary("dolt")
    # Managed workers call the praector CLI to request durable child tasks/results.
    require_binary("praector")
    cfg = project_config_from_row(project)
    if cfg.harness != "opencode":
        raise RuntimeError(f"Unsupported harness in the current release: {cfg.harness!r} (expected 'opencode')")
    cfg.resolved_opencode()
    if not cfg.root.exists():
        raise RuntimeError(f"Project repository does not exist: {cfg.root}")
    run(["git", "-C", str(cfg.root), "rev-parse", "--is-inside-work-tree"])


def _print_blocked_diagnostics(store: DoltStore, collection_id: str) -> None:
    tasks = store.list_tasks(collection_id)
    active = [t for t in tasks if t["status"] not in {"COMPLETE", "CANCELLED", "SUPERSEDED"}]
    if not active:
        return
    print("Blocked work:", file=sys.stderr)
    for task in active:
        result = task.get("result") or {}
        summary = str(result.get("summary") or "").strip()
        suffix = f": {summary}" if summary else ""
        print(
            f"  {task['id']} [{task['status']}/{task['profile']}] {task['title']}{suffix}",
            file=sys.stderr,
        )
        attempts = store.query(
            "SELECT * FROM attempts WHERE task_id=" + sql_literal(task["id"]) + " ORDER BY attempt_no DESC LIMIT 1"
        )
        if attempts:
            attempt = attempts[0]
            log_path = attempt.get("log_path") or "-"
            print(
                f"    latest attempt: {attempt['id']} status={attempt['status']} exit={attempt.get('exit_code')} log={log_path}",
                file=sys.stderr,
            )


def _project_from_context(store: DoltStore, identifier: str | None = None) -> dict[str, Any]:
    if identifier:
        return store.get_project(identifier)
    env_id = os.environ.get("PRAECTOR_PROJECT_ID")
    if env_id:
        return store.get_project(env_id)
    marker = find_project_marker()
    if not marker:
        raise RuntimeError("No Praector project found. Run `praector init` in a Git repository or pass --project.")
    cfg = read_project_marker(marker)
    # The repository marker is the user-editable project configuration. Sync it
    # into durable state when commands are run from the project.
    return store.register_project(cfg)


def _current_task_id(explicit: str | None = None) -> str:
    task_id = explicit or os.environ.get("PRAECTOR_TASK_ID")
    if not task_id:
        raise RuntimeError("No current task. Pass --task or run this command from a Praector-managed OpenCode task.")
    return task_id


def _latest_collection(store: DoltStore, project_id: str) -> dict[str, Any]:
    rows = store.list_collections(project_id)
    if not rows:
        raise RuntimeError("Project has no collections yet")
    return rows[0]


def cmd_init(args: argparse.Namespace) -> int:
    require_binary("git")
    require_binary("dolt")
    opencode = args.opencode or shutil.which("opencode") or "opencode"
    root = Path(run(["git", "rev-parse", "--show-toplevel"]).stdout.strip()).resolve()
    marker = root / ".praector" / "project.toml"
    if marker.exists():
        cfg = read_project_marker(marker)
    else:
        branch = args.base_branch or run(["git", "-C", str(root), "branch", "--show-current"]).stdout.strip()
        if not branch:
            branch = "main"
        cfg = ProjectConfig(
            project_id=make_id("PRJ"),
            name=args.name or root.name,
            root=root,
            base_branch=branch,
            opencode_command=opencode,
        )
    if args.name:
        cfg.name = args.name
    if args.base_branch:
        cfg.base_branch = args.base_branch
    if args.opencode:
        cfg.opencode_command = args.opencode
    marker = write_project_marker(cfg)
    # Keep local runtime metadata out of source control by default.
    exclude_path = run(["git", "-C", str(root), "rev-parse", "--git-path", "info/exclude"]).stdout.strip()
    exclude = Path(exclude_path)
    if not exclude.is_absolute():
        exclude = (root / exclude).resolve()
    exclude.parent.mkdir(parents=True, exist_ok=True)
    text = exclude.read_text(encoding="utf-8") if exclude.exists() else ""
    if ".praector/" not in text.splitlines():
        with exclude.open("a", encoding="utf-8") as fh:
            if text and not text.endswith("\n"):
                fh.write("\n")
            fh.write(".praector/\n")
    store = _store()
    store.register_project(cfg)
    print(f"Initialized Praector project {cfg.name} ({cfg.project_id})")
    print(f"Project marker: {marker}")
    print(f"Base branch: {cfg.base_branch}")
    print(f"OpenCode: {cfg.opencode_command}")
    return 0


def cmd_profile_list(args: argparse.Namespace) -> int:
    from .config import READ_ONLY_PROFILES

    for profile in PROFILE_NAMES:
        mode = "read-only" if profile in READ_ONLY_PROFILES else "write"
        print(f"{profile:12} {mode:9} {PROFILE_TITLES[profile]}")
    return 0


def cmd_profile_show(args: argparse.Namespace) -> int:
    print("# Inherited global engineering contract")
    print()
    print(load_global_prompt())
    print()
    print("# Role-specific operating contract")
    print()
    print(load_profile_prompt(args.profile))
    return 0


def cmd_project_list(args: argparse.Namespace) -> int:
    store = _store()
    for p in store.list_projects():
        print(f"{p['id']}  {p['name']}  {p['root_path']}")
    return 0


def cmd_project_show(args: argparse.Namespace) -> int:
    store = _store()
    p = _project_from_context(store, args.project)
    print(json.dumps(p, indent=2, default=str))
    return 0


def cmd_run(args: argparse.Namespace) -> int:
    store = _store()
    project = _project_from_context(store, args.project)
    _preflight_project_runtime(project)
    title = args.title or " ".join(args.objective.split())[:80]
    # Collection + required coordinator are a single durable bootstrap. Workspace
    # preparation is deferred to the scheduler so an interrupted Git operation can
    # fail/retry as an ordinary task attempt instead of orphaning the collection.
    coordinator_objective = (
        f"Own the initial decomposition of the new collection: {args.objective}. "
        "Inspect project context only as needed. Create durable critic/plan_review/research/implement/review/advise tasks using praector as appropriate. "
        "Do not perform implementation or substantive specialist work yourself. Ensure implementation tasks are sequenced by dependencies; "
        "independent review is automatically enforced for implementation tasks."
    )
    collection, coordinator = store.create_collection_with_coordinator(
        project["id"], title, args.objective, coordinator_objective
    )
    print(f"Created {collection['id']} in project {project['name']}")
    print(f"Objective: {collection['objective']}")
    print(f"Integration branch: {collection['integration_branch']}")
    print(f"Coordinator task: {coordinator['id']}")
    if args.detach:
        print("Queued. Run `praector daemon` to execute pending work.")
        return 0
    state = Scheduler(store, global_config(), max_workers=args.max_workers).run_collection(collection["id"])
    print(f"Collection {collection['id']}: {state}")
    if state == "BLOCKED":
        _print_blocked_diagnostics(store, collection["id"])
    return 0 if state == "COMPLETE" else 2


def _print_collection_status(store: DoltStore, collection: dict[str, Any]) -> None:
    tasks = store.list_tasks(collection["id"])
    counts: dict[str, int] = {}
    for t in tasks:
        counts[t["status"]] = counts.get(t["status"], 0) + 1
    summary = ", ".join(f"{k}={v}" for k, v in sorted(counts.items())) or "no tasks (invalid/orphaned; `praector daemon --once` will repair)"
    print(f"{collection['id']}  {collection['status']}  {collection['title']}")
    print(f"  objective: {collection['objective']}")
    print(f"  branch: {collection.get('integration_branch') or '-'}")
    acceptance = store.get_collection_acceptance(collection["id"])
    if acceptance:
        print(
            f"  accepted: {acceptance['target_branch']} @ {str(acceptance['accepted_commit'])[:12]} "
            f"({acceptance['merge_mode']}, {acceptance['created_at']})"
        )
    pull_request = store.get_collection_pull_request(collection["id"])
    if pull_request:
        draft = " draft" if bool(pull_request.get("is_draft")) else ""
        print(
            f"  pr: {pull_request['provider']} #{pull_request['pr_number']} "
            f"[{pull_request['state']}{draft}] {pull_request['url']}"
        )
    revisions = store.list_collection_revisions(collection["id"])
    if revisions:
        counts_rev: dict[str, int] = {}
        for revision in revisions:
            counts_rev[revision["status"]] = counts_rev.get(revision["status"], 0) + 1
        print("  revisions: " + ", ".join(f"{k}={v}" for k, v in sorted(counts_rev.items())))
        for revision in revisions:
            if revision["status"] == "OPEN":
                print(f"    {revision['id']} [OPEN] {revision['reason']}")
    print(f"  tasks: {summary}")
    # Surface actionable task IDs directly. A collection ID and task ID are
    # independent identifiers; users should never have to guess the latter.
    interesting = [
        t for t in tasks
        if t["status"] in {"FAILED", "RUNNING", "WAITING", "VERIFYING", "READY", "PENDING"}
    ]
    for t in interesting:
        print(f"    {t['id']} [{t['status']}/{t['profile']}] {t['title']}")


def cmd_status(args: argparse.Namespace) -> int:
    store = _store()
    target = args.target
    if target and target.startswith("COL-"):
        store.update_collection_terminal_state(target)
        _print_collection_status(store, store.get_collection(target))
        return 0
    if target and target.startswith("T-"):
        task = store.get_task(target)
        print(json.dumps(task, indent=2, default=str))
        return 0
    project = _project_from_context(store, args.project or target)
    print(f"Project {project['name']} ({project['id']})")
    for collection in store.list_collections(project["id"]):
        store.update_collection_terminal_state(collection["id"])
        _print_collection_status(store, store.get_collection(collection["id"]))
    return 0


def cmd_graph(args: argparse.Namespace) -> int:
    store = _store()
    if args.collection:
        collection = store.get_collection(args.collection)
    else:
        project = _project_from_context(store, args.project)
        collection = _latest_collection(store, project["id"])
    tasks = store.list_tasks(collection["id"])
    edges = store.list_edges(collection["id"])
    deps: dict[str, list[str]] = {}
    for e in edges:
        if e["relation"] == "depends_on":
            deps.setdefault(e["to_task_id"], []).append(e["from_task_id"])
    print(f"{collection['id']} {collection['title']} [{collection['status']}]")
    for t in tasks:
        dep_text = ",".join(deps.get(t["id"], [])) or "-"
        print(f"  {t['id']} [{t['status']:<10}] {t['profile']:<11} {t['title']}  deps={dep_text}")
    return 0


def cmd_daemon(args: argparse.Namespace) -> int:
    store = _store()
    print("praector daemon running (Ctrl-C to stop)")
    try:
        Scheduler(store, global_config(), max_workers=args.max_workers).daemon(once=args.once)
    except KeyboardInterrupt:
        print("Stopped.")
    return 0


def cmd_task_show(args: argparse.Namespace) -> int:
    store = _store()
    task = store.get_task(args.task_id)
    task["dependencies"] = [d["id"] for d in store.dependencies(args.task_id)]
    task["dependents"] = [d["id"] for d in store.dependents(args.task_id)]
    print(json.dumps(task, indent=2, default=str))
    return 0



def cmd_task_list(args: argparse.Namespace) -> int:
    store = _store()
    if args.collection:
        collection = store.get_collection(args.collection)
    else:
        project = _project_from_context(store, args.project)
        collection = _latest_collection(store, project["id"])
    for task in store.list_tasks(collection["id"]):
        deps = [d["id"] for d in store.dependencies(task["id"])]
        dep_text = ",".join(deps) if deps else "-"
        print(f"{task['id']} [{task['status']}/{task['profile']}] {task['title']} deps={dep_text}")
    return 0

def cmd_task_retry(args: argparse.Namespace) -> int:
    store = _store()
    failed = store.get_task(args.task_id)
    project = store.get_project(failed["project_id"])
    _preflight_project_runtime(project)
    replacement = store.retry_failed_task(args.task_id, args.reason)
    print(f"{args.task_id} -> {replacement['id']}")
    print(f"Collection {failed['collection_id']} resumed")
    if args.detach:
        print("Queued. Run `praector daemon` to execute pending work.")
        return 0
    state = Scheduler(store, global_config(), max_workers=args.max_workers).run_collection(failed["collection_id"])
    print(f"Collection {failed['collection_id']}: {state}")
    if state == "BLOCKED":
        _print_blocked_diagnostics(store, failed["collection_id"])
    return 0 if state == "COMPLETE" else 2


def _require_coordinator(store: DoltStore, explicit_task: str | None = None) -> dict[str, Any]:
    caller_id = _current_task_id(explicit_task)
    caller = store.get_task(caller_id)
    if caller["profile"] != "coordinator":
        raise PermissionError("This graph mutation is restricted to the coordinator profile")
    return caller


def cmd_task_add_dependency(args: argparse.Namespace) -> int:
    store = _store()
    caller = _require_coordinator(store, args.caller)
    task = store.get_task(args.task_id)
    dep = store.get_task(args.dependency_id)
    if task["collection_id"] != caller["collection_id"] or dep["collection_id"] != caller["collection_id"]:
        raise PermissionError("Coordinator may only mutate its current collection")
    store.add_dependency(args.task_id, args.dependency_id)
    print(f"{args.task_id} depends_on {args.dependency_id}")
    return 0


def cmd_task_supersede(args: argparse.Namespace) -> int:
    store = _store()
    caller = _require_coordinator(store, args.caller)
    old = store.get_task(args.old_task_id)
    replacement = store.get_task(args.replacement_task_id)
    if old["collection_id"] != caller["collection_id"] or replacement["collection_id"] != caller["collection_id"]:
        raise PermissionError("Coordinator may only mutate its current collection")
    store.supersede_task(args.old_task_id, args.replacement_task_id, args.reason)
    print(f"{args.old_task_id} -> {args.replacement_task_id}")
    return 0


def cmd_context_show(args: argparse.Namespace) -> int:
    store = _store()
    task_id = _current_task_id(args.task)
    task = store.get_task(task_id)
    project = store.get_project(task["project_id"])
    collection = store.get_collection(task["collection_id"])
    parent = store.get_parent(task)
    payload = {
        "execution": {
            "assigned_worktree": task.get("worktree_path") or os.environ.get("PRAECTOR_WORKTREE"),
            "rule": "Use assigned_worktree for all repository/file/shell operations; project.root_path is reference metadata only.",
        },
        "project": project,
        "collection": collection,
        "task": task,
        "parent": parent,
        "dependencies": store.dependencies(task_id),
        "tasks": [
            {"id": t["id"], "profile": t["profile"], "title": t["title"], "status": t["status"], "result": t.get("result", {})}
            for t in store.list_tasks(collection["id"])
        ],
        "memories": store.list_memories(project["id"], collection["id"]),
        "decisions": store.list_decisions(project["id"], collection["id"]),
    }
    print(json.dumps(payload, indent=2, default=str))
    return 0


def _limits_for_project(store: DoltStore, project_id: str) -> dict[str, int]:
    cfg = project_config_from_row(store.get_project(project_id))
    return {
        "max_task_depth": cfg.limits.max_task_depth,
        "max_tasks_per_collection": cfg.limits.max_tasks_per_collection,
        "max_children_per_task": cfg.limits.max_children_per_task,
        "max_attempts_per_task": cfg.limits.max_attempts_per_task,
    }


def cmd_task_request(args: argparse.Namespace) -> int:
    store = _store()
    caller_id = _current_task_id(args.task)
    caller = store.get_task(caller_id)
    workspace_mode = args.workspace
    if workspace_mode == "auto":
        workspace_mode = "inherit" if caller["profile"] == "implement" else "collection"
    if caller["profile"] == "implement" and args.profile == "implement" and workspace_mode == "inherit" and not args.blocking:
        raise RuntimeError("An inherited implementation child must be --blocking so its branch can safely merge back into the parent.")
    base_commit = None
    if workspace_mode == "inherit":
        base_commit = GitWorkspace(store, global_config()).checkpoint_for_delegation(caller_id)
    task, created = store.request_task(
        caller_id,
        profile=args.profile,
        title=args.title,
        objective=args.objective,
        reason=args.reason,
        blocking=args.blocking,
        depends_on=args.depends_on or [],
        workspace_mode=workspace_mode,
        base_commit=base_commit,
        limits=_limits_for_project(store, caller["project_id"]),
    )
    # Print ID first and keep output deliberately terse for agent callers.
    print(task["id"])
    if not created:
        print("deduplicated", file=sys.stderr)
    return 0


def cmd_coordinator_attention(args: argparse.Namespace) -> int:
    store = _store()
    caller_id = _current_task_id(args.task)
    caller = store.get_task(caller_id)
    task, _ = store.request_coordinator_attention(
        caller_id,
        args.reason,
        blocking=args.blocking,
        limits=_limits_for_project(store, caller["project_id"]),
    )
    print(task["id"])
    return 0


def cmd_result_show(args: argparse.Namespace) -> int:
    store = _store()
    task_id = _current_task_id(args.task_id)
    task = store.get_task(task_id)
    print(json.dumps(task.get("result") or {}, indent=2, default=str))
    return 0


def cmd_result_submit(args: argparse.Namespace) -> int:
    store = _store()
    task_id = _current_task_id(args.task)
    if args.stdin:
        raw = sys.stdin.read()
        result = json.loads(raw)
    elif args.file:
        result = json.loads(Path(args.file).read_text(encoding="utf-8"))
    elif args.summary:
        result = {"status": args.status, "summary": args.summary}
    else:
        raise RuntimeError("Use --stdin, --file, or --summary")
    if "status" not in result:
        result["status"] = args.status
    task_before = store.get_task(task_id)
    if task_before["profile"] in {"review", "plan_review"} and str(result.get("status", "complete")).lower() in {"complete", "completed"}:
        verdict = str(result.get("verdict", "")).strip().lower()
        if verdict not in {"pass", "fail"}:
            raise ValueError("A completed review/plan_review result must include verdict='pass' or verdict='fail'")
        result["verdict"] = verdict
    task = store.submit_result(task_id, result)
    print(task["status"])
    return 0


def cmd_memory_add(args: argparse.Namespace) -> int:
    store = _store()
    task_id = args.task or os.environ.get("PRAECTOR_TASK_ID")
    if task_id:
        task = store.get_task(task_id)
        project_id, collection_id = task["project_id"], task["collection_id"]
    else:
        project = _project_from_context(store, args.project)
        project_id, collection_id = project["id"], args.collection
    row = store.add_memory(
        project_id=project_id, collection_id=collection_id if args.scope == "collection" else None,
        task_id=task_id, scope=args.scope, state=args.state, kind=args.kind, text=args.text,
    )
    print(row["id"])
    return 0


def cmd_memory_list(args: argparse.Namespace) -> int:
    store = _store()
    project = _project_from_context(store, args.project)
    collection_id = args.collection
    for m in store.list_memories(project["id"], collection_id, state=args.state):
        print(f"{m['id']} [{m['scope']}/{m['state']}/{m['kind']}] {m['text']}")
    return 0


def cmd_decision_add(args: argparse.Namespace) -> int:
    store = _store()
    task_id = args.task or os.environ.get("PRAECTOR_TASK_ID")
    if task_id:
        task = store.get_task(task_id)
        project_id, collection_id = task["project_id"], task["collection_id"]
    else:
        project = _project_from_context(store, args.project)
        project_id, collection_id = project["id"], args.collection
    row = store.add_decision(
        project_id=project_id, collection_id=collection_id, task_id=task_id,
        statement=args.statement, rationale=args.rationale,
    )
    print(row["id"])
    return 0


def _tail_file(path: str | None, lines: int, *, prefix: str = "") -> None:
    if not path or lines <= 0:
        return
    p = Path(path)
    if not p.exists():
        print(f"{prefix}[log file missing: {p}]")
        return
    content = p.read_text(encoding="utf-8", errors="replace").splitlines()
    print(f"{prefix}--- last {min(lines, len(content))} log lines: {p} ---")
    for line in content[-lines:]:
        print(prefix + line)


def _print_task_logs(
    store: DoltStore, task: dict[str, Any], *, heading: bool = False, tail: int = 0
) -> int:
    rows = store.query(
        "SELECT * FROM attempts WHERE task_id=" + sql_literal(task["id"]) + " ORDER BY attempt_no"
    )
    if heading:
        print(f"{task['id']} [{task['status']}/{task['profile']}] {task['title']}")
    if not rows:
        if heading:
            print("  no attempts")
        else:
            print("no attempts")
        return 0
    prefix = "  " if heading else ""
    for row in rows:
        print(
            f"{prefix}{row['id']} attempt={row['attempt_no']} status={row['status']} "
            f"exit={row.get('exit_code')} log={row.get('log_path') or '-'}"
        )
    if tail:
        _tail_file(rows[-1].get("log_path"), tail, prefix=prefix)
    return len(rows)


def _validate_collection_for_acceptance(store: DoltStore, collection: dict[str, Any]) -> dict[str, Any]:
    state = store.update_collection_terminal_state(collection["id"])
    collection = store.get_collection(collection["id"])
    if state != "COMPLETE":
        critic_state = store.critic_revalidation_state(collection["id"])
        policy = classify_planning_policy(collection.get("objective") or "")
        if policy.critic_required and critic_state.get("stale"):
            raise RuntimeError(
                f"Collection {collection['id']} has a stale required critic: sealed material-plan "
                f"fingerprint {critic_state.get('sealed_fingerprint')} != current "
                f"{critic_state.get('current_material_plan_fingerprint')}. Re-run orchestration "
                "to complete critic revalidation before acceptance."
            )
        raise RuntimeError(f"Collection {collection['id']} is {state}, not COMPLETE")

    critic_state = store.critic_revalidation_state(collection["id"])
    policy = classify_planning_policy(collection.get("objective") or "")
    if policy.critic_required and critic_state.get("stale"):
        raise RuntimeError(
            f"Collection {collection['id']} has a stale required critic; acceptance requires a fresh critic seal."
        )

    tasks = store.list_tasks(collection["id"])
    edges = store.list_edges(collection["id"])
    tasks_by_id = {t["id"]: t for t in tasks}

    # Historical semantic failures remain durable evidence after repair. A
    # completed review must still carry an explicit pass/fail verdict, but a
    # historical fail does not poison acceptance when later fresh review lineage
    # proves every completed implementation has an independent pass.
    for task in tasks:
        if task["profile"] != "review" or task["status"] != "COMPLETE":
            continue
        verdict = str((task.get("result") or {}).get("verdict", "")).strip().lower()
        if verdict not in {"pass", "fail"}:
            raise RuntimeError(
                f"Review {task['id']} is COMPLETE but has verdict={verdict or '<missing>'}; "
                "acceptance requires an explicit pass/fail verdict"
            )

    verifies: dict[str, list[str]] = {}
    for edge in edges:
        if edge["relation"] == "verifies":
            verifies.setdefault(edge["from_task_id"], []).append(edge["to_task_id"])

    # Every completed implementation needs a passing independent review. Repair
    # implementations created by a failed review are verified by their parent
    # review when that review resumes and ultimately passes.
    for task in tasks:
        if task["profile"] != "implement" or task["status"] != "COMPLETE":
            continue
        candidates = list(verifies.get(task["id"], []))
        parent_id = task.get("parent_task_id")
        parent = tasks_by_id.get(parent_id, {}) if parent_id else {}
        if (
            parent.get("profile") == "review"
            and str((parent.get("result") or {}).get("verdict", "")).strip().lower() == "pass"
        ):
            candidates.append(parent_id)
        passed = False
        for review_id in candidates:
            review = tasks_by_id.get(review_id)
            if not review or review.get("status") != "COMPLETE":
                continue
            if str((review.get("result") or {}).get("verdict", "")).strip().lower() == "pass":
                passed = True
                break
        if not passed:
            raise RuntimeError(
                f"Implementation {task['id']} has no completed independent review with verdict=pass"
            )

    return {"collection": collection, "tasks": tasks}


def _accept_actor(project: dict[str, Any]) -> str:
    repo = Path(project["root_path"])
    proc = run(["git", "-C", str(repo), "config", "user.name"], check=False)
    value = proc.stdout.strip()
    return value or getpass.getuser()


def cmd_collection_show(args: argparse.Namespace) -> int:
    store = _store()
    collection = store.get_collection(args.collection_id)
    collection["planning_policy"] = classify_planning_policy(collection.get("objective") or "").as_dict()
    collection["delivery_policy"] = classify_delivery_policy(collection.get("objective") or "").as_dict()
    collection["acceptances"] = store.list_collection_acceptances(collection["id"])
    collection["pull_requests"] = store.list_collection_pull_requests(collection["id"])
    collection["revisions"] = store.list_collection_revisions(collection["id"])
    collection["tasks"] = store.list_tasks(collection["id"])
    collection["critic_revalidation"] = store.critic_revalidation_state(collection["id"])
    print(json.dumps(collection, indent=2, default=str))
    return 0


def cmd_collection_revise(args: argparse.Namespace) -> int:
    store = _store()
    collection = store.get_collection(args.collection_id)
    project = store.get_project(collection["project_id"])
    _preflight_project_runtime(project)
    state = store.update_collection_terminal_state(collection["id"])
    if state != "COMPLETE":
        raise RuntimeError(f"Collection {collection['id']} is {state}, not COMPLETE")
    revision, coordinator = store.request_collection_revision(collection["id"], args.reason)
    print(f"Revision {revision['id']} opened for {collection['id']}")
    print(f"Coordinator task: {coordinator['id']}")
    print(f"Reason: {revision['reason']}")
    if args.detach:
        print("Queued. Run `praector daemon` to execute revision work.")
        return 0
    state = Scheduler(store, global_config(), max_workers=args.max_workers).run_collection(collection["id"])
    print(f"Collection {collection['id']}: {state}")
    if state == "BLOCKED":
        _print_blocked_diagnostics(store, collection["id"])
    return 0 if state == "COMPLETE" else 2


def cmd_collection_diff(args: argparse.Namespace) -> int:
    store = _store()
    collection = store.get_collection(args.collection_id)
    project = store.get_project(collection["project_id"])
    repo = Path(project["root_path"])
    target = args.target or project["base_branch"]
    diff_args = ["git", "-C", str(repo), "diff"]
    if args.stat:
        diff_args.append("--stat")
    diff_args.append(f"{target}...{collection['integration_branch']}")
    proc = run(diff_args)
    print(proc.stdout, end="")
    return 0


def _resolve_pr_remote(repo: Path, base_branch: str, explicit: str | None = None) -> str:
    remotes = [line.strip() for line in run(["git", "-C", str(repo), "remote"]).stdout.splitlines() if line.strip()]
    if explicit:
        if explicit not in remotes:
            raise RuntimeError(f"Git remote not found: {explicit}")
        return explicit
    configured = run(
        ["git", "-C", str(repo), "config", "--get", f"branch.{base_branch}.remote"],
        check=False,
    ).stdout.strip()
    if configured and configured != "." and configured in remotes:
        return configured
    if "origin" in remotes:
        return "origin"
    if len(remotes) == 1:
        return remotes[0]
    if not remotes:
        raise RuntimeError("Repository has no Git remote. Add a GitHub remote before creating a PR.")
    raise RuntimeError("Unable to choose a Git remote. Pass --remote explicitly.")


def _pull_request_provider(repo: Path) -> GitHubCLIProvider:
    return GitHubCLIProvider(repo)


def _print_pr_plan(
    *,
    collection: dict[str, Any],
    source_branch: str,
    source_commit: str,
    base_branch: str,
    remote: str,
    title: str,
    body: str,
    draft: bool,
    existing: dict[str, Any] | None = None,
) -> None:
    print(f"Collection: {collection['id']}  {collection['title']}")
    print(f"Head:       {source_branch} @ {source_commit[:12]}")
    print(f"Base:       {base_branch}")
    print(f"Remote:     {remote}")
    if existing:
        print(f"Existing:   {existing['provider']} #{existing['pr_number']} [{existing['state']}] {existing['url']}")
    else:
        print(f"Draft:      {'yes' if draft else 'no'}")
    print(f"Title:      {title}")
    print("Body:")
    print(body.rstrip())


def cmd_collection_pr(args: argparse.Namespace) -> int:
    store = _store()
    collection = store.get_collection(args.collection_id)
    project = store.get_project(collection["project_id"])
    validated = _validate_collection_for_acceptance(store, collection)
    collection = validated["collection"]
    tasks = validated["tasks"]
    existing_record = store.get_collection_pull_request(collection["id"], "github")

    base = args.base or (existing_record.get("base_branch") if existing_record else None) or project["base_branch"]
    workspace = GitWorkspace(store, global_config())
    integration_path = collection.get("integration_worktree")
    if integration_path and Path(integration_path).exists() and workspace.is_dirty(Path(integration_path)):
        raise RuntimeError(
            f"Collection integration worktree is dirty: {integration_path}. "
            "Resolve or checkpoint those changes before creating/updating a PR."
        )
    plan = workspace.acceptance_plan(project, collection, target_branch=base)
    repo = Path(project["root_path"]).resolve()
    remote = args.remote or (existing_record.get("remote_name") if existing_record else None)
    remote = _resolve_pr_remote(repo, base, remote)
    title = args.title or (existing_record.get("title") if existing_record else None) or default_pull_request_title(collection, tasks)
    body = build_pull_request_body(collection, tasks, source_commit=plan["source_commit"])

    if args.dry_run:
        if not existing_record and (store.get_collection_acceptance(collection["id"]) or plan["already_contained"]):
            raise RuntimeError(
                f"Collection branch {plan['source_branch']} is already contained in {base}; "
                "there is no new candidate diff from which to create a PR."
            )
        _print_pr_plan(
            collection=collection,
            source_branch=plan["source_branch"],
            source_commit=plan["source_commit"],
            base_branch=base,
            remote=remote,
            title=title,
            body=body,
            draft=args.draft,
            existing=existing_record,
        )
        print("Dry run: no branch was pushed and no pull request was created or changed.")
        return 0

    provider = _pull_request_provider(repo)
    repository = provider.repository()
    actor = _accept_actor(project)

    remote_pr: dict[str, Any] | None = None
    if existing_record:
        remote_pr = provider.view(int(existing_record["pr_number"]))
        if remote_pr["head_branch"] != plan["source_branch"]:
            raise RuntimeError(
                f"Recorded PR #{remote_pr['number']} uses head {remote_pr['head_branch']}, "
                f"not collection branch {plan['source_branch']}"
            )
        if remote_pr["base_branch"] != base:
            raise RuntimeError(
                f"Recorded PR #{remote_pr['number']} targets {remote_pr['base_branch']}, not {base}. "
                f"Rerun with --base {remote_pr['base_branch']} to use the existing PR."
            )
        if args.title and args.title != remote_pr["title"]:
            raise RuntimeError("--title is creation-only for an existing PR; edit the PR in GitHub or omit --title.")
        if args.draft and not remote_pr["is_draft"]:
            raise RuntimeError("--draft is creation-only and the existing PR is already ready for review.")
    else:
        remote_pr = provider.find_for_head(plan["source_branch"])
        if remote_pr and remote_pr["base_branch"] != base:
            raise RuntimeError(
                f"Existing GitHub PR #{remote_pr['number']} for {plan['source_branch']} targets "
                f"{remote_pr['base_branch']}, not {base}. Rerun with --base {remote_pr['base_branch']} to adopt it."
            )

    if remote_pr and remote_pr["state"] != "OPEN":
        if existing_record and existing_record.get("source_commit") != plan["source_commit"]:
            raise RuntimeError(
                f"Existing PR #{remote_pr['number']} is {remote_pr['state']} but the collection branch has changed "
                f"from {str(existing_record.get('source_commit') or '')[:12]} to {plan['source_commit'][:12]}. "
                "Create a new collection for follow-up work rather than reusing a closed/merged PR."
            )
        record = store.record_collection_pull_request(
            project_id=project["id"], collection_id=collection["id"], provider="github",
            remote_name=remote, repository=repository.get("name"), pr_number=remote_pr["number"],
            url=remote_pr["url"], state=remote_pr["state"], is_draft=remote_pr["is_draft"],
            base_branch=remote_pr["base_branch"], head_branch=remote_pr["head_branch"],
            source_commit=plan["source_commit"], title=remote_pr["title"], actor=actor,
        )
        print(f"PR already exists: #{record['pr_number']} [{record['state']}]")
        print(record["url"])
        if args.open:
            provider.open(int(record["pr_number"]))
        return 0

    if not remote_pr:
        if store.get_collection_acceptance(collection["id"]):
            raise RuntimeError("Collection has already been accepted directly; refusing to create a redundant PR.")
        if plan["already_contained"]:
            raise RuntimeError(
                f"Collection branch {plan['source_branch']} is already contained in {base}; "
                "GitHub would have no new diff to review."
            )

    # Push only an acceptance-grade collection integration branch. Revisions keep
    # the same head branch, so an existing open PR updates naturally here.
    run(
        ["git", "-C", str(repo), "push", "--set-upstream", remote, f"{plan['source_branch']}:{plan['source_branch']}"],
    )

    if not remote_pr:
        remote_pr = provider.create(
            base_branch=base,
            head_branch=plan["source_branch"],
            title=title,
            body=body,
            draft=args.draft,
        )
        created = True
    else:
        if args.refresh_copy:
            remote_pr = provider.update_body(int(remote_pr["number"]), body)
        else:
            # Refresh after the push so state/title/draft metadata reflects GitHub.
            remote_pr = provider.view(int(remote_pr["number"]))
        created = False

    record = store.record_collection_pull_request(
        project_id=project["id"],
        collection_id=collection["id"],
        provider="github",
        remote_name=remote,
        repository=repository.get("name"),
        pr_number=remote_pr["number"],
        url=remote_pr["url"],
        state=remote_pr["state"],
        is_draft=remote_pr["is_draft"],
        base_branch=remote_pr["base_branch"],
        head_branch=remote_pr["head_branch"],
        source_commit=plan["source_commit"],
        title=remote_pr["title"],
        actor=actor,
    )
    verb = "created" if created else "already exists"
    draft_text = " draft" if bool(record.get("is_draft")) else ""
    print(f"PR #{record['pr_number']} {verb}: [{record['state']}{draft_text}]")
    print(record["url"])
    if args.open:
        provider.open(int(record["pr_number"]))
    return 0


def cmd_collection_accept(args: argparse.Namespace) -> int:
    if args.delete_branches and not args.cleanup:
        raise ValueError("--delete-branches requires --cleanup")
    store = _store()
    collection = store.get_collection(args.collection_id)
    project = store.get_project(collection["project_id"])
    _validate_collection_for_acceptance(store, collection)
    workspace = GitWorkspace(store, global_config())
    target = args.target or project["base_branch"]
    existing = store.get_collection_acceptance(collection["id"], target)
    plan = workspace.acceptance_plan(project, collection, target_branch=target)

    print(f"Collection: {collection['id']}  {collection['title']}")
    print(f"Source:     {plan['source_branch']} @ {plan['source_commit'][:12]}")
    print(f"Target:     {plan['target_branch']} @ {plan['target_commit'][:12]}")
    if plan["diff_stat"]:
        print("Changes:")
        for line in plan["diff_stat"].splitlines():
            print(f"  {line}")
    else:
        print("Changes:    none relative to target")

    if existing:
        if existing["source_commit"] != plan["source_commit"]:
            raise RuntimeError(
                f"Collection was previously accepted from {existing['source_commit'][:12]}, but its branch now points to "
                f"{plan['source_commit'][:12]}. Refusing to silently re-accept mutated completed work."
            )
        if not plan["already_contained"]:
            raise RuntimeError(
                f"Acceptance record exists for {target}, but the target no longer contains the accepted source commit"
            )
        print(
            f"Already accepted into {target} at {str(existing['accepted_commit'])[:12]} "
            f"({existing['merge_mode']})."
        )
        return 0

    if args.dry_run:
        mode = "already-contained" if plan["already_contained"] else ("no-ff" if args.no_ff else "ff-only")
        if mode == "ff-only" and not plan["fast_forward"]:
            raise RuntimeError(
                f"Target {target} has diverged; dry-run shows that --ff-only acceptance would fail. "
                "Use --no-ff only after reviewing the divergence."
            )
        print(f"Dry run: acceptance is valid; merge mode would be {mode}.")
        return 0

    merge = workspace.accept_collection(project, collection, target_branch=target, no_ff=args.no_ff)
    acceptance = store.record_collection_acceptance(
        project_id=project["id"],
        collection_id=collection["id"],
        target_branch=merge["target_branch"],
        source_branch=merge["source_branch"],
        source_commit=merge["source_commit"],
        target_before_commit=merge["target_before_commit"],
        accepted_commit=merge["accepted_commit"],
        merge_mode=merge["merge_mode"],
        actor=_accept_actor(project),
    )
    print(
        f"Accepted {collection['id']} into {acceptance['target_branch']} at "
        f"{str(acceptance['accepted_commit'])[:12]} ({acceptance['merge_mode']})."
    )
    if args.cleanup:
        cleaned = workspace.cleanup_collection(project, collection, delete_branches=args.delete_branches)
        print(f"Cleaned worktrees: {len(cleaned['worktrees'])}")
        if args.delete_branches:
            print(f"Deleted merged branches: {len(cleaned['branches'])}")
    return 0


def cmd_collection_cleanup(args: argparse.Namespace) -> int:
    store = _store()
    collection = store.get_collection(args.collection_id)
    project = store.get_project(collection["project_id"])
    if collection["status"] != "COMPLETE":
        raise RuntimeError("Collection cleanup requires status COMPLETE")
    if not store.get_collection_acceptance(collection["id"]) and not args.force:
        raise RuntimeError("Collection has not been accepted. Pass --force to remove worktrees anyway.")
    cleaned = GitWorkspace(store, global_config()).cleanup_collection(
        project, collection, delete_branches=args.delete_branches
    )
    for path in cleaned["worktrees"]:
        print(f"removed worktree {path}")
    for branch in cleaned["branches"]:
        print(f"deleted branch {branch}")
    if not cleaned["worktrees"] and not cleaned["branches"]:
        print("Nothing to clean.")
    return 0


def cmd_logs(args: argparse.Namespace) -> int:
    store = _store()
    target = args.target
    if target.startswith("COL-"):
        collection = store.get_collection(target)
        tasks = store.list_tasks(collection["id"])
        print(f"{collection['id']} {collection['title']} [{collection['status']}]")
        if not tasks:
            print("  no tasks")
            return 0
        total = 0
        for task in tasks:
            total += _print_task_logs(store, task, heading=True, tail=args.tail)
        if total == 0:
            print("No task attempts have been recorded for this collection.")
        return 0
    task = store.get_task(target)
    _print_task_logs(store, task, tail=args.tail)
    return 0


def cmd_doctor(args: argparse.Namespace) -> int:
    failures = 0
    for binary in ("git", "dolt", "opencode"):
        found = shutil.which(binary)
        if found:
            print(f"✓ {binary}: {found}")
        else:
            print(f"✗ {binary}: not found")
            failures += 1
    gh = shutil.which("gh")
    if gh:
        print(f"✓ gh: {gh} (collection PR delivery available)")
    else:
        print("⚠ gh: not found (required only for `praector collection pr`)")
    try:
        store = _store()
        print(f"✓ Dolt state: {store.state_dir}")
    except Exception as exc:
        print(f"✗ Dolt state: {exc}")
        failures += 1
        store = None
    marker = find_project_marker()
    if marker:
        try:
            cfg = read_project_marker(marker)
            print(f"✓ project: {cfg.name} ({cfg.project_id})")
            if store:
                store.get_project(cfg.project_id)
                print("✓ project registered in Dolt")
            try:
                cfg.resolved_opencode()
                print(f"✓ project OpenCode command: {cfg.opencode_command}")
            except Exception as exc:
                print(f"✗ project OpenCode command: {exc}")
                failures += 1
            active_venv = os.environ.get("VIRTUAL_ENV")
            if active_venv:
                try:
                    venv_path = Path(active_venv).expanduser().resolve()
                    project_root = cfg.root.expanduser().resolve()
                    if not venv_path.is_relative_to(project_root):
                        print(
                            f"⚠ active virtualenv outside project will be stripped from managed worker commands by default: {venv_path}"
                        )
                        print(
                            "  Prefer running Praector from a neutral shell/tool install. "
                            "Set PRAECTOR_SANITIZE_PARENT_VENV=0 only if your OpenCode installation intentionally needs that parent venv."
                        )
                except Exception:
                    pass
        except Exception as exc:
            print(f"✗ project marker: {exc}")
            failures += 1
    else:
        print("⚠ no project marker in current directory tree")
    return 1 if failures else 0


def build_parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(prog="praector", description="Durable, policy-gated agent orchestration")
    p.add_argument("--version", action="version", version=f"praector {__version__}")
    sub = p.add_subparsers(dest="command", required=True)

    sp = sub.add_parser("init", help="Register the current Git repository as a Praector project")
    sp.add_argument("--name")
    sp.add_argument("--base-branch")
    sp.add_argument("--opencode", help="OpenCode executable path/name")
    sp.set_defaults(func=cmd_init)

    sp = sub.add_parser("run", help="Create a collection and execute its evolving work graph")
    sp.add_argument("objective")
    sp.add_argument("--title")
    sp.add_argument("--project")
    sp.add_argument("--detach", action="store_true", help="Queue only; requires `praector daemon`")
    sp.add_argument("--max-workers", type=int)
    sp.set_defaults(func=cmd_run)

    sp = sub.add_parser("status", help="Show project, collection, or task status")
    sp.add_argument("target", nargs="?")
    sp.add_argument("--project")
    sp.set_defaults(func=cmd_status)

    sp = sub.add_parser("graph", help="Show the current durable task graph")
    sp.add_argument("collection", nargs="?")
    sp.add_argument("--project")
    sp.set_defaults(func=cmd_graph)

    sp = sub.add_parser("daemon", help="Run the durable task scheduler")
    sp.add_argument("--once", action="store_true")
    sp.add_argument("--max-workers", type=int)
    sp.set_defaults(func=cmd_daemon)

    sp = sub.add_parser("doctor", help="Validate local dependencies and project registration")
    sp.set_defaults(func=cmd_doctor)

    sp = sub.add_parser("logs", help="List attempt logs for a task or collection")
    sp.add_argument("target", help="Task ID (T-...) or collection ID (COL-...)")
    sp.add_argument("--tail", type=int, default=0, help="Also print the last N lines of each task's latest attempt log")
    sp.set_defaults(func=cmd_logs)

    collection = sub.add_parser("collection", help="Collection lifecycle operations")
    collection_sub = collection.add_subparsers(dest="collection_command", required=True)
    sp = collection_sub.add_parser("show", help="Show a collection including acceptance/revision history")
    sp.add_argument("collection_id")
    sp.set_defaults(func=cmd_collection_show)
    sp = collection_sub.add_parser("revise", help="Reopen a completed, unaccepted collection for mandatory operator-requested repair")
    sp.add_argument("collection_id")
    sp.add_argument("--reason", required=True, help="Concrete acceptance defect or required correction")
    sp.add_argument("--detach", action="store_true", help="Queue the revision without running the collection")
    sp.add_argument("--max-workers", type=int)
    sp.set_defaults(func=cmd_collection_revise)
    sp = collection_sub.add_parser("diff", help="Show collection changes relative to a target branch")
    sp.add_argument("collection_id")
    sp.add_argument("--target")
    sp.add_argument("--stat", action="store_true")
    sp.set_defaults(func=cmd_collection_diff)
    sp = collection_sub.add_parser("pr", help="Push a reviewed collection branch and create/reuse a GitHub pull request")
    sp.add_argument("collection_id")
    sp.add_argument("--draft", action="store_true", help="Create a draft PR (creation only)")
    sp.add_argument("--title", help="Override the generated PR title (creation only)")
    sp.add_argument("--base", help="PR base branch (defaults to project base branch, or existing PR base)")
    sp.add_argument("--remote", help="Git remote to push (auto-detected when unambiguous)")
    sp.add_argument("--dry-run", action="store_true", help="Validate and preview without pushing or changing GitHub/Dolt")
    sp.add_argument("--refresh-copy", action="store_true", help="Replace the body of an existing open PR with the current Praector-generated copy")
    sp.add_argument("--open", action="store_true", help="Open the created/existing PR in the browser")
    sp.set_defaults(func=cmd_collection_pr)
    sp = collection_sub.add_parser("accept", help="Accept reviewed collection work into a target branch")
    sp.add_argument("collection_id")
    sp.add_argument("--target", help="Target branch (defaults to the project's base branch)")
    sp.add_argument("--no-ff", action="store_true", help="Allow a merge commit when target has diverged; safe default is --ff-only")
    sp.add_argument("--dry-run", action="store_true", help="Validate and preview acceptance without modifying Git or Dolt")
    sp.add_argument("--cleanup", action="store_true", help="Remove Praector worktrees after successful acceptance")
    sp.add_argument("--delete-branches", action="store_true", help="With --cleanup, delete merged task/collection branches")
    sp.set_defaults(func=cmd_collection_accept)
    sp = collection_sub.add_parser("cleanup", help="Remove completed collection/task worktrees")
    sp.add_argument("collection_id")
    sp.add_argument("--delete-branches", action="store_true", help="Delete merged task/collection branches (requires acceptance)")
    sp.add_argument("--force", action="store_true", help="Allow worktree cleanup before acceptance; never deletes unaccepted branches")
    sp.set_defaults(func=cmd_collection_cleanup)

    profile = sub.add_parser("profile", help="Inspect built-in runtime profile contracts")
    profile_sub = profile.add_subparsers(dest="profile_command", required=True)
    sp = profile_sub.add_parser("list", help="List runtime profiles and capability mode")
    sp.set_defaults(func=cmd_profile_list)
    sp = profile_sub.add_parser("show", help="Print the inherited global and role-specific contracts for a runtime profile")
    sp.add_argument("profile", choices=PROFILE_NAMES)
    sp.set_defaults(func=cmd_profile_show)

    project = sub.add_parser("project", help="Project operations")
    project_sub = project.add_subparsers(dest="project_command", required=True)
    sp = project_sub.add_parser("list")
    sp.set_defaults(func=cmd_project_list)
    sp = project_sub.add_parser("show")
    sp.add_argument("--project")
    sp.set_defaults(func=cmd_project_show)

    task = sub.add_parser("task", help="Task operations")
    task_sub = task.add_subparsers(dest="task_command", required=True)
    sp = task_sub.add_parser("show")
    sp.add_argument("task_id")
    sp.set_defaults(func=cmd_task_show)
    sp = task_sub.add_parser("list", help="List tasks in a collection (defaults to latest collection)")
    sp.add_argument("--collection")
    sp.add_argument("--project")
    sp.set_defaults(func=cmd_task_list)
    sp = task_sub.add_parser("retry", help="Create a fresh replacement for a FAILED task and preserve prior attempts")
    sp.add_argument("task_id")
    sp.add_argument("--reason", default="Operator-requested retry after task failure")
    sp.add_argument("--detach", action="store_true", help="Queue the replacement without running the collection")
    sp.add_argument("--max-workers", type=int)
    sp.set_defaults(func=cmd_task_retry)
    sp = task_sub.add_parser("request", help="Request durable delegated work")
    sp.add_argument("--task")
    sp.add_argument("--profile", choices=[p for p in PROFILE_NAMES if p != "coordinator"], required=True)
    sp.add_argument("--title", required=True)
    sp.add_argument("--objective", required=True)
    sp.add_argument("--reason", required=True)
    sp.add_argument("--blocking", action="store_true")
    sp.add_argument("--depends-on", action="append", default=[])
    sp.add_argument("--workspace", choices=["auto", "inherit", "collection", "isolated"], default="auto")
    sp.set_defaults(func=cmd_task_request)
    sp = task_sub.add_parser("add-dependency", help="Coordinator-only graph mutation")
    sp.add_argument("task_id")
    sp.add_argument("--on", dest="dependency_id", required=True)
    sp.add_argument("--caller")
    sp.set_defaults(func=cmd_task_add_dependency)
    sp = task_sub.add_parser("supersede", help="Coordinator-only replacement/rewire")
    sp.add_argument("old_task_id")
    sp.add_argument("--with", dest="replacement_task_id", required=True)
    sp.add_argument("--reason", required=True)
    sp.add_argument("--caller")
    sp.set_defaults(func=cmd_task_supersede)

    context = sub.add_parser("context", help="Durable context operations")
    context_sub = context.add_subparsers(dest="context_command", required=True)
    sp = context_sub.add_parser("show")
    sp.add_argument("--task")
    sp.set_defaults(func=cmd_context_show)

    coord = sub.add_parser("coordinator", help="Coordinator escalation")
    coord_sub = coord.add_subparsers(dest="coord_command", required=True)
    sp = coord_sub.add_parser("attention")
    sp.add_argument("--task")
    sp.add_argument("--reason", required=True)
    sp.add_argument("--blocking", action="store_true")
    sp.set_defaults(func=cmd_coordinator_attention)

    result = sub.add_parser("result", help="Structured task results")
    result_sub = result.add_subparsers(dest="result_command", required=True)
    sp = result_sub.add_parser("show", help="Show the durable structured result for a task (defaults to current task)")
    sp.add_argument("task_id", nargs="?")
    sp.set_defaults(func=cmd_result_show)
    sp = result_sub.add_parser("submit")
    sp.add_argument("--task")
    sp.add_argument("--stdin", action="store_true")
    sp.add_argument("--file")
    sp.add_argument("--summary")
    sp.add_argument("--status", choices=["complete", "waiting", "failed"], default="complete")
    sp.set_defaults(func=cmd_result_submit)

    memory = sub.add_parser("memory", help="Project/collection memory")
    memory_sub = memory.add_subparsers(dest="memory_command", required=True)
    sp = memory_sub.add_parser("add")
    sp.add_argument("--project")
    sp.add_argument("--collection")
    sp.add_argument("--task")
    sp.add_argument("--scope", choices=["project", "collection"], default="collection")
    sp.add_argument("--state", choices=["candidate", "accepted"], default="candidate")
    sp.add_argument("--kind", default="finding")
    sp.add_argument("--text", required=True)
    sp.set_defaults(func=cmd_memory_add)
    sp = memory_sub.add_parser("list")
    sp.add_argument("--project")
    sp.add_argument("--collection")
    sp.add_argument("--state", choices=["candidate", "accepted"], default="accepted")
    sp.set_defaults(func=cmd_memory_list)

    decision = sub.add_parser("decision", help="Durable decisions")
    decision_sub = decision.add_subparsers(dest="decision_command", required=True)
    sp = decision_sub.add_parser("add")
    sp.add_argument("--project")
    sp.add_argument("--collection")
    sp.add_argument("--task")
    sp.add_argument("--statement", required=True)
    sp.add_argument("--rationale", default="")
    sp.set_defaults(func=cmd_decision_add)

    return p


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return int(args.func(args) or 0)
    except (RuntimeError, KeyError, ValueError, PermissionError) as exc:
        print(f"praector: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
