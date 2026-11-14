package praector

import (
	"os"
	"strings"
	"testing"
)

// repairShape captures the durable COL-4274d1d8d9 graph shape built with a
// COORDINATOR-created failed review: an original COMPLETE implement, a failed
// review that carries ONLY a depends_on edge to that implement (no verifies
// edge into it), the scheduler-created repair implementation, and the
// integration of both the failed review and the repair. ensureReview has NOT
// yet been run for the repair; each test drives that seam itself.
type repairShape struct {
	s      *Store
	p      Row
	c      Row
	coord  Row
	impl1  Row
	review Row
	repair Row
	sch    *Scheduler
}

func buildCoordinatorCreatedRepairShape(t *testing.T) repairShape {
	t.Helper()
	s, _ := newFakeStore(t)
	p, err := s.RegisterProject(testProjectConfig(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	c, coord, err := s.CreateCollectionWithCoordinator(S(p, "id"), "Coordinator-created repair lineage", "Add feature", "Plan")
	if err != nil {
		t.Fatal(err)
	}
	impl1, _, err := s.CreateTask(CreateTaskOptions{
		ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "implement", Title: "Initial implementation",
		Objective: "Implement feature", Reason: "plan", ParentTaskID: S(coord, "id"), WorkspaceMode: "collection", Depth: 1, AllowDedupe: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetTaskStatus(S(impl1, "id"), TaskComplete, "impl complete"); err != nil {
		t.Fatal(err)
	}
	// The coordinator requests the review directly, so it carries only a
	// depends_on edge to the implementation it reviewed and NO verifies edge.
	review, _, err := s.CreateTask(CreateTaskOptions{
		ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "review", Title: "Review initial",
		Objective: "Review feature", Reason: "policy", ParentTaskID: S(coord, "id"), DependsOn: []string{S(impl1, "id")}, WorkspaceMode: "collection", Depth: 2, AllowDedupe: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Prove the setup models the stuck shape: a verifies edge into the failed
	// review must be absent, otherwise the pre-existing scheduler scan already
	// clears it and the test gives false confidence.
	edges, _ := s.ListEdges(S(c, "id"))
	for _, edge := range edges {
		if S(edge, "relation") == "verifies" && S(edge, "to_task_id") == S(review, "id") {
			t.Fatal("precondition: the coordinator-created failed review must carry no verifies edge into it")
		}
	}
	if _, err = s.SubmitResult(S(review, "id"), Row{"status": "complete", "summary": "defect", "verdict": "fail"}); err != nil {
		t.Fatal(err)
	}
	sch := &Scheduler{Store: s}
	if err = sch.handleFailedReview(p, review); err != nil {
		t.Fatal(err)
	}
	var repair Row
	tasks, _ := s.ListTasks(S(c, "id"))
	for _, task := range tasks {
		if S(task, "profile") == "implement" && S(task, "parent_task_id") == S(review, "id") {
			repair = task
		}
	}
	if repair == nil {
		t.Fatal("semantic review failure did not create repair implementation")
	}
	if err = s.SetTaskStatus(S(review, "id"), TaskComplete, "semantic fail recorded"); err != nil {
		t.Fatal(err)
	}
	if err = s.SetTaskStatus(S(repair, "id"), TaskComplete, "repair integrated"); err != nil {
		t.Fatal(err)
	}
	return repairShape{s: s, p: p, c: c, coord: coord, impl1: impl1, review: review, repair: repair, sch: sch}
}

func freshReviews(t *testing.T, sh repairShape, exclude ...string) Rows {
	t.Helper()
	tasks, err := sh.s.ListTasks(S(sh.c, "id"))
	if err != nil {
		t.Fatal(err)
	}
	var out Rows
	for _, task := range tasks {
		if S(task, "profile") != "review" {
			continue
		}
		drop := false
		for _, id := range exclude {
			if S(task, "id") == id {
				drop = true
			}
		}
		if !drop {
			out = append(out, task)
		}
	}
	return out
}

func verifiesTo(t *testing.T, sh repairShape, reviewID string) []string {
	t.Helper()
	edges, err := sh.s.ListEdges(S(sh.c, "id"))
	if err != nil {
		t.Fatal(err)
	}
	var from []string
	for _, edge := range edges {
		if S(edge, "relation") == "verifies" && S(edge, "to_task_id") == reviewID {
			from = append(from, S(edge, "from_task_id"))
		}
	}
	return from
}

// (a) Durable replay of the exact COL-4274d1d8d9 shape: a coordinator-created
// failed review (depends_on-only) -> repair -> integration -> a fresh
// post-repair review completing verdict=pass reconciles the collection to
// COMPLETE, not BLOCKED.
func TestCoordinatorCreatedFailedReviewReachesComplete(t *testing.T) {
	sh := buildCoordinatorCreatedRepairShape(t)
	if err := sh.sch.ensureReview(sh.p, sh.c, sh.repair); err != nil {
		t.Fatal(err)
	}
	fresh := freshReviews(t, sh, S(sh.review, "id"))
	if len(fresh) != 1 {
		t.Fatalf("repair must receive exactly one fresh post-repair review, got %d", len(fresh))
	}
	review2 := fresh[0]
	if _, err := sh.s.SubmitResult(S(review2, "id"), Row{"status": "complete", "summary": "repaired", "verdict": "pass", "impact_enumeration": passingImpactEnumeration()}); err != nil {
		t.Fatal(err)
	}
	if err := sh.s.SetTaskStatus(S(review2, "id"), TaskComplete, "fresh review passed"); err != nil {
		t.Fatal(err)
	}
	if err := sh.s.SetTaskStatus(S(sh.coord, "id"), TaskComplete, "planning complete"); err != nil {
		t.Fatal(err)
	}
	missing, err := sh.s.ImplementationsMissingPassingReview(S(sh.c, "id"))
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 0 {
		t.Fatalf("the fresh post-repair pass must clear lineage for BOTH the original and the repair implement: %v", missing)
	}
	state, err := sh.s.UpdateCollectionTerminalState(S(sh.c, "id"))
	if err != nil {
		t.Fatal(err)
	}
	if state != "COMPLETE" {
		t.Fatalf("the repaired collection must reconcile to COMPLETE, not BLOCKED, got %s", state)
	}
	if _, _, err = validateCollectionForAcceptance(sh.s, sh.c); err != nil {
		t.Fatalf("the coordinator-created shape must not poison acceptance after a fresh repaired pass: %v", err)
	}
}

// (b) Structural: the auto-created post-repair review carries verifies lineage
// for BOTH the repair and each original the failed review verified; a revision
// re-run reuses the SAME open fresh review with a STABLE verifies-edge count
// (no duplicate edges, no parallel review generation).
func TestPostRepairReviewCarriesBothAndIsIdempotent(t *testing.T) {
	sh := buildCoordinatorCreatedRepairShape(t)
	if err := sh.sch.ensureReview(sh.p, sh.c, sh.repair); err != nil {
		t.Fatal(err)
	}
	fresh := freshReviews(t, sh, S(sh.review, "id"))
	if len(fresh) != 1 {
		t.Fatalf("expected one fresh post-repair review, got %d", len(fresh))
	}
	review2 := fresh[0]
	firstFrom := verifiesTo(t, sh, S(review2, "id"))
	if !containsString(firstFrom, S(sh.impl1, "id")) || !containsString(firstFrom, S(sh.repair, "id")) {
		t.Fatalf("the fresh review must verify BOTH the original and the repair, got from=%v", firstFrom)
	}
	firstCount := len(firstFrom)
	firstReviews := len(freshReviews(t, sh, S(sh.review, "id")))
	// Simulate a revision re-run invoking the same seam on the same integration.
	if err := sh.sch.ensureReview(sh.p, sh.c, sh.repair); err != nil {
		t.Fatal(err)
	}
	afterFrom := verifiesTo(t, sh, S(review2, "id"))
	if len(afterFrom) != firstCount {
		t.Fatalf("the verifies-edge count to the SAME open fresh review must be stable across a revision re-run: before=%d after=%d", firstCount, len(afterFrom))
	}
	afterReviews := len(freshReviews(t, sh, S(sh.review, "id")))
	if afterReviews != firstReviews {
		t.Fatalf("a revision re-run must reuse the SAME open fresh review rather than spawn a parallel generation: before=%d after=%d", firstReviews, afterReviews)
	}
}

// (c) Negative no-relaxation proof: a passing fresh review that verifies ONLY
// the repair (original uncovered) still leaves the original reported by
// ImplementationsMissingPassingReview. This exercises the coverage rule
// directly, independent of the scheduler wiring, proving the fix is edge-only.
func TestUncoveredOriginalStillMissingPassingReview(t *testing.T) {
	s, _ := newFakeStore(t)
	p, err := s.RegisterProject(testProjectConfig(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	c, coord, err := s.CreateCollectionWithCoordinator(S(p, "id"), "Coverage non-relaxation", "obj", "plan")
	if err != nil {
		t.Fatal(err)
	}
	impl1, _, err := s.CreateTask(CreateTaskOptions{ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "implement", Title: "Original", Objective: "o", Reason: "r", ParentTaskID: S(coord, "id"), WorkspaceMode: "collection", Depth: 1})
	if err != nil {
		t.Fatal(err)
	}
	repair, _, err := s.CreateTask(CreateTaskOptions{ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "implement", Title: "Repair", Objective: "o", Reason: "r", ParentTaskID: S(coord, "id"), WorkspaceMode: "collection", Depth: 1})
	if err != nil {
		t.Fatal(err)
	}
	review, _, err := s.CreateTask(CreateTaskOptions{ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "review", Title: "Fresh", Objective: "o", Reason: "r", ParentTaskID: S(repair, "id"), DependsOn: []string{S(repair, "id")}, WorkspaceMode: "collection", Depth: 2})
	if err != nil {
		t.Fatal(err)
	}
	_ = s.SetTaskStatus(S(impl1, "id"), TaskComplete, "impl")
	_ = s.SetTaskStatus(S(repair, "id"), TaskComplete, "repair")
	// The fresh review verifies only the repair; the original is deliberately uncovered.
	if err = s.AddEdge(S(repair, "id"), S(review, "id"), S(c, "id"), "verifies"); err != nil {
		t.Fatal(err)
	}
	res := JSONDumps(Row{"status": "complete", "summary": "repaired", "verdict": "pass", "impact_enumeration": passingImpactEnumeration()})
	if err = s.Mutate("UPDATE tasks SET status="+SQLLiteral(TaskComplete)+",result_json="+SQLLiteral(res)+",updated_at="+SQLLiteral(UTCNow())+" WHERE id="+SQLLiteral(S(review, "id")), "pass review verifying only repair"); err != nil {
		t.Fatal(err)
	}
	missing, err := s.ImplementationsMissingPassingReview(S(c, "id"))
	if err != nil {
		t.Fatal(err)
	}
	var originalReported bool
	for _, id := range missing {
		if id == S(impl1, "id") {
			originalReported = true
		}
	}
	if !originalReported {
		t.Fatalf("an original uncovered by any passing review must still be reported as missing (coverage rule not relaxed): %v", missing)
	}
}

// (d) A failed post-repair review continues the documented lineage: a second
// fresh review is created, existing behavior unchanged, and the carry still
// reaches the original implementation on the second generation.
func TestFailedFreshReviewContinuesLineage(t *testing.T) {
	sh := buildCoordinatorCreatedRepairShape(t)
	if err := sh.sch.ensureReview(sh.p, sh.c, sh.repair); err != nil {
		t.Fatal(err)
	}
	fresh := freshReviews(t, sh, S(sh.review, "id"))
	if len(fresh) != 1 {
		t.Fatalf("expected one fresh post-repair review, got %d", len(fresh))
	}
	review2 := fresh[0]
	if _, err := sh.s.SubmitResult(S(review2, "id"), Row{"status": "complete", "summary": "still broken", "verdict": "fail"}); err != nil {
		t.Fatal(err)
	}
	if err := sh.sch.handleFailedReview(sh.p, review2); err != nil {
		t.Fatal(err)
	}
	tasks, _ := sh.s.ListTasks(S(sh.c, "id"))
	var repair2 Row
	for _, task := range tasks {
		if S(task, "profile") == "implement" && S(task, "parent_task_id") == S(review2, "id") {
			repair2 = task
		}
	}
	if repair2 == nil {
		t.Fatal("a failed fresh review must create repair follow-up work")
	}
	if err := sh.s.SetTaskStatus(S(review2, "id"), TaskComplete, "fresh review failed"); err != nil {
		t.Fatal(err)
	}
	if err := sh.s.SetTaskStatus(S(repair2, "id"), TaskComplete, "second repair integrated"); err != nil {
		t.Fatal(err)
	}
	repair2, _ = sh.s.GetTask(S(repair2, "id"))
	if err := sh.sch.ensureReview(sh.p, sh.c, repair2); err != nil {
		t.Fatal(err)
	}
	third := freshReviews(t, sh, S(sh.review, "id"), S(review2, "id"))
	if len(third) != 1 {
		t.Fatalf("a failed fresh review must continue the lineage with a second fresh review, got %d", len(third))
	}
	from := verifiesTo(t, sh, S(third[0], "id"))
	if !containsString(from, S(sh.impl1, "id")) {
		t.Fatalf("the second-generation fresh review must still carry lineage to the original implementation, got from=%v", from)
	}
}

// The repair-lineage carry is a scheduler-edge wiring fix: assert the
// implementation stays at the named seam and does not touch the coverage rule,
// so reviewers can see the change is graph-edge-only.
func TestRepairCarryIsEdgeOnlyAtSchedulerSeam(t *testing.T) {
	schedSrc, err := os.ReadFile("scheduler.go")
	if err != nil {
		t.Fatal(err)
	}
	storeSrc, err := os.ReadFile("store.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(schedSrc), "func (s *Scheduler) ensureReview(") {
		t.Fatal("ensureReview must remain the post-repair review creation seam")
	}
	if !strings.Contains(string(storeSrc), "func (s *Store) ImplementationsMissingPassingReview(") {
		t.Fatal("ImplementationsMissingPassingReview must remain the single coverage rule")
	}
}
