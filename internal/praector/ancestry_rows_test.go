package praector

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Phase 3.5: the acceptance predicate's directive/research ancestry walk reads
// from already-loaded rows instead of one `dolt sql` subprocess per BFS node.
// The dangerous surface is that ACCEPTANCE shares this predicate with the
// non-blocking readiness sweep, so the deliverable is parity: the rows path must
// return the identical first-failure message the pre-Phase-3.5 SQL path did, for
// every would-block reason, and the sweep must collapse to one batched read.
//
// The parity oracle is `referenceAcceptanceBlockReasonSQL` below: a verbatim copy
// of the predicate body as it existed before this change (one read per decision
// plus one JOIN read per BFS frontier node), driven through the same durable
// store. Every seeded case asserts exact-message equality across THREE paths:
// the reference SQL predicate, the delegating `acceptanceBlockReason` wrapper the
// accept/PR path calls, and the `acceptanceBlockReasonFromRows` entry the sweep
// calls.

const (
	parityT0 = "2026-01-01 00:00:00.000000"
	parityT1 = "2026-01-01 00:00:01.000000"
	parityT2 = "2026-01-01 00:00:02.000000"
	parityT3 = "2026-01-01 00:00:03.000000"
	parityT4 = "2026-01-01 00:00:04.000000"
)

const (
	parityProject = "PRJ-par"
	parityCID     = "COL-par"
	// parityGateFreeObjective classifies with every planning gate false, so the
	// critic-staleness branch stays inert in the non-stale cases.
	parityGateFreeObjective = "Analyze and deep dive, see what we would need to change to have a CloudFormation parameter."
	// parityCriticObjective classifies with critic_required true, which is what
	// the stale-critic-seal would-block reason needs.
	parityCriticObjective = "Add API-key authentication and per-key rate limiting to the public API endpoint."
)

// parityReviewPassResult is a genuinely acceptable passing review result: a
// valid impact enumeration whose site evidence is citable, with no candidate
// dispositions.
func parityReviewPassResult() string {
	return JSONDumps(Row{
		"status": "complete", "summary": "reviewed", "verdict": "pass",
		"impact_enumeration": passingImpactEnumeration(),
	})
}

// parityStore seeds one COMPLETE collection plus any extra raw statements and
// returns the store and the freshly-read collection row. Everything is planted
// through raw SQL so ids and created_at values are deterministic.
func parityStore(t *testing.T, objective string, extra ...string) (*Store, Row) {
	t.Helper()
	s, _ := newFakeStore(t)
	stmts := []string{
		"INSERT INTO projects (id,name,root_path,base_branch,config_json,created_at,updated_at) VALUES (" + SQLLiteral(parityProject) + ",'par','/par','main','{}'," + SQLLiteral(parityT0) + "," + SQLLiteral(parityT0) + ")",
		"INSERT INTO collections (id,project_id,title,objective,status,created_at,updated_at) VALUES (" + SQLLiteral(parityCID) + ",'" + parityProject + "','Parity'," + SQLLiteral(objective) + ",'COMPLETE'," + SQLLiteral(parityT0) + "," + SQLLiteral(parityT0) + ")",
		"INSERT INTO tasks (id,project_id,collection_id,profile,title,objective,status,created_at,updated_at) VALUES ('T-coord','" + parityProject + "','" + parityCID + "','coordinator','c','o','COMPLETE'," + SQLLiteral(parityT0) + "," + SQLLiteral(parityT0) + ")",
		"INSERT INTO tasks (id,project_id,collection_id,profile,title,objective,status,created_at,updated_at) VALUES ('T-impl','" + parityProject + "','" + parityCID + "','implement','i','o','COMPLETE'," + SQLLiteral(parityT1) + "," + SQLLiteral(parityT1) + ")",
	}
	stmts = append(stmts, extra...)
	if err := s.Mutate(strings.Join(stmts, ";"), "seed parity fixture"); err != nil {
		t.Fatal(err)
	}
	c, err := s.GetCollection(parityCID)
	if err != nil {
		t.Fatal(err)
	}
	return s, c
}

// reviewTaskSQL plants one COMPLETE review task with an explicit result_json and
// an optional verifies edge from the implement.
func reviewTaskSQL(id, resultJSON string, verifies bool) []string {
	out := []string{"INSERT INTO tasks (id,project_id,collection_id,parent_task_id,profile,title,objective,status,result_json,created_at,updated_at) VALUES (" + SQLLiteral(id) + ",'" + parityProject + "','" + parityCID + "','T-coord','review','r','o','COMPLETE'," + SQLLiteral(resultJSON) + "," + SQLLiteral(parityT3) + "," + SQLLiteral(parityT3) + ")"}
	if verifies {
		out = append(out, "INSERT INTO task_edges (from_task_id,to_task_id,collection_id,relation,created_at) VALUES ('T-impl',"+SQLLiteral(id)+",'"+parityCID+"','verifies',"+SQLLiteral(parityT3)+")")
	}
	return out
}

// criticWithDirectivesSQL plants a COMPLETE critic whose result carries harvestable
// planner directives, wired as a depends_on prerequisite of the given task.
func criticWithDirectivesSQL(dependent string) []string {
	criticResult := JSONDumps(Row{
		"status": "complete", "summary": "preflight complete",
		"planner_directives": []any{Row{"text": "MUST enumerate every events-table writer before finalizing the schema change.", "normative_level": "MUST"}},
	})
	return []string{
		"INSERT INTO tasks (id,project_id,collection_id,parent_task_id,profile,title,objective,status,result_json,created_at,updated_at) VALUES ('T-critic','" + parityProject + "','" + parityCID + "','T-coord','critic','k','o','COMPLETE'," + SQLLiteral(criticResult) + "," + SQLLiteral(parityT1) + "," + SQLLiteral(parityT1) + ")",
		"INSERT INTO task_edges (from_task_id,to_task_id,collection_id,relation,created_at) VALUES ('T-critic'," + SQLLiteral(dependent) + ",'" + parityCID + "','depends_on'," + SQLLiteral(parityT2) + ")",
	}
}

// referenceDirectiveAncestorsSQL is the pre-Phase-3.5 directive walk verbatim: one
// JOIN read per visited frontier node, with no collection predicate at all.
func referenceDirectiveAncestorsSQL(t *testing.T, s *Store, tid string) Rows {
	t.Helper()
	seen := map[string]bool{tid: true}
	queue := []string{tid}
	byID := map[string]Row{}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		deps, err := s.Query("SELECT t.* FROM task_edges e JOIN tasks t ON t.id=e.from_task_id WHERE e.to_task_id=" + SQLLiteral(cur) + " AND e.relation='depends_on' ORDER BY e.created_at")
		if err != nil {
			t.Fatal(err)
		}
		for _, dep := range deps {
			id := S(dep, "id")
			if id == "" {
				continue
			}
			if directiveSourceProfiles[S(dep, "profile")] && S(dep, "status") == TaskComplete {
				dep["result"] = JSONMap(S(dep, "result_json"))
				byID[id] = dep
			}
			if !seen[id] {
				seen[id] = true
				queue = append(queue, id)
			}
		}
	}
	out := make(Rows, 0, len(byID))
	for _, row := range byID {
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := S(out[i], "created_at"), S(out[j], "created_at")
		if a == b {
			return S(out[i], "id") < S(out[j], "id")
		}
		return a < b
	})
	return out
}

// referenceAcceptanceBlockReasonSQL is the acceptance predicate exactly as it was
// before Phase 3.5 (same first-failure order, same message text), with the
// per-BFS-node walk replaced by nothing: it IS the per-BFS-node walk. It exists
// only as the parity oracle for the rows path.
func referenceAcceptanceBlockReasonSQL(t *testing.T, s *Store, c Row, state string, recordEmptyHarvest bool) string {
	t.Helper()
	critic, _ := s.CriticRevalidationState(S(c, "id"))
	policy := effectivePlanningPolicy(c)
	if state != TaskComplete && state != "COMPLETE" {
		if policy.CriticRequired && B(critic, "stale") {
			return fmt.Sprintf("Collection %s has a stale required critic: sealed material-plan fingerprint %s != current %s. Re-run orchestration to complete critic revalidation before acceptance.", S(c, "id"), S(critic, "sealed_fingerprint"), S(critic, "current_material_plan_fingerprint"))
		}
		return fmt.Sprintf("Collection %s is %s, not COMPLETE", S(c, "id"), state)
	}
	if policy.CriticRequired && B(critic, "stale") {
		return fmt.Sprintf("Collection %s has a stale required critic; acceptance requires a fresh critic seal.", S(c, "id"))
	}
	tasks, err := s.ListTasks(S(c, "id"))
	if err != nil {
		t.Fatal(err)
	}
	edges, err := s.ListEdges(S(c, "id"))
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]Row{}
	for _, task := range tasks {
		by[S(task, "id")] = task
		if S(task, "profile") == "review" && S(task, "status") == TaskComplete {
			v := strings.ToLower(strings.TrimSpace(S(M(task, "result"), "verdict")))
			if v != "pass" && v != "fail" {
				if v == "" {
					v = "<missing>"
				}
				return fmt.Sprintf("Review %s is COMPLETE but has verdict=%s; acceptance requires an explicit pass/fail verdict", S(task, "id"), v)
			}
		}
	}
	verifies := map[string][]string{}
	for _, ed := range edges {
		if S(ed, "relation") == "verifies" {
			verifies[S(ed, "from_task_id")] = append(verifies[S(ed, "from_task_id")], S(ed, "to_task_id"))
		}
	}
	legacyDecisions, err := s.ListDecisions(S(c, "project_id"), S(c, "id"))
	if err != nil {
		t.Fatal(err)
	}
	legacyIDs := LegacyGateTraceabilityExceptionIDs(legacyDecisions, S(c, "id"))
	harvestedConsumers := map[string]bool{}
	harvestEvents, evErr := s.EventsForCollection(S(c, "id"), 200)
	if evErr == nil {
		for _, ev := range harvestEvents {
			if S(ev, "event_type") == "DIRECTIVE_TRACEABILITY_EMPTY_HARVEST" {
				harvestedConsumers[S(ev, "task_id")] = true
			}
		}
	}
	governingReview := Row{}
	for _, task := range tasks {
		if S(task, "profile") != "review" || S(task, "status") != TaskComplete || strings.ToLower(strings.TrimSpace(S(M(task, "result"), "verdict"))) != "pass" {
			continue
		}
		if len(governingReview) == 0 || S(task, "created_at")+S(task, "id") >= S(governingReview, "created_at")+S(governingReview, "id") {
			governingReview = task
		}
	}
	for _, task := range tasks {
		if S(task, "profile") != "review" && S(task, "profile") != "plan_review" {
			continue
		}
		if S(task, "status") != TaskComplete || strings.ToLower(strings.TrimSpace(S(M(task, "result"), "verdict"))) != "pass" {
			continue
		}
		ancestors := referenceDirectiveAncestorsSQL(t, s, S(task, "id"))
		if directives := InheritedDirectives(ancestors); len(directives) > 0 {
			if e := ValidateDirectiveTraceability(M(task, "result"), directives, true, legacyIDs); e != nil {
				return fmt.Sprintf("Collection %s cannot be accepted: %s", S(c, "id"), e)
			}
		} else if len(ancestors) > 0 && !harvestedConsumers[S(task, "id")] {
			if recordEmptyHarvest {
				ids := []string{}
				for _, ancestor := range ancestors {
					ids = append(ids, S(ancestor, "id"))
				}
				_ = s.RecordEmptyDirectiveHarvest(S(c, "project_id"), S(c, "id"), S(task, "id"), ids)
			}
			harvestedConsumers[S(task, "id")] = true
		}
		if S(task, "profile") == "review" {
			candidates := []DerivedConfigCandidate(nil)
			if S(task, "id") != S(governingReview, "id") {
				candidates = nil
			}
			if e := ValidateReviewImpactEnumeration(M(task, "result"), legacyIDs, candidates); e != nil {
				return fmt.Sprintf("Collection %s cannot be accepted: %s", S(c, "id"), e)
			}
			if decisionIDs := CandidateDecisionIDs(M(task, "result")); len(decisionIDs) > 0 {
				known := map[string]bool{}
				for _, decision := range legacyDecisions {
					known[S(decision, "id")] = true
				}
				for _, decisionID := range decisionIDs {
					if !known[decisionID] {
						return fmt.Sprintf("Collection %s cannot be accepted: candidate disposition decision_id %s is not a durable decision in this Collection", S(c, "id"), decisionID)
					}
				}
			}
		}
	}
	for _, task := range tasks {
		if S(task, "profile") != "implement" || S(task, "status") != TaskComplete {
			continue
		}
		cand := append([]string{}, verifies[S(task, "id")]...)
		pid := S(task, "parent_task_id")
		if pid != "" && S(by[pid], "profile") == "review" && strings.ToLower(strings.TrimSpace(S(M(by[pid], "result"), "verdict"))) == "pass" {
			cand = append(cand, pid)
		}
		passed := false
		for _, rid := range cand {
			r := by[rid]
			if r != nil && S(r, "status") == TaskComplete && strings.ToLower(S(M(r, "result"), "verdict")) == "pass" {
				passed = true
				break
			}
		}
		if !passed {
			return fmt.Sprintf("Implementation %s has no completed independent review with verdict=pass", S(task, "id"))
		}
	}
	return ""
}

// parityError runs one seeded fixture through all three paths and returns the
// three messages.
func parityError(t *testing.T, s *Store, c Row, state string) (ref, wrap, rows string) {
	t.Helper()
	ref = referenceAcceptanceBlockReasonSQL(t, s, c, state, false)
	if _, e := acceptanceBlockReason(s, c, state, false); e != nil {
		wrap = e.Error()
	}
	snaps, err := s.loadCollectionRowsWithAcceptance([]string{S(c, "id")})
	if err != nil {
		t.Fatal(err)
	}
	if _, e := acceptanceBlockReasonFromRows(s, c, state, false, snaps[S(c, "id")]); e != nil {
		rows = e.Error()
	}
	return
}

func assertParity(t *testing.T, name, ref, wrap, rows, want string) {
	t.Helper()
	if ref != want || wrap != want || rows != want {
		t.Fatalf("%s: message parity broken\n reference sql: %q\n cid wrapper: %q\n rows path:     %q\n want:          %q", name, ref, wrap, rows, want)
	}
}

// TestAcceptanceBlockReasonRowsVerdictParity is the deliverable of Phase 3.5: for
// every would-block reason the rows path returns the IDENTICAL first-failure
// message string the pre-Phase-3.5 SQL path returns — exact-message equality, not
// a boolean.
func TestAcceptanceBlockReasonRowsVerdictParity(t *testing.T) {
	t.Run("stale critic seal on a COMPLETE collection", func(t *testing.T) {
		s, c := parityStore(t, parityCriticObjective,
			append(reviewTaskSQL("T-rev", parityReviewPassResult(), true),
				"INSERT INTO tasks (id,project_id,collection_id,parent_task_id,profile,title,objective,status,created_at,updated_at) VALUES ('T-critic','"+parityProject+"','"+parityCID+"','T-coord','critic','k','o','COMPLETE',"+SQLLiteral(parityT1)+","+SQLLiteral(parityT1)+")",
				"INSERT INTO events (id,project_id,collection_id,task_id,attempt_id,event_type,payload_json,created_at) VALUES ('EVT-seal','"+parityProject+"','"+parityCID+"','T-critic',NULL,'CRITIC_PLAN_BASELINE',"+SQLLiteral(`{"source":"initial_plan_seal","fingerprint":"SEALED-STALE"}`)+","+SQLLiteral(parityT2)+")")...)
		ref, wrap, rows := parityError(t, s, c, "COMPLETE")
		assertParity(t, "stale critic seal (COMPLETE)", ref, wrap, rows, "Collection "+parityCID+" has a stale required critic; acceptance requires a fresh critic seal.")
	})

	t.Run("stale critic seal on a non-COMPLETE collection", func(t *testing.T) {
		s, c := parityStore(t, parityCriticObjective,
			append(reviewTaskSQL("T-rev", parityReviewPassResult(), true),
				"INSERT INTO tasks (id,project_id,collection_id,parent_task_id,profile,title,objective,status,created_at,updated_at) VALUES ('T-critic','"+parityProject+"','"+parityCID+"','T-coord','critic','k','o','COMPLETE',"+SQLLiteral(parityT1)+","+SQLLiteral(parityT1)+")",
				"INSERT INTO events (id,project_id,collection_id,task_id,attempt_id,event_type,payload_json,created_at) VALUES ('EVT-seal','"+parityProject+"','"+parityCID+"','T-critic',NULL,'CRITIC_PLAN_BASELINE',"+SQLLiteral(`{"source":"initial_plan_seal","fingerprint":"SEALED-STALE"}`)+","+SQLLiteral(parityT2)+")")...)
		// The two-fingerprint message is computed, not constant: the assertion is
		// that all three paths agree byte-for-byte on it.
		ref, wrap, rows := parityError(t, s, c, "BLOCKED")
		assertParity(t, "stale critic seal (BLOCKED)", ref, wrap, rows, ref)
		if !strings.HasPrefix(ref, "Collection "+parityCID+" has a stale required critic: sealed material-plan fingerprint ") || !strings.Contains(ref, "Re-run orchestration to complete critic revalidation before acceptance.") {
			t.Fatalf("unexpected first-failure text for the non-COMPLETE stale-seal branch: %q", ref)
		}
	})

	t.Run("missing review verdict", func(t *testing.T) {
		s, c := parityStore(t, parityGateFreeObjective, reviewTaskSQL("T-rev", JSONDumps(Row{"status": "complete", "summary": "reviewed"}), true)...)
		ref, wrap, rows := parityError(t, s, c, "COMPLETE")
		assertParity(t, "missing verdict", ref, wrap, rows, "Review T-rev is COMPLETE but has verdict=<missing>; acceptance requires an explicit pass/fail verdict")
	})

	t.Run("uncited inherited directive", func(t *testing.T) {
		s, c := parityStore(t, parityGateFreeObjective, append(criticWithDirectivesSQL("T-rev"), reviewTaskSQL("T-rev", parityReviewPassResult(), true)...)...)
		ref, wrap, rows := parityError(t, s, c, "COMPLETE")
		assertParity(t, "uncited directive", ref, wrap, rows, ref)
		if !strings.Contains(ref, "T-critic:planner_directive:0") {
			t.Fatalf("the uncited-directive message must name the inherited directive id; got %q", ref)
		}
	})

	t.Run("missing review impact enumeration", func(t *testing.T) {
		noEnum := JSONDumps(Row{"status": "complete", "summary": "reviewed", "verdict": "pass"})
		s, c := parityStore(t, parityGateFreeObjective, reviewTaskSQL("T-rev", noEnum, true)...)
		ref, wrap, rows := parityError(t, s, c, "COMPLETE")
		assertParity(t, "missing impact enumeration", ref, wrap, rows, ref)
		if !strings.Contains(ref, "impact_enumeration") {
			t.Fatalf("the missing-enumeration message must name impact_enumeration; got %q", ref)
		}
	})

	t.Run("unknown candidate disposition", func(t *testing.T) {
		result := JSONDumps(Row{
			"status": "complete", "summary": "reviewed", "verdict": "pass",
			"impact_enumeration": Row{
				"method": "call-graph walk",
				"sites": []any{Row{
					"site": "internal/praector/store.go", "disposition": "unaffected",
					"evidence": "go test ./internal/praector/ -run TestAncestry",
				}},
				"candidate_dispositions": []any{Row{"knob": "--new-flag", "path": "internal/praector/cli.go", "decision_id": "DEC-not-durable", "disposition": "rejected", "evidence": []any{"go test ./internal/praector/ -run TestAncestry"}}},
			},
		})
		s, c := parityStore(t, parityGateFreeObjective, reviewTaskSQL("T-rev", result, true)...)
		ref, wrap, rows := parityError(t, s, c, "COMPLETE")
		assertParity(t, "unknown candidate disposition", ref, wrap, rows, "Collection "+parityCID+" cannot be accepted: candidate disposition decision_id DEC-not-durable is not a durable decision in this Collection")
	})

	t.Run("uncovered implementation", func(t *testing.T) {
		s, c := parityStore(t, parityGateFreeObjective, reviewTaskSQL("T-rev", parityReviewPassResult(), false)...)
		ref, wrap, rows := parityError(t, s, c, "COMPLETE")
		assertParity(t, "uncovered implementation", ref, wrap, rows, "Implementation T-impl has no completed independent review with verdict=pass")
	})
}

// TestEmptyDirectiveHarvestStillReachableFromAcceptPath proves the one durable
// write the predicate can make (RecordEmptyDirectiveHarvest) stays reachable
// through the delegating cid wrapper: a directive-bearing ancestor that yields
// zero harvestable directives is still audited on the accept path and still
// silent on the sweep path.
func TestEmptyDirectiveHarvestStillReachableFromAcceptPath(t *testing.T) {
	quietCritic := JSONDumps(Row{"status": "complete", "summary": "preflight complete"})
	s, c := parityStore(t, parityGateFreeObjective,
		"INSERT INTO tasks (id,project_id,collection_id,parent_task_id,profile,title,objective,status,result_json,created_at,updated_at) VALUES ('T-critic','"+parityProject+"','"+parityCID+"','T-coord','critic','k','o','COMPLETE',"+SQLLiteral(quietCritic)+","+SQLLiteral(parityT1)+","+SQLLiteral(parityT1)+")",
		"INSERT INTO task_edges (from_task_id,to_task_id,collection_id,relation,created_at) VALUES ('T-critic','T-rev','"+parityCID+"','depends_on',"+SQLLiteral(parityT2)+")",
	)
	s.Mutate(strings.Join(reviewTaskSQL("T-rev", parityReviewPassResult(), true), ";"), "seed review")
	if got := collectionEventsFor(t, s, c, "DIRECTIVE_TRACEABILITY_EMPTY_HARVEST"); len(got) != 0 {
		t.Fatalf("the sweep path must not have pre-recorded a harvest marker, got %d", len(got))
	}
	// Sweep path (recordEmptyHarvest=false): purely read-only.
	if _, e := acceptanceBlockReason(s, c, "COMPLETE", false); e != nil {
		t.Fatalf("the seeded collection must be otherwise acceptable; got %v", e)
	}
	if got := collectionEventsFor(t, s, c, "DIRECTIVE_TRACEABILITY_EMPTY_HARVEST"); len(got) != 0 {
		t.Fatalf("the read-only predicate must emit no harvest event, got %d", len(got))
	}
	// Accept path (recordEmptyHarvest=true): the audit write is still made.
	if _, e := acceptanceBlockReason(s, c, "COMPLETE", true); e != nil {
		t.Fatalf("the accept path must still accept this graph; got %v", e)
	}
	harvest := collectionEventsFor(t, s, c, "DIRECTIVE_TRACEABILITY_EMPTY_HARVEST")
	if len(harvest) != 1 {
		t.Fatalf("the accept path must still record the empty harvest, got %d", len(harvest))
	}
	if !strings.Contains(JSONDumps(JSONMap(S(harvest[0], "payload_json"))), "T-critic") {
		t.Fatalf("the harvest event must name the checked ancestor; got %s", S(harvest[0], "payload_json"))
	}
}

// TestAncestryWalkRowsPathMatchesSQLPath proves the walk itself (not just the
// predicate) agrees with the removed per-node SQL traversal on a multi-hop,
// diamond-shaped lineage, and that the pure core issues ZERO reads once rows are
// supplied.
func TestAncestryWalkRowsPathMatchesSQLPath(t *testing.T) {
	researchResult := JSONDumps(Row{"status": "complete", "summary": "researched", "sources": []any{}, "claims": []any{}})
	criticResult := JSONDumps(Row{
		"status": "complete", "summary": "preflight",
		"planner_directives": []any{Row{"text": "MUST verify the parity matrix.", "normative_level": "MUST"}},
	})
	s, _ := parityStore(t, parityGateFreeObjective,
		"INSERT INTO tasks (id,project_id,collection_id,parent_task_id,profile,title,objective,status,result_json,created_at,updated_at) VALUES ('T-research','"+parityProject+"','"+parityCID+"','T-coord','research','q','o','COMPLETE',"+SQLLiteral(researchResult)+","+SQLLiteral(parityT1)+","+SQLLiteral(parityT1)+")",
		"INSERT INTO tasks (id,project_id,collection_id,parent_task_id,profile,title,objective,status,result_json,created_at,updated_at) VALUES ('T-critic','"+parityProject+"','"+parityCID+"','T-coord','critic','k','o','COMPLETE',"+SQLLiteral(criticResult)+","+SQLLiteral(parityT1)+","+SQLLiteral(parityT1)+")",
		"INSERT INTO tasks (id,project_id,collection_id,parent_task_id,profile,title,objective,status,created_at,updated_at) VALUES ('T-adviser','"+parityProject+"','"+parityCID+"','T-coord','advise','a','o','COMPLETE',"+SQLLiteral(parityT2)+","+SQLLiteral(parityT2)+")",
		"INSERT INTO tasks (id,project_id,collection_id,parent_task_id,profile,title,objective,status,created_at,updated_at) VALUES ('T-draft','"+parityProject+"','"+parityCID+"','T-coord','review','d','o','RUNNING',"+SQLLiteral(parityT2)+","+SQLLiteral(parityT2)+")",
		"INSERT INTO task_edges (from_task_id,to_task_id,collection_id,relation,created_at) VALUES ('T-research','T-critic','"+parityCID+"','depends_on',"+SQLLiteral(parityT2)+")",
		"INSERT INTO task_edges (from_task_id,to_task_id,collection_id,relation,created_at) VALUES ('T-adviser','T-critic','"+parityCID+"','depends_on',"+SQLLiteral(parityT2)+")",
		"INSERT INTO task_edges (from_task_id,to_task_id,collection_id,relation,created_at) VALUES ('T-critic','T-draft','"+parityCID+"','depends_on',"+SQLLiteral(parityT3)+")",
		"INSERT INTO task_edges (from_task_id,to_task_id,collection_id,relation,created_at) VALUES ('T-adviser','T-draft','"+parityCID+"','depends_on',"+SQLLiteral(parityT3)+")",
	)
	wantDirective := []string{"T-critic", "T-adviser"}
	wantResearch := []string{"T-research"}

	ids := func(rs Rows) []string {
		out := []string{}
		for _, r := range rs {
			out = append(out, S(r, "id"))
		}
		return out
	}
	assertSame := func(label string, got, want []string) {
		t.Helper()
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("%s: got %v, want %v", label, got, want)
		}
	}

	// Public delegators still agree with the reference SQL walk.
	referenceDirective := ids(referenceDirectiveAncestorsSQL(t, s, "T-draft"))
	assertSame("reference directive walk", referenceDirective, wantDirective)
	liveDirective, err := s.DirectiveAncestors("T-draft")
	if err != nil {
		t.Fatal(err)
	}
	assertSame("delegating directive walk", ids(liveDirective), wantDirective)
	liveResearch, err := s.ResearchAncestors("T-draft")
	if err != nil {
		t.Fatal(err)
	}
	assertSame("delegating research walk", ids(liveResearch), wantResearch)

	// Locked twins agree with the public ones.
	var lockedDirective, lockedResearch Rows
	if err = s.withLock(func() error {
		var e error
		lockedDirective, e = s.directiveAncestorsLocked("T-draft")
		if e != nil {
			return e
		}
		lockedResearch, e = s.researchAncestorsLocked("T-draft")
		return e
	}); err != nil {
		t.Fatal(err)
	}
	assertSame("locked directive walk", ids(lockedDirective), wantDirective)
	assertSame("locked research walk", ids(lockedResearch), wantResearch)

	// The pure core over already-loaded rows produces the same membership, and
	// issues ZERO dolt reads to do it.
	logPath := filepath.Join(t.TempDir(), "sqlops.log")
	t.Setenv("PRAECTOR_FAKE_DOLT_LOG", logPath)
	tasks, edges, err := s.loadAncestryRows("T-draft")
	if err != nil {
		t.Fatal(err)
	}
	resetSQLLog(t, logPath)
	assertSame("pure directive walk", ids(ancestorWalkFromRows(tasks, edges, "T-draft", ancestorProfileIsDirectiveSource)), wantDirective)
	assertSame("pure research walk", ids(ancestorWalkFromRows(tasks, edges, "T-draft", ancestorProfileIsResearch)), wantResearch)
	assertSame("pure frontier", ids(dependsOnFrontierFromRows(tasks, edges, "T-draft")), []string{"T-adviser", "T-critic"})
	if n := countSQLOperations(t, logPath); n != 0 {
		t.Fatalf("the pure walk over supplied rows must issue zero dolt sql reads, got %d", n)
	}
}

// TestAncestryWalkCrossCollectionEdgeIsWriterInvariant proves the ONE semantic
// assumption the rows path makes that the old collection-blind SQL did not:
// membership is scoped to the start task's collection. Part (a) proves no
// production seam can create such an edge; part (b) pins the documented rows-path
// membership for a raw-SQL-injected malformed edge.
func TestAncestryWalkCrossCollectionEdgeIsWriterInvariant(t *testing.T) {
	// (a) Writer invariant: every seam that can create a depends_on edge rejects
	// a cross-collection task pair.
	s, _ := newFakeStore(t)
	p, err := s.RegisterProject(testProjectConfig(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	c1, coord1, err := s.CreateCollectionWithCoordinator(S(p, "id"), "One", parityGateFreeObjective, "plan")
	if err != nil {
		t.Fatal(err)
	}
	c2, coord2, err := s.CreateCollectionWithCoordinator(S(p, "id"), "Two", parityGateFreeObjective, "plan")
	if err != nil {
		t.Fatal(err)
	}
	t1, _, err := s.CreateTask(CreateTaskOptions{ProjectID: S(p, "id"), CollectionID: S(c1, "id"), Profile: "critic", Title: "Critic", Objective: "preflight", Reason: "plan", ParentTaskID: S(coord1, "id"), WorkspaceMode: "collection", Depth: 1, AllowDedupe: false})
	if err != nil {
		t.Fatal(err)
	}
	t2, _, err := s.CreateTask(CreateTaskOptions{ProjectID: S(p, "id"), CollectionID: S(c2, "id"), Profile: "implement", Title: "Implement", Objective: "implement", Reason: "plan", ParentTaskID: S(coord2, "id"), WorkspaceMode: "collection", Depth: 1, AllowDedupe: false})
	if err != nil {
		t.Fatal(err)
	}
	if e := s.AddDependency(S(t2, "id"), S(t1, "id")); e == nil {
		t.Fatal("AddDependency must reject a cross-collection depends_on pair")
	}
	if e := s.RemoveDependency(S(t2, "id"), S(t1, "id")); e == nil {
		t.Fatal("RemoveDependency must reject a cross-collection depends_on pair")
	}
	// AddEdge is the low-level seam, and it still refuses because the edge's own
	// collection_id cannot contain both tasks.
	if e := s.AddEdge(S(t1, "id"), S(t2, "id"), S(c2, "id"), "depends_on"); e == nil {
		t.Fatal("AddEdge must reject a task pair outside the edge's collection")
	}
	if edges, e := s.ListEdges(S(c2, "id")); e != nil || len(edges) != 0 {
		t.Fatalf("no cross-collection depends_on edge may exist; got %d edges (%v)", len(edges), e)
	}
	// The three writers that DO create depends_on edges derive collection_id
	// exclusively from the task's own collection: the initial-edge batch uses
	// o.CollectionID of the task being created (store.go:1135), AddEdge above, and
	// the review/plan-review wiring passes the owning cid (store.go:1759-1764).
	// There is therefore no seam that can produce the malformed row part (b)
	// injects by raw SQL.

	// (b) Seeded malformed edge: an edge whose collection_id differs from its
	// task pair. The documented rows-path membership is collection-scoped: the
	// walk from a task only ever traverses the depends_on edges of that task's
	// own collection.
	quiet := JSONDumps(Row{"status": "complete", "summary": "preflight complete"})
	s2, _ := newFakeStore(t)
	stmts := []string{
		"INSERT INTO projects (id,name,root_path,base_branch,config_json,created_at,updated_at) VALUES ('PRJ-x','x','/x','main','{}'," + SQLLiteral(parityT0) + "," + SQLLiteral(parityT0) + ")",
		"INSERT INTO collections (id,project_id,title,objective,status,created_at,updated_at) VALUES ('COL-home','PRJ-x','Home'," + SQLLiteral(parityGateFreeObjective) + ",'COMPLETE'," + SQLLiteral(parityT0) + "," + SQLLiteral(parityT0) + ")",
		"INSERT INTO collections (id,project_id,title,objective,status,created_at,updated_at) VALUES ('COL-foreign','PRJ-x','Foreign'," + SQLLiteral(parityGateFreeObjective) + ",'COMPLETE'," + SQLLiteral(parityT0) + "," + SQLLiteral(parityT0) + ")",
		"INSERT INTO tasks (id,project_id,collection_id,profile,title,objective,status,created_at,updated_at) VALUES ('T-x-rev','PRJ-x','COL-home','review','r','o','COMPLETE'," + SQLLiteral(parityT1) + "," + SQLLiteral(parityT1) + ")",
		"INSERT INTO tasks (id,project_id,collection_id,profile,title,objective,status,result_json,created_at,updated_at) VALUES ('T-x-critic','PRJ-x','COL-foreign','critic','k','o','COMPLETE'," + SQLLiteral(quiet) + "," + SQLLiteral(parityT0) + "," + SQLLiteral(parityT0) + ")",
		// The malformed row: collection_id belongs to neither task's collection.
		"INSERT INTO task_edges (from_task_id,to_task_id,collection_id,relation,created_at) VALUES ('T-x-critic','T-x-rev','COL-stray','depends_on'," + SQLLiteral(parityT2) + ")",
	}
	if e := s2.Mutate(strings.Join(stmts, ";"), "seed malformed cross-collection edge"); e != nil {
		t.Fatal(e)
	}
	live, err := s2.DirectiveAncestors("T-x-rev")
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 0 {
		t.Fatalf("documented rows-path membership is collection-scoped; a stray-collection edge must not extend the walk, got %v", live)
	}
	// The removed SQL path had NO collection predicate at all, so the malformed
	// row WAS the one input on which the two shapes could disagree. Pinning that
	// the reference traversal does see it documents exactly what changed and
	// why it is safe (part (a): no seam can create the row).
	seenReference := referenceDirectiveAncestorsSQL(t, s2, "T-x-rev")
	if len(seenReference) != 1 || S(seenReference[0], "id") != "T-x-critic" {
		t.Fatalf("the reference collection-blind walk must have traversed the stray edge (that is the divergence being pinned), got %v", seenReference)
	}
}

// TestAcceptanceSnapshotEventsWindowIsNotWidened seeds more than 200 events and
// proves the snapshot's events bucket is the SAME window the standalone
// EventsForCollection(cid, 200) read returns: the harvest-marker window was
// folded into the batch, never widened into an event_type-filtered IN-list.
func TestAcceptanceSnapshotEventsWindowIsNotWidened(t *testing.T) {
	s, c := parityStore(t, parityGateFreeObjective)
	stmts := []string{}
	for i := 0; i < 250; i++ {
		stmts = append(stmts, "INSERT INTO events (id,project_id,collection_id,task_id,attempt_id,event_type,payload_json,created_at) VALUES ("+SQLLiteral(fmt.Sprintf("EVT-%03d", i))+",'"+parityProject+"','"+parityCID+"','T-impl',NULL,'TASK_STATUS',"+SQLLiteral(`{"n":`+fmt.Sprintf("%d", i)+`}`)+","+SQLLiteral("2026-01-01 00:00:00.000001")+")")
	}
	if e := s.Mutate(strings.Join(stmts, ";"), "seed 250 events"); e != nil {
		t.Fatal(e)
	}
	standalone, err := s.EventsForCollection(parityCID, 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(standalone) != 200 {
		t.Fatalf("the standalone window must read exactly 200 rows, got %d", len(standalone))
	}
	snaps, err := s.loadCollectionRowsWithAcceptance([]string{parityCID})
	if err != nil {
		t.Fatal(err)
	}
	sn := snaps[parityCID]
	if len(sn.events) != 200 {
		t.Fatalf("the batched events window must equal the 200-row contract, got %d", len(sn.events))
	}
	for i := range standalone {
		if S(standalone[i], "id") != S(sn.events[i], "id") {
			t.Fatalf("batched events window diverges from EventsForCollection(cid,200) at row %d: %s vs %s", i, S(standalone[i], "id"), S(sn.events[i], "id"))
		}
	}
	// And the decisions bucket is the same inventory ListDecisions returns.
	standaloneDecisions, err := s.ListDecisions(S(c, "project_id"), parityCID)
	if err != nil {
		t.Fatal(err)
	}
	if len(standaloneDecisions) != len(sn.decisions) {
		t.Fatalf("batched decisions inventory diverges from ListDecisions: %d vs %d", len(standaloneDecisions), len(sn.decisions))
	}
}

// TestAcceptanceReadinessSweepIssuesOneBatchPerCollection is the Phase 3.5
// headline: a sweep over N settled COMPLETE collections costs one batched
// `dolt sql` per collection, and the per-BFS-node JOIN read is GONE from the
// whole sweep.
func TestAcceptanceReadinessSweepIssuesOneBatchPerCollection(t *testing.T) {
	assertFloorObjectiveAllFalse(t)
	s, cfg, c, coord := gateFreeFloorCollection(t, floorObjective)
	impl := floorImplement(t, s, c, coord)
	if err := s.SetTaskStatus(S(coord, "id"), TaskComplete, "coord handed off"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTaskStatus(S(impl, "id"), TaskComplete, "impl complete"); err != nil {
		t.Fatal(err)
	}
	review, _, err := s.CreateTask(CreateTaskOptions{ProjectID: S(c, "project_id"), CollectionID: S(c, "id"), Profile: "review", Title: "Independent review", Objective: "review", Reason: "review", ParentTaskID: S(coord, "id"), DependsOn: []string{S(impl, "id")}, WorkspaceMode: "collection", Depth: 1, AllowDedupe: false})
	if err != nil {
		t.Fatal(err)
	}
	completeReviewWithVerdict(t, s, review, "pass")
	if err := s.AddEdge(S(impl, "id"), S(review, "id"), S(c, "id"), "verifies"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetCollectionStatus(S(c, "id"), "COMPLETE", true); err != nil {
		t.Fatal(err)
	}
	sched := newFloorScheduler(t, s, cfg)
	logPath := filepath.Join(t.TempDir(), "sqlops.log")
	t.Setenv("PRAECTOR_FAKE_DOLT_LOG", logPath)
	resetSQLLog(t, logPath)
	sched.observeAcceptanceReadiness(S(c, "id"))

	if n := countSQLMatching(t, logPath, "FROM task_edges e JOIN tasks t ON t.id=e.from_task_id WHERE e.to_task_id="); n != 0 {
		t.Fatalf("the per-BFS-node ancestry read must be gone from the sweep, got %d invocations", n)
	}
	batches := countSQLMatching(t, logPath, "FROM collections WHERE id IN")
	if batches != 1 {
		t.Fatalf("a sweep of one collection must issue exactly one batched snapshot read, got %d", batches)
	}
	if n := countSQLMatching(t, logPath, "FROM decisions WHERE superseded_by IS NULL AND project_id IN (SELECT project_id FROM collections"); n != 1 {
		t.Fatalf("the decisions read must be folded into that same batch tail, got %d", n)
	}
	// The seam counts dolt sql INVOCATIONS, so this is the single batch line that
	// carries both the CRITIC_PLAN_BASELINE bucket statement and the 200-row
	// harvest-window statement: one process, two statements.
	if n := countSQLMatching(t, logPath, "FROM events WHERE collection_id="); n != 1 {
		t.Fatalf("the events reads must ride inside the single batched process, got %d invocations", n)
	}
	if n := countSQLOperations(t, logPath); n != 1 {
		t.Fatalf("an acceptable COMPLETE collection must cost exactly one dolt sql per sweep, got %d", n)
	}
	// No standalone per-decision read survives anywhere in the sweep.
	for _, standalone := range []string{"FROM decisions WHERE project_id=", "SELECT * FROM tasks WHERE collection_id='", "SELECT * FROM task_edges WHERE collection_id='"} {
		if n := countSQLMatching(t, logPath, standalone); n != 0 {
			t.Fatalf("sweep must not issue the standalone %q read, got %d", standalone, n)
		}
	}
}

// TestAcceptanceReadinessSweepOverNCollectionsIssuesNBatches is the read-budget
// floor recorded in the tracker: one batched `dolt sql` per settled COMPLETE
// collection per sweep, plus (only when a would-block actually fires) the
// fingerprint-dedup event read and the marker write. Nothing per BFS node, and no
// standalone per-decision read.
func TestAcceptanceReadinessSweepOverNCollectionsIssuesNBatches(t *testing.T) {
	assertFloorObjectiveAllFalse(t)
	s, cfg, first, firstCoord := gateFreeFloorCollection(t, floorObjective)
	makeAcceptableCollection(t, s, first, firstCoord)
	cids := []string{S(first, "id")}
	for _, title := range []string{"Second", "Third"} {
		c, coord, err := s.CreateCollectionWithCoordinator(S(first, "project_id"), title, floorObjective, "Plan")
		if err != nil {
			t.Fatal(err)
		}
		forceCollectionCreatedAt(t, s, S(c, "id"), postCutoffCreatedAt)
		makeAcceptableCollection(t, s, c, coord)
		cids = append(cids, S(c, "id"))
	}
	sched := newFloorScheduler(t, s, cfg)
	logPath := filepath.Join(t.TempDir(), "sqlops.log")
	t.Setenv("PRAECTOR_FAKE_DOLT_LOG", logPath)
	resetSQLLog(t, logPath)
	for _, cid := range cids {
		sched.observeAcceptanceReadiness(cid)
	}
	if got := countSQLMatching(t, logPath, "FROM collections WHERE id IN"); got != len(cids) {
		t.Fatalf("a sweep over %d settled collections must issue exactly %d batched reads, got %d", len(cids), len(cids), got)
	}
	if got := countSQLOperations(t, logPath); got != len(cids) {
		t.Fatalf("an acceptable sweep must cost exactly one dolt sql per collection, got %d for %d collections", got, len(cids))
	}
	if got := countSQLMatching(t, logPath, "FROM task_edges e JOIN tasks t ON t.id=e.from_task_id WHERE e.to_task_id="); got != 0 {
		t.Fatalf("the per-BFS-node ancestry read must be gone, got %d invocations", got)
	}
}

// makeAcceptableCollection drives a gate-free collection to COMPLETE-and-acceptable:
// coordinator + implement COMPLETE, one passing verifying review that covers the
// implement.
func makeAcceptableCollection(t *testing.T, s *Store, c, coord Row) {
	t.Helper()
	impl := floorImplement(t, s, c, coord)
	if err := s.SetTaskStatus(S(coord, "id"), TaskComplete, "coord handed off"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTaskStatus(S(impl, "id"), TaskComplete, "impl complete"); err != nil {
		t.Fatal(err)
	}
	review, _, err := s.CreateTask(CreateTaskOptions{ProjectID: S(c, "project_id"), CollectionID: S(c, "id"), Profile: "review", Title: "Independent review", Objective: "review", Reason: "review", ParentTaskID: S(coord, "id"), DependsOn: []string{S(impl, "id")}, WorkspaceMode: "collection", Depth: 1, AllowDedupe: false})
	if err != nil {
		t.Fatal(err)
	}
	completeReviewWithVerdict(t, s, review, "pass")
	if err := s.AddEdge(S(impl, "id"), S(review, "id"), S(c, "id"), "verifies"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetCollectionStatus(S(c, "id"), "COMPLETE", true); err != nil {
		t.Fatal(err)
	}
}

// TestAncestryWalkFrontierOrderingMatchesSQL pins the deterministic frontier
// order: (edge created_at, from_task_id) must visit exactly what the removed SQL
// `ORDER BY e.created_at` visited. The SQL has no secondary sort key, so dolt row
// order within equal created_at is NOT a contract; what IS pinned here is that
// the Go side never depends on Go map order.
func TestAncestryWalkFrontierOrderingMatchesSQL(t *testing.T) {
	s, _ := parityStore(t, parityGateFreeObjective,
		// Four prerequisites created at the SAME instant: the only place where the
		// tie-break can matter.
		"INSERT INTO tasks (id,project_id,collection_id,profile,title,objective,status,created_at,updated_at) VALUES ('T-e','PRJ-par','COL-par','critic','e','o','COMPLETE',"+SQLLiteral(parityT1)+","+SQLLiteral(parityT1)+")",
		"INSERT INTO tasks (id,project_id,collection_id,profile,title,objective,status,created_at,updated_at) VALUES ('T-d','PRJ-par','COL-par','advise','d','o','COMPLETE',"+SQLLiteral(parityT1)+","+SQLLiteral(parityT1)+")",
		"INSERT INTO tasks (id,project_id,collection_id,profile,title,objective,status,created_at,updated_at) VALUES ('T-c','PRJ-par','COL-par','plan_review','c','o','COMPLETE',"+SQLLiteral(parityT1)+","+SQLLiteral(parityT1)+")",
		"INSERT INTO tasks (id,project_id,collection_id,profile,title,objective,status,created_at,updated_at) VALUES ('T-b','PRJ-par','COL-par','implement','b','o','RUNNING',"+SQLLiteral(parityT1)+","+SQLLiteral(parityT1)+")",
		"INSERT INTO tasks (id,project_id,collection_id,profile,title,objective,status,created_at,updated_at) VALUES ('T-target','PRJ-par','COL-par','review','t','o','RUNNING',"+SQLLiteral(parityT2)+","+SQLLiteral(parityT2)+")",
	)
	// Seed the four edges last so their created_at ties cannot be resolved by the
	// insert order alone.
	seedEdges := []string{}
	for _, from := range []string{"T-e", "T-d", "T-c", "T-b"} {
		seedEdges = append(seedEdges, "INSERT INTO task_edges (from_task_id,to_task_id,collection_id,relation,created_at) VALUES ("+SQLLiteral(from)+",'T-target','"+parityCID+"','depends_on',"+SQLLiteral(parityT2)+")")
	}
	if e := s.Mutate(strings.Join(seedEdges, ";"), "seed tied frontier edges"); e != nil {
		t.Fatal(e)
	}
	tasks, edges, err := s.loadAncestryRows("T-target")
	if err != nil {
		t.Fatal(err)
	}
	frontier := dependsOnAdjacency(edges)["T-target"]
	got := []string{}
	for _, e := range frontier {
		got = append(got, S(e, "from_task_id"))
	}
	want := []string{"T-b", "T-c", "T-d", "T-e"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("frontier visit order must be (created_at, from_task_id) deterministic, got %v want %v", got, want)
	}
	// Stability: rebuilding the adjacency from a SHUFFLED edge slice (the shape a
	// map-ordered loader would produce) yields the identical visit order.
	for _, shuffle := range []Rows{reverseRows(edges), rotateRows(edges)} {
		again := []string{}
		for _, e := range dependsOnAdjacency(shuffle)["T-target"] {
			again = append(again, S(e, "from_task_id"))
		}
		if strings.Join(again, ",") != strings.Join(want, ",") {
			t.Fatalf("frontier order leaked input/map order: %v", again)
		}
	}
	// The walk's OUTPUT order stays the documented (created_at, id) contract and
	// matches the reference SQL walk on the same tied graph.
	rowsOut := idsOf(ancestorWalkFromRows(tasks, edges, "T-target", ancestorProfileIsDirectiveSource))
	sqlOut := idsOf(referenceDirectiveAncestorsSQL(t, s, "T-target"))
	assertSameIDs(t, "tied-frontier walk output", rowsOut, sqlOut)
}

func idsOf(rs Rows) []string {
	out := []string{}
	for _, r := range rs {
		out = append(out, S(r, "id"))
	}
	return out
}

func assertSameIDs(t *testing.T, label string, got, want []string) {
	t.Helper()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("%s: rows path %v, reference sql path %v", label, got, want)
	}
}

func reverseRows(rs Rows) Rows {
	out := make(Rows, 0, len(rs))
	for i := len(rs) - 1; i >= 0; i-- {
		out = append(out, rs[i])
	}
	return out
}

func rotateRows(rs Rows) Rows {
	if len(rs) < 2 {
		return append(Rows{}, rs...)
	}
	return append(append(Rows{}, rs[len(rs)/2:]...), rs[:len(rs)/2]...)
}
