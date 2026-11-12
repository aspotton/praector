# Praector CLI operating reference

Use this reference when driving Praector with the local `praector` binary instead of the MCP tools. The CLI and the MCP server are two adapters over the same typed `Service`/`Store` paths, so every durable concept, gate, state, and authorization rule in [concepts.md](concepts.md) applies unchanged. Only the mechanics below differ.

## Establish the boundary

1. Check the binary: `command -v praector && praector --version`.
2. Check dependencies when something behaves oddly: `praector doctor`.
3. `praector <command> --help` is authoritative for exact parser behavior on the installed version.
4. If neither the CLI nor MCP tools are available, say so; never simulate a durable action with raw `git`/`dolt` commands.

## Execution model difference that matters most

The MCP surface is enqueue-only; the CLI can also **run** the graph itself.

- `praector run OBJECTIVE` (attached) starts a scheduler in that process and blocks until the Collection is `COMPLETE` or `BLOCKED`. For an agent this can be a very long block.
- `praector run --detach OBJECTIVE` creates durable work without executing it; the running `praector daemon` (or a later attached command) does the work. This is the CLI equivalent of an MCP `praector_run`.
- `praector task retry` and `praector collection revise` resume the Collection immediately by default; pass `--detach` to queue them for an existing daemon instead.
- Only one scheduler owner may execute for a given `PRAECTOR_HOME`. If a daemon is already running, prefer `--detach` rather than starting a competing scheduler. `praector daemon --once` reconciles available work and exits.

## Context resolution difference

The CLI resolves project context in this order: explicit `--project`/ID, then `PRAECTOR_PROJECT_ID`, then the nearest ancestor `.praector/project.toml` marker. Inside the target checkout no ID is needed; outside it, pass `--project PRJ-*` or a `COL-*`/`T-*` target explicitly rather than trusting the current directory.

Inside a managed worker, `PRAECTOR_TASK_ID` and friends make IDs implicit, so `praector task request`, `praector context show`, `praector result show`, and `praector result submit --stdin` work with no IDs. Outside a worker, always pass explicit IDs.

## Command surface equivalence

| CLI | MCP equivalent |
| --- | --- |
| `praector init` | `praector_init` (privileged, real write) |
| `praector run --detach` | `praector_run` (MCP is always the enqueue path; attached `run` has no MCP equivalent) |
| `praector status [TARGET]` | `praector_status` |
| `praector graph [COL]` | `praector_graph` |
| `praector doctor` | `praector_doctor` |
| `praector logs TARGET [--tail N]` | `praector_logs` (metadata only) |
| `praector daemon [--once]` | launch-only; resource `praector://help/daemon` |
| `praector project list` / `project show` | `praector_project_list` / `praector_project_show` |
| `praector profile list` / `profile show P` | `praector_profile_list` / `praector_profile_show` |
| `praector collection show/revise/diff/pr/accept/cleanup` | `praector_collection_*` |
| `praector task show/list/retry/request/add-dependency/supersede` | `praector_task_*` |
| `praector context show [--task T]` | `praector_context_show` |
| `praector coordinator attention` | `praector_coordinator_attention` |
| `praector result show` / `result submit` | `praector_result_show` / `praector_result_submit` |
| `praector memory add` / `memory list` | `praector_memory_add` / `praector_memory_list` |
| `praector decision add` | `praector_decision_add` |

Coverage is intentionally 1:1 (`commandHelp` → `serviceOps` → MCP surface), so anything reachable through MCP is reachable through the CLI.

## Where the CLI is stronger, not just differently named

- `praector collection pr COL-*` really pushes and creates/reuses the GitHub PR (`gh` required); MCP `praector_collection_pr` only returns a plan. Use `--dry-run` first, and `--refresh-copy` to update an existing PR body.
- `praector collection accept COL-*` really fast-forwards the target and records acceptance; MCP `praector_collection_accept` only returns a plan. `--no-ff` and `--delete-branches` are explicit opt-ins.
- `praector collection cleanup COL-*` really removes worktrees; the MCP tool needs the privileged opt-in while the CLI has no such gate — the authorization check is you.
- `praector logs T-* --tail N` tails real Attempt output, including a running Attempt; MCP exposes only log metadata.
- `praector init` registers the current repository from cwd; the MCP tool requires an absolute `root_path`.

Because these side effects are real, never run `collection pr`, `collection accept`, `collection cleanup`, or `init` on inference. Confirm the user's request authorizes that specific effect, prefer the documented preview first (`--dry-run`, `--stat`, `collection show`), and state what actually changed.

## Verifying a CLI action

Read the durable state back rather than trusting an exit code:

```bash
praector status COL-xxxxxxxxxx
praector graph COL-xxxxxxxxxx
praector task show T-xxxxxxxxxx
praector result show T-xxxxxxxxxx
praector collection show COL-xxxxxxxxxx   # JSON: policy, revisions, acceptance, critic_revalidation
```

`collection show`, `task show`, and `project show` print JSON suitable for parsing; other commands print human text. A successful `run`/`retry`/`revise` return value means durable work exists, not that it completed.
