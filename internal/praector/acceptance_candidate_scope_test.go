package praector

import (
	"strings"
	"testing"
)

func candidateScopeProject(t *testing.T) (*Store, Row, Row, Row) {
	t.Helper()
	s, _ := newFakeStore(t)
	p, err := s.RegisterProject(testProjectConfig(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	c, coord, err := s.CreateCollectionWithCoordinator(S(p, "id"), "Candidate scope", "The weather report", "Plan")
	if err != nil {
		t.Fatal(err)
	}
	return s, p, c, coord
}

func candidateScopeGeneration(t *testing.T, s *Store, p, c, coord Row, title string, nameCandidate bool) Row {
	t.Helper()
	impl, _, err := s.CreateTask(CreateTaskOptions{
		ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "implement", Title: title + " implement",
		Objective: "implement", Reason: "plan", ParentTaskID: S(coord, "id"), WorkspaceMode: "collection", Depth: 1, AllowDedupe: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	review, _, err := s.CreateTask(CreateTaskOptions{
		ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "review", Title: title + " review",
		Objective: "review", Reason: "policy", ParentTaskID: S(impl, "id"), DependsOn: []string{S(impl, "id")}, WorkspaceMode: "collection", Depth: 2, AllowDedupe: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.AddEdge(S(impl, "id"), S(review, "id"), S(c, "id"), "verifies"); err != nil {
		t.Fatal(err)
	}
	sites := []any{Row{"site": "internal/praector/store.go", "disposition": "unaffected", "evidence": "go test ./internal/praector/ -run TestAcceptance"}}
	result := Row{"status": "complete", "summary": "pass", "verdict": "pass", "impact_enumeration": Row{"method": "call-graph walk", "sites": sites}}
	if nameCandidate {
		result["impact_enumeration"] = Row{"method": "call-graph walk", "sites": sites, "candidate_dispositions": []any{
			Row{"candidate": "--derive-test-knob", "disposition": "not-introduced", "evidence": "predates this collection per git log internal/praector/config.go"},
		}}
	}
	if _, err = s.SubmitResult(S(review, "id"), result); err != nil {
		t.Fatal(err)
	}
	if err = s.SetTaskStatus(S(review, "id"), TaskComplete, "review complete"); err != nil {
		t.Fatal(err)
	}
	if err = s.SetTaskStatus(S(impl, "id"), TaskComplete, "implement complete"); err != nil {
		t.Fatal(err)
	}
	return review
}

// A revised Collection must stay acceptable: the diff-derived candidate list is
// recomputed against the final integration branch, so only the newest passing
// review's enumeration is required to name it.
func TestAcceptanceDerivedCandidatesGovernedByNewestReview(t *testing.T) {
	s, p, c, coord := candidateScopeProject(t)
	candidates := []DerivedConfigCandidate{{Token: "--derive-test-knob", Path: "cmd.go"}}
	older := candidateScopeGeneration(t, s, p, c, coord, "first", false)
	newer := candidateScopeGeneration(t, s, p, c, coord, "second", true)
	if err := s.Mutate("UPDATE tasks SET created_at='2030-01-01 00:00:00' WHERE id="+SQLLiteral(S(newer, "id")), "make the second review newest"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SubmitResult(S(coord, "id"), Row{"status": "complete", "summary": "planned"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTaskStatus(S(coord, "id"), TaskComplete, "planning complete"); err != nil {
		t.Fatal(err)
	}
	collection, _ := s.GetCollection(S(c, "id"))
	if _, _, err := validateCollectionForAcceptance(s, collection, candidates...); err != nil {
		t.Fatalf("newest passing review names the candidate, so the older generation must not block acceptance: %v", err)
	}

	if err := s.Mutate("UPDATE tasks SET created_at='2031-01-01 00:00:00' WHERE id="+SQLLiteral(S(older, "id")), "make the candidate-blind review newest"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := validateCollectionForAcceptance(s, collection, candidates...); err == nil || !strings.Contains(err.Error(), "--derive-test-knob") {
		t.Fatalf("the governing review omitting a derived candidate must still block acceptance naming it: %v", err)
	}
}

// The documented legacy exception clears a review result written before the
// impact-enumeration validators existed, even when the diff yields candidates.
func TestAcceptanceLegacyExceptionClearsDerivedCandidateAccounting(t *testing.T) {
	s, p, c, coord := candidateScopeProject(t)
	candidates := []DerivedConfigCandidate{{Token: "--derive-test-knob", Path: "cmd.go"}}
	impl, _, err := s.CreateTask(CreateTaskOptions{
		ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "implement", Title: "Implement",
		Objective: "implement", Reason: "plan", ParentTaskID: S(coord, "id"), WorkspaceMode: "collection", Depth: 1, AllowDedupe: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	review, _, err := s.CreateTask(CreateTaskOptions{
		ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "review", Title: "Review",
		Objective: "review", Reason: "policy", ParentTaskID: S(impl, "id"), DependsOn: []string{S(impl, "id")}, WorkspaceMode: "collection", Depth: 2, AllowDedupe: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.AddEdge(S(impl, "id"), S(review, "id"), S(c, "id"), "verifies"); err != nil {
		t.Fatal(err)
	}
	if err = s.Mutate("UPDATE tasks SET result_json="+SQLLiteral(JSONDumps(Row{"status": "complete", "summary": "pass", "verdict": "pass"}))+" WHERE id="+SQLLiteral(S(review, "id")), "plant a pre-validator review result"); err != nil {
		t.Fatal(err)
	}
	if err = s.SetTaskStatus(S(review, "id"), TaskComplete, "review complete"); err != nil {
		t.Fatal(err)
	}
	if err = s.SetTaskStatus(S(impl, "id"), TaskComplete, "implement complete"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SubmitResult(S(coord, "id"), Row{"status": "complete", "summary": "planned"}); err != nil {
		t.Fatal(err)
	}
	if err = s.SetTaskStatus(S(coord, "id"), TaskComplete, "planning complete"); err != nil {
		t.Fatal(err)
	}
	collection, _ := s.GetCollection(S(c, "id"))
	if _, _, err = validateCollectionForAcceptance(s, collection, candidates...); err == nil || !strings.Contains(err.Error(), "--derive-test-knob") {
		t.Fatalf("a candidate-blind pre-validator review must block acceptance without the exception: %v", err)
	}
	if _, err = s.AddDecision(S(p, "id"), S(c, "id"), "", LegacyGateTraceabilityMarker+" pre-traceability review results accepted as-is for "+S(c, "id"), "Validators postdate this collection's reviews", []any{}, []any{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err = validateCollectionForAcceptance(s, collection, candidates...); err != nil {
		t.Fatalf("the documented legacy exception must clear derived-candidate accounting: %v", err)
	}
}
