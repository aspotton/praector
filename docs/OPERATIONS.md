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

The Python package remains in the repository only as a reference implementation and regression oracle; normal Praector operation should use the native binary.

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
implicitly accept. An MCP `init` call requires an explicit target `root_path` and
never operates on the daemon process working directory. Direct acceptance and
external PR delivery remain
operator-side actions.

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

A task with an unresolved `depends_on` edge cannot become `COMPLETE`, even if its model submits a structured `status=complete` result after requesting blocking work. Praector defers completion to `WAITING` and records `TASK_COMPLETION_DEFERRED`; a fresh attempt is released only after all hard dependencies are `COMPLETE`. Delivery-policy repair is a terminal backstop and does not start while coordinator/critic/plan-review/research/advise work remains nonterminal.

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
