package praector

import "testing"

// Forward-scope / non-retroactivity oracles. Every collection-derived seam now
// routes through effectivePlanningPolicy, so a collection created BEFORE
// GateCoherenceCutoff keeps the legacy verb-ANDed gates and one created ON/AFTER
// the cutoff gets the coherent category-derived gates. The protocol noun-phrase
// objective below records external_protocol_or_standard with NO mutation verb, so
// legacy disables the protocol risk gates while coherent enables them — the ideal
// probe for proving a seam is forward-scoped rather than retroactively re-gated.

const forwardScopeObjective = "MCP protocol specification compliance for the transport."

const (
	preCutoffCreatedAt  = "2020-01-01T00:00:00.000000"
	postCutoffCreatedAt = "2026-11-12T00:00:00.000000"
)

func forwardScopeCollection(t *testing.T, title string) (*Store, GlobalConfig, Row) {
	t.Helper()
	s, cfg := newFakeStore(t)
	p, err := s.RegisterProject(testProjectConfig(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	c, _, err := s.CreateCollectionWithCoordinator(S(p, "id"), title, forwardScopeObjective, "Plan")
	if err != nil {
		t.Fatal(err)
	}
	return s, cfg, c
}

func forceCollectionCreatedAt(t *testing.T, s *Store, cid, created string) {
	t.Helper()
	if err := s.Mutate("UPDATE collections SET created_at="+SQLLiteral(created)+" WHERE id="+SQLLiteral(cid), "force created_at for forward-scope probe"); err != nil {
		t.Fatal(err)
	}
}

// Seam 1 — scheduler staging (scheduler.enforcePlanningPolicy / scheduler.go).
// A pre-cutoff collection must NOT be retroactively gated on plan_review; a
// post-cutoff collection must derive plan_review from the security category. The
// security noun-phrase probe keeps research_required false (so staging is not
// short-circuited by the research-capability path) while its verb-ANDed
// plan_review gate differs across the cutoff. Also proves the additive
// gate_derivation audit field on PLANNING_POLICY_ENFORCED.
func TestPlanningGateForwardScope_SchedulerStaging(t *testing.T) {
	// freshStagedEvent stages one fresh security-noun collection with an implement
	// task and returns the last PLANNING_POLICY_ENFORCED payload under `created`.
	freshStagedEvent := func(created string) string {
		t.Helper()
		s, cfg := newFakeStore(t)
		p, err := s.RegisterProject(testProjectConfig(t.TempDir()))
		if err != nil {
			t.Fatal(err)
		}
		c, coord, err := s.CreateCollectionWithCoordinator(S(p, "id"), "Auth correction", "The auth bypass in the token check requires correction.", "Plan")
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err = s.CreateTask(CreateTaskOptions{ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "implement", Title: "Implement correction", Objective: "correct the token check", Reason: "plan", ParentTaskID: S(coord, "id"), WorkspaceMode: "collection", Depth: 1, AllowDedupe: false}); err != nil {
			t.Fatal(err)
		}
		sched, err := NewScheduler(s, cfg, 1)
		if err != nil {
			t.Fatal(err)
		}
		forceCollectionCreatedAt(t, s, S(c, "id"), created)
		if _, err = sched.enforcePlanningPolicy(S(c, "id")); err != nil {
			t.Fatal(err)
		}
		evts, _ := s.ListEvents(S(c, "project_id"), S(c, "id"), "", "PLANNING_POLICY_ENFORCED", "", "", 50)
		if len(evts) == 0 {
			t.Fatalf("no PLANNING_POLICY_ENFORCED audit event for created_at=%s", created)
		}
		return S(evts[len(evts)-1], "payload_json")
	}
	pre := freshStagedEvent(preCutoffCreatedAt)
	post := freshStagedEvent(postCutoffCreatedAt)
	if !contains(pre, "verb_legacy") {
		t.Fatalf("pre-cutoff enforced event must label gate_derivation=verb_legacy, got %s", pre)
	}
	if !contains(pre, `"plan_review_required":false`) {
		t.Fatalf("pre-cutoff security-noun collection must not be retroactively gated on plan_review, got %s", pre)
	}
	if !contains(post, "category") {
		t.Fatalf("post-cutoff enforced event must label gate_derivation=category, got %s", post)
	}
	if !contains(post, `"plan_review_required":true`) {
		t.Fatalf("post-cutoff security-noun collection must derive plan_review from the threat-model category, got %s", post)
	}
}

func contains(hay, needle string) bool {
	return len(hay) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(hay); i++ {
			if hay[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}

// Seam 2 — claim gate (store.planningGateAllowsClaimLocked / store.go).
// An implementation claim in a pre-cutoff protocol collection is not retroactively
// gated on a plan_review; the same claim post-cutoff is blocked without one.
func TestPlanningGateForwardScope_ClaimGate(t *testing.T) {
	s, _, c := forwardScopeCollection(t, "Protocol transport")
	impl, _, err := s.CreateTask(CreateTaskOptions{ProjectID: S(c, "project_id"), CollectionID: S(c, "id"), Profile: "implement", Title: "Implement transport", Objective: "implement the transport", Reason: "plan", WorkspaceMode: "collection", Depth: 1, AllowDedupe: false})
	if err != nil {
		t.Fatal(err)
	}
	forceCollectionCreatedAt(t, s, S(c, "id"), preCutoffCreatedAt)
	claimed, _, err := s.ClaimTask(S(impl, "id"), 3)
	if err != nil {
		t.Fatal(err)
	}
	if claimed == nil || S(claimed, "id") != S(impl, "id") {
		t.Fatal("pre-cutoff protocol collection must not retroactively gate the implement claim on a plan review")
	}
	_ = s.SetTaskStatus(S(impl, "id"), TaskReady, "reset for post-cutoff probe")
	forceCollectionCreatedAt(t, s, S(c, "id"), postCutoffCreatedAt)
	claimed2, _, err := s.ClaimTask(S(impl, "id"), 3)
	if err != nil {
		t.Fatal(err)
	}
	if claimed2 != nil && S(claimed2, "id") == S(impl, "id") {
		t.Fatal("post-cutoff protocol collection must gate the implement claim on a completed plan review")
	}
}

// Seam 3 — submit-time external-conformance oracle strictness (store.SubmitResult).
// A pre-cutoff collection must not retroactively demand an authoritative oracle
// from a protocol research result; a post-cutoff collection must.
func TestPlanningGateForwardScope_SubmitOracle(t *testing.T) {
	result := validFreshResearchResult()
	result["verification_oracles"] = []any{}
	run := func(created string) error {
		s, _, c := forwardScopeCollection(t, "Protocol research")
		r, _, err := s.CreateTask(CreateTaskOptions{ProjectID: S(c, "project_id"), CollectionID: S(c, "id"), Profile: "research", Title: "Fresh external research preflight", Objective: "research the current protocol", Reason: "fresh external evidence", WorkspaceMode: "collection", Depth: 1, AllowDedupe: false})
		if err != nil {
			t.Fatal(err)
		}
		forceCollectionCreatedAt(t, s, S(c, "id"), created)
		_, err = s.SubmitResult(S(r, "id"), result)
		return err
	}
	if err := run(preCutoffCreatedAt); err != nil {
		t.Fatalf("pre-cutoff protocol research submit must not retroactively require an authoritative oracle: %v", err)
	}
	if err := run(postCutoffCreatedAt); err == nil {
		t.Fatal("post-cutoff protocol research submit must require an authoritative independent oracle")
	}
}

// Seam 4 — COMPLETE reconciliation and acceptance critic-stale (store.go and
// cli.go). Both now call effectivePlanningPolicy(collection), so a pre-cutoff
// collection keeps the legacy CriticRequired and cannot be reopened or blocked by
// a retroactive critic-stale verdict, while a post-cutoff one is coherent.
func TestPlanningGateForwardScope_CompleteAndAcceptanceSeams(t *testing.T) {
	for _, tc := range []struct {
		created    string
		wantCritic bool
	}{
		{preCutoffCreatedAt, false},
		{postCutoffCreatedAt, true},
	} {
		c := Row{"objective": forwardScopeObjective, "created_at": tc.created}
		if got := effectivePlanningPolicy(c).CriticRequired; got != tc.wantCritic {
			t.Errorf("created_at=%s: COMPLETE/acceptance critic_required=%v want %v", tc.created, got, tc.wantCritic)
		}
	}
}

func sameGates(a, b PlanningPolicy) bool {
	return a.ExploreRequired == b.ExploreRequired &&
		a.ResearchRequired == b.ResearchRequired &&
		a.CriticRequired == b.CriticRequired &&
		a.PlanReviewRequired == b.PlanReviewRequired &&
		a.ExternalConformanceRequired == b.ExternalConformanceRequired &&
		a.ThreatModelRequired == b.ThreatModelRequired
}

// Seam 5 — display seams (service collection-show and envelope). The same one
// helper keeps display consistent with enforcement: for a post-cutoff collection the
// projection equals the coherent floor; for a pre-cutoff collection it equals the
// legacy verb-ANDed derivation.
func TestPlanningGateForwardScope_DisplaySeams(t *testing.T) {
	for _, obj := range []string{
		forwardScopeObjective,
		"Twilio gateway integration.",
		"Deletion of the stale records.",
		"The vendor library needs assessment.",
	} {
		coherent := ClassifyPlanningPolicy(obj)
		pre := effectivePlanningPolicy(Row{"objective": obj, "created_at": preCutoffCreatedAt})
		post := effectivePlanningPolicy(Row{"objective": obj, "created_at": postCutoffCreatedAt})
		if !sameGates(post, coherent) {
			t.Errorf("post-cutoff display projection must equal the coherent floor for %q", obj)
		}
		legacy := classifyPlanningPolicy(obj, false)
		if !sameGates(pre, legacy) {
			t.Errorf("pre-cutoff display projection must equal the legacy derivation for %q", obj)
		}
	}
}
