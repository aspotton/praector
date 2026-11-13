# Operating Praector

[Documentation index](README.md) · [Roadmap](../ROADMAP.md) · [CLI](CLI.md) · [Workspaces](WORKSPACES.md) · [State and recovery](STATE_AND_RECOVERY.md)

This is the practical operator guide for installing, initializing, running, observing, accepting, and troubleshooting Praector.

## Requirements

Runtime:

- Linux or macOS (runtime-tested); Windows binaries are cross-built, with full Windows runtime validation/support tracked in the near-term roadmap
- Git
- Dolt 2.x
- OpenCode
- GitHub CLI (`gh`) when using pull-request delivery

The native executable has no Python runtime dependency. File locking and process-tree cleanup use OS-specific adapters; one scheduler/daemon still owns execution for a given `PRAECTOR_HOME`.

## Install

Build the host executable from source with Go:

```bash
make build
```

On Linux/macOS, install to the user path with:

```bash
make install
```

Or copy `dist/praector` / `dist/praector.exe` to any directory on `PATH`. To produce the full release matrix manually:

```bash
make build-all
```

Validate:

```bash
praector --version
praector doctor
```

Normal Praector operation uses the native executable.

## Praector home

Default:

```text
~/.praector
```

Contains:

```text
state/           Dolt repository
logs/            attempt logs
worktrees/       Collection/task worktrees
state.lock       Dolt mutation lock
scheduler.lock   exclusive scheduler ownership lock
```

Override:

```bash
export PRAECTOR_HOME=/srv/praector-dev
```

Use a separate `PRAECTOR_HOME` when you want isolated test state without touching another Praector environment.

## Initialize a Project

Run inside an existing Git repository:

```bash
cd ~/src/my-project
praector init --opencode /path/to/opencode
```

Optional:

```bash
praector init \
  --name my-project \
  --base-branch main \
  --opencode /path/to/opencode
```

This creates:

```text
.praector/project.toml
```

and registers the Project in Dolt.

## Project configuration

Example marker:

```toml
version = 1

[project]
id = "PRJ-..."
name = "my-project"

[workspace]
type = "git"
repository = ".."
base_branch = "main"

[harness]
default = "opencode"
command = "/path/to/opencode"

[models]
coordinator = ""
critic = ""
plan_review = ""
research = ""
implement = ""
review = ""
advise = ""

[limits]
max_task_depth = 8
max_tasks_per_collection = 100
max_children_per_task = 10
max_parallel_tasks = 4
max_attempts_per_task = 3
task_timeout_seconds = 3600
```

Empty model values mean "use OpenCode's normal/default model".

Example profile-specific models:

```toml
[models]
coordinator = "local/qwen-reasoning"
critic = "local/qwen-reasoning"
plan_review = "local/qwen-reasoning"
research = "local/qwen-fast"
implement = "local/qwen-code"
review = "local/qwen-reasoning"
advise = "local/qwen-reasoning"
```

Project commands sync marker configuration into durable Project state.

## Preflight

Before creating a Collection, `praector run` verifies:

- Git executable;
- Dolt executable;
- Praector CLI executable (workers call it for durable delegation/results);
- configured OpenCode executable;
- repository existence and Git validity.

Run manually:

```bash
praector doctor
```

## Run a Collection

Attached:

```bash
praector run "Add a small JSON endpoint, tests, and documentation"
```

The command remains attached until the Collection becomes `COMPLETE` or `BLOCKED`.

Detached:

```bash
praector run --detach "Add enterprise SSO"
praector daemon
```

Override worker count for the invocation:

```bash
praector run --max-workers 2 "..."
praector daemon --max-workers 2
```

## MCP server (optional, local)

When enabled, a running `praector daemon` also hosts a **loopback-only**
(127.0.0.1) streamable-HTTP MCP listener as a background goroutine, so AI agents
speaking MCP can drive the same durable control plane the CLI uses. It is
**disabled by default** and always binds loopback only; authentication/identity
is deferred to the remote-access boundary and is not part of this local release.

Enable it in `~/.praector/config.toml`:

```toml
[mcp]
enabled = true
addr = "127.0.0.1:8765"        # optional; loopback only
privileged = false             # optional; expose privileged write tools (init/registration, pr/accept/cleanup) when the listener is already enabled
```

`enabled` is the only setting that starts the listener; `privileged` never
enables MCP by itself — it only widens the exposed tool surface on an
already-enabled listener.

or for a single invocation via the `PRAECTOR_MCP` environment override (a truthy
token toggles enablement; any other value is treated as an explicit loopback
`host:port`):

```bash
PRAECTOR_MCP=1 praector daemon
PRAECTOR_MCP=127.0.0.1:9000 praector daemon
PRAECTOR_MCP_PRIVILEGED=1 praector daemon   # on an already-enabled listener, also expose the privileged write tools (this alone does NOT start the listener)
```

The listener is decoupled from the reconcile loop and the scheduler lock. It
performs only durable Store reads and enqueue-only actions (for example
`praector_run` and `praector_task_request` create durable work that the running
daemon then executes); it never launches worker processes or runs a collection
inline, and never bypasses dependency, planning, review, capability, or
acceptance checks. A non-loopback address is refused at startup.

The tool/resource surface is **commandHelp-driven**: every CLI command maps to
exactly one MCP surface item, giving command/surface coverage parity (the CLI and
MCP surfaces cannot drift) rather than a claim of unrestricted behavioral parity. Leaf
commands appear as callable tools, and each command namespace plus the
launch-only `daemon` command appear as readable resources. Read-only commands
and pass-through queue writes (`task request`, `result submit`, `memory add`,
`decision add`, `task add-dependency`/`supersede`, `coordinator attention`,
`run`, `task retry`, `collection revise`) are always exposed, and each reuses
the identical gated `Store`/workspace method as the CLI, so policy, planning,
audit, cycle, and revision checks are never bypassed.

Privileged write tools — explicit-root project `init`/registration plus the
collection-finalizing `collection pr`, `collection accept`, and `collection
cleanup` — are **default-off** behind the independent
`privileged` opt-in. When disabled they are neither advertised nor callable, and
even when enabled the `pr`/`accept` tools only run the durable acceptance/PR
validation and return the resulting plan: they never push, merge, or otherwise
implicitly accept, while `cleanup` performs real worktree/branch removal and
`init` performs real project registration/filesystem changes. An MCP `init` call
requires an explicit **absolute** target `root_path` (relative paths, `.`, `..`,
and an empty value are refused) and never operates on the daemon process working
directory. Direct acceptance and external PR delivery remain
operator-side actions.

### Protocol and local-security contract

The listener implements a single MCP protocol revision and is honest about it: it
advertises only the revision it actually implements, and meets an `initialize`
request naming any other well-formed non-empty revision with a **counter-offer** —
a successful result advertising the implemented revision — rather than echoing the
client's request back as if it were supported (a JSON-RPC Invalid-params error is
reserved for a missing/empty/non-string/malformed version, the missing/empty edge
being a documented fail-closed hardening). On
every request after `initialize`, the client must send an `MCP-Protocol-Version`
HTTP header equal to that revision; a missing or unsupported header is rejected
with HTTP 400 (Praector does not implement — and therefore does not assume — any
older revision). `initialize` itself may omit the header.

Because the listener is loopback-only and unauthenticated, it also defends
against DNS-rebinding and related local-listener attacks: before any tool or
resource handler runs, the request's Host/local-authority and (when present) its
`Origin` are validated as a loopback authority (localhost, a `127.0.0.0/8`, or
`[::1]` address, with a matching port); a foreign authority or Origin — or a
literal `null` / otherwise-opaque Origin that names no host to validate — is
rejected with HTTP 403. A request with no `Origin` (a non-browser client) is
accepted on the Host check alone. This is defense in depth on top of the
authoritative socket-bind gate that already refuses to bind a non-loopback
address.

MCP operations carry **no implicit daemon cwd**. Any tool whose domain operation
would otherwise infer a project from the working directory instead requires an
explicit `PRJ-*` project identifier or a resolvable `COL-*`/`T-*` durable id
(`praector_run` requires `project`; `status`, `graph`, `doctor`, `task list`,
`project show`, `memory add`/`list`, and `decision add` follow the same rule).
When only a `COL-*` id is supplied for a project-scoped operation such as `memory
list`/`add` or `decision add`, its owning project is resolved from the
collection's stored reference through a deterministic, fail-closed resolver — an
unknown or project-less collection is refused rather than silently inheriting the
daemon working directory. The
CLI keeps its cwd-based project discovery unchanged.

Resources advertise an accurate content type: JSON-backed resources (the profile
and project listings) advertise `application/json`, while CLI/help-text resources
advertise `text/plain`. The authoritative independent conformance check is an
official MCP client — the Inspector CLI (driven by
`TestMCPOfficialClientInteroperability` when `PRAECTOR_MCP_INSPECTOR` is set) or a
Tier-1 SDK. In addition, Praector's own in-process Go tests exercise the same contract — negotiation, the
header/Origin contracts, `tools/list`, `resources/list`/`read`, a safe read call,
and a safe durable queue-write — independently of the official-client oracle,
 though this coverage is not itself authoritative conformance.

### Server-initiated event stream (SSE)

In addition to the POST request/response surface, the listener serves an
**observation-only** server-sent-events stream so an MCP client can subscribe to
durable events instead of polling `status`/`graph`. This push surface targets the
**MCP 2025-06-18** revision (the current 2026-07-28 revision differs; that migration
is deferred roadmap work, not current conformance — see `ROADMAP.md`). It targets
2025-06-18 in **version scope, not conformance**: Praector does not claim to
implement every 2025-06-18 multi-stream rule, only the subset it wired here.

To open a stream, send `GET /mcp` with `Accept: text/event-stream` **and** the
`MCP-Protocol-Version: 2025-06-18` header. Four optional query parameters scope
what the connection receives; each also applies to a `Last-Event-ID` reconnect,
so a stream replays only its own scoped rows:

| Parameter | Applies to | Meaning |
| --- | --- | --- |
| `project=PRJ-*` | live + replay | Only events whose durable `project_id` matches. |
| `collection=COL-*` | live + replay | Only events whose durable `collection_id` matches. |
| `type=EVENT_TYPE` | live + replay | Only events with this exact `event_type`. No registry check: an unrecognised string simply matches nothing and returns an empty page (a known gap, recorded in `ROADMAP.md`). |
| `level=LEVEL` | live + replay | Only events whose `eventLevel(...)` rank is at or above `LEVEL`. Validated against `mcp.ValidLoggingLevel`; an **invalid value silently keeps the default `info`** rather than returning an error. |

Anchors: query-parameter read is `internal/praector/mcp_sse.go:216-221`; the
same `filter{project, collection, eventType}` is stored on the stream struct at
`internal/praector/mcp_sse.go:58-60`, applied to live fan-out by `matches()`
(`internal/praector/mcp_sse.go:323-335`), and passed into `Store.ListEvents` for
replay at `internal/praector/mcp_sse.go:292`; the level filter on replay lives
at `internal/praector/mcp_sse.go:313`.

Worked example — subscribe to a single collection (ad-hoc observation against a
loopback listener; **never** enable MCP on the operator's live daemon just to
gather evidence — use a scratch `PRAECTOR_HOME`/test-owned listener instead):

```bash
curl -NsG http://127.0.0.1:8765/mcp \
  -H 'Accept: text/event-stream' \
  -H 'MCP-Protocol-Version: 2025-06-18' \
  --data-urlencode 'collection=COL-...'
```

Add `-H 'Last-Event-ID: EVT-...'` to resume after a disconnect. Append
`--data-urlencode 'level=debug'` to also receive the muted `debug` event types
listed below, or `--data-urlencode 'type=TASK_FAILED'` to narrow to one type.

Behaviour and guarantees:

- A `GET` that does **not** advertise `text/event-stream` still receives **405**
  (the POST surface is byte-for-byte unchanged for non-streaming clients;
  `internal/praector/mcp/transport.go:64-67`). "Advertise" means content
  negotiation, not an exact string: the check
  (`internal/praector/mcp/transport.go:86-97`) accepts `text/event-stream` and
  honors the `*/*` and `text/*` wildcards. A plain `curl` or browser `GET` that
  names no `Accept` header at all is therefore sending `Accept: */*` and **does
  open the stream**; the 405 case is a `GET` whose `Accept` explicitly excludes
  SSE (an empty header, or only non-SSE media types). This widens nothing that
  the gates below refuse, because the `MCP-Protocol-Version` header and the
  loopback Host/Origin authority check run first on every `GET` path.
- A `GET` is authorized **identically to POST**: the loopback-authority gate
  (`internal/praector/mcp/transport.go:42-45`) and the `MCP-Protocol-Version`
  header requirement (`internal/praector/mcp/transport.go:72-74`) are enforced
  before a single byte is written (a `GET` has no JSON-RPC body, so its version
  header is required unconditionally, unlike POST's `initialize` header
  exemption). The **complete GET refusal set** is:
  - **405** Method Not Allowed — a `GET` whose `Accept` header excludes SSE (an
    empty header, or one naming only non-SSE media types; `*/*` and `text/*`
    count as advertising and open the stream), or any `GET` while no
    `StreamHandler` is wired;
  - **403** Forbidden — a foreign or literal-null/opaque `Origin` or a
    non-loopback `Host`. This is Praector's fail-closed policy layered on top of
    the socket-bind gate (DNS-rebinding / cross-origin defense). The Origin
    requirement itself matches the **2025-11-25** revision strengthening, or
    Praector's own policy, not a 2025-06-18 mandate;
  - **400** Bad Request — a missing or wrong `MCP-Protocol-Version` header, per
    the 2025-06-18 Streamable-HTTP contract;
  - **503** Service Unavailable — `http.StatusServiceUnavailable` with body
    `too many event streams`, returned by a single shared code path when either
    the concurrent-stream cap is reached **or** the hub is closed during daemon
    shutdown (`internal/praector/mcp_sse.go:224-227` and `tryRegister`
    `internal/praector/mcp_sse.go:266-279`). One shared code path, one status code:
    both triggers produce the same `503`, not two distinguishable refusals.
- A `200` response opens with headers `Content-Type: text/event-stream`,
  `Cache-Control: no-cache`, `Connection: keep-alive`, `X-Accel-Buffering: no`,
  followed by a `: connected` SSE priming comment (a comment, never a JSON-RPC
  frame) so the client observes the `200` immediately before any data frame
  (`internal/praector/mcp_sse.go:230-238`).
- **Operational limits** are compiled constants —
  `defaultMaxStreams = 64`, `defaultStreamBuffer = 128`,
  `defaultReplayLimit = 2000`, `defaultSSELevel = "info"`, `sseLoggerName =
  "praector"` (`internal/praector/mcp_sse.go:64-70`) — overridable only in
  tests, not via config or environment.
- **Security boundary.** Loopback binding plus the Host/Origin authority gate
  and the mandatory `MCP-Protocol-Version` header together are the security
  boundary. The `project` / `collection` / `type` / `level` query parameters
  and the `logging/setLevel` level filter are **delivery filters only**, not
  authorization or access control: any local process that reaches the loopback
  listener can subscribe to any collection's events. Do not describe them as
  authorization, and do not describe loopback binding as stronger than it is.
- Each SSE frame carries **only** a JSON-RPC `notifications/message` — a
  JSON-RPC **response** is never written on the stream. The SSE `id:` line is
  the durable `EVT` id (so a reconnect can pass it back as `Last-Event-ID`),
  `event:` is `message`, and the `data:` body is the `notifications/message`
  JSON with `logger: "praector"` and `data` keys **exactly**
  `{event_type, event_id, project_id, collection, task, attempt, created_at}`
  (`internal/praector/mcp_sse.go:355-378`). There is no `collection_id`,
  `task_id`, or `attempt_id` key name on the wire — the internal Go struct uses
  those names, but the emitted JSON uses the shorter forms above.
- The stream is **read-only and observation-only**: it never implies a mutation
  happened, never launches a scheduler or process, and never bypasses
  policy/review/acceptance (`collection pr`/`accept` remain plan-only here too).
- Fan-out is **non-blocking**: a slow, stalled, or vanished client at most loses
  its own messages and has its own stream closed; it never blocks `Store.Event()`
  or starves another consumer.
- Reconnect with **`Last-Event-ID`** (the HTTP GET header, whose value is the
  SSE `id:` line's durable `EVT` id) **replays** that stream's missed rows
  strictly after the resolved `(created_at, id)` cursor from the durable
  `events` table, applying the same connect-time `project`/`collection`/`type`
  scope and the effective `level`, and **deduped by id**. The replay is
  **fail-closed**: an absent or unresolvable last id replays nothing rather
  than guessing a cursor (`internal/praector/mcp_sse.go:284-291`).
- **Level semantics.** The valid level domain is the eight RFC 5424 levels
  (`debug`, `info`, `notice`, `warning`, `error`, `critical`, `alert`,
  `emergency`; `internal/praector/mcp/protocol.go:72`). The default effective
  level on a fresh stream is `info`. There are **two different mechanisms with
  different scope**:
  - `?level=` is **per-connection** and is validated with
    `mcp.ValidLoggingLevel` (`internal/praector/mcp_sse.go:219-221`). An invalid
    value is silently ignored; the stream keeps the `info` default rather than
    failing.
  - `logging/setLevel` is a JSON-RPC method whose server-side handler calls
    `sseHub.SetLevel`, which loops over every active stream and stores the new
    rank on each (`internal/praector/mcp_sse.go:147-162`). It is therefore an
    **all-streams broadcast** — exact for a single observer, last-writer-wins
    across many concurrent subscribers. An invalid level parameter is rejected
    with JSON-RPC Invalid Params `-32602` at the protocol layer, which is a
    spec-mandated behavior of the targeted revision. Per-connection `setLevel`
    would require session identity that `mcp.LoggingProvider` does not
    currently carry; the 2025-06-18 spec leaves the method's scope
    unspecified, so this broadcast is a Praector implementation decision rather
    than a spec mandate. Deferred as roadmap work.
- **Event-type → level mapping (rule, not just examples).** `eventLevel` is a
  fixed `switch` (`internal/praector/mcp_sse.go:384-402`) whose `default:`
  branch returns `debug`. Consequence: **any emitted event type not explicitly
  listed in that map is `debug`**, therefore **muted at the default `info`
  level**; a subscriber that needs to see it must request `?level=debug` (or
  issue `logging/setLevel` at `debug`). The explicitly-listed set covers, at
  `error`: `IMPLEMENTATION_WORKSPACE_VIOLATION`, `READ_ONLY_CAPABILITY_RETRY`;
  at `warning`: `CRITIC_REVALIDATION_REQUIRED`, `COLLECTION_BLOCKED`,
  `TASK_FAILED`, `PLAN_REVIEW_CYCLE_REPAIRED`, `PLAN_REVIEW_REPAIR_CREATED`,
  `LEGACY_DEPENDENCY_CYCLE_REPAIRED`; at `info`: `COLLECTION_COMPLETE`,
  `COLLECTION_ACCEPTED`, `COLLECTION_PR_RECORDED`, `COLLECTION_PR_UPDATED`,
  `COLLECTION_REVISION_SATISFIED`, `COLLECTION_ENTRYPOINT_RECOVERED`,
  `TASK_COMPLETE`, `TASK_INTEGRATED`, `TASK_SUPERSEDED`,
  `TASK_MANUAL_RETRY_CREATED`. Everything else the store actually emits is
  `debug`/muted by default.
  - Derived emitted-but-muted list (current tree; re-derive with the command
    below). Note: two names in the level map (`PLAN_REVIEW_CYCLE_REPAIRED`,
    `PLAN_REVIEW_REPAIR_CREATED`) have no emission site in this tree, and three
    emitted names (`PLAN_REVIEW_COORDINATOR_CYCLE_REPAIRED`,
    `PLAN_REVIEW_DEPENDENCY_CYCLE_REPAIRED`, `PLAN_REVIEW_REPAIR_REQUESTED`)
    are **not** in that map and therefore fall through to `debug`.
    `ATTEMPT_FINISHED`, `COLLECTION_CREATED`, `COLLECTION_REVISION_REQUESTED`,
    `CONFIG_BASIS_UNAVAILABLE`, `COORDINATOR_ATTENTION_REQUESTED`,
    `COORDINATOR_FINALIZED_AFTER_PLAN_REVIEW`, `CRITIC_PLAN_BASELINE`,
    `DELIVERY_POLICY_ENFORCED`, `DEPENDENCY_CYCLE_QUARANTINED`,
    `DEPENDENCY_CYCLE_REJECTED`, `DIRECTIVE_TRACEABILITY_EMPTY_HARVEST`,
     `DIRECTIVE_TRACEABILITY_GAP`, `EXPLORE_REQUIRED`, `EXECUTOR_ERROR`,
    `PLANNING_POLICY_ENFORCED`, `PLAN_REVIEW_CALLER_DEPENDENCY_IGNORED`,
    `PLAN_REVIEW_COORDINATOR_CYCLE_REPAIRED`,
    `PLAN_REVIEW_DEPENDENCY_CYCLE_REPAIRED`,
    `PLAN_REVIEW_FLOOR_HELD`,
    `PLAN_REVIEW_IMPLEMENT_DEPENDENCIES_IGNORED`,
    `PLAN_REVIEW_REPAIR_REQUESTED`, `PLAN_REVIEW_REUSED`,
    `POST_REPAIR_REVIEW_CREATED`, `RECONCILIATION_REQUIRED`,
    `REDUNDANT_PLAN_REVIEW_COLLAPSED`, `RESEARCH_CAPABILITY_REQUIRED`,
    `RESEARCH_CONSISTENCY_REVALIDATION_REQUIRED`, `RESEARCH_REQUIRED`,
    `REVIEW_LINEAGE_REPAIRED`, `REVIEW_PROTOCOL_RETRY`, `REVIEW_REPAIR_CREATED`,
    `SESSION_STARTED`, `TASK_COMPLETION_DEFERRED`, `TASK_CREATED`,
    `TASK_INPUT_LINEAGE`, `TASK_REQUESTED`, `TASK_RESULT_SUBMITTED`. Whether
    promoting any of these to `info` is warranted is recorded in `ROADMAP.md`;
    none is promoted here. Note that `PLANNING_POLICY_ENFORCED` carries a
    `gate_derivation` payload field describing which planning gates were set
    (`internal/praector/scheduler.go:993`).
  - Reproducible enumeration command (union of `.Event(...)` /
    `emitEvent(... EventType: "...")` call sites plus inline `INSERT INTO
    events` string literals and the `statusTaskEvent`/`statusCollectionEvent`
    functions):

    ```bash
    {
      git grep -h -E 'Store\.Event|\.Event\(|emitEvent' -- 'internal/praector/*.go' \
        | grep -oE '"[A-Z][A-Z_]{3,}"'
      git grep -h "INSERT INTO events" -- 'internal/praector/*.go' \
        | grep -oE "'[A-Z_]{4,}'" | tr -d "'" | sed 's/^/"/;s/$/"/'
      git grep -A 12 'func statusTaskEvent\|func statusCollectionEvent' \
        -- 'internal/praector/*.go' | grep -oE '"[A-Z_]{4,}"'
    } | sort -u
    ```

    Subtract the levels listed above to derive the currently-muted set. Static
    grep can miss dynamically-composed type strings, so a reviewer should
    re-derive rather than trust this list across releases; treat it as derived
    from the current tree, not settled fact.

The `praector events [TARGET]` CLI command remains the no-daemon way to read the
same durable log by hand (`docs/CLI.md`) and is the right choice when no daemon
is running or when the caller is a short-lived worker session that cannot hold
an SSE connection open across a process boundary.

## Observe work

Project/Collection status:

```bash
praector status
praector status COL-...
```

Graph:

```bash
praector graph COL-...
```

Tasks:

```bash
praector task list --collection COL-...
praector task show T-...
```

Results:

```bash
praector result show T-...
```

Logs:

```bash
praector logs T-... --tail 120
praector logs COL-... --tail 50
```

Because log paths are recorded before OpenCode launches, live Attempts can be inspected while they are running.

Managed OpenCode prompts are streamed over stdin rather than placed in the child process argument vector. This keeps large exploration/research/critic envelopes intact without hitting operating-system argv limits; Praector closes stdin after writing so headless OpenCode observes EOF deterministically. Attempt logs retain prompt byte count and SHA-256 and record `prompt_transport=stdin`.

## Inspect the durable event log

The `events` command reads the durable `events` table **directly from the store**, so it works with no daemon running:

```bash
praector events COL-... --type COLLECTION_CREATED
praector events COL-... --since CURSOR --limit 50
praector events T-... --type TASK_FAILED
```

Rows are returned oldest-first by `created_at` then `id`. Each page prints a `next_cursor`; pass it back unchanged as `--since` for the next page (an exclusive composite cursor, so paging never repeats or skips a row). A project-scoped read (`praector events --project PRJ-...`) is always bounded by `--limit`. See [CLI](CLI.md#events) and [State and Recovery](STATE_AND_RECOVERY.md#events).

A scope id you supply must name something that exists: `praector events` exits `1` with the same `collection/task/project not found` message the other read commands print, so a mistyped id in a `--since` paging loop is an error rather than a page that looks empty because the target was wrong. A `--project`/`--collection` pair where the collection is not owned by that project is rejected the same way. An existing scope with no matching events is still an empty, successful page with no `next_cursor`, which is the only output that legitimately means "nothing happened yet".

## Understand `BLOCKED`

A Collection becomes `BLOCKED` when incomplete work remains but no Task is currently executing or runnable and no durable harness Session for the Collection is still running. A parent Task may briefly be `WAITING` while its OpenCode Session finishes; that state remains `RUNNING`, not `BLOCKED`.

Inspect:

```bash
praector status COL-...
praector graph COL-...
praector logs COL-... --tail 150
```

Common causes:

- failed Task exhausted retries;
- unresolved dependency;
- integration conflict;
- coordinator/worker harness error;
- graph repair needed.

The durable graph remains available for inspection/repair.

## Run one reconciliation cycle

Useful after installing a fix or recovering local state:

```bash
praector daemon --once
```

This performs startup recovery/reconciliation and runs available work until no runnable work remains.

## Inspect a completed Collection

```bash
praector collection show COL-...
praector collection diff COL-... --stat
praector collection diff COL-...
```

Also inspect the Git branch if useful:

```bash
git log --oneline --decorate praector/collections/<collection-id> -20
```

Your original checkout should remain clean unless you intentionally modify it.

## Revise a completed Collection after human acceptance feedback

A `COMPLETE` Collection is still unaccepted work until it is merged into the base branch. If operator/human review finds a concrete defect, reopen the same durable Collection:

```bash
praector collection revise COL-... \
  --reason "Concrete defect and required correction"
```

This creates an `OPEN` durable revision and a fresh coordinator task. The revision is satisfied only after post-revision implementation work is independently reviewed with `verdict=pass`; merely documenting or downgrading a defect cannot close it. Use `--detach` to queue the revision for the daemon.

Do not use `revise` after acceptance. Accepted history is immutable; create a new Collection for follow-up work.

## Open a pull request for a completed Collection

For the normal GitHub workflow:

```bash
praector collection pr COL-... --dry-run
praector collection pr COL-...
# when an existing open PR should receive regenerated Praector copy:
praector collection pr COL-... --refresh-copy
```

Praector validates the reviewed Collection, selects the Collection integration branch as head, pushes it to the configured/selected remote, and creates or reuses the GitHub PR through `gh`. Use `--draft`, `--title`, `--base`, `--remote`, or `--open` when needed.

PR copy is deterministic and review-oriented: completed implement workers should submit `pr_title`, `pr_summary`, plus 3–8 concise `changes` bullets. `collection pr` uses those verbatim where available. These fields should describe reviewer-facing behavior and scope, not source line numbers, helper/regex inventories, or file-by-file implementation prose. Older Collections without explicit PR metadata are still supported; Praector derives behavioral change bullets from durable findings/evidence where it can do so conservatively, adds test/documentation bullets only when the stored result supports them, and renders independent review evidence as compact pass/test/scope facts while preserving raw commands in `praector result show`.

Rerunning is idempotent. If a PR already exists for the Collection branch, Praector records/reuses it rather than creating another. Existing open PR bodies are preserved by default so human edits are not silently overwritten; use `--refresh-copy` when Praector should replace the body with regenerated deterministic copy. After `praector collection revise` completes fresh reviewed work, rerun `collection pr` to push the updated integration branch into that same open PR.

The PR command does not perform direct acceptance. If GitHub eventually merges the PR, `praector collection accept COL-...` can record the already-contained acceptance afterward.

## Accept a completed Collection

Recommended sequence:

```bash
praector collection diff COL-... --stat
praector collection accept COL-... --dry-run
praector collection accept COL-...
```

Default target is the Project base branch.

Acceptance verifies:

- Collection `COMPLETE`;
- explicit passing independent review for implementation work;
- clean Collection integration worktree;
- clean target worktree;
- safe fast-forward by default.

### Diverged target

If `main`/`master` advanced independently, default acceptance refuses.

Inspect the divergence. If an explicit merge commit is intended:

```bash
praector collection accept COL-... --no-ff
```

### Accept another target

```bash
praector collection accept COL-... --target develop
```

### Accept and cleanup

```bash
praector collection accept COL-... --cleanup
```

Also delete merged Praector branches:

```bash
praector collection accept COL-... --cleanup --delete-branches
```

See [WORKSPACES.md](WORKSPACES.md#acceptance) for exact Git semantics.

## Cleanup later

```bash
praector collection cleanup COL-...
```

Delete safe merged branches too:

```bash
praector collection cleanup COL-... --delete-branches
```

Before acceptance, worktree-only cleanup requires explicit force:

```bash
praector collection cleanup COL-... --force
```

This is not equivalent to accepting the work.

## Memory and decisions

Accepted project memory:

```bash
praector memory add \
  --scope project \
  --state accepted \
  --kind constraint \
  --text "Never log PHI"
```

Inspect:

```bash
praector memory list --state accepted
praector memory list --state candidate
```

Decision:

```bash
praector decision add \
  --statement "Use tenant-scoped SAML configuration" \
  --rationale "Existing identity boundary is tenant scoped"
```

If another external memory layer is attached to OpenCode, remember that it is independent of Praector's own Dolt memory. Disable external memory when you need to isolate/test Praector's memory behavior specifically.

## Deterministic planning gates

Praector enforces a scheduler-level planning policy floor. The coordinator may still choose extra planning specialists, but it cannot waive a required gate by calling it ceremony. Once an `implement` task exists, Praector classifies the Collection objective using deterministic risk signals.

Current mandatory triggers include non-trivial implementation requests, security/authentication/credential-sensitive work, stateful/rate-limit/concurrency behavior, persistence/schema/migration work, destructive/irreversible changes, and external provider/gateway or external-contract changes. A non-trivial implementation request requires `plan_review`; external integration/contract changes also require a `critic`. Clearly trivial documentation-only edits remain ungated. Delivery classification recognizes both imperative wording (`Add ...`) and common conversational requests such as `I'd like to add ...` or `Can you fix ...`.

**Risk gates fire from the detected risk category, not the objective's verb.** Each detected category (security, network trust boundary, external protocol/standard, external integration, external contract) now enables the gate that category justifies — `external_conformance`, `threat_model` (which feeds `plan_review`), and the integration-sensitive critic — even when the objective is phrased as a noun phrase or in the passive voice ("The auth bypass in the token check requires correction" gets the same threat-model/plan-review gate as "Fix the auth bypass"). Genuinely implementation-ceremony terms stay verb-based on purpose: the `plan_review` `implementation and not trivial_docs` term, the `delivery_policy.implementation_required` classification, and the research freshness triggers, so descriptive/trivial prose is not turned into plan-review theater. `explore` is de-verbed to `implementation OR any risk category recorded` (never unconditional). This widening is **forward-scoped**: collections created on or after the `GateCoherenceCutoff` date use the coherent category-derived gates, while collections created before it keep the historical verb-ANDed gates at every enforcement seam (scheduler staging, the claim gate, COMPLETE reconciliation, submit-time oracle strictness, and acceptance critic-stale) so pre-existing work is never retroactively re-gated or reopened. `praector collection show` reports the resolved `planning_policy` (the audit surface), and the `PLANNING_POLICY_ENFORCED` event carries an additive `gate_derivation` field (`category` vs `verb_legacy`).

**The plan-review claim floor is an artifact invariant, so a classifier miss is visible rather than silent.** Independently of the classifier, an implement in a coherent (post-`GateCoherenceCutoff`) Collection is never claimable until the Collection's latest non-cancelled/non-superseded `plan_review` reaches a terminal `COMPLETE` **with a passing verdict**; the floor holds it via the same claim seam and releases without the policy path's research/traceability/fingerprint/edge/coordinator extras. The scheduler also auto-stages the missing final plan review for a write-bearing collection even when all four gates classified false, and a `WAITING` coordinator (control already handed off) no longer suppresses that staging. When a claim is held by the floor rather than by the policy path, the store emits a dedicated `PLAN_REVIEW_FLOOR_HELD` event (with `hold_kind=plan_review_floor`) on the refusal, so an operator can tell why a `READY` implement is not starting: a policy-held claim shows up under `PLANNING_POLICY_ENFORCED`/the policy path, a floor-held claim under `PLAN_REVIEW_FLOOR_HELD`. A `COMPLETE verdict=fail` review does not release the floor at all: the floor requires the same passing verdict the policy path requires, and the fail-review repair lineage's newer (nonterminal) review is a second, independent hold. Pre-cutoff collections keep the exact pre-floor behavior.

For example, API-key authentication plus per-key rate limiting is classified as `implementation_change`, `security`, and `stateful_behavior`, so the minimum pre-write path is:

```text
coordinator
  -> critic
  -> plan_review
  -> implement
  -> review
```

If the coordinator already created suitable gates, Praector reuses them. While a coordinator is still active, deterministic policy does **not** auto-create a competing `Policy plan review`; the claim-time write barrier keeps implementation unclaimable while the coordinator publishes its final blocking review. If planning terminates without a required plan review, the scheduler then creates the fallback policy review and gates pending/ready implementation tasks. A failed plan review uses the normal durable coordinator-repair loop. Inspect the computed policy with:

```bash
praector collection show COL-...
```

The JSON includes `planning_policy` with `critic_required`, `plan_review_required`, categories, and reasons. It also includes `critic_revalidation`, showing the latest critic, sealed and current material-plan fingerprints, baseline source, and `stale` state. `revision_plan_seal` identifies fresh human-revision planning; `compatibility_plan_seal` is legacy-only, and bootstrap can append a corrected provenance event for the known legacy revision-publication race when lineage is provable.

For required-critic work, the first critic is sealed when the coordinator reaches its first final plan-review handoff. If a later coordinator repair/revision changes implementation/research/advice contracts or meaningful dependencies, Praector blocks plan review and write claims, creates `Revalidate materially changed plan`, then creates `Apply critic revalidation` coordinator work. If that coordinator makes another material change, another critic round is expected. Status changes, timestamps, coordinator/reviewer bookkeeping, and orchestration-only edges do not trigger revalidation.

## Gate traceability: reading a rejected submit and clearing a legacy exception

Directive traceability is enforced deterministically at three points: the reviewer's `praector result submit`, the claim-time planning barrier, and collection acceptance. Sibling-effect (impact) enumeration is enforced at exactly two points: the reviewer's `praector result submit` and collection acceptance — there is no claim-time impact-enumeration gate. No CLI command, flag, or output is involved.

A rejected review/plan-review submit names exactly what is missing, deterministically:

- `directive_dispositions did not disposition directive ids: T-…,T-…` — the result must add one disposition object per listed stable directive id (`<source-task-id>:<kind>:<ordinal>`), each with `disposition` (`satisfied`/`deferred`/`explicit_exception`) and concrete `evidence`. Rerun guidance: the reviewer simply resubmits with the dispositions; the ids come verbatim from the Envelope's `inherited_directives`.
- `directive_dispositions <id> deferred requires a durable decision_id` — record the decision first (`praector decision add --statement ... --collection COL-...`) and resubmit citing it.
- `directive_dispositions <id> is a MUST-level directive and cannot be deferred` — a directive whose level is MUST (structured `normative_level` or a leading `MUST:`/`MUST NOT:` prefix) must be resubmitted as `satisfied`, or as `explicit_exception` with a durable decision id; deferral is reserved for SHOULD-level and unlevelled directives.
- `requires citable evidence (a file path, a test name, or a command)` — the disposition or impact-site evidence is generic prose ("verified in code", "n/a"). Cite something a human can open: a path such as `internal/praector/policy.go`, a test name such as `TestDirectiveTraceabilityRejectsMissingDispositionsThenAcceptsCompleteSet`, or a command such as `go test ./internal/praector/ -run TestDirective` / `make test-go`.
- `references unknown directive ids (report genuinely new directives under additions, not dispositions)` — move the new directive into `additions` with a `reason`.
- `passing review requires impact_enumeration …` / `impact_enumeration.sites[N] …` — the passing review must name its enumeration method and dispose every listed sibling-effect site with citable evidence; an introduced configuration knob with no `reader_paths` is reported as the blocking introduced-but-never-read defect.
- `impact_enumeration does not account for the derived configuration candidates …` — Praector derived candidate introduced knobs from the collection diff (`git diff base...integration` added lines: flag literals, environment-variable names, config/table keys, setting-like keys; git's own flags and well-known environment names are exempt). Account for each named candidate by listing it in some site's identifier/`reader_paths`, or add an `impact_enumeration.candidate_dispositions` entry: `not-introduced`, or `deliberately-unread` with a durable decision id. The self-reported `introduced_knob` flag cannot decline this rule; the derived list is heuristic and human-falsifiable, not a completeness proof. The candidate list is recomputed against the collection integration branch at each check, so a genuinely new knob appearing at acceptance is correctly required rather than treated as drift.
- `oracle-exception review requires a non-empty probes list …` — the conformance-exception result must add probes with `scenario`, `command_or_test`, and `observed_outcome`.

If a completed review's gap is only discovered later, Praector records a `DIRECTIVE_TRACEABILITY_GAP` event on that task (visible via `praector collection show` / events) instead of silently passing.

Two more auditable events make previously silent results distinguishable, and neither one blocks work — they are informational. A `CONFIG_BASIS_UNAVAILABLE` event is recorded (at most once per collection) when the diff-derived knob candidate derivation cannot proceed at all because the project root is not a Git repository or the base/integration branch is absent, naming the missing ref; this is deliberately distinct from a change that genuinely introduced no knobs, which records nothing. A `DIRECTIVE_TRACEABILITY_EMPTY_HARVEST` event is recorded (at most once per consumer, by the scheduler sweep and the acceptance check) when a completed review/plan_review has a directive-bearing critic/plan_review/advise ancestor that yields zero harvestable directives, naming the consumer and the checked source task ids; a review with no planning ancestor at all records nothing. If you see a `CONFIG_BASIS_UNAVAILABLE` event, confirm the collection's integration branch (and its base) actually exist and were integrated — an absent branch means candidate accounting did not run. If you see an `DIRECTIVE_TRACEABILITY_EMPTY_HARVEST` event, the planning ancestor completed but emitted no directives worth tracking; treat it as a prompt to check that upstream planning output, not as a failure to resolve.

Collections whose reviews completed before this contract have results without the new fields. New submissions are always strict, but the retroactive claim-time and acceptance gates honor one documented, scoped escape hatch so history is not rewritten or retroactively failed. An operator creates it with `praector decision add` against the specific collection — without `--task` (a worker session always records decisions task-scoped, and a task-scoped decision can never arm the escape) — and the statement must name the collection id:

```bash
praector decision add --collection COL-... \
  --statement "Legacy gate-traceability exception: pre-contract review results accepted as-is for COL-... <why>" \
  --rationale "<evidence>"
```

A marker decision exempts a collection only when it is collection-scoped to that collection (`collection_id` set — a project-wide decision or one scoped to a different collection does nothing), its statement names that collection id, and it carries no task scope. That trio is the deliberate operator-ownership rule: the decisions table has no actor column, so scope plus task-NULL is the schema-free discriminator, and the naming requirement means a scoped decision cannot blanket a whole project. It never waives derived-configuration-candidate accounting, and it has no effect on new submissions. A wrongly created marker is retired by superseding it — `praector decision add --collection COL-... --supersedes DEC-... --statement "Retire erroneous legacy exception for COL-..." ...` — which re-enables the gates immediately (superseded decisions are ignored everywhere). Deleting is not required and should be avoided; the retirement decision keeps the audit trail. The honest default remains letting the reviewer resubmit with real dispositions.

## Troubleshooting

### `Required executable not found on PATH: dolt`

Check the same shell/environment used to invoke Praector:

```bash
command -v dolt
python -c 'import shutil; print(shutil.which("dolt"))'
praector doctor
```

### Reviewer exits without a structured verdict

A `review` or `plan_review` session may occasionally exit successfully without calling `praector result submit --stdin` with an explicit `verdict: pass|fail`. That is a reviewer protocol failure, not a substantive rejection of the implementation or plan. Praector retries the same durable review Task in a fresh Attempt while retry budget remains. Only after that budget is exhausted does the review become FAILED and coordinator attention become eligible.

If a coordinator supersedes a broken review with a replacement review, Praector preserves the original review prerequisites and the implementation `verifies` lineage. Collection reconciliation also requires every completed implementation to have a completed passing verifier before reporting the Collection as `COMPLETE`; otherwise it remains `BLOCKED`.

On upgrade, reconciliation also backfills this lineage for older collections that already contain a same-role `superseded_by` review chain, so an already-completed passing replacement reviewer can become valid acceptance evidence without rerunning the implementation.

### OpenCode exits immediately and log shows usage text

Inspect:

```bash
praector logs T-... --tail 120
opencode run --help
```

Praector uses the tested invocation style:

```text
opencode run --pure --format json --auto --agent build --dir <task-worktree> ...
```

### OpenCode returns `UnknownError: Unexpected server error` immediately

First inspect the Task log:

```bash
praector logs T-... --tail 120
```

If the log contains only an OpenCode `UnknownError` event and no model/tool activity, the failure may be in OpenCode's headless/session setup rather than in the Task prompt or model reasoning. Praector synchronizes the child process `PWD` with the assigned task worktree because some OpenCode `run` versions have resolved project/session context from both process cwd and `$PWD`.

Verify OpenCode independently from the Task worktree if needed, then retry the durable task without losing its failed Attempt history:

```bash
praector task retry T-... --reason "Transient OpenCode headless failure"
```

The original Task and all failed Attempt logs remain auditable; the retry is a new replacement Task linked by `superseded_by`.

### Worker appears to operate in the original checkout

Inspect log tool calls and Task state:

```bash
praector task show T-...
praector logs T-... --tail 120
```

The workdir should be under `$PRAECTOR_HOME/worktrees/...`.

### Worker Python resolves to the operator shell's virtualenv

Managed OpenCode runs strip an inherited active `VIRTUAL_ENV` activation before launch. The native Praector executable does not need a Python environment; the sanitization remains to stop the operator shell's environment from becoming the project runtime. If you manually enter a Task worktree from an activated shell, that shell still owns its activation.

For project tests, activate the project's own environment explicitly, for example:

```bash
./.venv/bin/python -m pytest
```

### Review is `VERIFYING`

This is normal immediately after result submission. The review becomes dependency-visible `COMPLETE` only after its process exits and Praector finishes deterministic post-processing.

### Collection complete but base branch unchanged

Expected. Completion and acceptance are separate:

```bash
praector collection accept COL-... --dry-run
praector collection accept COL-...
```

## Development/test reset

For isolated testing, prefer a disposable `PRAECTOR_HOME`:

```bash
export PRAECTOR_HOME=/tmp/praector-test
```

A full manual destructive reset is documented in [STATE_AND_RECOVERY.md](STATE_AND_RECOVERY.md#full-local-reset-for-developmenttesting).

## OpenCode process environment

Managed OpenCode runs preserve the caller/provider environment except for an inherited active Python virtualenv activation. Parent-virtualenv sanitization is enabled by default: Praector removes `VIRTUAL_ENV`, `VIRTUAL_ENV_PROMPT`, and only that virtualenv's executable-directory entry from worker `PATH`. This prevents project worker commands such as `python` and `pip` from accidentally resolving into the operator shell's environment.

`PWD` is forced to the assigned Praector worktree, matching subprocess cwd and OpenCode `--dir`, so all three working-directory views remain consistent. Other provider/user environment variables remain available.

If a project intentionally requires the parent Python virtualenv to remain active, opt out explicitly:

```bash
PRAECTOR_SANITIZE_PARENT_VENV=0 praector daemon
```

The native executable is already runtime-neutral; no pipx/uv environment is needed for Praector itself.

Attempt logs include a small `# HARNESS` diagnostic header with the command, profile, worktree, model, prompt size/hash, inherited virtualenv path, and whether parent-venv sanitization was enabled. It does not dump the full environment or prompt.


## Planning-only completion on a change request

If a direct change Collection contains only coordinator/critic/research tasks and no implementation, current Praector marks it `BLOCKED` and may create one `Delivery policy repair` coordinator. Inspect `praector collection show COL-...` for `delivery_policy`. Direct change objectives include both imperative and conversational requests and should show `implementation_required: true`.

Workers should always have `praector` available even when parent-venv sanitization is enabled. The harness injects a dedicated control shim and exposes its path as `PRAECTOR_CLI`. If a worker reports `praector: command not found`, inspect the attempt's `# HARNESS` header for `praector_control=` and run `praector doctor` from the operator shell before retrying.


## Blocking dependency completion invariant

A task with an unresolved `depends_on` edge cannot become `COMPLETE`, even if its model submits a structured `status=complete` result after requesting blocking work. Praector defers completion to `WAITING` and records `TASK_COMPLETION_DEFERRED`; a fresh attempt is released only after all hard dependencies are `COMPLETE`. Delivery-policy repair is a terminal backstop and does not start while coordinator/explore/critic/plan-review/research/advise work remains nonterminal.

### Observing policy-gated writes

It is valid for an implementation task to appear `READY` while planning is still in progress; `READY` is durable task state, not proof that the scheduler may claim it. For objectives requiring plan review, the claim-time barrier prevents execution until the latest valid review is complete and the coordinator has terminalized. A `RUNNING implement` before those conditions is an invariant violation and should be reported.

### Dependency cycles and plan-review repair

`depends_on` is guaranteed acyclic for newly committed graph mutations. If a coordinator or internal recovery path tries to add an edge that would close a transitive cycle, Praector rejects it before commit.

For upgraded collections that already contain a coordinator/plan-review cycle, reconciliation removes the review-side coordinator prerequisite that closes the loop. If two unstarted final plan reviews exist, the older gate is superseded into the newest one and waiting coordinators/implementations are rewired to the survivor. A healthy repaired graph should therefore converge to one final plan review with evidence prerequisites, followed by implementation and independent review.


### Revalidation handoff convergence

A coordinator in `WAITING` is not an active planning prerequisite; it has already handed control to a blocking child/review. Final plan-review strengthening therefore ignores waiting coordinators. This prevents a review that already releases a coordinator from being made to wait on that same coordinator in reverse.

Coordinator-requested final review is single-flight. If a non-failed nonterminal `plan_review` already exists, a later coordinator request reuses that gate, attaches any newly required critic/research evidence, blocks the caller on it, and gates current implementations. If the review was still waiting on that active coordinator, the handoff atomically flips that prerequisite into the blocking review-to-coordinator edge. Expect `PLAN_REVIEW_REUSED` rather than a second active final-review task. Failed plan-review tasks are not reused; they follow the normal repair-and-rerun path.

### Legacy dependency-cycle migration and quarantine

`praector daemon` must remain available even when an older Collection contains a hard-dependency cycle that predates the current DAG invariant. At bootstrap and before Collection reconciliation, Praector scans for recognized legacy cycle edges. It automatically removes only the two edge shapes that are invalid by the pre-write protocol:

- `implement -> plan_review` when the review is also a prerequisite of that implementation;
- `coordinator -> plan_review` when that review already reaches back to the coordinator.

Each removal emits `LEGACY_DEPENDENCY_CYCLE_REPAIRED`. If a cycle has another shape, Praector does not infer a destructive repair. The Collection is set `BLOCKED`, emits `DEPENDENCY_CYCLE_QUARANTINED`, and is excluded from task scheduling while other Collections continue through the same daemon. The daemon prints the quarantine once per process for visibility.

## External-research capability

Project markers include:

```toml
[capabilities]
current_external_research = true
```

The default is `true`, including for legacy project rows that predate the field. Set it to `false` when the configured harness must not be trusted to retrieve current external information. A Collection that deterministically needs fresh research, or a specialist that raises `RESEARCH_REQUIRED`, is then blocked/quarantined with `RESEARCH_CAPABILITY_REQUIRED` rather than continuing from model memory. Re-run project initialization/registration after changing durable project configuration so the stored project config is refreshed.
