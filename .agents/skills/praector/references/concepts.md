# Praector mental model

Use this reference when lifecycle, authority, policy, review, recovery, or Git/Dolt behavior affects the answer.

## What Praector is

Praector is a durable orchestration layer above disposable agent sessions. Models request durable work; deterministic software owns process lifecycle, dependencies, retries, recovery, workspace isolation, integration, and acceptance checks. OpenCode is the current worker harness, not the scheduler or orchestration database.

The central invariant is:

> Agents request durable work; they never own child-agent process lifecycle.

The optional MCP server is a loopback-only adapter over the same typed service and Store operations used by the CLI. It is command/surface coverage parity, not unrestricted behavioral parity. Queue-writing MCP calls persist work for the daemon; they do not run a scheduler or worker inside the MCP request.

Runtime state defaults to `~/.praector` and can be isolated with `PRAECTOR_HOME`. That home contains Dolt state, Attempt logs, managed worktrees, and state/scheduler locks. Only one scheduler/daemon owns execution for a given Praector home.

## Durable hierarchy

| Entity | Meaning | Key consequence |
| --- | --- | --- |
| Project | Long-lived repository identity and configuration | Owns base branch, limits, capabilities, memory, decisions, and Collections |
| Collection | One high-level objective and evolving task graph | Owns the integration candidate and remains distinct from acceptance |
| Task | Stable unit of work with profile, dependencies, workspace, and result | Survives retries and has one or more Attempts |
| Attempt | One execution effort for a Task | Records exit status, timestamps, log, and result snapshot |
| Session | One disposable harness process/context | Never required for correctness or continuity |

`Task != Attempt != Session`. A failed process may yield a failed Attempt while the same Task returns to `READY` for another Attempt.

## Durable state and project files

- Git owns project-file state, branches, checkpoints, integration, and accepted commits.
- Dolt owns Projects, Collections, Tasks, graph edges, Attempts, Sessions, events, memories, decisions, revisions, PR records, and acceptance records.
- The user's original checkout is not a worker workspace. Praector workers use managed worktrees under `$PRAECTOR_HOME/worktrees/...`.
- Collection candidates live on `praector/collections/<collection-id-lowercase>`; implementation work uses `praector/tasks/<collection>/<task>` branches.

## Task and Collection states

Typical Task flow:

```text
PENDING/READY -> RUNNING -> VERIFYING -> COMPLETE
                      \-> WAITING -> READY (after dependencies)
```

Other terminal/error states include `FAILED`, `CANCELLED`, and `SUPERSEDED`.

`VERIFYING` means the result was submitted but Praector is still waiting for process exit and deterministic post-processing such as capability checks, checkpointing, integration, and review setup. Do not treat it as dependency-visible completion.

Collection state is derived from the graph:

- `RUNNING`: work is executing, verifying, runnable, or still has a durable running Session.
- `BLOCKED`: incomplete work remains but nothing is runnable or executing.
- `COMPLETE`: the successful durable graph is terminal and implementation lineages have passing independent review. This is still not delivery or acceptance.

## Profiles and authority

| Profile | Purpose | Project-file capability |
| --- | --- | --- |
| `coordinator` | Plans and owns graph-level authority | read-only |
| `explore` | Maps current repository/system evidence | read-only |
| `critic` | Challenges intent, assumptions, scope, and QA before planning | read-only |
| `plan_review` | Verifies graph executability before writes | read-only |
| `research` | Gathers bounded current/authoritative external evidence | read-only |
| `advise` | Recommends one strategic technical direction | read-only |
| `implement` | Makes the bounded project-file change | write |
| `review` | Independently verifies the integrated implementation | read-only |

Only coordinators may perform graph repair such as adding dependencies or superseding Tasks. Other workers request durable work or coordinator attention. Read-only mutation checks are deterministic; a capability violation is an execution/protocol failure, not a semantic review rejection.

## Planning and evidence gates

Praector enforces a deterministic safety floor. Implementation receives repository exploration and critic preflight. Work requiring current external facts also receives fresh research before critique. Non-trivial, security-sensitive, stateful, migration, destructive, or integration work requires plan review. The coordinator can add safety work but cannot waive required gates.

A typical material change path is:

```text
explore (+ research when required) -> critic -> coordinator graph
  -> plan_review -> implement -> independent review
```

An implementation may appear `READY` while claim-time policy still prevents it from running. Inspect Collection planning policy and critic freshness rather than assuming `READY` means claimable.

## Delegation and waiting

A non-blocking durable child may run concurrently while its parent continues. A blocking request moves the parent to `WAITING`, ends the current Attempt, and later releases the same durable parent Task into a fresh Attempt after the child completes. No model session waits in the background.

Hard `depends_on` edges form an acyclic DAG. Never invent IDs or add a cross-Collection edge. Unknown legacy cycles quarantine only the affected Collection; recognized historical plan-review cycles are repaired conservatively.

## Review and repair

Every completed implementation needs independent passing review. A reviewer must submit `verdict: pass|fail`.

- Missing verdict or read-only capability violation: protocol/execution failure; retry the same durable review Task while budget remains.
- Genuine `verdict=fail`: durable defect evidence; create repair implementation depending on that failed review, then create a fresh review Task/context after repair integration.
- Historical failed reviews may remain, but every completed implementation must have a later independent passing verifier before acceptance.

`plan_review` is different: it checks the graph before implementation. It cannot depend on implementation execution. A failed plan review remains a blocking gate while coordinator repair occurs.

## Completion, revision, delivery, acceptance, cleanup

These are separate boundaries:

```text
Collection COMPLETE
  |-- revise (only before acceptance) -> RUNNING -> fresh implement/review -> COMPLETE
  |-- collection PR delivery -> external PR record; still COMPLETE
  `-- explicit acceptance -> target branch integration + acceptance record
```

Acceptance is fast-forward-only by default. A diverged target requires an explicit operator choice for `--no-ff`. MCP PR and acceptance tools only validate/build plans; they never push or merge. MCP cleanup is different: with privileged tools enabled, it performs real worktree removal and can delete safe reachable branches when explicitly requested.

An accepted Collection is immutable. Follow-up work belongs in a new Collection rather than `collection revise`.

## Recovery

On daemon restart, Praector aborts orphaned running Sessions/Attempts, returns affected Tasks to `READY`, preserves Git state, and launches fresh Attempts when runnable. Session continuation is never a correctness requirement.

Operator retry after exhausted failure creates a replacement Task, marks the old Task `SUPERSEDED`, preserves Attempt history, and rewires hard dependents. Diagnose before retrying; retry does not erase evidence.

## MCP security and context

The listener is local and unauthenticated, so it binds loopback only and validates Host and Origin to resist DNS rebinding. It advertises one implemented MCP revision and requires a matching protocol-version header after initialization.

MCP operations have no implicit daemon cwd. Project-aware tools need an explicit `PRJ-*`, `COL-*`, or `T-*` context; `praector_run` always needs `project`, and MCP `init` always needs an absolute `root_path`. A remote/cloud MCP client cannot reach the loopback listener without a separately designed remote boundary, which this release intentionally does not provide.
