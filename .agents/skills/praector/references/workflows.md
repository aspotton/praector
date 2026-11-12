# Praector MCP operating workflows

Use this reference before mutating durable work, diagnosing failures, revising candidates, or preparing delivery/acceptance/cleanup.

## Establish the live boundary

Confirm which Praector surface is actually available; the work and its rules are identical, only the mechanics differ (see [cli.md](cli.md)).

- **MCP:** the normal local endpoint is `http://127.0.0.1:8765/mcp`, hosted by a running `praector daemon`. If the `praector_*` tools are missing, report that connection/enablement is required — or fall back to the CLI if the binary is installed.
- **CLI:** `command -v praector && praector --version`, and `praector doctor` for dependency/registration checks. A missing daemon does not block CLI use: an attached command runs the scheduler itself, while `--detach` queues for a daemon.

Never claim a queued or completed action when no surface could be reached.

The default MCP surface includes reads and pass-through queue writes. Privileged tools are separately opt-in and may not be advertised. Their absence is expected when `[mcp] privileged = false`; the CLI has no such gate.

When the user asks how to enable the local server, the persistent configuration is:

```toml
[mcp]
enabled = true
addr = "127.0.0.1:8765"
privileged = false
```

It lives at `$PRAECTOR_HOME/config.toml` (normally `~/.praector/config.toml`). A one-shot alternative is `PRAECTOR_MCP=127.0.0.1:8765 praector daemon`. `PRAECTOR_MCP_PRIVILEGED=1` widens an already-enabled listener but never starts one by itself. Keep privileged mode off unless the requested workflow needs its real or plan-only boundary tools.

## Resolve context

1. Call `praector_project_list` when no exact Project is known.
2. Use `praector_project_show` for the selected `PRJ-*` when configuration, base branch, limits, or capabilities matter.
3. Keep returned `COL-*` and `T-*` IDs. Do not reconstruct or guess them.
4. Outside a managed Praector worker, explicitly pass Task IDs even where a tool schema marks them optional.

## Start a new objective

1. Confirm the user's objective and exact Project.
2. Call `praector_run` with `objective`, `project`, and an optional concise `title` — or `praector run --detach "OBJECTIVE" --project PRJ-*` (attached `praector run` without `--detach` blocks until `COMPLETE`/`BLOCKED`).
3. Record the returned Collection and coordinator IDs.
4. Call `praector_status` or `praector_graph` to verify durable creation.
5. Explain that work is queued; the daemon will execute planning and worker Tasks asynchronously.

Do not pre-decompose ordinary user objectives into MCP task requests. The coordinator and deterministic planning policy own initial graph formation. Use task-level operations only when working inside an established durable graph or when the user specifically requests graph intervention.

## Monitor work

Use the narrowest sequence that answers the question:

1. `praector_status` for the high-level Project/Collection/Task state.
2. `praector_graph` for dependencies and current graph shape.
3. `praector_task_show` for a particular node and its resolved edges.
4. `praector_result_show` for structured findings, verdicts, evidence, risks, and unresolved items.
5. `praector_logs` for Attempt log metadata when execution behavior or failure details matter.

Do not equate a completed coordinator with a completed Collection. Do not equate a submitted result with a `COMPLETE` Task while it remains `VERIFYING`.

## Diagnose a blocked or failed Collection

1. Read `praector_collection_show` for status, planning/delivery policy, revisions, critic freshness, Tasks, and acceptance history.
2. Read `praector_graph` to locate failed, waiting, or unsatisfied nodes.
3. Inspect the relevant Task, result, and logs.
4. Classify the cause before acting:
   - transient harness/provider failure after attempt exhaustion -> replacement retry may be appropriate;
   - real plan or graph defect -> coordinator attention/repair;
   - semantic review failure -> let Praector's repair lineage proceed; do not overwrite the failed review;
   - stale critic/research/policy gate -> allow required revalidation rather than bypassing it;
   - unknown dependency cycle -> report quarantine; do not guess at edge deletion;
   - missing current-research capability -> operator/project capability issue, not permission to use model memory.
5. Mutate only when the requested recovery matches the diagnosed cause.

## Retry exhausted work

`praector_task_retry` is for a terminal `FAILED` Task. It creates a new replacement Task, preserves the original Attempts, marks the old Task `SUPERSEDED`, and rewires hard dependents.

Before calling it:

- inspect the failure and logs;
- ensure the user asked to resume/recover or that retry is directly within the authorized workflow;
- provide a concrete `reason` when known;
- report both original and replacement IDs.

Do not use retry for a real implementation-review defect; semantic failed review has its own repair flow.

## Revise a completed candidate

Use `praector_collection_revise` only when all are true:

- the Collection is `COMPLETE`;
- it has not been accepted;
- human/operator acceptance found a concrete defect or mandatory correction;
- the reason is precise enough to serve as durable acceptance input.

Revision creates an `OPEN` revision and a new coordinator. The Collection returns to active work and cannot become `COMPLETE` until fresh post-revision implementation receives fresh passing independent review. Accepted Collections cannot be revised; start a new Collection.

## Delegate inside an existing graph

Use `praector_task_request` with the exact caller Task, one valid profile, bounded title/objective/reason, real dependency IDs, and normally `workspace: "auto"`.

- Non-blocking: the caller continues while the child may run concurrently.
- Blocking: the caller becomes `WAITING`, ends its current Attempt, and later resumes in a fresh Attempt after the child completes.
- Workers needing graph-level reconsideration should call `praector_coordinator_attention`, not create a coordinator directly.
- Only a coordinator should use add-dependency or supersede, with its caller identity when required.

Never keep a model session alive waiting for a blocking child.

## Submit worker results

This is normally used from managed worker context. Through external MCP, always pass the exact Task ID.

A normal result should include `status`, `summary`, and useful arrays such as `changes`, `findings`, `evidence`, `risks`, `unresolved`, `memory_candidates`, and `decisions`. Implementation results should include a short `pr_summary` and 3–8 reviewer-facing `changes` bullets.

A `review` or `plan_review` result must include `verdict: "pass"` or `"fail"`. Submitting a result moves the Task into `VERIFYING`; it is not immediately complete.

## Memory and decisions

Use memory for durable facts/constraints/conventions that should appear in future Task Envelopes. Candidate memory is not automatically accepted truth. Use decisions for durable statements with rationale.

Before adding either:

- provide explicit Project/Collection/Task context;
- distinguish evidence-backed durable context from temporary observations;
- avoid promoting a worker suggestion to accepted project truth without authorization.

## Delivery and acceptance

1. Verify the Collection is `COMPLETE`, critic freshness is current, and each implementation lineage has passing independent review.
2. Inspect `praector_collection_diff` and relevant results/reviews.
3. If privileged MCP tools are available:
   - `praector_collection_pr` returns deterministic PR copy and a plan only;
   - `praector_collection_accept` validates and returns an acceptance plan only.
4. State explicitly that no push, remote PR creation, merge, or acceptance record occurred.
5. Hand off the actual operator CLI action — or run it directly when the user has authorized the mutation and you are on the CLI surface, since these same commands perform the real effects there:

```bash
praector collection pr COL-... --dry-run
praector collection pr COL-...

praector collection accept COL-... --dry-run
praector collection accept COL-...
```

Default acceptance is fast-forward-only. Do not recommend `--no-ff` until the divergence has been inspected and the user explicitly chooses a merge commit.

## Cleanup

MCP cleanup is a real filesystem/Git operation, not a plan.

1. Inspect Collection and acceptance history.
2. Confirm exact requested scope:
   - default: remove managed Task/integration worktrees;
   - `delete_branches: true`: also delete only safe branches reachable from the accepted target;
   - `force: true`: permits worktree cleanup before acceptance and requires especially explicit intent.
3. Call `praector_collection_cleanup` only with that authorization.
4. Report returned worktree/branch counts and verify Collection state remains durable.

Never infer branch deletion or forced cleanup from a general request to “finish,” “accept,” or “tidy up.”
