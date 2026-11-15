package praector

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Acceptance-readiness sweep COOLDOWN (Scheduler.observeAcceptanceReadiness readySwept
// gate). An idle daemon previously re-ran the read-only acceptance predicate on every
// COMPLETE collection for every 1s poll tick, spawning a dolt child each call. The gate
// rate-limits that observability sweep per collection to at most once per
// acceptanceReadinessCooldown (30s) while preserving the exactly-one would-block marker.
//
// Cooldown suppression and marker dedup are INDEPENDENT mechanisms: the exactly-one
// ACCEPTANCE_READINESS_WOULD_BLOCK property is guaranteed by the material-plan fingerprint
// dedup in raiseAcceptanceReadinessWarning, NOT by the cooldown. So every cooldown assertion
// counts predicate EVALUATIONS through the fake-Dolt read-SQL seam
// (PRAECTOR_FAKE_DOLT_LOG + countSQLOperations) — a suppressed sweep issues ZERO dolt sql
// invocations because the gate returns before the first read — rather than counting events,
// which would pass for the wrong reason.

// unacceptableCompleteCollection builds a COMPLETE-but-unacceptable collection: an
// implement covered by a passing review (so the coverage rule reads COMPLETE) plus a second
// COMPLETE review with a MISSING verdict (the acceptance-only surprise the sweep surfaces).
func unacceptableCompleteCollection(t *testing.T) (*Store, GlobalConfig, Row, Row) {
	t.Helper()
	assertFloorObjectiveAllFalse(t)
	s, cfg, c, coord := gateFreeFloorCollection(t, floorObjective)
	impl := floorImplement(t, s, c, coord)
	if err := s.SetTaskStatus(S(coord, "id"), TaskComplete, "coord handed off"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTaskStatus(S(impl, "id"), TaskComplete, "impl complete"); err != nil {
		t.Fatal(err)
	}
	reviewPass, _, err := s.CreateTask(CreateTaskOptions{ProjectID: S(c, "project_id"), CollectionID: S(c, "id"), Profile: "review", Title: "Independent review", Objective: "review", Reason: "review", ParentTaskID: S(coord, "id"), DependsOn: []string{S(impl, "id")}, WorkspaceMode: "collection", Depth: 1, AllowDedupe: false})
	if err != nil {
		t.Fatal(err)
	}
	completeReviewWithVerdict(t, s, reviewPass, "pass")
	if err := s.AddEdge(S(impl, "id"), S(reviewPass, "id"), S(c, "id"), "verifies"); err != nil {
		t.Fatal(err)
	}
	reviewBad, _, err := s.CreateTask(CreateTaskOptions{ProjectID: S(c, "project_id"), CollectionID: S(c, "id"), Profile: "review", Title: "Second review", Objective: "review", Reason: "review", ParentTaskID: S(coord, "id"), WorkspaceMode: "collection", Depth: 1, AllowDedupe: false})
	if err != nil {
		t.Fatal(err)
	}
	completeReviewWithVerdict(t, s, reviewBad, "")
	if err := s.SetCollectionStatus(S(c, "id"), "COMPLETE", true); err != nil {
		t.Fatal(err)
	}
	return s, cfg, c, coord
}

func resetSQLLog(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

// (a)+(b) A blocking COMPLETE collection evaluates the readiness predicate AT MOST ONCE per
// cooldown window (counted via the read-SQL seam, not events) while still emitting exactly
// ONE ACCEPTANCE_READINESS_WOULD_BLOCK. The very first sweep on a fresh scheduler (empty
// readySwept) is unconditional, so a newly-COMPLETE collection is observed on the flip tick
// with no delay.
func TestAcceptanceReadinessSweepCooldownEvaluatesPredicateOncePerWindow(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "sqlops.log")
	s, cfg, c, _ := unacceptableCompleteCollection(t)
	t.Setenv("PRAECTOR_FAKE_DOLT_LOG", logPath)
	sched := newFloorScheduler(t, s, cfg)
	if len(sched.readySwept) != 0 {
		t.Fatalf("a fresh scheduler must start with an empty readySwept map, got %d entries", len(sched.readySwept))
	}

	// First sweep: no readySwept entry, so the predicate is evaluated immediately.
	resetSQLLog(t, logPath)
	sched.observeAcceptanceReadiness(S(c, "id"))
	first := countSQLOperations(t, logPath)
	if first == 0 {
		t.Fatal("a collection with no readySwept entry must be swept on the first call with no delay")
	}
	if got := collectionEventsFor(t, s, c, "ACCEPTANCE_READINESS_WOULD_BLOCK"); len(got) != 1 {
		t.Fatalf("the first sweep must emit exactly one would-block marker, got %d", len(got))
	}

	// Back-to-back sweeps inside the cooldown window: the gate suppresses the predicate
	// entirely (zero dolt sql), so no evaluation churn.
	for i := 0; i < 5; i++ {
		resetSQLLog(t, logPath)
		sched.observeAcceptanceReadiness(S(c, "id"))
		if n := countSQLOperations(t, logPath); n != 0 {
			t.Fatalf("a sweep within the cooldown window must evaluate the predicate zero times, got %d dolt sql calls on iteration %d", n, i)
		}
	}

	// Marker independence: five suppressed sweeps still yield exactly ONE marker. The
	// suppression is the cooldown, the single marker is the fingerprint dedup — both hold.
	if got := collectionEventsFor(t, s, c, "ACCEPTANCE_READINESS_WOULD_BLOCK"); len(got) != 1 {
		t.Fatalf("suppressed cooldown sweeps must keep the would-block marker at exactly one, got %d", len(got))
	}
	if got := S(mustGetCollection(t, s, c), "status"); got != "COMPLETE" {
		t.Fatalf("the sweep must leave a COMPLETE collection COMPLETE, got %s", got)
	}
}

// (D-test-meaning) After the cooldown window elapses (a STALE readySwept timestamp), the
// predicate re-evaluates AND an unchanged material-plan fingerprint still yields exactly
// one marker via dedup. This preserves the fingerprint-re-arm guarantee that the back-to-
// back emitsOnce test loses once the time gate suppresses repeat calls.
func TestAcceptanceReadinessSweepResweepsAfterStaleWindowKeepsFingerprintDedup(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "sqlops.log")
	s, cfg, c, _ := unacceptableCompleteCollection(t)
	t.Setenv("PRAECTOR_FAKE_DOLT_LOG", logPath)
	sched := newFloorScheduler(t, s, cfg)

	sched.observeAcceptanceReadiness(S(c, "id"))
	if got := collectionEventsFor(t, s, c, "ACCEPTANCE_READINESS_WOULD_BLOCK"); len(got) != 1 {
		t.Fatalf("the first sweep must emit exactly one marker, got %d", len(got))
	}

	// Force the entry stale, then re-sweep: the predicate re-evaluates (read-SQL seam) and
	// the unchanged fingerprint still suppresses a second marker.
	sched.readySwept[S(c, "id")] = time.Now().Add(-acceptanceReadinessCooldown - time.Second)
	resetSQLLog(t, logPath)
	sched.observeAcceptanceReadiness(S(c, "id"))
	if n := countSQLOperations(t, logPath); n == 0 {
		t.Fatal("a stale readySwept timestamp must re-evaluate the predicate on the next sweep")
	}
	if got := collectionEventsFor(t, s, c, "ACCEPTANCE_READINESS_WOULD_BLOCK"); len(got) != 1 {
		t.Fatalf("an unchanged fingerprint must still yield exactly one marker via dedup after a re-sweep, got %d", len(got))
	}
}

// (c) An acceptable COMPLETE collection emits nothing, and the cooldown never fabricates a
// would-block marker. (Complements TestAcceptanceReadinessSweepSilentForAcceptable by also
// proving the gate itself records the sweep and stays silent across a re-sweep window.)
func TestAcceptanceReadinessSweepCooldownSilentForAcceptable(t *testing.T) {
	assertFloorObjectiveAllFalse(t)
	s, cfg, c, coord := gateFreeFloorCollection(t, floorObjective)
	impl := floorImplement(t, s, c, coord)
	if err := s.SetTaskStatus(S(coord, "id"), TaskComplete, "coord handed off"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTaskStatus(S(impl, "id"), TaskComplete, "impl complete"); err != nil {
		t.Fatal(err)
	}
	reviewPass, _, err := s.CreateTask(CreateTaskOptions{ProjectID: S(c, "project_id"), CollectionID: S(c, "id"), Profile: "review", Title: "Independent review", Objective: "review", Reason: "review", ParentTaskID: S(coord, "id"), DependsOn: []string{S(impl, "id")}, WorkspaceMode: "collection", Depth: 1, AllowDedupe: false})
	if err != nil {
		t.Fatal(err)
	}
	completeReviewWithVerdict(t, s, reviewPass, "pass")
	if err := s.AddEdge(S(impl, "id"), S(reviewPass, "id"), S(c, "id"), "verifies"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetCollectionStatus(S(c, "id"), "COMPLETE", true); err != nil {
		t.Fatal(err)
	}
	sched := newFloorScheduler(t, s, cfg)
	sched.observeAcceptanceReadiness(S(c, "id"))
	// The gate recorded the sweep (so the second call is cooldown-suppressed), yet an
	// acceptable collection emits zero markers either way.
	if _, ok := sched.readySwept[S(c, "id")]; !ok {
		t.Fatal("an acceptable sweep must still record its readySwept timestamp")
	}
	sched.observeAcceptanceReadiness(S(c, "id"))
	if got := collectionEventsFor(t, s, c, "ACCEPTANCE_READINESS_WOULD_BLOCK"); len(got) != 0 {
		t.Fatalf("an acceptable COMPLETE collection must emit no would-block marker, got %d", len(got))
	}
	if _, _, e := validateCollectionForAcceptance(s, c); e != nil {
		t.Fatalf("the acceptable collection must pass accept-time validation unchanged; got %v", e)
	}
}

// (d) Reconcile isolation: the cooldown gate lives ONLY in observeAcceptanceReadiness. A
// sweep on a RUNNING collection is observability-only (it returns at the COMPLETE guard,
// emits nothing, and never regresses/delays/changes status), and the reconcile path for
// RUNNING/BLOCKED collections is untouched — reconcileCollectionSafely never emits a
// would-block marker.
func TestAcceptanceReadinessSweepCooldownDoesNotAffectReconcile(t *testing.T) {
	assertFloorObjectiveAllFalse(t)
	s, cfg, c, coord := gateFreeFloorCollection(t, floorObjective)
	impl := floorImplement(t, s, c, coord)
	if err := s.SetTaskStatus(S(coord, "id"), TaskComplete, "coord handed off"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTaskStatus(S(impl, "id"), TaskComplete, "impl complete"); err != nil {
		t.Fatal(err)
	}
	// Leave the collection RUNNING (never flip to COMPLETE) — reconcile owns RUNNING/BLOCKED.
	if got := S(mustGetCollection(t, s, c), "status"); got == "COMPLETE" {
		t.Fatalf("fixture must start non-COMPLETE, got %s", got)
	}
	sched := newFloorScheduler(t, s, cfg)

	// One sweep on the RUNNING collection never gates, blocks, delays, or changes status.
	sched.observeAcceptanceReadiness(S(c, "id"))
	if got := S(mustGetCollection(t, s, c), "status"); got != "RUNNING" {
		t.Fatalf("a sweep must never change a RUNNING collection's status, got %s", got)
	}
	if got := collectionEventsFor(t, s, c, "ACCEPTANCE_READINESS_WOULD_BLOCK"); len(got) != 0 {
		t.Fatalf("a sweep on a non-COMPLETE collection must emit no would-block marker, got %d", len(got))
	}

	// Reconcile (the RUNNING/BLOCKED path) is unaffected and never emits the sweep marker.
	sched.reconcileCollectionSafely(S(c, "id"))
	if got := collectionEventsFor(t, s, c, "ACCEPTANCE_READINESS_WOULD_BLOCK"); len(got) != 0 {
		t.Fatalf("reconcile must never emit a would-block marker, got %d", len(got))
	}
}
