package praector

import (
	"strings"
	"testing"
)

func TestSemanticReviewFailureCreatesRepairThenFreshIndependentReview(t *testing.T) {
	s, _ := newFakeStore(t)
	p, err := s.RegisterProject(testProjectConfig(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	c, coord, err := s.CreateCollectionWithCoordinator(S(p, "id"), "Semantic repair", "Add authenticated endpoint", "Plan")
	if err != nil {
		t.Fatal(err)
	}
	impl1, _, err := s.CreateTask(CreateTaskOptions{
		ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "implement", Title: "Initial implementation",
		Objective: "Implement endpoint", Reason: "plan", ParentTaskID: S(coord, "id"), WorkspaceMode: "collection", Depth: 1, AllowDedupe: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = s.SetTaskStatus(S(impl1, "id"), TaskComplete, "test complete")
	review1, _, err := s.CreateTask(CreateTaskOptions{
		ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "review", Title: "Review initial",
		Objective: "Review endpoint", Reason: "policy", ParentTaskID: S(impl1, "id"), DependsOn: []string{S(impl1, "id")}, WorkspaceMode: "collection", Depth: 2, AllowDedupe: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.AddEdge(S(impl1, "id"), S(review1, "id"), S(c, "id"), "verifies"); err != nil {
		t.Fatal(err)
	}
	_, _ = s.SubmitResult(S(review1, "id"), Row{"status": "complete", "summary": "authentication defect", "verdict": "fail"})

	sch := &Scheduler{Store: s}
	if err = sch.handleFailedReview(p, review1); err != nil {
		t.Fatal(err)
	}
	tasks, _ := s.ListTasks(S(c, "id"))
	var repair Row
	for _, task := range tasks {
		if S(task, "profile") == "implement" && S(task, "parent_task_id") == S(review1, "id") {
			repair = task
			break
		}
	}
	if repair == nil {
		t.Fatal("semantic review failure did not create repair implementation")
	}
	deps, _ := s.Dependencies(S(repair, "id"))
	if len(deps) != 1 || S(deps[0], "id") != S(review1, "id") {
		t.Fatalf("repair should wait for failed review to become terminal, deps=%v", deps)
	}
	currentReview, _ := s.GetTask(S(review1, "id"))
	if S(currentReview, "status") != TaskVerifying {
		t.Fatalf("failed review should remain VERIFYING until executor finalizes it, got %s", S(currentReview, "status"))
	}
	_ = s.SetTaskStatus(S(review1, "id"), TaskComplete, "semantic fail recorded")
	_ = s.SetTaskStatus(S(repair, "id"), TaskComplete, "repair integrated")
	repair, _ = s.GetTask(S(repair, "id"))
	if err = sch.ensureReview(p, c, repair); err != nil {
		t.Fatal(err)
	}

	tasks, _ = s.ListTasks(S(c, "id"))
	var review2 Row
	for _, task := range tasks {
		if S(task, "profile") == "review" && S(task, "id") != S(review1, "id") {
			review2 = task
		}
	}
	if review2 == nil {
		t.Fatal("repair did not receive a fresh durable review task")
	}
	if S(review2, "parent_task_id") != S(repair, "id") {
		t.Fatalf("fresh review should be parented by repair task: %v", review2)
	}
	historicalReview, _ := s.GetTask(S(review1, "id"))
	if S(historicalReview, "status") != TaskComplete || strings.ToLower(S(M(historicalReview, "result"), "verdict")) != "fail" {
		t.Fatalf("historical failed review should remain complete fail evidence: %v", historicalReview)
	}
	edges, _ := s.ListEdges(S(c, "id"))
	var verifiesOriginal, verifiesRepair bool
	for _, edge := range edges {
		if S(edge, "relation") != "verifies" || S(edge, "to_task_id") != S(review2, "id") {
			continue
		}
		if S(edge, "from_task_id") == S(impl1, "id") {
			verifiesOriginal = true
		}
		if S(edge, "from_task_id") == S(repair, "id") {
			verifiesRepair = true
		}
	}
	if !verifiesOriginal || !verifiesRepair {
		t.Fatalf("fresh post-repair review must verify original and repair implementations: original=%v repair=%v edges=%v", verifiesOriginal, verifiesRepair, edges)
	}
	_, _ = s.SubmitResult(S(review2, "id"), Row{"status": "complete", "summary": "repair verified", "verdict": "pass"})
	_ = s.SetTaskStatus(S(review2, "id"), TaskComplete, "fresh review passed")
	missing, err := s.ImplementationsMissingPassingReview(S(c, "id"))
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 0 {
		t.Fatalf("fresh post-repair pass should satisfy review lineage for original and repair implementations: %v", missing)
	}
	_, _ = s.SubmitResult(S(coord, "id"), Row{"status": "complete", "summary": "planned"})
	_ = s.SetTaskStatus(S(coord, "id"), TaskComplete, "planning complete")
	state, err := s.UpdateCollectionTerminalState(S(c, "id"))
	if err != nil {
		t.Fatal(err)
	}
	if state != "COMPLETE" {
		t.Fatalf("repaired collection should become COMPLETE, got %s", state)
	}
	if _, _, err = validateCollectionForAcceptance(s, c); err != nil {
		t.Fatalf("historical failed review should not poison acceptance after fresh repaired pass: %v", err)
	}
}
