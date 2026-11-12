# Praector

**Durable, policy-gated orchestration and delegated command for autonomous agents.**

Praector moves multi-agent work out of one long-lived model session and into a persistent task graph. Agents do not own child-agent processes. They request durable work; Praector stores that work in Dolt, launches disposable agent sessions through a harness, isolates code changes in Git worktrees, integrates reviewed results into a Collection branch, and keeps final acceptance explicit.

Praector is intentionally positioned as a **delegated command layer below human or executive authority**. Agents can plan, research, implement, review, retry, and recover within policy; decisions that exceed delegated authority are intended to rise through a future durable human/exec escalation path. See [`ROADMAP.md`](ROADMAP.md).

Release history is maintained in [`CHANGELOG.md`](CHANGELOG.md). The installed package version is available with `praector --version`.

## Project status

Praector is pre-1.0 but ready for general internal/day-to-day use with normal Git review and backup practices. The native Go runtime has been exercised on **Linux and macOS**. Linux and macOS are the currently tested runtime platforms. Windows binaries are cross-built today; full Windows runtime validation and support are near-term roadmap work and are expected soon.

The current implementation uses **OpenCode** as its first harness, but the durable task/review/state model is intentionally harness-independent.

## Core model

```text
Project
└── Collection / objective
    ├── Task
    │   ├── Attempt
    │   │   └── disposable harness session
    │   └── delegated child tasks
    └── dynamically evolving dependency graph
```

The key rule is:

> **Agents request durable work; they do not spawn or own background agents.**

`Task != Attempt != Session`. A Task survives retries, harness exits, scheduler restarts, and fresh sessions.

Praector currently provides:

- **OpenCode** as the first harness implementation;
- Git repositories as project workspaces;
- coordinator, explore, critic, plan-review, research, implement, review, and advise profiles;
- deterministic `explore -> critic` preflight for local implementation work, upgraded to parallel `explore + fresh research -> critic` when external/current evidence is material, plus plan-review gates for non-trivial/security-sensitive/stateful/migration/destructive/integration work;
- bounded local parallelism with durable crash/retry recovery;
- orchestration state/history in **Dolt**, with `depends_on` maintained as an acyclic DAG;
- isolated Git branches/worktrees, with human-readable Praector-generated commit subjects and durable provenance trailers;
- independent implementation review, semantic repair, and fresh post-repair review lineage;
- first-class GitHub pull-request publication of reviewed Collection integration branches through `gh`;
- explicit human/policy acceptance into the base branch;
- durable revisions when acceptance finds a defect before merge.

An opt-in, **loopback-only** MCP server is available: when enabled, a running `praector daemon` hosts a local MCP listener exposing the **same command/surface coverage as the CLI** (semantic surface parity, not unrestricted behavioral parity) — read-only tools, pass-through queue writes, and readable resources, with privileged write tools (project init/registration, which registers the requested target; `collection pr`/`accept`, which return validation results or plans; and `collection cleanup`, which performs real worktree/branch removal — none of which themselves push, merge, or finalize acceptance into the base branch) behind a separate default-off opt-in (see [`docs/OPERATIONS.md`](docs/OPERATIONS.md)). The listener advertises only the MCP protocol revision it implements (meeting an unsupported request with a counter-offer advertising that revision), requires a matching protocol-version header, and validates each request's loopback Host/Origin before dispatch, rejecting a foreign or opaque Origin (DNS-rebinding defense); MCP operations never fall back to the daemon working directory, and a collection-only context resolves its owning project through a fail-closed store-backed resolver (see [`docs/OPERATIONS.md`](docs/OPERATIONS.md)). Durable human/exec escalation, stronger platform validation, distributed execution, remote/ authenticated MCP access, and broader control-plane integrations are tracked in [`ROADMAP.md`](ROADMAP.md).

## Quick start

Requirements at runtime:

- Linux or macOS (tested); Windows builds are available but runtime validation/support is still being completed
- Git
- Dolt 2.x
- OpenCode
- GitHub CLI (`gh`) only when using `praector collection pr`

The primary runtime is a native Go executable; Python is not required to run Praector. To build the current host executable from this source tree, install Go and run:

```bash
make build
./dist/praector --version      # Linux/macOS
# .\dist\praector.exe --version   # Windows
```

Install it somewhere on `PATH`, for example on Linux/macOS:

```bash
make install
praector --version
praector doctor
```

`make build-all` cross-builds the Linux, macOS, and Windows amd64/arm64 release matrix. The Python implementation remains in-tree temporarily as a reference implementation and regression oracle while the native Go runtime is hardened.

Initialize an existing Git repository:

```bash
cd ~/src/my-project
praector init --opencode /path/to/opencode
```

Run an objective:

```bash
praector run "Add an authenticated health endpoint with tests and documentation"
```

Inspect it:

```bash
praector status
praector graph COL-xxxxxxxxxx
praector logs COL-xxxxxxxxxx --tail 100
```

Inspect runtime roles/prompts:

```bash
praector profile list
praector profile show coordinator
praector profile show explore
praector profile show critic
praector profile show plan_review
praector profile show advise
```

See [`docs/PROFILES.md`](docs/PROFILES.md) for the role map, inherited global engineering contract, and prompt composition.


A successful implementation normally evolves like this:

```text
                 explore
                /       \
objective ------           --> critic --> coordinator
                \       /
                 research       (when fresh external evidence is required)
                                  ↓
                             plan_review   (when policy requires it)
                                  ↓
                              implement
                                  ↓
                         independent review
                                  ↓
                         Collection COMPLETE
```

Every implementation Collection receives deterministic read-only repository exploration before coordinator planning. Purely local work follows `explore -> critic`; when correctness depends on a current protocol/standard/provider/regulatory/dependency contract or explicit freshness, Praector also injects fresh external `research` in parallel with exploration and makes the critic consume both. Structured research provenance and result digests become durable planning evidence, and materially refreshed evidence can stale prior planning. Specialists that discover a decisive external uncertainty may return `RESEARCH_REQUIRED` and resume after bounded fresh research instead of guessing. Clearly trivial documentation-only implementation may still skip final `plan_review`; all implementation remains subject to independent post-write review.
For non-implementation objectives whose answer itself depends on changing reality—such as current news, events, pricing, availability, or support status—the same freshness policy can inject research without adding implementation-only ceremony.

## Completed work is not automatically merged

A `COMPLETE` collection remains on its integration branch until explicitly accepted.

Inspect and preview:

```bash
praector collection show COL-xxxxxxxxxx
praector collection diff COL-xxxxxxxxxx --stat
praector collection pr COL-xxxxxxxxxx --dry-run
```

For the normal GitHub review workflow, publish the reviewed Collection branch as a pull request:

```bash
praector collection pr COL-xxxxxxxxxx
```

To refresh the generated body of an already-open Praector PR after copy rules change or after a reviewed revision:

```bash
praector collection pr COL-xxxxxxxxxx --refresh-copy
```

Praector pushes `praector/collections/<collection>` to the selected remote, creates or reuses the GitHub PR for that exact head branch, and records the PR separately from execution state. PR copy prefers the implementation result's durable `pr_summary` and reviewer-facing `changes` bullets; older results without those fields are summarized from their durable findings/evidence. Rerunning the command after a completed `collection revise` updates the same open PR instead of creating another one.

If you intentionally use direct local acceptance instead of a PR, preview with:

```bash
praector collection accept COL-xxxxxxxxxx --dry-run
```

If human acceptance finds a real defect before merge, reopen the same durable collection instead of creating an ad-hoc side branch:

```bash
praector collection revise COL-xxxxxxxxxx --reason "Describe the concrete acceptance defect"
```

An open revision prevents the Collection from returning to `COMPLETE` until post-revision implementation work receives a fresh passing independent review. Accepted collections are immutable; follow-up work after acceptance belongs in a new Collection.

Accept into the configured base branch:

```bash
praector collection accept COL-xxxxxxxxxx
```

The default is safe **fast-forward-only** acceptance. If the target branch has independently diverged, Praector refuses rather than silently creating a merge commit. An explicit `--no-ff` is available after reviewing the divergence.

Optional cleanup:

```bash
praector collection cleanup COL-xxxxxxxxxx
praector collection cleanup COL-xxxxxxxxxx --delete-branches
```

## Documentation map

The documentation is intentionally split so both humans and coding agents can find the right level quickly.

| If you need... | Read |
| --- | --- |
| A map of all documentation | [`docs/README.md`](docs/README.md) |
| Release history and per-change notes | [`CHANGELOG.md`](CHANGELOG.md) |
| Planned hardening, MCP, escalation, and future work | [`ROADMAP.md`](ROADMAP.md) |
| System boundaries, components, and control flow | [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) |
| Runtime profile roles and their built-in prompt contracts | [`docs/PROFILES.md`](docs/PROFILES.md) |
| Task Envelopes, delegation, results | [`docs/AGENT_PROTOCOL.md`](docs/AGENT_PROTOCOL.md) |
| Exact CLI surface and examples | [`docs/CLI.md`](docs/CLI.md) |
| Git branches, worktrees, integration, review, acceptance | [`docs/WORKSPACES.md`](docs/WORKSPACES.md) |
| Dolt schema, states, events, memory, crash recovery | [`docs/STATE_AND_RECOVERY.md`](docs/STATE_AND_RECOVERY.md) |
| Installation, operating workflows, troubleshooting | [`docs/OPERATIONS.md`](docs/OPERATIONS.md) |
| Repository layout, invariants, contribution workflow | [`docs/DEVELOPMENT.md`](docs/DEVELOPMENT.md) |
| Instructions for AI/coding agents modifying this repository | [`AGENTS.md`](AGENTS.md) |

### Recommended reading paths

**Using Praector:**

1. this README;
2. [`docs/OPERATIONS.md`](docs/OPERATIONS.md);
3. [`docs/CLI.md`](docs/CLI.md);
4. [`docs/WORKSPACES.md`](docs/WORKSPACES.md) before accepting real work.

**Understanding the design:**

1. [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md);
2. [`docs/PROFILES.md`](docs/PROFILES.md);
3. [`docs/AGENT_PROTOCOL.md`](docs/AGENT_PROTOCOL.md);
4. [`docs/WORKSPACES.md`](docs/WORKSPACES.md);
5. [`docs/STATE_AND_RECOVERY.md`](docs/STATE_AND_RECOVERY.md).

**Modifying Praector with a coding agent:**

1. [`AGENTS.md`](AGENTS.md);
2. [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md);
3. [`docs/DEVELOPMENT.md`](docs/DEVELOPMENT.md);
4. the focused doc for the subsystem being changed.

## Project configuration

`praector init` creates a local marker at:

```text
.praector/project.toml
```

It identifies the project and configures the base branch, OpenCode executable, optional model-per-profile mapping, and safety/concurrency limits. The global orchestration home defaults to:

```text
~/.praector
```

Override it with:

```bash
export PRAECTOR_HOME=/some/other/path
```

See [`docs/OPERATIONS.md`](docs/OPERATIONS.md#project-configuration) for the complete configuration shape.

## Safety boundaries worth knowing

- OpenCode's native `task`/background-agent delegation is denied for Praector-managed runs.
- Read-only profiles are denied normal edit tools and are checked for Git mutations after execution.
- Workers run in Praector-managed worktrees, not in the user's original checkout.
- `git push` is denied inside managed OpenCode runs.
- Implementation results require independent review before collection acceptance.
- Review completion requires an explicit `verdict: pass|fail`.
- Collection completion and collection acceptance are separate lifecycle events.
- Sessions are disposable; correctness must never depend on resuming the same OpenCode session.
- Failed Tasks keep immutable Attempt history; `praector task retry T-...` creates a fresh durable replacement rather than erasing failed attempts.

## Development

Run the native and reference-implementation validation suites before committing:

```bash
make check
```

Useful focused targets:

```bash
make test-go          # native Go unit/integration tests
make parity           # black-box native/Python CLI contract checks
make smoke            # quick compiled-binary smoke
make smoke-state      # optional fresh-repo/store smoke (best with real Dolt)
make test-reference   # original Python regression suite
make parity           # black-box shared-contract comparison
make smoke            # exercise the compiled executable
```

Coding agents should read [`AGENTS.md`](AGENTS.md) before changing the repository.

## Current scope and extension boundaries

Praector deliberately keeps the current runtime focused: one local scheduler, one Git repository per Project, OpenCode as the first harness, explicit acceptance, and no hidden background-agent ownership. Broader features such as MCP, durable human/exec escalation, additional harnesses, remote workers, multi-repository Projects, event triggers, richer authorization, and UI surfaces are planned as extensions over the same durable invariants rather than as shortcuts around them.

See [`ROADMAP.md`](ROADMAP.md) for prioritized future work and [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md#extension-boundaries) for the architectural attachment points.

### OpenCode environment compatibility

Managed OpenCode sessions preserve the caller/provider environment but, for compatibility and project isolation, still remove an active parent Python virtualenv activation from `VIRTUAL_ENV`/`PATH` before launching a worker. The native Praector executable itself does not require Python; this prevents whatever shell/tooling environment launched Praector from accidentally becoming the project's `python`/`pip`. Praector still pins subprocess cwd, `PWD`, and OpenCode `--dir` to the task worktree. Set `PRAECTOR_SANITIZE_PARENT_VENV=0` only when an installation intentionally requires the parent venv.

### Worker control-plane isolation

Praector strips an inherited active Python virtualenv from worker `PATH` by default so project `python`/`pip` do not resolve into the operator shell's environment. A dedicated `~/.praector/control-bin/praector` shim points to/copies the native executable and is injected back into worker `PATH`, so durable commands such as `praector task request` and `praector result submit` remain available independently of the project's language runtime.

Direct mutation objectives also carry a deterministic delivery policy: a Collection such as `Add ...`, `Fix ...`, or `Update ...` cannot become `COMPLETE` after planning/critique alone. At least one completed implementation with passing independent review is required.
