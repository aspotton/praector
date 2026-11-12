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
		item := Row{"directive_id": directive.ID, "disposition": disposition, "evidence": []any{"inspected internal/praector/policy.go for the directive's effect"}}
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
	}); err == nil || !strings.Contains(err.Error(), "MUST-level") {
		t.Fatalf("deferring a structured MUST-level directive must be rejected even with a decision id: %v", err)
	}
	if _, err = s.SubmitResult(S(review, "id"), Row{
		"status": "complete", "verdict": "pass", "summary": "looks good",
		"impact_enumeration":     passingImpactEnumeration(),
		"directive_dispositions": directiveDispositions(directives, "explicit_exception", S(decision, "id")),
	}); err != nil {
		t.Fatalf("explicit_exception disposition with a durable decision id rejected: %v", err)
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
	if err := ValidateReviewImpactEnumeration(base, nil, nil); err == nil {
		t.Fatal("missing impact_enumeration must be rejected on pass")
	}
	if err := ValidateReviewImpactEnumeration(Row{"verdict": "pass", "impact_enumeration": Row{"method": "grep", "sites": []any{}}}, nil, nil); err == nil {
		t.Fatal("empty impact_enumeration must be rejected on pass")
	}
	if err := ValidateReviewImpactEnumeration(Row{"verdict": "pass", "impact_enumeration": Row{"sites": []any{Row{"site": "x", "disposition": "affected", "evidence": "e"}}}}, nil, nil); err == nil {
		t.Fatal("impact_enumeration without a named method must be rejected")
	}
	if err := ValidateReviewImpactEnumeration(Row{"verdict": "pass", "impact_enumeration": passingImpactEnumeration()}, nil, nil); err != nil {
		t.Fatalf("method + per-site disposition + evidence must be accepted: %v", err)
	}
	if err := ValidateReviewImpactEnumeration(Row{"verdict": "fail", "summary": "s"}, nil, nil); err != nil {
		t.Fatalf("failed reviews are not gated on impact_enumeration: %v", err)
	}
	knob := Row{"verdict": "pass", "impact_enumeration": Row{"method": "config sweep", "sites": []any{
		Row{"site": "config.go:MCPEnabled", "disposition": "affected", "evidence": "grep -n MCPEnabled internal/praector/config.go shows the new flag", "introduced_knob": true},
	}}}
	if err := ValidateReviewImpactEnumeration(knob, nil, nil); err == nil || !strings.Contains(err.Error(), "introduced-but-never-read") {
		t.Fatalf("introduced-but-never-read knob must be blocking: %v", err)
	}
	knob["impact_enumeration"] = Row{"method": "config sweep", "sites": []any{
		Row{"site": "config.go:MCPEnabled", "disposition": "affected", "evidence": "grep -n MCPEnabled internal/praector/config.go shows the new flag", "introduced_knob": true, "reader_paths": []any{"mcp_server.go:serve"}},
	}}
	if err := ValidateReviewImpactEnumeration(knob, nil, nil); err != nil {
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
	decision, err := s.AddDecision(S(p, "id"), S(c, "id"), "", LegacyGateTraceabilityMarker+" pre-traceability review results accepted as-is for "+S(c, "id"), "Validators postdate this collection's reviews", []any{}, []any{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = validateCollectionForAcceptance(s, collection); err != nil {
		t.Fatalf("documented single-decision legacy exception must clear acceptance: %v", err)
	}
	_ = decision
}

func derivedKnobCandidates() []DerivedConfigCandidate {
	return []DerivedConfigCandidate{{Token: "--derive-test-knob", Path: "cmd.go"}}
}

func TestDerivedCandidateRuleFiresWithoutIntroducedKnobFlag(t *testing.T) {
	candidates := derivedKnobCandidates()
	base := Row{"verdict": "pass", "impact_enumeration": Row{"method": "call-graph walk", "sites": []any{
		Row{"site": "internal/praector/store.go", "disposition": "unaffected", "evidence": "go test ./internal/praector/ -run TestDirective"},
	}}}
	if err := ValidateReviewImpactEnumeration(base, nil, candidates); err == nil || !strings.Contains(err.Error(), "--derive-test-knob") {
		t.Fatalf("a review omitting a derived candidate without introduced_knob set must be rejected naming it: %v", err)
	}
	named := cloneRow(base)
	named["impact_enumeration"] = Row{"method": "call-graph walk", "sites": []any{
		Row{"site": "internal/praector/store.go", "disposition": "affected", "evidence": "go test ./internal/praector/ -run TestDirective", "reader_paths": []any{"internal/praector/config.go reads --derive-test-knob"}},
	}}
	if err := ValidateReviewImpactEnumeration(named, nil, candidates); err != nil {
		t.Fatalf("candidate named by a site reader_paths must pass: %v", err)
	}
	disposed := cloneRow(base)
	disposed["impact_enumeration"] = Row{"method": "call-graph walk", "sites": base["impact_enumeration"].(Row)["sites"], "candidate_dispositions": []any{
		Row{"candidate": "--derive-test-knob", "disposition": "not-introduced", "evidence": "predates this collection per git log internal/praector/config.go"},
	}}
	if err := ValidateReviewImpactEnumeration(disposed, nil, candidates); err != nil {
		t.Fatalf("candidate dispositioned not-introduced must pass: %v", err)
	}
	unread := cloneRow(disposed)
	unread["impact_enumeration"] = Row{"method": "call-graph walk", "sites": base["impact_enumeration"].(Row)["sites"], "candidate_dispositions": []any{
		Row{"candidate": "--derive-test-knob", "disposition": "deliberately-unread", "evidence": "run make test-go documents the unread decision"},
	}}
	if err := ValidateReviewImpactEnumeration(unread, nil, candidates); err == nil || !strings.Contains(err.Error(), "decision_id") {
		t.Fatalf("deliberately-unread without a durable decision id must be rejected: %v", err)
	}
	if err := ValidateReviewImpactEnumeration(base, []string{"DEC-legacy"}, nil); err != nil {
		t.Fatalf("nil derived candidates must keep byte-identical legacy behavior: %v", err)
	}
}

func TestSubmitResultRejectsUnaccountedDerivedCandidate(t *testing.T) {
	s, p, c, critic := traceabilityCollection(t)
	ancestors, err := s.DirectiveAncestors(S(critic, "id"))
	if err != nil {
		t.Fatal(err)
	}
	_ = ancestors
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
	directiveAncestors, err := s.DirectiveAncestors(S(review, "id"))
	if err != nil {
		t.Fatal(err)
	}
	directives := InheritedDirectives(directiveAncestors)
	if _, err = s.SubmitResult(S(review, "id"), Row{
		"status": "complete", "verdict": "pass", "summary": "looks good",
		"impact_enumeration":     passingImpactEnumeration(),
		"directive_dispositions": directiveDispositions(directives, "satisfied", ""),
	}, derivedKnobCandidates()...); err == nil || !strings.Contains(err.Error(), "--derive-test-knob") {
		t.Fatalf("submit seam with an unaccounted derived candidate must be rejected naming it: %v", err)
	}
	accounted := cloneRow(Row{"impact_enumeration": passingImpactEnumeration()})
	accounted["impact_enumeration"] = Row{"method": "literal-column grep set", "sites": []any{
		Row{"site": "internal/praector/config.go reads --derive-test-knob", "disposition": "affected", "evidence": "go test ./internal/praector/ -run TestDirective", "reader_paths": []any{"internal/praector/config.go"}},
	}}
	if _, err = s.SubmitResult(S(review, "id"), Row{
		"status": "complete", "verdict": "pass", "summary": "looks good",
		"impact_enumeration":     accounted["impact_enumeration"],
		"directive_dispositions": directiveDispositions(directives, "satisfied", ""),
	}, derivedKnobCandidates()...); err != nil {
		t.Fatalf("submit seam with the derived candidate accounted for must pass: %v", err)
	}
}

func TestLegacyMarkerMustBeCollectionScopedAndNameTheCollection(t *testing.T) {
	s, _ := newFakeStore(t)
	p, err := s.RegisterProject(testProjectConfig(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	c, coord, err := s.CreateCollectionWithCoordinator(S(p, "id"), "Scoped legacy", "Add authenticated endpoint with durable events", "Plan")
	if err != nil {
		t.Fatal(err)
	}
	critic, _, _ := s.CreateTask(CreateTaskOptions{
		ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "critic", Title: "Preflight",
		Objective: "preflight", Reason: "plan", ParentTaskID: S(coord, "id"), WorkspaceMode: "collection", Depth: 1, AllowDedupe: false,
	})
	_, _ = s.SubmitResult(S(critic, "id"), criticDirectiveResult())
	if err = s.SetTaskStatus(S(critic, "id"), TaskComplete, "critic complete"); err != nil {
		t.Fatal(err)
	}
	impl, _, _ := s.CreateTask(CreateTaskOptions{
		ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "implement", Title: "Implement",
		Objective: "implement", Reason: "plan", ParentTaskID: S(critic, "id"), DependsOn: []string{S(critic, "id")}, WorkspaceMode: "collection", Depth: 1, AllowDedupe: false,
	})
	review, _, _ := s.CreateTask(CreateTaskOptions{
		ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "review", Title: "Review",
		Objective: "review", Reason: "policy", ParentTaskID: S(impl, "id"), DependsOn: []string{S(impl, "id"), S(critic, "id")}, WorkspaceMode: "collection", Depth: 2, AllowDedupe: false,
	})
	_ = s.AddEdge(S(impl, "id"), S(review, "id"), S(c, "id"), "verifies")
	_ = s.SetTaskStatus(S(impl, "id"), TaskComplete, "complete")
	if err := s.Mutate("UPDATE tasks SET status='COMPLETE',result_json="+SQLLiteral(JSONDumps(Row{"status": "complete", "summary": "pass", "verdict": "pass"}))+" WHERE id="+SQLLiteral(S(review, "id")), "plant legacy review result"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SubmitResult(S(coord, "id"), Row{"status": "complete", "summary": "planned"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTaskStatus(S(coord, "id"), TaskComplete, "planning complete"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := validateCollectionForAcceptance(s, c); err == nil || !strings.Contains(err.Error(), "directive_dispositions") {
		t.Fatalf("undispositioned legacy review must block acceptance: %v", err)
	}
	if _, err := s.AddDecision(S(p, "id"), "", "", LegacyGateTraceabilityMarker+" pre-traceability review results accepted as-is for "+S(c, "id"), "project-wide marker", []any{}, []any{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := validateCollectionForAcceptance(s, c); err == nil || !strings.Contains(err.Error(), "directive_dispositions") {
		t.Fatalf("a project-scoped marker decision must NOT exempt a collection: %v", err)
	}
	if _, err := s.AddDecision(S(p, "id"), "COL-other", "", LegacyGateTraceabilityMarker+" pre-traceability review results accepted as-is for COL-other", "foreign collection marker", []any{}, []any{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := validateCollectionForAcceptance(s, c); err == nil || !strings.Contains(err.Error(), "directive_dispositions") {
		t.Fatalf("a marker decision scoped to a different collection must NOT exempt: %v", err)
	}
	if _, err := s.AddDecision(S(p, "id"), S(c, "id"), S(critic, "id"), LegacyGateTraceabilityMarker+" pre-traceability review results accepted as-is for "+S(c, "id"), "worker-created marker", []any{}, []any{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := validateCollectionForAcceptance(s, c); err == nil || !strings.Contains(err.Error(), "directive_dispositions") {
		t.Fatalf("a task-scoped (worker-created) marker decision must NOT arm the operator escape: %v", err)
	}
	if _, err := s.AddDecision(S(p, "id"), S(c, "id"), "", LegacyGateTraceabilityMarker+" pre-traceability review results accepted as-is without naming anything", "names no collection", []any{}, []any{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := validateCollectionForAcceptance(s, c); err == nil || !strings.Contains(err.Error(), "directive_dispositions") {
		t.Fatalf("a collection-scoped marker that does not name the collection must NOT exempt: %v", err)
	}
	if _, err := s.AddDecision(S(p, "id"), S(c, "id"), "", LegacyGateTraceabilityMarker+" pre-traceability review results accepted as-is for "+S(c, "id"), "properly scoped operator marker", []any{}, []any{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := validateCollectionForAcceptance(s, c); err != nil {
		t.Fatalf("a collection-scoped, task-NULL marker naming the collection must exempt acceptance: %v", err)
	}
}

func TestLiveDirectiveClaimGateBlocksImplementationClaim(t *testing.T) {
	s, _ := newFakeStore(t)
	p, err := s.RegisterProject(testProjectConfig(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	c, coord, err := s.CreateCollectionWithCoordinator(S(p, "id"), "Claim gate", "Add authenticated endpoint with durable events", "Plan")
	if err != nil {
		t.Fatal(err)
	}
	critic, _, _ := s.CreateTask(CreateTaskOptions{
		ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "critic", Title: "Preflight",
		Objective: "preflight", Reason: "plan", ParentTaskID: S(coord, "id"), WorkspaceMode: "collection", Depth: 1, AllowDedupe: false,
	})
	_, _ = s.SubmitResult(S(critic, "id"), criticDirectiveResult())
	if err = s.SetTaskStatus(S(critic, "id"), TaskComplete, "critic complete"); err != nil {
		t.Fatal(err)
	}
	planReview, _, err := s.CreateTask(CreateTaskOptions{
		ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "plan_review", Title: "Plan review",
		Objective: "review the plan", Reason: "policy", ParentTaskID: S(coord, "id"), DependsOn: []string{S(critic, "id")}, WorkspaceMode: "collection", Depth: 2, AllowDedupe: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	impl, _, err := s.CreateTask(CreateTaskOptions{
		ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "implement", Title: "Implement",
		Objective: "implement the authenticated endpoint", Reason: "plan", ParentTaskID: S(coord, "id"), DependsOn: []string{S(planReview, "id"), S(critic, "id")}, WorkspaceMode: "collection", Depth: 2, AllowDedupe: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The plan review is COMPLETE with verdict=pass but carries NO directive
	// dispositions. The LIVE claim-time directive gate must refuse the
	// implementation claim; removing the directiveTraceabilityGateLocked call
	// in planningGateAllowsClaimLocked makes this assertion fail.
	if err = s.Mutate("UPDATE tasks SET status='COMPLETE',result_json="+SQLLiteral(JSONDumps(Row{"status": "complete", "summary": "pass", "verdict": "pass"}))+",updated_at="+SQLLiteral(UTCNow())+" WHERE id="+SQLLiteral(S(planReview, "id")), "plant undispositioned plan review"); err != nil {
		t.Fatal(err)
	}
	claimed, _, err := s.ClaimTask(S(impl, "id"), 3)
	if err != nil {
		t.Fatal(err)
	}
	if claimed != nil && S(claimed, "id") == S(impl, "id") {
		t.Fatal("the live claim-time directive gate must block claiming an implement gated on an undispositioned plan review")
	}
	current, err := s.GetTask(S(impl, "id"))
	if err != nil {
		t.Fatal(err)
	}
	var ok bool
	if err = s.withLock(func() error {
		var e error
		ok, e = s.planningGateAllowsClaimLocked(current)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("the live claim-time directive gate must refuse the planning gate for an undispositioned plan review")
	}
}

func TestEvidenceContentFloorRejectsGenericProse(t *testing.T) {
	for _, weak := range []any{"verified in code", "n/a", "n/a.", []any{"verified in code"}, []any{"n/a"}} {
		if citableEvidence(weak) {
			t.Fatalf("generic prose evidence %v must be rejected by the content floor", weak)
		}
	}
	for _, strong := range []any{
		"inspected internal/praector/policy.go for the reader",
		"TestImpactEnumerationRequiredAndDeadKnobBlocking covers the failure path",
		"run go test ./internal/praector/ -run TestDirective -count=1",
		"run make test-go",
	} {
		if !citableEvidence(strong) {
			t.Fatalf("citable evidence form %v must be accepted", strong)
		}
	}
	directives := []InheritedDirective{{TaskID: "T-c", SourceProfile: "critic", Kind: "planner_directive", ID: "T-c:planner_directive:0", Text: "keep the seam strict"}}
	for _, weak := range []any{"verified in code", "n/a"} {
		result := Row{"verdict": "pass", "directive_dispositions": []any{Row{"directive_id": "T-c:planner_directive:0", "disposition": "satisfied", "evidence": weak}}}
		if err := ValidateDirectiveTraceability(result, directives, true, nil); err == nil || !strings.Contains(err.Error(), "citable evidence") {
			t.Fatalf("generic disposition evidence %v must be rejected: %v", weak, err)
		}
	}
	result := Row{"verdict": "pass", "directive_dispositions": []any{Row{"directive_id": "T-c:planner_directive:0", "disposition": "satisfied", "evidence": "inspected internal/praector/policy.go"}}}
	if err := ValidateDirectiveTraceability(result, directives, true, nil); err != nil {
		t.Fatalf("path-citing disposition evidence must be accepted: %v", err)
	}
	siteProse := Row{"verdict": "pass", "impact_enumeration": Row{"method": "grep", "sites": []any{Row{"site": "config.go", "disposition": "unaffected", "evidence": "verified in code"}}}}
	if err := ValidateReviewImpactEnumeration(siteProse, nil, nil); err == nil || !strings.Contains(err.Error(), "citable evidence") {
		t.Fatalf("generic impact-site evidence must be rejected: %v", err)
	}
	sitePath := Row{"verdict": "pass", "impact_enumeration": Row{"method": "grep", "sites": []any{Row{"site": "config.go", "disposition": "unaffected", "evidence": "grep -n MCPEnabled internal/praector/config.go shows the reader"}}}}
	if err := ValidateReviewImpactEnumeration(sitePath, nil, nil); err != nil {
		t.Fatalf("path-citing site evidence must be accepted: %v", err)
	}
}

func TestPlainStringMustDirectiveIsNonDeferrable(t *testing.T) {
	directives := Rows{{
		"id": "T-criticstr", "profile": "critic", "status": TaskComplete,
		"result_json": JSONDumps(Row{"planner_directives": []any{"MUST: keep the loopback bind gate strict.", "SHOULD: prefer narrow heuristics.", "An unlevelled suggestion."}}),
	}}
	harvested := InheritedDirectives(directives)
	if len(harvested) != 3 {
		t.Fatalf("expected three harvested directives, got %#v", harvested)
	}
	byID := map[string]InheritedDirective{}
	for _, directive := range harvested {
		byID[directive.ID] = directive
	}
	if byID["T-criticstr:planner_directive:0"].NormativeLevel != "MUST" {
		t.Fatalf("a plain-string `MUST: ...` critic directive must harvest as MUST-level: %#v", byID)
	}
	if byID["T-criticstr:planner_directive:1"].NormativeLevel != "SHOULD" {
		t.Fatalf("a plain-string `SHOULD: ...` critic directive must harvest as SHOULD-level: %#v", byID)
	}
	if byID["T-criticstr:planner_directive:2"].NormativeLevel != "" {
		t.Fatalf("an unlevelled string directive must stay unlevelled: %#v", byID)
	}
	if err := ValidateDirectiveTraceability(Row{"verdict": "pass", "directive_dispositions": stringDispositions(harvested, "deferred", "DEC-x")}, harvested, true, nil); err == nil || !strings.Contains(err.Error(), "MUST-level") {
		t.Fatalf("a MUST string directive must not be deferrable: %v", err)
	}
	if err := ValidateDirectiveTraceability(Row{"verdict": "pass", "directive_dispositions": stringDispositions(harvested, "satisfied", "")}, harvested, true, nil); err != nil {
		t.Fatalf("a MUST string directive must be satisfiable: %v", err)
	}
	if err := ValidateDirectiveTraceability(Row{"verdict": "pass", "directive_dispositions": stringDispositions(harvested, "explicit_exception", "DEC-x")}, harvested, true, nil); err != nil {
		t.Fatalf("a MUST string directive must accept explicit_exception with a decision id: %v", err)
	}
	mustOnly := []InheritedDirective{byID["T-criticstr:planner_directive:0"]}
	if err := ValidateDirectiveTraceability(Row{"verdict": "pass"}, mustOnly, true, nil); err == nil || !strings.Contains(err.Error(), "did not disposition") {
		t.Fatalf("unknown/missing directive rejection must stay intact: %v", err)
	}
	if err := ValidateDirectiveTraceability(Row{"verdict": "pass"}, nil, true, nil); err != nil {
		t.Fatalf("the zero-directive clean pass must stay intact: %v", err)
	}
}

func stringDispositions(directives []InheritedDirective, disposition, decisionID string) []any {
	out := []any{}
	for _, directive := range directives {
		item := Row{"directive_id": directive.ID, "disposition": disposition, "evidence": []any{"inspected internal/praector/policy.go"}}
		if disposition != "satisfied" && decisionID != "" {
			item["decision_id"] = decisionID
		}
		out = append(out, item)
	}
	return out
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
