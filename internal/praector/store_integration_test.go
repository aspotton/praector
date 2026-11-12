package praector

import (
	"strings"
	"testing"
)

func testProjectConfig(root string) ProjectConfig {
	models := map[string]string{}
	for _, n := range ProfileNames {
		models[n] = ""
	}
	return ProjectConfig{ProjectID: "PRJ-test", Name: "test", Root: root, BaseBranch: "main", Harness: "opencode", OpenCodeCommand: "opencode", Models: models, Limits: DefaultLimits()}
}

func TestStoreBootstrapGraphAndDAGInvariant(t *testing.T) {
	s, _ := newFakeStore(t)
	p, err := s.RegisterProject(testProjectConfig(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	c, coord, err := s.CreateCollectionWithCoordinator(S(p, "id"), "Feature", "Add API-key authentication", "Plan it")
	if err != nil {
		t.Fatal(err)
	}
	critic, _, err := s.CreateTask(CreateTaskOptions{ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "critic", Title: "Critic", Objective: "Critique", Reason: "policy", ParentTaskID: S(coord, "id"), RequestedByTaskID: S(coord, "id"), WorkspaceMode: "collection", Depth: 1, AllowDedupe: true})
	if err != nil {
		t.Fatal(err)
	}
	plan, _, err := s.CreateTask(CreateTaskOptions{ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "plan_review", Title: "Review", Objective: "Review plan", Reason: "gate", ParentTaskID: S(coord, "id"), RequestedByTaskID: S(coord, "id"), DependsOn: []string{S(critic, "id")}, WorkspaceMode: "collection", Depth: 1, AllowDedupe: true})
	if err != nil {
		t.Fatal(err)
	}
	impl, _, err := s.CreateTask(CreateTaskOptions{ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "implement", Title: "Implement", Objective: "Do it", Reason: "plan", ParentTaskID: S(coord, "id"), RequestedByTaskID: S(coord, "id"), DependsOn: []string{S(plan, "id")}, WorkspaceMode: "collection", Depth: 1, AllowDedupe: true})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.AddDependency(S(plan, "id"), S(impl, "id")); err == nil {
		t.Fatal("expected cycle rejection")
	} else if !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("wrong error: %v", err)
	}
	path, err := s.DependencyPath(S(critic, "id"), S(impl, "id"))
	if err != nil {
		t.Fatal(err)
	}
	if len(path) != 3 {
		t.Fatalf("unexpected path: %v", path)
	}
}

func TestSingleFlightPlanReviewRequest(t *testing.T) {
	s, _ := newFakeStore(t)
	p, _ := s.RegisterProject(testProjectConfig(t.TempDir()))
	c, coord, e := s.CreateCollectionWithCoordinator(S(p, "id"), "Feature", "Add API-key authentication", "Plan")
	if e != nil {
		t.Fatal(e)
	}
	critic, _, e := s.RequestTask(S(coord, "id"), RequestTaskOptions{Profile: "critic", Title: "Critic", Objective: "Critique", Reason: "security", Blocking: true, WorkspaceMode: "collection", Limits: DefaultLimits()})
	if e != nil {
		t.Fatal(e)
	}
	_, _ = s.SubmitResult(S(critic, "id"), Row{"status": "complete", "summary": "ok"})
	_ = s.SetTaskStatus(S(critic, "id"), TaskComplete, "test")
	coord, _ = s.GetTask(S(coord, "id"))
	_ = s.SetTaskStatus(S(coord, "id"), TaskRunning, "test resume")
	first, created, e := s.RequestTask(S(coord, "id"), RequestTaskOptions{Profile: "plan_review", Title: "Plan", Objective: "Review", Reason: "gate", Blocking: true, WorkspaceMode: "collection", Limits: DefaultLimits()})
	if e != nil {
		t.Fatal(e)
	}
	if !created {
		t.Fatal("first plan review should be created")
	}
	_ = s.SetTaskStatus(S(coord, "id"), TaskRunning, "simulate coordinator followup")
	second, created, e := s.RequestTask(S(coord, "id"), RequestTaskOptions{Profile: "plan_review", Title: "Another", Objective: "Review again", Reason: "gate", Blocking: true, WorkspaceMode: "collection", Limits: DefaultLimits()})
	if e != nil {
		t.Fatal(e)
	}
	if created || S(first, "id") != S(second, "id") {
		t.Fatalf("expected plan-review reuse: first=%v second=%v created=%v", first, second, created)
	}
	tasks, _ := s.ListTasks(S(c, "id"))
	n := 0
	for _, x := range tasks {
		if S(x, "profile") == "plan_review" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("expected one plan review, got %d", n)
	}
}

func TestRevisionReopensCompleteCollection(t *testing.T) {
	s, _ := newFakeStore(t)
	p, _ := s.RegisterProject(testProjectConfig(t.TempDir()))
	c, _, e := s.CreateCollectionWithCoordinator(S(p, "id"), "Feature", "Analyze README.md wording and report findings", "Plan")
	if e != nil {
		t.Fatal(e)
	}
	tasks, _ := s.ListTasks(S(c, "id"))
	for _, x := range tasks {
		_, _ = s.SubmitResult(S(x, "id"), Row{"status": "complete", "summary": "done"})
		_ = s.SetTaskStatus(S(x, "id"), TaskComplete, "done")
	}
	_, _ = s.UpdateCollectionTerminalState(S(c, "id"))
	c, _ = s.GetCollection(S(c, "id"))
	if S(c, "status") != "COMPLETE" {
		t.Fatalf("not complete: %v", c)
	}
	rev, coord, e := s.RequestCollectionRevision(S(c, "id"), "Fix acceptance issue")
	if e != nil {
		t.Fatal(e)
	}
	if S(rev, "status") != "OPEN" || S(coord, "profile") != "coordinator" {
		t.Fatalf("bad revision: %v %v", rev, coord)
	}
	c, _ = s.GetCollection(S(c, "id"))
	if S(c, "status") != "RUNNING" {
		t.Fatalf("expected reopened running: %v", c)
	}
}

func TestClaimGateBlocksImplementationUntilFreshCriticAndPlanReview(t *testing.T) {
	s, _ := newFakeStore(t)
	p, _ := s.RegisterProject(testProjectConfig(t.TempDir()))
	c, coord, err := s.CreateCollectionWithCoordinator(S(p, "id"), "Secure feature", "Add API-key authentication and per-key rate limiting to the existing /hello endpoint.", "Plan")
	if err != nil {
		t.Fatal(err)
	}
	impl, _, err := s.CreateTask(CreateTaskOptions{
		ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "implement",
		Title: "Implement", Objective: "Add the authenticated rate limit", Reason: "plan",
		ParentTaskID: S(coord, "id"), RequestedByTaskID: S(coord, "id"), WorkspaceMode: "collection", Depth: 1, AllowDedupe: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, attempt, err := s.ClaimTask(S(impl, "id"), 3)
	if err != nil {
		t.Fatal(err)
	}
	if claimed != nil || attempt != nil {
		t.Fatalf("implementation claimed before planning gates: task=%v attempt=%v", claimed, attempt)
	}

	critic, _, err := s.CreateTask(CreateTaskOptions{
		ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "critic", Title: "Critic", Objective: "Preflight", Reason: "policy",
		ParentTaskID: S(coord, "id"), WorkspaceMode: "collection", Depth: 1, AllowDedupe: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = s.SubmitResult(S(critic, "id"), Row{"status": "complete", "summary": "ok"})
	if err = s.SetTaskStatus(S(critic, "id"), TaskComplete, "critic complete"); err != nil {
		t.Fatal(err)
	}
	_, _ = s.SubmitResult(S(coord, "id"), Row{"status": "complete", "summary": "planned"})
	if err = s.SetTaskStatus(S(coord, "id"), TaskComplete, "coordinator complete"); err != nil {
		t.Fatal(err)
	}
	fp, err := s.MaterialPlanFingerprint(S(c, "id"), false)
	if err != nil {
		t.Fatal(err)
	}
	plan, _, err := s.CreateTask(CreateTaskOptions{
		ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "plan_review", Title: "Review plan", Objective: "Review", Reason: "gate",
		ParentTaskID: S(coord, "id"), DependsOn: []string{S(critic, "id")}, WorkspaceMode: "collection", Depth: 1, AllowDedupe: false,
		GateImplementTasks: true, CriticBaselineTaskID: S(critic, "id"), CriticBaselineFingerprint: fp, CriticBaselineSource: "initial_plan_seal",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = s.SubmitResult(S(plan, "id"), Row{"status": "complete", "summary": "pass", "verdict": "pass"})
	if err = s.SetTaskStatus(S(plan, "id"), TaskComplete, "plan review complete"); err != nil {
		t.Fatal(err)
	}

	claimed, attempt, err = s.ClaimTask(S(impl, "id"), 3)
	if err != nil {
		t.Fatal(err)
	}
	if claimed == nil || attempt == nil || S(claimed, "status") != TaskRunning {
		t.Fatalf("implementation did not become claimable after fresh gates: task=%v attempt=%v", claimed, attempt)
	}
}

func TestSupersededReviewPreservesImplementationVerificationLineage(t *testing.T) {
	s, _ := newFakeStore(t)
	p, _ := s.RegisterProject(testProjectConfig(t.TempDir()))
	c, coord, err := s.CreateCollectionWithCoordinator(S(p, "id"), "Feature", "Analyze repository behavior and report findings", "Plan")
	if err != nil {
		t.Fatal(err)
	}
	impl, _, err := s.CreateTask(CreateTaskOptions{ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "implement", Title: "Implement", Objective: "Do it", Reason: "plan", ParentTaskID: S(coord, "id"), WorkspaceMode: "collection", Depth: 1, AllowDedupe: false})
	if err != nil {
		t.Fatal(err)
	}
	review1, _, err := s.CreateTask(CreateTaskOptions{ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "review", Title: "Review 1", Objective: "Review", Reason: "auto", ParentTaskID: S(impl, "id"), DependsOn: []string{S(impl, "id")}, WorkspaceMode: "collection", Depth: 2, AllowDedupe: false})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.AddEdge(S(impl, "id"), S(review1, "id"), S(c, "id"), "verifies"); err != nil {
		t.Fatal(err)
	}
	review2, _, err := s.CreateTask(CreateTaskOptions{ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "review", Title: "Review 2", Objective: "Review again", Reason: "retry", ParentTaskID: S(impl, "id"), WorkspaceMode: "collection", Depth: 2, AllowDedupe: false})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SupersedeTask(S(review1, "id"), S(review2, "id"), "replace review"); err != nil {
		t.Fatal(err)
	}
	edges, err := s.ListEdges(S(c, "id"))
	if err != nil {
		t.Fatal(err)
	}
	var dep, verifies, superseded bool
	for _, e := range edges {
		from, to, rel := S(e, "from_task_id"), S(e, "to_task_id"), S(e, "relation")
		if from == S(impl, "id") && to == S(review2, "id") && rel == "depends_on" {
			dep = true
		}
		if from == S(impl, "id") && to == S(review2, "id") && rel == "verifies" {
			verifies = true
		}
		if from == S(review1, "id") && to == S(review2, "id") && rel == "superseded_by" {
			superseded = true
		}
	}
	if !dep || !verifies || !superseded {
		t.Fatalf("superseded review lineage incomplete: dep=%v verifies=%v superseded=%v edges=%v", dep, verifies, superseded, edges)
	}
}

func TestTerminalStateBlocksWhenRequiredCriticSealIsStale(t *testing.T) {
	s, _ := newFakeStore(t)
	p, _ := s.RegisterProject(testProjectConfig(t.TempDir()))
	c, coord, err := s.CreateCollectionWithCoordinator(S(p, "id"), "Secure feature", "Add API-key authentication and per-key rate limiting to the existing /hello endpoint.", "Plan")
	if err != nil {
		t.Fatal(err)
	}
	critic, _, err := s.CreateTask(CreateTaskOptions{ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "critic", Title: "Critic", Objective: "Preflight", Reason: "policy", ParentTaskID: S(coord, "id"), WorkspaceMode: "collection", Depth: 1, AllowDedupe: false})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = s.SubmitResult(S(critic, "id"), Row{"status": "complete", "summary": "ok"})
	if err = s.SetTaskStatus(S(critic, "id"), TaskComplete, "critic complete"); err != nil {
		t.Fatal(err)
	}
	impl, _, err := s.CreateTask(CreateTaskOptions{ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "implement", Title: "Implement", Objective: "Initial secure implementation contract", Reason: "plan", ParentTaskID: S(coord, "id"), WorkspaceMode: "collection", Depth: 1, AllowDedupe: false})
	if err != nil {
		t.Fatal(err)
	}
	fp, err := s.MaterialPlanFingerprint(S(c, "id"), false)
	if err != nil {
		t.Fatal(err)
	}
	plan, _, err := s.CreateTask(CreateTaskOptions{ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "plan_review", Title: "Review plan", Objective: "Review", Reason: "gate", ParentTaskID: S(coord, "id"), DependsOn: []string{S(critic, "id")}, WorkspaceMode: "collection", Depth: 1, AllowDedupe: false, GateImplementTasks: true, CriticBaselineTaskID: S(critic, "id"), CriticBaselineFingerprint: fp, CriticBaselineSource: "initial_plan_seal"})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = s.SubmitResult(S(plan, "id"), Row{"status": "complete", "summary": "pass", "verdict": "pass"})
	if err = s.SetTaskStatus(S(plan, "id"), TaskComplete, "plan pass"); err != nil {
		t.Fatal(err)
	}
	_, _ = s.SubmitResult(S(coord, "id"), Row{"status": "complete", "summary": "planned"})
	if err = s.SetTaskStatus(S(coord, "id"), TaskComplete, "coord complete"); err != nil {
		t.Fatal(err)
	}

	// Change the durable implementation contract after the critic seal.
	if err = s.Mutate("UPDATE tasks SET objective="+SQLLiteral("Materially changed secure implementation contract")+",updated_at="+SQLLiteral(UTCNow())+" WHERE id="+SQLLiteral(S(impl, "id")), "test: mutate material plan"); err != nil {
		t.Fatal(err)
	}
	_, _ = s.SubmitResult(S(impl, "id"), Row{"status": "complete", "summary": "implemented"})
	if err = s.SetTaskStatus(S(impl, "id"), TaskComplete, "impl complete"); err != nil {
		t.Fatal(err)
	}
	review, _, err := s.CreateTask(CreateTaskOptions{ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "review", Title: "Review implementation", Objective: "Review", Reason: "auto", ParentTaskID: S(impl, "id"), DependsOn: []string{S(impl, "id")}, WorkspaceMode: "collection", Depth: 2, AllowDedupe: false})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.AddEdge(S(impl, "id"), S(review, "id"), S(c, "id"), "verifies"); err != nil {
		t.Fatal(err)
	}
	_, _ = s.SubmitResult(S(review, "id"), Row{"status": "complete", "summary": "pass", "verdict": "pass"})
	if err = s.SetTaskStatus(S(review, "id"), TaskComplete, "review pass"); err != nil {
		t.Fatal(err)
	}

	state, err := s.CriticRevalidationState(S(c, "id"))
	if err != nil {
		t.Fatal(err)
	}
	if !B(state, "stale") {
		t.Fatalf("expected stale critic seal: %v", state)
	}
	terminal, err := s.UpdateCollectionTerminalState(S(c, "id"))
	if err != nil {
		t.Fatal(err)
	}
	if terminal != "BLOCKED" {
		t.Fatalf("stale critic must block terminal completion, got %s", terminal)
	}
}

func TestDependencyReferencesMustResolveWithinCollection(t *testing.T) {
	s, _ := newFakeStore(t)
	p, err := s.RegisterProject(testProjectConfig(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	c1, coord1, err := s.CreateCollectionWithCoordinator(S(p, "id"), "First", "Add a small endpoint", "Plan")
	if err != nil {
		t.Fatal(err)
	}
	c2, coord2, err := s.CreateCollectionWithCoordinator(S(p, "id"), "Second", "Add another endpoint", "Plan")
	if err != nil {
		t.Fatal(err)
	}

	before, err := s.ListTasks(S(c1, "id"))
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = s.CreateTask(CreateTaskOptions{
		ProjectID: S(p, "id"), CollectionID: S(c1, "id"), Profile: "implement",
		Title: "Bad missing dependency", Objective: "Should not publish", Reason: "test",
		ParentTaskID: S(coord1, "id"), RequestedByTaskID: S(coord1, "id"),
		DependsOn: []string{"T-DOES-NOT-EXIST"}, WorkspaceMode: "collection", Depth: 1, AllowDedupe: false,
	})
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "dependency task not found") {
		t.Fatalf("expected missing dependency rejection, got %v", err)
	}
	after, err := s.ListTasks(S(c1, "id"))
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("missing dependency published a task: before=%d after=%d", len(before), len(after))
	}

	_, _, err = s.CreateTask(CreateTaskOptions{
		ProjectID: S(p, "id"), CollectionID: S(c1, "id"), Profile: "implement",
		Title: "Bad cross collection dependency", Objective: "Should not publish", Reason: "test",
		ParentTaskID: S(coord1, "id"), RequestedByTaskID: S(coord1, "id"),
		DependsOn: []string{S(coord2, "id")}, WorkspaceMode: "collection", Depth: 1, AllowDedupe: false,
	})
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "belongs to collection") {
		t.Fatalf("expected cross-collection dependency rejection, got %v", err)
	}

	if err = s.AddEdge("T-DOES-NOT-EXIST", S(coord1, "id"), S(c1, "id"), "depends_on"); err == nil || !strings.Contains(strings.ToLower(err.Error()), "dependency task not found") {
		t.Fatalf("expected low-level missing dependency rejection, got %v", err)
	}
	if err = s.AddEdge(S(coord2, "id"), S(coord1, "id"), S(c1, "id"), "depends_on"); err == nil || !strings.Contains(strings.ToLower(err.Error()), "belongs to collection") {
		t.Fatalf("expected low-level cross-collection dependency rejection, got %v", err)
	}

	_ = c2
}

func TestCollectionRemainsRunningWhileParentSessionIsStillActive(t *testing.T) {
	s, _ := newFakeStore(t)
	p, err := s.RegisterProject(testProjectConfig(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	c, coord, err := s.CreateCollectionWithCoordinator(S(p, "id"), "Session handoff", "Coordinate a gated implementation", "Plan")
	if err != nil {
		t.Fatal(err)
	}
	claimed, attempt, err := s.ClaimTask(S(coord, "id"), 3)
	if err != nil || claimed == nil || attempt == nil {
		t.Fatalf("claim coordinator: task=%v attempt=%v err=%v", claimed, attempt, err)
	}
	if _, err = s.CreateSession(S(attempt, "id"), "opencode", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if err = s.SetTaskStatus(S(coord, "id"), TaskWaiting, "waiting on delegated work"); err != nil {
		t.Fatal(err)
	}
	_, _, err = s.CreateTask(CreateTaskOptions{
		ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "critic",
		Title: "Critic", Objective: "Critique", Reason: "handoff",
		ParentTaskID: S(coord, "id"), RequestedByTaskID: S(coord, "id"),
		DependsOn: []string{S(coord, "id")}, WorkspaceMode: "collection", Depth: 1, AllowDedupe: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	state, err := s.UpdateCollectionTerminalState(S(c, "id"))
	if err != nil {
		t.Fatal(err)
	}
	if state != "RUNNING" {
		t.Fatalf("live durable session must keep collection RUNNING, got %s", state)
	}
}

func TestV021CriticSealRemainsCompatibleAfterResearchDigestUpgrade(t *testing.T) {
	s, _ := newFakeStore(t)
	p, _ := s.RegisterProject(testProjectConfig(t.TempDir()))
	c, coord, err := s.CreateCollectionWithCoordinator(
		S(p, "id"),
		"Current framework docs",
		"Update README with the latest current framework guidance and add tests for the documented example.",
		"Plan",
	)
	if err != nil {
		t.Fatal(err)
	}
	research, _, err := s.CreateTask(CreateTaskOptions{
		ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "research",
		Title: "Verify current framework guidance (first-party)", Objective: "Research current framework guidance", Reason: "operator requested research",
		ParentTaskID: S(coord, "id"), RequestedByTaskID: S(coord, "id"), WorkspaceMode: "collection", Depth: 1, AllowDedupe: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	critic, _, err := s.CreateTask(CreateTaskOptions{
		ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "critic",
		Title: "Preflight implementation plan", Objective: "Critique current documentation plan", Reason: "policy",
		ParentTaskID: S(coord, "id"), RequestedByTaskID: S(coord, "id"), DependsOn: []string{S(research, "id")}, WorkspaceMode: "collection", Depth: 1, AllowDedupe: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	impl, _, err := s.CreateTask(CreateTaskOptions{
		ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "implement",
		Title: "Implement current docs update", Objective: "Implement researched current guidance", Reason: "plan",
		ParentTaskID: S(coord, "id"), RequestedByTaskID: S(coord, "id"), WorkspaceMode: "collection", Depth: 1, AllowDedupe: false,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, _ = s.SubmitResult(S(research, "id"), Row{"status": "complete", "summary": "fresh framework evidence", "evidence": []any{"official documentation"}})
	if err = s.SetTaskStatus(S(research, "id"), TaskComplete, "research complete"); err != nil {
		t.Fatal(err)
	}
	_, _ = s.SubmitResult(S(critic, "id"), Row{"status": "complete", "summary": "preflight complete"})
	if err = s.SetTaskStatus(S(critic, "id"), TaskComplete, "critic complete"); err != nil {
		t.Fatal(err)
	}

	// Simulate the material-plan seal produced by 0.2.1, which did not include
	// completed research result bytes in the fingerprint.
	tasks, err := s.ListTasks(S(c, "id"))
	if err != nil {
		t.Fatal(err)
	}
	edges, err := s.ListEdges(S(c, "id"))
	if err != nil {
		t.Fatal(err)
	}
	v021Fingerprint := MaterialPlanFingerprintV021(S(c, "objective"), tasks, edges, false)
	currentFingerprint := MaterialPlanFingerprint(S(c, "objective"), tasks, edges, false)
	if v021Fingerprint == currentFingerprint {
		t.Fatal("test setup requires 0.2.2 research digest to change the material fingerprint")
	}
	plan, _, err := s.CreateTask(CreateTaskOptions{
		ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "plan_review",
		Title: "Review current docs work graph", Objective: "Review plan", Reason: "gate",
		ParentTaskID: S(coord, "id"), DependsOn: []string{S(critic, "id")}, WorkspaceMode: "collection", Depth: 1, AllowDedupe: false,
		GateImplementTasks: true, CriticBaselineTaskID: S(critic, "id"), CriticBaselineFingerprint: v021Fingerprint, CriticBaselineSource: "initial_plan_seal",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = s.SubmitResult(S(plan, "id"), Row{"status": "complete", "summary": "pass", "verdict": "pass"})
	if err = s.SetTaskStatus(S(plan, "id"), TaskComplete, "plan review complete"); err != nil {
		t.Fatal(err)
	}
	_, _ = s.SubmitResult(S(coord, "id"), Row{"status": "complete", "summary": "planned"})
	if err = s.SetTaskStatus(S(coord, "id"), TaskComplete, "coordinator complete"); err != nil {
		t.Fatal(err)
	}
	_, _ = s.SubmitResult(S(impl, "id"), Row{"status": "complete", "summary": "implemented"})
	if err = s.SetTaskStatus(S(impl, "id"), TaskComplete, "implementation complete"); err != nil {
		t.Fatal(err)
	}
	review, _, err := s.CreateTask(CreateTaskOptions{
		ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "review",
		Title: "Review current docs update", Objective: "Review implementation", Reason: "auto",
		ParentTaskID: S(impl, "id"), DependsOn: []string{S(impl, "id")}, WorkspaceMode: "collection", Depth: 2, AllowDedupe: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.AddEdge(S(impl, "id"), S(review, "id"), S(c, "id"), "verifies"); err != nil {
		t.Fatal(err)
	}
	_, _ = s.SubmitResult(S(review, "id"), Row{"status": "complete", "summary": "pass", "verdict": "pass"})
	if err = s.SetTaskStatus(S(review, "id"), TaskComplete, "review complete"); err != nil {
		t.Fatal(err)
	}

	state, err := s.CriticRevalidationState(S(c, "id"))
	if err != nil {
		t.Fatal(err)
	}
	if B(state, "stale") || S(state, "compatible_via") != "v0.2.1_material_plan" {
		t.Fatalf("0.2.1 critic seal should remain upgrade-compatible: %v", state)
	}
	terminal, err := s.UpdateCollectionTerminalState(S(c, "id"))
	if err != nil {
		t.Fatal(err)
	}
	if terminal != "COMPLETE" {
		t.Fatalf("upgraded completed collection must remain COMPLETE, got %s", terminal)
	}
	if _, _, err = s.RequestCollectionRevision(S(c, "id"), "Correct remaining documentation issues"); err != nil {
		t.Fatalf("0.2.1-complete collection must remain revisable after 0.2.2 upgrade: %v", err)
	}
}

func TestClaimGateRejectsLegacyCriticUnderTransitiveResearchContract(t *testing.T) {
	s, _ := newFakeStore(t)
	p, _ := s.RegisterProject(testProjectConfig(t.TempDir()))
	c, coord, err := s.CreateCollectionWithCoordinator(S(p, "id"), "MCP", "Implement support for the current MCP protocol specification in an HTTP server.", "Plan")
	if err != nil {
		t.Fatal(err)
	}
	research, _, err := s.CreateTask(CreateTaskOptions{
		ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "research", Title: "Fresh external research preflight",
		Objective: "research", Reason: "fresh external evidence", ParentTaskID: S(coord, "id"), RequestedByTaskID: S(coord, "id"), WorkspaceMode: "collection", Depth: 1, AllowDedupe: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SubmitResult(S(research, "id"), validFreshResearchResult()); err != nil {
		t.Fatal(err)
	}
	if err = s.SetTaskStatus(S(research, "id"), TaskComplete, "research complete"); err != nil {
		t.Fatal(err)
	}
	critic, _, err := s.CreateTask(CreateTaskOptions{
		ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "critic", Title: "Preflight implementation plan",
		Objective: "critique", Reason: "policy", ParentTaskID: S(coord, "id"), RequestedByTaskID: S(coord, "id"), DependsOn: []string{S(research, "id")}, WorkspaceMode: "collection", Depth: 1, AllowDedupe: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	legacy := Row{"status": "complete", "summary": "legacy critic", "research_consistency": Row{"verdict": "pass", "checked_claim_ids": []any{"claim-x"}, "contradictions": []any{}}}
	if err = s.Mutate("UPDATE tasks SET status='COMPLETE',result_json="+SQLLiteral(JSONDumps(legacy))+" WHERE id="+SQLLiteral(S(critic, "id")), "seed legacy critic"); err != nil {
		t.Fatal(err)
	}
	plan, _, err := s.CreateTask(CreateTaskOptions{
		ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "plan_review", Title: "Review MCP plan", Objective: "Review plan", Reason: "gate",
		ParentTaskID: S(coord, "id"), RequestedByTaskID: S(coord, "id"), DependsOn: []string{S(critic, "id")}, WorkspaceMode: "collection", Depth: 1, AllowDedupe: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	fp, err := s.MaterialPlanFingerprint(S(c, "id"), false)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RecordCriticPlanBaseline(S(critic, "id"), fp, "test", S(plan, "id")); err != nil {
		t.Fatal(err)
	}
	claimed, attempt, err := s.ClaimTask(S(plan, "id"), 3)
	if err != nil {
		t.Fatal(err)
	}
	if claimed != nil || attempt != nil {
		t.Fatalf("plan review claimed with legacy critic that cannot satisfy current research contract: task=%v attempt=%v", claimed, attempt)
	}
}

func TestTransitiveResearchContractReachesPlanReviewImplementationAndReview(t *testing.T) {
	s, _ := newFakeStore(t)
	p, _ := s.RegisterProject(testProjectConfig(t.TempDir()))
	c, coord, err := s.CreateCollectionWithCoordinator(S(p, "id"), "MCP", "Implement support for the current MCP protocol specification in an HTTP server.", "Plan")
	if err != nil {
		t.Fatal(err)
	}
	research, _, err := s.CreateTask(CreateTaskOptions{
		ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "research", Title: "Fresh external research preflight",
		Objective: "research", Reason: "fresh external evidence", ParentTaskID: S(coord, "id"), RequestedByTaskID: S(coord, "id"), WorkspaceMode: "collection", Depth: 1, AllowDedupe: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SubmitResult(S(research, "id"), validFreshResearchResult()); err != nil {
		t.Fatal(err)
	}
	_ = s.SetTaskStatus(S(research, "id"), TaskComplete, "research complete")

	critic, _, err := s.CreateTask(CreateTaskOptions{
		ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "critic", Title: "Preflight implementation plan", Objective: "critique", Reason: "policy",
		ParentTaskID: S(coord, "id"), RequestedByTaskID: S(coord, "id"), DependsOn: []string{S(research, "id")}, WorkspaceMode: "collection", Depth: 1, AllowDedupe: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SubmitResult(S(critic, "id"), Row{"status": "complete", "summary": "critic", "research_consistency": researchConsistencyFor(Rows{mustGetTask(t, s, S(research, "id"))})}); err != nil {
		t.Fatal(err)
	}
	_ = s.SetTaskStatus(S(critic, "id"), TaskComplete, "critic complete")

	plan, _, err := s.CreateTask(CreateTaskOptions{
		ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "plan_review", Title: "Review MCP plan", Objective: "Review plan", Reason: "gate",
		ParentTaskID: S(coord, "id"), RequestedByTaskID: S(coord, "id"), DependsOn: []string{S(critic, "id")}, WorkspaceMode: "collection", Depth: 1, AllowDedupe: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	impl, _, err := s.CreateTask(CreateTaskOptions{
		ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "implement", Title: "Implement MCP", Objective: "Implement the reviewed plan", Reason: "plan",
		ParentTaskID: S(coord, "id"), RequestedByTaskID: S(coord, "id"), DependsOn: []string{S(plan, "id")}, WorkspaceMode: "collection", Depth: 1, AllowDedupe: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	fp, err := s.MaterialPlanFingerprint(S(c, "id"), false)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RecordCriticPlanBaseline(S(critic, "id"), fp, "test", S(plan, "id")); err != nil {
		t.Fatal(err)
	}
	claimedPlan, planAttempt, err := s.ClaimTask(S(plan, "id"), 3)
	if err != nil {
		t.Fatal(err)
	}
	if claimedPlan == nil || planAttempt == nil {
		t.Fatal("valid plan review was not claimable")
	}
	if _, err = s.SubmitResult(S(plan, "id"), Row{"status": "complete", "summary": "pass", "verdict": "pass", "research_consistency": researchConsistencyFor(Rows{mustGetTask(t, s, S(research, "id"))})}); err != nil {
		t.Fatalf("plan review did not validate transitive research ancestry: %v", err)
	}
	_ = s.SetTaskStatus(S(plan, "id"), TaskComplete, "plan review complete")
	if _, err = s.SubmitResult(S(coord, "id"), Row{"status": "complete", "summary": "planned"}); err != nil {
		t.Fatal(err)
	}
	_ = s.SetTaskStatus(S(coord, "id"), TaskComplete, "coordinator complete")

	ancestors, err := s.ResearchAncestors(S(impl, "id"))
	if err != nil {
		t.Fatal(err)
	}
	if len(ancestors) != 1 || S(ancestors[0], "id") != S(research, "id") {
		t.Fatalf("implementation lost transitive research ancestry: %v", ancestors)
	}
	claimedImpl, implAttempt, err := s.ClaimTask(S(impl, "id"), 3)
	if err != nil {
		t.Fatal(err)
	}
	if claimedImpl == nil || implAttempt == nil {
		t.Fatal("implementation was not claimable after valid transitive research-aware gates")
	}
}

func mustGetTask(t *testing.T, s *Store, id string) Row {
	t.Helper()
	row, err := s.GetTask(id)
	if err != nil {
		t.Fatal(err)
	}
	return row
}
