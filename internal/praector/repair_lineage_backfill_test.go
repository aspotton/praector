package praector

import (
	"os"
	"strings"
	"testing"
)

// Retroactive repair-lineage carry-edge backfill sweep
// (Scheduler.backfillRepairLineageCarryEdges, driven from Scheduler.reconcileCollection).
//
// The carry invariant was previously wired ONLY at trigger time by ensureReview. A
// failed-review -> repair -> fresh-passing-review cycle that settled before that wiring
// existed, or where the daemon died between integration and wiring, left the carrying pass
// review with no verifies edge onto the ORIGINAL implementation. Because
// ImplementationsMissingPassingReview counts coverage only via verifies edges or a review
// parent, such a collection stayed permanently BLOCKED with no reachable graph movement
// (the live COL-4274d1d8d9 shape). These tests replay that exact durable graph shape and
// drive ONE daemon reconcile tick — the retroactive path, NOT a direct ensureReview call —
// to prove the sweep heals it, is idempotent, and never relaxes coverage.

// backfillReconcileSched returns a reconcile-capable Scheduler over an existing store. The
// reconcile path only touches Store-backed enforcement and the quarantine maps, so a fully
// wired NewScheduler (with a Git workspace) is not required to drive a single tick.
func backfillReconcileSched(s *Store) *Scheduler {
	return &Scheduler{Store: s, quarantined: map[string]bool{}, reportedQuarantines: map[string]bool{}}
}

// addFreshPassingReviewOnRepair plants the missing final review of a settled repair lineage:
// a COMPLETE review that depends_on both the repair and the failed review (the deps shape
// ensureReview wires) and carries the NORMAL verifies edge onto the repair it was created to
// re-check, but is MISSING the carry edge onto the ORIGINAL implementation. This is the live
// COL-4274d1d8d9 gap: the carrying pass review exists with verdict=pass but carries no
// verifies edge onto the original implementation(s). The coverage rule counts the repair via
// its repair->review verifies edge; only the original carry is absent, which is exactly the
// gap the sweep closes.
func addFreshPassingReviewOnRepair(t *testing.T, sh repairShape) Row {
	t.Helper()
	fresh, _, err := sh.s.CreateTask(CreateTaskOptions{
		ProjectID: S(sh.p, "id"), CollectionID: S(sh.c, "id"), Profile: "review", Title: "Fresh post-repair",
		Objective: "Independent post-repair review", Reason: "policy", ParentTaskID: S(sh.repair, "id"),
		DependsOn: []string{S(sh.repair, "id"), S(sh.review, "id")}, WorkspaceMode: "collection", Depth: 3, AllowDedupe: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The repair's own verifies edge is the normal wiring that DID happen; only the
	// original->review carry is missing (that is the whole point of the sweep).
	if err := sh.s.AddEdge(S(sh.repair, "id"), S(fresh, "id"), S(sh.c, "id"), "verifies"); err != nil {
		t.Fatal(err)
	}
	if _, err := sh.s.SubmitResult(S(fresh, "id"), Row{"status": "complete", "summary": "repaired", "verdict": "pass", "impact_enumeration": passingImpactEnumeration()}); err != nil {
		t.Fatal(err)
	}
	if err := sh.s.SetTaskStatus(S(fresh, "id"), TaskComplete, "fresh review passed"); err != nil {
		t.Fatal(err)
	}
	return fresh
}

func edgeCount(t *testing.T, s *Store, cid string) int {
	t.Helper()
	edges, err := s.ListEdges(cid)
	if err != nil {
		t.Fatal(err)
	}
	return len(edges)
}

// (1) The exact COL-4274d1d8d9 replay: a settled BLOCKED Collection with a coordinator-
// created failed review -> scheduler repair -> a fresh passing review carrying NO edge onto
// the original is healed by ONE reconcile tick: the verifies carry edge appears, coverage is
// empty, the collection flips to COMPLETE, and acceptance validation would pass.
func TestReconcileTickBackfillsSettledRepairLineageToComplete(t *testing.T) {
	sh := buildCoordinatorCreatedRepairShape(t)
	// Pin terminal-flip preconditions: legacy-coherent (pre-cutoff) creation so the
	// artifact planning floors / critic staleness cannot keep the collection RUNNING
	// independent of the sweep, and zero OPEN revisions.
	forceCollectionCreatedAt(t, sh.s, S(sh.c, "id"), preCutoffCreatedAt)
	fresh := addFreshPassingReviewOnRepair(t, sh)
	if err := sh.s.SetTaskStatus(S(sh.coord, "id"), TaskComplete, "planning complete"); err != nil {
		t.Fatal(err)
	}
	// The settled, self-evidently-BLOCKED shape: the original carries no carry edge to the
	// fresh pass review, so coverage is genuinely missing before the sweep.
	if from := verifiesTo(t, sh, S(fresh, "id")); containsString(from, S(sh.impl1, "id")) {
		t.Fatalf("precondition: the original carry edge must be absent before the sweep")
	}
	missing, err := sh.s.ImplementationsMissingPassingReview(S(sh.c, "id"))
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(missing, S(sh.impl1, "id")) {
		t.Fatalf("precondition: the original must be reported uncovered before the sweep: %v", missing)
	}

	if state := backfillReconcileSched(sh.s).reconcileCollectionSafely(S(sh.c, "id")); state != "COMPLETE" {
		t.Fatalf("one reconcile tick must heal the settled lineage to COMPLETE, got %s", state)
	}
	if from := verifiesTo(t, sh, S(fresh, "id")); !containsString(from, S(sh.impl1, "id")) {
		t.Fatalf("the sweep must add the implement->passing-review carry edge onto the original, got %v", from)
	}
	missing, err = sh.s.ImplementationsMissingPassingReview(S(sh.c, "id"))
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 0 {
		t.Fatalf("the backfilled original must satisfy the coverage rule, got %v", missing)
	}
	evts := collectionEventsFor(t, sh.s, sh.c, "REPAIR_LINEAGE_BACKFILLED")
	if len(evts) != 1 {
		t.Fatalf("the sweep must emit exactly one backfill event, got %d", len(evts))
	}
	if _, _, err := validateCollectionForAcceptance(sh.s, sh.c); err != nil {
		t.Fatalf("a healed collection must pass acceptance validation, got %v", err)
	}
}

// (2) Byte-idempotence: a SECOND reconcile tick and a fresh Scheduler over the same store
// (simulating a daemon restart) add zero edges and emit zero further backfill events. This
// also proves the strict Go-side existing-edge pre-check (AddEdge 'verifies' would otherwise
// churn Dolt on every tick).
func TestRepairLineageBackfillIsIdempotentAcrossTicksAndRestart(t *testing.T) {
	sh := buildCoordinatorCreatedRepairShape(t)
	forceCollectionCreatedAt(t, sh.s, S(sh.c, "id"), preCutoffCreatedAt)
	addFreshPassingReviewOnRepair(t, sh)
	if err := sh.s.SetTaskStatus(S(sh.coord, "id"), TaskComplete, "planning complete"); err != nil {
		t.Fatal(err)
	}
	if state := backfillReconcileSched(sh.s).reconcileCollectionSafely(S(sh.c, "id")); state != "COMPLETE" {
		t.Fatalf("expected COMPLETE after first tick, got %s", state)
	}
	baseEdges := edgeCount(t, sh.s, S(sh.c, "id"))
	baseEvents := len(collectionEventsFor(t, sh.s, sh.c, "REPAIR_LINEAGE_BACKFILLED"))

	// Second tick on the same scheduler.
	if got := backfillReconcileSched(sh.s).reconcileCollectionSafely(S(sh.c, "id")); got != "COMPLETE" {
		t.Fatalf("a second tick must keep the healed collection COMPLETE, got %s", got)
	}
	// Simulated daemon restart: an entirely fresh Scheduler over the same store.
	if got := backfillReconcileSched(sh.s).reconcileCollectionSafely(S(sh.c, "id")); got != "COMPLETE" {
		t.Fatalf("a restart-simulated tick must keep the healed collection COMPLETE, got %s", got)
	}
	if after := edgeCount(t, sh.s, S(sh.c, "id")); after != baseEdges {
		t.Fatalf("repeated ticks must be byte-idempotent on edges: %d -> %d", baseEdges, after)
	}
	if after := len(collectionEventsFor(t, sh.s, sh.c, "REPAIR_LINEAGE_BACKFILLED")); after != baseEvents {
		t.Fatalf("repeated ticks must emit zero further backfill events: %d -> %d", baseEvents, after)
	}
}

// (2b) A healthy post-fix Collection (the original carry edge already wired by the normal
// trigger-time path) produces ZERO backfill events across a tick.
func TestRepairLineageBackfillSilentWhenAlreadyWired(t *testing.T) {
	sh := buildCoordinatorCreatedRepairShape(t)
	// Drive the real trigger-time seam so the carry edge is already present.
	if err := sh.sch.ensureReview(sh.p, sh.c, sh.repair); err != nil {
		t.Fatal(err)
	}
	fresh := freshReviews(t, sh, S(sh.review, "id"))[0]
	if _, err := sh.s.SubmitResult(S(fresh, "id"), Row{"status": "complete", "summary": "repaired", "verdict": "pass", "impact_enumeration": passingImpactEnumeration()}); err != nil {
		t.Fatal(err)
	}
	if err := sh.s.SetTaskStatus(S(fresh, "id"), TaskComplete, "fresh passed"); err != nil {
		t.Fatal(err)
	}
	if err := sh.s.SetTaskStatus(S(sh.coord, "id"), TaskComplete, "planning complete"); err != nil {
		t.Fatal(err)
	}
	if !containsString(verifiesTo(t, sh, S(fresh, "id")), S(sh.impl1, "id")) {
		t.Fatal("the trigger-time seam should already carry the original, so this fixture is not the post-fix shape")
	}
	beforeEdges := edgeCount(t, sh.s, S(sh.c, "id"))
	backfillReconcileSched(sh.s).reconcileCollectionSafely(S(sh.c, "id"))
	if after := edgeCount(t, sh.s, S(sh.c, "id")); after != beforeEdges {
		t.Fatalf("an already-wired collection must gain no edges across a tick: %d -> %d", beforeEdges, after)
	}
	if got := len(collectionEventsFor(t, sh.s, sh.c, "REPAIR_LINEAGE_BACKFILLED")); got != 0 {
		t.Fatalf("an already-wired collection must emit zero backfill events, got %d", got)
	}
}

// (3) Negative no-relaxation proof: a passing review that is NOT in a failed review's repair
// lineage (it depends_on an unrelated implement, not the failed review) gains no verifies
// edge, so the sweep never lets a plain depends_on chain count as coverage.
func TestRepairLineageBackfillIgnoresPassingReviewOutsideLineage(t *testing.T) {
	s, cfg := newFakeStore(t)
	p, err := s.RegisterProject(testProjectConfig(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	c, coord, err := s.CreateCollectionWithCoordinator(S(p, "id"), "Lineage non-membership", "Add feature", "Plan")
	if err != nil {
		t.Fatal(err)
	}
	forceCollectionCreatedAt(t, s, S(c, "id"), preCutoffCreatedAt)
	original, _, err := s.CreateTask(CreateTaskOptions{ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "implement", Title: "Original", Objective: "o", Reason: "r", ParentTaskID: S(coord, "id"), WorkspaceMode: "collection", Depth: 1})
	if err != nil {
		t.Fatal(err)
	}
	failed, _, err := s.CreateTask(CreateTaskOptions{ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "review", Title: "Failed", Objective: "o", Reason: "r", ParentTaskID: S(coord, "id"), DependsOn: []string{S(original, "id")}, WorkspaceMode: "collection", Depth: 2, AllowDedupe: false})
	if err != nil {
		t.Fatal(err)
	}
	// A separate passing review that depends_on an UNRELATED implement and never on the
	// failed review: it is outside the repair lineage and must gain no carry edge.
	unrelated, _, err := s.CreateTask(CreateTaskOptions{ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "implement", Title: "Unrelated impl", Objective: "o", Reason: "r", ParentTaskID: S(coord, "id"), WorkspaceMode: "collection", Depth: 1})
	if err != nil {
		t.Fatal(err)
	}
	passing, _, err := s.CreateTask(CreateTaskOptions{ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "review", Title: "Outsider pass", Objective: "o", Reason: "r", ParentTaskID: S(coord, "id"), DependsOn: []string{S(unrelated, "id")}, WorkspaceMode: "collection", Depth: 2, AllowDedupe: false})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SubmitResult(S(failed, "id"), Row{"status": "complete", "summary": "defect", "verdict": "fail"}); err != nil {
		t.Fatal(err)
	}
	_ = s.SetTaskStatus(S(original, "id"), TaskComplete, "impl")
	_ = s.SetTaskStatus(S(failed, "id"), TaskComplete, "semantic fail")
	_ = s.SetTaskStatus(S(unrelated, "id"), TaskComplete, "impl")
	if _, err := s.SubmitResult(S(passing, "id"), Row{"status": "complete", "summary": "ok", "verdict": "pass", "impact_enumeration": passingImpactEnumeration()}); err != nil {
		t.Fatal(err)
	}
	_ = s.SetTaskStatus(S(passing, "id"), TaskComplete, "pass")
	_ = s.SetTaskStatus(S(coord, "id"), TaskComplete, "coord")
	beforeEdges := edgeCount(t, s, S(c, "id"))
	sched := newFloorScheduler(t, s, cfg)
	sched.reconcileCollectionSafely(S(c, "id"))
	if after := edgeCount(t, s, S(c, "id")); after != beforeEdges {
		t.Fatalf("a passing review outside a repair lineage must gain no verifies edge: %d -> %d", beforeEdges, after)
	}
	if got := collectionEventsFor(t, s, c, "REPAIR_LINEAGE_BACKFILLED"); len(got) != 0 {
		t.Fatalf("an out-of-lineage passing review must produce no backfill event, got %d", len(got))
	}
}

// (4) Multi-generation repair chains reach a fixed point: in the realistic shape where an
// intermediate fresh review was created by the trigger-time seam (and therefore already
// carries verifies edges onto the ORIGINAL), the daemon dying mid-wiring on the FINAL review
// still leaves the second-generation passing review recoverable — the shared resolver's
// verifies scan on the intermediate failed review reconstructs the original carry, exactly
// as ensureReview would have.
func TestRepairLineageBackfillReachesSecondGenerationCarry(t *testing.T) {
	sh := buildCoordinatorCreatedRepairShape(t)
	// First generation settles with a FAILING fresh review that spawns a second repair.
	if err := sh.sch.ensureReview(sh.p, sh.c, sh.repair); err != nil {
		t.Fatal(err)
	}
	review2 := freshReviews(t, sh, S(sh.review, "id"))[0]
	if _, err := sh.s.SubmitResult(S(review2, "id"), Row{"status": "complete", "summary": "still broken", "verdict": "fail"}); err != nil {
		t.Fatal(err)
	}
	if err := sh.sch.handleFailedReview(sh.p, review2); err != nil {
		t.Fatal(err)
	}
	var repair2 Row
	tasks, _ := sh.s.ListTasks(S(sh.c, "id"))
	for _, task := range tasks {
		if S(task, "profile") == "implement" && S(task, "parent_task_id") == S(review2, "id") {
			repair2 = task
		}
	}
	if repair2 == nil {
		t.Fatal("a failing second-generation review must spawn a repair")
	}
	if err := sh.s.SetTaskStatus(S(review2, "id"), TaskComplete, "gen-2 review failed"); err != nil {
		t.Fatal(err)
	}
	if err := sh.s.SetTaskStatus(S(repair2, "id"), TaskComplete, "gen-2 repair integrated"); err != nil {
		t.Fatal(err)
	}
	repair2, _ = sh.s.GetTask(S(repair2, "id"))
	// The FINAL review is created and passes, but the daemon dies before writing the
	// original carry edge onto it.
	if err := sh.sch.ensureReview(sh.p, sh.c, repair2); err != nil {
		t.Fatal(err)
	}
	fresh3 := freshReviews(t, sh, S(sh.review, "id"), S(review2, "id"))[0]
	if !containsString(verifiesTo(t, sh, S(fresh3, "id")), S(sh.impl1, "id")) {
		t.Fatal("precondition: the trigger-time seam should have carried the original onto the third review")
	}
	if err := sh.s.Mutate("DELETE FROM task_edges WHERE from_task_id="+SQLLiteral(S(sh.impl1, "id"))+" AND to_task_id="+SQLLiteral(S(fresh3, "id"))+" AND relation='verifies'", "simulate mid-wiring crash"); err != nil {
		t.Fatal(err)
	}
	if containsString(verifiesTo(t, sh, S(fresh3, "id")), S(sh.impl1, "id")) {
		t.Fatal("precondition: the original carry edge must be gone before the sweep")
	}
	if _, err := sh.s.SubmitResult(S(fresh3, "id"), Row{"status": "complete", "summary": "repaired", "verdict": "pass", "impact_enumeration": passingImpactEnumeration()}); err != nil {
		t.Fatal(err)
	}
	if err := sh.s.SetTaskStatus(S(fresh3, "id"), TaskComplete, "gen-3 review passed"); err != nil {
		t.Fatal(err)
	}
	if _, err := sh.sch.backfillRepairLineageCarryEdges(S(sh.c, "id")); err != nil {
		t.Fatal(err)
	}
	if from := verifiesTo(t, sh, S(fresh3, "id")); !containsString(from, S(sh.impl1, "id")) {
		t.Fatalf("the fixed-point sweep must restore the second-generation original carry, got %v", from)
	}
}

// The sweep must not be reachable through a renamed/moved seam: guard the invariant that
// the carry rule stays a single shared resolver reused by both ensureReview and reconcile.
func TestRepairLineageBackfillKeepsSharedResolverAndCoverageRule(t *testing.T) {
	schedSrc, err := os.ReadFile("scheduler.go")
	if err != nil {
		t.Fatal(err)
	}
	storeSrc, err := os.ReadFile("store.go")
	if err != nil {
		t.Fatal(err)
	}
	sched := string(schedSrc)
	if !strings.Contains(sched, "func (s *Scheduler) backfillRepairLineageCarryEdges(") {
		t.Fatal("the reconcile sweep seam must exist")
	}
	if strings.Count(sched, "repairOriginalImplementations(") < 3 {
		t.Fatal("the original-set resolver must be shared by ensureReview and the sweep (definition + both call sites)")
	}
	if !strings.Contains(string(storeSrc), "func (s *Store) ImplementationsMissingPassingReview(") {
		t.Fatal("ImplementationsMissingPassingReview must remain the single coverage rule")
	}
}
