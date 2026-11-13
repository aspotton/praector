package praector

import "testing"

// gate is one planning-gate accessor used by the coherence and phrasing oracles.
type gate func(PlanningPolicy) bool

var (
	gateExplore  gate = func(p PlanningPolicy) bool { return p.ExploreRequired }
	gateResearch gate = func(p PlanningPolicy) bool { return p.ResearchRequired }
	gateCritic   gate = func(p PlanningPolicy) bool { return p.CriticRequired }
	gatePlan     gate = func(p PlanningPolicy) bool { return p.PlanReviewRequired }
	gateExtConf  gate = func(p PlanningPolicy) bool { return p.ExternalConformanceRequired }
	gateThreat   gate = func(p PlanningPolicy) bool { return p.ThreatModelRequired }
	anyGate      gate = func(p PlanningPolicy) bool {
		return p.ExploreRequired || p.ResearchRequired || p.CriticRequired || p.PlanReviewRequired || p.ExternalConformanceRequired || p.ThreatModelRequired
	}
)

func hasCategory(p PlanningPolicy, want string) bool {
	for _, c := range p.Categories {
		if c == want {
			return true
		}
	}
	return false
}

// TestPlanningGatePhrasingMatrix is the core regression oracle: for each risk
// category, a verb-first objective and a noun-phrase/passive objective describing
// the SAME underlying work must require the SAME risk gate. On the pre-change
// classifier (reproduced here by classifyPlanningPolicy(_, false)) the noun form
// misses the gate; the coherent floor closes that gap. This matrix must FAIL on
// the pre-change classifier and PASS on the coherent one.
func TestPlanningGatePhrasingMatrix(t *testing.T) {
	rows := []struct {
		category   string
		verb       string
		noun       string
		risk       gate
		verbLegacy bool // whether the pre-change verb-masked gate fired for the verb form
		nounLegacy bool // whether the pre-change verb-masked gate fired for the noun form (the defect: false)
	}{
		{"external_protocol_or_standard", "Update the transport to comply with the MCP protocol specification.", "MCP protocol specification compliance for the transport.", gateExtConf, true, false},
		{"security", "Fix the auth bypass in the token check.", "The auth bypass in the token check requires correction.", gateThreat, true, false},
		{"migration_or_persistence", "Migrate the ledger to the new format.", "Migrating the ledger to the new format.", gatePlan, true, true},
		{"destructive_change", "Delete the stale records.", "Deletion of the stale records.", gateCritic, true, true},
		{"external_integration", "Add Twilio gateway integration.", "Twilio gateway integration.", gateExtConf, true, false},
	}
	for _, r := range rows {
		cv, cn := ClassifyPlanningPolicy(r.verb), ClassifyPlanningPolicy(r.noun)
		if !hasCategory(cn, r.category) {
			t.Errorf("category %q: noun objective %q must record the category, got %v", r.category, r.noun, cn.Categories)
		}
		if r.risk(cv) != r.risk(cn) {
			t.Errorf("category %q: verb gate=%v noun gate=%v must agree for verb=%q noun=%q", r.category, r.risk(cv), r.risk(cn), r.verb, r.noun)
		}
		if !r.risk(cn) {
			t.Errorf("category %q: noun objective %q must enable its risk gate under the coherent floor", r.category, r.noun)
		}
		// Legacy (pre-cutoff) reproduction: the de-verb defect is that the noun
		// form's gate was off. For migration/destructive the residual gap was a
		// detector miss (both forms now detected), so both legacy flags agree.
		if got := r.risk(classifyPlanningPolicy(r.verb, false)); got != r.verbLegacy {
			t.Errorf("category %q: legacy verb gate=%v want %v (verb=%q)", r.category, got, r.verbLegacy, r.verb)
		}
		if got := r.risk(classifyPlanningPolicy(r.noun, false)); got != r.nounLegacy {
			t.Errorf("category %q: legacy noun gate=%v want %v (noun=%q)", r.category, got, r.nounLegacy, r.noun)
		}
	}
}

// TestPlanningGateCoherenceAllCategories proves the coherence invariant over all
// twelve category literals: a recorded category must never leave every gate it
// justifies disabled. evolving_dependency_or_vendor maps to explore (and NOT
// research), which is the category the deterministic floor would otherwise miss.
func TestPlanningGateCoherenceAllCategories(t *testing.T) {
	table := []struct {
		category  string
		objective string
		witness   gate
	}{
		{"implementation_change", "Add a helper function to the module.", anyGate},
		{"security", "Fix the authentication token check.", gateCritic},
		{"stateful_behavior", "The cache layer behavior under load.", gatePlan},
		{"migration_or_persistence", "Migrating the ledger to the new format.", gateCritic},
		{"destructive_change", "Deletion of the stale records.", gateCritic},
		{"external_contract", "Review the public API endpoint backward compatibility.", gateCritic},
		{"external_integration", "Twilio gateway integration.", gateExtConf},
		{"external_protocol_or_standard", "MCP protocol specification compliance for the transport.", gateExtConf},
		{"freshness_sensitive", "Summarize current news and recent events.", gateResearch},
		{"law_regulation_or_compliance", "Assess GDPR compliance implications.", gateResearch},
		{"network_trust_boundary", "Harden the HTTP server listener.", gateThreat},
	}
	for _, row := range table {
		p := ClassifyPlanningPolicy(row.objective)
		if !hasCategory(p, row.category) {
			t.Errorf("category %q not recorded for %q: %v", row.category, row.objective, p.Categories)
		}
		if !row.witness(p) {
			t.Errorf("category %q recorded for %q but its justified gate is disabled: %#v", row.category, row.objective, p)
		}
	}
	// evolving_dependency_or_vendor -> explore and NOT research.
	p := ClassifyPlanningPolicy("The vendor library needs assessment.")
	if !hasCategory(p, "evolving_dependency_or_vendor") {
		t.Fatalf("evolving category not recorded: %v", p.Categories)
	}
	if !p.ExploreRequired {
		t.Errorf("evolving_dependency_or_vendor must enable explore: %#v", p)
	}
	if p.ResearchRequired {
		t.Errorf("non-implementation evolving_dependency_or_vendor must NOT force research: %#v", p)
	}
}

// TestPlanningGateDestructiveNounDetectorKnownGap documents the residual detector
// gap as non-coverage: an objective naming destructive work only through the
// "removal" synonym records no destructive category and enables no destructive
// gate, so the coherence test's PASS is not mistaken for detector coverage.
func TestPlanningGateDestructiveNounDetectorKnownGap(t *testing.T) {
	p := ClassifyPlanningPolicy("The removal of stale records.")
	if hasCategory(p, "destructive_change") || hasCategory(p, "migration_or_persistence") {
		t.Fatalf("known-gap: 'removal' synonym must NOT be detected as destructive/migration: %v", p.Categories)
	}
	if p.CriticRequired || p.PlanReviewRequired || p.ExploreRequired {
		t.Fatalf("known-gap: undetected destructive noun must enable no gate: %#v", p)
	}
	// The morphological inflections that ARE covered.
	for _, o := range []string{"Deletion of stale records.", "Deleted stale records.", "Deleting stale records.", "Drop the stale table.", "Purging old rows."} {
		if !hasCategory(ClassifyPlanningPolicy(o), "destructive_change") && !hasCategory(ClassifyPlanningPolicy(o), "migration_or_persistence") {
			t.Errorf("widened destructive detector must classify %q", o)
		}
	}
}

// TestPlanningGateCoherenceDualSeparator proves the day-granularity comparison
// classifies the SQLite "T"-separated and Dolt " "-separated DATETIME renderings
// identically; it fails if the comparison is widened to the full string.
func TestPlanningGateCoherenceDualSeparator(t *testing.T) {
	for _, created := range []string{"2020-01-01T10:00:00.000000", "2020-01-01 10:00:00.000000"} {
		col := Row{"objective": "MCP protocol specification compliance for the transport.", "created_at": created}
		if planningGateCoherent(col) {
			t.Errorf("pre-cutoff %q must be legacy", created)
		}
	}
	for _, created := range []string{"2026-11-12T00:00:01.000000", "2026-11-12 23:59:59.000000"} {
		col := Row{"objective": "MCP protocol specification compliance for the transport.", "created_at": created}
		if !planningGateCoherent(col) {
			t.Errorf("on-cutoff %q must be coherent", created)
		}
	}
	// Fail closed to coherent when no created_at is reachable.
	if !planningGateCoherent(Row{"objective": "MCP protocol specification compliance"}) {
		t.Errorf("seam without a collection row must fail closed to the coherent floor")
	}
}

// TestPlanningGateTrivialDocsNotNewlyForced proves the widening does not turn
// trivial documentation work into plan_review theater.
func TestPlanningGateTrivialDocsNotNewlyForced(t *testing.T) {
	for _, o := range []string{
		"Add API-key authentication and per-key rate limiting to the public API endpoint.", // verb: plan_review IS required (non-trivial), kept
		"Fix a typo in README.md and update one example sentence.",
		"Update a typo in README.md.",
		"Fix documentation wording in the docs.",
		"A minor wording correction in the README.",
	} {
		p := ClassifyPlanningPolicy(o)
		// Trivial docs must never force plan_review; only the non-trivial verb
		// objective above is expected to force it (retained verb term).
		isTrivial := o != "Add API-key authentication and per-key rate limiting to the public API endpoint."
		if isTrivial && p.PlanReviewRequired {
			t.Errorf("trivial docs objective %q must not newly force plan_review: %#v", o, p)
		}
	}
	if !ClassifyPlanningPolicy("Add API-key authentication and per-key rate limiting to the public API endpoint.").PlanReviewRequired {
		t.Errorf("non-trivial implementation must retain the plan_review verb term")
	}
}

// TestGoPythonClassifierParityVectors pins the coherent-floor gate booleans over
// the shared objective matrix. scripts/parity_check.py does not compare classifier
// verdicts, so this table is machine-checked parity; the identical table is asserted
// in tests/test_planning_policy.py against the Python mirror.
func TestGoPythonClassifierParityVectors(t *testing.T) {
	type want struct{ e, r, c, p, x, th bool }
	table := map[string]want{
		"Update the transport to comply with the MCP protocol specification.":              {true, true, true, true, true, false},
		"MCP protocol specification compliance for the transport.":                         {true, true, true, true, true, false},
		"Fix the auth bypass in the token check.":                                          {true, false, true, true, false, true},
		"The auth bypass in the token check requires correction.":                          {true, false, true, true, false, true},
		"Migrate the ledger to the new format.":                                            {true, false, true, true, false, false},
		"Migrating the ledger to the new format.":                                          {true, false, true, true, false, false},
		"Delete the stale records.":                                                        {true, false, true, true, false, false},
		"Deletion of the stale records.":                                                   {true, false, true, true, false, false},
		"Integrate the Twilio gateway.":                                                    {true, true, true, true, true, false},
		"Twilio gateway integration.":                                                      {true, true, true, true, true, false},
		"The vendor library needs assessment.":                                             {true, false, false, false, false, false},
		"Summarize current news and recent events.":                                        {true, true, false, false, false, false},
		"Assess GDPR compliance implications.":                                             {true, true, false, false, false, false},
		"Review the public API endpoint backward compatibility.":                           {true, false, true, true, false, false},
		"The cache layer behavior under load.":                                             {true, false, false, true, false, false},
		"Harden the HTTP server listener.":                                                 {true, false, false, true, false, true},
		"Update a typo in README.md.":                                                      {true, false, true, false, false, false},
		"Fix documentation wording in the docs.":                                           {true, false, true, false, false, false},
		"A minor wording correction in the README.":                                        {false, false, false, false, false, false},
		"The removal of stale records.":                                                    {false, false, false, false, false, false},
		"Add API-key authentication and per-key rate limiting to the public API endpoint.": {true, false, true, true, false, true},
		"Fix a typo in README.md and update one example sentence.":                         {true, false, true, false, false, false},
	}
	for obj, w := range table {
		p := ClassifyPlanningPolicy(obj)
		if p.ExploreRequired != w.e || p.ResearchRequired != w.r || p.CriticRequired != w.c || p.PlanReviewRequired != w.p || p.ExternalConformanceRequired != w.x || p.ThreatModelRequired != w.th {
			t.Errorf("parity vector mismatch for %q: got E=%v R=%v C=%v P=%v X=%v T=%v want E=%v R=%v C=%v P=%v X=%v T=%v", obj,
				p.ExploreRequired, p.ResearchRequired, p.CriticRequired, p.PlanReviewRequired, p.ExternalConformanceRequired, p.ThreatModelRequired,
				w.e, w.r, w.c, w.p, w.x, w.th)
		}
	}
}
