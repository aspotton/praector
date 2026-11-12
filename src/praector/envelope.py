from __future__ import annotations

import json
from typing import Any

from .config import READ_ONLY_PROFILES
from .db import DoltStore
from .profile_prompts import load_global_prompt, load_profile_prompt
from .planning_policy import classify_delivery_policy, classify_planning_policy


PROFILE_GUIDANCE = {
    "coordinator": {
        "purpose": "Produce/refine a decision-complete durable work graph and own graph-level reasoning.",
        "own": "Consume exploration/critic evidence, resolve planning assumptions, decompose work, and define dependencies/verification.",
        "delegate": "Repository exploration, substantive research, implementation, independent plan review, implementation review, or specialist consultation.",
    },
    "explore": {
        "purpose": "Map the existing repository/system surface relevant to the Collection objective before planning.",
        "own": "Inspect relevant files, modules, tests, configuration, runtime boundaries, callers/dependencies, existing patterns, and documentation; return evidence-grounded current-state context.",
        "delegate": "Solution design, architecture preference, implementation, graph mutation, or independent review.",
    },
    "critic": {
        "purpose": "Challenge the request before planning by surfacing hidden intent, ambiguity, scope hazards, and verification gaps.",
        "own": "Intent classification, evidence-grounded gap analysis, planner directives, risk/assumption analysis, and QA directives.",
        "delegate": "Implementation or graph mutation. Request bounded research only when decisive evidence is genuinely missing.",
    },
    "plan_review": {
        "purpose": "Independently determine whether the proposed durable work graph is executable without blocking gaps.",
        "own": "Reference checks, task executability, dependency coherence, scope coherence, and verification-plan review.",
        "delegate": "Plan repair or implementation. Return a verdict; Praector/coordinator handles graph repair.",
    },
    "research": {
        "purpose": "Investigate a bounded question and return verifiable evidence.",
        "own": "Repository investigation, evidence gathering, source comparison, and analysis within the assigned question.",
        "delegate": "Independent specialist work, implementation, architecture changes, or review outside this task's scope.",
    },
    "implement": {
        "purpose": "Make the bounded code/file change assigned by this task and prove it works.",
        "own": "Inspect relevant code, implement the requested change, run tests/checks, and debug your own implementation.",
        "delegate": "External research, architecture decisions, independent review, or materially separate implementation scope.",
    },
    "review": {
        "purpose": "Independently verify completed implementation against its objective and acceptance intent.",
        "own": "Inspect integrated code/diffs, run appropriate checks/tests/smoke tests, identify material defects, and determine pass/fail.",
        "delegate": "Repairs. If defects require changes, return fail; Praector creates/resumes bounded implementation repair work.",
    },
    "advise": {
        "purpose": "Provide deep technical judgment and one concrete recommendation without owning implementation.",
        "own": "Reason about the bounded question, tradeoffs, risks, assumptions, confidence, and recommendation.",
        "delegate": "Implementation or review. Request bounded research only when a decisive factual gap remains.",
    },
}

AVAILABLE_PROFILES = {
    "explore": "Use for broad repository/system discovery before planning: what exists, how it connects, and what constraints matter.",
    "critic": "Use for pre-planning gap analysis: hidden intent, ambiguity, scope hazards, assumptions, and QA directives.",
    "plan_review": "Use to review whether a proposed graph/task plan is executable before write work begins.",
    "research": "Use for additional facts, repository investigation, authoritative evidence, or bounded discovery.",
    "implement": "Use when an actual code/file change must be produced.",
    "review": "Use for independent post-implementation verification and pass/fail judgment.",
    "advise": "Use for difficult conceptual/architectural judgment and a concrete recommendation.",
}


READ_ONLY_CAPABILITY_PROMPT = """
This task is read-only. Editor permissions are denied, but shell commands, tests, imports, build tools, and smoke checks can still create or modify repository files. Treat **no net workspace mutation** as part of the task's completion contract.

- At the start of the attempt, capture the worktree state (normally `git status --porcelain=v1 --untracked-files=all`) so you know the baseline.
- Prefer verification modes that avoid repository writes when they do not change the behavior being tested. Examples include `PYTHONDONTWRITEBYTECODE=1` for ordinary Python verification where bytecode caches are irrelevant, or directing tool caches/temporary output outside the worktree when the tool supports it.
- After tests/smokes, inspect the worktree again. Restore only incidental tracked changes caused by your own verification and remove only new untracked artifacts that your commands created (for example bytecode caches, coverage output, test caches, or temporary build products).
- Never discard pre-existing changes, committed candidate content, or another task's work merely to make `git status` clean. If cleanup cannot be done safely and precisely, stop and let Praector's capability check reject/retry the attempt rather than mutating the candidate.
- Before submitting the result, confirm the final worktree state matches the starting baseline. A semantic `verdict` is about the plan/implementation; a dirty read-only worktree is a separate capability failure and can invalidate the attempt.
""".strip()


_RESULT_KEYS = (
    "verdict",
    "recommendation",
    "confidence",
    "intent_classification",
    "planner_directives",
    "qa_directives",
    "assumptions",
    "findings",
    "evidence",
    "artifacts",
    "risks",
    "unresolved",
)


def _result_summary(row: dict[str, Any]) -> dict[str, Any]:
    result = row.get("result") or {}
    summary: dict[str, Any] = {
        "id": row["id"],
        "profile": row["profile"],
        "title": row["title"],
        "status": row["status"],
        "objective": row["objective"],
        "summary": result.get("summary", ""),
    }
    for key in _RESULT_KEYS:
        value = result.get(key)
        if value not in (None, "", [], {}):
            summary[key] = value
    return summary


def _graph_snapshot(store: DoltStore, collection_id: str) -> dict[str, Any]:
    tasks = store.list_tasks(collection_id)
    edges = store.list_edges(collection_id)
    return {
        "tasks": [
            {
                "id": row["id"],
                "profile": row["profile"],
                "status": row["status"],
                "title": row["title"],
                "objective": row["objective"],
                "reason": row.get("reason") or "",
                "depth": int(row.get("depth") or 0),
            }
            for row in tasks
        ],
        "edges": [
            {
                "from": row["from_task_id"],
                "to": row["to_task_id"],
                "relation": row["relation"],
            }
            for row in edges
        ],
    }


def build_task_envelope(
    store: DoltStore,
    project: dict[str, Any],
    collection: dict[str, Any],
    task: dict[str, Any],
    attempt: dict[str, Any],
) -> str:
    deps = store.dependencies(task["id"])
    for dep in deps:
        dep["result"] = json.loads(dep.get("result_json") or "{}") if dep.get("result_json") else {}
    parent = store.get_parent(task)
    if parent:
        parent["result"] = json.loads(parent.get("result_json") or "{}") if parent.get("result_json") else {}
    memories = store.list_memories(project["id"], collection["id"])
    decisions = store.list_decisions(project["id"], collection["id"])
    guide = PROFILE_GUIDANCE[task["profile"]]
    global_prompt = load_global_prompt()
    role_prompt = load_profile_prompt(task["profile"])

    planning_policy = classify_planning_policy(collection.get("objective") or "")
    delivery_policy = classify_delivery_policy(collection.get("objective") or "")

    context: dict[str, Any] = {
        "project": {"id": project["id"], "name": project["name"], "base_branch": project["base_branch"]},
        "workspace": {
            "assigned_worktree": task.get("worktree_path"),
            "mode": task.get("workspace_mode"),
            "rule": "All repository/file/shell operations for this attempt must stay inside assigned_worktree.",
        },
        "collection": {
            "id": collection["id"],
            "title": collection["title"],
            "objective": collection["objective"],
            "status": collection["status"],
        },
        "task": {
            "id": task["id"],
            "profile": task["profile"],
            "title": task["title"],
            "objective": task["objective"],
            "reason": task.get("reason") or "",
            "workspace_mode": task.get("workspace_mode"),
        },
        "attempt": {"id": attempt["id"], "number": attempt["attempt_no"]},
        "parent": _result_summary(parent) if parent else None,
        "completed_dependencies": [_result_summary(dep) for dep in deps if dep["status"] == "COMPLETE"],
        "project_and_collection_memory": [
            {"kind": m["kind"], "text": m["text"], "scope": m["scope"]} for m in memories
        ],
        "active_decisions": [
            {"id": d["id"], "statement": d["statement"], "rationale": d.get("rationale") or ""} for d in decisions
        ],
        "planning_policy": planning_policy.as_dict(),
        "delivery_policy": delivery_policy.as_dict(),
        "control_plane": {
            "command": "praector",
            "executable_env": "PRAECTOR_CLI",
            "rule": "Use `praector` for durable control-plane commands. Worker PATH contains a dedicated Praector shim even when the parent Python virtualenv is sanitized.",
        },
    }
    if task["profile"] in {"coordinator", "critic", "plan_review"}:
        context["collection_graph"] = _graph_snapshot(store, collection["id"])

    available = dict(AVAILABLE_PROFILES)
    if task["profile"] == "coordinator":
        available["coordinator"] = "Do not create another coordinator merely to delegate reasoning; you already own graph-level authority."

    capability_prompt = READ_ONLY_CAPABILITY_PROMPT if task["profile"] in READ_ONLY_PROFILES else "This task has write capability within its assigned implementation scope."

    review_note = ""
    if task["profile"] == "review":
        review_note = (
            "For an implementation review, include `verdict: pass` or `verdict: fail`. "
            "A fail is kept out of acceptance and drives bounded repair work."
        )
    elif task["profile"] == "plan_review":
        review_note = (
            "For a plan review, include `verdict: pass` or `verdict: fail`. "
            "A fail remains blocking; Praector will wake a coordinator to repair the graph before this reviewer runs again."
        )

    return f"""# PRAECTOR TASK ENVELOPE v1

You are running as one disposable execution attempt inside a durable orchestrator. The task, graph, workspace, and results persist outside this OpenCode session.

## Non-negotiable orchestration rules

1. DO NOT use OpenCode's built-in `task` / subagent/background-agent mechanism. It is disabled.
2. Agents do not spawn agents. Agents request durable work from the orchestrator.
3. Stay inside this task's responsibility boundary. Do not absorb meaningful out-of-scope work merely because you can do it.
4. If another independent check, specialist investigation, implementation, or review is needed, use `praector task request`.
5. A blocking task request changes this task to WAITING. After making a blocking request, stop substantive work and end this attempt; do NOT submit a complete result afterward. The dependency edge is authoritative. Normally Praector launches a fresh attempt after the dependency completes; coordinator-requested final `plan_review` is the exception—on a pass Praector finalizes that waiting coordinator automatically, while a failure creates explicit coordinator-repair work.
6. Use `praector context show` if you need current durable context. Do not infer missing organizational state from old chat history.
7. Before finishing, submit a structured result with `praector result submit --stdin`. The `praector` command is a dedicated control-plane shim; do not replace it with project Python tooling. For `review` and `plan_review`, this is mandatory: final chat text is NOT a valid fallback verdict, and omitting the structured result is a protocol failure that will be retried. For other profiles, Praector may preserve final response text as a fallback summary.
8. WORKSPACE ISOLATION IS MANDATORY. `PRAECTOR_WORKTREE` / `workspace.assigned_worktree` is your execution root. Run all repository reads, writes, shell commands, tests, and Git commands there. Never switch tool workdir to the user's original project checkout.
9. If `delivery_policy.implementation_required` is true, a planning-only successful Collection is invalid. Coordinators must create a durable implementation path or surface a real blocker; other profiles must not reinterpret a direct change request as analysis-only work.

## Global engineering contract

{global_prompt}

## Role

Profile: **{task['profile']}**
Purpose: {guide['purpose']}

OWN - do this yourself:
{guide['own']}

ASK - if durable project/task context is missing:
Use `praector context show`, `praector task list`, `praector task show <TASK_ID>`, or `praector result show <TASK_ID>`.

DELEGATE - do not cross this boundary yourself:
{guide['delegate']}

## Capability discipline

{capability_prompt}

## Role-specific operating contract

{role_prompt}

## Available durable delegation profiles

{json.dumps(available, indent=2)}

Example non-blocking request:
`praector task request --profile research --title "Check X" --objective "Determine X with evidence" --reason "Needed because ..."`

Example blocking request:
`praector task request --profile advise --title "Architecture decision" --objective "Evaluate A vs B and recommend one" --reason "Implementation is blocked on this" --blocking`

Coordinator planning examples:
`praector task request --profile critic --title "Preflight the plan" --objective "Identify hidden requirements, scope hazards, and verification gaps" --reason "Complex work needs a planning preflight" --blocking`
`praector task request --profile plan_review --title "Review proposed work graph" --objective "Determine whether the current graph is executable and verifiable" --reason "Gate non-trivial implementation before execution"`
`praector task request --profile implement --title "Implement X" --objective "DELIVERABLE: ... SCOPE: ... MUST HAVE: ... MUST NOT: ... VERIFY: ..." --reason "Approved plan"`

When using `--depends-on`, use only the exact ID of an existing task in this Collection, as returned by Praector or shown by `praector task list`; never invent or pass symbolic `T-*` placeholders. Nonexistent and cross-Collection dependencies are rejected.

Coordinator graph repair commands (substitute exact task IDs):
`praector task add-dependency "$TASK_ID" --on "$PREREQ_TASK_ID"`
`praector task supersede "$FAILED_TASK_ID" --with "$REPLACEMENT_TASK_ID" --reason "Why the replacement supersedes the old work"`

If the work graph itself needs reconsideration and you are not the coordinator:
`praector coordinator attention --reason "Explain why the plan needs reconsideration" --blocking`

## Structured result

Submit JSON on stdin, for example:

```bash
cat <<'JSON' | praector result submit --stdin
{{
  "status": "complete",
  "summary": "Concise description of what was accomplished",
  "pr_title": "Optional concise pull-request title for implementation work",
  "pr_summary": "Optional concise pull-request summary for implementation work",
  "changes": [],
  "findings": [],
  "evidence": [],
  "artifacts": [],
  "risks": [],
  "unresolved": [],
  "memory_candidates": [
    {{"kind": "finding|constraint|decision|convention", "text": "Durable fact worth retaining"}}
  ],
  "decisions": [
    {{"statement": "Decision made", "rationale": "Why", "evidence": [], "alternatives": []}}
  ]
}}
JSON
```

{review_note}

## STOP WHEN

The stated objective and acceptance boundary are satisfied, or you have created a blocking durable task for work you must not/should not perform yourself.

## Durable context

```json
{json.dumps(context, indent=2, ensure_ascii=False)}
```
"""
