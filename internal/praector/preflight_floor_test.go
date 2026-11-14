package praector

import "testing"

// Artifact-derived preflight symmetry floor (DELIVERABLE B). A COHERENT
// (post-GateCoherenceCutoff) write-bearing Collection whose objective-text
// classifier required NO gate must not gate write work on plan_review evidence
// alone with zero preflight evidence. This mirrors the plan-review artifact floor
// in both layers: enforcePlanningPolicy stages the canonical explore + critic
// preflight (PREFLIGHT_FLOOR_STAGED), and the store claim floor holds the
// implement (PREFLIGHT_FLOOR_HELD) until this generation's explore AND critic
// reach terminal COMPLETE, releasing only alongside a passing final plan review.
// The motivating failure is COL-4274d1d8d9, whose all-false classification let an
// implement be claimed with a plan_review gate but zero preflight evidence. These
// are durable-state tests: the authoritative claim seam is Store.ClaimTask and
// staging runs through the real scheduler planning reconciliation.

const colReplayObjective = "Identify the next best feature or enhancement and do it"

func collectionEventsFor(t *testing.T, s *Store, c Row, evType string) Rows {
	t.Helper()
	evts, err := s.ListEvents(S(c, "project_id"), S(c, "id"), "", evType, "", "", 50)
	if err != nil {
		t.Fatal(err)
	}
	return evts
}

func profileTasksIn(t *testing.T, s *Store, c Row, profile string) Rows {
	t.Helper()
	rows, err := s.ListTasks(S(c, "id"))
	if err != nil {
		t.Fatal(err)
	}
	out := Rows{}
	for _, r := range rows {
		if S(r, "profile") == profile && S(r, "status") != TaskSuperseded && S(r, "status") != TaskCancelled {
			out = append(out, r)
		}
	}
	return out
}

func dependencyIncludes(t *testing.T, s *Store, task Row, depID string) bool {
	t.Helper()
	deps, err := s.Dependencies(S(task, "id"))
	if err != nil {
		t.Fatal(err)
	}
	return depsIDSet(deps)[depID]
}

func gateFreeFloorCollection(t *testing.T, objective string) (*Store, GlobalConfig, Row, Row) {
	t.Helper()
	s, cfg := newFakeStore(t)
	p, err := s.RegisterProject(testProjectConfig(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	c, coord, err := s.CreateCollectionWithCoordinator(S(p, "id"), "Gate-free probe", objective, "Plan")
	if err != nil {
		t.Fatal(err)
	}
	forceCollectionCreatedAt(t, s, S(c, "id"), postCutoffCreatedAt)
	return s, cfg, c, coord
}

func newFloorScheduler(t *testing.T, s *Store, cfg GlobalConfig) *Scheduler {
	t.Helper()
	sched, err := NewScheduler(s, cfg, 1)
	if err != nil {
		t.Fatal(err)
	}
	return sched
}

// (a) A gate-free coherent collection whose coordinator hands off after creating
// only an implement gets a staged canonical explore (parented to the coordinator)
// + critic (depending on it) + a plan_review->critic prerequisite edge +
// PREFLIGHT_FLOOR_STAGED. The implement stays held with PREFLIGHT_FLOOR_HELD once
// the plan review has passed and releases only after both preflights COMPLETE.
func TestPreflightFloorStagesGateFreePreflightAndHoldsClaim(t *testing.T) {
	assertFloorObjectiveAllFalse(t)
	s, cfg, c, coord := gateFreeFloorCollection(t, floorObjective)
	impl := floorImplement(t, s, c, coord)
	if err := s.SetTaskStatus(S(coord, "id"), TaskComplete, "coordinator handed off"); err != nil {
		t.Fatal(err)
	}
	sched := newFloorScheduler(t, s, cfg)
	if _, err := sched.enforcePlanningPolicy(S(c, "id")); err != nil {
		t.Fatal(err)
	}

	explorations := profileTasksIn(t, s, c, "explore")
	critics := profileTasksIn(t, s, c, "critic")
	reviews := planReviewsIn(t, s, c)
	if len(explorations) != 1 || len(critics) != 1 {
		t.Fatalf("a gate-free coherent write collection must auto-stage exactly one explore and one critic, got %d/%d", len(explorations), len(critics))
	}
	if S(explorations[0], "parent_task_id") != S(coord, "id") || S(explorations[0], "requested_by_task_id") != S(coord, "id") {
		t.Fatal("the staged explore must be parented to and requested by the generation coordinator")
	}
	if !dependencyIncludes(t, s, critics[0], S(explorations[0], "id")) {
		t.Fatal("the staged critic must depend on the staged explore")
	}
	if len(reviews) != 1 {
		t.Fatalf("the preflight floor must not create a second plan-review generation; expected one review, got %d", len(reviews))
	}
	if !dependencyIncludes(t, s, reviews[0], S(critics[0], "id")) {
		t.Fatal("the final plan review must take the staged critic as a prerequisite")
	}
	if ev := collectionEventsFor(t, s, c, "PREFLIGHT_FLOOR_STAGED"); len(ev) == 0 {
		t.Fatal("the gate-free staging path must emit a dedicated PREFLIGHT_FLOOR_STAGED audit event")
	}
	if ev := collectionEventsFor(t, s, c, "PLANNING_POLICY_ENFORCED"); len(ev) != 0 {
		t.Fatal("the gate-free floor path must never reuse PLANNING_POLICY_ENFORCED")
	}

	// Passing the final plan review releases the PLAN-REVIEW floor, but the
	// preflight floor now holds the implement because the preflights are nonterminal.
	completePlanReview(t, s, reviews[0], "pass")
	if held, _, err := s.ClaimTask(S(impl, "id"), 3); err != nil {
		t.Fatal(err)
	} else if held != nil && S(held, "id") == S(impl, "id") {
		t.Fatal("the implement must remain held while this generation's preflight is nonterminal")
	}
	if ev := floorEventsFor(t, s, c, impl, "PREFLIGHT_FLOOR_HELD"); len(ev) == 0 {
		t.Fatal("a passing plan_review with an incomplete preflight must label the hold PREFLIGHT_FLOOR_HELD")
	}

	completeGenerationPreflights(t, s, c, coord)
	claimed, _, err := s.ClaimTask(S(impl, "id"), 3)
	if err != nil {
		t.Fatal(err)
	}
	if claimed == nil || S(claimed, "id") != S(impl, "id") {
		t.Fatal("terminal explore + critic COMPLETE plus a passing plan review must release both floors")
	}
}

// (b) A coordinator-authored-preflight graph (COL-4274d1d8d9-shaped) takes exactly
// zero staging mutations: no PREFLIGHT_FLOOR_STAGED and no new explore/critic.
func TestPreflightFloorCoordinatorAuthoredTakesZeroStaging(t *testing.T) {
	assertFloorObjectiveAllFalse(t)
	s, cfg, c, coord := gateFreeFloorCollection(t, floorObjective)
	floorImplement(t, s, c, coord)
	// The coordinator requested its own preflight for its generation.
	expl, _, err := s.CreateTask(CreateTaskOptions{ProjectID: S(c, "project_id"), CollectionID: S(c, "id"), Profile: "explore", Title: "Explore requested by coordinator", Objective: "map the surface", Reason: "coordinator", ParentTaskID: S(coord, "id"), RequestedByTaskID: S(coord, "id"), WorkspaceMode: "collection", Depth: 1, AllowDedupe: false})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.CreateTask(CreateTaskOptions{ProjectID: S(c, "project_id"), CollectionID: S(c, "id"), Profile: "critic", Title: "Critic requested by coordinator", Objective: "preflight", Reason: "coordinator", ParentTaskID: S(coord, "id"), RequestedByTaskID: S(coord, "id"), DependsOn: []string{S(expl, "id")}, WorkspaceMode: "collection", Depth: 1, AllowDedupe: false}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTaskStatus(S(coord, "id"), TaskComplete, "coordinator handed off"); err != nil {
		t.Fatal(err)
	}
	sched := newFloorScheduler(t, s, cfg)
	if _, err := sched.enforcePlanningPolicy(S(c, "id")); err != nil {
		t.Fatal(err)
	}
	if ev := collectionEventsFor(t, s, c, "PREFLIGHT_FLOOR_STAGED"); len(ev) != 0 {
		t.Fatal("a coordinator-authored preflight must take zero PREFLIGHT_FLOOR_STAGED staging")
	}
	if e, cr := profileTasksIn(t, s, c, "explore"), profileTasksIn(t, s, c, "critic"); len(e) != 1 || len(cr) != 1 {
		t.Fatalf("a coordinator-authored preflight must not stage new explore/critic, got %d/%d", len(e), len(cr))
	}
}

// (b2) MIXED-AUTHORING backfill (explore-only): a coordinator that authored an
// explore but NOT a critic must have the missing critic half backfilled, because
// the claim floor (preflightFloorAllowsClaimLocked) requires explore AND critic
// INDEPENDENTLY. Before this repair the staging gate was paired (stage only when
// BOTH were absent), so an explore-only graph staged nothing and the implement was
// held permanently as PREFLIGHT_FLOOR_HELD with no self-heal. The authoritative
// seam asserted is Store.ClaimTask, driven through the real scheduler planning pass.
func TestPreflightFloorBackfillsMissingCriticForExploreOnlyCoordinator(t *testing.T) {
	assertFloorObjectiveAllFalse(t)
	s, cfg, c, coord := gateFreeFloorCollection(t, floorObjective)
	impl := floorImplement(t, s, c, coord)
	if _, _, err := s.CreateTask(CreateTaskOptions{ProjectID: S(c, "project_id"), CollectionID: S(c, "id"), Profile: "explore", Title: "Explore requested by coordinator", Objective: "map the surface", Reason: "coordinator", ParentTaskID: S(coord, "id"), RequestedByTaskID: S(coord, "id"), WorkspaceMode: "collection", Depth: 1, AllowDedupe: false}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTaskStatus(S(coord, "id"), TaskComplete, "coordinator handed off"); err != nil {
		t.Fatal(err)
	}
	sched := newFloorScheduler(t, s, cfg)
	if _, err := sched.enforcePlanningPolicy(S(c, "id")); err != nil {
		t.Fatal(err)
	}
	explorations := profileTasksIn(t, s, c, "explore")
	critics := profileTasksIn(t, s, c, "critic")
	if len(explorations) != 1 || len(critics) != 1 {
		t.Fatalf("an explore-only coordinator graph must backfill exactly one critic while reusing the authored explore, got %d/%d", len(explorations), len(critics))
	}
	if S(explorations[0], "title") != "Explore requested by coordinator" {
		t.Fatal("the backfill must reuse the coordinator-authored explore, not stage a new one")
	}
	if !dependencyIncludes(t, s, critics[0], S(explorations[0], "id")) {
		t.Fatal("the backfilled critic must depend on the generation explore")
	}
	if ev := collectionEventsFor(t, s, c, "PREFLIGHT_FLOOR_STAGED"); len(ev) == 0 {
		t.Fatal("backfilling the missing critic half must emit PREFLIGHT_FLOOR_STAGED")
	}
	reviews := planReviewsIn(t, s, c)
	if len(reviews) != 1 {
		t.Fatalf("backfill must not create a second plan-review generation, got %d", len(reviews))
	}
	if !dependencyIncludes(t, s, reviews[0], S(critics[0], "id")) {
		t.Fatal("the final plan review must take the generation critic as a prerequisite")
	}
	// Passing the review releases the plan-review floor; the preflight floor now
	// holds the implement because the backfilled critic is nonterminal.
	completePlanReview(t, s, reviews[0], "pass")
	if held, _, err := s.ClaimTask(S(impl, "id"), 3); err != nil {
		t.Fatal(err)
	} else if held != nil && S(held, "id") == S(impl, "id") {
		t.Fatal("the implement must remain held while the backfilled preflight is nonterminal")
	}
	if ev := floorEventsFor(t, s, c, impl, "PREFLIGHT_FLOOR_HELD"); len(ev) == 0 {
		t.Fatal("a passing plan review with an incomplete preflight must label the hold PREFLIGHT_FLOOR_HELD")
	}
	completeGenerationPreflights(t, s, c, coord)
	claimed, _, err := s.ClaimTask(S(impl, "id"), 3)
	if err != nil {
		t.Fatal(err)
	}
	if claimed == nil || S(claimed, "id") != S(impl, "id") {
		t.Fatal("terminal preflights plus a passing review must release both floors")
	}
}

// (b3) MIXED-AUTHORING backfill (critic-only): the symmetric case where a
// coordinator authored a critic but NOT an explore. Staging existence is per
// profile, so only the missing explore half is backfilled; the authored critic is
// reused and zero extra critic tasks appear. Store.ClaimTask drives hold + release.
func TestPreflightFloorBackfillsMissingExploreForCriticOnlyCoordinator(t *testing.T) {
	assertFloorObjectiveAllFalse(t)
	s, cfg, c, coord := gateFreeFloorCollection(t, floorObjective)
	impl := floorImplement(t, s, c, coord)
	if _, _, err := s.CreateTask(CreateTaskOptions{ProjectID: S(c, "project_id"), CollectionID: S(c, "id"), Profile: "critic", Title: "Critic requested by coordinator", Objective: "preflight", Reason: "coordinator", ParentTaskID: S(coord, "id"), RequestedByTaskID: S(coord, "id"), WorkspaceMode: "collection", Depth: 1, AllowDedupe: false}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTaskStatus(S(coord, "id"), TaskComplete, "coordinator handed off"); err != nil {
		t.Fatal(err)
	}
	sched := newFloorScheduler(t, s, cfg)
	if _, err := sched.enforcePlanningPolicy(S(c, "id")); err != nil {
		t.Fatal(err)
	}
	explorations := profileTasksIn(t, s, c, "explore")
	critics := profileTasksIn(t, s, c, "critic")
	if len(explorations) != 1 || len(critics) != 1 {
		t.Fatalf("a critic-only coordinator graph must backfill exactly one explore while reusing the authored critic, got %d/%d", len(explorations), len(critics))
	}
	if S(critics[0], "title") != "Critic requested by coordinator" {
		t.Fatal("the backfill must reuse the coordinator-authored critic, not stage a new one")
	}
	if ev := collectionEventsFor(t, s, c, "PREFLIGHT_FLOOR_STAGED"); len(ev) == 0 {
		t.Fatal("backfilling the missing explore half must emit PREFLIGHT_FLOOR_STAGED")
	}
	if ev := collectionEventsFor(t, s, c, "PLANNING_POLICY_ENFORCED"); len(ev) != 0 {
		t.Fatal("the gate-free floor path must never reuse PLANNING_POLICY_ENFORCED")
	}
	reviews := planReviewsIn(t, s, c)
	if len(reviews) != 1 {
		t.Fatalf("backfill must not create a second plan-review generation, got %d", len(reviews))
	}
	if !dependencyIncludes(t, s, reviews[0], S(critics[0], "id")) {
		t.Fatal("the final plan review must take the generation critic as a prerequisite")
	}
	completePlanReview(t, s, reviews[0], "pass")
	if held, _, err := s.ClaimTask(S(impl, "id"), 3); err != nil {
		t.Fatal(err)
	} else if held != nil && S(held, "id") == S(impl, "id") {
		t.Fatal("the implement must remain held while the backfilled preflight is nonterminal")
	}
	if ev := floorEventsFor(t, s, c, impl, "PREFLIGHT_FLOOR_HELD"); len(ev) == 0 {
		t.Fatal("a passing plan review with an incomplete preflight must label the hold PREFLIGHT_FLOOR_HELD")
	}
	completeGenerationPreflights(t, s, c, coord)
	claimed, _, err := s.ClaimTask(S(impl, "id"), 3)
	if err != nil {
		t.Fatal(err)
	}
	if claimed == nil || S(claimed, "id") != S(impl, "id") {
		t.Fatal("terminal preflights plus a passing review must release both floors")
	}
}

// (c) The implement claim is held with PREFLIGHT_FLOOR_HELD while this generation's
// critic is nonterminal and claimable after the critic reaches terminal COMPLETE
// plus a passing final plan review.
func TestPreflightFloorHoldsClaimWhileCriticNonterminal(t *testing.T) {
	assertFloorObjectiveAllFalse(t)
	s, cfg, c, coord := gateFreeFloorCollection(t, floorObjective)
	impl := floorImplement(t, s, c, coord)
	if err := s.SetTaskStatus(S(coord, "id"), TaskComplete, "coordinator handed off"); err != nil {
		t.Fatal(err)
	}
	sched := newFloorScheduler(t, s, cfg)
	if _, err := sched.enforcePlanningPolicy(S(c, "id")); err != nil {
		t.Fatal(err)
	}
	reviews := planReviewsIn(t, s, c)
	completePlanReview(t, s, reviews[0], "pass")

	// Retire only the explore; leave the critic nonterminal.
	explorations := profileTasksIn(t, s, c, "explore")
	critics := profileTasksIn(t, s, c, "critic")
	if err := s.Mutate("UPDATE tasks SET status="+SQLLiteral(TaskComplete)+",updated_at="+SQLLiteral(UTCNow())+" WHERE id="+SQLLiteral(S(explorations[0], "id")), "complete explore only"); err != nil {
		t.Fatal(err)
	}
	if held, _, err := s.ClaimTask(S(impl, "id"), 3); err != nil {
		t.Fatal(err)
	} else if held != nil && S(held, "id") == S(impl, "id") {
		t.Fatal("the implement must stay held while the generation critic is nonterminal")
	}
	if ev := floorEventsFor(t, s, c, impl, "PREFLIGHT_FLOOR_HELD"); len(ev) == 0 {
		t.Fatal("a nonterminal generation critic must label the hold PREFLIGHT_FLOOR_HELD")
	}

	if err := s.Mutate("UPDATE tasks SET status="+SQLLiteral(TaskComplete)+",updated_at="+SQLLiteral(UTCNow())+" WHERE id="+SQLLiteral(S(critics[0], "id")), "complete critic"); err != nil {
		t.Fatal(err)
	}
	claimed, _, err := s.ClaimTask(S(impl, "id"), 3)
	if err != nil {
		t.Fatal(err)
	}
	if claimed == nil || S(claimed, "id") != S(impl, "id") {
		t.Fatal("terminal critic COMPLETE plus a passing plan review must release the floor")
	}
}

// (d) A pre-cutoff legacy collection keeps byte-identical classifier-driven
// behavior: zero PREFLIGHT_FLOOR_* events and the pre-floor claim result.
func TestPreflightFloorPreCutoffLegacyUnchanged(t *testing.T) {
	s, cfg, c, coord := gateFreeFloorCollection(t, floorObjective)
	forceCollectionCreatedAt(t, s, S(c, "id"), preCutoffCreatedAt)
	impl := floorImplement(t, s, c, coord)
	if err := s.SetTaskStatus(S(coord, "id"), TaskComplete, "coordinator handed off"); err != nil {
		t.Fatal(err)
	}
	sched := newFloorScheduler(t, s, cfg)
	if _, err := sched.enforcePlanningPolicy(S(c, "id")); err != nil {
		t.Fatal(err)
	}
	if ev := collectionEventsFor(t, s, c, "PREFLIGHT_FLOOR_STAGED"); len(ev) != 0 {
		t.Fatal("a pre-cutoff collection must never take the gate-free preflight staging path")
	}
	if _, _, err := s.ClaimTask(S(impl, "id"), 3); err != nil {
		t.Fatal(err)
	}
	if ev := collectionEventsFor(t, s, c, "PREFLIGHT_FLOOR_HELD"); len(ev) != 0 {
		t.Fatal("a pre-cutoff collection must not emit PREFLIGHT_FLOOR_HELD claim holds")
	}
}

// (e) Replaying the COL-4274d1d8d9 objective text classifies gate-free (all four
// effectivePlanningPolicy booleans false) yet still converges the preflight and
// plan-review gates before any write claim.
func TestPreflightFloorCol4274ReplayConvergesBeforeWriteClaim(t *testing.T) {
	p := effectivePlanningPolicy(Row{"objective": colReplayObjective, "created_at": postCutoffCreatedAt})
	if p.ExploreRequired || p.ResearchRequired || p.CriticRequired || p.PlanReviewRequired {
		t.Fatalf("the COL-4274d1d8d9 objective must classify gate-free, got %+v", p)
	}
	s, cfg, c, coord := gateFreeFloorCollection(t, colReplayObjective)
	impl := floorImplement(t, s, c, coord)
	if err := s.SetTaskStatus(S(coord, "id"), TaskComplete, "coordinator handed off"); err != nil {
		t.Fatal(err)
	}
	// Before reconciliation the implement is unclaimable with no preflight evidence.
	if held, _, err := s.ClaimTask(S(impl, "id"), 3); err != nil {
		t.Fatal(err)
	} else if held != nil && S(held, "id") == S(impl, "id") {
		t.Fatal("a gate-free replayed collection must not hand out the implement before any preflight/plan-review")
	}
	sched := newFloorScheduler(t, s, cfg)
	if _, err := sched.enforcePlanningPolicy(S(c, "id")); err != nil {
		t.Fatal(err)
	}
	if ev := collectionEventsFor(t, s, c, "PREFLIGHT_FLOOR_STAGED"); len(ev) == 0 {
		t.Fatal("the replayed objective must converge the preflight gate")
	}
	if e, cr, rv := profileTasksIn(t, s, c, "explore"), profileTasksIn(t, s, c, "critic"), planReviewsIn(t, s, c); len(e) != 1 || len(cr) != 1 || len(rv) != 1 {
		t.Fatalf("the replayed objective must converge preflight + exactly one plan review, got %d/%d/%d", len(e), len(cr), len(rv))
	}
	// Passing the plan review releases the plan-review floor; the preflight floor
	// still holds the write claim until the converged preflights complete.
	completePlanReview(t, s, planReviewsIn(t, s, c)[0], "pass")
	if held, _, err := s.ClaimTask(S(impl, "id"), 3); err != nil {
		t.Fatal(err)
	} else if held != nil && S(held, "id") == S(impl, "id") {
		t.Fatal("the write claim must be held until the converged preflight gate releases")
	}
	if ev := collectionEventsFor(t, s, c, "PREFLIGHT_FLOOR_HELD"); len(ev) == 0 {
		t.Fatal("the replayed collection must hold the write claim on the converged preflight floor")
	}
	completeGenerationPreflights(t, s, c, coord)
	claimed, _, err := s.ClaimTask(S(impl, "id"), 3)
	if err != nil {
		t.Fatal(err)
	}
	if claimed == nil || S(claimed, "id") != S(impl, "id") {
		t.Fatal("after both preflights COMPLETE plus a passing review, the implement must claim")
	}
}
