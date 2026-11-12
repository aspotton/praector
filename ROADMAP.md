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

## 3. Evidence-aware orchestration quality

Praector should treat current external evidence as durable planning input when correctness depends on changing reality rather than model memory. The current baseline can require fresh research alongside repository exploration, feed both into critic and plan review, record structured source/claim provenance, and stale material planning when accepted external evidence changes.

Continue hardening this layer through real dogfood runs:

- expand deterministic freshness classification without making purely local work pay an unnecessary research cost;
- improve authoritative-source selection and contradiction handling for standards, APIs, vendor behavior, laws/regulations, security guidance, and current events;
- exercise `RESEARCH_REQUIRED` round-trips raised by critic, plan-review, review, and advise tasks;
- prefer official/reference/provider verification oracles for external contracts, while distinguishing them from implementation-owned black-box tests;
- stress prerequisite/evidence lineage changes while downstream implementation is already running, including reconciliation and fresh-review behavior;
- extend capability-aware routing beyond current external research as additional harness capabilities become relevant;
- improve operator visibility into why research was required, which evidence made a plan stale, and why reconciliation was triggered.

The design goal is traceable reasoning rather than more ceremony: authoritative evidence should be able to flow into durable claims, planner directives, implementation contracts, and independent verification without silently changing meaning along the way.

## 4. MCP interface

MCP is a first-class Praector interface for coding agents and human-facing agents. It must expose the same durable **domain operations** as the CLI rather than invoking CLI subprocesses or maintaining a second orchestration implementation. The normal user experience should be that a human talks to an MCP-capable agent, and that agent can inspect and operate Praector with the same authority/policy boundaries as an equivalent CLI caller.

**Status:** the local loopback listener is live, and the CLI/MCP surface is now
commandHelp-driven so every command maps to exactly one MCP tool or resource —
command/surface coverage parity (the surfaces cannot drift in coverage), not
unrestricted behavioral parity. Read-only tools and pass-through queue writes are
always exposed, and privileged write tools (explicit-root project
`init`/registration, `collection pr`/`accept` (which only run the durable
validation or return the plan), and `collection cleanup` (which performs real
worktree/branch removal) — none of which themselves push, merge, or finalize
acceptance into the base branch) sit behind a separate,
default-off `privileged` opt-in. The remaining work below (remote transport +
auth, richer escalation integration) stays on the roadmap.

### Shared service/operation layer

- Extract command-independent typed domain/service operations from CLI handlers.
- Make CLI and MCP thin adapters over those same operations.
- Maintain one operation registry describing name, input/output schema, mutability, authority class, help text, and audit requirements where practical.
- Add parity tests so a new operator-visible domain capability cannot silently appear in only one interface.
- Define parity semantically rather than literally: process/bootstrap commands such as starting the daemon do not become self-referential MCP tools, but their underlying inspectable/configurable domain behavior should remain available when meaningful.

### Daemon-hosted transport

When MCP is enabled, the running Praector daemon hosts the MCP endpoint. Target the modern stateless MCP model and Streamable HTTP for network clients. Default binding should be loopback-only (`127.0.0.1` / `::1`) so local coding agents can connect without exposing Praector on the LAN. Remote/LAN/Tailscale/public binding is a separate explicit security mode requiring authentication/authorization. An optional stdio bridge may be useful for clients that insist on spawning a local MCP process, but it should bridge to the same service layer rather than become a second server implementation.

### Server-initiated push: current scope and the deferred migration

Praector's current push surface (`GET /mcp` → SSE `notifications/message`, plus the durable `events` read surface and the `TASK_*`/`COLLECTION_*` status-transition events) targets **MCP 2025-06-18** explicitly, where GET-SSE + `notifications/message` + Last-Event-ID replay is the spec-supported mechanism. This is version scope, not a claim of conformance to the newest revision. The MCP **2026-07-28** revision removes the GET-SSE/Last-Event-ID mechanism in favour of a different model, so adopting it is deferred roadmap work, not current behavior:

- **`subscriptions/listen` + multi-round-trip (MRTR)**: the 2026-07-28 replacement for GET-SSE. When adopted, the durable `events` table + `(created_at, id)` cursor seam built for the current stream is the natural replay substrate; the stream-registry/level/replay state already lives in the wiring layer (`mcp_sse.go`), so a future transport can swap the delivery channel without touching the domain-agnostic `mcp/` core or the store write path.
- **`resources/subscribe` / `notifications/resources/updated` and per-entity resource URIs** (e.g. `praector://collection/COL-*`): deferred. Resources are currently one-per-command namespace and the `commandHelp`→`mcpSurface` 1:1 parity invariant in `mcp_parity_test.go` would break; making per-entity resources is a deliberate documented change, not an incidental one.
- **Server-to-client JSON-RPC requests** (sampling, roots, ping-to-client): intentionally absent; there is no plan to introduce them as part of push.

### Agent-facing state surface

Use resources for durable inspectable state and tools for parameterized queries/actions. Expose at minimum:

- project/configuration discovery;
- Collection list/show/status/graph and revision/acceptance state;
- Task show/dependencies and role/objective/status;
- attempts, sessions, summarized/tail logs, structured results, evidence, decisions, and review verdicts;
- pull-request delivery metadata;
- daemon/runtime health;
- pending attention/escalation objects once that layer exists.

Stable Praector IDs (`PRJ-*`, `COL-*`, `T-*`, attempts, revisions, decisions, escalations) are the handles agents pass between calls. MCP has no trustworthy equivalent of the CLI caller's current working directory, so project-scoped mutations must resolve context from an explicit registered `PRJ-*` (or from a durable child ID that deterministically identifies its project), never from server process cwd or arbitrary client-supplied filesystem paths. The daemon can therefore serve multiple registered projects under one Praector home without ambiguous write targets.

### Agent-facing actions

Agents with appropriate authority should be able to perform the same durable work a human can initiate through the CLI, including:

- start a new Collection from a natural-language objective;
- revise/retry/supersede work where the equivalent CLI operation exists;
- request specialist Tasks where authorized;
- publish/update PR delivery;
- inspect blocked/failed work and its evidence;
- respond to durable human/exec attention requests when the caller has the required authority;
- perform acceptance/other privileged actions only under an explicit stronger authority scope.

Every MCP mutation must call the same service/domain operation as CLI, emit the same durable events, and pass the same dependency/planning/review/capability/acceptance checks. MCP must never mutate Dolt tables directly.

### Long-running work and MCP Tasks

Praector Collections are already durable asynchronous work. A basic `run` tool can return the new `COL-*` immediately and let clients query status/resources. Where client support makes it useful, integrate the official `io.modelcontextprotocol/tasks` extension so long-running tool calls can return a durable MCP task handle, support polling, and represent mid-flight input requirements. The MCP task is an interface handle; the Praector Collection/Task graph remains the source of truth.

### Human attention through agents

A primary target workflow is:

```text
Human -> coding/general agent -> Praector MCP -> durable Praector graph
                                      |
                                      +-> status / evidence / logs
                                      +-> start or revise work
                                      +-> pending attention
                                              |
Human <- agent presents decision/evidence <----+
Human -> agent records authorized response -> Praector resumes graph
```

This should allow a person to ask an agent “what is Praector doing?”, “start this implementation”, or “what needs my attention?” without opening a shell. Attention responses must still be recorded as durable Praector decisions with responder/authority provenance; conversational prose alone is never approval.

### Authorization and exposure

Before exposing Praector beyond trusted loopback clients:

- define authenticated caller identity and propagate it into audit/event records;
- scope capabilities by project and operation class (`read`, `work`, `delivery`, `accept`, `respond_human`, `respond_exec`);
- separate normal agent authority from human/executive approval authority;
- protect repository logs/results and other potentially sensitive data;
- bound/rate-limit expensive reads and work-creating mutations;
- support explicit remote-server configuration rather than changing the safe local default.

## 5. Human and executive escalation

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

## 6. Broader control-plane evolution

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
