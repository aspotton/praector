package praector

import (
	"testing"
)

func TestExploreRequiredEscalationCreatesBlockingExploreAndResumes(t *testing.T) {
	s, cfg := newFakeStore(t)
	p, _ := s.RegisterProject(testProjectConfig(t.TempDir()))
	c, coord, _ := s.CreateCollectionWithCoordinator(S(p, "id"), "Feature", "Add a local parser test.", "Plan")
	critic, _, err := s.CreateTask(CreateTaskOptions{ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "critic", Title: "Critic", Objective: "critique", Reason: "policy", ParentTaskID: S(coord, "id"), WorkspaceMode: "collection", Depth: 1, AllowDedupe: false})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.SubmitResult(S(critic, "id"), Row{"status": "waiting", "summary": "need repo fact", "explore_required": []any{Row{"question": "What currently depends on the symbol the change removes?"}}})
	if err != nil {
		t.Fatal(err)
	}
	sched, _ := NewScheduler(s, cfg, 1)
	changed, err := sched.enforceExploreRequests(S(c, "id"))
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected EXPLORE_REQUIRED child")
	}
	tasks, _ := s.ListTasks(S(c, "id"))
	var explore Row
	for _, task := range tasks {
		if S(task, "profile") == "explore" && S(task, "requested_by_task_id") == S(critic, "id") {
			explore = task
		}
	}
	if explore == nil {
		t.Fatalf("missing explore child: %v", tasks)
	}
	deps, _ := s.Dependencies(S(critic, "id"))
	if !containsTaskID(deps, S(explore, "id")) {
		t.Fatalf("critic not blocked on explore: %v", deps)
	}
	_, err = s.SubmitResult(S(explore, "id"), Row{"summary": "dependency map", "findings": []any{"caller at store.go"}, "evidence": []any{"internal/praector/store.go"}})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetTaskStatus(S(explore, "id"), TaskComplete, "explore complete"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.RefreshWaitingTasks(S(c, "id")); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetTask(S(critic, "id"))
	if S(got, "status") != TaskReady {
		t.Fatalf("critic should resume READY, got %s", S(got, "status"))
	}
}

func TestExploreRequiredEnforceDedupesToOneLiveChild(t *testing.T) {
	s, cfg := newFakeStore(t)
	p, _ := s.RegisterProject(testProjectConfig(t.TempDir()))
	c, coord, _ := s.CreateCollectionWithCoordinator(S(p, "id"), "Feature", "Add a local parser test.", "Plan")
	critic, _, err := s.CreateTask(CreateTaskOptions{ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "critic", Title: "Critic", Objective: "critique", Reason: "policy", ParentTaskID: S(coord, "id"), WorkspaceMode: "collection", Depth: 1, AllowDedupe: false})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.SubmitResult(S(critic, "id"), Row{"status": "waiting", "summary": "need repo fact", "explore_required": []any{Row{"question": "What currently depends on the symbol the change removes?"}}})
	if err != nil {
		t.Fatal(err)
	}
	sched, _ := NewScheduler(s, cfg, 1)
	if _, err = sched.enforceExploreRequests(S(c, "id")); err != nil {
		t.Fatal(err)
	}
	changed, err := sched.enforceExploreRequests(S(c, "id"))
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("second enforce pass must not create a second live explore child")
	}
	count := 0
	tasks, _ := s.ListTasks(S(c, "id"))
	for _, task := range tasks {
		if S(task, "profile") == "explore" && S(task, "requested_by_task_id") == S(critic, "id") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly one live explore child, got %d", count)
	}
}

func TestExploreRequiredRoutingDoesNotAffectResearchRouting(t *testing.T) {
	s, cfg := newFakeStore(t)
	p, _ := s.RegisterProject(testProjectConfig(t.TempDir()))
	c, coord, _ := s.CreateCollectionWithCoordinator(S(p, "id"), "Feature", "Add a local parser test.", "Plan")
	researchCaller, _, err := s.CreateTask(CreateTaskOptions{ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "critic", Title: "RCritic", Objective: "critique", Reason: "policy", ParentTaskID: S(coord, "id"), WorkspaceMode: "collection", Depth: 1, AllowDedupe: false})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SubmitResult(S(researchCaller, "id"), Row{"status": "waiting", "summary": "need current fact", "research_required": []any{Row{"question": "What does the current external standard require?"}}}); err != nil {
		t.Fatal(err)
	}
	exploreCaller, _, err := s.CreateTask(CreateTaskOptions{ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "advise", Title: "ECritic", Objective: "advise", Reason: "policy", ParentTaskID: S(coord, "id"), WorkspaceMode: "collection", Depth: 1, AllowDedupe: false})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SubmitResult(S(exploreCaller, "id"), Row{"status": "waiting", "summary": "need repo fact", "explore_required": []any{Row{"question": "What depends on the target?"}}}); err != nil {
		t.Fatal(err)
	}
	sched, _ := NewScheduler(s, cfg, 1)
	if _, err = sched.enforceResearchRequests(S(c, "id")); err != nil {
		t.Fatal(err)
	}
	if _, err = sched.enforceExploreRequests(S(c, "id")); err != nil {
		t.Fatal(err)
	}
	tasks, _ := s.ListTasks(S(c, "id"))
	var researchChild, exploreForResearch, researchForExplore, exploreChild Row
	for _, task := range tasks {
		switch S(task, "profile") {
		case "research":
			if S(task, "requested_by_task_id") == S(researchCaller, "id") {
				researchChild = task
			}
			if S(task, "requested_by_task_id") == S(exploreCaller, "id") {
				researchForExplore = task
			}
		case "explore":
			if S(task, "requested_by_task_id") == S(exploreCaller, "id") {
				exploreChild = task
			}
			if S(task, "requested_by_task_id") == S(researchCaller, "id") {
				exploreForResearch = task
			}
		}
	}
	if researchChild == nil {
		t.Fatal("research_required result must still produce a research child")
	}
	if exploreForResearch != nil {
		t.Fatal("research_required result must not produce an explore child")
	}
	if exploreChild == nil {
		t.Fatal("explore_required result must produce an explore child")
	}
	if researchForExplore != nil {
		t.Fatal("explore_required result must not produce a research child")
	}
}
