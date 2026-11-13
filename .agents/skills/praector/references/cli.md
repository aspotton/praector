# Praector CLI operating reference

Use this reference when driving Praector with the local `praector` binary instead of the MCP tools. The CLI and the MCP server are two adapters over the same typed `Service`/`Store` paths, so every durable concept, gate, state, and authorization rule in [concepts.md](concepts.md) applies unchanged. Only the mechanics below differ.

This page answers CLI usage questions **in place**: the command/usage table below is derived from the `commandHelp` registry (`internal/praector/help.go`) and is kept in sync by a drift-guard test (`TestSkillCLICommandHelpCoversCommandHelp`), so you normally do **not** need `praector <command> --help` to discover flags. For deeper prose on any command, follow the linked [repository `docs/CLI.md`](../../../../docs/CLI.md) section (that path resolves from the **repository root**, not this skill directory — `docs/CLI.md` lives outside the skill package).

## Establish the boundary

1. Check the binary: `command -v praector && praector --version`.
2. Check dependencies when something behaves oddly: `praector doctor`.
3. Prefer the usage table below. `praector <command> --help` remains **authoritative for exact parser behavior on the installed version**, so treat it as a **fallback** — reach for it only when the installed binary's version differs from this checkout, or when a flag is genuinely absent from the table. It is not the default discovery step, and a stale table must still defer to the binary.
4. If neither the CLI nor MCP tools are available, say so; never simulate a durable action with raw `git`/`dolt` commands.

## Command and usage table

Every `praector` command and its exact usage line (copied verbatim from `commandHelp`). `<NAME>` is a required positional argument; `[--flag X]` is optional; a `[TARGET]`-style bracketed positional is optional and defaults to context (see Managed worker IDs below).

| Command | Exact usage line | Positional args | Gotcha / required flags | Prose |
| --- | --- | --- | --- | --- |
| `init` | `Usage: praector init [--name NAME] [--base-branch BRANCH] [--opencode PATH]` | none (registers cwd) | Real write: registers the current repo; needs explicit authorization. | [Initialize](../../../../docs/CLI.md#initialize) |
| `run` | `Usage: praector run OBJECTIVE [--title TITLE] [--project ID] [--detach] [--max-workers N]` | `OBJECTIVE` | Attached `run` **blocks**; `--detach` only enqueues for a daemon. | [Run an objective](../../../../docs/CLI.md#run-an-objective) |
| `status` | `Usage: praector status [TARGET] [--project ID]` | `[TARGET]` optional | No target → project-level status; pass a `COL-*`/`T-*` for that scope. | [Status](../../../../docs/CLI.md#status) |
| `graph` | `Usage: praector graph [COLLECTION] [--project ID]` | `[COLLECTION]` optional | Defaults to the latest collection. | [Graph](../../../../docs/CLI.md#graph) |
| `daemon` | `Usage: praector daemon [--once] [--max-workers N]` | none | Only one scheduler owner per `PRAECTOR_HOME`; `--once` reconciles and exits. | [Scheduler / daemon](../../../../docs/CLI.md#scheduler--daemon) |
| `doctor` | `Usage: praector doctor` | none | Validates dependencies and registration; no args. | [Doctor](../../../../docs/CLI.md#doctor) |
| `logs` | `Usage: praector logs TARGET [--tail N]` | `TARGET` (required) | `--tail N` streams a running Attempt's real output. | [Logs](../../../../docs/CLI.md#logs) |
| `events` | `Usage: praector events [TARGET] [--project ID] [--collection COL-ID] [--type EVENT_TYPE] [--since CURSOR] [--limit N]` | `[TARGET]` optional | Direct store read (no daemon); `--since CURSOR` is an exclusive opaque cursor from the emitted `next_cursor`. | [Events](../../../../docs/CLI.md#events) |
| `collection show` | `Usage: praector collection show COL-ID` | `COL-ID` | JSON output (policy, revisions, acceptance). | [Collection lifecycle](../../../../docs/CLI.md#collection-lifecycle) |
| `collection revise` | `Usage: praector collection revise COL-ID --reason TEXT [--detach] [--max-workers N]` | `COL-ID` | `--reason` is **required**; reopens a COMPLETE, unaccepted collection. | [Revise](../../../../docs/CLI.md#revise) |
| `collection diff` | `Usage: praector collection diff COL-ID [--target BRANCH] [--stat]` | `COL-ID` | `--stat` is the safe preview before accept/pr. | [Diff](../../../../docs/CLI.md#diff) |
| `collection pr` | `Usage: praector collection pr COL-ID [--draft] [--title TITLE] [--base BRANCH] [--remote REMOTE] [--dry-run] [--refresh-copy] [--open]` | `COL-ID` | **Real push/PR** — needs explicit authorization; preview with `--dry-run`. | [Pull request](../../../../docs/CLI.md#pull-request) |
| `collection accept` | `Usage: praector collection accept COL-ID [--target BRANCH] [--no-ff] [--dry-run] [--cleanup] [--delete-branches]` | `COL-ID` | **Real fast-forward accept** — explicit authorization; `--no-ff`/`--delete-branches` are opt-ins. | [Accept](../../../../docs/CLI.md#accept) |
| `collection cleanup` | `Usage: praector collection cleanup COL-ID [--delete-branches] [--force]` | `COL-ID` | **Really removes worktrees**; never deletes unaccepted branches. | [Cleanup](../../../../docs/CLI.md#cleanup) |
| `profile list` | `Usage: praector profile list` | none | Lists profiles and capability mode. | [Profile](../../../../docs/CLI.md#profile) |
| `profile show` | `Usage: praector profile show PROFILE` | `PROFILE` | Prints inherited global + role contracts. | [Profile](../../../../docs/CLI.md#profile) |
| `project list` | `Usage: praector project list` | none | Start here when the Project is unknown. | [Project](../../../../docs/CLI.md#project) |
| `project show` | `Usage: praector project show [--project ID]` | none (flag-only) | Project selected via `--project`, not positional. | [Project — Show](../../../../docs/CLI.md#project) |
| `task show` | `Usage: praector task show TASK` | `TASK` | Shows one task. | [Task — Show](../../../../docs/CLI.md#task) |
| `task list` | `Usage: praector task list [--collection COL-ID] [--project ID]` | none | Defaults to the latest collection. | [Task — List](../../../../docs/CLI.md#task) |
| `task retry` | `Usage: praector task retry TASK [--reason TEXT] [--detach] [--max-workers N]` | `TASK` | Resumes immediately by default; `--detach` queues. | [Retry a failed task](../../../../docs/CLI.md#retry-a-failed-task) |
| `task request` | `Usage: praector task request --profile PROFILE --title TITLE --objective TEXT --reason TEXT [--task TASK] [--blocking] [--depends-on TASK]... [--workspace MODE]` | none (all flags) | `--profile`, `--title`, `--objective`, `--reason` all **required**; `--depends-on` may repeat. | [Request](../../../../docs/CLI.md#request) |
| `task add-dependency` | `Usage: praector task add-dependency TASK --on DEPENDENCY [--caller TASK]` | `TASK` | Coordinator-only; `--on` names the prerequisite. | [Add dependency](../../../../docs/CLI.md#add-dependency) |
| `task supersede` | `Usage: praector task supersede OLD --with NEW --reason TEXT [--caller TASK]` | `OLD` | Coordinator-only; `--with` and `--reason` required. | [Supersede](../../../../docs/CLI.md#supersede) |
| `context show` | `Usage: praector context show [--task TASK]` | none | Inside a worker no `--task` is needed. | [Context](../../../../docs/CLI.md#context) |
| `coordinator attention` | `Usage: praector coordinator attention --reason TEXT [--task TASK] [--blocking]` | none (flag-only) | `--reason` is **required**. | [Coordinator escalation](../../../../docs/CLI.md#coordinator-escalation) |
| `result show` | `Usage: praector result show [TASK]` | `[TASK]` optional | Defaults to the current task. | [Result — Show](../../../../docs/CLI.md#result) |
| `result submit` | `Usage: praector result submit [--task TASK] [--stdin] [--file PATH] [--summary TEXT] [--status STATUS]` | none | Structured results use `--stdin` (mandatory for `review`/`plan_review`). | [Submit](../../../../docs/CLI.md#submit) |
| `memory add` | `Usage: praector memory add --text TEXT [--project ID] [--collection COL-ID] [--task TASK] [--scope SCOPE] [--state STATE] [--kind KIND]` | none (flag-only) | `--text` is **required**. | [Memory — Add](../../../../docs/CLI.md#memory) |
| `memory list` | `Usage: praector memory list [--project ID] [--collection COL-ID] [--state STATE]` | none | Filter by project/collection/state. | [Memory](../../../../docs/CLI.md#memory) |
| `decision add` | `Usage: praector decision add --statement TEXT [--rationale TEXT] [--project ID] [--collection COL-ID] [--task TASK]` | none (flag-only) | `--statement` is **required**. | [Decision](../../../../docs/CLI.md#decision) |

### Namespace / group usage lines

A namespace `--help` prints its subcommand names but **zero flags**, so do not spend a call on it — the flags are on the per-leaf commands above. The group forms are:

```text
Usage: praector collection {show|revise|diff|pr|accept|cleanup} ...
Usage: praector profile {list|show} ...
Usage: praector project {list|show} ...
Usage: praector task {show|list|retry|request|add-dependency|supersede} ...
Usage: praector context {show} ...
Usage: praector coordinator {attention} ...
Usage: praector result {show|submit} ...
Usage: praector memory {add|list} ...
Usage: praector decision {add} ...
```

## Mandatory flags for a useful call

These flags are required and are the usual cause of a wrong invocation: `task request` (`--profile` `--title` `--objective` `--reason`), `coordinator attention` (`--reason`), `memory add` (`--text`), `decision add` (`--statement`), `collection revise` (`--reason`). Arguments such as `run OBJECTIVE`, `logs TARGET`, and the `COL-ID`/`TASK` targets are **positional**, whereas `project show` is **flag-only** (`--project ID`) with no positional argument.

## Managed worker IDs and run semantics

- `praector run OBJECTIVE` (attached) starts a scheduler in that process and **blocks** until the Collection is `COMPLETE` or `BLOCKED` — potentially a very long block for an agent.
- `praector run --detach OBJECTIVE` **only enqueues** durable work; the running `praector daemon` (or a later attached command) does the work. This is the CLI equivalent of an MCP `praector_run`.
- `praector task retry` and `praector collection revise` resume the Collection immediately by default; pass `--detach` to queue them for an existing daemon instead.
- Only one scheduler owner may execute for a given `PRAECTOR_HOME`. If a daemon is already running, prefer `--detach` rather than starting a competing scheduler. `praector daemon --once` reconciles available work and exits.
- Inside a managed worker, `PRAECTOR_TASK_ID` (and friends) make IDs **implicit**, so `praector task request`, `praector context show`, `praector result show`, and `praector result submit --stdin` work with no IDs. Outside a worker, always pass explicit IDs.

The CLI resolves project context in this order: explicit `--project`/ID, then `PRAECTOR_PROJECT_ID`, then the nearest ancestor `.praector/project.toml` marker. Inside the target checkout no ID is needed; outside it, pass `--project PRJ-*` or a `COL-*`/`T-*` target explicitly rather than trusting the current directory.

## Command surface equivalence

| CLI | MCP equivalent |
| --- | --- |
| `praector init` | `praector_init` (privileged, real write) |
| `praector run --detach` | `praector_run` (MCP is always the enqueue path; attached `run` has no MCP equivalent) |
| `praector status [TARGET]` | `praector_status` |
| `praector graph [COL]` | `praector_graph` |
| `praector doctor` | `praector_doctor` |
| `praector logs TARGET [--tail N]` | `praector_logs` (metadata only) |
| `praector events [TARGET] [--since CURSOR]` | `praector_events` (read-only store read; requires an explicit scope on the MCP surface) |
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

## Poll vs. push for state changes

The CLI has no push surface of its own, so polling is the default when no daemon is running or when the caller is a short-lived worker session: `praector events [TARGET] [--since CURSOR]` reads the durable `events` table directly from the store (see the table above for its full flag list) and does not require a daemon.

When a daemon **is** running with MCP enabled, a client can also open a scoped SSE stream against the same loopback endpoint the MCP tools use, rather than polling `status`/`graph`. One-line form (ad-hoc against a scratch or test-owned listener; never enable MCP on or restart the operator's live daemon to gather evidence):

```bash
curl -NsG http://127.0.0.1:8765/mcp \
  -H 'Accept: text/event-stream' \
  -H 'MCP-Protocol-Version: 2025-06-18' \
  --data-urlencode 'collection=COL-...'
```

Pass a previous `id:` (the durable `EVT` id) back as a `Last-Event-ID` header to resume. Live delivery and replay share the same `project`/`collection`/`type`/`level` scoping, and the stream is **observation-only**: it launches no scheduler and bypasses no gate. These query parameters are delivery filters only, not authorization; loopback plus the Host/Origin authority gate plus the `MCP-Protocol-Version` header are the security boundary. Prefer `praector events` whenever the caller cannot hold a connection open across a process boundary, no daemon is running, or a one-shot audit is enough. See [Operations § Server-initiated event stream](../../../../docs/OPERATIONS.md#server-initiated-event-stream-sse) for the full contract.

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
