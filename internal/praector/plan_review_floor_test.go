package praector

import "testing"

// Artifact-derived plan-review floor: an implement in a COHERENT (post
// GateCoherenceCutoff) Collection must never be claimable while the Collection
// has no terminal plan_review decision, independent of what the objective-text
// classifier inferred. These are durable-state integration tests: the
// authoritative seam asserted is store.ClaimTask (not a bare RunnableTasks
// query), and the auto-staging is driven through the real scheduler planning
// reconciliation. The motivating failure is COL-85ab51aa9e, whose all-false
// classification let an implement run before its plan review.

const floorObjective = "Analyze and deep dive, see what we would need to change to have a CloudFormation parameter."

func assertFloorObjectiveAllFalse(t *testing.T) {
	t.Helper()
	p := effectivePlanningPolicy(Row{"objective": floorObjective, "created_at": postCutoffCreatedAt})
	if p.ExploreRequired || p.ResearchRequired || p.CriticRequired || p.PlanReviewRequired {
		t.Fatalf("floor objective must classify all four gates false, got %+v", p)
	}
}

func floorImplement(t *testing.T, s *Store, c, coord Row) Row {
	t.Helper()
	impl, _, err := s.CreateTask(CreateTaskOptions{ProjectID: S(c, "project_id"), CollectionID: S(c, "id"), Profile: "implement", Title: "Investigate change", Objective: "Investigate the CloudFormation parameter change.", Reason: "plan", ParentTaskID: S(coord, "id"), WorkspaceMode: "collection", Depth: 1, AllowDedupe: false})
	if err != nil {
		t.Fatal(err)
	}
	return impl
}

// completePlanReview plants a COMPLETE plan_review with the given verdict without
// running its submit-time validators (mirrors directive_traceability_test.go).
func completePlanReview(t *testing.T, s *Store, review Row, verdict string) {
	t.Helper()
	res := JSONDumps(Row{"status": "complete", "summary": "plan reviewed", "verdict": verdict})
	if err := s.Mutate("UPDATE tasks SET status="+SQLLiteral(TaskComplete)+",result_json="+SQLLiteral(res)+",updated_at="+SQLLiteral(UTCNow())+" WHERE id="+SQLLiteral(S(review, "id")), "plant complete plan_review verdict="+verdict); err != nil {
		t.Fatal(err)
	}
}

func floorEventsFor(t *testing.T, s *Store, c, task Row, evType string) Rows {
	t.Helper()
	evts, err := s.ListEvents(S(c, "project_id"), S(c, "id"), S(task, "id"), evType, "", "", 50)
	if err != nil {
		t.Fatal(err)
	}
	return evts
}

// (a) Authoritative floor refusal + auto-staged plan_review + release after pass.
func TestArtifactPlanReviewFloorBlocksClaimAndAutoStagesReview(t *testing.T) {
	assertFloorObjectiveAllFalse(t)
	s, cfg := newFakeStore(t)
	p, err := s.RegisterProject(testProjectConfig(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	c, coord, err := s.CreateCollectionWithCoordinator(S(p, "id"), "CloudFormation probe", floorObjective, "Plan")
	if err != nil {
		t.Fatal(err)
	}
	forceCollectionCreatedAt(t, s, S(c, "id"), postCutoffCreatedAt)
	impl := floorImplement(t, s, c, coord)

	// While no plan_review exists, the authoritative ClaimTask must refuse the
	// implement even though the classifier required nothing. This FAILS against
	// base b84f1f3, where the gate returned true for a not-required collection.
	claimed, _, err := s.ClaimTask(S(impl, "id"), 3)
	if err != nil {
		t.Fatal(err)
	}
	if claimed != nil && S(claimed, "id") == S(impl, "id") {
		t.Fatal("a coherent all-gates-false implement must be unclaimable with no plan_review decision")
	}
	if ev := floorEventsFor(t, s, c, impl, "PLAN_REVIEW_FLOOR_HELD"); len(ev) == 0 {
		t.Fatal("a floor-held claim must emit a dedicated PLAN_REVIEW_FLOOR_HELD audit event")
	}

	// The coordinator hands off (COMPLETE) WITHOUT creating a final review. The
	// planner reconciliation must stage exactly one plan_review on its own.
	if err = s.SetTaskStatus(S(coord, "id"), TaskComplete, "coordinator handed off"); err != nil {
		t.Fatal(err)
	}
	sched, err := NewScheduler(s, cfg, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = sched.enforcePlanningPolicy(S(c, "id")); err != nil {
		t.Fatal(err)
	}
	reviews := planReviewsIn(t, s, c)
	if len(reviews) != 1 {
		t.Fatalf("a write-bearing gate-free collection must auto-stage exactly one plan_review, got %d", len(reviews))
	}

	// Before the staged review is terminal, RunnableTasks must not hand out the
	// implement (the review->implement edge now holds it) and ClaimTask refuses.
	if r, _ := s.RunnableTasks(S(c, "id"), 5); containsTaskID(r, S(impl, "id")) {
		t.Fatal("the implement must be held while the staged plan_review is nonterminal")
	}

	completePlanReview(t, s, reviews[0], "pass")
	claimed2, _, err := s.ClaimTask(S(impl, "id"), 3)
	if err != nil {
		t.Fatal(err)
	}
	if claimed2 == nil || S(claimed2, "id") != S(impl, "id") {
		t.Fatal("a terminal COMPLETE plan_review must release the floor and let the implement claim")
	}
}

// (b) Regression: a policy-required (implementation-classified) collection still
// yields exactly one final plan review with no duplicate/extra generation.
func TestArtifactPlanReviewFloorPolicyRequiredSingleReview(t *testing.T) {
	s, cfg := newFakeStore(t)
	p, err := s.RegisterProject(testProjectConfig(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	c, coord, err := s.CreateCollectionWithCoordinator(S(p, "id"), "Parser test", "Add a local parser test.", "Plan")
	if err != nil {
		t.Fatal(err)
	}
	forceCollectionCreatedAt(t, s, S(c, "id"), postCutoffCreatedAt)
	impl, _, err := s.CreateTask(CreateTaskOptions{ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "implement", Title: "Add parser test", Objective: "Add a parser unit test.", Reason: "plan", ParentTaskID: S(coord, "id"), WorkspaceMode: "collection", Depth: 1, AllowDedupe: false})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetTaskStatus(S(coord, "id"), TaskComplete, "coordinator handed off"); err != nil {
		t.Fatal(err)
	}
	sched, err := NewScheduler(s, cfg, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = sched.enforcePlanningPolicy(S(c, "id")); err != nil {
		t.Fatal(err)
	}
	first := planReviewsIn(t, s, c)
	if len(first) != 1 {
		t.Fatalf("a policy-required collection must stage exactly one final plan review, got %d", len(first))
	}
	if _, err = sched.enforcePlanningPolicy(S(c, "id")); err != nil {
		t.Fatal(err)
	}
	if second := planReviewsIn(t, s, c); len(second) != 1 {
		t.Fatalf("reconciling again must not generate a second/collapsed plan review, got %d", len(second))
	}
	_ = impl
}

// (c) A WAITING-only coordinator (control already handed off) no longer
// suppresses final-plan-review staging: exactly one review is staged.
func TestArtifactPlanReviewFloorWaitingCoordinatorStagesOneReview(t *testing.T) {
	assertFloorObjectiveAllFalse(t)
	s, cfg := newFakeStore(t)
	p, err := s.RegisterProject(testProjectConfig(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	c, coord, err := s.CreateCollectionWithCoordinator(S(p, "id"), "CloudFormation probe", floorObjective, "Plan")
	if err != nil {
		t.Fatal(err)
	}
	forceCollectionCreatedAt(t, s, S(c, "id"), postCutoffCreatedAt)
	floorImplement(t, s, c, coord)
	if err = s.SetTaskStatus(S(coord, "id"), TaskWaiting, "handed control to delegated work"); err != nil {
		t.Fatal(err)
	}
	sched, err := NewScheduler(s, cfg, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = sched.enforcePlanningPolicy(S(c, "id")); err != nil {
		t.Fatal(err)
	}
	if reviews := planReviewsIn(t, s, c); len(reviews) != 1 {
		t.Fatalf("a WAITING-only coordinator must no longer suppress staging; expected exactly one staged review, got %d", len(reviews))
	}
}

// (d) A lone COMPLETE verdict=fail review must not prematurely release the claim:
// the fail-review repair lineage records a NEWER (nonterminal) review that the
// same latest-review lookup sees, so the implement stays unclaimable.
func TestArtifactPlanReviewFloorFailReviewRepairLineage(t *testing.T) {
	assertFloorObjectiveAllFalse(t)
	s, _ := newFakeStore(t)
	p, err := s.RegisterProject(testProjectConfig(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	c, coord, err := s.CreateCollectionWithCoordinator(S(p, "id"), "CloudFormation probe", floorObjective, "Plan")
	if err != nil {
		t.Fatal(err)
	}
	forceCollectionCreatedAt(t, s, S(c, "id"), postCutoffCreatedAt)
	impl := floorImplement(t, s, c, coord)

	older, _, err := s.CreateTask(CreateTaskOptions{ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "plan_review", Title: "Failed plan review", Objective: "review the plan", Reason: "gate", WorkspaceMode: "collection", Depth: 1, AllowDedupe: false})
	if err != nil {
		t.Fatal(err)
	}
	completePlanReview(t, s, older, "fail")

	// The repair lineage creates a fresh final plan review that gates the
	// implementations (the same GateImplementTasks the coordinator uses).
	newer, _, err := s.CreateTask(CreateTaskOptions{ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "plan_review", Title: "Repair plan review", Objective: "re-review the repaired plan", Reason: "repair", WorkspaceMode: "collection", Depth: 1, AllowDedupe: false, GateImplementTasks: true})
	if err != nil {
		t.Fatal(err)
	}
	_ = newer

	claimed, _, err := s.ClaimTask(S(impl, "id"), 3)
	if err != nil {
		t.Fatal(err)
	}
	if claimed != nil && S(claimed, "id") == S(impl, "id") {
		t.Fatal("a COMPLETE verdict=fail followed by the newer repair review must not release the floor")
	}
	if ev := floorEventsFor(t, s, c, impl, "PLAN_REVIEW_FLOOR_HELD"); len(ev) == 0 {
		t.Fatal("the repair-lineage held claim must be recorded as a floor-held claim")
	}
}

func planReviewsIn(t *testing.T, s *Store, c Row) Rows {
	t.Helper()
	rows, err := s.ListTasks(S(c, "id"))
	if err != nil {
		t.Fatal(err)
	}
	out := Rows{}
	for _, r := range rows {
		if S(r, "profile") == "plan_review" && S(r, "status") != TaskSuperseded && S(r, "status") != TaskCancelled {
			out = append(out, r)
		}
	}
	return out
}
