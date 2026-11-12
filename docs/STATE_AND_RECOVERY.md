# Durable state, data model, and recovery

[Documentation index](README.md) · [Architecture](ARCHITECTURE.md) · [Workspaces](WORKSPACES.md)

Praector stores organizational/orchestration state in a local Dolt repository. Git independently stores project-file state.

Default Praector home:

```text
~/.praector
```

Override with:

```bash
export PRAECTOR_HOME=/path/to/praector-home
```

The Dolt repository lives at:

```text
$PRAECTOR_HOME/state
```

## Why Dolt

Praector needs ordinary relational queries for current state and versioned history for meaningful organizational changes.

Dolt provides both:

- SQL tables for Projects/Collections/Tasks/etc.;
- working-set updates for operational state;
- semantic commit history for important transitions.

Git answers "what changed in code?". Dolt answers "what work existed, why, and what happened to it?"


## Read-only capability violations

Read-only profiles (`coordinator`, `explore`, `critic`, `plan_review`, `research`, `review`, and `advise`) are instructed to baseline their worktree before verification, avoid incidental repository writes where practical, restore only byproducts caused by their own commands, and confirm the final worktree state matches the starting baseline before result submission. This prevents common test/cache side effects from unnecessarily consuming attempts while preserving candidate and pre-existing state.

Praector still checks the worktree after every harness run. If a read-only worker leaves mutations behind, Praector rolls those changes back deterministically. That condition is an execution/protocol failure, not a semantic verdict on the plan or implementation. While the Task still has attempt budget, Praector records the attempt as failed, returns the same durable Task to `READY`, and emits `READ_ONLY_CAPABILITY_RETRY`. It does **not** create coordinator repair work merely because the read-only worker violated its capability boundary.

Only after the retry budget is exhausted does the Task become terminally `FAILED`, at which point normal coordinator-attention recovery may apply. This is distinct from an independent review that successfully submits `verdict=fail`; a real failed verdict represents an actual defect finding. The failed review completes as historical evidence, a repair implementation is created, material-plan revalidation applies as usual, and successful repair integration receives a **new independent review Task/context** rather than resuming the failed reviewer. The fresh reviewer verifies both the repair and the original implementation lineage. Missing review verdicts follow the retry-before-escalation principle.

## Main tables

The native executable embeds the schema from `internal/assets/schema.sql`; `make check-assets` verifies it is byte-identical to the Python reference `src/praector/schema.sql`.

### `projects`

Long-lived Project identity and configuration snapshot.

Key fields:

- `id`
- `name`
- `root_path`
- `base_branch`
- `config_json`
- timestamps

### `collections`

One high-level objective.

Key fields:

- `id`
- `project_id`
- `title`
- `objective`
- `status`
- `integration_branch`
- `integration_worktree`
- timestamps

### `tasks`

Durable work nodes.

Key fields:

- Project/Collection IDs;
- parent/requester IDs;
- profile/title/objective/reason;
- status/priority/depth;
- workspace mode;
- Git base/branch/worktree/head;
- dedupe key;
- structured result JSON.

### `task_edges`

Directed graph relations.

Relations currently used include:

- `depends_on`
- `spawned_by`
- `verifies`
- `superseded_by`

The schema allows additional relationship types without changing Task identity.

### `attempts`

One execution attempt per durable Task.

Key fields:

- attempt number;
- status;
- exit code;
- external session ID;
- log path;
- start/end timestamps;
- result JSON.

### `sessions`

Disposable harness/process metadata.

Key fields:

- Attempt ID;
- harness;
- external session ID;
- PID;
- status;
- workdir;
- start/end timestamps.

### `events`

Append-style causal/operational event records.

Events make it possible to inspect what happened without reconstructing every transition from current rows alone.

`praector events [TARGET]` reads this table **directly from the store** (no daemon required). Ordering is deterministic and oldest-first by `created_at` then `id`; `created_at DATETIME(6)` is the monotonic ordering key while `EVT` ids are random hex, so a page emits a `next_cursor` packing the last row's `(created_at, id)` pair as one opaque token. Passing it back as `--since` is an **exclusive** composite cursor that never repeats or skips a row across pages. A collection- or task-scoped read uses the existing `(collection_id, created_at)` / `(task_id, created_at)` indexes; a project-only read has no index and is therefore always bounded by `--limit`.

### `memories`

Project/Collection durable context.

Fields include:

- scope (`project`/`collection`);
- state (`candidate`/`accepted`);
- kind;
- text;
- evidence;
- provenance IDs.

Only accepted memory is injected into Task Envelopes by default.

### `decisions`

Durable statements with rationale/evidence/alternatives and optional supersession.

This supports "why did we do X?" independently of old model transcripts.

### `collection_revisions`

Durable operator/human acceptance-feedback records for reopening completed but unaccepted work. Important fields include:

- revision id;
- Collection and Project ids;
- mandatory revision reason;
- status (`OPEN` or `SATISFIED`);
- coordinator task id;
- creation/satisfaction timestamps.

An `OPEN` revision prevents Collection `COMPLETE`. Reconciliation marks it `SATISFIED` only after fresh post-revision implementation reaches a passing independent review and all post-revision work is terminal.

### `collection_pull_requests`

Durable delivery records for Collection pull requests. Important fields include:

- provider (`github` in the current release);
- remote/repository;
- PR number and URL;
- state/draft flag;
- base/head branches;
- Collection source commit last pushed/recorded;
- title/actor/timestamps.

There is one provider record per Collection. PR state is intentionally separate from Collection execution state and from `collection_acceptances`: `COMPLETE` means Praector finished reviewed work, an open PR means that candidate has been published for external review, and acceptance means the work is recorded as integrated into a target branch. Bootstrap creates this table for existing Dolt state without rewriting historical Collections.

### `collection_acceptances`

Audit records for explicit post-completion acceptance.

See [Acceptance records](#acceptance-records).

## Task state machine

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

Additional terminal/error states:

```text
FAILED
CANCELLED
SUPERSEDED
```

### Why `VERIFYING` exists

A worker can submit a result while its OpenCode process is still alive.

`VERIFYING` prevents downstream dependencies from being released before Praector completes deterministic post-processing such as:

- process exit confirmation;
- read-only mutation checks;
- implementation checkpointing;
- Git integration;
- review setup;
- review verdict handling;
- result metadata persistence.

## Attempt states

```text
CREATED -> STARTING -> RUNNING -> SUCCEEDED
                             ├-> FAILED
                             ├-> TIMED_OUT
                             └-> ABORTED
```

Attempt state represents the concrete harness invocation. It is intentionally not inferred from eventual Task retry behavior.

A non-zero OpenCode exit is a failed Attempt even if the durable Task is moved back to `READY` for another try.

## Collection status derivation

Collection status is derived from its durable Task graph.

### `COMPLETE`

All Tasks are in successful terminal states (`COMPLETE`, `CANCELLED`, `SUPERSEDED`) and at least one completed work node exists.

### `RUNNING`

At least one Task is executing/verifying, a Task is runnable, or a durable harness Session in the Collection is still `RUNNING`. Counting the Session matters during delegation handoff: a parent Task may already be `WAITING` while its OpenCode process is still finishing protocol work, and children are intentionally held until that parent Session exits.

### `BLOCKED`

Tasks remain incomplete, but none are currently executable/runnable and no durable harness Session is still running.

The scheduler continuously reconciles this state.

## Attempts, retries, and limits

A Task may have multiple Attempts up to the Project's configured `max_attempts_per_task`.

Executor/harness errors may return a Task to `READY` when attempts remain. Exhaustion leads to failure and, for non-coordinator work, coordinator-attention creation when possible.

## Plan-review repair loop

`plan_review` uses the same durable WAITING/READY mechanics as other blocking delegation, but its repair target is the graph rather than project files. A failed plan-review result causes Praector to:

1. create a blocking coordinator-attention Task;
2. move the durable plan-review Task to `WAITING`;
3. keep implementation Tasks that depend on the plan-review gate non-runnable;
4. let the coordinator repair/supersede/resequence graph work;
5. return the same plan-review Task to `READY` after the coordinator completes;
6. run a fresh Attempt against the current durable graph snapshot.

This loop never depends on resuming the same model session.

## Crash recovery

OpenCode session continuation is not used as a correctness mechanism.

On scheduler startup/recovery:

1. query Sessions still marked `RUNNING`;
2. if their recorded PID/process group is alive, terminate it;
3. mark Session `ABORTED`;
4. mark Attempt `ABORTED`;
5. move affected Task from `RUNNING`/`VERIFYING` back to `READY`;
6. retain Git branch/worktree state;
7. let the normal scheduler launch a fresh Attempt.

This makes the durable Task and Git checkpoint the recovery boundary.

## Scheduler ownership

Only one scheduler/daemon owns execution at a time using an OS-native exclusive file lock under `$PRAECTOR_HOME`.

Dolt mutations are also serialized with a separate state lock.

This is a deliberate current simplification for local single-machine operation.

## Events and semantic commits

Praector records normal SQL updates for current state and creates semantic Dolt commits at meaningful transitions.

Examples include:

```text
collection COL-...: create ...
task T-...: create implement task
task T-...: waiting on T-...
task T-...: integration complete
task T-OLD: superseded by T-NEW
collection COL-...: accepted into main at <commit>
```

The event table provides a finer-grained causal trail; Dolt commits provide semantic snapshots/history.

Inspect directly:

```bash
cd "$PRAECTOR_HOME/state"
dolt status
dolt log
dolt diff HEAD~1 HEAD
```

## Memory

Current memory is intentionally simple.

Accepted Project memory:

```bash
praector memory add \
  --scope project \
  --state accepted \
  --kind constraint \
  --text "Never put PHI in logs"
```

List accepted memory:

```bash
praector memory list --state accepted
```

Workers may return `memory_candidates`, which are stored as candidates rather than automatically promoted to accepted truth.

This prevents one worker's suggestion from silently becoming organizational fact.

## Decisions

Manual decision:

```bash
praector decision add \
  --statement "Use tenant-scoped SAML configuration" \
  --rationale "Existing identity boundaries are tenant scoped"
```

Structured worker result decisions are also persisted when valid.

Decisions belong in durable state rather than only in chat/log text because they are useful context for later fresh Attempts.

## Pull-request records

`praector collection pr` writes or refreshes `collection_pull_requests` after it has confirmed the GitHub PR identity. Rerunning against an open PR updates the same durable row with the latest source commit/state; it never creates a second provider record for the same Collection. `COLLECTION_PR_RECORDED` and `COLLECTION_PR_UPDATED` events provide audit breadcrumbs.

A PR record does not change `collections.status`. Human feedback can still reopen an unaccepted Collection through `collection revise`; after that revision returns to `COMPLETE`, the same integration branch is pushed to the existing open PR. Closed/merged PRs with subsequently mutated Collection source are rejected rather than silently reused.

## Acceptance records

Collection execution state remains `COMPLETE` after acceptance. Acceptance is separate durable data.

Each `collection_acceptances` row records:

- Project ID;
- Collection ID;
- target branch;
- source Collection branch;
- exact source commit;
- target commit before acceptance;
- resulting accepted commit;
- merge mode (`ff-only`, `no-ff`, or `already-contained`);
- actor;
- timestamp.

There is one acceptance record per Collection/target-branch pair.

This gives a durable answer to:

- Was this completed work accepted?
- Into which branch?
- From which exact candidate commit?
- What commit resulted?
- Was it fast-forwarded or merged?
- Who accepted it?

## Bootstrap invariant repair

The store performs limited invariant repair during bootstrap. This includes correcting historical Attempt rows that were previously marked `SUCCEEDED` despite non-zero exit codes.

The goal is to repair clearly invalid local metadata, not to guess at ambiguous business/graph state.

## Full local reset for development/testing

There is no current `praector reset --all` command. A destructive development reset is manual.

After stopping Praector/OpenCode worker processes and ensuring no valuable unmerged work remains:

```bash
rm -rf "$PRAECTOR_HOME"
rm -rf .praector
git worktree prune
```

Old `praector/*` branches may then be reviewed/deleted manually.

Do **not** use this as a production cleanup workflow. Prefer `praector collection cleanup` for completed Collections because it preserves durable history and enforces acceptance-aware branch deletion.

### Atomic task publication

A requested task is not scheduler-visible until its initial graph contract is durable. Praector writes the task row, initial `depends_on` edges, `spawned_by` edge, blocking child-to-parent edge, and parent `WAITING` transition under one state lock and semantic commit. This prevents a concurrent scheduler from claiming a task in the gap between task creation and dependency attachment. Coordinator-requested `plan_review` is always a blocking handoff and automatically carries the latest non-superseded critic as a dependency when one exists.


### Claim-time write-gate recovery

Older or racing graph states may temporarily show a `READY` implementation before its required plan-review edge is attached. This is no longer sufficient to start work: `claim_task()` re-evaluates the deterministic planning gate and refuses the claim. Reconciliation can then attach/create the required review without any write session having started. A passing final blocking plan review atomically leaves a complete review gate and finalizes its waiting coordinator, avoiding a post-review planner wake-up that could stale the approved graph.

### Critic freshness recovery

`CRITIC_PLAN_BASELINE` events durably record the material-plan fingerprint associated with a critic. The first baseline is written when the initial critic-informed plan reaches final plan-review handoff; revalidation critics receive a baseline for the changed plan they are inspecting. If current material state no longer matches the latest critic baseline, Praector records `CRITIC_REVALIDATION_REQUIRED`, blocks plan-review/write claims, runs a fresh critic, and creates fresh coordinator incorporation work. This state is recoverable after scheduler restart because the baseline and graph are durable, not session-local.

Recovery preserves this invariant. Workspace preparation no longer retroactively turns an unset collection/isolated `base_commit` into critic-relevant plan material, and legacy raw-base seals are compatibility-recognized. A genuinely stale required critic prevents terminal `COMPLETE`, and `praector collection accept` independently refuses acceptance until critic revalidation is fresh.


### Critic baseline provenance repair

Final coordinator-to-plan-review handoff now publishes the plan-review task, initial dependencies, implementation gate edges, caller `WAITING` transition, and `CRITIC_PLAN_BASELINE` event under one Dolt lock and semantic commit. This makes baseline provenance transactionally coherent with the graph it describes.

Baseline sources have distinct meanings:

- `initial_plan_seal`: normal first coordinator-authored final plan handoff;
- `initial_policy_plan_seal`: deterministic policy created the initial final review after planning terminated without one;
- `revision_plan_seal`: a human acceptance-revision coordinator sealed a fresh critic-informed repair plan;
- `material_revalidation`: Praector automatically created a critic because a previously sealed material plan changed;
- `compatibility_plan_seal`: migration backstop for older graphs that genuinely predate baseline metadata.

Older releases could briefly publish a new plan review before writing its baseline event, allowing reconciliation to mislabel fresh revision work as compatibility state. Bootstrap now performs a narrow append-only repair only when the lineage is provable: both the critic and plan review were requested by the coordinator recorded on a Collection revision. The original event is retained and a corrected `revision_plan_seal` event is appended. Genuine legacy compatibility seals are unchanged.

The stale-plan safety net is also regression-tested independently of agent behavior: after a sealed critic, a direct material contract mutation with no proactive critic request must cause deterministic reconciliation to create a fresh critic and an `Apply critic revalidation` coordinator before plan review or implementation can proceed.


## Single-flight final-review recovery

Material-plan revalidation may create fresh critic and coordinator work while an older coordinator is already `WAITING` on final review. Waiting coordinators are terminal handoff waiters, not active graph shapers, so reconciliation never adds them as prerequisites of the review they await.

If a revalidation/repair coordinator requests another final review while a non-failed nonterminal `plan_review` already exists, Praector reuses the existing gate. The store atomically attaches new evidence prerequisites, the review-to-caller blocking edge, and review-to-implementation gates. When the review already depends on that active coordinator, the same transaction first removes the coordinator-to-review prerequisite and flips it into the blocking handoff direction. This keeps recovery at one final review generation and avoids repeated supersession/revalidation loops.

## Legacy cyclic graph recovery

The current store rejects every new cyclic `depends_on` mutation, but persistent state may contain cycles written by older Praector releases. Bootstrap and per-Collection reconciliation therefore run a legacy normalization pass before any new graph rewiring. Only protocol-invalid review prerequisites are auto-removed: implementation execution cannot be a prerequisite of pre-write plan review, and a plan review cannot wait on a coordinator that is already downstream of that review. Unknown cycle shapes remain untouched and place only that Collection into `BLOCKED` quarantine. Quarantine is durable through `DEPENDENCY_CYCLE_QUARANTINED`; the global daemon continues scheduling unrelated Collections and never schedules runnable tasks from the quarantined Collection.

## Evidence and input-lineage recovery

Fresh research is durable task/result state. Its result digest participates in material planning fingerprints, so evidence refreshes can trigger the existing critic/plan-review revalidation machinery. `RESEARCH_REQUIRED`, `RESEARCH_CAPABILITY_REQUIRED`, `TASK_INPUT_LINEAGE`, and `RECONCILIATION_REQUIRED` events make uncertainty/capability/lineage transitions inspectable after daemon restart.

Praector does not discard implementation merely because its inputs changed while it was running. Integration is preserved and the independent review is reframed as reconciliation against the current material plan, research, and prerequisite lineage.
