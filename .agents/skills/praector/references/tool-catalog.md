# Praector MCP tool and resource catalog

This catalog reflects the commandHelp-driven MCP surface. The live tool schema is authoritative for the installed daemon. Pass JSON booleans/arrays as native values, and use exact durable IDs.

Privileged tools are advertised only when `[mcp] privileged = true` or `PRAECTOR_MCP_PRIVILEGED=1` on an already-enabled listener.

## Top-level tools

- `praector_init({root_path, name?, base_branch?, opencode?})` — **privileged, real write**. Registers the Git repository at an absolute `root_path`, writes its project marker, and never uses daemon cwd.
- `praector_run({objective, project, title?})` — queues a new Collection and coordinator against explicit `PRJ-*`; daemon executes it asynchronously.
- `praector_status({target?, project?})` — status for explicit Project context or a `COL-*`/`T-*` target. Provide at least one context value.
- `praector_graph({collection?, project?})` — current durable graph for explicit Collection or Project context.
- `praector_doctor({project})` — dependency and registration checks for explicit Project.
- `praector_logs({target})` — Attempt log metadata for a `T-*` or `COL-*`; use the operator CLI for streamed `--tail` output when MCP metadata is insufficient.

## Collection tools

- `praector_collection_show({collection})` — full Collection JSON: Tasks, policy, critic freshness, revisions, PR/acceptance history.
- `praector_collection_revise({collection, reason})` — queues mandatory repair for a `COMPLETE`, unaccepted Collection; creates a revision and coordinator.
- `praector_collection_diff({collection, target?, stat?})` — Git diff against the Project base branch or explicit target.
- `praector_collection_pr({collection, base?, title?, remote?, draft?})` — **privileged, plan-only**. Validates and returns deterministic PR plan/copy; does not push or create/update a remote PR.
- `praector_collection_accept({collection, target?})` — **privileged, plan-only**. Validates and returns fast-forward/already-contained/diverged acceptance plan; does not merge or record acceptance.
- `praector_collection_cleanup({collection, delete_branches?, force?})` — **privileged, real destructive write**. Removes managed worktrees; optionally deletes safe accepted branches. `force` permits pre-acceptance worktree cleanup.

## Project and profile tools

- `praector_project_list({})` — lists registered Projects; preferred first call when Project ID is unknown.
- `praector_project_show({project})` — Project configuration for explicit `PRJ-*`.
- `praector_profile_list({})` — built-in profiles and capability modes.
- `praector_profile_show({profile})` — inherited global contract plus one role contract. Valid profiles: `coordinator`, `explore`, `critic`, `plan_review`, `research`, `implement`, `review`, `advise`.

## Task and graph tools

- `praector_task_show({task})` — Task JSON with dependencies/dependents and result.
- `praector_task_list({collection?, project?})` — Tasks for explicit Collection or Project context.
- `praector_task_retry({task, reason?})` — queues a replacement for terminal `FAILED` Task; preserves/supersedes old history and rewires dependents.
- `praector_task_request({caller, profile, title, objective, reason, blocking?, depends_on?, workspace?})` — creates or deduplicates durable delegated work. `depends_on` is an array of existing same-Collection Task IDs. `workspace` is normally `auto`, otherwise `inherit`, `collection`, or `isolated` when semantics require it.
- `praector_task_add_dependency({task, on, caller?})` — coordinator-only hard edge mutation. `task` becomes dependent on prerequisite `on`; cycle and scope guards apply.
- `praector_task_supersede({old, with, reason?, caller?})` — coordinator-only replacement/rewire. Preserve a concrete reason and coordinator caller.
- `praector_context_show({task?})` — compiled durable context for a Task. External MCP callers should always pass `task`.
- `praector_coordinator_attention({reason, task?, blocking?})` — requests graph-level coordinator work. Pass the calling Task when outside managed worker environment.

## Result tools

- `praector_result_show({task?})` — structured durable Task result. External MCP callers should pass `task`.
- `praector_result_submit({result, task?, status?, summary?})` — submits a structured result and moves the Task to verification. External MCP callers should pass `task`. Review and plan-review results require `result.verdict` of `pass` or `fail`.

Example implementation result payload:

```json
{
  "task": "T-...",
  "result": {
    "status": "complete",
    "summary": "Implemented and verified the bounded change",
    "pr_summary": "Adds the requested behavior with regression coverage.",
    "changes": ["Added the behavior", "Added regression tests"],
    "evidence": ["go test ./..."],
    "risks": [],
    "unresolved": []
  }
}
```

## Memory and decision tools

- `praector_memory_add({text, project?, collection?, task?, scope?, state?, kind?})` — adds durable memory. Supply explicit context. Defaults are normally collection scope, candidate state, finding kind.
- `praector_memory_list({project, collection?, state?})` — lists memory for explicit Project and optional Collection. The live schema currently requires `project`; pass it even when a Collection is known. Default state is accepted.
- `praector_decision_add({statement, rationale?, project?, collection?, task?})` — adds a durable decision. Supply explicit context rather than relying on daemon cwd.

## MCP resources

Resources expose namespaces/launch-only help and JSON listings:

| URI | Contents | MIME type |
| --- | --- | --- |
| `praector://projects` | Registered Project listing | `application/json` |
| `praector://profiles` | Built-in profile listing | `application/json` |
| `praector://help/daemon` | Daemon help; daemon itself is launch-only | `text/plain` |
| `praector://help/collection` | Collection command help | `text/plain` |
| `praector://help/task` | Task command help | `text/plain` |
| `praector://help/context` | Context command help | `text/plain` |
| `praector://help/coordinator` | Coordinator command help | `text/plain` |
| `praector://help/result` | Result command help | `text/plain` |
| `praector://help/memory` | Memory command help | `text/plain` |
| `praector://help/decision` | Decision command help | `text/plain` |

## Semantic cautions

- A successful queue-write confirms durable creation, not completion.
- A tool-domain failure is returned as an MCP tool error; do not treat client transport success as domain success without reading the result.
- `collection pr` and `collection accept` are deliberately weaker than their CLI counterparts: MCP returns plans, while the CLI can push/merge/record.
- `collection cleanup` and `init` are deliberately real writes, so authorization must be checked immediately before calling them.
- Tool coverage follows the CLI command registry, but output/side-effect semantics can differ at privileged external boundaries.
