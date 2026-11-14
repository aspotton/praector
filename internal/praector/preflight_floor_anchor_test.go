package praector

import "testing"

// Generation-anchor agreement between the claim-time preflight floor
// (Store.preflightFloorAllowsClaimLocked) and the scheduler staging floor
// (enforcePlanningPolicy gateFree floorCoordinator). Both layers resolve "this
// generation" as the latest coordinator whose title is a generation-creating
// title ('Initial coordination' / 'Acceptance revision'), so a within-generation
// helper coordinator (attention / critic / research revalidation / delivery-policy
// repair) neither moves the claim anchor nor misdirects staging existence. The
// motivating failure is a gate-free coherent collection that ever spawns a
// 'Coordinator attention' task: the pre-fix title-agnostic claim anchor resolved
// the attention task, found zero explore/critic requested by it, and held the
// implement as PREFLIGHT_FLOOR_HELD forever while staging saw the generation as
// satisfied and backfilled nothing. The authoritative seam is Store.ClaimTask.

// (1) Regression: after a completed explore+critic preflight and a passing plan
// review under 'Initial coordination', a LATER 'Coordinator attention' coordinator
// must NOT hijack the claim anchor. ClaimTask RELEASES the implement instead of
// holding it PREFLIGHT_FLOOR_HELD forever. The counter-example is asserted
// structurally: the attention task is the latest non-cancelled coordinator (the
// pre-fix title-agnostic anchor) and it has zero explore/critic requested by it,
// which is exactly what made the pre-fix floor hold permanently.
func TestPreflightFloorAnchorReleasesOnCoordinatorAttention(t *testing.T) {
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
	completePlanReview(t, s, planReviewsIn(t, s, c)[0], "pass")
	completeGenerationPreflights(t, s, c, coord)

	// A real within-generation helper coordinator, created LATER than the
	// generation coordinator (caller is the implement, a non-coordinator).
	attn, _, err := s.RequestCoordinatorAttention(S(impl, "id"), "Re-plan after the floor released.", false, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if S(attn, "title") != "Coordinator attention" {
		t.Fatalf("RequestCoordinatorAttention must spawn a 'Coordinator attention' coordinator, got %q", S(attn, "title"))
	}

	// Counter-example: the pre-fix title-agnostic anchor is the LATEST coordinator
	// by created_at,id, which is now the attention task with zero preflight children.
	coords, err := s.Query("SELECT id,title FROM tasks WHERE collection_id=" + SQLLiteral(S(c, "id")) + " AND profile='coordinator' AND status NOT IN ('CANCELLED','SUPERSEDED') ORDER BY created_at,id")
	if err != nil {
		t.Fatal(err)
	}
	if len(coords) == 0 || S(coords[len(coords)-1], "id") != S(attn, "id") {
		t.Fatal("the coordinator-attention task must be the latest non-cancelled coordinator (the pre-fix title-agnostic anchor)")
	}
	orphan, err := s.Query("SELECT id FROM tasks WHERE collection_id=" + SQLLiteral(S(c, "id")) + " AND profile IN ('explore','critic') AND requested_by_task_id=" + SQLLiteral(S(attn, "id")) + " AND status NOT IN ('CANCELLED','SUPERSEDED')")
	if err != nil {
		t.Fatal(err)
	}
	if len(orphan) != 0 {
		t.Fatal("the attention coordinator must own no explore/critic, which is what permanently held the pre-fix floor")
	}

	// After the fix the title-based anchor ignores the helper and resolves the
	// 'Initial coordination' generation, whose terminal preflight releases the floor.
	claimed, _, err := s.ClaimTask(S(impl, "id"), 3)
	if err != nil {
		t.Fatal(err)
	}
	if claimed == nil || S(claimed, "id") != S(impl, "id") {
		t.Fatal("a within-generation coordinator-attention task must NOT hold the implement PREFLIGHT_FLOOR_HELD after the generation preflight and review complete")
	}
	if ev := floorEventsFor(t, s, c, impl, "PREFLIGHT_FLOOR_HELD"); len(ev) != 0 {
		t.Fatal("the anchored floor must not label the claim PREFLIGHT_FLOOR_HELD once the generation anchor releases")
	}
}

// (2) Parity: on graphs containing each non-generation helper coordinator title,
// staging existence and the claim floor must anchor identically to the titled
// generation coordinator (never the helper). Each case drives a fresh collection
// through the real scheduler planning pass and asserts both layers resolve the
// 'Initial coordination' coordinator: staging parents the backfilled explore to it
// (PREFLIGHT_FLOOR_STAGED), and the claim floor releases once its preflight and a
// passing review complete. The final case forces a created_at TIE between the
// generation coordinator and the helper to prove the title filter (not timestamp
// ordering) governs both layers under identical timestamps.
func TestPreflightFloorAnchorParityAcrossHelperTitles(t *testing.T) {
	helperTitles := []string{
		"Coordinator attention",
		"Apply critic revalidation",
		"Apply inherited research revalidation",
		"Delivery policy repair",
	}
	run := func(t *testing.T, title string, tie bool) {
		t.Helper()
		assertFloorObjectiveAllFalse(t)
		s, cfg, c, coord := gateFreeFloorCollection(t, floorObjective)
		impl := floorImplement(t, s, c, coord)
		helper, _, err := s.CreateTask(CreateTaskOptions{ProjectID: S(c, "project_id"), CollectionID: S(c, "id"), Profile: "coordinator", Title: title, Objective: "Within-generation helper coordinator", Reason: "parity probe", ParentTaskID: S(coord, "id"), RequestedByTaskID: S(coord, "id"), WorkspaceMode: "collection", Depth: 1, AllowDedupe: false})
		if err != nil {
			t.Fatal(err)
		}
		if tie {
			// Force an identical timestamp so ordering tie-breaks are exercised; the
			// title predicate must resolve the generation coordinator regardless.
			if err := s.Mutate("UPDATE tasks SET created_at="+SQLLiteral(S(coord, "created_at"))+" WHERE id="+SQLLiteral(S(helper, "id")), "tie helper created_at to generation coordinator"); err != nil {
				t.Fatal(err)
			}
		}
		// A handled within-generation helper coordinator is terminal; a nonterminal one
		// would (correctly) act as an active graph shaper and suppress artifact-floor
		// staging for a reason orthogonal to the anchor agreement under test.
		if err := s.SetTaskStatus(S(helper, "id"), TaskComplete, "helper coordinator handled"); err != nil {
			t.Fatal(err)
		}
		if err := s.SetTaskStatus(S(coord, "id"), TaskComplete, "coordinator handed off"); err != nil {
			t.Fatal(err)
		}
		sched := newFloorScheduler(t, s, cfg)
		if _, err := sched.enforcePlanningPolicy(S(c, "id")); err != nil {
			t.Fatal(err)
		}
		// Staging existence must anchor on the GENERATION coordinator, not the helper:
		// the canonical explore is parented/requested_by the generation coordinator.
		if ev := collectionEventsFor(t, s, c, "PREFLIGHT_FLOOR_STAGED"); len(ev) == 0 {
			t.Fatalf("staging must anchor on the generation coordinator (PREFLIGHT_FLOOR_STAGED) despite the %q helper", title)
		}
		explorations := profileTasksIn(t, s, c, "explore")
		if len(explorations) != 1 || S(explorations[0], "requested_by_task_id") != S(coord, "id") {
			t.Fatalf("staging must parent the generation explore to the generation coordinator, not the %q helper", title)
		}
		completeGenerationPreflights(t, s, c, coord)
		completePlanReview(t, s, planReviewsIn(t, s, c)[0], "pass")
		// The claim floor must anchor identically: the generation preflight releases it.
		claimed, _, err := s.ClaimTask(S(impl, "id"), 3)
		if err != nil {
			t.Fatal(err)
		}
		if claimed == nil || S(claimed, "id") != S(impl, "id") {
			t.Fatalf("the claim floor must anchor on the generation coordinator identically to staging despite the %q helper", title)
		}
	}
	for _, title := range helperTitles {
		title := title
		t.Run(title, func(t *testing.T) { run(t, title, false) })
	}
	t.Run("CreatedTimestampTie", func(t *testing.T) { run(t, "Coordinator attention", true) })
}
