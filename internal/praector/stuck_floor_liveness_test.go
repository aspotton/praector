package praector

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// Stuck-floor liveness escalation (Scheduler.observeStuckFloors). A floor-held READY
// implement that never releases, or a BLOCKED collection with no active work, would
// otherwise repeat an identical refusal forever while crash recovery (RUNNING attempts)
// and attempt budgets (FAILED tasks) stay silent. The sweep raises exactly one
// deduplicated non-blocking Coordinator attention. The authoritative seams are
// Store.ClaimTask (which labels the refusal PREFLIGHT_FLOOR_HELD / PLAN_REVIEW_FLOOR_HELD)
// and Scheduler.observeStuckFloors (the scheduler escalation step run from
// reconcileCollection), driven here directly against the durable store.

// forceFloorHeld re-claims a floor-held implement n times, so ClaimTask re-emits its
// floor-held label exactly as the daemon re-attempts it every poll cycle (RunnableTasks
// does not apply the floor gate). n is the number of accumulated refusals.
func forceFloorHeld(t *testing.T, s *Store, impl Row, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, _, err := s.ClaimTask(S(impl, "id"), 3); err != nil {
			t.Fatal(err)
		}
	}
}

func coordinatorAttentionTasks(t *testing.T, s *Store, c Row) Rows {
	t.Helper()
	rows, err := s.ListTasks(S(c, "id"))
	if err != nil {
		t.Fatal(err)
	}
	out := Rows{}
	for _, r := range rows {
		if S(r, "profile") == "coordinator" && S(r, "title") == "Coordinator attention" {
			out = append(out, r)
		}
	}
	return out
}

// (a) A gate-free coherent collection whose implement is floor-held with no graph
// movement raises exactly ONE deduplicated Coordinator attention past the threshold and
// NOT again while the graph state is unchanged.
func TestStuckFloorHeldImplementRaisesExactlyOneAttention(t *testing.T) {
	assertFloorObjectiveAllFalse(t)
	s, cfg, c, coord := gateFreeFloorCollection(t, floorObjective)
	impl := floorImplement(t, s, c, coord)
	if err := s.SetTaskStatus(S(coord, "id"), TaskComplete, "coordinator handed off"); err != nil {
		t.Fatal(err)
	}
	sched := newFloorScheduler(t, s, cfg)
	// Below the threshold: still silent.
	forceFloorHeld(t, s, impl, stuckFloorRefusalThreshold-1)
	sched.observeStuckFloors(S(c, "id"))
	if got := coordinatorAttentionTasks(t, s, c); len(got) != 0 {
		t.Fatalf("must not raise attention below the threshold, got %d", len(got))
	}
	// One more refusal crosses the threshold with no graph movement.
	forceFloorHeld(t, s, impl, 1)
	sched.observeStuckFloors(S(c, "id"))
	first := coordinatorAttentionTasks(t, s, c)
	if len(first) != 1 {
		t.Fatalf("a floor-held, graph-stuck implement must raise exactly one attention, got %d", len(first))
	}
	if ev := collectionEventsFor(t, s, c, "STUCK_FLOOR_ATTENTION"); len(ev) != 1 {
		t.Fatalf("the stuck-floor dedup marker must be emitted once, got %d", len(ev))
	}
	// Re-running the sweep on the unchanged graph must NOT stack another attention.
	sched.observeStuckFloors(S(c, "id"))
	sched.observeStuckFloors(S(c, "id"))
	if got := coordinatorAttentionTasks(t, s, c); len(got) != 1 {
		t.Fatalf("an unchanged stuck graph must never re-storm attention, got %d", len(got))
	}
	// The reason text cites the stuck task id, the hold kind, and the no-movement claim.
	reason := S(first[0], "reason")
	if reason == "" {
		reason = S(first[0], "objective")
	}
	for _, want := range []string{S(impl, "id"), "floor", "movement"} {
		if !strings.Contains(strings.ToLower(reason), strings.ToLower(want)) {
			t.Fatalf("attention reason must cite %q; got %q", want, reason)
		}
	}
}

// (b1) A real graph-movement event after the last refusal suppresses the rule.
func TestStuckFloorGraphMovementSuppressesAttention(t *testing.T) {
	assertFloorObjectiveAllFalse(t)
	s, cfg, c, coord := gateFreeFloorCollection(t, floorObjective)
	impl := floorImplement(t, s, c, coord)
	if err := s.SetTaskStatus(S(coord, "id"), TaskComplete, "coordinator handed off"); err != nil {
		t.Fatal(err)
	}
	sched := newFloorScheduler(t, s, cfg)
	forceFloorHeld(t, s, impl, stuckFloorRefusalThreshold+1)
	// Create a mover BEFORE the refusals, then, AFTER the last floor-held label, drive a
	// real status transition on it. That transition is the LAST event, so it sits strictly
	// after the last refusal and counts as graph movement, suppressing the rule.
	mover, _, err := s.CreateTask(CreateTaskOptions{ProjectID: S(c, "project_id"), CollectionID: S(c, "id"), Profile: "advise", Title: "Mover", Objective: "graph mover", Reason: "movement", ParentTaskID: S(coord, "id"), WorkspaceMode: "collection", Depth: 1, AllowDedupe: false})
	if err != nil {
		t.Fatal(err)
	}
	forceFloorHeld(t, s, impl, 1) // last event is now a floor-held label
	if err := s.SetTaskStatus(S(mover, "id"), TaskComplete, "mover completes after the last refusal"); err != nil {
		t.Fatal(err)
	}
	sched.observeStuckFloors(S(c, "id"))
	if got := coordinatorAttentionTasks(t, s, c); len(got) != 0 {
		t.Fatalf("a real status-transition event after the last refusal is movement and must suppress attention, got %d", len(got))
	}
}

// (b2) A successful later claim resets/suppresses: a now-RUNNING implement is not stuck.
func TestStuckFloorSuccessfulClaimSuppressesAttention(t *testing.T) {
	assertFloorObjectiveAllFalse(t)
	s, cfg, c, coord := gateFreeFloorCollection(t, floorObjective)
	impl := floorImplement(t, s, c, coord)
	if err := s.SetTaskStatus(S(coord, "id"), TaskComplete, "coordinator handed off"); err != nil {
		t.Fatal(err)
	}
	sched := newFloorScheduler(t, s, cfg)
	forceFloorHeld(t, s, impl, stuckFloorRefusalThreshold+1)
	// Release the floors (passing review + terminal generation preflights) and claim.
	planReview, _, err := s.CreateTask(CreateTaskOptions{ProjectID: S(c, "project_id"), CollectionID: S(c, "id"), Profile: "plan_review", Title: "Review proposed work graph", Objective: "gate", Reason: "plan", ParentTaskID: S(coord, "id"), WorkspaceMode: "collection", Depth: 1, AllowDedupe: false})
	if err != nil {
		t.Fatal(err)
	}
	completePlanReview(t, s, planReview, "pass")
	// Stage-and-complete the generation preflights by hand so the preflight floor also
	// releases, then the claim succeeds and the task leaves READY.
	for _, p := range []string{"explore", "critic"} {
		pt, _, e := s.CreateTask(CreateTaskOptions{ProjectID: S(c, "project_id"), CollectionID: S(c, "id"), Profile: p, Title: "gen " + p, Objective: p, Reason: "preflight", ParentTaskID: S(coord, "id"), RequestedByTaskID: S(coord, "id"), WorkspaceMode: "collection", Depth: 1, AllowDedupe: false})
		if e != nil {
			t.Fatal(e)
		}
		if e := s.SetTaskStatus(S(pt, "id"), TaskComplete, "preflight complete"); e != nil {
			t.Fatal(e)
		}
	}
	claimed, _, err := s.ClaimTask(S(impl, "id"), 3)
	if err != nil {
		t.Fatal(err)
	}
	if claimed == nil || S(claimed, "id") != S(impl, "id") {
		t.Fatal("the claim must succeed once both floors release")
	}
	sched.observeStuckFloors(S(c, "id"))
	if got := coordinatorAttentionTasks(t, s, c); len(got) != 0 {
		t.Fatalf("a successfully-claimed (no-longer-READY) implement must not raise attention, got %d", len(got))
	}
}

// (b3) A legitimately WAITING prerequisite never triggers the rule.
func TestStuckFloorWaitingPrerequisiteSuppressesAttention(t *testing.T) {
	assertFloorObjectiveAllFalse(t)
	s, cfg, c, coord := gateFreeFloorCollection(t, floorObjective)
	impl := floorImplement(t, s, c, coord)
	if err := s.SetTaskStatus(S(coord, "id"), TaskComplete, "coordinator handed off"); err != nil {
		t.Fatal(err)
	}
	sched := newFloorScheduler(t, s, cfg)
	// A WAITING task the implement depends on means the graph may still move.
	prereq, _, err := s.CreateTask(CreateTaskOptions{ProjectID: S(c, "project_id"), CollectionID: S(c, "id"), Profile: "advise", Title: "Prereq", Objective: "waiting prerequisite", Reason: "wait", ParentTaskID: S(coord, "id"), WorkspaceMode: "collection", Depth: 1, AllowDedupe: false})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetTaskStatus(S(prereq, "id"), TaskWaiting, "prereq waiting"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddEdge(S(prereq, "id"), S(impl, "id"), S(c, "id"), "depends_on"); err != nil {
		t.Fatal(err)
	}
	forceFloorHeld(t, s, impl, stuckFloorRefusalThreshold+1)
	sched.observeStuckFloors(S(c, "id"))
	if got := coordinatorAttentionTasks(t, s, c); len(got) != 0 {
		t.Fatalf("a WAITING prerequisite means the graph may move; must not raise attention, got %d", len(got))
	}
}

// (c) Self-reference proof: repeated *_FLOOR_HELD label events ALONE allow the rule to
// fire (labels are not movement), and a real status-transition event DOES count.
func TestStuckFloorLabelsAreNotMovementButStatusTransitionIs(t *testing.T) {
	assertFloorObjectiveAllFalse(t)
	s, cfg, c, coord := gateFreeFloorCollection(t, floorObjective)
	impl := floorImplement(t, s, c, coord)
	if err := s.SetTaskStatus(S(coord, "id"), TaskComplete, "coordinator handed off"); err != nil {
		t.Fatal(err)
	}
	sched := newFloorScheduler(t, s, cfg)
	// Only floor-held labels accumulate -> the rule fires (labels are not movement).
	forceFloorHeld(t, s, impl, stuckFloorRefusalThreshold)
	sched.observeStuckFloors(S(c, "id"))
	if got := coordinatorAttentionTasks(t, s, c); len(got) != 1 {
		t.Fatalf("floor-held labels alone must allow the rule (labels are not movement); got %d attention", len(got))
	}
}

// (d) Collection-level twin: a BLOCKED collection whose graph has no nonterminal or
// blocking task (a COL-4274d1d8d9-shaped all-COMPLETE graph) is flagged exactly once,
// and the independent-passing-review coverage rule is NOT relaxed (ImplementationsMissingPassingReview
// still reports the uncovered implement — the detection is orthogonal to that gate).
func TestStuckFloorCollectionTwinFlagsBlockedNoWorkOnce(t *testing.T) {
	s, cfg, c, coord := gateFreeFloorCollection(t, colReplayObjective)
	impl := floorImplement(t, s, c, coord)
	review, _, err := s.CreateTask(CreateTaskOptions{ProjectID: S(c, "project_id"), CollectionID: S(c, "id"), Profile: "review", Title: "Independent review", Objective: "review", Reason: "review", ParentTaskID: S(coord, "id"), DependsOn: []string{S(impl, "id")}, WorkspaceMode: "collection", Depth: 1, AllowDedupe: false})
	if err != nil {
		t.Fatal(err)
	}
	// Terminal everything: coordinator + implement COMPLETE, a passing review that
	// carries NO verifies edge back to the implement (the DEFERRED root cause), then the
	// collection reads BLOCKED with no nonterminal/blocking task.
	if err := s.SetTaskStatus(S(coord, "id"), TaskComplete, "coord complete"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTaskStatus(S(impl, "id"), TaskComplete, "impl complete"); err != nil {
		t.Fatal(err)
	}
	res := JSONDumps(Row{"status": "complete", "summary": "reviewed", "verdict": "pass"})
	if err := s.Mutate("UPDATE tasks SET status="+SQLLiteral(TaskComplete)+",result_json="+SQLLiteral(res)+",updated_at="+SQLLiteral(UTCNow())+" WHERE id="+SQLLiteral(S(review, "id")), "pass review without verifies edge"); err != nil {
		t.Fatal(err)
	}
	// The coverage rule must still see the implement as uncovered (we did NOT relax it).
	uncovered, err := s.ImplementationsMissingPassingReview(S(c, "id"))
	if err != nil {
		t.Fatal(err)
	}
	coverageIsOrthogonal := len(uncovered) > 0
	if err := s.SetCollectionStatus(S(c, "id"), "BLOCKED", true); err != nil {
		t.Fatal(err)
	}
	sched := newFloorScheduler(t, s, cfg)
	sched.observeStuckFloors(S(c, "id"))
	first := coordinatorAttentionTasks(t, s, c)
	if len(first) != 1 {
		t.Fatalf("a BLOCKED collection with no nonterminal/blocking task must raise exactly one attention, got %d", len(first))
	}
	// Re-running on the unchanged collection must not storm.
	sched.observeStuckFloors(S(c, "id"))
	if got := coordinatorAttentionTasks(t, s, c); len(got) != 1 {
		t.Fatalf("the collection twin must fire once, not every cycle, got %d", len(got))
	}
	if !coverageIsOrthogonal {
		t.Fatal("the collection-twin detection must not relax ImplementationsMissingPassingReview; the uncovered implement must still be reported")
	}
}

// (e) Structural proof that both artifact-floor layers consume the single named
// generation-title helper, while selectGenerationCoordinator stays a stricter variant.
func TestGenerationIdentitySingleNamedHelper(t *testing.T) {
	storeSrc, err := os.ReadFile("store.go")
	if err != nil {
		t.Fatal(err)
	}
	schedSrc, err := os.ReadFile("scheduler.go")
	if err != nil {
		t.Fatal(err)
	}
	ss, sc := string(storeSrc), string(schedSrc)
	if !strings.Contains(ss, "func generationCoordinatorTitlePredicate()") || !strings.Contains(ss, "AND \" + generationCoordinatorTitlePredicate() + \"") {
		t.Fatal("the store claim-floor clause must consume the single named generationCoordinatorTitlePredicate helper")
	}
	if !strings.Contains(ss, "generationCoordinatorTitles = []string{\"Initial coordination\", \"Acceptance revision\"}") {
		t.Fatal("the generation-title list must live in exactly one named source (generationCoordinatorTitles)")
	}
	// Both scheduler identity sites (staging floor + stricter variant) route through the
	// Go helper; the definition lives in store.go; no inline generation-title literal may
	// remain in scheduler.go.
	if !strings.Contains(ss, "func isGenerationCoordinatorTitle(") {
		t.Fatal("isGenerationCoordinatorTitle must be the single named Go generation-title helper")
	}
	if strings.Count(sc, "isGenerationCoordinatorTitle(") < 2 {
		t.Fatal("both scheduler generation-identity sites (staging floor + stricter variant) must consume isGenerationCoordinatorTitle")
	}
	if strings.Contains(sc, `"Initial coordination" || S(`) || strings.Contains(sc, `== "Acceptance revision"`) {
		t.Fatal("scheduler.go must not retain an inline generation-title equality; it must resolve through the named helper")
	}
	// The stricter variant keeps its extra stableGenerationState condition (not merged).
	if !strings.Contains(sc, "stableGenerationState") {
		t.Fatal("selectGenerationCoordinator must remain a stricter variant (keep its stableGenerationState condition)")
	}
}

// (f) Per research R3: the liveness movement window is decided through the ListEvents
// (created_at,id) exclusive cursor, so a datetime-window check must be exercised against
// the real `dolt sql -r json` seam (DATETIME(6) rendering), not only the sqlite3 fake.
// Skips when the real dolt binary is unavailable (the fake shim is not authoritative).
func TestStuckFloorCursorAgainstRealDoltSeam(t *testing.T) {
	dolt, err := exec.LookPath("dolt")
	if err != nil {
		t.Skip("real dolt binary unavailable; the sqlite3 fake harness is not a DATETIME conformance oracle")
	}
	_ = dolt // NewStore resolves the real dolt from PATH (no fake shim is installed here).
	cfg := GlobalConfig{Home: t.TempDir()}
	s, err := NewStore(cfg)
	if err != nil {
		t.Skipf("real dolt store unavailable: %v", err)
	}
	if err := s.Bootstrap(); err != nil {
		t.Skipf("real dolt bootstrap unavailable: %v", err)
	}
	// Two events with distinct DATETIME(6) created_at in an otherwise-empty synthetic
	// scope (so no collection-bootstrap events pollute the window): an earlier floor-held
	// label and a later status transition. The exclusive cursor on the earlier row's
	// (created_at,id) must return exactly the later event and none of the earlier.
	const pid, cid = "PRJ_DT_WIN", "COL_DT_WIN"
	base := "2026-11-12 10:00:00.000000"
	later := "2026-11-12 10:00:01.000000"
	if err := s.Mutate("INSERT INTO events (id,project_id,collection_id,task_id,attempt_id,event_type,payload_json,created_at) VALUES "+
		"('EVT_A',"+SQLLiteral(pid)+","+SQLLiteral(cid)+",NULL,NULL,'PREFLIGHT_FLOOR_HELD','{}',"+SQLLiteral(base)+"),"+
		"('EVT_B',"+SQLLiteral(pid)+","+SQLLiteral(cid)+",NULL,NULL,'TASK_COMPLETE','{}',"+SQLLiteral(later)+")", "seed events"); err != nil {
		t.Fatal(err)
	}
	after, err := s.ListEvents(pid, cid, "", "", base, "EVT_A", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 || S(after[0], "id") != "EVT_B" {
		t.Fatalf("the (created_at,id) exclusive cursor must return exactly the later event across real-Dolt DATETIME(6) rendering; got %d rows", len(after))
	}
}
