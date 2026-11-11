# Praector roadmap

Praector is usable today as a local, durable control plane for delegated agent work. The roadmap focuses on making that core safer to operate broadly, exposing it through MCP, and adding a durable human/executive escalation layer without weakening the existing task, review, recovery, and acceptance invariants.

This roadmap describes direction, not guaranteed release dates. Items move only after they have durable tests and, where appropriate, real black-box validation.

## 1. Public-repository and general-use hardening

### Release and repository readiness

- Publish the renamed Praector repository with reproducible release artifacts and checksums.
- Add GitHub Actions for Go tests/vet, Python reference tests, parity/smoke checks, and Linux/macOS/Windows build artifacts.
- Keep `CHANGELOG.md` as the release history and require it to be updated for every behavioral, code, prompt, workflow, or documentation change.
- Add contributor/release guidance as outside contributors begin using the project.

### Real-world dogfooding

- Use Praector to implement and review ordinary changes to Praector itself.
- Prefer real repositories and realistic objectives over synthetic endpoint tests for continuing validation.
- Capture operator/agent UX problems that are technically correct but difficult to understand, then improve observability without weakening durable semantics.

### GitHub pull-request delivery hardening

Exercise `praector collection pr` beyond the current happy-path coverage:

- create and publish a new PR;
- reuse an existing PR idempotently;
- `--refresh-copy` after reviewed revisions;
- draft/non-draft behavior;
- base/remote selection and unusual default-branch layouts;
- push/authentication failures and retry behavior;
- already-merged/closed PR handling;
- preservation of human-edited PR copy unless explicitly refreshed.

### Parallel and long-running workload testing

- Stress multiple independent Collections running concurrently.
- Exercise bounded worker saturation, read-only and write tasks, retries, and simultaneous integration branches.
- Test daemon restart/crash recovery while several Collections are active.
- Validate cleanup and stale-worktree handling after interrupted parallel work.

### Recovery and state operations

- Document and automate Dolt backup/export/restore for Praector state.
- Add a safe diagnostic/export command suitable for support and disaster recovery.
- Validate restore onto a fresh machine/repository checkout.
- Define retention/cleanup rules for old logs, attempts, sessions, worktrees, and accepted Collections.
- Resolve the project-identity edge case where a recreated repository path can conflict with an older durable Project registration.

### Remaining black-box acceptance tests

- Naturally exercise semantic implementation failure -> failed review -> repair -> **fresh durable post-repair review Task** in a real run. Automated regression coverage already exists; the goal is live end-to-end evidence without intentionally instructing an agent to violate acceptance requirements.
- Continue crash/restart tests around narrow task/session handoff boundaries.

## 2. Platform support

Praector has been exercised on **Linux and macOS**. The release build already targets Linux, macOS, and Windows on amd64/arm64.

Near-term platform work:

- run the complete native runtime/daemon/worktree/crash-recovery suite on Windows;
- validate Windows process-tree termination, file locking, paths, Git worktrees, Dolt, OpenCode invocation, and cleanup behavior;
- publish Windows-specific installation/troubleshooting notes;
- maintain Linux/macOS regression coverage while Windows reaches the same supported-runtime bar.

The goal is full Windows support, not a permanently experimental port.

## 3. MCP interface

MCP should expose the same durable control plane that the CLI uses, not create a second orchestration implementation.

### Architecture principles

- CLI and MCP share the same store/service/domain operations.
- MCP must never bypass dependency, planning, review, capability, revision, or acceptance checks.
- Start read-only and expand mutation capabilities deliberately.
- Preserve durable IDs (`PRJ-*`, `COL-*`, `T-*`, attempts, decisions, revisions) across CLI and MCP.
- Keep local stdio operation simple; design remote transport/auth as a separate security boundary.

### Read-only MCP surface first

Expose agent-friendly equivalents for the information operators already inspect through the CLI:

- projects and project configuration;
- Collection list/show/status/graph;
- Task show and dependency context;
- attempts/sessions and summarized logs;
- structured results, review verdicts, evidence, decisions, revisions, and acceptance state;
- durable memory/context records;
- pull-request delivery metadata;
- pending human/exec escalations once that layer exists.

Prefer MCP resources for durable inspectable state and MCP tools for queries/actions that require parameters or side effects.

### Controlled write surface

After read-only parity is stable, consider tools for:

- create/run a Collection;
- request durable specialist Tasks;
- submit structured results from authorized external workers;
- retry/supersede/revise work;
- publish/update PRs;
- respond to human/exec escalation requests;
- explicitly privileged acceptance actions.

Every mutating MCP operation must use the same policy checks and audit/event history as the CLI.

### Remote access and authorization

Before exposing Praector beyond a trusted local process:

- define authentication and identity propagation;
- scope capabilities by project/action;
- protect logs/results that may contain sensitive repository information;
- add audit events for remote reads/writes where appropriate;
- rate-limit and bound expensive queries/actions;
- separate ordinary agent authority from human/executive approval authority.

## 4. Human and executive escalation

Praector should remain the delegated command layer, with humans or an executive agent above it for choices that exceed delegated authority.

### Durable escalation model

Add first-class durable states such as:

- `AWAITING_HUMAN`
- `AWAITING_EXEC`

An escalation should be a durable object, not a suspended chat session. It should contain:

- who raised it and from which Collection/Task;
- concise question/decision required;
- why autonomous resolution is unsafe or unauthorized;
- evidence and relevant durable references;
- bounded options/recommendation when useful;
- impact of each choice;
- required authority level;
- final response, responder identity, timestamp, and rationale.

Dependent work remains blocked until the durable response is recorded.

### Operator surfaces

The same escalation should be visible through both CLI and MCP, for example conceptually:

```text
praector escalation pending
praector escalation show ESC-...
praector escalation respond ESC-... --decision ...
```

Exact command names remain a design decision. The key invariant is that CLI and MCP are two interfaces to one durable approval object.

### Executive pathway

MCP makes this especially useful: an executive/human-facing application can subscribe to or query pending escalations, present evidence in a higher-level workflow, record the authorized response, and let Praector resume the blocked graph deterministically.

No worker may infer approval from prose or bypass the durable escalation record.

## 5. Broader control-plane evolution

Later extensions, after the local core and MCP boundary are solid:

- additional agent harnesses behind the existing harness interface;
- remote/distributed workers with durable leasing and ownership;
- multi-repository Projects/workspaces;
- event/schedule/watch triggers that create or wake Collections;
- richer memory/search while keeping durable source-of-truth boundaries clear;
- multi-user identity, RBAC, and approval policy;
- web/desktop operator UI over the same CLI/MCP/domain model;
- GitHub issue/project synchronization without making GitHub the orchestration database;
- policy-defined authority tiers for autonomous, human, and executive actions.

## Guiding rule

Praector starts from a clean public namespace: new features should extend the canonical `praector` / `.praector` / `PRAECTOR_*` surface rather than carrying transitional aliases.

New interfaces and automation must extend Praector's deterministic boundaries, not weaken them. Durable task identity, append-only attempt evidence, explicit dependencies, capability separation, independent review, crash recovery, and explicit acceptance remain the foundation as the system grows.
