package praector

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"sort"
	"strings"
)

type PlanningPolicy struct {
	CriticRequired, PlanReviewRequired bool
	Categories, Reasons                []string
}
type DeliveryPolicy struct {
	ImplementationRequired bool
	Categories, Reasons    []string
}

func (p PlanningPolicy) AsRow() Row {
	return Row{"critic_required": p.CriticRequired, "plan_review_required": p.PlanReviewRequired, "categories": p.Categories, "reasons": p.Reasons}
}
func (p DeliveryPolicy) AsRow() Row {
	return Row{"implementation_required": p.ImplementationRequired, "categories": p.Categories, "reasons": p.Reasons}
}

var securityPatterns = []string{`\bauth(?:entication|orization)?\b`, `\bapi[-_ ]?key(?:s)?\b`, `\bcredential(?:s)?\b`, `\bsecret(?:s)?\b`, `\bpassword(?:s)?\b`, `\boauth\b`, `\btoken(?:s)?\b`, `\baccess[ -]?control\b`, `\bpermission(?:s)?\b`, `\bencrypt(?:ion|ed)?\b`, `\bsecurity\b`}
var statefulPatterns = []string{`\brate[ -]?limit(?:ing)?\b`, `\bstateful\b`, `\bsession(?:s)?\b`, `\bcache(?:d|s|ing)?\b`, `\bqueue(?:s)?\b`, `\bretr(?:y|ies)\b`, `\bidempoten(?:t|cy)\b`, `\bconcurren(?:t|cy)\b`, `\block(?:ing|s)?\b`, `\btransaction(?:s)?\b`}
var migrationPatterns = []string{`\bmigrat(?:e|ion|ions)\b`, `\bschema\b`, `\bdatabase\b`, `\bbackfill\b`, `\bdata conversion\b`}
var destructivePatterns = []string{`\bdelete\b`, `\bdrop\b`, `\bpurge\b`, `\bdestructive\b`, `\birreversible\b`}
var externalContractPatterns = []string{`\bapi\b`, `\bwebhook(?:s)?\b`, `\bpublic endpoint\b`, `\bbackward compatib(?:le|ility)\b`}
var externalIntegrationPatterns = []string{`\bgateway(?:s)?\b`, `\bintegration(?:s)?\b`, `\bthird[- ]party\b`, `\bexternal service\b`, `\bprovider(?:s)?\b`, `\bsdk\b`, `\btwilio\b`, `\btelephony\b`, `\btwiml\b`}

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
	if implementation && !trivial {
		cats = append(cats, "implementation_change")
		reasons = append(reasons, "Non-trivial implementation work requires an independent plan-review gate before write work.")
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
	integrationSensitive := implementation && (extContract || extIntegration)
	critic := security || migration || destructive || (stateful && extContract) || integrationSensitive
	plan := critic || stateful || migration || destructive || (implementation && !trivial)
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
	return PlanningPolicy{critic, plan, cats, reasons}
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

func MaterialPlanFingerprint(objective string, tasks Rows, edges Rows, legacy bool) string {
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
		tp = append(tp, Row{"id": S(t, "id"), "profile": S(t, "profile"), "title": NormalizeText(S(t, "title")), "objective": NormalizeText(S(t, "objective")), "reason": NormalizeText(S(t, "reason")), "workspace_mode": wm, "base_commit": base})
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
