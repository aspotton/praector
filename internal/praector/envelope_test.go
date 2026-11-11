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
	for _, want := range []string{"## Capability discipline", "no net workspace mutation", "PYTHONDONTWRITEBYTECODE=1", "final worktree state matches the starting baseline", "Never discard pre-existing changes"} {
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
}
