# Performance and query-cost tracker

This document is a durable engineering tracker for Praector performance work. It records baselines, evidence, decisions, optimization phases, and measured results as iterative work lands.

It is not release-notes prose: [`../CHANGELOG.md`](../CHANGELOG.md) remains the per-change history. This document should be updated whenever a performance baseline is captured or a query-cost optimization is designed, landed, benchmarked, or deliberately deferred.

## Current status

- Initial performance investigation: **complete**.
- Code optimization: **Phase 1, Phase 2, Phase 3, and Phase 3.5 landed** (multi-query seam + CLI bootstrap read batching; then collection-snapshot reconciliation for `status`; then a 30s idle acceptance-readiness sweep cooldown; then the acceptance predicate's ancestry walk moved onto already-loaded rows, so one sweep of one Collection is one batched read). Phase 4 not yet landed.
- Primary affected surface: `praector status`, CLI startup/bootstrap, terminal-state reconciliation, and idle daemon observability sweeps.
- Primary conclusion: the dominant cost is **Dolt subprocess churn and repeated loading of the same state**, not slow SQL over the current dataset size.
- Recommended order:
  1. batch logical reads through a JSONL-aware multi-query helper — **landed** (`Store.QueryMany` / `queryManyLocked`);
  2. batch CLI bootstrap invariant scans — **landed** (`Store.loadInvariantRowsLocked` drives one batched read across both bootstrap repairs);
   3. use one per-collection snapshot across status/terminal reconciliation — **landed** (`Store.loadCollectionRows` and the shared decision cores);
   4. rate-limit idle COMPLETE-collection readiness sweeps — **landed** (`Scheduler.readySwept`, 30s per collection);
   5. evaluate the acceptance predicate, including its ancestor walks, from already-loaded rows — **landed** (`ancestorWalkFromRows` + `loadCollectionRowsWithAcceptance`, one read per swept collection);
   6. add targeted indexes through an idempotent migration path.

## Investigation snapshot

Date: 2026-11-14

Runtime and dataset:

| Item | Observed value |
| --- | ---: |
| Installed Praector | `praector 0.2.5` |
| Dolt | `2.7.2` |
| Projects | 6 |
| Collections | 23 |
| Tasks | 241 |
| Task edges | 603 |
| Events | 1713 |
| Attempts | 262 |
| Sessions | 262 |
| Collection acceptances | 16 |
| Collection revisions | 8 |
| Collection PR records | 3 |

Collection status at the time of the snapshot:

| Status | Collections |
| --- | ---: |
| `COMPLETE` | 22 |
| `BLOCKED` | 1 |

Task status at the time of the snapshot:

| Status | Tasks |
| --- | ---: |
| `COMPLETE` | 232 |
| `FAILED` | 4 |
| `READY` | 1 |
| `SUPERSEDED` | 3 |
| `WAITING` | 1 |

These row counts are intentionally tiny. The investigation found that the data volume cannot explain multi-second status calls.

## Measured baseline

The baseline was captured while a normal user-level `praector daemon` was active.

| Operation | Wall time | Dolt invocations |
| --- | ---: | ---: |
| One trivial `dolt sql` process | ~128 ms | 1 |
| 50 sequential trivial `dolt sql` processes | ~5.76 s | 50 |
| `praector status COL-504ca4be8f` | 8.919 s | 86 |
| `praector status --project PRJ-fe97887502` | 21.96 s | 169 |

The invocation counts include all `dolt` child processes, not only `dolt sql` queries.

Important behavior: `praector status` is not purely read-only. Its terminal-state reconciliation can update durable state and create Dolt commits. Benchmarks should therefore be interpreted as realistic operational measurements, not immutable snapshots.

### Clean benchmark vs live environment

Two distinct measurement contexts appear in this tracker and must not be conflated:

- **Live environment** — the tables in this "Measured baseline" section (the daemon-active `status` captures above) were taken against the real, populated user store with a normal `praector daemon` running.
- **Clean benchmark** — the seeded-scale numbers in the Results tracker and the "Phase 1 measured result" subsection (a scratch DB built to the investigation snapshot size: 23 collections, 241 tasks, 603 edges, 262 attempts, 1713 events) are reproducible before/after counts from the PATH-shim methodology, not live production traffic.

The 51 → 3 `dolt sql` and bootstrap-repair 49 → 2 figures in the Results tracker are **clean-benchmark** numbers; they are not a live-environment capture.

The **live post-optimization `project list` and `status` counts** are now captured directly against the real, populated user store with a normal `praector daemon` active, under the same PATH-shim methodology (see the Reproduction methodology above). They are recorded in the "Live Phase 1.1 captures" subsection of the Results tracker and are kept separate from both the seeded 23-collection clean benchmark and the original live baseline above. These are live-environment captures, not the clean-benchmark `dolt sql` counts.

## Reproduction methodology

The measurement harness used a temporary PATH shim that records every Dolt invocation and forwards it to the real binary:

```sh
mkdir -p /tmp/praector-perf
cat >/tmp/praector-perf/dolt <<'EOF'
#!/bin/sh
printf '%s\n' "$*" >> /tmp/praector-perf/queries.log
exec /usr/local/bin/dolt "$@"
EOF
chmod 755 /tmp/praector-perf/dolt
```

Example:

```sh
rm -f /tmp/praector-perf/queries.log
time env PATH="/tmp/praector-perf:$PATH" \
  praector status COL-504ca4be8f
wc -l /tmp/praector-perf/queries.log
grep -c '^sql' /tmp/praector-perf/queries.log
```

For project-wide measurements, run the same command with the project target and inspect the resulting log. The important measurements are both wall time and invocation count; a time-only benchmark hides the process-startup cost being optimized.

## Confirmed cost model

Praector currently executes each logical Dolt read by starting a new Dolt CLI process under the filesystem lock.

The relevant seam is `Store.Query()` in `internal/praector/store.go`:

- it invokes `dolt sql -r json -q <statement>`;
- it parses one JSON object containing a `rows` array;
- every public read usually enters through `withLock()` and launches its own subprocess.

At the measured ~115–130 ms per invocation, a 50-query operation costs roughly five to seven seconds before the application does useful work. This explains the measurements at a scale where all indexed lookups should otherwise be very small.

## Dolt multi-statement capability

Dolt 2.7.2 was verified to accept semicolon-separated statements in one `-q` argument.

Command:

```sh
dolt sql -r json -q \
  'SELECT 1 AS a; SELECT 2 AS b; SELECT id FROM projects WHERE 0=1'
```

Observed output:

```json
{"rows":[{"a":1}]}
{"rows":[{"b":2}]}
{}
```

Implications:

- Praector can reduce process count without embedding Dolt or introducing a SQL server.
- `QueryMany()` can use `json.Decoder` over the process output rather than expecting a single JSON document.
- An empty `{}` document represents a zero-row result and must map to empty `Rows`.
- Failure handling should identify the failing statement index.
- The existing fake-Dolt test harness currently emits one JSON object and must be extended before multi-query production code is landed.

## Confirmed call-path findings

### CLI startup performs bootstrap repair on every command

`newStore()` calls `Store.Bootstrap()` for ordinary CLI operations.

Bootstrap:

1. initializes the Dolt store when absent;
2. applies the schema;
3. commits schema state;
4. runs invariant repair.

Relevant code:

- `internal/praector/cli.go`: `newStore()`
- `internal/praector/store.go`: `Bootstrap()`
- `internal/praector/store.go`: `repairInvariantsLocked()`

Consequence: a status command pays repair scanning before it renders anything.

### Bootstrap dependency-cycle repair scans collections one at a time

Before Phase 1, `repairInvariantsLocked`'s cycle-repair step listed every collection (ordered by `created_at`) and invoked the per-collection `repairLegacyDependencyCyclesLocked` repair for each. (That global list-then-walk wrapper has since been removed; the live bootstrap path is now the batched `repairLegacyDependencyCyclesLockedAllWithRows`, and the standalone per-collection entry point is `RepairLegacyDependencyCycles`.)

For each collection it runs:

```sql
SELECT id,project_id,profile,status
FROM tasks
WHERE collection_id=...

SELECT from_task_id,to_task_id
FROM task_edges
WHERE collection_id=...
  AND relation='depends_on'
```

With 23 collections, this alone accounts for approximately 47 Dolt SQL invocations.

The existing repair logic is per collection and should remain per collection, but the reads can be loaded globally once and grouped in Go without changing repair semantics.

### Bootstrap revision-baseline repair queries task pairs repeatedly

`repairRevisionBaselineProvenanceLocked()` first scans baseline events globally. For each candidate critic/plan-review pair, it issues a separate task-pair query.

The observed log contained repeated queries shaped like:

```sql
SELECT id,collection_id,requested_by_task_id
FROM tasks
WHERE id IN ('T-...','T-...')
```

These can be collected and batched. Revision and project lookups after the task pass can also be batched or satisfied from a small snapshot.

### Project status reconciles each collection independently

`Service.Status()`:

1. resolves the project;
2. lists collections;
3. calls `UpdateCollectionTerminalState()` for every collection;
4. reloads the collection;
5. calls `collectionStatus()`.

`collectionStatus()` then loads:

- tasks;
- latest acceptance;
- latest GitHub PR;
- revisions.

The project path therefore combines:

- CLI bootstrap cost;
- one terminal-state reconciliation per collection;
- additional per-collection display reads.

That path is consistent with the measured 169 Dolt invocations.

### `UpdateCollectionTerminalState()` reloads the same snapshot repeatedly

For a non-empty collection, the terminal-state path performs operations that independently load:

- tasks;
- edges;
- revisions;
- open revisions;
- runnable tasks;
- running sessions;
- the collection;
- implementations missing passing review;
- critic revalidation state.

`RepairSupersededReviewLineage()`, `ReconcileCollectionRevisions()`, `ImplementationsMissingPassingReview()`, and `CriticRevalidationState()` each acquire their own views of the same graph.

`CriticRevalidationState()` is particularly wasteful:

- it lists tasks to find the latest critic;
- computes the current material-plan fingerprint:
  - collection
  - tasks
  - edges
- computes the legacy fingerprint:
  - collection again
  - tasks again
  - edges again
- reloads compatibility tasks/edges/collection;
- queries the latest critic baseline event.

The observed single-collection log shows the same collection, task, and edge tables being selected repeatedly before acceptance/PR/revisions are fetched.

### Idle daemon readiness sweeps are expensive

`Scheduler.Daemon()` polls with a one-second default. For every `COMPLETE` collection, it calls `observeAcceptanceReadiness()`.

That sweep:

1. fetches the collection;
2. lists tasks;
3. computes a material-plan fingerprint;
4. evaluates `acceptanceBlockReason()`;
5. may traverse directive ancestors and other acceptance validators.

Before Phase 3.5, item 5 was the expensive part: the directive/research ancestry walk launched one `dolt sql` per BFS node. It now walks the already-loaded rows.

The emitted readiness event is deduplicated by material-plan fingerprint, but deduplication prevents repeated writes, not repeated predicate evaluation. Consequently, a completely idle system can repeatedly execute expensive acceptance predicates against unchanged COMPLETE collections.

This path is an observability sweep, not a correctness gate. Phase 3 landed a 30s in-memory per-collection cooldown on it (`Scheduler.readySwept`), and Phase 3.5 moved the predicate's directive/research ancestry walk onto already-loaded rows, so one sweep of one Collection is now **one** batched `dolt sql` read plus the dedup `ListEvents`. An event-driven recheck via the store event-emit seam remains a further upgrade path but is no longer the dominant cost lever.

## Root-cause classification

### Observed

- A logical `Store.Query()` launches one Dolt process.
- CLI startup runs invariant repair on every invocation.
- Bootstrap dependency-cycle repair performs per-collection reads.
- Revision-baseline repair performs task-pair queries.
- `status` repeatedly reloads the same collection state.
- Daemon polling evaluates acceptance readiness for COMPLETE collections every second.
- Dolt 2.7.2 emits newline-delimited JSON objects for semicolon-separated queries.
- Current Dolt data volumes are extremely small.

### Inferred

- Most measured wall time is subprocess startup and process coordination overhead.
- Reducing query invocations from tens to a few should provide most of the practical improvement.
- Indexes provide real but comparatively smaller benefit against current data volumes.
- The daemon idle sweep is likely a larger continuous background cost than a single interactive status call.

### Recommended

- Batch before embedding Dolt or running a SQL server.
- Make reads snapshot-aware while preserving current repair and terminal-state semantics.
- Rate-limit observability sweeps before making them event-driven or persistent-server-backed.
- Add targeted indexes after the dominant process-count cost has been reduced.

## Optimization plan

### Phase 1 — Multi-query and bootstrap batching

State: **landed**

Adds `Store.QueryMany(queries ...string) ([]Rows, error)` and its internal
`queryManyLocked` sibling (mirroring the existing `queryLocked`/`Query` split, so the
locked variant is the only one safe to call inside Bootstrap's single `withLock`). It
runs one `dolt sql -r json` subprocess for a semicolon-joined batch, decodes the
newline-delimited JSON so document `i` maps to statement `i`, treats `{}` /
`{"rows":[]}` / blank stdout as an empty result, and reports a statement-indexed
error when a batch fails mid-way or returns a document count other than the number of
statements. `Query`/`queryLocked` behavior is unchanged; `QueryMany` is additive and
read-side only.

CLI bootstrap invariant repair (`repairInvariantsLocked` and its two sub-repairs) now
loads the inconsistent-attempt scan, the critic-baseline event scan, the collection
inventory, the task rows, and the `depends_on` dependency edges in a **single**
`queryManyLocked` process (`loadInvariantRowsLocked`) and groups tasks/edges by
collection in Go. The per-collection cycle-repair algorithm and the revision-baseline
provenance repair (including the per-pair `collection_revisions ... LIMIT 1` selection)
are byte-for-byte unchanged: the batched path feeds a preloaded-rows variant
(`repairLegacyDependencyCyclesLockedAllWithRows` / `repairRevisionBaselineProvenanceLockedAll`),
while the standalone `RepairLegacyDependencyCycles` (and the scheduler-time repair path)
keeps issuing its own `queryLocked` reads.

### Phase 2 — Collection-snapshot reconciliation

State: **landed**

`praector status` now loads each collection's rows once via a single batched
`dolt sql -r json -q` read and reconciles the status decision from that snapshot,
preserving byte-for-byte the default path's durable side effects (superseded-review-lineage
repair, revision reconciliation, `SetCollectionStatus` flips and commits, and every
event). This is a read-path refactor, not a behavior change.

- Batched loader: `Store.loadCollectionRows(ids…)` (`internal/praector/store_status_snapshot.go`),
  built on the existing `QueryMany`/`queryManyLocked` primitive (`internal/praector/store.go`).
  A single multi-SELECT reads collections, tasks, edges, all + OPEN revisions, latest
  acceptance, latest PR, the per-collection runnable and running-session existence
  probes, and the `CRITIC_PLAN_BASELINE` events, each bucketed back per collection in
  Go. Tasks/edges reuse `ListTasks`/`ListEdges`' `ORDER BY created_at` (per collection)
  and the existing `result_json → result` parse; they deliberately do **not** reuse
  `materialPlanFingerprintLocked`'s `ORDER BY created_at,id`, and the two loaders stay
  separate so the critic `stale` flag cannot flip. Runnable/running-session are per-collection
  existence booleans with `WHERE` predicates identical to `RunnableTasks`/`HasRunningSession`.
- Pure decision cores + delegating wrappers (single source of truth): `terminalStateForCollection`,
  `implementationsMissingPassingReviewFromRows`, `repairSupersededReviewLineageFromRows`,
  `reconcileRevisionsFromRows`, and `criticRevalidationFromRows` (reusing the existing
  pure `MaterialPlanFingerprint`/`MaterialPlanFingerprintV021`). The unchanged cid wrappers
  `UpdateCollectionTerminalState`, `ImplementationsMissingPassingReview`,
  `RepairSupersededReviewLineage`, `ReconcileCollectionRevisions`, and
  `CriticRevalidationState` now load-then-call these cores, so the scheduler, claim,
  delivery-plan, and `CollectionShow`/`CollectionRevise` callers cannot drift from the
  batched `Service.Status` path.
- Read-after-write correctness: `reconcileCollectionSnapshot` runs the repair and
  reconcile cores first (their `AddEdge`/SATISFIED writes refresh the in-memory edges and
  OPEN-revision buckets), so the terminal core observes the same post-write row set the
  per-query path would.
- `collectionStatus` display reads from the same snapshot (`collectionStatusFromSnapshot`),
  and the project loop no longer re-fetches the collection and re-lists tasks after the reconcile.
- Opt-in non-reconciling status: `StatusInput.NoReconcile` (CLI `--no-reconcile`, alias
  `--observed`, default off) renders the stored `collections.status` and the display view
  from the batched snapshot with zero writes (no lineage repair, no revision satisfy, no
  status flip, no `dolt add`/`commit`).

The Dolt contract scope is pinned to 2.7.2 (latest 2.7.3 contract-unchanged); the batched
read introduces no BEGIN/transaction (intra-batch consistency is the OS store lock, the
documented invariant). The Phase-2 threat-model/external-conformance classifier gates are
waived via a durable decision — the change touches only local `dolt sql`/`add`/`commit`
subprocesses and exposes no network/remote/protocol surface — rather than being dropped.

The snapshot contents are:

- collection;
- tasks;
- edges;
- open revisions;
- all revisions;
- latest acceptance;
- latest PR;
- runnable-task probe (existence);
- running-session probe (existence);
- relevant critic-baseline events.

### Phase 3 — Idle sweep cooldown

State: **landed**

`Scheduler.observeAcceptanceReadiness` now carries a per-collection in-memory cooldown gate (`Scheduler.readySwept map[string]time.Time`, `acceptanceReadinessCooldown = 30 * time.Second`). After the existing nil-store guard it returns early when a sweep for that collection ran less than 30s ago, otherwise it records `time.Now()` and proceeds exactly as before. The interval is **30s, deliberately not the 60s the earlier sketch suggested**: cost scales as 1/interval, so 30s already removes ~97% of the churn and 60s buys an unmeasurable extra ~0.5 invocations/s while doubling the only real downside. The sweep stays purely read-only and observability-only: same `acceptanceBlockReason(recordEmptyHarvest=false)` predicate, same fingerprint-keyed dedup in `raiseAcceptanceReadinessWarning`, no Git fast-forward probe, and it never gates or blocks any work path. A collection with no `readySwept` entry (nil map or zero-value time) is swept immediately, so a collection that newly becomes COMPLETE is still observed on the tick it flips — the cooldown only ever delays re-reporting something already settled. Cooldown decisions use monotonic-preserving comparisons on a raw `time.Now()` value (`now.Sub(last) < cooldown`) so a step/NTP clock change cannot freeze or prematurely re-arm the sweep.

Measured outcome (authoritative via the PATH-shim `dolt sql` invocation-count seam — see methodology warning — with `PRAECTOR_FAKE_DOLT_LOG`):

- **Before:** one predicate evaluation per COMPLETE collection per 1s poll tick. Measured through the seam, a single full sweep of a graph that walks the whole predicate is **10 `dolt sql` invocations**, so a 30-tick (30s) window for one COMPLETE collection is **~301 `dolt sql` invocations (~10/s)**. Live against the real user store (idle since 2026-11-11 22:54, 29 COMPLETE + 2 BLOCKED across 6 projects), the idle daemon's floor was **~8 unique daemon-child `dolt` PIDs/s** (118 in a 15s ps sample, itself an *undercount* — see below) and `/proc/<daemon>/io` showed **14.2 MB/s written and 13,590 read-syscalls/s** with zero durable work — consistent with this every-tick sweep.
- **After:** the same 30-tick window for one COMPLETE collection is **10 `dolt sql` invocations** — a single sweep, a **~30× / ~97% reduction** in the acceptance-readiness sweep's Dolt churn per collection, with the `ACCEPTANCE_READINESS_WOULD_BLOCK` marker still emitted exactly once per fingerprint.
- **Methodology warning (verified, not just asserted):** sampling a process tree with `ps`/`pgrep`, or reading only the daemon parent's `/proc/<pid>/io`, is **unreliable and can read zero while the sweep is churning**. Each `dolt` child lives under ~50 ms, so a plain `pgrep -P` snapshot loses half the exits to a race, and the read/write syscalls land in the transient children (whose `/proc` entries vanish before sampling), not the daemon parent. Process sampling is unreliable in **both** directions, so a `0` reading proves neither presence nor absence of churn: two independent live samples against the same pre-change binary disagreed, one 39.7s parent/ps sample reading a misleading `0/s` and `0` children while a 15s unique-daemon-child-PID sample read ~8/s with nonzero parent `/proc/io`. A frequent false-zero cause is worth naming: a `pgrep -f` pattern that matches the sampling shell's own command line resolves the wrong parent pid and yields a clean-looking zero (this happened during the investigation). The PATH-shim wrapper (`dolt` invoked through a counting shim) or a child `/proc/<pid>/io` delta are the authoritative counters; a clean process listing is **not** evidence of idleness, and neither is a busy one.

Outcome:

- substantially lower background Dolt traffic on an idle system;
- no change to acceptance or terminal-state correctness or to the fingerprint dedup semantics;
- delayed observability only — an out-of-band edit to an already-settled COMPLETE collection reports its would-block marker up to 30s late — which is acceptable for the non-gating sweep. The upgrade path is to invalidate the entry from the store's event-emit seam instead, but `Store.emitter` is a single optional SSE-only sink (nil when MCP is disabled), so that is not a one-line reuse.

### Phase 3.5 — Pure-core ancestry walk for the acceptance-readiness sweep

State: **landed**

Phase 3 reduced how *often* the readiness sweep runs; it deliberately left the per-sweep cost untouched, so one evaluation of one Collection still launched roughly ten `dolt sql` processes. The dominant remaining item was the predicate's directive/research ancestry walk: `Store.DirectiveAncestors` / `Store.ResearchAncestors` performed a breadth-first walk over `depends_on` edges issuing **one `dolt sql` per visited node** (a `task_edges` JOIN per frontier node), and `acceptanceBlockReason` also re-read the collection's tasks, edges, critic-baseline events, reviews/verdicts, and candidate decisions individually, plus a separate material-fingerprint tasks/edges pair.

Phase 3.5 moves that walk onto rows instead of subprocesses, reusing the Phase 2 batched snapshot machinery rather than adding a second read path:

- the BFS body is now a **pure Go core** — `ancestorWalkFromRows(tasks, edges, tid, acceptProfile)` in `internal/praector/store_status_snapshot.go` performs the same cycle-guarded, deterministically-ordered walk (frontier ordered by `created_at, from_task_id`, accepted rows copied and appended to, final `created_at,id` sort) over an already-loaded task/edge row set, with the profile filter injected as `ancestorProfileIsResearch` / `ancestorProfileIsDirectiveSource`;
- `Store.ResearchAncestors`, `Store.researchAncestorsLocked`, `Store.DirectiveAncestors`, `Store.directiveAncestorsLocked`, and `Store.Dependencies` become **load-then-call delegators**: they load the task and edge rows through one two-statement batched read (`ancestryRowStatements` / `loadAncestryRows`, the collection's `tasks` and `task_edges`, the collection resolved by a scalar subquery because the caller only knows a task id) and hand them to the same core. Their signatures, returned membership, and error text are unchanged, so the Envelope compiler, the claim-time directive gate, and the scheduler callers need no change;
- `acceptanceBlockReason` splits the same way: `acceptanceBlockReasonFromRows` holds the predicate verbatim (critic staleness from `criticRevalidationFromRows`, coverage, verdict, directive traceability with the walk now row-fed, impact enumeration, candidate decisions from the snapshot's tail statement, and the 200-row empty-harvest event window from the snapshot's events rows), and the cid-facing entry point loads one `loadCollectionRowsWithAcceptance` snapshot and delegates. Verdicts and the six error strings are byte-for-byte identical;
- `Scheduler.observeAcceptanceReadiness` loads **one** snapshot per swept Collection and passes it to both `acceptanceBlockReasonFromRows` and the new `materialFingerprintFromRows`, so the fingerprint costs zero additional reads instead of its own tasks/edges pair;
- the snapshot statement prefix stays byte-identical to Phase 2's `collectionSnapshotStatements` (statements 0..9), with the extra decisions and per-collection events-LIMIT-200 statements appended at the **tail** so the shared `status` path is untouched. One deviation is recorded rather than glossed: the appended decisions statement resolves `project_id` through `project_id IN (SELECT project_id FROM collections WHERE id=…)` because the batch cannot know the project id before reading `collections`; membership is identical and never wider than `ListDecisions`'s.

Consistency comes from the store file lock, as elsewhere — no `START TRANSACTION`/`COMMIT`, no `-b`, no `-r jsonl`/`-f file`/server mode, and Dolt 2.7.x only.

### Phase 4 — Targeted indexes

State: **planned**

Indexes should be introduced through an idempotent migration mechanism. Existing databases require explicit handling because `CREATE TABLE IF NOT EXISTS ... INDEX ...` only affects newly created tables.

Candidates:

```sql
ALTER TABLE task_edges
  ADD INDEX idx_edges_collection_relation
  (collection_id, relation);

ALTER TABLE sessions
  ADD INDEX idx_sessions_status_attempt
  (status, attempt_id);

ALTER TABLE events
  ADD INDEX idx_events_event_created
  (event_type, created_at);

ALTER TABLE events
  ADD INDEX idx_events_collection_type_created
  (collection_id, event_type, created_at);

ALTER TABLE collections
  ADD INDEX idx_collections_project_created
  (project_id, created_at);
```

Priority rationale:

1. `task_edges(collection_id, relation)` addresses a query shape with no collection-scoped edge index.
2. `sessions(status, attempt_id)` addresses running-session probes.
3. Event-type-aware indexes address readiness and baseline queries that currently scan date-ordered rows and then filter event type.
4. Project-created ordering supports project-wide collection listing.

Index write-overhead should be measured individually after query batching. They are useful, but they are not the expected primary fix at current scale.

### Deferred — Persistent or embedded Dolt

State: **deferred**

A persistent Dolt server or embedded storage engine could reduce process startup further, but adds lifecycle, connection, upgrade, locking, and deployment complexity.

Reconsider only if:

- query batching leaves an unacceptable per-query floor;
- Praector needs transactional or connection-scoped semantics unavailable through CLI batching;
- interactive operations require sub-100ms behavior independent of CLI process startup.

## Current schema observations

As of the investigation:

| Table | Useful current indexes | Notable gap |
| --- | --- | --- |
| `collections` | `(project_id, status)` | no project-created ordering index |
| `tasks` | `(collection_id, status)`, project status, dedupe | profile/status/latest-task patterns rely on smaller scans |
| `task_edges` | edge-primary plus from/to indexes | no `collection_id` index |
| `attempts` | task status, unique attempt number | global repair scan has no status/exit-code index |
| `sessions` | attempt id | no status index for running-session probes |
| `events` | collection-created and task-created | no event-type-aware indexes |
| `collection_revisions` | collection/status/created | generally adequate |
| acceptances/PRs | unique collection target/provider plus collection indexes | generally adequate |

The exact migration path has not yet been implemented. `docs/DEVELOPMENT.md` notes that bootstrap is not a full migration framework, so index addition needs a deliberate idempotent migration helper.

## Code anchors

Function names are more durable than line numbers. At the time of investigation:

| Area | Anchor |
| --- | --- |
| CLI store startup | `newStore()` in `internal/praector/cli.go` |
| Status command | `cmdStatus()` in `internal/praector/cli.go` |
| Status service | `Service.Status()` and `collectionStatus()` in `internal/praector/service.go` |
| One Dolt subprocess per query | `Store.Query()` / `Store.queryLocked()` in `internal/praector/store.go` |
| Bootstrap | `Store.Bootstrap()` and `Store.repairInvariantsLocked()` |
| Bootstrap batching (live) | `Store.loadInvariantRowsLocked()` → `Store.repairLegacyDependencyCyclesLockedAllWithRows()` / `Store.repairRevisionBaselineProvenanceLockedAll()` |
| Standalone cycle repair | `Store.RepairLegacyDependencyCycles()` / `Store.repairLegacyDependencyCyclesLocked()` |
| Revision baseline repair | `Store.repairRevisionBaselineProvenanceLocked()` |
| Terminal reconciliation | `Store.UpdateCollectionTerminalState()` |
| Coverage validator | `Store.ImplementationsMissingPassingReview()` |
| Critic revalidation | `Store.CriticRevalidationState()` |
| Acceptance predicate | `acceptanceBlockReason()` / `acceptanceBlockReasonFromRows()` in `internal/praector/cli.go` |
| Ancestry walk core (no reads) | `ancestorWalkFromRows()` / `dependsOnAdjacency()` / `ancestorProfileIsResearch` / `ancestorProfileIsDirectiveSource` in `internal/praector/store_status_snapshot.go` |
| Ancestry row loader | `ancestryRowStatements()` / `Store.loadAncestryRows()` / `Store.loadAncestryRowsLocked()` in `internal/praector/store.go` |
| Acceptance-variant snapshot loader | `Store.loadCollectionRowsWithAcceptance()` / `materialFingerprintFromRows()` |
| Daemon polling | `Scheduler.Daemon()` in `internal/praector/scheduler.go` |
| Acceptance readiness sweep | `Scheduler.observeAcceptanceReadiness()` |
| Embedded schema | `internal/assets/schema.sql` |
| Fake Dolt test harness | `internal/praector/testdolt_test.go` |

## Verification protocol

Do not accept an optimization based on source inspection alone.

For each landed phase:

1. Run `make check`.
2. Capture the same operation under the PATH wrapper.
3. Compare:
   - wall time;
   - total Dolt invocations;
   - `dolt sql` invocations;
   - number of Dolt add/commit processes.
4. Record before/after values in the tracker below.
5. Confirm the operation’s durable semantics did not change.
6. Update focused architecture/state docs when behavior or lifecycle changed.

Useful commands:

```bash
make check

rm -f /tmp/praector-perf/queries.log
time env PATH="/tmp/praector-perf:$PATH" praector status COL-...
wc -l /tmp/praector-perf/queries.log
grep -c '^sql' /tmp/praector-perf/queries.log
```

For tests, prefer a counting fake-Dolt or wrapper seam that asserts an upper bound on invocations. A test that only checks the result while still launching dozens of processes would not protect against regression.

## Acceptance criteria

The optimization work is complete when:

- a normal CLI status invocation no longer performs dozens of independent Dolt reads;
- bootstrap invariant scanning uses a bounded small number of SQL processes;
- one status path loads each collection’s tasks/edges once per reconciliation phase;
- idle daemon acceptance sweeps are rate-limited;
- the acceptance predicate, including its ancestor walks, evaluates from already-loaded rows rather than one read per graph node;
- existing cycle repair, terminal-state, review-coverage, revision, and acceptance semantics remain unchanged;
- targeted indexes are added idempotently or their omission is explicitly measured and justified;
- `make check` passes;
- measurements are recorded here rather than inferred.

## Results tracker

| Phase | State | Baseline | Result | Notes |
| --- | --- | --- | --- | --- |
| Investigation | Landed in this tracker | 86 queries / 8.92 s single status | Evidence recorded | No runtime change |
| Multi-query helper | **Landed** | One `dolt sql` per logical read | `QueryMany`/`queryManyLocked` batch N reads into one JSONL process | Read-side only; `{}`/blank → empty; statement-indexed errors |
| Bootstrap batching | **Landed** | 51 `dolt sql` / 53 dolt / 2.75 s (`project list`, 23 collections) | 3 `dolt sql` / 5 dolt / 0.30 s; bootstrap repair alone 49 → 2 | Cycle + baseline repair outcomes unchanged |
| Bootstrap batching refinement (Phase 1.1) | **Landed** | Revision-baseline repair issues one `tasks WHERE id IN` read per matching candidate | 0 such reads (satisfied from the batched snapshot); batched task/edge reads now carry a deterministic `ORDER BY` | Clean benchmark; per-candidate `collection_revisions`/`collections` reads retained; repair SET unchanged |
| Snapshot reconciliation (Phase 2) | **Landed** | ~31 `dolt sql` single collection; ~491 for a no-target project (pre-Phase-1.1) / ~114 (live post-Phase-1.1) | One batched snapshot read per collection: project-wide `status` is a constant **3** `dolt sql` regardless of collection count; single-collection default **6** total (incl. the flip write); `--no-reconcile` **1** read, **0** write | Read-path refactor; terminal-state/revision/coverage/repair semantics byte-identical; opt-in `--no-reconcile` zero-write render |
| Sweep cooldown | **Landed** | One sweep per COMPLETE collection per second (~10 `dolt sql`/s per collection; ~8 daemon-child PIDs/s live idle) | 30s in-memory per-collection gate (`readySwept`): one sweep per COMPLETE collection per 30s → **~301 → 10 `dolt sql` per 30s window (~97% less)**; exactly one `ACCEPTANCE_READINESS_WOULD_BLOCK` still; first-tick unconditional | Internal 30s constant, no config/CLI/schema surface; sweep stays read-only/observability-only, no gate/block; monotonic-preserving compare; `ps`/parent-`/proc` sampling unreliable (two live samples disagreed: `0/s` vs ~8/s) — PATH-shim seam authoritative |
| Acceptance-predicate ancestry walk (Phase 3.5) | **Landed** | One acceptance-readiness sweep ≈10 `dolt sql` per collection, incl. one per-node `task_edges JOIN tasks` read per BFS ancestor node | **One** batched `dolt sql` per swept collection (N collections → N invocations), 0 per-node reads, 0 extra reads for the material-plan fingerprint; would-block path adds only the dedup `ListEvents` + marker write | Pure-core walk over already-loaded rows (`ancestorWalkFromRows`); the four `*Ancestors`/`Dependencies` wrappers and `acceptanceBlockReason` became load-then-call delegators with byte-identical membership, verdicts, and error text; no gate/cycle-guard/fingerprint-dedup/cooldown change; appended snapshot statements are tail-only so the shared `status` path is byte-identical; live idle-daemon re-capture not re-run (sandbox-blocked) |
| Targeted indexes | Planned | No collection edge/event-type indexes | Pending | Needs idempotent migration |
| Persistent/embedded Dolt | Deferred | Not measured | Deferred | Revisit only after batching |

### Phase 1 measured result (clean benchmark)

These are **clean-benchmark** figures, not a live-environment capture (see
"Clean benchmark vs live environment" above). Captured under the PATH-shim methodology
against a benchmark DB seeded to the investigation snapshot scale (23 collections, 241
tasks, 603 edges, 262 attempts, 1713 events), comparing the committed pre-change binary
against this change's build:

| Command | Before | After |
| --- | ---: | ---: |
| `praector project list` — `dolt sql` invocations | 51 | 3 |
| `praector project list` — total `dolt` invocations | 53 | 5 |
| `praector project list` — `dolt add`/`commit` | 2 | 2 |
| `praector project list` — wall time | 2.75 s | 0.30 s |

The `dolt sql` line for the batched bootstrap is a single process carrying all five
reads (`attempts` / baseline `events` / `collections` / `tasks` / `task_edges`) joined
by `;`, plus the one schema-initialization `sql` and the command's own `projects`
query. Bootstrap invariant repair alone therefore dropped from 49 SQL processes (schema
+ global scans + one read pair per collection) to 2. `TestQueryManySingleProcessAndBootstrapBound`
asserts the batch is one process and bootstrap stays bounded regardless of collection
count, and `TestBootstrapBatchedRepairMatchesStandaloneRepair` asserts the batched run
reproduces the standalone repair's durable outcome for all three repair paths.

### Phase 1.1 measured result (clean benchmark)

Phase 1.1 refines the same batched bootstrap region without changing any repair
outcome:

- The revision-baseline provenance repair no longer issues a `tasks WHERE id IN
  (criticID,planReviewID)` read per matching candidate; the two-row selection and the
  `requested_by_task_id` coordinator match are reproduced from the rows already loaded
  by `loadInvariantRowsLocked`, so the batched path makes **exactly one fewer `dolt
  sql` per matching candidate** while the per-candidate `collection_revisions ... LIMIT
  1` selection and the `collections` project_id fallback remain. This is a clean-
  benchmark reduction measured through the invocation-count seam, not a live capture.
- The batched `tasks` and `task_edges` reads now carry an explicit deterministic
  `ORDER BY`, so batched-vs-standalone agreement rests on MySQL SELECT ordering rather
  than Dolt's implicit Prolly-Tree scan order. Because `computeLegacyRepairs` reaches a
  fixpoint, the repaired edge SET is unchanged; only the DELETE-statement and emitted
  `LEGACY_DEPENDENCY_CYCLE_REPAIRED` ordering is pinned.

`TestBootstrapRevisionBaselineDropsTaskPairRead` asserts the removed per-candidate
task-pair read (zero `WHERE id IN` reads) while the `collection_revisions` and
`collections` per-candidate reads remain; `TestComputeLegacyRepairsOrderIndependent`
asserts the surviving/removed edge SET is invariant to read order across every edge
permutation; and `TestBootstrapCycleRepairDeterministicEmission` asserts the batched
edge read's `ORDER BY` is present, the surviving edge SET matches the expected repaired
outcome, and the emitted DELETE order is deterministic. `TestBootstrapBatchedRepairMatchesStandaloneRepair`
still passes byte-for-byte.

### Phase 2 measured result (clean benchmark)

These are **clean-benchmark** figures measured through the counting fake-Dolt seam
(`PRAECTOR_FAKE_DOLT_LOG`, the same methodology the "prefer a counting fake-Dolt or
wrapper seam" note above prescribes), not a live-environment capture. Each figure is the
number of `dolt sql` subprocesses a single `praector status` launches:

| Command | Before | After |
| --- | ---: | ---: |
| project-wide `status` — `dolt sql`, 2 collections | scales with collections | 3 |
| project-wide `status` — `dolt sql`, 18 collections | scales with collections (~491 pre-Phase-1.1 / ~114 live post-Phase-1.1) | **3** (constant) |
| single-collection `status COL-…` (reconciles a flip) — `dolt sql` | ~31 | 6 (incl. the status-flip write) |
| single-collection `status COL-… --no-reconcile` — `dolt sql` | ~31 | 1 |
| `status --no-reconcile` — `dolt add`/`commit`/writes | performs writes | **0** |

The project-wide read count is now a **constant** independent of the collection count
(the batched snapshot read plus the per-collection status-flip write are the only SQL;
the display issues no extra per-collection process). The parity/HEAD/read-count guards
live in `internal/praector/status_snapshot_test.go`:

- `TestStatusBatchedMatchesReference` / `TestStatusSingleCollectionMatchesReference`: the
  batched path yields an identical `CollectionStatus` display, identical final table rows
  and collection status, and an identical event stream versus the reference
  `UpdateCollectionTerminalState` + `collectionStatus` path, across seeds that include an
  OPEN revision reconcile flips SATISFIED, a stale critic baseline, a collection with
  multiple critic tasks, and a collection where repair adds an edge;
- `TestStatusDefaultFlipsAndNoReconcileLeavesStateProvesZeroWrites`: default status still
  performs the needed `SetCollectionStatus` flip write, while `--no-reconcile` leaves stored
  state unchanged with zero write `dolt sql`;
- `TestStatusReadCountConstantVsCollectionCount` / `TestStatusSingleAndNoReconcileReadCount`:
  the constant-vs-count and single-collection / `--no-reconcile` read-count figures above;
- `TestBatchedDocumentShapeAgainstRealDolt`: a real-Dolt 2.7.2 oracle that the batched
  `dolt sql -r json -q` multi-SELECT document shape (one document per statement, `{}` for an
  empty mid-batch member) decodes identically to the standalone reads.

`TestSkillCLICommandHelpCoversCommandHelp` and the `serviceOps`/MCP parity tests still pass
with the new `--no-reconcile` flag and the additive `StatusInput.NoReconcile` field.

### Phase 3.5 measured result (counting seam)

These figures are measured through the counting `dolt sql` seam (`PRAECTOR_FAKE_DOLT_LOG`, the same seam the Phase 2 figures above use and the same PATH-shim methodology the wrapper note above prescribes), not a live-environment capture. Each figure counts `dolt sql` subprocesses launched by one acceptance-readiness sweep:

| Operation | Before | After |
| --- | ---: | ---: |
| one sweep of one settled, acceptable `COMPLETE` collection | ~10 `dolt sql` (the Phase 3 seam figure for a full predicate walk) | **1** batched `dolt sql` |
| one sweep over N settled `COMPLETE` collections (N = 3 seeded) | scales per collection, ~10 each | **exactly N** — one batched read per collection |
| per-BFS-node ancestry read (`task_edges e JOIN tasks t … WHERE e.to_task_id=`) | one per visited node | **0** |
| material-plan fingerprint inside the same sweep | its own tasks/edges pair | **0 extra reads** (same snapshot) |
| would-block path (marker fires) | predicate reads + dedup read + marker write | the one snapshot batch + the dedup `ListEvents` + the marker write |

The floor guards are durable tests in `internal/praector/ancestry_rows_test.go`:

- `TestAcceptanceReadinessSweepIssuesOneBatchPerCollection` / `TestAcceptanceReadinessSweepOverNCollectionsIssuesNBatches`: the read-budget rows above (one batched read per collection, N collections → N invocations, zero per-node JOIN reads, decisions/events riding inside the same process);
- the three-way parity tests: a copied reference of the removed SQL predicate, the cid-facing wrapper, and the rows path produce the **identical** first-block message for all six would-block reasons, and the empty-harvest write is still reachable from the rows path;
- `TestAncestryWalkCrossCollectionEdgeIsWriterInvariant`: a seeded stray edge into another collection's task admits no foreign ancestor;
- the over-200-events window test: the snapshot's tail events statement equals `EventsForCollection(cid, 200)`;
- the pure-walk and frontier tests: `ancestorWalkFromRows` performs no reads and the frontier visit order is `(created_at, from_task_id)`-stable under shuffled input.

**Live capture status:** the idle-daemon child-`cmdline` re-measurement against the real user store was **not** re-run for this phase — the worker sandbox for the landing attempt blocks the out-of-tree writes and binary execution the live harness needs. The Phase 3 live figures above therefore remain the last live-environment capture of this sweep, and are explicitly **pre-Phase-3.5**; the per-collection floor they imply (~10 reads per sweep, ~8 daemon-child PIDs/s) is superseded by the seam-measured **one read per collection per 30s window** recorded here. A live re-capture is the first item under Risks and open questions.

### Live Phase 1.1 captures (live environment)

These are **live-environment** captures — the real, populated user store with a normal
`praector daemon` active — and are deliberately kept distinct from both the seeded
23-collection clean benchmark (the "Phase 1 / Phase 1.1 measured result" tables above)
and the original live baseline in the "Measured baseline" section. They are **not** a
seeded benchmark and must not be conflated with the clean-benchmark `dolt sql` counts.

Methodology and environment:

- PATH-shim wrapping of every Dolt invocation, forwarding to the real binary; totals read with `grep -c '^sql'` plus the `dolt add`/`commit` processes (per the Reproduction methodology above).
- Installed `praector 0.2.5` binary; `main` commit `5c12bff6`; Dolt `2.7.2`; the live user store; a normal `praector daemon` running during measurement.
- Caveat: `praector status` is not purely read-only — its terminal-state reconciliation can write durable state, so a `status` capture may include terminal-state Dolt `add`/`commit` writes. Read these as realistic operational measurements, not immutable snapshots.
- Determinism/ordering claims below are scoped to **Dolt 2.7.x** (verified on `2.7.2`); they are not a permanent cross-version guarantee.

Observed live totals and wall times:

| Command | `dolt sql` | `dolt add`/`commit` | Total Dolt | Wall time |
| --- | ---: | ---: | ---: | ---: |
| `praector project list` | 3 | 2 | 5 | ~0.58–0.61 s |
| `praector status COL-91f2bd6664` | 29 | 2 | 31 | ~4.90–5.11 s |
| `praector status COL-504ca4be8f` | 29 | 2 | 31 | ~5.07 s |
| `praector status --project PRJ-fe97887502` | 112 | 2 | 114 | ~18.79 s |

Where a directly comparable documented baseline exists in the "Measured baseline" section, the before/after is:

| Command | Baseline | Live Phase 1.1 |
| --- | ---: | ---: |
| `praector status COL-504ca4be8f` | 86 calls / 8.92 s | 31 calls / 5.07 s |
| `praector status --project PRJ-fe97887502` | 169 calls / 21.96 s | 114 calls / 18.79 s |

`praector status COL-91f2bd6664` is a **live-only** capture: no documented "before"
baseline exists for it (only `COL-504ca4be8f` and `PRJ-fe97887502` are baselined
above), so it is never presented as a before/after pair.

Interpretation: bootstrap-dominated commands (`project list`) are now small (a single
batched read + the command's own query). Collection `status` terminal-state
reconciliation remains the dominant remaining cost, and project-wide `status` remains
Phase 2 (collection-snapshot reconciliation) work rather than anything addressed here.

## Risks and open questions

- Multi-statement Dolt output may include multiple JSON documents and `{}` for empty results; the parser must handle both.
- Existing tests assume one JSON object from fake Dolt.
- Multi-statement reads are not equivalent to a declared transaction unless Dolt’s CLI session semantics are explicitly verified. The filesystem lock prevents ordinary concurrent Praector writes, but transaction semantics should not be assumed merely because statements share one process.
- `status` can create durable terminal-state updates and commits. Benchmarks should compare the same realistic collection states.
- Existing databases have already been created from the current schema, so index definitions must have a migration path rather than only appearing in `schema.sql`.
- Sweep cooldown trades observability latency for background cost. The cooldown should be justified and measured rather than introduced as a configuration-heavy feature.
- **Open:** the post-Phase-3.5 idle-daemon live re-capture (PATH-shim `dolt sql` counts plus the child-`cmdline` classification against the real user store) has not been run; the phase's floor is recorded from the counting seam. Re-run it on a 0.2.6+ daemon and record it here, remembering that process sampling is unreliable in both directions and a child-`/proc` or shim counter is authoritative.
- Row-fed walks read the row set captured under the file lock at the start of the predicate, so a concurrent commit landing mid-predicate is observed as of the snapshot rather than mid-walk. That is the same consistency model every other batched path already relies on (no declared transaction), and it cannot widen any membership rule, but it does mean a walk and a concurrent write are ordered by the lock rather than linearized.
- The daemon may have an active project/collection mix different from the benchmark database. Separate idle-daemon measurements from interactive CLI measurements.

## Maintenance rules

Future performance work must update this document in the same change with:

1. the problem and baseline measurement;
2. the design/phase changed;
3. before/after invocation counts and timings;
4. any new schema/migration decisions;
5. tests that protect against query-count regression;
6. a status update in the results tracker.

When documentation and source/tests disagree, source/tests are authoritative and the documentation must be corrected in the same change.
