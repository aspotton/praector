package praector

import (
	"strings"
	"testing"
)

func TestSchedulerPolicyStagesCriticThenPlanReviewBeforeImplementation(t *testing.T) {
	s, cfg := newFakeStore(t)
	p, _ := s.RegisterProject(testProjectConfig(t.TempDir()))
	c, coord, e := s.CreateCollectionWithCoordinator(S(p, "id"), "Secure feature", "Add API-key authentication and per-key rate limiting to the existing /hello endpoint.", "Plan")
	if e != nil {
		t.Fatal(e)
	}
	impl, _, e := s.CreateTask(CreateTaskOptions{ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "implement", Title: "Implement", Objective: "Do work", Reason: "plan", ParentTaskID: S(coord, "id"), RequestedByTaskID: S(coord, "id"), WorkspaceMode: "collection", Depth: 1, AllowDedupe: true})
	if e != nil {
		t.Fatal(e)
	}
	sched, e := NewScheduler(s, cfg, 1)
	if e != nil {
		t.Fatal(e)
	}

	// Exact Python behavior: while a coordinator is still shaping the graph,
	// deterministic policy inserts the required critic but does not manufacture
	// the final policy plan-review gate. Claim-time gating keeps implementation
	// from running until planning settles.
	changed, e := sched.enforcePlanningPolicy(S(c, "id"))
	if e != nil {
		t.Fatal(e)
	}
	if !changed {
		t.Fatal("expected critic policy mutation")
	}
	tasks, _ := s.ListTasks(S(c, "id"))
	var critic Row
	planCount := 0
	for _, x := range tasks {
		switch S(x, "profile") {
		case "critic":
			critic = x
		case "plan_review":
			planCount++
		}
	}
	if critic == nil {
		t.Fatalf("missing critic gate: %v", tasks)
	}
	if planCount != 0 {
		t.Fatalf("plan review should wait for coordinator handoff, got %d: %v", planCount, tasks)
	}

	// Simulate the critic finishing and the coordinator finishing graph shaping.
	_, _ = s.SubmitResult(S(critic, "id"), Row{"status": "complete", "summary": "preflight complete"})
	if e = s.SetTaskStatus(S(critic, "id"), TaskComplete, "test critic complete"); e != nil {
		t.Fatal(e)
	}
	_, _ = s.SubmitResult(S(coord, "id"), Row{"status": "complete", "summary": "planning complete"})
	if e = s.SetTaskStatus(S(coord, "id"), TaskComplete, "test coordinator complete"); e != nil {
		t.Fatal(e)
	}

	changed, e = sched.enforcePlanningPolicy(S(c, "id"))
	if e != nil {
		t.Fatal(e)
	}
	if !changed {
		t.Fatal("expected final plan-review policy mutation")
	}
	tasks, _ = s.ListTasks(S(c, "id"))
	var plan Row
	for _, x := range tasks {
		if S(x, "profile") == "plan_review" && S(x, "status") != TaskSuperseded && S(x, "status") != TaskCancelled {
			plan = x
		}
	}
	if plan == nil {
		t.Fatalf("missing plan-review gate: %v", tasks)
	}
	pdeps, _ := s.Dependencies(S(plan, "id"))
	foundCritic := false
	for _, d := range pdeps {
		if S(d, "id") == S(critic, "id") {
			foundCritic = true
		}
	}
	if !foundCritic {
		t.Fatalf("plan review doesn't depend on critic: %v", pdeps)
	}
	ideps, _ := s.Dependencies(S(impl, "id"))
	foundPlan := false
	for _, d := range ideps {
		if S(d, "id") == S(plan, "id") {
			foundPlan = true
		}
	}
	if !foundPlan {
		t.Fatalf("implementation not gated by plan review: %v", ideps)
	}

	changed, e = sched.enforcePlanningPolicy(S(c, "id"))
	if e != nil {
		t.Fatal(e)
	}
	if changed {
		t.Fatal("policy reconciliation should be idempotent once final gate exists")
	}
}

func TestReadOnlyCapabilityViolationRetriesSameTaskBeforeCoordinatorAttention(t *testing.T) {
	s, cfg := newFakeStore(t)
	pcfg := testProjectConfig(t.TempDir())
	pcfg.Limits.MaxAttemptsPerTask = 3
	p, err := s.RegisterProject(pcfg)
	if err != nil {
		t.Fatal(err)
	}
	c, coord, err := s.CreateCollectionWithCoordinator(S(p, "id"), "Feature", "Add endpoint", "Plan")
	if err != nil {
		t.Fatal(err)
	}
	review, _, err := s.CreateTask(CreateTaskOptions{
		ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "review", Title: "Review", Objective: "Review it",
		Reason: "verify", ParentTaskID: S(coord, "id"), WorkspaceMode: "collection", Depth: 1, AllowDedupe: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetTaskStatus(S(review, "id"), TaskRunning, "test running"); err != nil {
		t.Fatal(err)
	}
	sched, err := NewScheduler(s, cfg, 1)
	if err != nil {
		t.Fatal(err)
	}
	msg := sched.handleReadOnlyCapabilityViolation(pcfg, review, Row{"id": "A-1", "attempt_no": 1})
	if msg == "" {
		t.Fatal("expected protocol failure message")
	}
	got, err := s.GetTask(S(review, "id"))
	if err != nil {
		t.Fatal(err)
	}
	if S(got, "status") != TaskReady {
		t.Fatalf("expected same review task READY for retry, got %s", S(got, "status"))
	}
	tasks, err := s.ListTasks(S(c, "id"))
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range tasks {
		if S(task, "profile") == "coordinator" && S(task, "title") == "Coordinator attention" {
			t.Fatalf("capability retry must not create coordinator attention: %v", task)
		}
	}
	events, err := s.EventsForCollection(S(c, "id"), 50)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if S(event, "event_type") == "READ_ONLY_CAPABILITY_RETRY" && S(event, "task_id") == S(review, "id") {
			found = true
		}
	}
	if !found {
		t.Fatal("missing READ_ONLY_CAPABILITY_RETRY event")
	}
}

func TestReadOnlyCapabilityViolationFailsAfterRetryBudget(t *testing.T) {
	s, cfg := newFakeStore(t)
	pcfg := testProjectConfig(t.TempDir())
	pcfg.Limits.MaxAttemptsPerTask = 3
	p, _ := s.RegisterProject(pcfg)
	c, coord, _ := s.CreateCollectionWithCoordinator(S(p, "id"), "Feature", "Add endpoint", "Plan")
	review, _, err := s.CreateTask(CreateTaskOptions{
		ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "review", Title: "Review", Objective: "Review it",
		Reason: "verify", ParentTaskID: S(coord, "id"), WorkspaceMode: "collection", Depth: 1, AllowDedupe: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = s.SetTaskStatus(S(review, "id"), TaskRunning, "test running")
	sched, _ := NewScheduler(s, cfg, 1)
	sched.handleReadOnlyCapabilityViolation(pcfg, review, Row{"id": "A-3", "attempt_no": 3})
	got, err := s.GetTask(S(review, "id"))
	if err != nil {
		t.Fatal(err)
	}
	if S(got, "status") != TaskFailed {
		t.Fatalf("expected FAILED after retry budget, got %s", S(got, "status"))
	}
	if !strings.Contains(JSONDumps(M(got, "result")), "retry budget exhausted") {
		t.Fatalf("expected exhausted-budget risk, got %v", M(got, "result"))
	}
}
