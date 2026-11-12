package praector

import (
	"strings"
	"testing"
)

func TestSchedulerPolicyStagesExploreCriticThenPlanReviewBeforeImplementation(t *testing.T) {
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

	// Deterministic policy injects repository exploration followed by a critic
	// before the coordinator is allowed to shape the final implementation graph.
	changed, e := sched.enforcePlanningPolicy(S(c, "id"))
	if e != nil {
		t.Fatal(e)
	}
	if !changed {
		t.Fatal("expected explore/critic policy mutation")
	}
	tasks, _ := s.ListTasks(S(c, "id"))
	var explore Row
	var critic Row
	planCount := 0
	for _, x := range tasks {
		switch S(x, "profile") {
		case "explore":
			explore = x
		case "critic":
			critic = x
		case "plan_review":
			planCount++
		}
	}
	if explore == nil || critic == nil {
		t.Fatalf("missing explore/critic preflight: %v", tasks)
	}
	cdeps, _ := s.Dependencies(S(critic, "id"))
	if !containsTaskID(cdeps, S(explore, "id")) {
		t.Fatalf("critic does not depend on explore: %v", cdeps)
	}
	if planCount != 0 {
		t.Fatalf("plan review should wait for coordinator handoff, got %d: %v", planCount, tasks)
	}

	// Simulate exploration, critic, and coordinator finishing graph shaping.
	_, _ = s.SubmitResult(S(explore, "id"), Row{"status": "complete", "summary": "repository mapped"})
	if e = s.SetTaskStatus(S(explore, "id"), TaskComplete, "test explore complete"); e != nil {
		t.Fatal(e)
	}
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

func TestSchedulerPolicyGivesRevisionFreshExploreAndCritic(t *testing.T) {
	s, cfg := newFakeStore(t)
	p, _ := s.RegisterProject(testProjectConfig(t.TempDir()))
	c, initial, err := s.CreateCollectionWithCoordinator(S(p, "id"), "Feature", "Add API-key authentication to the API.", "Plan")
	if err != nil {
		t.Fatal(err)
	}
	oldExplore, _, err := s.CreateTask(CreateTaskOptions{
		ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "explore", Title: "Explore current implementation surface", Objective: "Map repo", Reason: "policy",
		ParentTaskID: S(initial, "id"), RequestedByTaskID: S(initial, "id"), WorkspaceMode: "collection", Depth: 1, AllowDedupe: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	oldCritic, _, err := s.CreateTask(CreateTaskOptions{
		ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "critic", Title: "Preflight implementation plan", Objective: "Critique", Reason: "policy",
		ParentTaskID: S(initial, "id"), RequestedByTaskID: S(initial, "id"), DependsOn: []string{S(oldExplore, "id")}, WorkspaceMode: "collection", Depth: 1, AllowDedupe: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range []Row{oldExplore, oldCritic, initial} {
		_, _ = s.SubmitResult(S(task, "id"), Row{"status": "complete", "summary": "done"})
		if err := s.SetTaskStatus(S(task, "id"), TaskComplete, "done"); err != nil {
			t.Fatal(err)
		}
	}
	// This test isolates revision-generation preflight rather than delivery acceptance.
	if err := s.SetCollectionStatus(S(c, "id"), TaskComplete, true); err != nil {
		t.Fatal(err)
	}
	rev, revision, err := s.RequestCollectionRevision(S(c, "id"), "Change the accepted implementation")
	if err != nil {
		t.Fatal(err)
	}
	_ = rev
	sched, err := NewScheduler(s, cfg, 1)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := sched.enforcePlanningPolicy(S(c, "id"))
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected fresh revision preflight")
	}
	tasks, _ := s.ListTasks(S(c, "id"))
	var freshExplore, freshCritic Row
	for _, task := range tasks {
		if S(task, "requested_by_task_id") != S(revision, "id") {
			continue
		}
		switch S(task, "profile") {
		case "explore":
			freshExplore = task
		case "critic":
			freshCritic = task
		}
	}
	if freshExplore == nil || freshCritic == nil {
		t.Fatalf("revision missing fresh explore/critic: %v", tasks)
	}
	if S(freshExplore, "id") == S(oldExplore, "id") || S(freshCritic, "id") == S(oldCritic, "id") {
		t.Fatal("revision reused prior-generation preflight")
	}
	criticDeps, _ := s.Dependencies(S(freshCritic, "id"))
	if !containsTaskID(criticDeps, S(freshExplore, "id")) {
		t.Fatalf("fresh critic does not depend on fresh explore: %v", criticDeps)
	}
	revisionDeps, _ := s.Dependencies(S(revision, "id"))
	if !containsTaskID(revisionDeps, S(freshExplore, "id")) || !containsTaskID(revisionDeps, S(freshCritic, "id")) {
		t.Fatalf("revision coordinator not blocked on fresh preflight: %v", revisionDeps)
	}
}
