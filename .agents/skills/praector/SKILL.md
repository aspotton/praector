---
name: praector
description: "Operate and explain Praector's durable agent-work control plane through the local praector binary or its MCP server. Use for live Praector work: registering projects; creating, inspecting, monitoring, revising, retrying, delivering, accepting, or cleaning up Collections and Tasks; inspecting graphs, logs, results, memory, decisions, profiles, or recovery state; and explaining how Praector behaves. Do not trigger for ordinary edits to Praector source unless the request also operates the live control plane."
---

# Praector

Drive Praector as a durable control plane through either of its two adapters: the local `praector` executable or the loopback MCP server. Both call the same typed service and store paths, so the mental model, gates, states, and authorization rules below are identical. Praector owns task-graph state, retries, scheduling, isolated Git worktrees, integration, and review; neither adapter launches worker agents inline as part of your session.

## Choose the surface

1. If `praector_*` MCP tools are advertised, use them (see [tool-catalog.md](references/tool-catalog.md)).
2. Otherwise check the binary: `command -v praector && praector --version`, then use the CLI (see [references/cli.md](references/cli.md)).
3. The CLI is the only route to real PR push, real acceptance, real cleanup, and streamed `--tail` logs; MCP is enqueue/plan-oriented with privileged writes behind a default-off opt-in.
4. If neither exists, state that plainly. Do not fake a durable action with raw `git`/`dolt` commands.

## Load only the detail needed

- Read [references/concepts.md](references/concepts.md) when explaining Praector, interpreting lifecycle state, reasoning about profiles/policy/review, or diagnosing recovery and workspace behavior.
- Read [references/workflows.md](references/workflows.md) before creating or changing durable work, recovering failures, revising a completed Collection, or preparing delivery/acceptance/cleanup.
- Read [references/tool-catalog.md](references/tool-catalog.md) when selecting a tool, checking exact inputs, or using a less-common operation or resource.
- Read [references/cli.md](references/cli.md) when using the binary, mapping a command to its MCP equivalent, or running a real (non-plan-only) delivery/acceptance/cleanup action.

## Essential operating rules

1. Treat `Project -> Collection -> Task -> Attempt -> Session` as distinct durable concepts. A Task survives Attempts, disposable model sessions, daemon restarts, and retries.
2. Use whichever adapter is actually present: the `praector_*` MCP tools or the equivalent `praector <command>` subcommands. Tool names may receive a client-specific namespace prefix; match the advertised name ending in `praector_*`. The command/usage table in [references/cli.md](references/cli.md) already lists every command's exact flags, so do not default to `praector <command> --help`; it stays authoritative on the installed version and is a fallback for a differing binary version or a flag genuinely absent from the table.
3. Never infer live context from the daemon's working directory. Via MCP, always pass explicit `PRJ-*`, `COL-*`, and `T-*` identifiers; via the CLI, context resolves from an explicit ID/flag, then `PRAECTOR_PROJECT_ID`, then the nearest `.praector/project.toml` — so run it inside the target checkout or pass the ID explicitly, never from an unrelated cwd. Start with `praector project list` / `praector_project_list` when the Project is unknown.
4. Inspect before mutating. For Collection work, normally read `praector_collection_show` / `praector collection show` plus `praector_graph` / `praector graph`; inspect relevant Tasks/results/logs when the requested action depends on their state.
5. Distinguish **queued**, **running**, **complete**, **delivered**, and **accepted**. An MCP queue-write returning successfully means durable work was created; it does not mean a worker finished. `COMPLETE` means reviewed candidate work, not merged base-branch state.
6. Do not spawn or manage agent processes. Use `praector_run` / `praector run --detach`, `praector_task_request` / `praector task request`, `praector_coordinator_attention` / `praector coordinator attention`, or `praector_task_retry` / `praector task retry`; the daemon owns execution and recovery. An attached `praector run` starts a scheduler in your own process and blocks until the Collection is `COMPLETE` or `BLOCKED` — prefer `--detach` when a daemon already owns the Praector home or the work should not block the session.
7. Never invent durable IDs or dependency placeholders. Use IDs returned by Praector and confirm referenced Tasks belong to the same Collection.
8. Preserve authorization boundaries. Read-only inspection is safe by default. Create/revise/retry/delegate only when the user's request calls for it. The current request must explicitly authorize real project registration or cleanup; do not ask again when it already does. Do not infer permission to push, merge, accept, delete branches, or expand scope.
9. Delivery and acceptance differ by surface. MCP `praector_collection_pr` and `praector_collection_accept` are validation/plan operations only — they never push, create a remote PR, merge, or record acceptance, so tell the user the operator-side CLI action remains. The CLI `praector collection pr`/`accept` **do** perform those real effects, which is exactly why they need explicit authorization and a preview (`--dry-run`, `collection diff --stat`) first.
10. MCP `praector_init` and `praector_collection_cleanup` perform real writes only when privileged tools are enabled; `init` requires an absolute `root_path`. The CLI `praector init` and `praector collection cleanup` have no privileged gate — they really register repositories and remove worktrees/branches — so on that surface the authorization check is you.
11. Do not bypass planning gates, review gates, dependency checks, or coordinator authority. A `READY` implementation can still be claim-blocked by deterministic policy.
12. If neither the MCP server nor the `praector` binary (nor a required privileged tool) is available, state that plainly. Do not claim a live action occurred or silently replace it with a shell mutation.

## Default workflow

Each step names the MCP tool; the identically named CLI subcommand does the same thing (`praector_project_list` ↔ `praector project list`).

1. Resolve scope with `praector_project_list` and, as needed, `praector_project_show`.
2. Establish current state with the narrowest relevant read tools.
3. Explain the intended durable effect before a meaningful mutation; obtain any authorization not already present in the user's request.
4. Call one bounded tool operation and retain every returned durable ID.
5. Re-read state to verify the durable effect rather than assuming it from a client exit code.
6. For asynchronous work, monitor with status/graph and drill into Tasks, results, or logs only where needed.
7. Report the exact state reached, durable IDs, remaining blockers, and whether an operator-side delivery or acceptance action remains.

## Common routing

- New objective: project list/show -> `praector_run` -> status/graph.
- Progress check: status -> graph -> task show/result show/logs for the relevant node.
- Blocked work: collection show + graph -> failed/waiting Task details -> logs/result -> diagnose before retrying or escalating.
- Human defect in a `COMPLETE`, unaccepted Collection: inspect candidate -> `praector_collection_revise` with a concrete mandatory correction -> monitor fresh implementation and review.
- Delivery or acceptance: inspect show/diff and review evidence -> via MCP, request the PR/acceptance plan if privileged then hand the real action to the operator; via the CLI, preview with `--dry-run` and run it only on explicit authorization.
- Post-acceptance cleanup: verify the acceptance record -> ask for explicit cleanup scope -> call cleanup -> verify worktree/branch counts.

## Communication contract

Lead with the current durable outcome. Name IDs and status values exactly. Say whether an action merely queued work, produced a plan, changed durable state, removed filesystem state, or still requires operator action. Never collapse Task, Attempt, Session, Collection completion, PR delivery, and acceptance into one generic “done.”

## Version-specific source of truth

The running surface is authoritative for the installed version: live MCP schemas and returned durable state when using the server, and `praector <command> --help` plus returned durable state when using the binary. For deeper repository-specific behavior, consult [Operations](../../../docs/OPERATIONS.md), [CLI](../../../docs/CLI.md), [Architecture](../../../docs/ARCHITECTURE.md), [Agent protocol](../../../docs/AGENT_PROTOCOL.md), [Workspaces](../../../docs/WORKSPACES.md), and [State and recovery](../../../docs/STATE_AND_RECOVERY.md) rather than relying on model memory.
