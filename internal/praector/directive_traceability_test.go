package praector

import (
	"encoding/json"
	"strings"
	"testing"
)

func criticDirectiveResult() Row {
	return Row{
		"status": "complete", "summary": "preflight complete",
		"planner_directives": []any{
			Row{"text": "MUST enumerate every events-table writer before finalizing the schema change.", "normative_level": "MUST"},
			"MUST NOT relax the loopback bind gate.",
		},
		"qa_directives": []any{"VERIFY: the rejected-submit error names each directive id."},
		"risks":         []any{Row{"risk": "silent degradation", "mitigation": "Pin the reflog-read form in a regression test."}},
	}
}

func directiveDispositions(directives []InheritedDirective, disposition string, decisionID string) []any {
	out := []any{}
	for _, directive := range directives {
		item := Row{"directive_id": directive.ID, "disposition": disposition, "evidence": []any{"inspected the integrated candidate"}}
		if disposition != "satisfied" && decisionID != "" {
			item["decision_id"] = decisionID
		}
		out = append(out, item)
	}
	return out
}

func traceabilityCollection(t *testing.T) (*Store, Row, Row, Row) {
	t.Helper()
	s, _ := newFakeStore(t)
	p, err := s.RegisterProject(testProjectConfig(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	c, coord, err := s.CreateCollectionWithCoordinator(S(p, "id"), "Gate traceability", "Add authenticated endpoint with durable events", "Plan")
	if err != nil {
		t.Fatal(err)
	}
	critic, _, err := s.CreateTask(CreateTaskOptions{
		ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "critic", Title: "Preflight",
		Objective: "preflight", Reason: "plan", ParentTaskID: S(coord, "id"), WorkspaceMode: "collection", Depth: 1, AllowDedupe: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = s.SubmitResult(S(critic, "id"), criticDirectiveResult())
	if err = s.SetTaskStatus(S(critic, "id"), TaskComplete, "critic complete"); err != nil {
		t.Fatal(err)
	}
	return s, p, c, critic
}

func TestDirectiveTraceabilityRejectsMissingDispositionsThenAcceptsCompleteSet(t *testing.T) {
	s, p, c, critic := traceabilityCollection(t)
	impl, _, err := s.CreateTask(CreateTaskOptions{
		ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "implement", Title: "Implement",
		Objective: "implement", Reason: "plan", ParentTaskID: S(critic, "id"), DependsOn: []string{S(critic, "id")}, WorkspaceMode: "collection", Depth: 2, AllowDedupe: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	review, _, err := s.CreateTask(CreateTaskOptions{
		ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "review", Title: "Review",
		Objective: "review", Reason: "policy", ParentTaskID: S(impl, "id"), DependsOn: []string{S(impl, "id"), S(critic, "id")}, WorkspaceMode: "collection", Depth: 3, AllowDedupe: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	ancestors, err := s.DirectiveAncestors(S(review, "id"))
	if err != nil {
		t.Fatal(err)
	}
	directives := InheritedDirectives(ancestors)
	if len(directives) != 4 {
		t.Fatalf("expected two planner directives, one qa directive, and one risk mitigation, got %v", directives)
	}
	for _, directive := range directives {
		if directive.SourceProfile != "critic" || directive.TaskID != S(critic, "id") {
			t.Fatalf("directive lineage wrong: %#v", directive)
		}
	}
	missing, err := s.SubmitResult(S(review, "id"), Row{"status": "complete", "verdict": "pass", "summary": "looks good", "impact_enumeration": passingImpactEnumeration()})
	if err == nil {
		t.Fatalf("passing review without dispositions must be rejected, submitted=%v", missing)
	}
	for _, directive := range directives {
		if !strings.Contains(err.Error(), directive.ID) {
			t.Fatalf("reject error %q must name missing directive %s", err.Error(), directive.ID)
		}
	}
	if _, err = s.SubmitResult(S(review, "id"), Row{
		"status": "complete", "verdict": "pass", "summary": "looks good",
		"impact_enumeration":     passingImpactEnumeration(),
		"directive_dispositions": directiveDispositions(directives, "satisfied", ""),
	}); err != nil {
		t.Fatalf("complete dispositions rejected: %v", err)
	}
}

func TestDirectiveDispositionRequiresDurableDecisionID(t *testing.T) {
	s, p, c, critic := traceabilityCollection(t)
	impl, _, _ := s.CreateTask(CreateTaskOptions{
		ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "implement", Title: "Implement",
		Objective: "implement", Reason: "plan", ParentTaskID: S(critic, "id"), DependsOn: []string{S(critic, "id")}, WorkspaceMode: "collection", Depth: 2, AllowDedupe: false,
	})
	review, _, _ := s.CreateTask(CreateTaskOptions{
		ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "review", Title: "Review",
		Objective: "review", Reason: "policy", ParentTaskID: S(impl, "id"), DependsOn: []string{S(impl, "id"), S(critic, "id")}, WorkspaceMode: "collection", Depth: 3, AllowDedupe: false,
	})
	ancestors, _ := s.DirectiveAncestors(S(review, "id"))
	directives := InheritedDirectives(ancestors)
	if _, err := s.SubmitResult(S(review, "id"), Row{
		"status": "complete", "verdict": "pass", "summary": "looks good",
		"impact_enumeration":     passingImpactEnumeration(),
		"directive_dispositions": directiveDispositions(directives, "deferred", ""),
	}); err == nil || !strings.Contains(err.Error(), "decision_id") {
		t.Fatalf("deferred disposition without a durable decision id must be rejected: %v", err)
	}
	if _, err := s.SubmitResult(S(review, "id"), Row{
		"status": "complete", "verdict": "pass", "summary": "looks good",
		"impact_enumeration":     passingImpactEnumeration(),
		"directive_dispositions": directiveDispositions(directives, "explicit_exception", "DEC-doesnotexist"),
	}); err == nil || !strings.Contains(err.Error(), "not a durable decision") {
		t.Fatalf("disposition citing a nonexistent decision must be rejected: %v", err)
	}
	decision, err := s.AddDecision(S(p, "id"), S(c, "id"), S(critic, "id"), "Defer the sweep directive to the next revision", "Bounded scope", []any{}, []any{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SubmitResult(S(review, "id"), Row{
		"status": "complete", "verdict": "pass", "summary": "looks good",
		"impact_enumeration":     passingImpactEnumeration(),
		"directive_dispositions": directiveDispositions(directives, "deferred", S(decision, "id")),
	}); err != nil {
		t.Fatalf("deferred disposition with a durable decision id rejected: %v", err)
	}
}

func TestDirectiveTraceabilityZeroDirectivesPassesCleanly(t *testing.T) {
	s, p, c, _ := traceabilityCollection(t)
	impl, _, _ := s.CreateTask(CreateTaskOptions{
		ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "implement", Title: "Implement other",
		Objective: "implement", Reason: "plan", ParentTaskID: S(p, "id"), WorkspaceMode: "collection", Depth: 1, AllowDedupe: false,
	})
	review, _, _ := s.CreateTask(CreateTaskOptions{
		ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "review", Title: "Review other",
		Objective: "review", Reason: "policy", ParentTaskID: S(impl, "id"), DependsOn: []string{S(impl, "id")}, WorkspaceMode: "collection", Depth: 2, AllowDedupe: false,
	})
	ancestors, _ := s.DirectiveAncestors(S(review, "id"))
	if directives := InheritedDirectives(ancestors); len(directives) != 0 {
		t.Fatalf("review with no planning ancestors must harvest zero directives: %v", directives)
	}
	if err := ValidateDirectiveTraceability(Row{"verdict": "pass"}, nil, true, nil); err != nil {
		t.Fatalf("empty directive set must pass cleanly: %v", err)
	}
	if _, err := s.SubmitResult(S(review, "id"), Row{"status": "complete", "verdict": "pass", "summary": "clean", "impact_enumeration": passingImpactEnumeration()}); err != nil {
		t.Fatalf("zero-directive review rejected: %v", err)
	}
	project, _ := s.GetProject(S(p, "id"))
	collection, _ := s.GetCollection(S(c, "id"))
	envelope, err := BuildTaskEnvelope(s, project, collection, review, Row{"id": "A-empty", "attempt_no": 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := envelopeContextRow(t, envelope)["inherited_directives"]; ok {
		t.Fatal("zero-directive envelope context must omit the projection entirely")
	}
}

func envelopeContextRow(t *testing.T, envelope string) Row {
	t.Helper()
	start := strings.Index(envelope, "## Durable context")
	if start < 0 {
		t.Fatal("envelope has no durable context block")
	}
	rest := envelope[start:]
	start = strings.Index(rest, "```json")
	if start < 0 {
		t.Fatal("durable context block is not json")
	}
	rest = rest[start+len("```json"):]
	end := strings.Index(rest, "```")
	if end < 0 {
		t.Fatal("unterminated envelope json context block")
	}
	var ctx Row
	if err := json.Unmarshal([]byte(strings.TrimSpace(rest[:end])), &ctx); err != nil {
		t.Fatalf("unparseable envelope context: %v", err)
	}
	return ctx
}

func TestImpactEnumerationRequiredAndDeadKnobBlocking(t *testing.T) {
	base := Row{"verdict": "pass", "summary": "s"}
	if err := ValidateReviewImpactEnumeration(base, nil); err == nil {
		t.Fatal("missing impact_enumeration must be rejected on pass")
	}
	if err := ValidateReviewImpactEnumeration(Row{"verdict": "pass", "impact_enumeration": Row{"method": "grep", "sites": []any{}}}, nil); err == nil {
		t.Fatal("empty impact_enumeration must be rejected on pass")
	}
	if err := ValidateReviewImpactEnumeration(Row{"verdict": "pass", "impact_enumeration": Row{"sites": []any{Row{"site": "x", "disposition": "affected", "evidence": "e"}}}}, nil); err == nil {
		t.Fatal("impact_enumeration without a named method must be rejected")
	}
	if err := ValidateReviewImpactEnumeration(Row{"verdict": "pass", "impact_enumeration": passingImpactEnumeration()}, nil); err != nil {
		t.Fatalf("method + per-site disposition + evidence must be accepted: %v", err)
	}
	if err := ValidateReviewImpactEnumeration(Row{"verdict": "fail", "summary": "s"}, nil); err != nil {
		t.Fatalf("failed reviews are not gated on impact_enumeration: %v", err)
	}
	knob := Row{"verdict": "pass", "impact_enumeration": Row{"method": "config sweep", "sites": []any{
		Row{"site": "config.go:MCPEnabled", "disposition": "affected", "evidence": "new flag", "introduced_knob": true},
	}}}
	if err := ValidateReviewImpactEnumeration(knob, nil); err == nil || !strings.Contains(err.Error(), "introduced-but-never-read") {
		t.Fatalf("introduced-but-never-read knob must be blocking: %v", err)
	}
	knob["impact_enumeration"] = Row{"method": "config sweep", "sites": []any{
		Row{"site": "config.go:MCPEnabled", "disposition": "affected", "evidence": "new flag", "introduced_knob": true, "reader_paths": []any{"mcp_server.go:serve"}},
	}}
	if err := ValidateReviewImpactEnumeration(knob, nil); err != nil {
		t.Fatalf("introduced knob with a named reader must not be flagged: %v", err)
	}
}

func TestPostRepairReviewCarriesFailedReviewFindings(t *testing.T) {
	s, _ := newFakeStore(t)
	p, err := s.RegisterProject(testProjectConfig(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	c, coord, err := s.CreateCollectionWithCoordinator(S(p, "id"), "Carry-over", "Add authenticated endpoint", "Plan")
	if err != nil {
		t.Fatal(err)
	}
	impl, _, _ := s.CreateTask(CreateTaskOptions{
		ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "implement", Title: "Initial implementation",
		Objective: "Implement endpoint", Reason: "plan", ParentTaskID: S(coord, "id"), WorkspaceMode: "collection", Depth: 1, AllowDedupe: false,
	})
	_ = s.SetTaskStatus(S(impl, "id"), TaskComplete, "complete")
	review1, _, _ := s.CreateTask(CreateTaskOptions{
		ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "review", Title: "Review initial",
		Objective: "Review endpoint", Reason: "policy", ParentTaskID: S(impl, "id"), DependsOn: []string{S(impl, "id")}, WorkspaceMode: "collection", Depth: 2, AllowDedupe: false,
	})
	_ = s.AddEdge(S(impl, "id"), S(review1, "id"), S(c, "id"), "verifies")
	_, _ = s.SubmitResult(S(review1, "id"), Row{"status": "complete", "summary": "authentication defect", "verdict": "fail", "findings": []any{"GET handler reads the task row before the auth check"}})
	sch := &Scheduler{Store: s}
	if err = sch.handleFailedReview(p, review1); err != nil {
		t.Fatal(err)
	}
	tasks, _ := s.ListTasks(S(c, "id"))
	var repair Row
	for _, task := range tasks {
		if S(task, "profile") == "implement" && S(task, "parent_task_id") == S(review1, "id") {
			repair = task
		}
	}
	if repair == nil {
		t.Fatal("missing repair task")
	}
	_ = s.SetTaskStatus(S(review1, "id"), TaskComplete, "semantic fail recorded")
	_ = s.SetTaskStatus(S(repair, "id"), TaskComplete, "repair integrated")
	repair, _ = s.GetTask(S(repair, "id"))
	if err = sch.ensureReview(p, c, repair); err != nil {
		t.Fatal(err)
	}
	tasks, _ = s.ListTasks(S(c, "id"))
	var review2 Row
	for _, task := range tasks {
		if S(task, "profile") == "review" && S(task, "id") != S(review1, "id") {
			review2 = task
		}
	}
	if review2 == nil {
		t.Fatal("missing fresh post-repair review")
	}
	deps, _ := s.Dependencies(S(review2, "id"))
	if !containsTaskID(deps, S(review1, "id")) {
		t.Fatalf("fresh review must depend on the failed review so its findings project into the envelope: %v", deps)
	}
	project, _ := s.GetProject(S(p, "id"))
	collection, _ := s.GetCollection(S(c, "id"))
	freshEnvelope, err := BuildTaskEnvelope(s, project, collection, review2, Row{"id": "A-fresh", "attempt_no": 1})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(freshEnvelope, "GET handler reads the task row before the auth check") {
		t.Fatal("post-repair envelope must carry the failed review's findings")
	}
	if !strings.Contains(freshEnvelope, "re-check or explicitly refute") {
		t.Fatal("post-repair objective must demand per-finding re-check, not amnesia")
	}
	normal, _, _ := s.CreateTask(CreateTaskOptions{
		ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "review", Title: "Review standalone",
		Objective: "Review other", Reason: "policy", ParentTaskID: S(impl, "id"), DependsOn: []string{S(impl, "id")}, WorkspaceMode: "collection", Depth: 2, AllowDedupe: false,
	})
	if _, err = BuildTaskEnvelope(s, project, collection, normal, Row{"id": "A-normal", "attempt_no": 1}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(S(normal, "objective"), "re-check or explicitly refute") {
		t.Fatal("a review without a failed parent must keep the ordinary review objective")
	}
	if strings.Contains(S(normal, "objective"), "Fresh review after repair") {
		t.Fatal("a review without a failed parent must not use the post-repair title/objective")
	}
}

func TestLegacyReviewReachesAcceptanceViaDocumentedException(t *testing.T) {
	s, _ := newFakeStore(t)
	p, err := s.RegisterProject(testProjectConfig(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	c, coord, err := s.CreateCollectionWithCoordinator(S(p, "id"), "Legacy collection", "Add authenticated endpoint", "Plan")
	if err != nil {
		t.Fatal(err)
	}
	critic, _, _ := s.CreateTask(CreateTaskOptions{
		ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "critic", Title: "Preflight",
		Objective: "preflight", Reason: "plan", ParentTaskID: S(coord, "id"), WorkspaceMode: "collection", Depth: 1, AllowDedupe: false,
	})
	_, _ = s.SubmitResult(S(critic, "id"), criticDirectiveResult())
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
	// A pre-upgrade review result: no directive_dispositions, no
	// impact_enumeration, written before these validators existed. It is
	// planted directly because a new passing submit is always strict.
	_, _ = s.SubmitResult(S(review, "id"), Row{"status": "complete", "summary": "pass", "verdict": "pass"})
	if err = s.SetTaskStatus(S(review, "id"), TaskComplete, "legacy review complete"); err != nil {
		t.Fatal(err)
	}
	review, _ = s.GetTask(S(review, "id"))
	review["result_json"] = JSONDumps(Row{"status": "complete", "summary": "pass", "verdict": "pass"})
	if err = s.Mutate("UPDATE tasks SET result_json="+SQLLiteral(JSONDumps(Row{"status": "complete", "summary": "pass", "verdict": "pass"}))+" WHERE id="+SQLLiteral(S(review, "id")), "plant legacy review result"); err != nil {
		t.Fatal(err)
	}
	if err = s.SetTaskStatus(S(impl, "id"), TaskComplete, "keep complete"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SubmitResult(S(coord, "id"), Row{"status": "complete", "summary": "planned"}); err != nil {
		t.Fatal(err)
	}
	_ = s.SetTaskStatus(S(coord, "id"), TaskComplete, "planning complete")

	collection, _ := s.GetCollection(S(c, "id"))
	if _, _, err = validateCollectionForAcceptance(s, collection); err == nil || !strings.Contains(err.Error(), "directive_dispositions") {
		t.Fatalf("legacy review without dispositions must block acceptance before the exception: %v", err)
	}
	decision, err := s.AddDecision(S(p, "id"), S(c, "id"), "", LegacyGateTraceabilityMarker+" pre-traceability review results accepted as-is for COL history", "Validators postdate this collection's reviews", []any{}, []any{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = validateCollectionForAcceptance(s, collection); err != nil {
		t.Fatalf("documented single-decision legacy exception must clear acceptance: %v", err)
	}
	_ = decision
}

func TestDirectiveIDDerivationIsStableAndDistinctFromResearchRefs(t *testing.T) {
	tasks := Rows{{
		"id": "T-critic1", "profile": "critic", "status": TaskComplete,
		"result_json": JSONDumps(Row{"planner_directives": []any{"d0", "d1"}, "qa_directives": []any{"q0"}}),
	}, {
		"id": "T-advise1", "profile": "advise", "status": TaskComplete,
		"result_json": JSONDumps(Row{"recommendations": []any{"r0"}, "risks": []any{Row{"risk": "x", "mitigation": "m0"}, Row{"risk": "y"}}, "tradeoffs": []any{"ignored"}, "revisit_triggers": []any{"ignored"}}),
	}}
	first := InheritedDirectives(tasks)
	second := InheritedDirectives(tasks)
	if len(first) != 5 {
		t.Fatalf("expected planner:0/1, qa:0, recommendation:0, and advise risk mitigation:0; got %#v", first)
	}
	for i := range first {
		if first[i].ID != second[i].ID || first[i].Text != second[i].Text {
			t.Fatalf("directive derivation must be stable across reads: %v vs %v", first, second)
		}
		if strings.HasPrefix(first[i].ID, "claim") {
			t.Fatalf("directive ids must not collide with research claim refs: %v", first[i].ID)
		}
	}
	want := []string{"T-advise1:recommendation:0", "T-advise1:risk_mitigation:0", "T-critic1:planner_directive:0", "T-critic1:planner_directive:1", "T-critic1:qa_directive:0"}
	got := []string{}
	for _, d := range first {
		got = append(got, d.ID)
	}
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want[:len(got)])
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("directive ids = %s want %s", gotJSON, wantJSON)
	}
	if _, err := json.Marshal(first); err != nil {
		t.Fatal(err)
	}
}
