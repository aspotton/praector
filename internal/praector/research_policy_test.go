package praector

import (
	"errors"
	"testing"
)

// passingImpactEnumeration is the minimal durable sibling-effect enumeration
// a passing review result must carry under the gate-traceability contract.
func passingImpactEnumeration() Row {
	return Row{"method": "literal-column grep set", "sites": []any{
		Row{"site": "internal/praector/store.go", "disposition": "unaffected", "evidence": "grep of internal/praector/store.go for the mutated column shows no other writer"},
	}}
}

func validFreshResearchResult() Row {
	return Row{
		"status":  "complete",
		"summary": "current external contract verified",
		"as_of":   "2026-11-12",
		"sources": []any{Row{"id": "spec", "authority": "official", "authority_level": "official_specification", "reference": "https://example.test/spec", "version": "v-current"}},
		"claims": []any{Row{
			"id":                "claim-x",
			"claim":             "The current specification requires behavior X.",
			"source":            "spec",
			"authority":         "official specification",
			"normative_level":   "MUST",
			"planner_directive": "Implementation MUST preserve behavior X.",
			"directive_effect":  "preserve",
			"confidence":        "high",
		}},
		"verification_oracles": []any{Row{"kind": "official_reference_oracle", "name": "official SDK"}},
		"risks":                []any{}, "unresolved": []any{},
	}
}

func researchConsistencyFor(tasks Rows) Row {
	constraints, err := ResearchClaimConstraints(tasks)
	if err != nil {
		panic(err)
	}
	ids := []any{}
	claims := []any{}
	for _, constraint := range constraints {
		ids = append(ids, constraint.ClaimRef)
		claims = append(claims, Row{
			"claim_id": constraint.ClaimRef, "disposition": "preserved",
			"inherited_directive": constraint.PlannerDirective, "derived_directives": []any{},
		})
	}
	return Row{"verdict": "pass", "checked_claim_ids": ids, "claims": claims, "contradictions": []any{}}
}

func TestPlanningPolicyRequiresFreshResearchForExternalProtocol(t *testing.T) {
	p := ClassifyPlanningPolicy("Implement support for the current MCP protocol specification in an HTTP server.")
	if !p.ExploreRequired || !p.ResearchRequired || !p.CriticRequired || !p.PlanReviewRequired {
		t.Fatalf("expected full external-protocol planning gates: %#v", p)
	}
	if !p.ExternalConformanceRequired || !p.ThreatModelRequired {
		t.Fatalf("expected conformance + threat model: %#v", p)
	}

	local := ClassifyPlanningPolicy("Refactor local parser helper to remove duplicate code and update its tests.")
	if local.ResearchRequired {
		t.Fatalf("purely local refactor must not require fresh external research: %#v", local)
	}
}

func TestDeterministicResearchResultValidationAndFingerprint(t *testing.T) {
	good := validFreshResearchResult()
	if err := ValidateDeterministicResearchResult(good, true); err != nil {
		t.Fatalf("valid research rejected: %v", err)
	}
	bad := validFreshResearchResult()
	bad["claims"] = []any{Row{"id": "claim-x", "claim": "x", "source": "spec", "authority": "official", "normative_level": "MUST", "planner_directive": "ignore x", "directive_effect": "weaken", "confidence": "high"}}
	if err := ValidateDeterministicResearchResult(bad, true); err == nil {
		t.Fatal("expected invalid directive_effect to be rejected")
	}
	exception := validFreshResearchResult()
	exception["claims"] = []any{Row{"id": "claim-x", "claim": "x", "source": "spec", "authority": "official", "normative_level": "MUST", "planner_directive": "authorized exception", "directive_effect": "exception", "confidence": "high"}}
	if err := ValidateDeterministicResearchResult(exception, true); err == nil {
		t.Fatal("expected exception without durable decision id to be rejected")
	}

	objective := "Implement current MCP protocol support"
	tasks := Rows{{"id": "T-R", "profile": "research", "title": "Fresh external research preflight", "objective": "research", "reason": "fresh external evidence", "workspace_mode": "collection", "result_json": JSONDumps(good)}}
	fp1 := MaterialPlanFingerprint(objective, tasks, nil, false)
	changed := validFreshResearchResult()
	changed["as_of"] = "2026-11-13"
	tasks[0]["result_json"] = JSONDumps(changed)
	fp2 := MaterialPlanFingerprint(objective, tasks, nil, false)
	if fp1 == fp2 {
		t.Fatal("research result changes must change material plan fingerprint")
	}
}

func TestSchedulerExternalProtocolPreflightStagesExploreAndResearchBeforeCritic(t *testing.T) {
	s, cfg := newFakeStore(t)
	p, err := s.RegisterProject(testProjectConfig(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	c, coord, err := s.CreateCollectionWithCoordinator(S(p, "id"), "Protocol feature", "Implement support for the current MCP protocol specification in an HTTP server.", "Plan")
	if err != nil {
		t.Fatal(err)
	}
	sched, err := NewScheduler(s, cfg, 1)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := sched.enforcePlanningPolicy(S(c, "id"))
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected deterministic preflight")
	}
	tasks, _ := s.ListTasks(S(c, "id"))
	var explore, research, critic Row
	for _, task := range tasks {
		if S(task, "requested_by_task_id") != S(coord, "id") {
			continue
		}
		switch S(task, "profile") {
		case "explore":
			explore = task
		case "research":
			research = task
		case "critic":
			critic = task
		}
	}
	if explore == nil || research == nil || critic == nil {
		t.Fatalf("missing explore/research/critic: %v", tasks)
	}
	rdeps, _ := s.Dependencies(S(research, "id"))
	if len(rdeps) != 0 {
		t.Fatalf("research should run in parallel with explore, deps=%v", rdeps)
	}
	cdeps, _ := s.Dependencies(S(critic, "id"))
	if !containsTaskID(cdeps, S(explore, "id")) || !containsTaskID(cdeps, S(research, "id")) {
		t.Fatalf("critic must depend on explore+research: %v", cdeps)
	}
	coordDeps, _ := s.Dependencies(S(coord, "id"))
	if !containsTaskID(coordDeps, S(explore, "id")) || !containsTaskID(coordDeps, S(research, "id")) || !containsTaskID(coordDeps, S(critic, "id")) {
		t.Fatalf("coordinator must be blocked on full preflight: %v", coordDeps)
	}
}

func TestResearchRequiredEscalationCreatesBlockingResearchAndResumes(t *testing.T) {
	s, cfg := newFakeStore(t)
	p, _ := s.RegisterProject(testProjectConfig(t.TempDir()))
	c, coord, _ := s.CreateCollectionWithCoordinator(S(p, "id"), "Feature", "Add a local parser test.", "Plan")
	critic, _, err := s.CreateTask(CreateTaskOptions{ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "critic", Title: "Critic", Objective: "critique", Reason: "policy", ParentTaskID: S(coord, "id"), WorkspaceMode: "collection", Depth: 1, AllowDedupe: false})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.SubmitResult(S(critic, "id"), Row{"status": "waiting", "summary": "need current fact", "research_required": []any{Row{"question": "What does the current external standard require?"}}})
	if err != nil {
		t.Fatal(err)
	}
	sched, _ := NewScheduler(s, cfg, 1)
	changed, err := sched.enforceResearchRequests(S(c, "id"))
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected RESEARCH_REQUIRED child")
	}
	tasks, _ := s.ListTasks(S(c, "id"))
	var research Row
	for _, task := range tasks {
		if S(task, "profile") == "research" && S(task, "requested_by_task_id") == S(critic, "id") {
			research = task
		}
	}
	if research == nil {
		t.Fatalf("missing research child: %v", tasks)
	}
	deps, _ := s.Dependencies(S(critic, "id"))
	if !containsTaskID(deps, S(research, "id")) {
		t.Fatalf("critic not blocked on research: %v", deps)
	}
	_, err = s.SubmitResult(S(research, "id"), validFreshResearchResult())
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetTaskStatus(S(research, "id"), TaskComplete, "research complete"); err != nil {
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

func TestResearchCapabilityDisabledBlocksDeterministicPreflight(t *testing.T) {
	s, cfg := newFakeStore(t)
	pcfg := testProjectConfig(t.TempDir())
	pcfg.Capabilities = DefaultProjectCapabilities()
	pcfg.Capabilities.CurrentExternalResearch = false
	p, _ := s.RegisterProject(pcfg)
	c, _, _ := s.CreateCollectionWithCoordinator(S(p, "id"), "Protocol", "Implement support for the current MCP protocol specification.", "Plan")
	sched, _ := NewScheduler(s, cfg, 1)
	_, err := sched.enforcePlanningPolicy(S(c, "id"))
	var capErr *ResearchCapabilityError
	if !errors.As(err, &capErr) {
		t.Fatalf("expected ResearchCapabilityError, got %v", err)
	}
}

func TestResearchCapabilityUnavailableQuarantinesCollection(t *testing.T) {
	s, cfg := newFakeStore(t)
	pcfg := testProjectConfig(t.TempDir())
	pcfg.Capabilities = DefaultProjectCapabilities()
	pcfg.Capabilities.CurrentExternalResearch = false
	p, _ := s.RegisterProject(pcfg)
	c, _, _ := s.CreateCollectionWithCoordinator(S(p, "id"), "Protocol", "Implement support for the current MCP protocol specification.", "Plan")
	sched, _ := NewScheduler(s, cfg, 1)
	if got := sched.reconcileCollectionSafely(S(c, "id")); got != "BLOCKED" {
		t.Fatalf("expected BLOCKED, got %s", got)
	}
	got, _ := s.GetCollection(S(c, "id"))
	if S(got, "status") != "BLOCKED" {
		t.Fatalf("collection not durably blocked: %v", got)
	}
	events, _ := s.EventsForCollection(S(c, "id"), 50)
	found := false
	for _, event := range events {
		if S(event, "event_type") == "RESEARCH_CAPABILITY_REQUIRED" {
			found = true
		}
	}
	if !found {
		t.Fatal("missing RESEARCH_CAPABILITY_REQUIRED event")
	}
}

func TestExternalProtocolRevisionGetsFreshExploreResearchAndCritic(t *testing.T) {
	s, cfg := newFakeStore(t)
	p, _ := s.RegisterProject(testProjectConfig(t.TempDir()))
	c, initial, err := s.CreateCollectionWithCoordinator(S(p, "id"), "Protocol", "Implement support for the current MCP protocol specification.", "Plan")
	if err != nil {
		t.Fatal(err)
	}
	for _, profile := range []string{"explore", "research", "critic"} {
		task, _, e := s.CreateTask(CreateTaskOptions{
			ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: profile,
			Title: "Old " + profile, Objective: "old evidence", Reason: "prior generation",
			ParentTaskID: S(initial, "id"), RequestedByTaskID: S(initial, "id"), WorkspaceMode: "collection", Depth: 1, AllowDedupe: false,
		})
		if e != nil {
			t.Fatal(e)
		}
		result := Row{"status": "complete", "summary": "done"}
		if profile == "research" {
			result = validFreshResearchResult()
		}
		if _, e = s.SubmitResult(S(task, "id"), result); e != nil {
			t.Fatal(e)
		}
		if e = s.SetTaskStatus(S(task, "id"), TaskComplete, "done"); e != nil {
			t.Fatal(e)
		}
	}
	_, _ = s.SubmitResult(S(initial, "id"), Row{"status": "complete", "summary": "done"})
	if err = s.SetTaskStatus(S(initial, "id"), TaskComplete, "done"); err != nil {
		t.Fatal(err)
	}
	if err = s.SetCollectionStatus(S(c, "id"), TaskComplete, true); err != nil {
		t.Fatal(err)
	}
	_, revision, err := s.RequestCollectionRevision(S(c, "id"), "Re-evaluate current protocol behavior")
	if err != nil {
		t.Fatal(err)
	}
	sched, _ := NewScheduler(s, cfg, 1)
	if _, err = sched.enforcePlanningPolicy(S(c, "id")); err != nil {
		t.Fatal(err)
	}
	tasks, _ := s.ListTasks(S(c, "id"))
	var explore, research, critic Row
	for _, task := range tasks {
		if S(task, "requested_by_task_id") != S(revision, "id") {
			continue
		}
		switch S(task, "profile") {
		case "explore":
			explore = task
		case "research":
			research = task
		case "critic":
			critic = task
		}
	}
	if explore == nil || research == nil || critic == nil {
		t.Fatalf("revision missing fresh explore/research/critic: %v", tasks)
	}
	deps, _ := s.Dependencies(S(critic, "id"))
	if !containsTaskID(deps, S(explore, "id")) || !containsTaskID(deps, S(research, "id")) {
		t.Fatalf("revision critic missing fresh evidence deps: %v", deps)
	}
}

func TestImplementationLineageChangeCreatesReconciliationReview(t *testing.T) {
	s, cfg := newFakeStore(t)
	p, _ := s.RegisterProject(testProjectConfig(t.TempDir()))
	c, coord, _ := s.CreateCollectionWithCoordinator(S(p, "id"), "Feature", "Add a local endpoint.", "Plan")
	impl, _, err := s.CreateTask(CreateTaskOptions{ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "implement", Title: "Implement endpoint", Objective: "do it", Reason: "plan", ParentTaskID: S(coord, "id"), WorkspaceMode: "collection", Depth: 1, AllowDedupe: false})
	if err != nil {
		t.Fatal(err)
	}
	sched, _ := NewScheduler(s, cfg, 1)
	if err = sched.recordTaskInputLineage(impl, Row{"id": "A-test"}); err != nil {
		t.Fatal(err)
	}
	// A new material task appearing while implementation is running changes the accepted lineage.
	_, _, err = s.CreateTask(CreateTaskOptions{ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "advise", Title: "New material advice", Objective: "new constraint", Reason: "material change", ParentTaskID: S(coord, "id"), WorkspaceMode: "collection", Depth: 1, AllowDedupe: false})
	if err != nil {
		t.Fatal(err)
	}
	if err = sched.ensureReview(p, c, impl); err != nil {
		t.Fatal(err)
	}
	review, err := s.FindRelatedReview(S(impl, "id"))
	if err != nil || review == nil {
		t.Fatalf("missing reconciliation review: %v %v", review, err)
	}
	if S(review, "title") != "Reconcile and review Implement endpoint" {
		t.Fatalf("unexpected review title: %s", S(review, "title"))
	}
	events, _ := s.EventsForCollection(S(c, "id"), 100)
	found := false
	for _, event := range events {
		if S(event, "event_type") == "RECONCILIATION_REQUIRED" && S(event, "task_id") == S(impl, "id") {
			found = true
		}
	}
	if !found {
		t.Fatal("missing RECONCILIATION_REQUIRED event")
	}
}

func TestFreshResearchClassificationDoesNotTriggerOnLocalDependencyGraphWork(t *testing.T) {
	p := ClassifyPlanningPolicy("Refactor the local task dependency graph reconciliation and add unit tests.")
	if p.ResearchRequired {
		t.Fatalf("local dependency-graph work must not require external research: %#v", p)
	}
	if !p.ExploreRequired || !p.CriticRequired {
		t.Fatalf("local implementation should still get normal implementation preflight: %#v", p)
	}
	currentLocal := ClassifyPlanningPolicy("Inspect the current implementation and refactor its local parser tests.")
	if currentLocal.ResearchRequired {
		t.Fatalf("generic current local state should not require external research: %#v", currentLocal)
	}
	currentNews := ClassifyPlanningPolicy("Research current news and recent events affecting the vendor before making a recommendation.")
	if !currentNews.ResearchRequired {
		t.Fatalf("current news/events should require fresh external research: %#v", currentNews)
	}
}

func TestResearchValidationRequiresTraceableDirectivesAndAuthoritativeOracleClassification(t *testing.T) {
	unbacked := validFreshResearchResult()
	unbacked["planner_directives"] = []any{"A contradictory free-form directive"}
	if err := ValidateDeterministicResearchResult(unbacked, true); err == nil {
		t.Fatal("expected unbacked top-level planner directive to be rejected")
	}

	blackBoxOnly := validFreshResearchResult()
	blackBoxOnly["verification_oracles"] = []any{Row{"kind": "black_box_test", "name": "implementation-owned client"}}
	if err := ValidateDeterministicResearchResult(blackBoxOnly, true); err == nil {
		t.Fatal("expected external-conformance research to require authoritative oracle or explicit unavailability")
	}
	blackBoxOnly["authoritative_oracle_unavailable_reason"] = "Provider publishes no SDK, conformance suite, or sandbox."
	if err := ValidateDeterministicResearchResult(blackBoxOnly, true); err != nil {
		t.Fatalf("explicit authoritative-oracle unavailability should be allowed for planning: %v", err)
	}
}

func TestResearchConsumerMustTraceEveryClaim(t *testing.T) {
	research := Row{"id": "T-R", "profile": "research", "status": TaskComplete, "result_json": JSONDumps(validFreshResearchResult())}
	missing := Row{"research_consistency": Row{"verdict": "pass", "checked_claim_ids": []any{}, "contradictions": []any{}}}
	if err := ValidateResearchConsumerResult(missing, Rows{research}, true); err == nil {
		t.Fatal("expected missing claim trace to fail")
	}
	good := Row{"research_consistency": researchConsistencyFor(Rows{research})}
	if err := ValidateResearchConsumerResult(good, Rows{research}, true); err != nil {
		t.Fatalf("valid claim trace rejected: %v", err)
	}
	badConsistency := Row{"research_consistency": researchConsistencyFor(Rows{research})}
	badConsistency["research_consistency"].(Row)["verdict"] = "fail"
	badConsistency["research_consistency"].(Row)["contradictions"] = []any{"directive conflicts with MUST"}
	if err := ValidateResearchConsumerResult(badConsistency, Rows{research}, true); err == nil {
		t.Fatal("passing consumer contract must reject failed research consistency")
	}
}

func TestSubmitResultEnforcesResearchTraceAndExternalReviewOracle(t *testing.T) {
	s, _ := newFakeStore(t)
	p, err := s.RegisterProject(testProjectConfig(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	c, coord, err := s.CreateCollectionWithCoordinator(S(p, "id"), "Protocol", "Implement support for the current MCP protocol specification in an HTTP server.", "Plan")
	if err != nil {
		t.Fatal(err)
	}
	research, _, err := s.CreateTask(CreateTaskOptions{
		ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "research", Title: "Fresh external research preflight",
		Objective: "research", Reason: "fresh external evidence", ParentTaskID: S(coord, "id"), WorkspaceMode: "collection", Depth: 1, AllowDedupe: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SubmitResult(S(research, "id"), validFreshResearchResult()); err != nil {
		t.Fatal(err)
	}
	if err = s.SetTaskStatus(S(research, "id"), TaskComplete, "done"); err != nil {
		t.Fatal(err)
	}
	critic, _, err := s.CreateTask(CreateTaskOptions{
		ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "critic", Title: "Critic", Objective: "critique", Reason: "policy",
		ParentTaskID: S(coord, "id"), DependsOn: []string{S(research, "id")}, WorkspaceMode: "collection", Depth: 1, AllowDedupe: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SubmitResult(S(critic, "id"), Row{"status": "complete", "summary": "checked"}); err == nil {
		t.Fatal("critic should be required to submit research_consistency")
	}
	completedResearch, _ := s.GetTask(S(research, "id"))
	if _, err = s.SubmitResult(S(critic, "id"), Row{
		"status": "complete", "summary": "checked",
		"research_consistency": researchConsistencyFor(Rows{completedResearch}),
	}); err != nil {
		t.Fatalf("critic trace rejected: %v", err)
	}

	review, _, err := s.CreateTask(CreateTaskOptions{
		ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "review", Title: "Review", Objective: "review", Reason: "policy",
		ParentTaskID: S(coord, "id"), WorkspaceMode: "collection", Depth: 1, AllowDedupe: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	blackBoxPass := Row{
		"status": "complete", "verdict": "pass", "summary": "looks good",
		"impact_enumeration":   passingImpactEnumeration(),
		"verification_oracles": []any{Row{"kind": "black_box_test", "name": "same-team client", "used": true}},
	}
	if _, err = s.SubmitResult(S(review, "id"), blackBoxPass); err == nil {
		t.Fatal("external-conformance review must not pass on black-box-only evidence")
	}
	decision, err := s.AddDecision(S(p, "id"), S(c, "id"), S(coord, "id"), "Accept black-box-only conformance for this bounded run", "No authoritative oracle is available", []any{}, []any{})
	if err != nil {
		t.Fatal(err)
	}
	blackBoxPass["authoritative_oracle_unavailable_reason"] = "No official SDK or provider sandbox is available."
	blackBoxPass["oracle_exception_decision_id"] = S(decision, "id")
	if _, err = s.SubmitResult(S(review, "id"), cloneRow(blackBoxPass)); err == nil {
		t.Fatal("oracle exception without a replacement probes list must be rejected")
	}
	blackBoxPass["probes"] = []any{Row{"scenario": "initialize then tools/list over loopback", "command_or_test": "go test ./internal/praector/ -run TestMCPConformance", "observed_outcome": "server counter-offered the implemented revision and served tools/list"}}
	if _, err = s.SubmitResult(S(review, "id"), blackBoxPass); err != nil {
		t.Fatalf("durably authorized oracle exception rejected: %v", err)
	}
}

func cloneRow(r Row) Row {
	out := Row{}
	for k, v := range r {
		out[k] = v
	}
	return out
}

func freshResearchResultWithID(id string) Row {
	result := validFreshResearchResult()
	claims := result["claims"].([]any)
	claim := Row{}
	for k, v := range claims[0].(Row) {
		claim[k] = v
	}
	claim["id"] = id
	claim["claim"] = "Current external requirement " + id
	claim["planner_directive"] = "Implementation MUST preserve " + id
	result["claims"] = []any{claim}
	return result
}

func TestExternalProtocolPlanReviewDependsOnAllCurrentResearchAndChecksClaims(t *testing.T) {
	s, cfg := newFakeStore(t)
	p, _ := s.RegisterProject(testProjectConfig(t.TempDir()))
	c, coord, _ := s.CreateCollectionWithCoordinator(S(p, "id"), "Protocol", "Implement support for the current MCP protocol specification in an HTTP server.", "Plan")

	researchIDs := []string{}
	researchRows := Rows{}
	claimIDs := []any{}
	for _, claimID := range []string{"claim-a", "claim-b"} {
		r, _, err := s.CreateTask(CreateTaskOptions{
			ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "research", Title: "Fresh external research " + claimID,
			Objective: "research", Reason: "fresh external evidence", ParentTaskID: S(coord, "id"), WorkspaceMode: "collection", Depth: 1, AllowDedupe: false,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.SubmitResult(S(r, "id"), freshResearchResultWithID(claimID)); err != nil {
			t.Fatal(err)
		}
		if err = s.SetTaskStatus(S(r, "id"), TaskComplete, "done"); err != nil {
			t.Fatal(err)
		}
		researchIDs = append(researchIDs, S(r, "id"))
		completedResearch, _ := s.GetTask(S(r, "id"))
		researchRows = append(researchRows, completedResearch)
		claimIDs = append(claimIDs, claimID)
	}

	critic, _, err := s.CreateTask(CreateTaskOptions{
		ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "critic", Title: "Critic", Objective: "critique", Reason: "policy",
		ParentTaskID: S(coord, "id"), DependsOn: researchIDs, WorkspaceMode: "collection", Depth: 1, AllowDedupe: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SubmitResult(S(critic, "id"), Row{
		"status": "complete", "summary": "checked",
		"research_consistency": researchConsistencyFor(researchRows),
	}); err != nil {
		t.Fatal(err)
	}
	if err = s.SetTaskStatus(S(critic, "id"), TaskComplete, "done"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SubmitResult(S(coord, "id"), Row{"status": "complete", "summary": "planned"}); err != nil {
		t.Fatal(err)
	}
	if err = s.SetTaskStatus(S(coord, "id"), TaskComplete, "done"); err != nil {
		t.Fatal(err)
	}
	impl, _, err := s.CreateTask(CreateTaskOptions{
		ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "implement", Title: "Implement", Objective: "implement protocol", Reason: "plan",
		ParentTaskID: S(coord, "id"), WorkspaceMode: "collection", Depth: 1, AllowDedupe: false,
	})
	if err != nil {
		t.Fatal(err)
	}

	sched, _ := NewScheduler(s, cfg, 1)
	if _, err = sched.enforcePlanningPolicy(S(c, "id")); err != nil {
		t.Fatal(err)
	}
	tasks, _ := s.ListTasks(S(c, "id"))
	var planReview Row
	for _, task := range tasks {
		if S(task, "profile") == "plan_review" && S(task, "status") != TaskSuperseded && S(task, "status") != TaskCancelled {
			planReview = task
		}
	}
	if planReview == nil {
		t.Fatalf("missing plan review: %v", tasks)
	}
	deps, _ := s.Dependencies(S(planReview, "id"))
	for _, researchID := range researchIDs {
		if !containsTaskID(deps, researchID) {
			t.Fatalf("plan review missing research dependency %s: %v", researchID, deps)
		}
	}
	implDeps, _ := s.Dependencies(S(impl, "id"))
	if !containsTaskID(implDeps, S(planReview, "id")) {
		t.Fatalf("implementation not gated on plan review: %v", implDeps)
	}
	if _, err = s.SubmitResult(S(planReview, "id"), Row{
		"status": "complete", "verdict": "pass", "summary": "plan is coherent",
		"research_consistency": researchConsistencyFor(researchRows),
	}); err != nil {
		t.Fatalf("plan review claim trace rejected: %v", err)
	}
}

func TestExternalProtocolImplementationReviewDependsOnCurrentResearch(t *testing.T) {
	s, cfg := newFakeStore(t)
	p, _ := s.RegisterProject(testProjectConfig(t.TempDir()))
	c, coord, _ := s.CreateCollectionWithCoordinator(S(p, "id"), "Protocol", "Implement support for the current MCP protocol specification in an HTTP server.", "Plan")
	research, _, err := s.CreateTask(CreateTaskOptions{
		ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "research", Title: "Fresh external research preflight", Objective: "research",
		Reason: "fresh external evidence", ParentTaskID: S(coord, "id"), WorkspaceMode: "collection", Depth: 1, AllowDedupe: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SubmitResult(S(research, "id"), validFreshResearchResult()); err != nil {
		t.Fatal(err)
	}
	_ = s.SetTaskStatus(S(research, "id"), TaskComplete, "done")
	completedResearch, _ := s.GetTask(S(research, "id"))
	impl, _, err := s.CreateTask(CreateTaskOptions{
		ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "implement", Title: "Implement", Objective: "implement", Reason: "plan",
		ParentTaskID: S(coord, "id"), WorkspaceMode: "collection", Depth: 1, AllowDedupe: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	sched, _ := NewScheduler(s, cfg, 1)
	if err = sched.ensureReview(p, c, impl); err != nil {
		t.Fatal(err)
	}
	review, err := s.FindRelatedReview(S(impl, "id"))
	if err != nil || review == nil {
		t.Fatalf("missing review: %v %v", review, err)
	}
	deps, _ := s.Dependencies(S(review, "id"))
	if !containsTaskID(deps, S(research, "id")) {
		t.Fatalf("review must consume current research evidence: %v", deps)
	}
	if _, err = s.SubmitResult(S(review, "id"), Row{
		"status": "complete", "verdict": "pass", "summary": "verified",
		"impact_enumeration":   passingImpactEnumeration(),
		"research_consistency": researchConsistencyFor(Rows{completedResearch}),
		"verification_oracles": []any{Row{"kind": "official_reference_oracle", "name": "official SDK", "used": true}},
	}); err != nil {
		t.Fatalf("review with research trace + authoritative oracle rejected: %v", err)
	}
}

func TestResearchConsumerQualifiesDuplicateClaimIDsAcrossTasks(t *testing.T) {
	r1 := Row{"id": "T-R1", "profile": "research", "status": TaskComplete, "result_json": JSONDumps(validFreshResearchResult())}
	r2 := Row{"id": "T-R2", "profile": "research", "status": TaskComplete, "result_json": JSONDumps(validFreshResearchResult())}
	ids, err := ResearchClaimIDs(Rows{r1, r2})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] != "T-R1:claim-x" || ids[1] != "T-R2:claim-x" {
		t.Fatalf("duplicate claim ids must be task-qualified, got %v", ids)
	}
	ambiguous := Row{"research_consistency": Row{"verdict": "pass", "checked_claim_ids": []any{"claim-x"}, "contradictions": []any{}}}
	if err := ValidateResearchConsumerResult(ambiguous, Rows{r1, r2}, true); err == nil {
		t.Fatal("ambiguous unqualified duplicate claim id should not satisfy traceability")
	}
	qualified := Row{"research_consistency": researchConsistencyFor(Rows{r1, r2})}
	if err := ValidateResearchConsumerResult(qualified, Rows{r1, r2}, true); err != nil {
		t.Fatalf("qualified duplicate claim trace rejected: %v", err)
	}
}

func TestSubmitResearchExceptionRequiresDurableDecision(t *testing.T) {
	s, _ := newFakeStore(t)
	p, err := s.RegisterProject(testProjectConfig(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	c, coord, err := s.CreateCollectionWithCoordinator(S(p, "id"), "Protocol", "Implement support for the current MCP protocol specification in an HTTP server.", "Plan")
	if err != nil {
		t.Fatal(err)
	}
	research, _, err := s.CreateTask(CreateTaskOptions{
		ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "research", Title: "Fresh external research preflight",
		Objective: "research", Reason: "fresh external evidence", ParentTaskID: S(coord, "id"), WorkspaceMode: "collection", Depth: 1, AllowDedupe: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	result := validFreshResearchResult()
	claim := result["claims"].([]any)[0].(Row)
	claim["directive_effect"] = "exception"
	claim["exception_decision_id"] = "DEC-missing"
	if _, err = s.SubmitResult(S(research, "id"), result); err == nil {
		t.Fatal("research exception must reference a durable decision")
	}
	decision, err := s.AddDecision(S(p, "id"), S(c, "id"), S(coord, "id"), "Approve bounded exception", "Operator-approved deviation from current evidence", []any{}, []any{})
	if err != nil {
		t.Fatal(err)
	}
	claim["exception_decision_id"] = S(decision, "id")
	if _, err = s.SubmitResult(S(research, "id"), result); err != nil {
		t.Fatalf("durably authorized research exception rejected: %v", err)
	}
}

func TestCurrentNewsObjectiveGetsFreshResearchWithoutImplementationCeremony(t *testing.T) {
	s, cfg := newFakeStore(t)
	p, err := s.RegisterProject(testProjectConfig(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	c, coord, err := s.CreateCollectionWithCoordinator(S(p, "id"), "Current vendor news", "Research current news and recent events affecting the vendor and summarize the implications.", "Plan")
	if err != nil {
		t.Fatal(err)
	}
	sched, err := NewScheduler(s, cfg, 1)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := sched.enforcePlanningPolicy(S(c, "id"))
	if err != nil || !changed {
		t.Fatalf("expected fresh-research policy mutation, changed=%v err=%v", changed, err)
	}
	tasks, _ := s.ListTasks(S(c, "id"))
	var research Row
	for _, task := range tasks {
		switch S(task, "profile") {
		case "research":
			research = task
		case "explore", "critic", "plan_review", "implement":
			t.Fatalf("non-implementation current-news objective should not add %s ceremony: %v", S(task, "profile"), tasks)
		}
	}
	if research == nil {
		t.Fatalf("current-news objective missing deterministic research: %v", tasks)
	}
	deps, _ := s.Dependencies(S(coord, "id"))
	if !containsTaskID(deps, S(research, "id")) {
		t.Fatalf("coordinator should wait for fresh research: %v", deps)
	}
}

func TestResearchConsumerRejectsChangedInheritedDirective(t *testing.T) {
	researchResult := validFreshResearchResult()
	researchResult["claims"] = []any{Row{
		"id": "C1", "claim": "initialize MUST include protocolVersion", "source": "spec", "authority": "official specification",
		"normative_level": "MUST", "planner_directive": "Missing, non-string, or malformed protocolVersion MUST return Invalid Params.",
		"directive_effect": "preserve", "confidence": "high",
	}}
	research := Row{"id": "T-R", "profile": "research", "status": TaskComplete, "result_json": JSONDumps(researchResult)}
	consistency := researchConsistencyFor(Rows{research})
	claims := consistency["claims"].([]any)
	claims[0].(Row)["inherited_directive"] = "Absent params may succeed with the default protocol version."
	result := Row{"research_consistency": consistency}
	if err := ValidateResearchConsumerResult(result, Rows{research}, true); err == nil {
		t.Fatal("changed inherited directive must be rejected even when the consumer claims verdict=pass")
	}
}

func TestActiveGenerationLegacyResearchConsistencyCriticIsRevalidated(t *testing.T) {
	s, cfg := newFakeStore(t)
	p, err := s.RegisterProject(testProjectConfig(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	c, coord, err := s.CreateCollectionWithCoordinator(S(p, "id"), "Protocol", "Implement support for the current MCP protocol specification in an HTTP server.", "Plan")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetTaskStatus(S(coord, "id"), TaskReady, "ready for preflight"); err != nil {
		t.Fatal(err)
	}
	explore, _, err := s.CreateTask(CreateTaskOptions{
		ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "explore", Title: "Explore current implementation surface",
		Objective: "map", Reason: "policy", ParentTaskID: S(coord, "id"), RequestedByTaskID: S(coord, "id"), WorkspaceMode: "collection", Depth: 1, AllowDedupe: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SubmitResult(S(explore, "id"), Row{"status": "complete", "summary": "mapped"}); err != nil {
		t.Fatal(err)
	}
	if err = s.SetTaskStatus(S(explore, "id"), TaskComplete, "done"); err != nil {
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
	if err = s.SetTaskStatus(S(research, "id"), TaskComplete, "done"); err != nil {
		t.Fatal(err)
	}
	critic, _, err := s.CreateTask(CreateTaskOptions{
		ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "critic", Title: "Preflight implementation plan",
		Objective: "critique", Reason: "policy", ParentTaskID: S(coord, "id"), RequestedByTaskID: S(coord, "id"),
		DependsOn: []string{S(explore, "id"), S(research, "id")}, WorkspaceMode: "collection", Depth: 1, AllowDedupe: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	legacyResult := Row{
		"status": "complete", "summary": "legacy 0.2.3 critic",
		"research_consistency": Row{"verdict": "pass", "checked_claim_ids": []any{"claim-x"}, "contradictions": []any{}},
	}
	if err = s.Mutate("UPDATE tasks SET status='COMPLETE',result_json="+SQLLiteral(JSONDumps(legacyResult))+" WHERE id="+SQLLiteral(S(critic, "id")), "seed legacy critic"); err != nil {
		t.Fatal(err)
	}
	for _, depID := range []string{S(explore, "id"), S(research, "id"), S(critic, "id")} {
		if err = s.AddDependency(S(coord, "id"), depID); err != nil {
			t.Fatal(err)
		}
	}

	sched, _ := NewScheduler(s, cfg, 1)
	changed, err := sched.enforcePlanningPolicy(S(c, "id"))
	if err != nil || !changed {
		t.Fatalf("expected active-generation critic revalidation, changed=%v err=%v", changed, err)
	}
	oldCritic, _ := s.GetTask(S(critic, "id"))
	if S(oldCritic, "status") != TaskSuperseded {
		t.Fatalf("legacy critic must be superseded, got %s", S(oldCritic, "status"))
	}
	tasks, _ := s.ListTasks(S(c, "id"))
	var replacement Row
	for _, task := range tasks {
		if S(task, "profile") == "critic" && S(task, "title") == "Revalidate inherited research constraints" && S(task, "status") != TaskSuperseded {
			replacement = task
		}
	}
	if replacement == nil {
		t.Fatalf("missing replacement critic: %v", tasks)
	}
	deps, _ := s.Dependencies(S(replacement, "id"))
	if !containsTaskID(deps, S(explore, "id")) || !containsTaskID(deps, S(research, "id")) {
		t.Fatalf("replacement critic missing original evidence deps: %v", deps)
	}
	coordDeps, _ := s.Dependencies(S(coord, "id"))
	if !containsTaskID(coordDeps, S(replacement, "id")) || containsTaskID(coordDeps, S(critic, "id")) {
		t.Fatalf("coordinator not rewired to replacement critic: %v", coordDeps)
	}
	events, _ := s.EventsForCollection(S(c, "id"), 200)
	found := false
	for _, event := range events {
		if S(event, "event_type") == "RESEARCH_CONSISTENCY_REVALIDATION_REQUIRED" {
			found = true
		}
	}
	if !found {
		t.Fatal("missing RESEARCH_CONSISTENCY_REVALIDATION_REQUIRED event")
	}
}

func TestRunningCollectionRevalidatesLegacyCriticAfterGenerationCoordinatorCompleted(t *testing.T) {
	s, cfg := newFakeStore(t)
	p, err := s.RegisterProject(testProjectConfig(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	c, coord, err := s.CreateCollectionWithCoordinator(S(p, "id"), "Protocol", "Implement support for the current MCP protocol specification in an HTTP server.", "Plan")
	if err != nil {
		t.Fatal(err)
	}
	explore, _, err := s.CreateTask(CreateTaskOptions{ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "explore", Title: "Explore current implementation surface", Objective: "map", Reason: "policy", ParentTaskID: S(coord, "id"), RequestedByTaskID: S(coord, "id"), WorkspaceMode: "collection", Depth: 1, AllowDedupe: false})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = s.SubmitResult(S(explore, "id"), Row{"status": "complete", "summary": "mapped"})
	_ = s.SetTaskStatus(S(explore, "id"), TaskComplete, "done")
	research, _, err := s.CreateTask(CreateTaskOptions{ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "research", Title: "Fresh external research preflight", Objective: "research", Reason: "fresh external evidence", ParentTaskID: S(coord, "id"), RequestedByTaskID: S(coord, "id"), WorkspaceMode: "collection", Depth: 1, AllowDedupe: false})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SubmitResult(S(research, "id"), validFreshResearchResult()); err != nil {
		t.Fatal(err)
	}
	_ = s.SetTaskStatus(S(research, "id"), TaskComplete, "done")
	critic, _, err := s.CreateTask(CreateTaskOptions{ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "critic", Title: "Preflight implementation plan", Objective: "critique", Reason: "policy", ParentTaskID: S(coord, "id"), RequestedByTaskID: S(coord, "id"), DependsOn: []string{S(explore, "id"), S(research, "id")}, WorkspaceMode: "collection", Depth: 1, AllowDedupe: false})
	if err != nil {
		t.Fatal(err)
	}
	legacyResult := Row{"status": "complete", "summary": "legacy critic", "research_consistency": Row{"verdict": "pass", "checked_claim_ids": []any{"claim-x"}, "contradictions": []any{}}}
	if err = s.Mutate("UPDATE tasks SET status='COMPLETE',result_json="+SQLLiteral(JSONDumps(legacyResult))+" WHERE id="+SQLLiteral(S(critic, "id")), "seed legacy critic"); err != nil {
		t.Fatal(err)
	}
	for _, depID := range []string{S(explore, "id"), S(research, "id"), S(critic, "id")} {
		if err = s.AddDependency(S(coord, "id"), depID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.SubmitResult(S(coord, "id"), Row{"status": "complete", "summary": "old planning completed"}); err != nil {
		t.Fatal(err)
	}
	if err = s.SetTaskStatus(S(coord, "id"), TaskComplete, "old coordinator complete"); err != nil {
		t.Fatal(err)
	}
	if err = s.SetCollectionStatus(S(c, "id"), "RUNNING", false); err != nil {
		t.Fatal(err)
	}

	sched, _ := NewScheduler(s, cfg, 1)
	changed, err := sched.enforcePlanningPolicy(S(c, "id"))
	if err != nil || !changed {
		t.Fatalf("expected completed-generation upgrade reconciliation, changed=%v err=%v", changed, err)
	}
	oldCritic, _ := s.GetTask(S(critic, "id"))
	if S(oldCritic, "status") != TaskSuperseded {
		t.Fatalf("legacy critic must be superseded, got %s", S(oldCritic, "status"))
	}
	tasks, _ := s.ListTasks(S(c, "id"))
	var replacement, repair Row
	for _, task := range tasks {
		if S(task, "profile") == "critic" && S(task, "title") == "Revalidate inherited research constraints" && S(task, "status") != TaskSuperseded {
			replacement = task
		}
		if S(task, "profile") == "coordinator" && S(task, "title") == "Apply inherited research revalidation" && S(task, "status") != TaskSuperseded {
			repair = task
		}
	}
	if replacement == nil || repair == nil {
		t.Fatalf("expected replacement critic and repair coordinator, tasks=%v", tasks)
	}
	repairDeps, _ := s.Dependencies(S(repair, "id"))
	if !containsTaskID(repairDeps, S(replacement, "id")) {
		t.Fatalf("repair coordinator must wait on replacement critic: %v", repairDeps)
	}
}

func TestHistoricalCompletedLegacyResearchConsumerIsNotRetroactivelyReopened(t *testing.T) {
	s, cfg := newFakeStore(t)
	p, err := s.RegisterProject(testProjectConfig(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	c, coord, err := s.CreateCollectionWithCoordinator(S(p, "id"), "Protocol", "Implement support for the current MCP protocol specification in an HTTP server.", "Plan")
	if err != nil {
		t.Fatal(err)
	}
	research, _, err := s.CreateTask(CreateTaskOptions{ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "research", Title: "Fresh external research preflight", Objective: "research", Reason: "fresh external evidence", ParentTaskID: S(coord, "id"), RequestedByTaskID: S(coord, "id"), WorkspaceMode: "collection", Depth: 1, AllowDedupe: false})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SubmitResult(S(research, "id"), validFreshResearchResult()); err != nil {
		t.Fatal(err)
	}
	_ = s.SetTaskStatus(S(research, "id"), TaskComplete, "done")
	critic, _, err := s.CreateTask(CreateTaskOptions{ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "critic", Title: "Preflight implementation plan", Objective: "critique", Reason: "policy", ParentTaskID: S(coord, "id"), RequestedByTaskID: S(coord, "id"), DependsOn: []string{S(research, "id")}, WorkspaceMode: "collection", Depth: 1, AllowDedupe: false})
	if err != nil {
		t.Fatal(err)
	}
	legacyResult := Row{"status": "complete", "summary": "historical critic", "research_consistency": Row{"verdict": "pass", "checked_claim_ids": []any{"claim-x"}, "contradictions": []any{}}}
	if err = s.Mutate("UPDATE tasks SET status='COMPLETE',result_json="+SQLLiteral(JSONDumps(legacyResult))+" WHERE id="+SQLLiteral(S(critic, "id")), "seed historical critic"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SubmitResult(S(coord, "id"), Row{"status": "complete", "summary": "historical planning complete"}); err != nil {
		t.Fatal(err)
	}
	_ = s.SetTaskStatus(S(coord, "id"), TaskComplete, "done")
	_ = s.SetCollectionStatus(S(c, "id"), "COMPLETE", false)

	sched, _ := NewScheduler(s, cfg, 1)
	if _, err = sched.enforcePlanningPolicy(S(c, "id")); err != nil {
		t.Fatal(err)
	}
	after, _ := s.GetTask(S(critic, "id"))
	if S(after, "status") != TaskComplete {
		t.Fatalf("historical completed critic must remain COMPLETE, got %s", S(after, "status"))
	}
}
