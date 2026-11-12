# AGENTS.md — guide for coding agents working on Praector

This file is the **repository entry point for AI/coding agents modifying Praector itself**. It is not the runtime prompt for Praector worker profiles. Runtime worker behavior is documented in [`docs/AGENT_PROTOCOL.md`](docs/AGENT_PROTOCOL.md).

## Read this first

Before making architectural or behavioral changes, read:

1. [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) — system boundaries and invariants;
2. [`docs/DEVELOPMENT.md`](docs/DEVELOPMENT.md) — repository layout and change workflow;
3. the focused document for the subsystem you are changing:
   - runtime profile roles/prompts: [`docs/PROFILES.md`](docs/PROFILES.md)
   - agent/delegation semantics: [`docs/AGENT_PROTOCOL.md`](docs/AGENT_PROTOCOL.md)
   - CLI: [`docs/CLI.md`](docs/CLI.md)
   - Git/worktrees/acceptance: [`docs/WORKSPACES.md`](docs/WORKSPACES.md)
   - Dolt/state/recovery: [`docs/STATE_AND_RECOVERY.md`](docs/STATE_AND_RECOVERY.md)
   - operator-facing behavior: [`docs/OPERATIONS.md`](docs/OPERATIONS.md)

The documentation index is [`docs/README.md`](docs/README.md).

## Repository skill

Use the repo-local [`$praector`](.agents/skills/praector/SKILL.md) skill whenever a request operates, inspects, or explains the live Praector control plane through the `praector` binary or its MCP surface: Projects, Collections, Tasks, graphs, status, results, logs, revisions, retries, delivery/acceptance plans, or cleanup. Do not load it merely because a task edits Praector's source code; ordinary repository development continues to follow this file and the focused docs above.

## Central invariant

> **Agents request durable work; they never own child-agent process lifecycle.**

Do not reintroduce in-session/background-agent orchestration as the source of truth.

## Architectural invariants

Preserve these unless the change explicitly redesigns them and updates the architecture docs/tests:

- `Project -> Collection -> Task -> Attempt -> Session` are distinct durable concepts.
- **Task != Attempt != Session.** Session continuity is never required for correctness.
- The scheduler owns process lifecycle, retries, dependency readiness, recovery, and integration. Hard `depends_on` edges are an acyclic DAG invariant; never bypass the store cycle guard. Upgraded legacy cycles must be normalized before reconciliation, and unknown cycle shapes are quarantined to their Collection rather than guessed at or allowed to terminate the global daemon.
- OpenCode is a harness adapter, not the orchestration database or scheduler.
- The optional MCP surface is another adapter over the same `Service`/Store paths as the CLI: it is disabled by default, binds **loopback only**, and is enqueue-only — it never constructs a scheduler, launches processes, runs a collection, or bypasses policy/review/acceptance. Its tool/resource surface is commandHelp-driven, giving **command/surface coverage parity** (each CLI command maps to exactly one tool/resource via `serviceOps`, so the surfaces cannot drift) rather than a claim of unrestricted behavioral parity; read-only and pass-through queue-write tools reuse the identical gated `Store` methods, and the privileged write tools (`collection pr`/`accept`, which only validate/plan and never push/merge/accept; `collection cleanup`, which performs real worktree/branch removal; plus explicit-root project `init`/registration) sit behind a separate, default-off `privileged` opt-in. It advertises only the single MCP protocol revision it implements and meets any other well-formed requested revision with a counter-offer advertising the implemented revision (never echoes an unsupported one; Invalid-params is reserved for a missing/empty/non-string/malformed version, the missing/empty edge being a documented fail-closed hardening), requires a matching `MCP-Protocol-Version` header on post-`initialize` requests, validates each request's Host/Origin as a loopback authority before dispatch (rejecting a foreign, literal `null`, or otherwise-opaque Origin; DNS-rebinding defense on top of the socket-bind gate), carries no implicit daemon cwd (project inference requires an explicit `PRJ-*`/`COL-*`/`T-*` id, and a collection-only context resolves its owning project via a fail-closed store-backed resolver), and requires an absolute `init` `root_path`; `collection cleanup`/`init` are described as performing real removal/registration (not plan-only), and resources advertise accurate content types. The push surface targets MCP **2025-06-18** explicitly (the current 2026-07-28 revision differs; its subscriptions/listen + MRTR model is deferred roadmap, not current conformance): a `GET /mcp` advertising `Accept: text/event-stream` opens an **observation-only** SSE stream carrying only `notifications/message` logging frames and **never a JSON-RPC response** (a non-advertising `GET` still gets 405 and the POST surface stays byte-for-byte unchanged); the stream is **loopback-gated identically to POST** (the shared authority gate runs above method dispatch and the `MCP-Protocol-Version` header is required unconditionally before any byte), fan-out is **non-blocking** on the write path via a bounded side-channel drained outside the Dolt file lock (a stalled client at most drops and closes its own stream, never stalling `Store.Event()`), and **all stream/level/replay state lives in the Praector wiring layer, never inside the domain-agnostic `internal/praector/mcp` package** (DEC-17fa4b4aff).
- Project file state belongs in **Git**; organizational/orchestration state belongs in **Dolt**.
- Workers execute in Praector-managed worktrees, never by mutating the user's original checkout.
- `coordinator`, `explore`, `critic`, `plan_review`, `research`, `review`, and `advise` are read-only with deterministic Git mutation checks.
- Every read-only worker must treat no-net-workspace-mutation as part of completion: capture its starting Git status, prefer verification modes that avoid incidental repository writes, restore only artifacts/changes caused by its own checks, and verify the final status matches the starting baseline. Never erase pre-existing or candidate state merely to make the worktree clean.
- Only `implement` owns project-file mutation in the current release.
- Native OpenCode `task`/subagent delegation remains disabled for managed runs.
- Workers request new durable tasks through Praector; they do not spawn processes directly.
- A blocking delegation ends the current attempt and later resumes the same durable Task in a fresh attempt/session.
- Every completed implementation needs independent passing review before acceptance.
- Completed reviews require an explicit `verdict: pass|fail`. A read-only capability violation or missing reviewer verdict is an execution/protocol failure, not a semantic rejection: retry the same durable Task within its attempt budget before any graph-level repair or coordinator escalation.
- Deterministic planning policy is a safety floor: implementation receives repository exploration and critic preflight; freshness-sensitive external work also receives fresh research before critique; non-trivial/higher-risk work requires `plan_review`; mandatory gates may not be waived by coordinator preference.
- `COMPLETE` does not mean merged to the base branch. Pull-request delivery and direct acceptance are explicit, separate, and audited.
- External PRs use the Collection integration branch as the head; never publish an individual Task branch as the Collection PR boundary.
- Safe acceptance is fast-forward-only by default.

## Repository map

```text
README.md                    human entry point
ROADMAP.md                   prioritized hardening/MCP/escalation direction
CHANGELOG.md                 release history and per-change log
AGENTS.md                    coding-agent entry point
.agents/skills/praector/     repo-local skill for operating Praector through the CLI or MCP
docs/README.md               documentation index
docs/ARCHITECTURE.md         authoritative design overview
docs/PROFILES.md            runtime role map and built-in prompt contracts
docs/AGENT_PROTOCOL.md       runtime agent/task protocol
docs/CLI.md                  command reference
docs/WORKSPACES.md           Git/worktree/integration/acceptance
docs/STATE_AND_RECOVERY.md   Dolt/state machines/recovery
docs/OPERATIONS.md           install/run/operate/troubleshoot
docs/DEVELOPMENT.md          code layout/change guidance
cmd/praector/main.go              native executable entry point
internal/praector/cli.go          CLI parser and operator commands
internal/praector/service.go        shared typed domain service layer (CLI/MCP parity seam)
internal/praector/store.go        Dolt persistence and graph operations
internal/praector/scheduler.go    deterministic scheduler/executor/recovery
internal/praector/policy.go       deterministic pre-write risk/gate classifier
internal/praector/mcp_server.go   MCP tool/resource bridge over the shared service layer
internal/praector/mcp_sse.go      SSE stream registry/level/replay wiring (stream state kept out of the mcp core)
internal/praector/mcp/            dependency-light stdlib MCP core + loopback transport
internal/praector/envelope.go     Task Envelope/context compiler
internal/praector/pull_request.go external PR provider + deterministic PR copy
internal/praector/workspace.go    Git workspace/integration/acceptance
internal/praector/harness.go      OpenCode harness adapter
internal/praector/lock_*.go       OS-specific store/scheduler locking
internal/praector/process_*.go    OS-specific worker process-tree control
internal/assets/schema.sql    embedded durable relational schema
internal/assets/prompts/*.md  embedded global/profile operating contracts
internal/praector/*_test.go       native executable/state regression tests
src/praector/                     Python reference implementation
tests/                        original Python behavioral regression contract
```

## Where behavior belongs

Keep responsibilities separated:

- **CLI (`internal/praector/cli.go`)**: parse input, resolve project/context, validate operator actions, format output, and launch the scheduler. Every domain operation delegates to the shared service layer.
- **Shared service layer (`internal/praector/service.go`)**: one typed `Op(ctx, Input) -> Output` per CLI command that BOTH the CLI and the future MCP adapter call, so the surfaces cannot diverge. It performs durable domain reads/mutations via the existing store/workspace only; adapters own parsing/formatting/exit codes and process launching. The `serviceOps` registry maps each `commandHelp` entry 1:1 to an op; a parity test enforces this. It must not launch processes or introduce a second store/orchestration path.
- **Dolt store (`internal/praector/store.go`)**: durable entities, graph edges, state transitions, attempts/sessions/events/memory/decisions/PR/acceptance records.
- **Scheduler (`internal/praector/scheduler.go`)**: readiness, execution, retry/recovery, deterministic planning-gate enforcement, post-run verification, review/repair policy.
- **Planning policy (`internal/praector/policy.go`)**: auditable deterministic classification that sets mandatory pre-write safety gates.
- **Pull requests (`internal/praector/pull_request.go`)**: provider-specific PR lookup/create/open behavior and deterministic PR copy; no scheduler policy or merge semantics.
- **Workspace (`internal/praector/workspace.go`)**: branches, worktrees, checkpoints, integration, acceptance, cleanup.
- **Envelope (`internal/praector/envelope.go`)**: model-facing semantic contract/context compilation.
- **Harness (`internal/praector/harness.go`)**: launch/observe one disposable model/tool session. Do not move scheduling policy here.
- **MCP (`internal/praector/mcp/` + `mcp_server.go`)**: the MCP protocol core, loopback-only transport, and the tool/resource bridge over the shared service layer. It adds no orchestration/process-launch path; every MCP action delegates to a typed `Service` op. The bridge is driven off `commandHelp`/`serviceOps` so every CLI command projects to exactly one MCP tool (leaf) or resource (namespace / launch-only). Enqueue-only "execute" actions mirror the CLI `--detach` path and rely on the running daemon; privileged collection-finalizing tools remain default-off behind an independent `privileged` opt-in. The server-initiated SSE stream is another observation-only adapter surface: the domain-agnostic `mcp/` core owns only the GET gate/negotiation and the `logging/setLevel` protocol shape, while every stream, per-connection level, and Last-Event-ID replay decision lives in `mcp_sse.go`, fed by a non-blocking store emit seam (`Store.emitEvent`) wired by the daemon — it launches no process and bypasses no gate.

Add future harnesses behind the native harness boundary; do not scatter provider-specific branches through scheduler/store code. The Python tree is a regression oracle, not the primary place for new runtime behavior.

## Change-impact checklist

When changing a subsystem, update the corresponding docs in the same change:

| Change | Update |
| --- | --- |
| CLI command/flag/output | `docs/CLI.md`, `.agents/skills/praector/references/cli.md` (derive the usage table from `internal/praector/help.go` `commandHelp`; enforced by `TestSkillCLICommandHelpCoversCommandHelp`), and README/OPERATIONS if user-facing workflow changes |
| Runtime global/profile prompt semantics | `internal/assets/prompts/global.md`, `docs/PROFILES.md`, `docs/AGENT_PROTOCOL.md`, possibly `docs/ARCHITECTURE.md` |
| Deterministic planning-gate policy | `docs/PROFILES.md`, `docs/AGENT_PROTOCOL.md`, `docs/ARCHITECTURE.md`, `docs/OPERATIONS.md` |
| Task Envelope/delegation/result schema | `docs/AGENT_PROTOCOL.md`, possibly `docs/ARCHITECTURE.md` |
| Git branch/worktree/integration/PR/acceptance | `docs/WORKSPACES.md`, `docs/ARCHITECTURE.md` |
| Schema/state transitions/events/recovery | `docs/STATE_AND_RECOVERY.md`, `docs/ARCHITECTURE.md` |
| Install/config/operator workflow | `README.md`, `docs/OPERATIONS.md` |
| Repository/module architecture | `AGENTS.md`, `docs/DEVELOPMENT.md`, `docs/ARCHITECTURE.md` |
| Planned public/MCP/escalation direction | `ROADMAP.md`, plus the focused architecture/protocol docs when behavior lands |
| Version/release | `internal/praector/version.go`, `pyproject.toml`, `src/praector/__init__.py`, `CHANGELOG.md` |

Do not leave behavior discoverable only by reading source or old conversation history.

## Validation

Run before committing:

```bash
make check
```

For native/runtime changes also use `make parity` and `make smoke`; `make test-reference` runs the original Python regression contract.

When changing orchestration semantics, add tests that cover the durable behavior, not only a helper function. In particular, prefer integration-style tests for:

- parent/child workspace inheritance;
- review/repair loops;
- acceptance/divergence/idempotency;
- crash/restart recovery;
- read-only mutation enforcement;
- task/attempt/session state transitions.

## Collection pull-request boundary

A reviewed `COMPLETE` Collection may be published with:

```bash
praector collection pr COL-...
praector collection pr COL-... --refresh-copy  # explicitly refresh an existing open PR body

Implementation results should include `pr_summary` plus a concise `changes` array so `collection pr` can build reviewer-facing PR copy without a new model pass. Older results fall back to durable findings/evidence.
```

The PR head is always the Collection integration branch. The command may push/create/reuse an external GitHub PR, but it must not merge the target or synthesize another model task. PR metadata is durable state separate from `COMPLETE` and from direct acceptance. Reruns after a completed revision must update/reuse the same open PR rather than duplicate it.

## Collection acceptance boundary

Agents and the scheduler may produce a `COMPLETE` Collection, but they must not implicitly merge it into the Project base branch.

Operator flow:

```bash
praector collection diff COL-... --stat
praector collection accept COL-... --dry-run
praector collection accept COL-...
```

`--no-ff` is an explicit opt-in for a diverged target. Acceptance is stored in Dolt separately from execution state. Cleanup is optional and must not delete unaccepted branches.

## Documentation rule

For future agents: **treat the repository docs as durable project context**. Do not depend on this chat or prior model sessions to understand why the system works this way. If a design decision changes, encode it in the appropriate document and test.

After substantive repository writes, inspect the relevant `README.md` and `AGENTS.md` files before finishing. Update them when behavior, architecture, interfaces, developer workflow, constraints, conventions, or operational assumptions changed; if they remain accurate, leave them untouched rather than creating ceremonial churn.

`CHANGELOG.md` is different: **every repository change must update it.** Record in-progress work under `Unreleased`; when publishing a release, move the accumulated entries into the new version section. Do not put package release numbers or release-history prose back into README or focused docs.

### Single-flight plan-review handoff

A `WAITING` coordinator has already handed control to a blocking child/review and is not an active graph shaper. Never add a waiting coordinator as a prerequisite of a final `plan_review`. Coordinator-requested final review is single-flight: reuse an existing non-failed nonterminal `plan_review` and attach new evidence/gating edges atomically instead of creating a parallel review generation. A blocking plan-review caller must not also be a prerequisite of that same review; if the review currently waits on that active coordinator, final handoff atomically flips the edge direction.

### Dependency DAG and final plan-review convergence

`depends_on` is an acyclic DAG invariant. Before any hard edge is committed, Praector checks whether the dependent already reaches the proposed prerequisite transitively; if so the edge is rejected. This applies to ordinary graph mutations and to the atomic initial edge batch published with a new task.

`plan_review` remains a pre-write gate over durable implementation contracts. It may wait for prerequisite evidence (`explore`, `critic`, `research`, `advise`) but must not depend on `implement` execution or on a coordinator that is itself waiting, directly or transitively, on that review. Reconciliation repairs legacy coordinator/review cycles and collapses multiple unstarted final plan reviews to one newest surviving gate, rewiring waiting coordinators and implementations to that gate.

Fresh external evidence is a first-class planning input. When `planning_policy.research_required` is true, Praector creates fresh research alongside exploration, requires the critic to consume both, carries structured source/claim/oracle provenance in Task Envelopes, and includes completed research evidence in material-plan fingerprints. `RESEARCH_REQUIRED` is the durable escape hatch for critic/plan-review/review/advise uncertainty; workers must not substitute model memory for current evidence. Project capability `[capabilities] current_external_research = false` intentionally blocks such work.

Task attempts also record input lineage. If material planning/research/prerequisite lineage changes while implementation is running, its independent review is promoted to a reconciliation review against the current accepted state; do not bypass `RECONCILIATION_REQUIRED` by treating the old implementation assumptions as authoritative.

### Semantic review repair lineage

A successful implementation review with `verdict=fail` is terminal evidence of a real defect, not a protocol failure. Record it as `COMPLETE`, create repair implementation work that depends on that failed review, and after repair integration create a **new durable review Task/context**. The fresh review must independently verify the repaired candidate and carry `verifies` lineage for both the repair implementation and each original implementation verified by the failed review. Acceptance may retain historical failed reviews, but every completed implementation must have a later completed independent `verdict=pass`. Do not revert to the older model where the same failed review Task waits on and resumes after its repair.

### Acceptance-coherent planning

Critic and `plan_review` must block a graph that knowingly violates Collection MUST/MUST NOT, security, or acceptance requirements. Do not intentionally dispatch a known-bad implementation with the expectation that downstream review will repair it. Praector currently has no durable fault-injection/human/exec exception authorization; until such a mechanism exists, intentional acceptance contradictions remain blocking operator issues.
