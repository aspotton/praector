package praector

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/datascape/praector/internal/assets"
)

type profileGuide struct{ Purpose, Own, Delegate string }

var profileGuidance = map[string]profileGuide{
	"coordinator": {"Produce/refine a decision-complete durable work graph and own graph-level reasoning.", "Consume exploration/critic evidence, resolve planning assumptions, decompose work, and define dependencies/verification.", "Repository exploration, substantive research, implementation, independent plan review, implementation review, or specialist consultation."},
	"explore":     {"Map the existing repository/system surface relevant to the Collection objective before planning.", "Inspect relevant files, modules, tests, configuration, runtime boundaries, callers/dependencies, existing patterns, and documentation; return evidence-grounded current-state context.", "Solution design, architecture preference, implementation, graph mutation, or independent review."},
	"critic":      {"Challenge the request before planning by surfacing hidden intent, ambiguity, scope hazards, and verification gaps.", "Intent classification, evidence-grounded gap analysis, planner directives, risk/assumption analysis, and QA directives.", "Implementation or graph mutation. Request bounded research only when decisive evidence is genuinely missing."},
	"plan_review": {"Independently determine whether the proposed durable work graph is executable without blocking gaps.", "Reference checks, task executability, dependency coherence, scope coherence, and verification-plan review.", "Plan repair or implementation. Return a verdict; Praector/coordinator handles graph repair."},
	"research":    {"Resolve a bounded factual uncertainty with current, verifiable evidence when freshness matters.", "Authoritative source retrieval, version/date scoping, claim-to-directive traceability, oracle classification, and explicit uncertainty/contradiction reporting.", "Implementation, architecture changes, or pretending model memory is current evidence when fresh retrieval is required."},
	"implement":   {"Make the bounded code/file change assigned by this task and prove it works.", "Inspect relevant code, implement the requested change, run tests/checks, and debug your own implementation.", "External research, architecture decisions, independent review, or materially separate implementation scope."},
	"review":      {"Independently verify completed implementation against its objective and acceptance intent.", "Inspect integrated code/diffs, run appropriate checks/tests/smoke tests, identify material defects, and determine pass/fail.", "Repairs. If defects require changes, return fail; Praector creates/resumes bounded implementation repair work."},
	"advise":      {"Provide deep technical judgment and one concrete recommendation without owning implementation.", "Reason about the bounded question, tradeoffs, risks, assumptions, confidence, and recommendation.", "Implementation or review. Request bounded research only when a decisive factual gap remains."},
}

var availableProfiles = map[string]string{
	"explore":     "Use for broad repository/system discovery before planning: what exists, how it connects, and what constraints matter.",
	"critic":      "Use for pre-planning gap analysis: hidden intent, ambiguity, scope hazards, assumptions, and QA directives.",
	"plan_review": "Use to review whether a proposed graph/task plan is executable before write work begins.",
	"research":    "Use for bounded authoritative evidence gathering; freshness-sensitive work must use current external retrieval and structured claim/source/oracle evidence.",
	"implement":   "Use when an actual code/file change must be produced.",
	"review":      "Use for independent post-implementation verification and pass/fail judgment.",
	"advise":      "Use for difficult conceptual/architectural judgment and a concrete recommendation.",
}

var resultKeys = []string{"verdict", "recommendation", "confidence", "intent_classification", "planner_directives", "qa_directives", "assumptions", "findings", "evidence", "artifacts", "risks", "unresolved", "as_of", "sources", "claims", "verification_oracles", "research_consistency", "authoritative_oracle_unavailable_reason", "oracle_exception_decision_id", "research_required", "explore_required", "directive_dispositions", "additions", "impact_enumeration", "probes"}

const readOnlyCapabilityPrompt = "This task is read-only. Editor permissions are denied, but shell commands, tests, imports, build tools, and smoke checks can still create or modify repository files. Treat **no net workspace mutation** as part of the task's completion contract.\n\n" +
	"- At the start of the attempt, capture the worktree state (normally `git status --porcelain=v1 --untracked-files=all`) so you know the baseline.\n" +
	"- Prefer verification modes that avoid repository writes when they do not change the behavior being tested. Examples include `PYTHONDONTWRITEBYTECODE=1` for ordinary Python verification where bytecode caches are irrelevant, or directing tool caches/temporary output outside the worktree when the tool supports it.\n" +
	"- Preserve the assigned detached HEAD and its history. Do not create/switch branches, check out another commit, commit, or run reset operations that move HEAD, even if you intend to switch back; inspect other refs with non-mutating commands such as `git show`.\n" +
	"- After tests/smokes, inspect the worktree again. Restore only incidental tracked changes caused by your own verification and remove only new untracked artifacts that your commands created (for example bytecode caches, coverage output, test caches, or temporary build products).\n" +
	"- Never discard pre-existing changes, committed candidate content, or another task's work merely to make `git status` clean. If cleanup cannot be done safely and precisely, stop and let Praector's capability check reject/retry the attempt rather than mutating the candidate.\n" +
	"- Before submitting the result, confirm the final worktree state matches the starting baseline. A semantic `verdict` is about the plan/implementation; a dirty read-only worktree is a separate capability failure and can invalidate the attempt."

func capabilityPrompt(profile string) string {
	if ReadOnlyProfiles[profile] {
		return readOnlyCapabilityPrompt
	}
	return "This task has write capability within its assigned implementation scope."
}

func promptText(name string) (string, error) {
	b, err := assets.FS.ReadFile("prompts/" + name + ".md")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func nonempty(v any) bool {
	if v == nil {
		return false
	}
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x) != ""
	case []any:
		return len(x) != 0
	case []string:
		return len(x) != 0
	case map[string]any:
		return len(x) != 0
	case Row:
		return len(x) != 0
	default:
		return true
	}
}

func resultSummary(row Row) Row {
	result := M(row, "result")
	if len(result) == 0 && strings.TrimSpace(S(row, "result_json")) != "" {
		result = JSONMap(S(row, "result_json"))
	}
	out := Row{"id": S(row, "id"), "profile": S(row, "profile"), "title": S(row, "title"), "status": S(row, "status"), "objective": S(row, "objective"), "summary": S(result, "summary")}
	for _, key := range resultKeys {
		if result != nil && nonempty(result[key]) {
			out[key] = result[key]
		}
	}
	return out
}

func inheritedResearchConstraints(deps Rows) ([]any, error) {
	constraints, err := ResearchClaimConstraints(deps)
	if err != nil {
		return nil, err
	}
	out := make([]any, 0, len(constraints))
	for _, constraint := range constraints {
		out = append(out, Row{
			"claim_id": constraint.ClaimRef, "research_task_id": constraint.TaskID,
			"normative_level": constraint.NormativeLevel, "authority": constraint.Authority,
			"source": constraint.Source, "planner_directive": constraint.PlannerDirective,
		})
	}
	return out, nil
}

// inheritedDirectiveProjection mirrors the research-constraint projection:
// a thin, stable view of planning directives harvested from completed
// critic/plan_review/advise ancestors. The projection is omitted entirely
// when the inherited set is empty so zero-directive tasks pay no envelope
// cost.
func inheritedDirectiveProjection(deps Rows) []any {
	directives := InheritedDirectives(deps)
	if len(directives) == 0 {
		return nil
	}
	out := make([]any, 0, len(directives))
	for _, directive := range directives {
		row := Row{
			"directive_id": directive.ID, "kind": directive.Kind, "text": directive.Text,
			"source_task_id": directive.TaskID, "source_profile": directive.SourceProfile,
		}
		if directive.NormativeLevel != "" {
			row["normative_level"] = directive.NormativeLevel
		}
		out = append(out, row)
	}
	return out
}

func graphSnapshot(store *Store, collectionID string) (Row, error) {
	tasks, err := store.ListTasks(collectionID)
	if err != nil {
		return nil, err
	}
	edges, err := store.ListEdges(collectionID)
	if err != nil {
		return nil, err
	}
	ts := []any{}
	for _, r := range tasks {
		ts = append(ts, Row{"id": S(r, "id"), "profile": S(r, "profile"), "status": S(r, "status"), "title": S(r, "title"), "objective": S(r, "objective"), "reason": S(r, "reason"), "depth": I(r, "depth")})
	}
	es := []any{}
	for _, r := range edges {
		es = append(es, Row{"from": S(r, "from_task_id"), "to": S(r, "to_task_id"), "relation": S(r, "relation")})
	}
	return Row{"tasks": ts, "edges": es}, nil
}

func BuildTaskEnvelope(store *Store, project, collection, task, attempt Row) (string, error) {
	deps, err := store.Dependencies(S(task, "id"))
	if err != nil {
		return "", err
	}
	parent, err := store.GetParent(task)
	if err != nil {
		return "", err
	}
	memories, err := store.ListMemories(S(project, "id"), S(collection, "id"), "")
	if err != nil {
		return "", err
	}
	decisions, err := store.ListDecisions(S(project, "id"), S(collection, "id"))
	if err != nil {
		return "", err
	}
	guide, ok := profileGuidance[S(task, "profile")]
	if !ok {
		return "", fmt.Errorf("unknown profile: %s", S(task, "profile"))
	}
	globalPrompt, err := promptText("global")
	if err != nil {
		return "", err
	}
	rolePrompt, err := promptText(S(task, "profile"))
	if err != nil {
		return "", err
	}
	planning := effectivePlanningPolicy(collection)
	delivery := ClassifyDeliveryPolicy(S(collection, "objective"))
	completed := []any{}
	for _, d := range deps {
		if S(d, "status") == TaskComplete {
			completed = append(completed, resultSummary(d))
		}
	}
	mems := []any{}
	for _, m := range memories {
		mems = append(mems, Row{"kind": S(m, "kind"), "text": S(m, "text"), "scope": S(m, "scope")})
	}
	decs := []any{}
	for _, d := range decisions {
		decs = append(decs, Row{"id": S(d, "id"), "statement": S(d, "statement"), "rationale": S(d, "rationale")})
	}
	researchAncestors, err := store.ResearchAncestors(S(task, "id"))
	if err != nil {
		return "", err
	}
	inheritedConstraints, err := inheritedResearchConstraints(researchAncestors)
	if err != nil {
		return "", err
	}
	ctx := Row{
		"project":                Row{"id": S(project, "id"), "name": S(project, "name"), "base_branch": S(project, "base_branch")},
		"workspace":              Row{"assigned_worktree": S(task, "worktree_path"), "mode": S(task, "workspace_mode"), "rule": "All repository/file/shell operations for this attempt must stay inside assigned_worktree."},
		"collection":             Row{"id": S(collection, "id"), "title": S(collection, "title"), "objective": S(collection, "objective"), "status": S(collection, "status")},
		"task":                   Row{"id": S(task, "id"), "profile": S(task, "profile"), "title": S(task, "title"), "objective": S(task, "objective"), "reason": S(task, "reason"), "workspace_mode": S(task, "workspace_mode")},
		"attempt":                Row{"id": S(attempt, "id"), "number": I(attempt, "attempt_no")},
		"completed_dependencies": completed, "project_and_collection_memory": mems, "active_decisions": decs,
		"inherited_research_constraints": inheritedConstraints,
		"planning_policy":                planning.AsRow(), "delivery_policy": delivery.AsRow(),
		"control_plane": Row{"command": "praector", "executable_env": "PRAECTOR_CLI", "rule": "Use `praector` for durable control-plane commands. Worker PATH contains a dedicated Praector shim even when the parent Python virtualenv is sanitized."},
	}
	directiveAncestors, err := store.DirectiveAncestors(S(task, "id"))
	if err != nil {
		return "", err
	}
	if inheritedDirectives := inheritedDirectiveProjection(directiveAncestors); len(inheritedDirectives) > 0 {
		ctx["inherited_directives"] = inheritedDirectives
	}
	if parent != nil {
		ctx["parent"] = resultSummary(parent)
	} else {
		ctx["parent"] = nil
	}
	if p := S(task, "profile"); p == "coordinator" || p == "critic" || p == "plan_review" {
		g, e := graphSnapshot(store, S(collection, "id"))
		if e != nil {
			return "", e
		}
		ctx["collection_graph"] = g
	}
	avail := map[string]string{}
	for k, v := range availableProfiles {
		avail[k] = v
	}
	if S(task, "profile") == "coordinator" {
		avail["coordinator"] = "Do not create another coordinator merely to delegate reasoning; you already own graph-level authority."
	}
	availableJSON, _ := json.MarshalIndent(avail, "", "  ")
	contextJSON, _ := json.MarshalIndent(ctx, "", "  ")
	capability := capabilityPrompt(S(task, "profile"))
	reviewNote := ""
	if S(task, "profile") == "review" {
		reviewNote = "For an implementation review, include `verdict: pass` or `verdict: fail`. A fail is kept out of acceptance and drives bounded repair work. A passing review also requires `impact_enumeration` (method + per-site dispositions with evidence) and, when `inherited_directives` are present, `directive_dispositions` (one per directive id: satisfied/deferred/explicit_exception with evidence; deferred/exception require a durable `decision_id`). Every evidence value must cite a file path, a test name, or a command; generic prose such as \"verified in code\" or \"n/a\" is rejected. A MUST-level directive (structured level or a leading `MUST:`/`MUST NOT:` prefix) may never be deferred, only satisfied or explicit_exception with a decision id. Praector derives candidate introduced configuration knobs from the collection diff; every derived candidate must be named by a site's reader_paths or given an `impact_enumeration.candidate_dispositions` entry (`not-introduced`, or `deliberately-unread` with a durable `decision_id`) — leaving `introduced_knob` unset does not opt out."
	} else if S(task, "profile") == "plan_review" {
		reviewNote = "For a plan review, include `verdict: pass` or `verdict: fail`. A fail remains blocking; Praector will wake a coordinator to repair the graph before this reviewer runs again. When `inherited_directives` are present, a passing verdict also requires `directive_dispositions` covering every directive id with citable evidence (a file path, a test name, or a command; deferred/exception require a durable `decision_id`). A MUST-level directive (structured level or a leading `MUST:`/`MUST NOT:` prefix) may never be deferred, only satisfied or explicit_exception with a decision id."
	}
	tmpl, err := assets.FS.ReadFile("envelope.md")
	if err != nil {
		return "", err
	}
	r := strings.NewReplacer(
		"{{GLOBAL_PROMPT}}", globalPrompt, "{{PROFILE}}", S(task, "profile"), "{{PURPOSE}}", guide.Purpose, "{{OWN}}", guide.Own, "{{DELEGATE}}", guide.Delegate,
		"{{CAPABILITY_PROMPT}}", capability, "{{ROLE_PROMPT}}", rolePrompt, "{{AVAILABLE_JSON}}", string(availableJSON), "{{REVIEW_NOTE}}", reviewNote, "{{CONTEXT_JSON}}", string(contextJSON),
	)
	return r.Replace(string(tmpl)), nil
}
