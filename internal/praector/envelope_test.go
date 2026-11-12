package praector

import (
	"strings"
	"testing"
)

func TestBuildTaskEnvelopeInjectsReadOnlyWorkspaceHygieneOnlyForReadOnlyProfiles(t *testing.T) {
	store, _ := newFakeStore(t)
	project, err := store.RegisterProject(testProjectConfig(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	collection, coordinator, err := store.CreateCollectionWithCoordinator(S(project, "id"), "Feature", "Add a small endpoint", "Plan")
	if err != nil {
		t.Fatal(err)
	}
	implement, _, err := store.CreateTask(CreateTaskOptions{
		ProjectID: S(project, "id"), CollectionID: S(collection, "id"), Profile: "implement",
		Title: "Implement", Objective: "Implement it", Reason: "plan", ParentTaskID: S(coordinator, "id"),
		RequestedByTaskID: S(coordinator, "id"), WorkspaceMode: "collection", Depth: 1, AllowDedupe: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	review, _, err := store.CreateTask(CreateTaskOptions{
		ProjectID: S(project, "id"), CollectionID: S(collection, "id"), Profile: "review",
		Title: "Review", Objective: "Review it", Reason: "policy", ParentTaskID: S(implement, "id"),
		RequestedByTaskID: S(coordinator, "id"), DependsOn: []string{S(implement, "id")}, WorkspaceMode: "collection", Depth: 2, AllowDedupe: false,
	})
	if err != nil {
		t.Fatal(err)
	}

	reviewPrompt, err := BuildTaskEnvelope(store, project, collection, review, Row{"id": "A-review", "attempt_no": 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"## Capability discipline", "no net workspace mutation", "PYTHONDONTWRITEBYTECODE=1", "assigned detached HEAD", "final worktree state matches the starting baseline", "Never discard pre-existing changes"} {
		if !strings.Contains(reviewPrompt, want) {
			t.Fatalf("rendered review envelope missing %q", want)
		}
	}

	implementPrompt, err := BuildTaskEnvelope(store, project, collection, implement, Row{"id": "A-implement", "attempt_no": 1})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(implementPrompt, "no net workspace mutation") {
		t.Fatal("implement envelope incorrectly received read-only workspace hygiene contract")
	}
	if !strings.Contains(implementPrompt, "write capability within its assigned implementation scope") {
		t.Fatal("implement envelope missing write-capability contract")
	}
	if !strings.Contains(implementPrompt, "Praector has already created and checked out the only Task branch") {
		t.Fatal("implement envelope missing Praector-owned branch contract")
	}
}

func TestBuildTaskEnvelopeProjectsInheritedResearchConstraints(t *testing.T) {
	store, _ := newFakeStore(t)
	project, err := store.RegisterProject(testProjectConfig(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	collection, coordinator, err := store.CreateCollectionWithCoordinator(S(project, "id"), "Protocol", "Implement support for the current MCP protocol specification in an HTTP server.", "Plan")
	if err != nil {
		t.Fatal(err)
	}
	research, _, err := store.CreateTask(CreateTaskOptions{ProjectID: S(project, "id"), CollectionID: S(collection, "id"), Profile: "research", Title: "Fresh external research preflight", Objective: "research", Reason: "fresh external evidence", ParentTaskID: S(coordinator, "id"), RequestedByTaskID: S(coordinator, "id"), WorkspaceMode: "collection", Depth: 1, AllowDedupe: false})
	if err != nil {
		t.Fatal(err)
	}
	result := validFreshResearchResult()
	result["claims"].([]any)[0].(Row)["planner_directive"] = "Missing protocolVersion MUST return Invalid Params."
	if _, err = store.SubmitResult(S(research, "id"), result); err != nil {
		t.Fatal(err)
	}
	if err = store.SetTaskStatus(S(research, "id"), TaskComplete, "done"); err != nil {
		t.Fatal(err)
	}
	critic, _, err := store.CreateTask(CreateTaskOptions{ProjectID: S(project, "id"), CollectionID: S(collection, "id"), Profile: "critic", Title: "Critic", Objective: "critique", Reason: "policy", ParentTaskID: S(coordinator, "id"), RequestedByTaskID: S(coordinator, "id"), DependsOn: []string{S(research, "id")}, WorkspaceMode: "collection", Depth: 1, AllowDedupe: false})
	if err != nil {
		t.Fatal(err)
	}
	prompt, err := BuildTaskEnvelope(store, project, collection, critic, Row{"id": "A-critic", "attempt_no": 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"\"inherited_research_constraints\"", "\"claim_id\": \"claim-x\"", "Missing protocolVersion MUST return Invalid Params."} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("rendered critic envelope missing %q", want)
		}
	}

	planReview, _, err := store.CreateTask(CreateTaskOptions{ProjectID: S(project, "id"), CollectionID: S(collection, "id"), Profile: "plan_review", Title: "Plan review", Objective: "Review plan", Reason: "gate", ParentTaskID: S(coordinator, "id"), RequestedByTaskID: S(coordinator, "id"), DependsOn: []string{S(critic, "id")}, WorkspaceMode: "collection", Depth: 1, AllowDedupe: false})
	if err != nil {
		t.Fatal(err)
	}
	implement, _, err := store.CreateTask(CreateTaskOptions{ProjectID: S(project, "id"), CollectionID: S(collection, "id"), Profile: "implement", Title: "Implement", Objective: "Apply plan", Reason: "plan", ParentTaskID: S(coordinator, "id"), RequestedByTaskID: S(coordinator, "id"), DependsOn: []string{S(planReview, "id")}, WorkspaceMode: "collection", Depth: 1, AllowDedupe: false})
	if err != nil {
		t.Fatal(err)
	}
	implementPrompt, err := BuildTaskEnvelope(store, project, collection, implement, Row{"id": "A-implement-research", "attempt_no": 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"\"inherited_research_constraints\"", "Missing protocolVersion MUST return Invalid Params.", "higher-precedence implementation requirements"} {
		if !strings.Contains(implementPrompt, want) {
			t.Fatalf("transitive implement envelope missing %q", want)
		}
	}
}
