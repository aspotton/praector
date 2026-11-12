package praector

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// zeroDirectiveCriticResult is a completed planning-stage result whose profile
// is directive-bearing (critic) but which yields ZERO harvestable directives,
// so a consumer that depends on it is the empty-harvest case (ancestor present,
// harvest empty) rather than the genuinely directive-free case.
func zeroDirectiveCriticResult() Row {
	return Row{"status": "complete", "summary": "preflight complete", "planner_directives": []any{}, "qa_directives": []any{}, "risks": []any{}}
}

func countEvents(t *testing.T, s *Store, cid, eventType string) int {
	t.Helper()
	events, err := s.EventsForCollection(cid, 500)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, ev := range events {
		if S(ev, "event_type") == eventType {
			n++
		}
	}
	return n
}

// gitMainRepo creates a temporary git repository with a `main` branch holding a
// single commit, so the base branch exists while a collection's integration
// branch need not.
func gitMainRepo(t *testing.T) string {
	t.Helper()
	git, err := RequireBinary("git")
	if err != nil {
		t.Skip("git unavailable")
	}
	repo := t.TempDir()
	if _, err = Run(context.Background(), []string{git, "init", "-b", "main", repo}, "", nil, "", true); err != nil {
		t.Fatal(err)
	}
	gitTest(t, repo, "config", "user.email", "praector@test")
	gitTest(t, repo, "config", "user.name", "Praector Test")
	if err = os.WriteFile(filepath.Join(repo, "base.txt"), []byte("base\n"), 0644); err != nil {
		t.Fatal(err)
	}
	gitTest(t, repo, "add", ".")
	gitTest(t, repo, "commit", "-m", "base")
	return repo
}

// TestAbsentConfigBasisRecordedAtSeam proves an absent diff basis (an absent
// integration branch, or a non-Git root) is surfaced as ONE deduped durable
// event naming the collection and the reason, and that this is distinct from a
// genuine derivable-but-no-knob diff, which stays silent. This is the shared
// seam helper both the submit path and the two acceptance paths call, so all
// three consumers surface the state identically.
func TestAbsentConfigBasisRecordedAtSeam(t *testing.T) {
	repo := gitMainRepo(t)
	s, cfg := newFakeStore(t)
	svc := &Service{store: s, cfg: cfg}
	project := Row{"id": "PRJ-basis", "root_path": repo, "base_branch": "main"}

	absent := Row{"id": "COL-absent", "integration_branch": "praector/collections/does-not-exist"}
	candidates, err := svc.deriveConfigCandidates(project, absent)
	if err != nil || len(candidates) != 0 {
		t.Fatalf("an absent integration branch yields no candidates and no error: %#v %v", candidates, err)
	}
	if n := countEvents(t, s, "COL-absent", "CONFIG_BASIS_UNAVAILABLE"); n != 1 {
		t.Fatalf("an absent basis must record exactly one event: %d", n)
	}
	// Neuter-and-restore: repeat calls must not spam the events table.
	if _, err = svc.deriveConfigCandidates(project, absent); err != nil {
		t.Fatal(err)
	}
	if _, err = svc.deriveConfigCandidates(project, absent); err != nil {
		t.Fatal(err)
	}
	if n := countEvents(t, s, "COL-absent", "CONFIG_BASIS_UNAVAILABLE"); n != 1 {
		t.Fatalf("the absent-basis event must be deduped per collection: %d", n)
	}

	// A derivable basis with zero candidates must NOT be recorded as absent.
	present := Row{"id": "COL-present", "integration_branch": "main"}
	if _, err = svc.deriveConfigCandidates(project, present); err != nil {
		t.Fatal(err)
	}
	if n := countEvents(t, s, "COL-present", "CONFIG_BASIS_UNAVAILABLE"); n != 0 {
		t.Fatalf("a derivable no-knob basis must record no absent-basis event: %d", n)
	}

	// A non-Git root is also an absent basis, recorded once and naming the reason.
	nongit := Row{"id": "PRJ-nongit", "root_path": filepath.Join(t.TempDir(), "not-a-repo"), "base_branch": "main"}
	nongitCollection := Row{"id": "COL-nongit", "integration_branch": "praector/collections/x"}
	if _, err = svc.deriveConfigCandidates(nongit, nongitCollection); err != nil {
		t.Fatal(err)
	}
	if n := countEvents(t, s, "COL-nongit", "CONFIG_BASIS_UNAVAILABLE"); n != 1 {
		t.Fatalf("a non-Git root must record exactly one absent-basis event: %d", n)
	}
	events, _ := s.EventsForCollection("COL-absent", 500)
	found := false
	for _, ev := range events {
		if S(ev, "event_type") == "CONFIG_BASIS_UNAVAILABLE" && strings.Contains(S(ev, "payload_json"), "praector/collections/does-not-exist") {
			found = true
		}
	}
	if !found {
		t.Fatal("the absent-basis event payload must name the missing integration ref")
	}
}

// TestEmptyDirectiveHarvestRecordedByObserver proves a completed review/plan_review
// consumer whose directive-bearing ancestor yields zero harvestable directives
// emits ONE deduped DIRECTIVE_TRACEABILITY_EMPTY_HARVEST event naming the
// consumer and the checked source task ids, while a consumer with NO
// directive-bearing ancestor stays completely silent.
func TestEmptyDirectiveHarvestRecordedByObserver(t *testing.T) {
	s, _ := newFakeStore(t)
	p, err := s.RegisterProject(testProjectConfig(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	c, coord, err := s.CreateCollectionWithCoordinator(S(p, "id"), "Empty harvest", "Add authenticated endpoint with durable events", "Plan")
	if err != nil {
		t.Fatal(err)
	}
	critic, _, _ := s.CreateTask(CreateTaskOptions{
		ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "critic", Title: "Preflight",
		Objective: "preflight", Reason: "plan", ParentTaskID: S(coord, "id"), WorkspaceMode: "collection", Depth: 1, AllowDedupe: false,
	})
	_, _ = s.SubmitResult(S(critic, "id"), zeroDirectiveCriticResult())
	if err = s.SetTaskStatus(S(critic, "id"), TaskComplete, "critic complete"); err != nil {
		t.Fatal(err)
	}
	impl, _, _ := s.CreateTask(CreateTaskOptions{
		ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "implement", Title: "Implement",
		Objective: "implement", Reason: "plan", ParentTaskID: S(critic, "id"), DependsOn: []string{S(critic, "id")}, WorkspaceMode: "collection", Depth: 2, AllowDedupe: false,
	})
	review, _, _ := s.CreateTask(CreateTaskOptions{
		ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "review", Title: "Review",
		Objective: "review", Reason: "policy", ParentTaskID: S(impl, "id"), DependsOn: []string{S(impl, "id"), S(critic, "id")}, WorkspaceMode: "collection", Depth: 3, AllowDedupe: false,
	})
	_ = s.SetTaskStatus(S(review, "id"), TaskComplete, "review complete")

	// A second review with NO directive-bearing ancestor exercises the silent
	// clean-pass branch (TestDirectiveTraceabilityZeroDirectivesPassesCleanly
	// semantics) and must never be recorded.
	impl2, _, _ := s.CreateTask(CreateTaskOptions{
		ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "implement", Title: "Implement other",
		Objective: "implement other", Reason: "plan", ParentTaskID: S(coord, "id"), WorkspaceMode: "collection", Depth: 2, AllowDedupe: false,
	})
	cleanReview, _, _ := s.CreateTask(CreateTaskOptions{
		ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "review", Title: "Review other",
		Objective: "review other", Reason: "policy", ParentTaskID: S(impl2, "id"), DependsOn: []string{S(impl2, "id")}, WorkspaceMode: "collection", Depth: 3, AllowDedupe: false,
	})
	_ = s.SetTaskStatus(S(cleanReview, "id"), TaskComplete, "clean review complete")

	sch := &Scheduler{Store: s}
	review, _ = s.GetTask(S(review, "id"))
	cleanReview, _ = s.GetTask(S(cleanReview, "id"))
	sch.observeDirectiveTraceabilityGaps(S(c, "id"), Rows{review, cleanReview}, critic)

	if n := countEvents(t, s, S(c, "id"), "DIRECTIVE_TRACEABILITY_EMPTY_HARVEST"); n != 1 {
		t.Fatalf("exactly one empty-harvest event (empty-harvest consumer only) expected: %d", n)
	}
	events, _ := s.EventsForCollection(S(c, "id"), 500)
	namedConsumer, namedSource, cleanSilent := false, false, true
	for _, ev := range events {
		if S(ev, "event_type") != "DIRECTIVE_TRACEABILITY_EMPTY_HARVEST" {
			continue
		}
		if S(ev, "task_id") == S(review, "id") {
			namedConsumer = true
			if strings.Contains(S(ev, "payload_json"), S(critic, "id")) {
				namedSource = true
			}
		}
		if S(ev, "task_id") == S(cleanReview, "id") {
			cleanSilent = false
		}
	}
	if !namedConsumer || !namedSource {
		t.Fatalf("the empty-harvest event must name the consumer task and the checked source task ids (consumer=%v source=%v)", namedConsumer, namedSource)
	}
	if !cleanSilent {
		t.Fatal("a genuinely directive-free lineage must emit no empty-harvest event")
	}

	// A repeated sweep must not add a second event.
	sch.observeDirectiveTraceabilityGaps(S(c, "id"), Rows{review, cleanReview}, critic)
	if n := countEvents(t, s, S(c, "id"), "DIRECTIVE_TRACEABILITY_EMPTY_HARVEST"); n != 1 {
		t.Fatalf("the empty-harvest event must be deduped per consumer across sweeps: %d", n)
	}
}

// TestEmptyDirectiveHarvestRecordedAtAcceptance proves the acceptance seam also
// records the empty-harvest event (observability only) for a completed review
// whose directive-bearing ancestor yielded zero directives.
func TestEmptyDirectiveHarvestRecordedAtAcceptance(t *testing.T) {
	s, _ := newFakeStore(t)
	p, err := s.RegisterProject(testProjectConfig(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	c, coord, err := s.CreateCollectionWithCoordinator(S(p, "id"), "Empty harvest acceptance", "Add authenticated endpoint with durable events", "Plan")
	if err != nil {
		t.Fatal(err)
	}
	critic, _, _ := s.CreateTask(CreateTaskOptions{
		ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "critic", Title: "Preflight",
		Objective: "preflight", Reason: "plan", ParentTaskID: S(coord, "id"), WorkspaceMode: "collection", Depth: 1, AllowDedupe: false,
	})
	_, _ = s.SubmitResult(S(critic, "id"), zeroDirectiveCriticResult())
	_ = s.SetTaskStatus(S(critic, "id"), TaskComplete, "critic complete")
	impl, _, _ := s.CreateTask(CreateTaskOptions{
		ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "implement", Title: "Implement",
		Objective: "implement", Reason: "plan", ParentTaskID: S(coord, "id"), DependsOn: []string{S(critic, "id")}, WorkspaceMode: "collection", Depth: 1, AllowDedupe: false,
	})
	_ = s.SetTaskStatus(S(impl, "id"), TaskComplete, "complete")
	review, _, _ := s.CreateTask(CreateTaskOptions{
		ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "review", Title: "Review",
		Objective: "review", Reason: "policy", ParentTaskID: S(impl, "id"), DependsOn: []string{S(impl, "id"), S(critic, "id")}, WorkspaceMode: "collection", Depth: 2, AllowDedupe: false,
	})
	_ = s.AddEdge(S(impl, "id"), S(review, "id"), S(c, "id"), "verifies")
	if _, err = s.SubmitResult(S(review, "id"), Row{"status": "complete", "verdict": "pass", "summary": "clean", "impact_enumeration": passingImpactEnumeration()}); err != nil {
		t.Fatal(err)
	}
	_ = s.SetTaskStatus(S(review, "id"), TaskComplete, "review complete")
	if _, err = s.SubmitResult(S(coord, "id"), Row{"status": "complete", "summary": "planned"}); err != nil {
		t.Fatal(err)
	}
	_ = s.SetTaskStatus(S(coord, "id"), TaskComplete, "planning complete")

	collection, _ := s.GetCollection(S(c, "id"))
	_, _, _ = validateCollectionForAcceptance(s, collection)

	if n := countEvents(t, s, S(c, "id"), "DIRECTIVE_TRACEABILITY_EMPTY_HARVEST"); n != 1 {
		t.Fatalf("the acceptance seam must record the empty-harvest event once: %d", n)
	}
	// A second acceptance check must not duplicate it.
	_, _, _ = validateCollectionForAcceptance(s, collection)
	if n := countEvents(t, s, S(c, "id"), "DIRECTIVE_TRACEABILITY_EMPTY_HARVEST"); n != 1 {
		t.Fatalf("the acceptance seam empty-harvest event must be deduped: %d", n)
	}
}

// TestClaimTaskOnZeroHarvestLineageDoesNotHang is the no-deadlock smoke: the
// claim-time directive gate sees a zero-harvest ancestor lineage but must never
// call Store.Event under the store lock, so ClaimTask returns under a timeout.
func TestClaimTaskOnZeroHarvestLineageDoesNotHang(t *testing.T) {
	s, _ := newFakeStore(t)
	p, err := s.RegisterProject(testProjectConfig(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	c, coord, err := s.CreateCollectionWithCoordinator(S(p, "id"), "Zero harvest claim", "Add authenticated endpoint with durable events", "Plan")
	if err != nil {
		t.Fatal(err)
	}
	critic, _, _ := s.CreateTask(CreateTaskOptions{
		ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "critic", Title: "Preflight",
		Objective: "preflight", Reason: "plan", ParentTaskID: S(coord, "id"), WorkspaceMode: "collection", Depth: 1, AllowDedupe: false,
	})
	_, _ = s.SubmitResult(S(critic, "id"), zeroDirectiveCriticResult())
	if err = s.SetTaskStatus(S(critic, "id"), TaskComplete, "critic complete"); err != nil {
		t.Fatal(err)
	}
	planReview, _, _ := s.CreateTask(CreateTaskOptions{
		ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "plan_review", Title: "Plan review",
		Objective: "review the plan", Reason: "policy", ParentTaskID: S(coord, "id"), DependsOn: []string{S(critic, "id")}, WorkspaceMode: "collection", Depth: 2, AllowDedupe: false,
	})
	if err = s.Mutate("UPDATE tasks SET status='COMPLETE',result_json="+SQLLiteral(JSONDumps(Row{"status": "complete", "summary": "pass", "verdict": "pass"}))+" WHERE id="+SQLLiteral(S(planReview, "id")), "plant zero-harvest plan review"); err != nil {
		t.Fatal(err)
	}
	impl, _, _ := s.CreateTask(CreateTaskOptions{
		ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "implement", Title: "Implement",
		Objective: "implement the authenticated endpoint", Reason: "plan", ParentTaskID: S(coord, "id"), DependsOn: []string{S(planReview, "id"), S(critic, "id")}, WorkspaceMode: "collection", Depth: 2, AllowDedupe: false,
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, _ = s.ClaimTask(S(impl, "id"), 3)
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("ClaimTask on a zero-harvest lineage hung, indicating a Store.Event-under-lock deadlock on the claim path")
	}
	if n := countEvents(t, s, S(c, "id"), "DIRECTIVE_TRACEABILITY_EMPTY_HARVEST"); n != 0 {
		t.Fatalf("the claim-time gate must stay silent and never emit an empty-harvest event under the lock: %d", n)
	}
}
