package praector

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

type PlanningPolicy struct {
	ExploreRequired, ResearchRequired, CriticRequired, PlanReviewRequired bool
	ExternalConformanceRequired, ThreatModelRequired                      bool
	Categories, Reasons                                                   []string
}
type DeliveryPolicy struct {
	ImplementationRequired bool
	Categories, Reasons    []string
}

func (p PlanningPolicy) AsRow() Row {
	return Row{
		"explore_required":              p.ExploreRequired,
		"research_required":             p.ResearchRequired,
		"critic_required":               p.CriticRequired,
		"plan_review_required":          p.PlanReviewRequired,
		"external_conformance_required": p.ExternalConformanceRequired,
		"threat_model_required":         p.ThreatModelRequired,
		"categories":                    p.Categories,
		"reasons":                       p.Reasons,
	}
}
func (p DeliveryPolicy) AsRow() Row {
	return Row{"implementation_required": p.ImplementationRequired, "categories": p.Categories, "reasons": p.Reasons}
}

var securityPatterns = []string{`\bauth(?:entication|orization)?\b`, `\bapi[-_ ]?key(?:s)?\b`, `\bcredential(?:s)?\b`, `\bsecret(?:s)?\b`, `\bpassword(?:s)?\b`, `\boauth\b`, `\btoken(?:s)?\b`, `\baccess[ -]?control\b`, `\bpermission(?:s)?\b`, `\bencrypt(?:ion|ed)?\b`, `\bsecurity\b`}
var statefulPatterns = []string{`\brate[ -]?limit(?:ing)?\b`, `\bstateful\b`, `\bsession(?:s)?\b`, `\bcache(?:d|s|ing)?\b`, `\bqueue(?:s)?\b`, `\bretr(?:y|ies)\b`, `\bidempoten(?:t|cy)\b`, `\bconcurren(?:t|cy)\b`, `\block(?:ing|s)?\b`, `\btransaction(?:s)?\b`}
var migrationPatterns = []string{`\bmigrat(?:e|ion|ions)\b`, `\bschema\b`, `\bdatabase\b`, `\bbackfill\b`, `\bdata conversion\b`}
var destructivePatterns = []string{`\bdelete\b`, `\bdrop\b`, `\bpurge\b`, `\bdestructive\b`, `\birreversible\b`}
var externalContractPatterns = []string{`\bapi\b`, `\bwebhook(?:s)?\b`, `\bpublic endpoint\b`, `\bbackward compatib(?:le|ility)\b`}
var externalIntegrationPatterns = []string{`\bgateway(?:s)?\b`, `\bintegration(?:s)?\b`, `\bthird[- ]party\b`, `\bexternal service\b`, `\bexternal api\b`, `\bprovider(?:s)?\b`, `\bsdk\b`, `\btwilio\b`, `\btelephony\b`, `\btwiml\b`, `\bcloud\b`}
var protocolPatterns = []string{`\bprotocol\b`, `\bspecification\b`, `\bstandard\b`, `\brfc\b`, `\bwire format\b`, `\bcompatibility matrix\b`, `\bmcp\b`, `\bmodel context protocol\b`, `\bopenapi\b`}
var freshnessPatterns = []string{`\blatest\b`, `\brecent\b`, `\bas[- ]of\b`, `\btoday\b`, `\bthis week\b`, `\bnewest\b`, `\bup[- ]to[- ]date\b`, `\bnews\b`, `\bcurrent events?\b`, `\bcurrent\s+(?:version|release|specification|standard|api|sdk|law|regulation|policy|events?|news|pricing|availability|support)\b`}
var regulationPatterns = []string{`\blaw\b`, `\blegal\b`, `\bregulat(?:ion|ory|ions)\b`, `\bcompliance\b`, `\bpolicy requirement\b`, `\bhipaa\b`, `\bgdpr\b`, `\bsec rule\b`}
var evolvingDependencyPatterns = []string{
	`\blibrar(?:y|ies)\b`, `\bframework\b`, `\bvendor\b`, `\bsupport matrix\b`,
	`\b(?:package|dependency|library|framework)\s+(?:version|release|compatibility|support)\b`,
	`\b(?:upgrade|update)\s+(?:a\s+)?(?:package|dependency|library|framework)\b`,
}
var networkBoundaryPatterns = []string{`\blistener\b`, `\bhttp server\b`, `\bserver\b`, `\bnetwork\b`, `\bbind\b`, `\bsocket\b`, `\bremote\b`, `\bwebhook\b`, `\binbound\b`, `\bpublic endpoint\b`}

const mutationVerbs = `add|implement|build|create|modify|change|update|fix|repair|refactor|remove|delete|migrate|extend|introduce|replace|upgrade|convert|make|enforce|support`

var implementationFirstLine = regexp.MustCompile(`(?i)^(?:please\s+)?(?:` + mutationVerbs + `)\b`)
var implementationRequests = []string{`\b(?:i(?:'|’)d|i would|we would)\s+like\s+to\s+(?:` + mutationVerbs + `)\b`, `\b(?:i|we)\s+(?:want|need)\s+(?:you\s+)?to\s+(?:` + mutationVerbs + `)\b`, `\b(?:can|could|would)\s+you\s+(?:please\s+)?(?:` + mutationVerbs + `)\b`, `\bplease\s+(?:` + mutationVerbs + `)\b`, `\blet(?:'|’)s\s+(?:` + mutationVerbs + `)\b`}
var documentationPatterns = []string{`\breadme(?:\.md)?\b`, `\bdocumentation\b`, `\bdocs?\b`, `\btypo\b`, `\bspelling\b`, `\bwording\b`, `\bexample sentence\b`}
var codeRuntimePatterns = []string{`\bfeature\b`, `\bbug\b`, `\bcode(?:base)?\b`, `\bfunction\b`, `\bmethod\b`, `\bclass\b`, `\bendpoint\b`, `\broute\b`, `\bhandler\b`, `\bgateway\b`, `\btemplate\b`, `\bservice\b`, `\bclient\b`, `\bserver\b`, `\btest(?:s|ing)?\b`, `\bapi\b`, `\bwebhook\b`, `\btransfer\b`, `\bdtmf\b`, `\btwiml\b`, `\bdial\b`, `\b(?:py|js|ts|java|go|rs|rb|php|cs|cpp|c|h)\b`, `\.(?:py|pyi|js|jsx|ts|tsx|java|go|rs|rb|php|cs|cpp|cc|cxx|c|h|hpp|sql|sh)\b`}

func matchesAny(text string, pats []string) bool {
	for _, p := range pats {
		if regexp.MustCompile(`(?i)`+p).FindStringIndex(text) != nil {
			return true
		}
	}
	return false
}
func requiresImplementation(obj string) bool {
	first := ""
	for _, ln := range strings.Split(obj, "\n") {
		if strings.TrimSpace(ln) != "" {
			first = strings.TrimSpace(ln)
			break
		}
	}
	if implementationFirstLine.MatchString(first) {
		return true
	}
	return matchesAny(obj, implementationRequests)
}
func trivialDocs(obj string) bool {
	return matchesAny(obj, documentationPatterns) && !matchesAny(obj, codeRuntimePatterns)
}

func ClassifyPlanningPolicy(obj string) PlanningPolicy {
	cats := []string{}
	reasons := []string{}
	implementation := requiresImplementation(obj)
	trivial := implementation && trivialDocs(obj)
	security := matchesAny(obj, securityPatterns)
	stateful := matchesAny(obj, statefulPatterns)
	migration := matchesAny(obj, migrationPatterns)
	destructive := matchesAny(obj, destructivePatterns)
	extContract := matchesAny(obj, externalContractPatterns)
	extIntegration := matchesAny(obj, externalIntegrationPatterns)
	protocol := matchesAny(obj, protocolPatterns)
	freshness := matchesAny(obj, freshnessPatterns)
	regulation := matchesAny(obj, regulationPatterns)
	evolving := matchesAny(obj, evolvingDependencyPatterns)
	networkBoundary := matchesAny(obj, networkBoundaryPatterns)

	if implementation {
		cats = append(cats, "implementation_change")
		reasons = append(reasons, "Implementation work requires a read-only repository exploration preflight before coordination.")
		reasons = append(reasons, "Implementation work always requires an independent pre-planning critic.")
		if !trivial {
			reasons = append(reasons, "Non-trivial implementation work requires an independent plan-review gate before write work.")
		}
	}
	if security {
		cats = append(cats, "security")
		reasons = append(reasons, "Security/authentication/credential-sensitive behavior requires an independent pre-planning critic.")
	}
	if stateful {
		cats = append(cats, "stateful_behavior")
		reasons = append(reasons, "Stateful/concurrency/rate-limit behavior requires explicit state and verification semantics.")
	}
	if migration {
		cats = append(cats, "migration_or_persistence")
		reasons = append(reasons, "Persistence/schema/migration work requires explicit compatibility, rollback, and verification planning.")
	}
	if destructive {
		cats = append(cats, "destructive_change")
		reasons = append(reasons, "Potentially destructive or irreversible work requires an independent planning challenge and review.")
	}
	if extContract {
		cats = append(cats, "external_contract")
	}
	if extIntegration {
		cats = append(cats, "external_integration")
		reasons = append(reasons, "External provider/gateway integration behavior requires an independent critic before write work.")
	}
	if protocol {
		cats = append(cats, "external_protocol_or_standard")
	}
	if freshness {
		cats = append(cats, "freshness_sensitive")
	}
	if regulation {
		cats = append(cats, "law_regulation_or_compliance")
	}
	if evolving {
		cats = append(cats, "evolving_dependency_or_vendor")
	}
	if networkBoundary {
		cats = append(cats, "network_trust_boundary")
	}

	research := protocol || freshness || regulation || extIntegration || (implementation && evolving)
	if research {
		reasons = append(reasons, "Correctness depends on fresh external evidence; current authoritative research is required before critique/planning.")
	}
	externalConformance := implementation && (protocol || extIntegration)
	if externalConformance {
		reasons = append(reasons, "External protocol/provider work requires an authoritative independent verification oracle when practical.")
	}
	threatModel := implementation && (security || networkBoundary)
	if threatModel {
		reasons = append(reasons, "A network/security trust boundary requires explicit threat-model directives before write work.")
	}
	integrationSensitive := implementation && (extContract || extIntegration || protocol)
	explore := implementation
	critic := implementation || security || migration || destructive || (stateful && extContract) || integrationSensitive
	plan := stateful || migration || destructive || integrationSensitive || externalConformance || threatModel || (implementation && !trivial)
	if plan {
		found := false
		for _, r := range reasons {
			lr := strings.ToLower(r)
			if strings.Contains(lr, "plan-review") || strings.Contains(lr, "plan review") {
				found = true
			}
		}
		if !found {
			reasons = append(reasons, "The objective crosses the deterministic threshold for an independent plan-review gate before write work.")
		}
	}
	return PlanningPolicy{ExploreRequired: explore, ResearchRequired: research, CriticRequired: critic, PlanReviewRequired: plan, ExternalConformanceRequired: externalConformance, ThreatModelRequired: threatModel, Categories: cats, Reasons: reasons}
}
func ClassifyDeliveryPolicy(obj string) DeliveryPolicy {
	impl := requiresImplementation(obj)
	if impl {
		return DeliveryPolicy{true, []string{"implementation_delivery"}, []string{"The Collection objective is a direct change request; at least one completed implementation with passing independent review is required before COMPLETE."}}
	}
	return DeliveryPolicy{}
}

var materialProfiles = map[string]bool{"implement": true, "research": true, "advise": true}
var materialRelations = map[string]bool{"depends_on": true, "blocks": true, "supersedes": true, "superseded_by": true}

func researchResultDigest(t Row) string {
	if S(t, "profile") != "research" {
		return ""
	}
	var value any
	if raw := strings.TrimSpace(S(t, "result_json")); raw != "" {
		value = JSONMap(raw)
	} else if t["result"] != nil {
		value = t["result"]
	} else {
		return ""
	}
	b, _ := json.Marshal(value)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// DependencyLineageDigest captures the direct prerequisite state a task started
// from. It complements MaterialPlanFingerprint: the plan fingerprint detects
// graph/research contract changes, while this digest catches a prerequisite
// task being reopened or changing its integrated head/result in place.
func DependencyLineageDigest(deps Rows) string {
	type depPayload struct {
		ID, Status, CurrentHead, ResultDigest string
	}
	items := make([]depPayload, 0, len(deps))
	for _, dep := range deps {
		raw := strings.TrimSpace(S(dep, "result_json"))
		resultDigest := ""
		if raw != "" {
			h := sha256.Sum256([]byte(raw))
			resultDigest = hex.EncodeToString(h[:])
		}
		items = append(items, depPayload{ID: S(dep, "id"), Status: S(dep, "status"), CurrentHead: S(dep, "current_head"), ResultDigest: resultDigest})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	b, _ := json.Marshal(items)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func materialPlanFingerprint(objective string, tasks Rows, edges Rows, legacy, includeResearchDigest bool) string {
	mt := Rows{}
	for _, t := range tasks {
		if materialProfiles[S(t, "profile")] && S(t, "status") != "CANCELLED" && S(t, "status") != "SUPERSEDED" {
			mt = append(mt, t)
		}
	}
	sort.Slice(mt, func(i, j int) bool {
		a := S(mt[i], "profile") + "\x00" + S(mt[i], "id")
		b := S(mt[j], "profile") + "\x00" + S(mt[j], "id")
		return a < b
	})
	ids := map[string]bool{}
	tp := []Row{}
	for _, t := range mt {
		ids[S(t, "id")] = true
		wm := S(t, "workspace_mode")
		if wm == "" {
			wm = "collection"
		}
		base := S(t, "base_commit")
		if !legacy && wm != "inherit" {
			base = ""
		}
		row := Row{"id": S(t, "id"), "profile": S(t, "profile"), "title": NormalizeText(S(t, "title")), "objective": NormalizeText(S(t, "objective")), "reason": NormalizeText(S(t, "reason")), "workspace_mode": wm, "base_commit": base}
		if includeResearchDigest {
			if d := researchResultDigest(t); d != "" {
				row["research_result_digest"] = d
			}
		}
		tp = append(tp, row)
	}
	ep := [][]string{}
	for _, e := range edges {
		rel := S(e, "relation")
		from := S(e, "from_task_id")
		to := S(e, "to_task_id")
		if materialRelations[rel] && ids[from] && ids[to] {
			ep = append(ep, []string{from, to, rel})
		}
	}
	sort.Slice(ep, func(i, j int) bool { return strings.Join(ep[i], "\x00") < strings.Join(ep[j], "\x00") })
	payload := map[string]any{"objective": NormalizeText(objective), "tasks": tp, "edges": ep}
	b, _ := json.Marshal(payload)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func MaterialPlanFingerprint(objective string, tasks Rows, edges Rows, legacy bool) string {
	return materialPlanFingerprint(objective, tasks, edges, legacy, true)
}

// MaterialPlanFingerprintV021 reproduces the 0.2.1 fingerprint contract.
// 0.2.2 intentionally added completed research-result digests to the material
// plan fingerprint so refreshed external evidence can stale active planning.
// Historical critic seals must remain valid across that upgrade, though; an
// already-COMPLETE Collection must not become BLOCKED solely because the hash
// algorithm gained stronger evidence lineage.
func MaterialPlanFingerprintV021(objective string, tasks Rows, edges Rows, legacy bool) string {
	return materialPlanFingerprint(objective, tasks, edges, legacy, false)
}

// ValidateDeterministicResearchResult enforces a machine-checkable evidence ->
// directive contract for policy-injected fresh research. Semantic review still
// belongs to critic/plan_review, but obvious provenance and normative weakening
// errors are rejected before the research can become planning evidence.
func researchFieldString(v any) string {
	if v == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(v))
}

func ValidateDeterministicResearchResult(result Row, requireOracle bool) error {
	if strings.TrimSpace(S(result, "as_of")) == "" {
		return fmt.Errorf("deterministic fresh research result requires as_of")
	}
	sources, ok := result["sources"].([]any)
	if !ok || len(sources) == 0 {
		return fmt.Errorf("deterministic fresh research result requires non-empty sources")
	}
	sourceIDs := map[string]bool{}
	allowedAuthority := map[string]bool{
		"official_specification": true, "official_standard": true, "official_release_notes": true,
		"official_sdk": true, "first_party": true, "vendor_documentation": true,
		"secondary": true, "community": true,
	}
	for i, raw := range sources {
		m, ok := raw.(map[string]any)
		if !ok {
			if r, ok2 := raw.(Row); ok2 {
				m = map[string]any(r)
			} else {
				return fmt.Errorf("research source %d must be an object", i)
			}
		}
		id := researchFieldString(m["id"])
		authority := strings.ToLower(researchFieldString(m["authority_level"]))
		if id == "" || researchFieldString(m["authority"]) == "" || researchFieldString(m["reference"]) == "" || authority == "" {
			return fmt.Errorf("research source %d requires id, authority, authority_level, and reference", i)
		}
		if !allowedAuthority[authority] {
			return fmt.Errorf("research source %d has unsupported authority_level %q", i, authority)
		}
		if sourceIDs[id] {
			return fmt.Errorf("research source id %q is duplicated", id)
		}
		sourceIDs[id] = true
	}
	claims, ok := result["claims"].([]any)
	if !ok || len(claims) == 0 {
		return fmt.Errorf("deterministic fresh research result requires non-empty claims")
	}
	allowedNorm := map[string]bool{"MUST": true, "MUST_NOT": true, "SHOULD": true, "MAY": true, "FACT": true, "INFERENCE": true}
	allowedEffect := map[string]bool{"preserve": true, "strengthen": true, "exception": true}
	claimIDs := map[string]bool{}
	directives := map[string]bool{}
	for i, raw := range claims {
		m, ok := raw.(map[string]any)
		if !ok {
			if r, ok2 := raw.(Row); ok2 {
				m = map[string]any(r)
			} else {
				return fmt.Errorf("research claim %d must be an object", i)
			}
		}
		need := []string{"id", "claim", "source", "authority", "normative_level", "planner_directive", "directive_effect", "confidence"}
		for _, k := range need {
			if researchFieldString(m[k]) == "" {
				return fmt.Errorf("research claim %d requires %s", i, k)
			}
		}
		claimID := researchFieldString(m["id"])
		if claimIDs[claimID] {
			return fmt.Errorf("research claim id %q is duplicated", claimID)
		}
		claimIDs[claimID] = true
		if !sourceIDs[researchFieldString(m["source"])] {
			return fmt.Errorf("research claim %d references unknown source %q", i, researchFieldString(m["source"]))
		}
		norm := strings.ToUpper(researchFieldString(m["normative_level"]))
		if !allowedNorm[norm] {
			return fmt.Errorf("research claim %d has unsupported normative_level %q", i, norm)
		}
		effect := strings.ToLower(researchFieldString(m["directive_effect"]))
		if !allowedEffect[effect] {
			return fmt.Errorf("research claim %d directive_effect must be preserve, strengthen, or exception", i)
		}
		if effect == "exception" && researchFieldString(m["exception_decision_id"]) == "" {
			return fmt.Errorf("research claim %d exception requires exception_decision_id", i)
		}
		directives[NormalizeText(researchFieldString(m["planner_directive"]))] = true
	}
	// Structured claims are the authoritative place for material research
	// directives. If a researcher also emits the legacy top-level
	// planner_directives list, each entry must be traceable verbatim to a claim.
	// This prevents an unstructured "code-ready" directive from silently
	// contradicting the structured normative evidence.
	for i, raw := range Slice(result, "planner_directives") {
		d := NormalizeText(researchFieldString(raw))
		if d == "" || !directives[d] {
			return fmt.Errorf("research planner_directive %d is not backed by a structured claim", i)
		}
	}
	oracles, oracleOK := result["verification_oracles"].([]any)
	if requireOracle && (!oracleOK || len(oracles) == 0) {
		return fmt.Errorf("external-conformance research requires at least one verification_oracle")
	}
	allowedOracle := map[string]bool{"internal_test": true, "black_box_test": true, "independent_implementation": true, "official_reference_oracle": true, "live_provider_oracle": true}
	authoritativeOracle := false
	if oracleOK {
		for i, raw := range oracles {
			m, ok := raw.(map[string]any)
			if !ok {
				if r, ok2 := raw.(Row); ok2 {
					m = map[string]any(r)
				} else {
					return fmt.Errorf("verification_oracle %d must be an object", i)
				}
			}
			kind := strings.ToLower(researchFieldString(m["kind"]))
			if !allowedOracle[kind] || researchFieldString(m["name"]) == "" {
				return fmt.Errorf("verification_oracle %d requires a supported kind and name", i)
			}
			if kind == "independent_implementation" || kind == "official_reference_oracle" || kind == "live_provider_oracle" {
				authoritativeOracle = true
			}
		}
	}
	if requireOracle && !authoritativeOracle && strings.TrimSpace(S(result, "authoritative_oracle_unavailable_reason")) == "" {
		return fmt.Errorf("external-conformance research requires an authoritative independent oracle or authoritative_oracle_unavailable_reason")
	}
	return nil
}

type ResearchClaimConstraint struct {
	TaskID           string
	ClaimID          string
	ClaimRef         string
	PlannerDirective string
	NormativeLevel   string
	Authority        string
	Source           string
}

func ResearchClaimConstraints(tasks Rows) ([]ResearchClaimConstraint, error) {
	type rawRef struct {
		TaskID, ClaimID, PlannerDirective, NormativeLevel, Authority, Source string
	}
	refs := []rawRef{}
	counts := map[string]int{}
	for _, task := range tasks {
		if S(task, "profile") != "research" || S(task, "status") != TaskComplete {
			continue
		}
		result := M(task, "result")
		if len(result) == 0 && strings.TrimSpace(S(task, "result_json")) != "" {
			result = JSONMap(S(task, "result_json"))
		}
		for i, raw := range Slice(result, "claims") {
			var claim Row
			switch x := raw.(type) {
			case map[string]any:
				claim = Row(x)
			case Row:
				claim = x
			default:
				return nil, fmt.Errorf("research task %s claim %d must be an object", S(task, "id"), i)
			}
			id := strings.TrimSpace(S(claim, "id"))
			directive := strings.TrimSpace(S(claim, "planner_directive"))
			if id == "" {
				return nil, fmt.Errorf("research task %s claim %d is missing id", S(task, "id"), i)
			}
			if directive == "" {
				return nil, fmt.Errorf("research task %s claim %s is missing planner_directive", S(task, "id"), id)
			}
			refs = append(refs, rawRef{
				TaskID: S(task, "id"), ClaimID: id, PlannerDirective: directive,
				NormativeLevel: strings.ToUpper(strings.TrimSpace(S(claim, "normative_level"))),
				Authority:      strings.TrimSpace(S(claim, "authority")), Source: strings.TrimSpace(S(claim, "source")),
			})
			counts[id]++
		}
	}
	out := make([]ResearchClaimConstraint, 0, len(refs))
	for _, ref := range refs {
		claimRef := ref.ClaimID
		if counts[ref.ClaimID] > 1 {
			claimRef = ref.TaskID + ":" + ref.ClaimID
		}
		out = append(out, ResearchClaimConstraint{
			TaskID: ref.TaskID, ClaimID: ref.ClaimID, ClaimRef: claimRef,
			PlannerDirective: ref.PlannerDirective, NormativeLevel: ref.NormativeLevel,
			Authority: ref.Authority, Source: ref.Source,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ClaimRef < out[j].ClaimRef })
	return out, nil
}

func ResearchClaimIDs(tasks Rows) ([]string, error) {
	constraints, err := ResearchClaimConstraints(tasks)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(constraints))
	for _, constraint := range constraints {
		ids = append(ids, constraint.ClaimRef)
	}
	return ids, nil
}

// ResearchExceptionDecisionIDs returns durable decision ids referenced by
// structured research claims that deliberately take an exception to evidence.
func ResearchExceptionDecisionIDs(result Row) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, raw := range Slice(result, "claims") {
		var claim Row
		switch x := raw.(type) {
		case map[string]any:
			claim = Row(x)
		case Row:
			claim = x
		default:
			continue
		}
		if strings.ToLower(strings.TrimSpace(S(claim, "directive_effect"))) != "exception" {
			continue
		}
		id := strings.TrimSpace(S(claim, "exception_decision_id"))
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

func ResearchConsumerExceptionDecisionIDs(result Row) []string {
	consistency := M(result, "research_consistency")
	seen := map[string]bool{}
	out := []string{}
	for _, raw := range Slice(consistency, "claims") {
		var item Row
		switch x := raw.(type) {
		case map[string]any:
			item = Row(x)
		case Row:
			item = x
		default:
			continue
		}
		if strings.ToLower(strings.TrimSpace(S(item, "disposition"))) != "explicit_exception" {
			continue
		}
		id := strings.TrimSpace(S(item, "exception_decision_id"))
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// ValidateResearchConsumerResult makes normative research directives inherited
// constraints rather than advisory prose. Every research claim must be mapped
// to a disposition and must repeat the canonical inherited directive verbatim
// (modulo whitespace/case normalization). Downstream roles may preserve or
// strengthen it, or take an explicit durable exception; they may not silently
// replace the research directive with a contradictory derived instruction.
func ValidateResearchConsumerResult(result Row, researchTasks Rows, requirePass bool) error {
	constraints, err := ResearchClaimConstraints(researchTasks)
	if err != nil || len(constraints) == 0 {
		return err
	}
	consistency := M(result, "research_consistency")
	if len(consistency) == 0 {
		return fmt.Errorf("result requires research_consistency for fresh research evidence")
	}
	verdict := strings.ToLower(strings.TrimSpace(S(consistency, "verdict")))
	if verdict != "pass" && verdict != "fail" {
		return fmt.Errorf("research_consistency.verdict must be pass or fail")
	}

	entries := Slice(consistency, "claims")
	if len(entries) == 0 {
		return fmt.Errorf("research_consistency.claims must map every structured research claim to an inherited directive disposition")
	}
	byRef := map[string]Row{}
	for i, raw := range entries {
		var item Row
		switch x := raw.(type) {
		case map[string]any:
			item = Row(x)
		case Row:
			item = x
		default:
			return fmt.Errorf("research_consistency.claims[%d] must be an object", i)
		}
		claimRef := strings.TrimSpace(S(item, "claim_id"))
		if claimRef == "" {
			return fmt.Errorf("research_consistency.claims[%d] requires claim_id", i)
		}
		if byRef[claimRef] != nil {
			return fmt.Errorf("research_consistency claim_id %q is duplicated", claimRef)
		}
		byRef[claimRef] = item
	}

	expected := map[string]ResearchClaimConstraint{}
	for _, constraint := range constraints {
		expected[constraint.ClaimRef] = constraint
	}
	missing := []string{}
	for claimRef := range expected {
		if byRef[claimRef] == nil {
			missing = append(missing, claimRef)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		return fmt.Errorf("research_consistency did not disposition claim ids: %s", strings.Join(missing, ","))
	}
	unknown := []string{}
	for claimRef := range byRef {
		if _, ok := expected[claimRef]; !ok {
			unknown = append(unknown, claimRef)
		}
	}
	sort.Strings(unknown)
	if len(unknown) > 0 {
		return fmt.Errorf("research_consistency references unknown claim ids: %s", strings.Join(unknown, ","))
	}

	allowedDisposition := map[string]bool{"preserved": true, "strengthened": true, "explicit_exception": true}
	for claimRef, constraint := range expected {
		item := byRef[claimRef]
		disposition := strings.ToLower(strings.TrimSpace(S(item, "disposition")))
		if !allowedDisposition[disposition] {
			return fmt.Errorf("research_consistency claim %s disposition must be preserved, strengthened, or explicit_exception", claimRef)
		}
		inherited := strings.TrimSpace(S(item, "inherited_directive"))
		if NormalizeText(inherited) != NormalizeText(constraint.PlannerDirective) {
			return fmt.Errorf("research_consistency claim %s must preserve the exact inherited research directive", claimRef)
		}
		for _, raw := range Slice(item, "derived_directives") {
			if strings.TrimSpace(researchFieldString(raw)) == "" {
				return fmt.Errorf("research_consistency claim %s derived_directives must contain only non-empty strings", claimRef)
			}
		}
		decisionID := strings.TrimSpace(S(item, "exception_decision_id"))
		if disposition == "explicit_exception" && decisionID == "" {
			return fmt.Errorf("research_consistency claim %s explicit_exception requires exception_decision_id", claimRef)
		}
		if disposition != "explicit_exception" && decisionID != "" {
			return fmt.Errorf("research_consistency claim %s may cite exception_decision_id only for explicit_exception", claimRef)
		}
	}

	checkedRaw, hasChecked := consistency["checked_claim_ids"]
	if hasChecked && checkedRaw != nil {
		checked := map[string]bool{}
		for _, raw := range Slice(consistency, "checked_claim_ids") {
			id := strings.TrimSpace(researchFieldString(raw))
			if id != "" {
				checked[id] = true
			}
		}
		missingChecked := []string{}
		for claimRef := range expected {
			if !checked[claimRef] {
				missingChecked = append(missingChecked, claimRef)
			}
		}
		sort.Strings(missingChecked)
		if len(missingChecked) > 0 {
			return fmt.Errorf("research_consistency did not check claim ids: %s", strings.Join(missingChecked, ","))
		}
	}

	contradictions := Slice(consistency, "contradictions")
	if verdict == "fail" && len(contradictions) == 0 {
		return fmt.Errorf("failed research_consistency requires contradictions")
	}
	if verdict == "pass" && len(contradictions) > 0 {
		return fmt.Errorf("passing research_consistency cannot report contradictions")
	}
	if requirePass && verdict != "pass" {
		return fmt.Errorf("passing plan/review requires research_consistency verdict=pass")
	}
	return nil
}

func ValidateExternalConformanceReviewResult(result Row) error {
	if strings.ToLower(strings.TrimSpace(S(result, "verdict"))) != "pass" {
		return nil
	}
	for _, raw := range Slice(result, "verification_oracles") {
		var oracle Row
		switch x := raw.(type) {
		case map[string]any:
			oracle = Row(x)
		case Row:
			oracle = x
		default:
			continue
		}
		kind := strings.ToLower(strings.TrimSpace(S(oracle, "kind")))
		used := true
		if oracle["used"] != nil {
			used = B(oracle, "used")
		}
		if used && (kind == "independent_implementation" || kind == "official_reference_oracle" || kind == "live_provider_oracle") {
			return nil
		}
	}
	if strings.TrimSpace(S(result, "authoritative_oracle_unavailable_reason")) != "" && strings.TrimSpace(S(result, "oracle_exception_decision_id")) != "" {
		if err := validateOracleExceptionProbes(result); err != nil {
			return err
		}
		return nil
	}
	return fmt.Errorf("passing external-conformance review requires an authoritative oracle actually used, or a durable oracle exception decision")
}

// validateOracleExceptionProbes requires a durable oracle-exception decision
// to name the concrete probes that replace the missing authoritative oracle.
// A documented exception may discharge the oracle requirement only when each
// probe records the scenario, the command or test that exercised it, and the
// observed outcome. An internal live-loopback regression is an acceptable
// probe; an out-of-process same-interpretation client must not be relabelled
// as authoritative conformance.
func validateOracleExceptionProbes(result Row) error {
	probes := Slice(result, "probes")
	if len(probes) == 0 {
		return fmt.Errorf("oracle-exception review requires a non-empty probes list naming the scenario, command_or_test, and observed outcome that replace the missing authoritative oracle")
	}
	for i, raw := range probes {
		var probe Row
		switch x := raw.(type) {
		case map[string]any:
			probe = Row(x)
		case Row:
			probe = x
		default:
			return fmt.Errorf("probes[%d] must be an object", i)
		}
		if strings.TrimSpace(researchFieldString(probe["scenario"])) == "" {
			return fmt.Errorf("probes[%d] requires a non-empty scenario", i)
		}
		if strings.TrimSpace(researchFieldString(probe["command_or_test"])) == "" {
			return fmt.Errorf("probes[%d] requires a non-empty command_or_test", i)
		}
		if strings.TrimSpace(researchFieldString(probe["observed_outcome"])) == "" {
			return fmt.Errorf("probes[%d] requires a non-empty observed_outcome", i)
		}
	}
	return nil
}

// InheritedDirective is one normative directive harvested from a completed
// planning-stage ancestor (critic, plan_review, or advise). Directives stay
// inside the source task's result_json; there is no directive table. The
// stable ID is <source-task-id>:<kind>:<ordinal-in-source-array>, derived
// purely from persisted result order so repeated reads re-match prior
// dispositions and cannot collide with research claim references.
type InheritedDirective struct {
	TaskID         string
	SourceProfile  string
	Kind           string
	ID             string
	Text           string
	NormativeLevel string
}

// directiveSourceProfiles are the completed planning-stage profiles whose
// results are harvested for directives. Research-claim planner directives are
// NOT harvested here because they are already governed by the
// research_consistency mechanism.
var directiveSourceProfiles = map[string]bool{"critic": true, "plan_review": true, "advise": true}

// InheritedDirectives harvests directives from completed planning-stage
// ancestor results: critic planner_directives, critic/advise qa_directives,
// critic/advise risks[].mitigation, and advise recommendations. Advise
// tradeoffs, revisit_triggers, and unresolved items are intentionally not
// harvested.
func InheritedDirectives(tasks Rows) []InheritedDirective {
	out := []InheritedDirective{}
	for _, task := range tasks {
		profile := S(task, "profile")
		if !directiveSourceProfiles[profile] || S(task, "status") != TaskComplete {
			continue
		}
		result := M(task, "result")
		if len(result) == 0 && strings.TrimSpace(S(task, "result_json")) != "" {
			result = JSONMap(S(task, "result_json"))
		}
		if len(result) == 0 {
			continue
		}
		appendDirective := func(kind string, ordinal int, raw any) {
			text, level := directiveTextAndLevel(raw)
			if strings.TrimSpace(text) == "" {
				return
			}
			out = append(out, InheritedDirective{
				TaskID: S(task, "id"), SourceProfile: profile, Kind: kind,
				ID:             fmt.Sprintf("%s:%s:%d", S(task, "id"), kind, ordinal),
				Text:           strings.TrimSpace(text),
				NormativeLevel: strings.ToUpper(strings.TrimSpace(level)),
			})
		}
		if profile == "critic" || profile == "plan_review" {
			for i, raw := range Slice(result, "planner_directives") {
				appendDirective("planner_directive", i, raw)
			}
		}
		for i, raw := range Slice(result, "qa_directives") {
			appendDirective("qa_directive", i, raw)
		}
		for i, raw := range Slice(result, "risks") {
			var risk Row
			switch x := raw.(type) {
			case map[string]any:
				risk = Row(x)
			case Row:
				risk = x
			default:
				continue
			}
			appendDirective("risk_mitigation", i, risk["mitigation"])
		}
		if profile == "advise" {
			for i, raw := range Slice(result, "recommendations") {
				appendDirective("recommendation", i, raw)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func directiveTextAndLevel(raw any) (string, string) {
	switch x := raw.(type) {
	case string:
		return x, directiveLevelFromText(x)
	case map[string]any:
		return directiveFieldRow(Row(x)), directiveFieldRowLevel(Row(x))
	case Row:
		return directiveFieldRow(x), directiveFieldRowLevel(x)
	default:
		return researchFieldString(raw), ""
	}
}

// directiveLevelFromText derives a normative level from the plain-string form
// critics actually emit, recognizing only a leading `MUST NOT:`/`MUST:`/
// `SHOULD:` prefix (never a mid-sentence occurrence) and matching
// `MUST NOT` before `MUST`. It is scoped to the inherited-directive harvest
// path; research-claim normative levels keep their separate structured path.
func directiveLevelFromText(text string) string {
	t := strings.ToUpper(strings.TrimSpace(text))
	for _, p := range []string{"MUST NOT", "MUST", "SHOULD"} {
		if strings.HasPrefix(t, p) {
			if rest := strings.TrimSpace(t[len(p):]); strings.HasPrefix(rest, ":") {
				return p
			}
		}
	}
	return ""
}

// mustLevel reports whether a normative level belongs to the MUST family,
// which is never deferrable.
func mustLevel(level string) bool {
	switch strings.ToUpper(strings.TrimSpace(level)) {
	case "MUST", "MUST NOT":
		return true
	}
	return false
}

func directiveFieldRow(r Row) string {
	for _, key := range []string{"text", "directive", "mitigation", "statement", "recommendation"} {
		if v := strings.TrimSpace(researchFieldString(r[key])); v != "" {
			return v
		}
	}
	return ""
}

func directiveFieldRowLevel(r Row) string {
	return strings.TrimSpace(researchFieldString(r["normative_level"]))
}

// LegacyGateTraceabilityMarker is the leading text a durable decision
// statement must carry to exempt a pre-existing collection from retroactive
// directive-traceability and impact-enumeration enforcement at the collection
// read/acceptance seams. It is the documented single-decision escape hatch
// for history that predates these validators; it never applies to a new
// result submission.
const LegacyGateTraceabilityMarker = "Legacy gate-traceability exception:"

// LegacyGateTraceabilityExceptionIDs returns durable decision ids that
// qualify as the documented legacy gate-traceability escape hatch. A decision
// qualifies only when it is collection-scoped (its collection_id is exactly
// this collection), it is a standing operator exception rather than a
// worker/task-scoped one (task_id is NULL), and its statement both begins with
// the legacy marker and names this collection so the exemption cannot be reused
// across collections.
func LegacyGateTraceabilityExceptionIDs(decisions Rows, collectionID string) []string {
	out := []string{}
	for _, d := range decisions {
		statement := strings.TrimSpace(S(d, "statement"))
		if !strings.HasPrefix(statement, LegacyGateTraceabilityMarker) {
			continue
		}
		// A standing operator exception is collection-scoped and never tied to
		// a single task: task_id must be NULL and collection_id must be exactly
		// this collection, so a project-wide or foreign-collection or worker-
		// authored marker cannot arm the escape.
		if strings.TrimSpace(S(d, "task_id")) != "" {
			continue
		}
		if collectionID == "" || strings.TrimSpace(S(d, "collection_id")) != collectionID {
			continue
		}
		// Belt and braces: the statement must itself name this collection so a
		// mis-scoped row cannot silently exempt it.
		if !strings.Contains(statement, collectionID) {
			continue
		}
		out = append(out, S(d, "id"))
	}
	sort.Strings(out)
	return out
}

// ValidateDirectiveTraceability makes inherited planning directives enforced
// rather than advisory. It is shaped like ValidateResearchConsumerResult:
// a passing review or plan_review must disposition every inherited directive
// as satisfied, deferred, or explicit_exception with concrete evidence.
// deferred and explicit_exception require a durable decision id from the
// decisions table. Directives the result reports that are not in the
// inherited set (additions) must be reported explicitly and are never
// silently accepted as satisfied. When the inherited set is empty the result
// passes cleanly. legacyExceptionIDs enable the documented single-decision
// escape hatch for pre-existing history at the read/acceptance seams.
func ValidateDirectiveTraceability(result Row, directives []InheritedDirective, requirePass bool, legacyExceptionIDs []string) error {
	if len(directives) == 0 {
		return nil
	}
	if requirePass && strings.ToLower(strings.TrimSpace(S(result, "verdict"))) != "pass" {
		return nil
	}
	if len(legacyExceptionIDs) > 0 {
		return nil
	}
	entries := Slice(result, "directive_dispositions")
	byID := map[string]Row{}
	for i, raw := range entries {
		var item Row
		switch x := raw.(type) {
		case map[string]any:
			item = Row(x)
		case Row:
			item = x
		default:
			return fmt.Errorf("directive_dispositions[%d] must be an object", i)
		}
		id := strings.TrimSpace(S(item, "directive_id"))
		if id == "" {
			return fmt.Errorf("directive_dispositions[%d] requires directive_id", i)
		}
		if byID[id] != nil {
			return fmt.Errorf("directive_dispositions directive_id %q is duplicated", id)
		}
		byID[id] = item
	}
	expected := map[string]InheritedDirective{}
	for _, directive := range directives {
		expected[directive.ID] = directive
	}
	missing := []string{}
	for id := range expected {
		if byID[id] == nil {
			missing = append(missing, id)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		return fmt.Errorf("directive_dispositions did not disposition directive ids: %s", strings.Join(missing, ","))
	}
	unknown := []string{}
	for id := range byID {
		if _, ok := expected[id]; !ok {
			unknown = append(unknown, id)
		}
	}
	sort.Strings(unknown)
	if len(unknown) > 0 {
		return fmt.Errorf("directive_dispositions references unknown directive ids (report genuinely new directives under additions, not dispositions): %s", strings.Join(unknown, ","))
	}
	allowedDisposition := map[string]bool{"satisfied": true, "deferred": true, "explicit_exception": true}
	for _, id := range sortedDirectiveKeys(byID) {
		item := byID[id]
		disposition := strings.ToLower(strings.TrimSpace(S(item, "disposition")))
		if !allowedDisposition[disposition] {
			return fmt.Errorf("directive_dispositions %s disposition must be satisfied, deferred, or explicit_exception", id)
		}
		if !nonempty(item["evidence"]) {
			return fmt.Errorf("directive_dispositions %s requires concrete evidence", id)
		}
		if e := citableEvidenceText(item["evidence"]); e != "" {
			return fmt.Errorf("directive_dispositions %s requires citable evidence (a changed file path, a test name, or a command run); %s", id, e)
		}
		decisionID := strings.TrimSpace(S(item, "decision_id"))
		if (disposition == "deferred" || disposition == "explicit_exception") && decisionID == "" {
			return fmt.Errorf("directive_dispositions %s %s requires a durable decision_id", id, disposition)
		}
		if disposition == "satisfied" && decisionID != "" {
			return fmt.Errorf("directive_dispositions %s may cite decision_id only for deferred or explicit_exception", id)
		}
		if disposition == "deferred" && mustLevel(expected[id].NormativeLevel) {
			return fmt.Errorf("directive_dispositions %s cannot defer a MUST-level directive; satisfy it or cite an explicit_exception decision_id", id)
		}
	}
	for i, raw := range Slice(result, "additions") {
		var item Row
		switch x := raw.(type) {
		case map[string]any:
			item = Row(x)
		case Row:
			item = x
		default:
			return fmt.Errorf("additions[%d] must be an object", i)
		}
		if strings.TrimSpace(researchFieldString(item["directive"])) == "" {
			return fmt.Errorf("additions[%d] requires a non-empty directive", i)
		}
		if strings.TrimSpace(researchFieldString(item["reason"])) == "" {
			return fmt.Errorf("additions[%d] requires a non-empty reason explaining why it is not inherited coverage", i)
		}
	}
	return nil
}

func sortedDirectiveKeys(byID map[string]Row) []string {
	keys := make([]string, 0, len(byID))
	for k := range byID {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ValidateReviewImpactEnumeration requires a passing review to carry the
// sibling-effect enumeration as a durable, auditable artifact: the field
// must exist, name the enumeration method, and give every listed site an
// explicit affected/unaffected disposition with citable evidence. This makes
// the enumeration falsifiable by a human reviewer, not provably complete. The
// introduced-knob rule is diff-scoped: a site the change introduced as a
// configuration key or flag must name the code path that reads it.
//
// derivedCandidates are configuration keys/flags the tooling derived from the
// diff itself, independent of whether the reviewer flagged any site with
// introduced_knob. Every one must be accounted for either by a site whose
// reader_paths names it or by a candidate_dispositions entry; a legacy
// exemption never waives this derived accounting, since it is a fact about the
// diff rather than a self-report. A nil/empty list keeps prior behavior.
func ValidateReviewImpactEnumeration(result Row, legacyExceptionIDs []string, derivedCandidates []DerivedConfigCandidate) error {
	if strings.ToLower(strings.TrimSpace(S(result, "verdict"))) != "pass" {
		return nil
	}
	enumeration := M(result, "impact_enumeration")
	if len(enumeration) == 0 {
		if err := validateDerivedCandidateAccounting(Row{}, derivedCandidates); err != nil {
			return err
		}
		if len(legacyExceptionIDs) > 0 {
			return nil
		}
		return fmt.Errorf("passing review requires impact_enumeration naming the enumeration method and an explicit disposition for every sibling-effect site")
	}
	if strings.TrimSpace(S(enumeration, "method")) == "" {
		return fmt.Errorf("impact_enumeration.method must name the enumeration method used (for example literal-column grep set, call-graph walk, or store-mutation sweep)")
	}
	sites := Slice(enumeration, "sites")
	if len(sites) == 0 && len(derivedCandidates) == 0 {
		return fmt.Errorf("impact_enumeration.sites must list every site performing the same effect as the changed code")
	}
	for i, raw := range sites {
		var site Row
		switch x := raw.(type) {
		case map[string]any:
			site = Row(x)
		case Row:
			site = x
		default:
			return fmt.Errorf("impact_enumeration.sites[%d] must be an object", i)
		}
		if strings.TrimSpace(researchFieldString(site["site"])) == "" {
			return fmt.Errorf("impact_enumeration.sites[%d] requires a non-empty site identifier", i)
		}
		disposition := strings.ToLower(strings.TrimSpace(researchFieldString(site["disposition"])))
		if disposition != "affected" && disposition != "unaffected" {
			return fmt.Errorf("impact_enumeration.sites[%d] disposition must be affected or unaffected", i)
		}
		if strings.TrimSpace(researchFieldString(site["evidence"])) == "" {
			return fmt.Errorf("impact_enumeration.sites[%d] requires evidence for its disposition", i)
		}
		if e := citableEvidenceText(site["evidence"]); e != "" {
			return fmt.Errorf("impact_enumeration.sites[%d] requires citable evidence (a changed file path, a test name, or a command run); %s", i, e)
		}
		if B(site, "introduced_knob") && len(Slice(site, "reader_paths")) == 0 {
			return fmt.Errorf("impact_enumeration.sites[%d] is a configuration key or flag introduced by this change with no reader_paths; an introduced-but-never-read knob is a blocking defect, not a style preference", i)
		}
	}
	return validateDerivedCandidateAccounting(enumeration, derivedCandidates)
}

// DerivedConfigCandidate is a configuration key or command-line flag that the
// tooling derives directly from a diff, independent of any worker self-report.
// Token is the knob text (for example a `--some-flag` or an env var name) and
// Path is the changed file it was found in.
type DerivedConfigCandidate struct {
	Token string
	Path  string
}

// Evidence-citation forms. A piece of evidence is citable only when it names a
// concrete artifact a human can independently check: a source file path, a
// test identifier, or a command actually run. Prose that merely asserts a
// conclusion without naming an artifact is not citable.
var (
	evidencePathLikeRe    = regexp.MustCompile(`[\w.\-]+/[\w./\-]+\.\w+|[\w\-]+\.(go|md|py|sql|json|ya?ml|toml|ts|tsx|js|jsx|c|h|rs|java|kt|sh|txt|csv)`)
	evidenceTestLikeRe    = regexp.MustCompile(`\bTest[A-Z]\w*|\btest_[a-z0-9_]+`)
	evidenceCommandLikeRe = regexp.MustCompile(`\b(go\s+(test|vet|run|build)|make\s+\S+|pytest\b|cargo\s+(test|run)|npm\s+(run|test)|git\s+(log|diff|show|status)|grep\s|ripgrep\b|rg\s)`)
	evidenceSettingWordRe = regexp.MustCompile(`--[a-z0-9][\w-]*|\b[A-Z][A-Z0-9_]{2,}\b`)
)

// evidenceGenericRe lists assertions that read like evidence but name no
// artifact; such text is rejected even when it incidentally contains one of the
// citable forms' stopwords.
var evidenceGenericRe = regexp.MustCompile(`(?i)^(?:n/?a\.?|none|verified( in code)?|confirmed|tested|as discussed|looks good|see code|it works?|done|checked)$`)

// evidenceText flattens an evidence field that is either a string or a list of
// strings into a single searchable string.
func evidenceText(raw any) string {
	switch x := raw.(type) {
	case string:
		return strings.TrimSpace(x)
	case []any:
		parts := []string{}
		for _, item := range x {
			if s := strings.TrimSpace(researchFieldString(item)); s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, " ; ")
	default:
		return strings.TrimSpace(researchFieldString(raw))
	}
}

// citableEvidence reports whether an evidence value names a concrete,
// independently checkable artifact.
func citableEvidence(raw any) bool {
	return citableEvidenceText(raw) == ""
}

// citableEvidenceText returns an empty string when the evidence is citable and
// a short reason otherwise. It is the falsifiable content floor behind both
// directive dispositions and impact-site enumerations.
func citableEvidenceText(raw any) string {
	text := evidenceText(raw)
	if text == "" {
		return "the evidence is empty"
	}
	if evidenceGenericRe.MatchString(strings.TrimSpace(text)) {
		return "it only asserts a conclusion without naming a file, test, or command"
	}
	joined := strings.ToLower(text)
	if evidencePathLikeRe.MatchString(text) || evidenceTestLikeRe.MatchString(text) || evidenceCommandLikeRe.MatchString(joined) || evidenceSettingWordRe.MatchString(text) {
		return ""
	}
	return "it does not name a changed file path, a test identifier, or a command run"
}

// candidateAccounted reports whether a derived configuration candidate is
// covered by the enumeration: either a site's reader_paths (or site text) names
// the token, or a candidate_dispositions entry dispositions it.
func candidateAccounted(enumeration Row, dispositions map[string]Row, candidate DerivedConfigCandidate) bool {
	token := strings.ToLower(strings.TrimSpace(candidate.Token))
	if token == "" {
		return true
	}
	if _, ok := dispositions[token]; ok {
		return true
	}
	for _, raw := range Slice(enumeration, "sites") {
		site := rowFromAny(raw)
		if site == nil {
			continue
		}
		blob := strings.ToLower(researchFieldString(site["site"]))
		for _, rp := range Slice(site, "reader_paths") {
			blob += " " + strings.ToLower(researchFieldString(rp))
		}
		if strings.Contains(blob, token) {
			return true
		}
	}
	return false
}

// candidateRequiresDecision reports the dispositions whose accounting must be
// backed by a durable decision id: a deliberately-unread or explicitly-
// excepted knob is a decision, not an observation.
func candidateRequiresDecision(disposition string) bool {
	switch strings.ToLower(strings.TrimSpace(disposition)) {
	case "deliberately-unread", "explicit-exception", "explicit_exception":
		return true
	}
	return false
}

// validateDerivedCandidateAccounting requires that every candidate derived
// from the diff be accounted for by the enumeration, and that any candidate
// disposition which asserts a deliberate decision cites a durable decision id.
// It is skipped entirely for a nil/empty candidate list so the pre-existing
// behavior is byte-identical.
func validateDerivedCandidateAccounting(enumeration Row, derivedCandidates []DerivedConfigCandidate) error {
	if len(derivedCandidates) == 0 {
		return nil
	}
	dispositions := map[string]Row{}
	for _, raw := range Slice(enumeration, "candidate_dispositions") {
		item := rowFromAny(raw)
		if item == nil {
			continue
		}
		token := strings.ToLower(strings.TrimSpace(researchFieldString(item["candidate"])))
		if token == "" {
			return fmt.Errorf("candidate_dispositions entry requires a non-empty candidate token")
		}
		if strings.TrimSpace(researchFieldString(item["disposition"])) == "" {
			return fmt.Errorf("candidate_dispositions for %s requires a disposition", item["candidate"])
		}
		if candidateRequiresDecision(researchFieldString(item["disposition"])) && strings.TrimSpace(researchFieldString(item["decision_id"])) == "" {
			return fmt.Errorf("candidate_dispositions for %s requires a durable decision_id", item["candidate"])
		}
		dispositions[token] = item
	}
	for _, candidate := range derivedCandidates {
		if !candidateAccounted(enumeration, dispositions, candidate) {
			return fmt.Errorf("impact_enumeration does not account for the derived configuration candidate %s in %s; mark the site as introduced_knob with reader_paths, or disposition it under candidate_dispositions", candidate.Token, candidate.Path)
		}
	}
	return nil
}

// CandidateDecisionIDs returns the durable decision ids cited by a result's
// candidate_dispositions so the submit/acceptance seams can verify they exist.
func CandidateDecisionIDs(result Row) []string {
	enumeration := M(result, "impact_enumeration")
	seen := map[string]bool{}
	out := []string{}
	for _, raw := range Slice(enumeration, "candidate_dispositions") {
		item := rowFromAny(raw)
		if item == nil {
			continue
		}
		id := strings.TrimSpace(researchFieldString(item["decision_id"]))
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// rowFromAny coerces a decoded JSON value into a Row, returning nil when it is
// not an object.
func rowFromAny(raw any) Row {
	switch x := raw.(type) {
	case Row:
		return x
	case map[string]any:
		return Row(x)
	default:
		return nil
	}
}
