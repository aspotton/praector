# Developing Praector

[Documentation index](README.md) · [Architecture](ARCHITECTURE.md) · [Coding-agent guide](../AGENTS.md)

This document is for humans and coding agents changing the Praector codebase.

## Start with the invariants

Before changing behavior, read:

- [`ARCHITECTURE.md`](ARCHITECTURE.md)
- [`../AGENTS.md`](../AGENTS.md)

The most important invariant is:

> **Agents request durable work; they never own child-agent process lifecycle.**

Do not solve an orchestration problem by making model sessions more persistent or by reintroducing hidden background-agent ownership.

## Source layout

The native Go implementation is the primary runtime. The Python implementation remains temporarily as a reference implementation so behavior can be compared while the native runtime is hardened.

```text
cmd/praector/main.go              executable entry point
internal/praector/
├── cli.go                    CLI/operator/worker command surface
├── service.go                shared typed domain service layer (CLI/MCP parity seam)
├── config.go                 global/project configuration
├── store.go                  Dolt persistence + graph/state operations
├── scheduler.go              execution/retry/recovery/review policy
├── policy.go                 deterministic planning/delivery classifier
├── envelope.go               Task Envelope/context compiler
├── workspace.go              Git worktrees/integration/acceptance/cleanup
├── harness.go                OpenCode harness adapter
├── pull_request.go           PR provider + deterministic PR copy
├── lock_unix.go              Unix file lock adapter
├── lock_windows.go           Windows file lock adapter
├── process_unix.go           Unix process-tree control
├── process_windows.go        Windows Job Object process-tree control
└── *_test.go                 native unit/integration tests
internal/assets/
├── schema.sql                schema embedded in the executable
├── envelope.md               embedded Task Envelope template
└── prompts/*.md              embedded global/profile contracts
src/praector/                     Python reference runtime
tests/                        original Python regression suite
scripts/parity_check.py       black-box shared-contract comparison
```

`make check-assets` guarantees the embedded schema/profile prompts remain byte-identical to the Python reference copies while both implementations coexist.

## Responsibility boundaries

### `internal/praector/cli.go`

Good place for:

- argparse surface;
- project/context resolution;
- user-facing validation;
- formatting status/output;
- commands that call durable services.

Avoid:

- embedding Git merge/worktree implementation;
- embedding scheduler loops;
- provider-specific process control.

### `internal/praector/service.go`

`Service` is the shared typed domain seam that BOTH the CLI `cmd*` adapters and
the future MCP adapter call for the same domain operations, so the two
surfaces can never diverge ("now and always", decision DEC-28fcd38435). Each
`commandHelp` entry in `help.go` maps 1:1 to one typed `Op(ctx, Input) -> Output`
method via the `serviceOps` registry; `service_parity_test.go` enforces that
mapping in both directions.

Good place for:

- one typed domain op per CLI command, reusing existing `*Store` / `GitWorkspace`
  methods and deterministic policy classification;
- durable reads/mutations that an adapter would otherwise call inline.

Avoid:

- argument parsing, stdout formatting, or exit codes (adapter-only);
- scheduler/daemon process launching (the scheduler remains the sole launcher);
- external PR provider delivery and privileged accept/merge writes (adapter,
  opt-in); a second store or orchestration implementation.

### `internal/praector/store.go`

Good place for:

- durable CRUD/state transitions;
- graph relationships;
- task/attempt/session records;
- memory/decisions/events;
- semantic Dolt commit boundaries.

Avoid:

- OpenCode parsing;
- Git file operations;
- long-running scheduler logic.

### `internal/praector/scheduler.go`

Good place for:

- readiness/execution;
- retries;
- stale-session recovery;
- post-run verification;
- auto-review/repair policy;
- collection reconciliation.

Avoid:

- provider-specific subprocess details;
- ad hoc direct SQL when a store operation belongs in `store.go`;
- asking the model to perform deterministic scheduling/recovery work.

### `internal/praector/pull_request.go`

Good place for:

- provider abstraction for external pull requests;
- GitHub `gh` invocation;
- deterministic PR title/body assembly from durable results.

Avoid:

- scheduler/task-graph policy;
- silently merging/accepting Collections;
- storing provider credentials in Praector state.

### `internal/praector/workspace.go`

Good place for:

- branch/worktree creation;
- checkpoints;
- integration;
- acceptance and operator-driven pre-acceptance revision;
- cleanup;
- Git safety checks.

Avoid:

- task graph policy except where needed to choose an integration target;
- OpenCode process lifecycle.

### `internal/praector/envelope.go`

Good place for:

- runtime role contract;
- compiled durable context;
- delegation/result instructions.

Avoid:

- putting correctness-critical enforcement only in prompt text;
- dumping entire historical logs/conversations into every Attempt.

### `internal/praector/harness.go` and platform adapters

Good place for:

- one disposable harness run;
- provider-specific CLI/API arguments;
- provider-specific output/session parsing;
- process environment and permission injection.

Avoid:

- graph creation;
- scheduler policy;
- Git integration;
- persistent organizational state.

## Adding another harness

Praector currently exposes OpenCode. A future harness should implement the native harness boundary and be selected through configuration.

Do not add conditionals like:

```python
if harness == "x":
    ...
elif harness == "y":
    ...
```

throughout scheduler/workspace/database code.

Keep these dimensions independent:

```text
profile = implement
harness = opencode
model   = local/qwen-code
```

## Changing the agent protocol

When changing profiles, Task Envelope fields, delegation semantics, or result schema:

1. update the relevant `internal/assets/prompts/<profile>.md` role contract;
2. update `internal/praector/config.go` if the profile set changes;
3. update `internal/praector/envelope.go` when context composition or common protocol changes;
4. add/update tests in `tests/test_profiles.py`, `tests/test_envelope.py`, or integration tests;
5. update [`PROFILES.md`](PROFILES.md) and [`AGENT_PROTOCOL.md`](AGENT_PROTOCOL.md);
6. update [`ARCHITECTURE.md`](ARCHITECTURE.md) if the authority/lifecycle model changed;
7. update [`CLI.md`](CLI.md) / [`OPERATIONS.md`](OPERATIONS.md) when commands/configuration change.

Prompt guidance is not enough for safety-critical rules. Pair it with deterministic permissions/state validation where possible.

## Changing task/graph semantics

Graph changes should preserve explicit lineage.

Current important relations:

- `depends_on`
- `spawned_by`
- `verifies`
- `superseded_by`

If adding a relation, define:

- direction (`from`/`to` meaning);
- whether it affects readiness;
- whether it affects acceptance;
- whether it is lineage-only;
- how it should appear in context/graph output.

Add tests for restart/retry behavior when applicable.

## Changing workspace semantics

Read [`WORKSPACES.md`](WORKSPACES.md) first.

At minimum, test:

- original checkout remains untouched;
- correct base commit is used;
- inherited child integrates into parent, not Collection;
- independent implementation integrates into Collection;
- read-only Task mutation is rejected/rolled back;
- read-only envelopes require baseline-aware workspace hygiene so ordinary verification byproducts are cleaned without erasing pre-existing/candidate state;
- conflicts fail safely;
- acceptance does not silently switch/dirty the user's checkout;
- PR publication uses the Collection branch, is idempotent, and does not call acceptance.

## Changing state/schema

Read [`STATE_AND_RECOVERY.md`](STATE_AND_RECOVERY.md) first.

Schema changes must consider existing local Dolt repositories. `bootstrap()` currently creates missing tables and performs limited invariant repair; it is not a full migration framework.

When adding a durable field/table, decide:

- bootstrap behavior for existing state;
- whether old rows need repair/defaults;
- which transitions deserve semantic Dolt commits;
- which events should be emitted;
- how status/recovery logic behaves after a crash between writes.

## Acceptance changes

Acceptance is a privileged post-execution lifecycle action, not normal scheduler work.

Preserve:

- explicit operator/policy invocation;
- completed/passing review requirement;
- clean Git state validation;
- fast-forward-only default;
- explicit opt-in for merge commits;
- idempotency;
- durable audit record;
- conservative cleanup.

Acceptance tests live in `tests/test_acceptance.py`; durable human-revision lifecycle tests live in `tests/test_collection_revision.py`; Git integration tests remain in workspace coverage.

## Versioning and changelog

The native and reference-implementation versions must stay synchronized while both runtimes ship from one tree:

```text
internal/praector/version.go      native executable version
pyproject.toml                Python reference package version
src/praector/__init__.py          Python reference __version__
```

Release history belongs only in `../CHANGELOG.md`, not README or focused docs. Every repository change updates the changelog: add in-progress work under `Unreleased`; when publishing, move those entries into the new version section.

## Documentation maintenance

Documentation is part of the implementation contract.

When behavior changes, update the focused doc in the same change:

| Behavior | Document |
| --- | --- |
| CLI | `CLI.md` |
| profile roles/prompts | `PROFILES.md` |
| Envelope/delegation/result | `AGENT_PROTOCOL.md` |
| Git/worktrees/integration/acceptance | `WORKSPACES.md` |
| schema/state/events/recovery | `STATE_AND_RECOVERY.md` |
| install/operator workflow | `OPERATIONS.md` |
| architectural boundary | `ARCHITECTURE.md` |
| repo guidance for agents | `../AGENTS.md` |

Root `README.md` should remain an approachable entry point, not become the only complete reference.

## Tests

Run:

```bash
make check
```

Focused targets:

```bash
make test-go
make test-reference
make parity
make smoke
```

Current test areas include:

- OpenCode permission/invocation/environment behavior, including cwd/`PWD` synchronization;
- failed-task replacement/retry semantics;
- runtime profile prompt loading/capability contract;
- Task Envelope contract;
- project marker/config round trip;
- harness factory boundary;
- workspace inheritance/integration;
- acceptance validation and CLI safety defaults;
- utility behavior;
- documentation discoverability.

### Testing philosophy

Prefer tests at the boundary where the invariant matters.

Examples:

- If changing acceptance, create a real Git graph and prove divergence/refusal/idempotency.
- If changing inherited delegation, use real worktrees and prove child changes land in the parent branch first.
- If changing recovery, simulate stale Session/Attempt state and prove a fresh Attempt can resume from durable Git state.
- If changing read-only enforcement, prove unauthorized mutation cannot become integrated state.

## Manual smoke test

A useful end-to-end smoke objective is:

```text
Add a small /hello JSON endpoint, tests, and documentation.
```

Expected lifecycle:

```text
coordinator COMPLETE
implement   COMPLETE
review      COMPLETE (verdict=pass)
Collection  COMPLETE
```

Then:

```bash
praector collection accept COL-... --dry-run
praector collection accept COL-...
```

Verify:

- base branch contains accepted result;
- original checkout was not mutated during worker execution;
- acceptance is visible in `praector status` / `praector collection show`;
- re-acceptance is idempotent.

## Harness environment compatibility

The OpenCode harness must preserve the parent environment by default. Do not remove `PATH`, `VIRTUAL_ENV`, provider helper variables, or wrapper state at the harness boundary unless the behavior is explicitly opt-in and covered by a live compatibility test. `PWD` should match the actual task worktree.

## Planning-policy changes

Deterministic planning-gate classification lives in `internal/praector/policy.go`; scheduler enforcement lives in `Scheduler.enforcePlanningPolicy`. Keep the classifier conservative and auditable. Delivery intent must handle normal conversational mutation requests, not only first-token imperatives. Non-trivial implementation requests receive a plan-review floor; external integration/contract-sensitive changes may require critic as well. A policy change should add tests for the exact regression wording, a generalized positive trigger, and a simple objective that remains ungated. Do not move the decision entirely into prompts: the point of this layer is that mandatory independent review cannot be waived by the planner it is meant to review.

### Atomic task publication

A requested task is not scheduler-visible until its initial graph contract is durable. Praector writes the task row, initial `depends_on` edges, `spawned_by` edge, blocking child-to-parent edge, and parent `WAITING` transition under one state lock and semantic commit. This prevents a concurrent scheduler from claiming a task in the gap between task creation and dependency attachment. Coordinator-requested `plan_review` is always a blocking handoff and automatically carries the latest non-superseded critic as a dependency when one exists.


### Planning-race regression tests

Tests cover the scheduler race where an implementation row exists before a later coordinator call creates plan review. The suite verifies claim-time refusal before review, rejection of stale or unlinked reviews, required-critic inclusion, active-coordinator blocking, atomic plan-review-to-implementation edge publication, automatic coordinator finalization after a passing final blocking review, and fallback-review deferral while a coordinator remains active. The suite additionally tests stable material-plan fingerprinting, plan-review/write refusal under a stale critic, fresh critic/coordinator insertion after a material change, and repeated critic rounds when revalidation incorporation changes the material plan again.

Semantic-repair tests also require a genuine `review verdict=fail` to become terminal historical evidence, repair work to depend on that review, and repair integration to create a **fresh review Task/context** that verifies both original and repair implementation lineage. Acceptance tests allow historical failed review evidence only when every completed implementation is covered by a later independent passing review. Prompt-contract tests require critic/plan-review to block implementation contracts with known acceptance violations instead of relying on downstream repair.


## Runtime prompt hierarchy

`internal/assets/prompts/global.md` is the mandatory engineering contract inherited by every runtime profile. `internal/assets/prompts/<profile>.md` files specialize role behavior. `internal/praector/envelope.go` composes the effective prompt and must keep the global contract above the role-specific contract.

When changing global prompt semantics, update `docs/PROFILES.md`, `docs/AGENT_PROTOCOL.md`, relevant role prompts/tests, and repository README/AGENTS documentation in the same change. Do not duplicate the entire global contract into role prompts or material task objectives; reinforce only role-specific enforcement behavior.

### Dependency DAG invariant and plan-review convergence

Any change to task/graph semantics must preserve acyclicity of `depends_on`. Use the store helpers rather than writing hard edges directly. Tests for graph changes should cover both a one-hop cycle and a transitive cycle, plus any recovery path that rewires or supersedes tasks.

For pre-write planning, maintain exactly one effective final `plan_review` gate for the current graph. It may depend on evidence tasks but not on implementation execution or on a coordinator that reaches back to the review. Recovery tests should prove that legacy multi-node cycles converge by removing review-side coordinator back-edges, collapsing redundant unstarted plan reviews, rewiring dependents, and terminalizing waiting coordinators after the surviving review passes.


### Single-flight plan-review regressions

Planning-policy tests must cover the handoff distinction between active graph shapers and terminal handoff waiters. `WAITING` coordinators must never be added as prerequisites of the final review they await. Request-path tests must also prove that a coordinator asking for another final review reuses an existing non-failed nonterminal gate, strips caller-as-prerequisite input, attaches new evidence, and does not publish a parallel review task. Include the scheduler-strengthened case where `coordinator -> plan_review` already exists and final handoff must atomically flip it to `plan_review -> coordinator`.

### Legacy cycle migration tests

When changing dependency semantics, test both modern write-time rejection and migration of already-persisted bad graphs. Legacy repair must be conservative: auto-remove only edges whose invalidity follows from Praector protocol, leave unknown cycle shapes untouched, and verify daemon fault containment so a quarantined Collection cannot prevent unrelated Collections from reconciling or running.


## Pull-request copy regressions

When changing `praector collection pr`, keep tests for both result generations:

- current implementation results with explicit `pr_summary` and `changes` must render those reviewer-facing fields directly;
- older results without `changes` must recover multiple concrete change bullets from durable findings/evidence rather than collapsing to the implementation task title;
- verification remains grounded in passing independent review evidence and unresolved items remain durable-result derived.
