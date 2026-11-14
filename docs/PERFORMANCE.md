# Performance and query-cost tracker

This document is a durable engineering tracker for Praector performance work. It records baselines, evidence, decisions, optimization phases, and measured results as iterative work lands.

It is not release-notes prose: [`../CHANGELOG.md`](../CHANGELOG.md) remains the per-change history. This document should be updated whenever a performance baseline is captured or a query-cost optimization is designed, landed, benchmarked, or deliberately deferred.

## Current status

- Initial performance investigation: **complete**.
- Code optimization: **Phase 1 landed** (multi-query seam + CLI bootstrap read batching). Phases 2–4 not yet landed.
- Primary affected surface: `praector status`, CLI startup/bootstrap, terminal-state reconciliation, and idle daemon observability sweeps.
- Primary conclusion: the dominant cost is **Dolt subprocess churn and repeated loading of the same state**, not slow SQL over the current dataset size.
- Recommended order:
  1. batch logical reads through a JSONL-aware multi-query helper — **landed** (`Store.QueryMany` / `queryManyLocked`);
  2. batch CLI bootstrap invariant scans — **landed** (`Store.loadInvariantRowsLocked` drives one batched read across both bootstrap repairs);
  3. use one per-collection snapshot across status/terminal reconciliation;
  4. rate-limit idle COMPLETE-collection readiness sweeps;
  5. add targeted indexes through an idempotent migration path.

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

`repairLegacyDependencyCyclesLockedAll()` lists collections, then invokes a per-collection repair.

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

The emitted readiness event is deduplicated by material-plan fingerprint, but deduplication prevents repeated writes, not repeated predicate evaluation. Consequently, a completely idle system can repeatedly execute expensive acceptance predicates against unchanged COMPLETE collections.

This path is an observability sweep, not a correctness gate. It is therefore a strong candidate for a cooldown or event-driven recheck.

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

State: **planned**

Introduce an internal snapshot for a collection containing the information needed by both terminal-state reconciliation and display:

- collection;
- tasks;
- edges;
- open revisions;
- all revisions;
- latest acceptance;
- latest PR;
- runnable-task probe;
- running-session probe;
- relevant critic-baseline events.

Refactor internal helpers to accept preloaded task/edge views rather than reloading them:

- superseded review-lineage repair;
- revision reconciliation;
- terminal-state calculation;
- passing-review coverage;
- critic revalidation;
- `collectionStatus`.

Expected outcome:

- a single collection requires a small number of SQL processes rather than dozens;
- all validators operate on one internally consistent view while the Dolt lock is held.

### Phase 3 — Idle sweep cooldown

State: **planned**

Rate-limit acceptance-readiness sweeps for unchanged COMPLETE collections. The first implementation can use an in-memory interval such as 60 seconds and retain the existing material-fingerprint dedup marker.

Expected outcome:

- substantially lower background Dolt traffic on an idle system;
- no change to acceptance or terminal-state correctness;
- delayed observability only, which is acceptable for the non-gating sweep.

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
| Global cycle repair | `Store.repairLegacyDependencyCyclesLockedAll()` |
| Revision baseline repair | `Store.repairRevisionBaselineProvenanceLocked()` |
| Terminal reconciliation | `Store.UpdateCollectionTerminalState()` |
| Coverage validator | `Store.ImplementationsMissingPassingReview()` |
| Critic revalidation | `Store.CriticRevalidationState()` |
| Acceptance predicate | `acceptanceBlockReason()` in `internal/praector/cli.go` |
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
| Snapshot reconciliation | Planned | 86+ queries single status | Pending | Preserve all terminal-state semantics |
| Sweep cooldown | Planned | One sweep per COMPLETE collection per second | Pending | Internal interval first |
| Targeted indexes | Planned | No collection edge/event-type indexes | Pending | Needs idempotent migration |
| Persistent/embedded Dolt | Deferred | Not measured | Deferred | Revisit only after batching |

### Phase 1 measured result

Captured under the PATH-shim methodology against a benchmark DB seeded to the
investigation snapshot scale (23 collections, 241 tasks, 603 edges, 262 attempts,
1713 events), comparing the committed pre-change binary against this change's build:

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

## Risks and open questions

- Multi-statement Dolt output may include multiple JSON documents and `{}` for empty results; the parser must handle both.
- Existing tests assume one JSON object from fake Dolt.
- Multi-statement reads are not equivalent to a declared transaction unless Dolt’s CLI session semantics are explicitly verified. The filesystem lock prevents ordinary concurrent Praector writes, but transaction semantics should not be assumed merely because statements share one process.
- `status` can create durable terminal-state updates and commits. Benchmarks should compare the same realistic collection states.
- Existing databases have already been created from the current schema, so index definitions must have a migration path rather than only appearing in `schema.sql`.
- Sweep cooldown trades observability latency for background cost. The cooldown should be justified and measured rather than introduced as a configuration-heavy feature.
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
