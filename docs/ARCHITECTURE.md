# Praector architecture

[Documentation index](README.md) · [Agent protocol](AGENT_PROTOCOL.md) · [Workspaces](WORKSPACES.md) · [State and recovery](STATE_AND_RECOVERY.md) · [Development](DEVELOPMENT.md)

## Purpose

Praector is a durable orchestration layer above disposable agent sessions. Its job is to move delegation, dependency tracking, retries, persistence, process lifecycle, workspace state, and acceptance out of model context and into deterministic software.

The central design rule is:

> **Agents request durable work; they do not own child-agent process lifecycle.**

OpenCode is the only harness implementation in the current release, but OpenCode sessions are deliberately treated as disposable execution attempts rather than durable organizational state.

## Core invariants

1. **Task != Attempt != Session.**
   - A Task is the durable unit of work.
   - An Attempt is one execution effort for that Task.
   - A Session is one disposable harness process/context.
2. **The graph is durable and dynamic.** Workers can discover new work at runtime.
3. **Git owns project files. Dolt owns orchestration state.**
4. **The user's original checkout is not an agent workspace.** Praector uses dedicated worktrees.
5. **Delegation crosses the Praector boundary.** Native OpenCode subagent/background-task delegation is disabled.
6. **Only the coordinator has graph-level authority.** Workers may request work or coordinator attention, not arbitrarily mutate the graph.
7. **Read-only profiles are enforced outside the model.** Tool permissions are paired with Git mutation detection/rollback.
8. **Implementation requires independent review.** A completed implementation is not acceptable without a completed passing verifier.
9. **Collection completion is not base-branch acceptance.** Acceptance is explicit, validated, and recorded separately.
10. **Session continuity is never required for correctness.** Restarting with a fresh OpenCode invocation must be safe.

## Entity hierarchy

```text
Organization          future
└── Project           current durable scope
    └── Collection    high-level objective / initiative
        └── Task      durable unit of work
            └── Attempt
                └── Session
```

### Project

A long-lived project identity. Praector currently maps one Project to one Git repository.

A Project owns:

- repository/root path;
- base branch;
- harness/model/profile configuration;
- execution limits;
- project memory and decisions;
- Collections.

The repository contains a local `.praector/project.toml` marker. Global durable state defaults to `~/.praector` or `$PRAECTOR_HOME`.

### Collection

A Collection is one objective created by `praector run`.

It owns:

- objective/title;
- evolving task graph;
- integration branch/worktree;
- collection-scoped memory/decisions;
- all Tasks needed to reach a terminal result;
- optional later acceptance records.

The graph is not merely the original plan. It is the durable record of the organization's evolving attempt to accomplish the objective.

### Task

A Task is a stable durable identity with fields such as:

- profile;
- objective/reason;
- parent/requester;
- depth/priority;
- status;
- dependency/lineage edges;
- workspace mode/branch/worktree/commit references;
- structured result.

A Task may have multiple Attempts.

### Attempt

One execution effort for a Task. Attempts record:

- attempt number;
- status;
- start/end time;
- exit code;
- OpenCode session identifier if available;
- log path;
- result snapshot.

The log path is persisted before OpenCode launches so a running attempt can be inspected with `praector logs ... --tail`.

### Session

One disposable harness invocation. In the current release this is one `opencode run` process.

Sessions record harness/process details, but are never the durable source of truth.

## Major components

```text
CLI / operator
     │
     ▼
DoltStore  ◄────────────── Task Envelope compiler
     │                            │
     │                            ▼
     ├────────► Scheduler ───► Harness(OpenCode)
     │              │              │
     │              ▼              │
     └────────► GitWorkspace ◄──────┘
                    │
                    ▼
               Git repository
```

### CLI — `internal/praector/cli.go`

Responsibilities:

- project/context resolution;
- runtime preflight;
- operator commands;
- task/delegation/result commands exposed to workers;
- acceptance validation and UX;
- status/log output.

The CLI should not own scheduling policy or Git implementation details.

### Store — `internal/praector/store.go`

Responsibilities:

- relational durable state;
- task graph edges;
- task/attempt/session transitions;
- memory/decisions/events;
- semantic Dolt commits at meaningful boundaries;
- acceptance audit records;
- invariant repair on bootstrap.

See [STATE_AND_RECOVERY.md](STATE_AND_RECOVERY.md).

### Scheduler — `internal/praector/scheduler.go`

Responsibilities:

- exclusive scheduler ownership;
- stale-session recovery;
- dependency readiness;
- bounded parallel execution;
- claim/retry behavior;
- post-run capability checks;
- checkpoint/integration;
- auto-review policy;
- review-failure repair loop;
- collection terminal-state reconciliation.

Scheduling and retries are deterministic software concerns, not coordinator-model responsibilities.

### Global engineering contract

Every disposable attempt receives the same packaged global engineering contract before its role-specific prompt. This contract is part of Praector's semantic continuity layer: minimal coherent edits, surrounding-impact/inheritance awareness, durable README/AGENTS knowledge preservation, simple/local-style implementation, useful docstrings, and scope restraint survive delegation, retry, and fresh-session boundaries without being copied into every material task objective. Role prompts specialize and enforce this common contract.

### Task Envelope compiler — `internal/praector/envelope.go`

Builds `ORCH TASK ENVELOPE v1` for every fresh attempt.

It compiles:

- task identity/objective/profile;
- assigned worktree;
- parent result;
- completed dependency results;
- accepted memory;
- active decisions;
- role boundary;
- allowed delegation operations;
- result schema/stop conditions.

See [AGENT_PROTOCOL.md](AGENT_PROTOCOL.md).

### Pull-request provider — `internal/praector/pull_request.go`

Owns external code-review publication semantics without moving provider details into the scheduler or Dolt store. Praector currently implements `GitHubCLIProvider` through the authenticated `gh` CLI and exposes a small provider protocol for repository lookup, find/view/create/open operations. PR body/title assembly is deterministic from durable Collection/Task results.

### GitWorkspace — `internal/praector/workspace.go`

Responsibilities:

- collection integration branch/worktree;
- task branches/worktrees;
- checkpoints;
- read-only reset/cleanup;
- inherited-child integration;
- collection-level integration;
- acceptance into a target branch;
- safe cleanup.

See [WORKSPACES.md](WORKSPACES.md).

### Harness — `internal/praector/harness.go`

The scheduler depends on a generic `Harness` contract, not on OpenCode internals.

Praector currently implements only `OpenCodeHarness`.

The adapter is responsible for one disposable run:

- clean inherited parent virtualenv leakage from the worker environment;
- synchronize `$PWD` with the assigned worktree as well as subprocess cwd/`--dir`;
- inject Praector's high-precedence OpenCode permissions;
- launch `opencode run --pure --format json --auto --agent build --dir <worktree>`;
- stream/capture output;
- expose PID/start callback;
- return process exit/final text/session metadata.

Provider-specific behavior belongs here, not in scheduler/database logic.

## Objective lifecycle

A normal collection follows this control flow:

```text
praector run "objective"
        │
        ▼
Project resolved + preflight
        │
        ▼
Collection + coordinator Task created durably
        │
        ▼
Scheduler launches fresh coordinator Attempt
        │
        ├── coordinator creates durable critic/plan_review/research/implement/review/advise Tasks
        │
        └── coordinator submits result and exits
        │
        ▼
Dependencies become READY as prerequisites COMPLETE
        │
        ▼
Worker Attempts run in isolated worktrees
        │
        ▼
Implementation checkpoint + integration
        │
        ▼
Independent review (auto-created if needed)
        │
        ├── pass ──► COMPLETE
        │
        └── fail ──► repair implement Task ──► fresh durable review Task/context
        │
        ▼
All graph work terminal/successful
        │
        ▼
Collection COMPLETE
        │
        ▼
explicit `praector collection accept`
        │
        ▼
base/target branch contains accepted result
```

## Dynamic delegation

The initial coordinator may seed a graph, but the graph remains appendable throughout execution.

A worker can request a new Task because it discovers:

- a new prerequisite;
- an out-of-scope implementation;
- missing evidence;
- a specialist judgment question;
- a need for independent verification;
- a plan assumption that is false.

Normal workers use `praector task request`. Graph-level reconsideration uses `praector coordinator attention`.

### Non-blocking delegation

```text
Parent RUNNING
  ├── request Child
  └── continue own bounded work

Child may execute concurrently when dependencies permit.
```

### Blocking delegation

```text
Parent RUNNING
   │
   ├── checkpoint if write-capable
   ├── create Child
   ├── dependency is recorded
   └── Parent -> WAITING; current Attempt exits

Child COMPLETE
   │
   ▼
Parent -> READY
   │
   ▼
fresh Parent Attempt with child result in the Envelope
```

No model session is kept alive merely to wait for a dependency.

## Profiles and authority

Praector currently uses one runtime contract and a small role-based profile taxonomy:

- `coordinator` — privileged planner and durable graph authority; read-only project files;
- `critic` — pre-planning gap/ambiguity/scope/QA critic; read-only;
- `plan_review` — practical executability reviewer for the proposed graph; read-only;
- `research` — bounded evidence gathering; read-only;
- `implement` — bounded project-file mutation;
- `review` — independent post-implementation verification; read-only;
- `advise` — strategic technical/architecture recommendation; read-only.

The distinction between `plan_review` and `review` is intentional: the former asks whether the work graph is executable before write work proceeds; the latter verifies the integrated implementation after write work completes. Domain specialization belongs in task context, not an ever-growing persona taxonomy.

Only the coordinator may directly perform graph repair such as adding dependencies or superseding tasks. Workers can request durable work and coordinator attention. A failed `plan_review` deterministically blocks the gate, wakes a coordinator for graph repair, and then reruns the same durable plan-review Task in a fresh Attempt.

See [PROFILES.md](PROFILES.md) for the built-in role contracts and [AGENT_PROTOCOL.md](AGENT_PROTOCOL.md) for runtime delegation/result mechanics.

## State machines

### Task

```text
PENDING/READY
     │
     ▼
  RUNNING
     │
     ├────────────► WAITING
     │                 │
     │       dependencies complete
     │                 │
     │                 ▼
     │               READY
     │
     ▼
 VERIFYING
     │
     └────────────► COMPLETE
```

Error/terminal variants:

```text
FAILED
CANCELLED
SUPERSEDED
```

`VERIFYING` is intentionally distinct. A model may submit its result before its OpenCode process exits. Dependents must not be released until deterministic post-processing finishes.

### Attempt

```text
CREATED -> STARTING -> RUNNING -> SUCCEEDED
                             ├-> FAILED
                             ├-> TIMED_OUT
                             └-> ABORTED
```

Task state and Attempt state are independent. For example, a concrete Attempt with non-zero process exit is `FAILED` even if the Task is moved back to `READY` for another retry.

Automatic retries reuse the same durable Task until its configured Attempt budget is exhausted. An operator-requested retry after terminal failure creates a fresh replacement Task and marks the exhausted Task `SUPERSEDED`; prior Attempts are never deleted or renumbered. Hard dependents are rewired to the replacement so recovery remains graph-safe and auditable.

## Review and repair policy

Every completed implementation must be independently reviewed.

```text
Implement
    │
    ▼
integrate candidate
    │
    ▼
Review #1
  ┌─┴─┐
 pass fail
  │    │
  │    └──► Repair(implement)
  │                 │
  │                 ▼
  │          Fresh Review #2
  │                 │
  │              pass/fail
  ▼
COMPLETE
```

A review Task cannot count as complete review evidence unless its structured result contains an explicit `verdict: pass` or `verdict: fail`.

Exiting a reviewer session without that structured verdict is a protocol failure, not a substantive fail verdict. Praector retries the same durable review Task first. A genuine `verdict=fail` is different: the failed review becomes terminal historical evidence, the repair implementation depends on it, and successful repair integration creates a **new durable review Task with a fresh context**. That fresh reviewer independently re-evaluates the repaired integrated candidate and verifies both the repair implementation and the original implementation it corrected. Collection terminal reconciliation therefore requires every completed implementation lineage to end in a completed passing verifier before the Collection may become `COMPLETE`.

Historical failed reviews remain visible for provenance; they do not permanently poison acceptance once a fresh post-repair review proves the corrected lineage. Protocol-broken reviewer replacements still use same-role supersession and preserve prerequisite/verification lineage.

## Workspace and integration boundary

The original user checkout is not a worker workspace.

Praector creates:

```text
praector/collections/<collection-id>       collection integration branch
praector/tasks/<collection-id>/<task-id>   implementation task branch
```

and physical worktrees under:

```text
$PRAECTOR_HOME/worktrees/<project>/<collection>/integration
$PRAECTOR_HOME/worktrees/<project>/<collection>/<task>
```

Read-only tasks receive detached worktrees. Implement tasks receive task branches.

An inherited implementation child merges back into its parent Task branch; an independent implementation merges into the Collection integration branch.

See [WORKSPACES.md](WORKSPACES.md).

## Collection completion vs delivery vs acceptance

A `COMPLETE` Collection is an acceptance-grade reviewed candidate. Publishing that candidate as a pull request and accepting it into a base branch are separate lifecycle actions:

```text
Collection COMPLETE
      │
      ├── praector collection pr ──> external PR delivery record
      │                            (Collection remains COMPLETE)
      │
      └── praector collection accept -> acceptance record / target integration
```

PR delivery always uses the Collection integration branch as the head. Task branches are never the external review boundary. Existing PRs are reused by head branch/provider identity, which lets `collection revise` update the same open PR after fresh reviewed work.


`COMPLETE` means the durable work graph finished successfully. It does not mean the result has been accepted into `main`/`master`.

```text
Collection COMPLETE
      │
      ├── inspect / diff / dry-run
      ├── defect found -> `praector collection revise` -> RUNNING -> repaired + freshly reviewed -> COMPLETE
      ▼
praector collection accept
      │
      ├── require explicit passing reviews
      ├── require clean integration/target worktrees
      ├── safe fast-forward by default
      ├── optional explicit --no-ff merge
      └── record acceptance in Dolt
      ▼
Target branch contains accepted result
```

PR delivery is stored in `collection_pull_requests`, while direct acceptance is stored in `collection_acceptances`; neither overloads scheduler state. Pre-acceptance human revision requests are stored in `collection_revisions`; an `OPEN` revision is itself a completion blocker until fresh repaired implementation is independently verified.

This keeps execution status simple and makes acceptance an auditable organizational boundary.

See [WORKSPACES.md](WORKSPACES.md#acceptance) and [OPERATIONS.md](OPERATIONS.md#accept-a-completed-collection).

## Persistence model

Praector deliberately uses two versioned stores:

### Git

Answers:

- what changed in project files?
- which commit/branch contains the work?
- how do implementation branches integrate?

### Dolt

Answers:

- what work existed?
- who/what requested it?
- what depended on what?
- why did work wait/retry/fail?
- what result/evidence/decision was recorded?
- what PR was published/updated for a Collection?
- what was accepted, when, and into which branch?

Operational SQL changes may happen frequently. Semantic Dolt commits are reserved for meaningful organizational transitions.

See [STATE_AND_RECOVERY.md](STATE_AND_RECOVERY.md).

## Recovery model

A scheduler restart does not rely on OpenCode session recovery.

On startup, running Session rows are treated as orphaned:

1. terminate the old process group if still alive;
2. mark Session/Attempt `ABORTED`;
3. return affected `RUNNING`/`VERIFYING` Tasks to `READY`;
4. preserve the Git worktree/branch;
5. launch a fresh Attempt when runnable.

The durable Task and Git state survive; the Session does not matter.

## Concurrency model

Praector is currently local/single-machine with bounded parallelism.

The control plane is a native Go executable. Unix and Windows use separate locking/process adapters; Windows worker trees are assigned to Job Objects so cancellation/recovery can terminate descendants without Unix signals.

- Only one scheduler/daemon owns execution at a time via an OS-native exclusive lock (Unix `flock`; Windows file locking).
- A thread pool executes runnable Tasks up to `max_parallel_tasks`.
- Dolt mutations are serialized through the store lock.
- Dependency readiness determines which Tasks may run.
- Blocking inherited delegation prevents parent/child concurrent mutation of the same logical code line.

Distributed scheduling is intentionally a future extension.

## Observability

Operator surfaces include:

```bash
praector status
praector graph COL-...
praector task list --collection COL-...
praector task show T-...
praector result show T-...
praector logs T-... --tail 100
praector logs COL-... --tail 100
praector collection show COL-...
```

Dolt also preserves an event stream and semantic commit history. Git preserves code history independently.

## Extension boundaries

The design leaves room for later additions without making them current dependencies. The prioritized sequence is maintained in [`../ROADMAP.md`](../ROADMAP.md). Important attachment points include:

- **MCP** is now a live interface over the same durable domain/service/store
  operations used by the CLI: when enabled and the daemon is running, Praector
  hosts a **loopback-only** (127.0.0.1) streamable-HTTP MCP listener as a goroutine
  decoupled from the reconcile loop and `scheduler.lock`. It performs only Store
  reads and durable enqueues on the same gated paths, never constructs a
  scheduler, and never launches processes or runs a collection inline. It is
  disabled by default, binds loopback only, and defers auth/identity to the
  remote boundary. The surface is **commandHelp-driven**: each CLI command maps
  to exactly one MCP tool (leaf command) or resource (command namespace /
  launch-only command) through the shared `serviceOps` registry, giving
  **command/surface coverage parity** (the surfaces cannot drift) rather than a
  claim of unrestricted behavioral parity. Read-only commands and pass-through
  queue writes reuse the identical gated `Store`/workspace methods; privileged
  write tools (`collection pr`/`accept`/`cleanup`, which only run the durable
  acceptance/PR validation and never push, merge, or implicitly accept, plus
   explicit-root project `init`/registration) sit
   behind a separate, default-off `privileged` opt-in
   (decisions DEC-1580dff0e0 / DEC-9997d2394f / DEC-28fcd38435 / DEC-aacb7bfc23).
   The listener advertises only the single MCP protocol revision it implements
   and **refuses** any other requested revision (never echoing an unsupported
   one), requires a matching `MCP-Protocol-Version` header on post-`initialize`
   requests (HTTP 400 otherwise), and — because it is loopback-only and
   unauthenticated — validates each request's Host/Origin as a loopback authority
   before dispatch (HTTP 403 on a foreign authority), as DNS-rebinding defense in
   depth on top of the socket-bind gate. MCP operations carry no implicit daemon
   cwd (a tool inferring a project requires an explicit `PRJ-*`/`COL-*`/`T-*`
   identifier), `init` requires an absolute `root_path`, and resources advertise
   accurate content types (`application/json` for JSON listings, `text/plain` for
   CLI/help text); an independent external conformance oracle (`scripts/
   mcp_conformance.py`) verifies the contract against a live loopback listener;

- **durable human/executive escalation** above Projects/Collections, with blocked work waiting on recorded decisions rather than live session continuity;
- **additional harnesses** through the native harness boundary;
- **multi-workspace/multi-repo Projects** above the current one-repo mapping;
- **remote/distributed executors** behind the scheduler/execution boundary;
- **schedules/events/watches** that create/wake Collections;
- **external integrations** that create Tasks or consume results;
- **richer memory/search** behind the durable context interface;
- **approval/RBAC policy** around acceptance and privileged actions;
- **web/desktop UI** over the same durable state model.

Do not implement these by weakening Praector's invariants. Extend the relevant boundary instead.

## Focused references

- Runtime agent contract: [AGENT_PROTOCOL.md](AGENT_PROTOCOL.md)
- CLI: [CLI.md](CLI.md)
- Git/worktrees/acceptance: [WORKSPACES.md](WORKSPACES.md)
- State/recovery/schema: [STATE_AND_RECOVERY.md](STATE_AND_RECOVERY.md)
- Operations: [OPERATIONS.md](OPERATIONS.md)
- Development: [DEVELOPMENT.md](DEVELOPMENT.md)

## Deterministic planning-policy floor

Coordinator discretion is intentionally bounded by a deterministic pre-write policy. `internal/praector/policy.go` classifies the Collection objective using conservative risk signals. The policy is exposed in Task Envelopes and enforced by the scheduler once pending/ready implementation work exists.

The policy currently recognizes security/authentication/credential-sensitive changes, stateful/rate-limit/concurrency behavior, persistence/schema/migration work, destructive changes, and external API/contract signals. High-risk combinations require `critic`; risky/stateful work requires `plan_review`. Missing mandatory gates are inserted before implementation can run, and the resulting plan-review task is added as a hard dependency of pending/ready write tasks.

This is deliberately a floor rather than a complete planner: the coordinator can add specialist tasks for other reasons, and the critic/plan reviewer still use model judgment. The deterministic layer only prevents a planner from opting out of independent controls on categories where Praector policy requires them.

The model-based planning gates also enforce **acceptance coherence**: critic and plan review must treat a task contract that knowingly violates a Collection MUST/MUST NOT, security boundary, or acceptance requirement as a blocker. A plan may not intentionally dispatch known-bad work on the theory that downstream implementation review will repair it. Praector does not yet have a durable fault-injection or human/exec exception authorization mechanism, so no worker may infer such an exception from prose; an intentional contradiction must remain blocked for operator resolution.

### Critic material-plan revalidation

A required critic is not valid forever. The initial critic is a preflight over the objective/current graph; when the first final plan-review handoff is created, Praector seals that critic to a stable fingerprint of the material plan. The fingerprint includes non-superseded `implement`, `research`, and `advise` contracts plus meaningful dependencies among them, while excluding status/timestamp churn and orchestration-only coordinator/reviewer edges.

If that fingerprint changes after the seal, claim-time gates refuse both `plan_review` and `implement`. After active coordinator mutation settles, Praector creates `Revalidate materially changed plan`, records the changed fingerprint as that critic's basis, and creates `Apply critic revalidation` coordinator work that depends on the fresh critic. The final plan review waits for both. If the revalidation coordinator changes the material plan again, the fingerprint changes again and another critic/coordinator round is required. This produces a convergent `critic -> coordinator -> critic ... -> plan_review` loop without re-running critics for ordinary status changes or for initial incorporation of the first critic's directives.

Final plan-review publication includes the corresponding critic baseline in the same Dolt mutation as the review task and its initial graph edges. This closes the inter-process gap where reconciliation could previously observe a new review with no baseline and incorrectly synthesize `compatibility_plan_seal`. Human acceptance revisions use `revision_plan_seal`; automatic stale-plan recovery uses `material_revalidation`; `compatibility_plan_seal` is reserved for legacy state.

Mutable workspace Git pointers are explicitly excluded from material-plan identity. `base_commit` is fingerprint-significant only for `inherit` workspaces, where it is an explicit delegation checkpoint. Collection/isolated tasks historically received a base SHA lazily when their worktree was prepared; that runtime fill must not invalidate an already-reviewed plan. Praector preserves compatibility with legacy raw-base seals while using the canonical form for new seals.

## Delivery completion invariant

Planning success is not delivery success. For direct mutation objectives, deterministic `delivery_policy` requires at least one completed `implement` task with passing independent review before the Collection may become `COMPLETE`. If planning terminates without write work, the scheduler creates one bounded `Delivery policy repair` coordinator. If that coordinator still creates no implementation, the Collection remains `BLOCKED` rather than looping indefinitely or falsely completing.

Worker environment isolation also separates project runtime from Praector control-plane access. An inherited active Python virtualenv is removed from worker `PATH`, then a dedicated `~/.praector/control-bin/praector` shim points to/copies the native executable and is prepended. This keeps `praector task request`, `praector result submit`, and related control commands available without coupling project `python`/`pip` to the operator shell.

### Atomic task publication

A requested task is not scheduler-visible until its initial graph contract is durable. Praector writes the task row, initial `depends_on` edges, `spawned_by` edge, blocking child-to-parent edge, and parent `WAITING` transition under one state lock and semantic commit. This prevents a concurrent scheduler from claiming a task in the gap between task creation and dependency attachment. Coordinator-requested `plan_review` is always a blocking handoff and automatically carries the latest non-superseded critic as a dependency when one exists.


### Claim-time write barrier

Cross-process orchestration cannot assume several coordinator CLI calls form one transaction. A coordinator may create implementation work and only afterward request plan review. Praector therefore treats deterministic planning policy as a claim-time capability boundary: policy-gated `implement` tasks cannot be claimed until the latest valid plan review is complete and explicitly gates them, any required critic is incorporated, and no coordinator remains active. Creating a coordinator-requested plan review atomically publishes its edges to existing implementation tasks. To avoid duplicate review gates, deterministic policy does not auto-create a fallback `Policy plan review` while any coordinator remains active; the fallback is created only after planning terminates without a required gate. A passing final blocking plan review terminalizes the waiting coordinator instead of reopening planning.

### Hard-dependency DAG and plan-review convergence

All `depends_on` edges form an acyclic directed graph. The store rejects an edge `A -> B` when a path already exists from `B` back to `A`; task creation validates the whole initial dependency/blocking/gating batch before publishing the task row. This is a control-plane invariant, not a model convention.

`plan_review` is a pre-write gate over durable implementation contracts. It may wait for prerequisite evidence (`critic`, `research`, `advise`) but must not depend on `implement` execution or on a coordinator that transitively waits on that review. Reconciliation detects those legacy coordinator/review back-edges, removes the review-side prerequisite, and if repair activity produced multiple unstarted final reviews, supersedes older gates into the newest one. Waiting coordinators and implementations are rewired to that one surviving gate. When it passes, every waiting coordinator whose hard dependencies are satisfied is terminalized instead of being awakened to reshape an already approved graph.


### Single-flight final review and coordinator handoff

Final plan review is a single-flight control-plane gate. When a coordinator requests `plan_review` and a non-failed nonterminal final review already exists, the store reuses that task instead of publishing another review row. New critic/research/advice prerequisites, caller blocking, and implementation gates are attached atomically to the existing review. If scheduler strengthening previously made that review depend on the active coordinator, the explicit final handoff atomically deletes `coordinator -> plan_review` and publishes `plan_review -> coordinator` in the same transaction.

`WAITING` coordinators are excluded from plan-review prerequisite strengthening. Waiting means the coordinator has already handed control to another durable task; wiring it back ahead of the same review generation would create a reverse-edge cycle. Only `PENDING`, `READY`, `RUNNING`, or `VERIFYING` coordinators are considered active graph shapers. The blocking caller itself is also stripped from explicit plan-review prerequisites because the valid handoff edge is `plan_review -> coordinator`.

### Legacy DAG migration and scheduler fault containment

Acyclicity applies to persistent upgraded state as well as new mutations. Before normal reconciliation, the store detects cycles and removes only recognized historical pre-write violations (`implement -> plan_review` and cyclic `coordinator -> plan_review`). Unrecognized legacy cycles are quarantined at Collection scope. Scheduler fault containment guarantees that one quarantined Collection cannot stop the daemon or consume worker slots; runnable work is collected only from non-quarantined Collections.
