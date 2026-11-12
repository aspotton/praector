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
		return nil
	}
	return fmt.Errorf("passing external-conformance review requires an authoritative oracle actually used, or a durable oracle exception decision")
}
