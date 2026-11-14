package praector

import (
	"strings"
	"testing"
)

// Acceptance-readiness observability sweep (Scheduler.observeAcceptanceReadiness). The
// reconcile loop only visits RUNNING/BLOCKED collections, so a Collection that already
// reached COMPLETE but whose graph would still be refused by `collection accept` (an
// acceptance-only validator surprise, e.g. a COMPLETE review with a missing verdict, which
// UpdateCollectionTerminalState does not itself check) is invisible until the operator runs
// accept. The sweep runs the SAME read-only decision predicate the accept path uses and
// records exactly ONE deduplicated ACCEPTANCE_READINESS_WOULD_BLOCK event naming the reason,
// WITHOUT regressing the collection or mutating anything. The authoritative seams are
// acceptanceBlockReason (the pure read-only predicate both the accept path and the sweep
// share) and Scheduler.observeAcceptanceReadiness (the scheduler escalation step), driven
// directly against the durable store, mirroring stuck_floor_liveness_test.go.

// completeReviewWithVerdict plants a COMPLETE review with an explicit verdict WITHOUT
// running its submit-time validators. A pass verdict additionally carries a valid
// impact_enumeration so the review is genuinely acceptable (a passing review with no
// enumeration is itself an acceptance blocker, which is not what these tests probe).
func completeReviewWithVerdict(t *testing.T, s *Store, review Row, verdict string) {
	t.Helper()
	result := Row{"status": "complete", "summary": "reviewed", "verdict": verdict}
	if verdict == "pass" {
		sites := []any{Row{"site": "internal/praector/store.go", "disposition": "unaffected", "evidence": "go test ./internal/praector/ -run TestAcceptance"}}
		result["impact_enumeration"] = Row{"method": "call-graph walk", "sites": sites}
	}
	res := JSONDumps(result)
	if err := s.Mutate("UPDATE tasks SET status="+SQLLiteral(TaskComplete)+",result_json="+SQLLiteral(res)+",updated_at="+SQLLiteral(UTCNow())+" WHERE id="+SQLLiteral(S(review, "id")), "plant complete review verdict="+verdict); err != nil {
		t.Fatal(err)
	}
}

// (e) A COMPLETE-but-unacceptable collection emits EXACTLY ONE would-block event naming the
// reason and remains COMPLETE (no status/revision mutation), and re-running the sweep on the
// unchanged graph never re-storms. An acceptable collection emits nothing. Accept-time
// behavior is unchanged (validateCollectionForAcceptance yields the same verdict on both).
func TestAcceptanceReadinessSweepEmitsOnceForUnacceptable(t *testing.T) {
	assertFloorObjectiveAllFalse(t)
	s, cfg, c, coord := gateFreeFloorCollection(t, floorObjective)
	impl := floorImplement(t, s, c, coord)
	if err := s.SetTaskStatus(S(coord, "id"), TaskComplete, "coord handed off"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTaskStatus(S(impl, "id"), TaskComplete, "impl complete"); err != nil {
		t.Fatal(err)
	}
	// A passing review that COVERS the implement so the coverage rule (the only acceptance
	// check UpdateCollectionTerminalState also runs) is satisfied and the collection reads
	// COMPLETE.
	reviewPass, _, err := s.CreateTask(CreateTaskOptions{ProjectID: S(c, "project_id"), CollectionID: S(c, "id"), Profile: "review", Title: "Independent review", Objective: "review", Reason: "review", ParentTaskID: S(coord, "id"), DependsOn: []string{S(impl, "id")}, WorkspaceMode: "collection", Depth: 1, AllowDedupe: false})
	if err != nil {
		t.Fatal(err)
	}
	completeReviewWithVerdict(t, s, reviewPass, "pass")
	if err := s.AddEdge(S(impl, "id"), S(reviewPass, "id"), S(c, "id"), "verifies"); err != nil {
		t.Fatal(err)
	}
	// A second COMPLETE review with a MISSING verdict: UpdateCollectionTerminalState does
	// not check verdict presence, so the collection is still COMPLETE, but the accept-path
	// validator rejects it. This is the acceptance-only surprise the sweep must surface.
	reviewBad, _, err := s.CreateTask(CreateTaskOptions{ProjectID: S(c, "project_id"), CollectionID: S(c, "id"), Profile: "review", Title: "Second review", Objective: "review", Reason: "review", ParentTaskID: S(coord, "id"), WorkspaceMode: "collection", Depth: 1, AllowDedupe: false})
	if err != nil {
		t.Fatal(err)
	}
	completeReviewWithVerdict(t, s, reviewBad, "")
	if err := s.SetCollectionStatus(S(c, "id"), "COMPLETE", true); err != nil {
		t.Fatal(err)
	}

	before, _ := s.ListCollectionRevisions(S(c, "id"), "OPEN")
	sched := newFloorScheduler(t, s, cfg)
	sched.observeAcceptanceReadiness(S(c, "id"))

	first := collectionEventsFor(t, s, c, "ACCEPTANCE_READINESS_WOULD_BLOCK")
	if len(first) != 1 {
		t.Fatalf("a COMPLETE-but-unacceptable collection must emit exactly one would-block event, got %d", len(first))
	}
	reason := S(JSONMap(S(first[0], "payload_json")), "reason")
	if !strings.Contains(reason, S(reviewBad, "id")) || !strings.Contains(strings.ToLower(reason), "verdict") {
		t.Fatalf("the would-block event must name the offending review and the verdict reason; got %q", reason)
	}

	// Observability only: the sweep must not regress the collection or satisfy a revision.
	after, _ := s.ListCollectionRevisions(S(c, "id"), "OPEN")
	if got := S(mustGetCollection(t, s, c), "status"); got != "COMPLETE" {
		t.Fatalf("the sweep must leave a COMPLETE collection COMPLETE, got %s", got)
	}
	if len(before) != len(after) {
		t.Fatalf("the sweep must not satisfy any revision (open revisions changed %d -> %d)", len(before), len(after))
	}

	// Re-running on the unchanged graph must NOT emit a second marker.
	sched.observeAcceptanceReadiness(S(c, "id"))
	sched.observeAcceptanceReadiness(S(c, "id"))
	if got := collectionEventsFor(t, s, c, "ACCEPTANCE_READINESS_WOULD_BLOCK"); len(got) != 1 {
		t.Fatalf("an unchanged COMPLETE collection must never re-storm the would-block marker, got %d", len(got))
	}

	// Accept-time behavior unchanged: the real accept path still rejects with the same reason.
	if _, _, e := validateCollectionForAcceptance(s, c); e == nil || !strings.Contains(e.Error(), S(reviewBad, "id")) {
		t.Fatalf("accept-time validation must still reject the bad review; got %v", e)
	}
}

// (e-negative) A COMPLETE-and-acceptable collection emits nothing and accept-time validation
// passes, proving the sweep does not fabricate would-block events for deliverable collections.
func TestAcceptanceReadinessSweepSilentForAcceptable(t *testing.T) {
	assertFloorObjectiveAllFalse(t)
	s, cfg, c, coord := gateFreeFloorCollection(t, floorObjective)
	impl := floorImplement(t, s, c, coord)
	if err := s.SetTaskStatus(S(coord, "id"), TaskComplete, "coord handed off"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTaskStatus(S(impl, "id"), TaskComplete, "impl complete"); err != nil {
		t.Fatal(err)
	}
	reviewPass, _, err := s.CreateTask(CreateTaskOptions{ProjectID: S(c, "project_id"), CollectionID: S(c, "id"), Profile: "review", Title: "Independent review", Objective: "review", Reason: "review", ParentTaskID: S(coord, "id"), DependsOn: []string{S(impl, "id")}, WorkspaceMode: "collection", Depth: 1, AllowDedupe: false})
	if err != nil {
		t.Fatal(err)
	}
	completeReviewWithVerdict(t, s, reviewPass, "pass")
	if err := s.AddEdge(S(impl, "id"), S(reviewPass, "id"), S(c, "id"), "verifies"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetCollectionStatus(S(c, "id"), "COMPLETE", true); err != nil {
		t.Fatal(err)
	}
	sched := newFloorScheduler(t, s, cfg)
	sched.observeAcceptanceReadiness(S(c, "id"))
	sched.observeAcceptanceReadiness(S(c, "id"))
	if got := collectionEventsFor(t, s, c, "ACCEPTANCE_READINESS_WOULD_BLOCK"); len(got) != 0 {
		t.Fatalf("an acceptable COMPLETE collection must emit no would-block event, got %d", len(got))
	}
	if _, _, e := validateCollectionForAcceptance(s, c); e != nil {
		t.Fatalf("the acceptable collection must pass accept-time validation unchanged; got %v", e)
	}
}

// (e-purity) The predicate the sweep shares with the accept path is purely read-only when
// invoked with recordEmptyHarvest=false: it must make no durable write at all (the marker
// itself is the scheduler's, not the predicate's).
func TestAcceptanceBlockReasonReadOnlyDoesNotWrite(t *testing.T) {
	s, _, c, coord := gateFreeFloorCollection(t, floorObjective)
	impl := floorImplement(t, s, c, coord)
	if err := s.SetTaskStatus(S(coord, "id"), TaskComplete, "coord complete"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTaskStatus(S(impl, "id"), TaskComplete, "impl complete"); err != nil {
		t.Fatal(err)
	}
	beforeEvents, _ := s.EventsForCollection(S(c, "id"), 500)
	if _, e := acceptanceBlockReason(s, c, "COMPLETE", false); e == nil {
		t.Fatal("an uncovered implement must block acceptance via the shared predicate")
	}
	afterEvents, _ := s.EventsForCollection(S(c, "id"), 500)
	if len(afterEvents) != len(beforeEvents) {
		t.Fatalf("the read-only predicate must emit no events; got %d -> %d", len(beforeEvents), len(afterEvents))
	}
}

func mustGetCollection(t *testing.T, s *Store, c Row) Row {
	t.Helper()
	r, err := s.GetCollection(S(c, "id"))
	if err != nil {
		t.Fatal(err)
	}
	return r
}
