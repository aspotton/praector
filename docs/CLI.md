# CLI reference

[Documentation index](README.md) · [Operations](OPERATIONS.md) · [Agent protocol](AGENT_PROTOCOL.md)

This document reflects the current CLI surface in `internal/praector/cli.go`.

For exact parser behavior on an installed version, `praector <command> --help` is authoritative.

## Global

```bash
praector --version
praector <command> --help
```

Project-aware commands resolve context in this order where applicable:

1. explicit `--project`/ID;
2. `PRAECTOR_PROJECT_ID` for managed worker context;
3. nearest ancestor `.praector/project.toml` marker.

Global state defaults to `~/.praector`; override with `PRAECTOR_HOME`.

## Initialize

```bash
praector init [--name NAME] [--base-branch BRANCH] [--opencode PATH_OR_NAME]
```

Registers the current Git repository as a Project and writes `.praector/project.toml`.

Example:

```bash
praector init --opencode /home/user/bin/opencode
```

## Run an objective

```bash
praector run OBJECTIVE [--title TITLE] [--project PROJECT] [--detach] [--max-workers N]
```

Attached mode runs until the Collection becomes `COMPLETE` or `BLOCKED`.

Detached mode only queues the Collection:

```bash
praector run --detach "Add enterprise SSO"
praector daemon
```

Before creating durable work, `praector run` preflights Git, Dolt, Praector itself, the configured OpenCode command, and repository validity.

## Status

```bash
praector status [TARGET] [--project PROJECT]
```

`TARGET` may be a Collection or Task ID. With no target, shows the current Project and its Collections.

Examples:

```bash
praector status
praector status COL-abc123
praector status T-def456
```

## Graph

```bash
praector graph [COLLECTION] [--project PROJECT]
```

Displays Tasks and dependency relationships for a Collection. With no Collection, defaults to the latest Collection in project context.

## Scheduler / daemon

```bash
praector daemon [--once] [--max-workers N]

An unrecognized dependency cycle in upgraded state does not terminate the global daemon. That Collection is marked `BLOCKED` and quarantined from scheduling while other Collections continue. Recognized historical plan-review cycles are repaired automatically before reconciliation.
```

Only one scheduler owner may execute at a time.

`--once` reconciles/runs available work until no runnable work remains, then exits.

## Doctor

```bash
praector doctor
```

Checks:

- `git`;
- `dolt`;
- `opencode` on PATH;
- Dolt state bootstrap;
- project marker/registration when present;
- configured project OpenCode command.

## Logs

```bash
praector logs TARGET [--tail N]
```

`TARGET` may be a Task or Collection ID.

Examples:

```bash
praector logs T-abc --tail 120
praector logs COL-abc --tail 50
```

Attempt log paths are stored before OpenCode launches, so `--tail` can inspect a running Attempt.

## Collection lifecycle

### Show

```bash
praector collection show COL-ID
```

Prints Collection JSON including Tasks, planning policy, revision history, acceptance history, and `critic_revalidation`. The revalidation block shows the latest critic, its sealed material-plan fingerprint, the current material fingerprint, baseline source, compatibility mode when applicable, and whether the critic is currently stale. Baseline sources distinguish `initial_plan_seal`, `initial_policy_plan_seal`, `revision_plan_seal`, `material_revalidation`, and legacy-only `compatibility_plan_seal`. A final plan-review task and its critic seal are published atomically, so fresh revision work cannot be mislabeled as compatibility state.

### Revise

```bash
praector collection revise COL-ID \
  --reason "Concrete acceptance defect or required correction" \
  [--detach] \
  [--max-workers N]
```

Reopens a `COMPLETE`, **unaccepted** Collection after operator/human acceptance finds a real defect. Praector creates a durable `OPEN` revision and an `Acceptance revision` coordinator task. The revision reason is mandatory acceptance input, not a suggestion.

The Collection cannot become `COMPLETE` again until at least one post-revision implementation is completed and receives a fresh independent `review` with `verdict=pass`. If the coordinator fails to create repair work, the open revision keeps the Collection blocked. Accepted Collections cannot be revised in place; start a new Collection for follow-up changes.

Example:

```bash
praector collection revise COL-abc \
  --reason "Rate-limit prune/check/append is not atomic under concurrent requests; synchronize it and add a concurrency regression test."
```

### Diff

```bash
praector collection diff COL-ID [--target BRANCH] [--stat]
```

Compares the Collection integration branch to the target branch, defaulting to the Project base branch.

Examples:

```bash
praector collection diff COL-abc --stat
praector collection diff COL-abc --target develop
```

### Pull request

```bash
praector collection pr COL-ID \
  [--draft] \
  [--title TITLE] \
  [--base BRANCH] \
  [--remote REMOTE] \
  [--dry-run] \
  [--refresh-copy] \
  [--open]
```

Publishes a reviewed `COMPLETE` Collection integration branch as a GitHub pull request using the authenticated `gh` CLI. The command applies the same acceptance-grade validation used by direct acceptance: every completed implementation must have a completed independent review with `verdict=pass`, and required critic freshness must be current.

Defaults:

- head = the Collection integration branch;
- base = Project base branch, or the base already recorded for an existing Collection PR;
- remote = configured base-branch remote, then `origin`, then the sole remaining remote;
- title = the single completed implementation title when there is exactly one, otherwise the Collection title;
- body = deterministic summary/changes/verification/unresolved sections assembled from durable Task results; implementation `pr_summary`/`changes` are preferred, with a findings/evidence fallback for older Collections.

Idempotency:

- an existing recorded PR is reused;
- an existing GitHub PR with the same head branch is adopted and recorded;
- a completed revision pushes the same Collection branch and therefore updates the existing open PR;
- a closed/merged PR is never silently replaced with a duplicate;
- a directly accepted or already-contained candidate cannot create a redundant new PR.

`--dry-run` performs no push and no GitHub/Dolt mutation. `--draft`, `--title`, and `--base` are creation-time semantics; reruns preserve the existing PR contract. Existing open PR bodies are not overwritten by default; `--refresh-copy` explicitly replaces the body with the current deterministic Praector-generated copy. `--open` opens the resulting PR in the browser.

PR delivery does **not** call `praector collection accept`; Collection execution state, PR delivery state, and direct acceptance records remain distinct.

### Accept

```bash
praector collection accept COL-ID \
  [--target BRANCH] \
  [--no-ff] \
  [--dry-run] \
  [--cleanup] \
  [--delete-branches]
```

Default behavior is fast-forward-only.

Recommended workflow:

```bash
praector collection diff COL-abc --stat
praector collection accept COL-abc --dry-run
praector collection accept COL-abc
```

Use `--no-ff` only after intentionally reviewing a diverged target. `--delete-branches` requires `--cleanup`.

Acceptance validation requires a `COMPLETE` Collection and explicit passing independent review for completed implementation work.

See [WORKSPACES.md](WORKSPACES.md#acceptance).

### Cleanup

```bash
praector collection cleanup COL-ID [--delete-branches] [--force]
```

Default behavior removes Praector worktrees for a completed, accepted Collection.

- `--force` allows worktree cleanup before acceptance.
- branch deletion still requires an acceptance record and only deletes branches whose tips are reachable from the accepted target.

## Project

### List

```bash
praector project list
```

### Show

```bash
praector project show [--project PROJECT]
```

## Profile

Inspect the built-in runtime profile contracts that are injected into every managed Attempt:

```bash
praector profile list
praector profile show coordinator
praector profile show critic
praector profile show plan_review
praector profile show advise
```

`profile list` shows capability mode (read-only/write) and the human-facing role title. `profile show` prints the mandatory embedded prompt from `internal/assets/prompts/<profile>.md`. See [PROFILES.md](PROFILES.md).

## Task

### Show

```bash
praector task show T-ID
```

Prints durable Task JSON, including decoded result/dependencies where applicable.

### List

```bash
praector task list [--collection COL-ID] [--project PROJECT]
```

Defaults to the latest Collection in context.

### Retry a failed task

```bash
praector task retry T-ID [--reason TEXT] [--detach] [--max-workers N]
```

Creates a fresh replacement Task for a `FAILED` task instead of erasing/reusing its exhausted Attempt history. The failed Task is marked `SUPERSEDED`, hard dependents are rewired to the replacement, prerequisite dependencies are inherited, and the Collection returns to `RUNNING`. By default Praector resumes the Collection immediately; use `--detach` to queue the replacement for an existing daemon.

This is the preferred recovery path for transient harness/provider failures after the normal attempt budget has been exhausted.

### Request

```bash
praector task request \
  [--task CALLER] \
  --profile critic|plan_review|research|implement|review|advise \
  --title TITLE \
  --objective OBJECTIVE \
  --reason REASON \
  [--blocking] \
  [--depends-on T-ID ...] \
  [--workspace auto|inherit|collection|isolated]
```

Managed workers normally omit `--task`; `PRAECTOR_TASK_ID` identifies the caller.

See [AGENT_PROTOCOL.md](AGENT_PROTOCOL.md#request-durable-work).

`--depends-on` accepts only exact IDs of durable tasks that already exist in the same Collection. Praector rejects missing or cross-Collection task IDs before publishing the requested task, so symbolic placeholders such as invented `T-*` names must never be used.

### Add dependency

Coordinator-only graph mutation:

```bash
praector task add-dependency T-TASK --on T-PREREQ [--caller T-COORDINATOR]
```

### Supersede

Coordinator-only replacement/rewire:

```bash
praector task supersede T-OLD \
  --with T-REPLACEMENT \
  --reason REASON \
  [--caller T-COORDINATOR]
```

## Context

```bash
praector context show [--task T-ID]
```

Returns durable Project/Collection/Task/dependency/memory/decision context. Managed workers normally use the current `PRAECTOR_TASK_ID`.

## Coordinator escalation

```bash
praector coordinator attention \
  [--task T-ID] \
  --reason REASON \
  [--blocking]
```

Workers use this when the graph/plan itself needs reconsideration.

## Result

### Show

```bash
praector result show [T-ID]
```

With no ID inside a managed worker, defaults to the current `PRAECTOR_TASK_ID`.

### Submit

```bash
praector result submit \
  [--task T-ID] \
  [--stdin | --file PATH | --summary TEXT] \
  [--status complete|waiting|failed]
```

Structured stdin is preferred:

```bash
cat result.json | praector result submit --stdin
```

Review completion requires `verdict: pass|fail` in the JSON payload.

## Memory

### Add

```bash
praector memory add \
  [--project PROJECT] \
  [--collection COL-ID] \
  [--task T-ID] \
  [--scope project|collection] \
  [--state candidate|accepted] \
  [--kind KIND] \
  --text TEXT
```

Defaults: `scope=collection`, `state=candidate`, `kind=finding`.

### List

```bash
praector memory list \
  [--project PROJECT] \
  [--collection COL-ID] \
  [--state candidate|accepted]
```

Default state is `accepted`.

## Decision

```bash
praector decision add \
  [--project PROJECT] \
  [--collection COL-ID] \
  [--task T-ID] \
  --statement TEXT \
  [--rationale TEXT]
```

## Managed worker environment

Praector supplies environment variables used by the CLI to identify current context. The exact set is internal implementation detail, but worker-facing commands are designed to work without manually supplying Project/Collection/Task IDs in the common case.

Do not build external integrations around undocumented internal environment-variable names; prefer explicit CLI IDs when invoking Praector outside a managed worker.

### Delivery policy visibility

`praector collection show COL-...` includes both `planning_policy` and `delivery_policy`. Direct mutation requests are marked `implementation_required: true`; they cannot reach `COMPLETE` without completed implementation and passing review. A genuinely stale required critic also blocks `COMPLETE` and `praector collection accept`; workspace-only base-SHA bookkeeping does not count as material plan change.


Coordinator-requested `plan_review` is always treated as blocking, even if the caller omits `--blocking`; the latest active critic is automatically attached as a prerequisite when present. The same atomic mutation attaches that plan review as a prerequisite of all existing nonterminal implementation tasks. Final plan review is single-flight: if a non-failed nonterminal `plan_review` already exists, the request reuses it and atomically attaches new prerequisites/caller/implementation gates instead of creating another review generation. A blocking caller is never retained as a prerequisite of that same review; if the review currently waits on that caller, the handoff atomically flips the edge direction. Required write work is additionally protected at claim time. The deterministic policy backstop defers automatic `Policy plan review` creation while any coordinator remains active, preventing duplicate plan-review tasks for the same graph. If the coordinator terminates without creating the required review, policy creates the fallback afterward. A required critic is sealed to the material plan at first final review handoff; later material contract/dependency changes stale it and force fresh critic plus coordinator incorporation before plan review or implementation can be claimed. The seal is written atomically with coordinator-created plan-review publication, and human revision handoffs are labeled `revision_plan_seal` rather than upgrade compatibility.
