package praector

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/datascape/praector/internal/praector/mcp"
)

// mcpBridge adapts the shared typed Service layer to the dependency-light MCP
// core in internal/praector/mcp. It implements only the tool/resource provider
// interfaces, so every exposed MCP operation reuses the exact same durable Store
// reads, gated enqueue paths, and acceptance/PR checks the CLI uses
// (DEC-28fcd38435 / DEC-aacb7bfc23).
//
// Ownership boundary: this bridge NEVER constructs a scheduler and NEVER runs a
// collection inline. "Execute"-style actions enqueue durable work through the
// same CreateCollectionWithCoordinator / CreateTask Store paths the CLI's
// --detach path uses, and rely on the already-running daemon to execute it.
//
// The exposed surface is COMMAND-HELP DRIVEN: every commandHelp key maps to
// exactly one MCP item — a tool (for a leaf domain command) or a resource (for a
// command namespace / launch-only command) — through the shared serviceOps
// registry, giving command/surface coverage parity so the CLI and MCP surfaces
// cannot drift apart ("now and always"), rather than a claim of unrestricted
// behavioral parity. Privileged write tools (collection accept/pr/cleanup and
// explicit-root init/registration) are additionally
// gated behind the independent, default-off Privileged opt-in.
type mcpBridge struct {
	svc        *Service
	privileged bool
}

// objectSchema builds a JSON-Schema-shaped object descriptor.
func objectSchema(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

var (
	stringProp = map[string]any{"type": "string"}
	boolProp   = map[string]any{"type": "boolean"}
	arrayProp  = map[string]any{"type": "array", "items": stringProp}
	objProp    = map[string]any{"type": "object"}
)

// toolFunc maps raw JSON tool arguments onto a typed Service op and renders a
// text result. It may launch no process (enqueued work is executed by the daemon).
type toolFunc func(*Service, json.RawMessage) (string, error)

// resourceFunc renders the text contents of a resource from the Service layer.
type resourceFunc func(*Service) (string, error)

// mcpSurfaceEntry is the MCP projection of ONE commandHelp key: exactly one tool
// (opDomain leaf) or resource (opGroup namespace / opLaunchOnly command). The
// commandHelp-driven parity test asserts this map covers commandHelp 1:1.
type mcpSurfaceEntry struct {
	// tool surface (opDomain keys)
	toolName   string
	desc       string
	schema     map[string]any
	call       toolFunc
	privileged bool
	// resource surface (opGroup / opLaunchOnly keys)
	resourceURI  string
	resourceName string
	read         resourceFunc
}

// mcpSurface maps every commandHelp key to its single MCP projection. Handlers
// call exactly one typed Service op each and never launch a process.
var mcpSurface = map[string]mcpSurfaceEntry{
	// --- top-level commands ---
	"init": {toolName: "praector_init", privileged: true, desc: "Register the requested target repository as a Praector project. Requires an explicit root_path (never operates on the daemon cwd); privileged project registration.",
		schema: objectSchema(map[string]any{"root_path": stringProp, "name": stringProp, "base_branch": stringProp, "opencode": stringProp}, "root_path"),
		call: func(s *Service, raw json.RawMessage) (string, error) {
			var a struct {
				RootPath   string `json:"root_path"`
				Name       string `json:"name"`
				BaseBranch string `json:"base_branch"`
				OpenCode   string `json:"opencode"`
			}
			if err := bindArgs(raw, &a); err != nil {
				return "", err
			}
			if strings.TrimSpace(a.RootPath) == "" {
				return "", fmt.Errorf("root_path is required")
			}
			// Clone the Service so Init's internal store re-registration never
			// mutates the daemon's shared store connection.
			out, e := (&Service{store: s.store, cfg: s.cfg}).Init(context.Background(), InitInput{Root: a.RootPath, Name: a.Name, BaseBranch: a.BaseBranch, OpenCode: a.OpenCode})
			if e != nil {
				return "", e
			}
			return fmt.Sprintf("Initialized project %s (%s) base=%s opencode=%s marker=%s", out.Name, out.ProjectID, out.BaseBranch, out.OpenCodeCommand, out.Marker), nil
		}},
	"run": {toolName: "praector_run", desc: "Enqueue a new collection with its coordinator task (the running daemon executes it). Does not run inline.",
		schema: objectSchema(map[string]any{"objective": stringProp, "title": stringProp, "project": stringProp}, "objective"),
		call: func(s *Service, raw json.RawMessage) (string, error) {
			var a struct{ Objective, Title, Project string }
			if err := bindArgs(raw, &a); err != nil {
				return "", err
			}
			if a.Objective == "" {
				return "", fmt.Errorf("objective is required")
			}
			out, e := s.Run(context.Background(), RunInput{Objective: a.Objective, Title: a.Title, ProjectID: a.Project})
			if e != nil {
				return "", e
			}
			return fmt.Sprintf("Queued collection %s (coordinator %s). The running daemon executes pending work.", S(out.Collection, "id"), S(out.Coordinator, "id")), nil
		}},
	"status": {toolName: "praector_status", desc: "Show project, collection, or task status.",
		schema: objectSchema(map[string]any{"target": stringProp, "project": stringProp}),
		call: func(s *Service, raw json.RawMessage) (string, error) {
			var a struct{ Target, Project string }
			if err := bindArgs(raw, &a); err != nil {
				return "", err
			}
			out, e := s.Status(context.Background(), StatusInput{Target: a.Target, ProjectID: a.Project})
			if e != nil {
				return "", e
			}
			return MarshalPretty(out), nil
		}},
	"graph": {toolName: "praector_graph", desc: "Show the current durable task graph.",
		schema: objectSchema(map[string]any{"collection": stringProp, "project": stringProp}),
		call: func(s *Service, raw json.RawMessage) (string, error) {
			var a struct{ Collection, Project string }
			if err := bindArgs(raw, &a); err != nil {
				return "", err
			}
			out, e := s.Graph(context.Background(), GraphInput{Collection: a.Collection, ProjectID: a.Project})
			if e != nil {
				return "", e
			}
			return MarshalPretty(out), nil
		}},
	"daemon": {resourceURI: "praector://help/daemon", resourceName: "daemon (launch-only)", read: helpResource("daemon")},
	"doctor": {toolName: "praector_doctor", desc: "Validate local dependencies and project registration.",
		schema: objectSchema(map[string]any{}),
		call: func(s *Service, _ json.RawMessage) (string, error) {
			out, e := s.Doctor(context.Background(), DoctorInput{})
			if e != nil {
				return "", e
			}
			var b strings.Builder
			for _, c := range out.Checks {
				fmt.Fprintf(&b, "[%s] %s\n", c.Status, c.Text)
			}
			fmt.Fprintf(&b, "failures=%d", out.Failures)
			return b.String(), nil
		}},
	"logs": {toolName: "praector_logs", desc: "List attempt-log metadata for a task or collection.",
		schema: objectSchema(map[string]any{"target": stringProp}, "target"),
		call: func(s *Service, raw json.RawMessage) (string, error) {
			var a struct{ Target string }
			if err := bindArgs(raw, &a); err != nil {
				return "", err
			}
			out, e := s.Logs(context.Background(), LogsInput{Target: a.Target})
			if e != nil {
				return "", e
			}
			return MarshalPretty(out), nil
		}},

	// --- collection ---
	"collection":      {resourceURI: "praector://help/collection", resourceName: "collection commands", read: helpResource("collection")},
	"collection show": {toolName: "praector_collection_show", desc: "Show a collection including acceptance/revision history.", schema: collectionIDSchema(), call: collectionShowTool},
	"collection revise": {toolName: "praector_collection_revise", desc: "Reopen a completed, unaccepted collection for mandatory repair (the running daemon executes the new coordinator).", schema: objectSchema(map[string]any{"collection": stringProp, "reason": stringProp}, "collection", "reason"), call: func(s *Service, raw json.RawMessage) (string, error) {
		var a struct{ Collection, Reason string }
		if err := bindArgs(raw, &a); err != nil {
			return "", err
		}
		out, e := s.CollectionRevise(context.Background(), CollectionReviseInput{CollectionID: a.Collection, Reason: a.Reason})
		if e != nil {
			return "", e
		}
		return fmt.Sprintf("Revision %s opened for %s (coordinator %s). The running daemon executes pending work.", S(out.Revision, "id"), S(out.Collection, "id"), S(out.Coordinator, "id")), nil
	}},
	"collection diff": {toolName: "praector_collection_diff", desc: "Show collection changes relative to a target branch.",
		schema: objectSchema(map[string]any{"collection": stringProp, "target": stringProp, "stat": boolProp}, "collection"),
		call: func(s *Service, raw json.RawMessage) (string, error) {
			var a struct {
				Collection, Target string
				Stat               bool
			}
			if err := bindArgs(raw, &a); err != nil {
				return "", err
			}
			out, e := s.CollectionDiff(context.Background(), CollectionDiffInput{CollectionID: a.Collection, Target: a.Target, Stat: a.Stat})
			if e != nil {
				return "", e
			}
			return out.Stdout, nil
		}},
	"collection pr": {toolName: "praector_collection_pr", privileged: true, desc: "Validate a reviewed collection and build its pull-request plan (no push/merge; delivery stays operator-side).",
		schema: objectSchema(map[string]any{"collection": stringProp, "base": stringProp, "title": stringProp, "remote": stringProp, "draft": boolProp}, "collection"),
		call: func(s *Service, raw json.RawMessage) (string, error) {
			var a struct {
				Collection, Base, Title, Remote string
				Draft                           bool
			}
			if err := bindArgs(raw, &a); err != nil {
				return "", err
			}
			out, e := s.CollectionPRPlan(context.Background(), CollectionPRPlanInput{CollectionID: a.Collection, Base: a.Base, Title: a.Title, Remote: a.Remote, Draft: a.Draft})
			if e != nil {
				return "", e
			}
			return fmt.Sprintf("PR plan for %s: %s -> %s (remote %s)\n\n%s", S(out.Collection, "id"), out.Title, out.Base, out.Remote, out.Body), nil
		}},
	"collection accept": {toolName: "praector_collection_accept", privileged: true, desc: "Validate acceptance and build the acceptance plan for a reviewed collection. Performs NO merge; actual acceptance stays operator-side.",
		schema: objectSchema(map[string]any{"collection": stringProp, "target": stringProp}, "collection"),
		call: func(s *Service, raw json.RawMessage) (string, error) {
			var a struct{ Collection, Target string }
			if err := bindArgs(raw, &a); err != nil {
				return "", err
			}
			out, e := s.CollectionAccept(context.Background(), CollectionAcceptInput{CollectionID: a.Collection, Target: a.Target})
			if e != nil {
				return "", e
			}
			mode := "ff-only"
			if B(out.Plan, "already_contained") {
				mode = "already-contained"
			} else if !B(out.Plan, "fast_forward") {
				mode = "diverged (--no-ff required)"
			}
			return fmt.Sprintf("Acceptance plan for %s: %s @ %s -> %s @ %s (mode would be %s). No merge was performed.", S(out.Collection, "id"), S(out.Plan, "source_branch"), shortSHA(S(out.Plan, "source_commit")), out.Target, shortSHA(S(out.Plan, "target_commit")), mode), nil
		}},
	"collection cleanup": {toolName: "praector_collection_cleanup", privileged: true, desc: "Remove completed collection/task worktrees (requires COMPLETE + accepted, or force).",
		schema: objectSchema(map[string]any{"collection": stringProp, "delete_branches": boolProp, "force": boolProp}, "collection"),
		call: func(s *Service, raw json.RawMessage) (string, error) {
			var a struct {
				Collection     string
				DeleteBranches bool
				Force          bool
			}
			if err := bindArgs(raw, &a); err != nil {
				return "", err
			}
			out, e := s.CollectionCleanup(context.Background(), CollectionCleanupInput{CollectionID: a.Collection, DeleteBranches: a.DeleteBranches, Force: a.Force})
			if e != nil {
				return "", e
			}
			return fmt.Sprintf("Cleaned worktrees: %d, branches: %d", len(out.Worktrees), len(out.Branches)), nil
		}},

	// --- profile ---
	"profile": {resourceURI: "praector://profiles", resourceName: "Built-in profiles", read: func(s *Service) (string, error) {
		out, e := s.ProfileList(context.Background(), ProfileListInput{})
		if e != nil {
			return "", e
		}
		return MarshalPretty(out.Profiles), nil
	}},
	"profile list": {toolName: "praector_profile_list", desc: "List runtime profiles and capability mode.",
		schema: objectSchema(map[string]any{}),
		call: func(s *Service, _ json.RawMessage) (string, error) {
			out, e := s.ProfileList(context.Background(), ProfileListInput{})
			if e != nil {
				return "", e
			}
			return MarshalPretty(out.Profiles), nil
		}},
	"profile show": {toolName: "praector_profile_show", desc: "Print the inherited global and role-specific contracts for a runtime profile.",
		schema: objectSchema(map[string]any{"profile": stringProp}, "profile"),
		call: func(s *Service, raw json.RawMessage) (string, error) {
			var a struct{ Profile string }
			if err := bindArgs(raw, &a); err != nil {
				return "", err
			}
			out, e := s.ProfileShow(context.Background(), ProfileShowInput{Profile: a.Profile})
			if e != nil {
				return "", e
			}
			return fmt.Sprintf("== global ==\n%s\n\n== role: %s ==\n%s", out.Global, a.Profile, out.Role), nil
		}},

	// --- project ---
	"project": {resourceURI: "praector://projects", resourceName: "Registered projects", read: func(s *Service) (string, error) {
		out, e := s.ProjectList(context.Background(), ProjectListInput{})
		if e != nil {
			return "", e
		}
		return MarshalPretty(out.Projects), nil
	}},
	"project list": {toolName: "praector_project_list", desc: "List registered projects.",
		schema: objectSchema(map[string]any{}),
		call: func(s *Service, _ json.RawMessage) (string, error) {
			out, e := s.ProjectList(context.Background(), ProjectListInput{})
			if e != nil {
				return "", e
			}
			return MarshalPretty(out.Projects), nil
		}},
	"project show": {toolName: "praector_project_show", desc: "Show a registered project.",
		schema: objectSchema(map[string]any{"project": stringProp}),
		call: func(s *Service, raw json.RawMessage) (string, error) {
			var a struct{ Project string }
			if err := bindArgs(raw, &a); err != nil {
				return "", err
			}
			out, e := s.ProjectShow(context.Background(), ProjectShowInput{ProjectID: a.Project})
			if e != nil {
				return "", e
			}
			return MarshalPretty(out.Project), nil
		}},

	// --- task ---
	"task": {resourceURI: "praector://help/task", resourceName: "task commands", read: helpResource("task")},
	"task show": {toolName: "praector_task_show", desc: "Show one task with resolved dependencies/dependents.",
		schema: objectSchema(map[string]any{"task": stringProp}, "task"),
		call: func(s *Service, raw json.RawMessage) (string, error) {
			var a struct{ Task string }
			if err := bindArgs(raw, &a); err != nil {
				return "", err
			}
			if a.Task == "" {
				return "", fmt.Errorf("task is required")
			}
			out, e := s.TaskShow(context.Background(), TaskShowInput{TaskID: a.Task})
			if e != nil {
				return "", e
			}
			return MarshalPretty(out.Task), nil
		}},
	"task list": {toolName: "praector_task_list", desc: "List tasks in a collection with dependencies.",
		schema: objectSchema(map[string]any{"collection": stringProp, "project": stringProp}),
		call: func(s *Service, raw json.RawMessage) (string, error) {
			var a struct{ Collection, Project string }
			if err := bindArgs(raw, &a); err != nil {
				return "", err
			}
			out, e := s.TaskList(context.Background(), TaskListInput{Collection: a.Collection, ProjectID: a.Project})
			if e != nil {
				return "", e
			}
			return MarshalPretty(out.Tasks), nil
		}},
	"task retry": {toolName: "praector_task_retry", desc: "Create a fresh replacement for a FAILED task (the running daemon executes it).",
		schema: objectSchema(map[string]any{"task": stringProp, "reason": stringProp}, "task"),
		call: func(s *Service, raw json.RawMessage) (string, error) {
			var a struct{ Task, Reason string }
			if err := bindArgs(raw, &a); err != nil {
				return "", err
			}
			out, e := s.TaskRetry(context.Background(), TaskRetryInput{TaskID: a.Task, Reason: a.Reason})
			if e != nil {
				return "", e
			}
			return fmt.Sprintf("Created replacement task %s for %s. The running daemon executes pending work.", S(out.Replacement, "id"), S(out.Original, "id")), nil
		}},
	"task request": {toolName: "praector_task_request", desc: "Enqueue durable delegated work against a caller task (the running daemon executes it).",
		schema: objectSchema(map[string]any{"caller": stringProp, "profile": stringProp, "title": stringProp, "objective": stringProp, "reason": stringProp, "blocking": boolProp, "depends_on": arrayProp, "workspace": stringProp}, "caller", "profile", "title", "objective", "reason"),
		call: func(s *Service, raw json.RawMessage) (string, error) {
			var a struct {
				Caller, Profile, Title, Objective, Reason, Workspace string
				Blocking                                             bool
				DependsOn                                            []string
			}
			if err := bindArgs(raw, &a); err != nil {
				return "", err
			}
			for k, v := range map[string]string{"caller": a.Caller, "profile": a.Profile, "title": a.Title, "objective": a.Objective, "reason": a.Reason} {
				if v == "" {
					return "", fmt.Errorf("%s is required", k)
				}
			}
			out, e := s.TaskRequest(context.Background(), TaskRequestInput{CallerID: a.Caller, Profile: a.Profile, Title: a.Title, Objective: a.Objective, Reason: a.Reason, Blocking: a.Blocking, DependsOn: a.DependsOn, Workspace: a.Workspace})
			if e != nil {
				return "", e
			}
			id := "deduplicated"
			if out.Created {
				id = "created"
			}
			return fmt.Sprintf("%s task %s", id, S(out.Task, "id")), nil
		}},
	"task add-dependency": {toolName: "praector_task_add_dependency", desc: "Coordinator-only dependency-edge mutation on the durable task graph.",
		schema: objectSchema(map[string]any{"task": stringProp, "on": stringProp, "caller": stringProp}, "task", "on"),
		call: func(s *Service, raw json.RawMessage) (string, error) {
			var a struct{ Task, On, Caller string }
			if err := bindArgs(raw, &a); err != nil {
				return "", err
			}
			out, e := s.TaskAddDependency(context.Background(), TaskAddDependencyInput{TaskID: a.Task, OnID: a.On, Caller: a.Caller})
			if e != nil {
				return "", e
			}
			return fmt.Sprintf("Added dependency: %s now depends on %s", S(out.Task, "id"), S(out.Dep, "id")), nil
		}},
	"task supersede": {toolName: "praector_task_supersede", desc: "Coordinator-only replacement/rewire of a task.",
		schema: objectSchema(map[string]any{"old": stringProp, "with": stringProp, "reason": stringProp, "caller": stringProp}, "old", "with"),
		call: func(s *Service, raw json.RawMessage) (string, error) {
			var a struct{ Old, With, Reason, Caller string }
			if err := bindArgs(raw, &a); err != nil {
				return "", err
			}
			out, e := s.TaskSupersede(context.Background(), TaskSupersedeInput{OldID: a.Old, NewID: a.With, Reason: a.Reason, Caller: a.Caller})
			if e != nil {
				return "", e
			}
			return fmt.Sprintf("Superseded %s with %s", S(out.Old, "id"), S(out.New, "id")), nil
		}},

	// --- context ---
	"context": {resourceURI: "praector://help/context", resourceName: "context commands", read: helpResource("context")},
	"context show": {toolName: "praector_context_show", desc: "Show durable task context.", schema: objectSchema(map[string]any{"task": stringProp}), call: func(s *Service, raw json.RawMessage) (string, error) {
		var a struct{ Task string }
		if err := bindArgs(raw, &a); err != nil {
			return "", err
		}
		out, e := s.ContextShow(context.Background(), ContextShowInput{TaskID: a.Task})
		if e != nil {
			return "", e
		}
		return MarshalPretty(out.Payload), nil
	}},

	// --- coordinator ---
	"coordinator": {resourceURI: "praector://help/coordinator", resourceName: "coordinator commands", read: helpResource("coordinator")},
	"coordinator attention": {toolName: "praector_coordinator_attention", desc: "Request coordinator attention as durable work (the running daemon executes it).",
		schema: objectSchema(map[string]any{"task": stringProp, "reason": stringProp, "blocking": boolProp}, "reason"),
		call: func(s *Service, raw json.RawMessage) (string, error) {
			var a struct {
				Task, Reason string
				Blocking     bool
			}
			if err := bindArgs(raw, &a); err != nil {
				return "", err
			}
			out, e := s.CoordinatorAttention(context.Background(), CoordinatorAttentionInput{TaskID: a.Task, Reason: a.Reason, Blocking: a.Blocking})
			if e != nil {
				return "", e
			}
			return fmt.Sprintf("Coordinator attention task %s", S(out.Task, "id")), nil
		}},

	// --- result ---
	"result": {resourceURI: "praector://help/result", resourceName: "result commands", read: helpResource("result")},
	"result show": {toolName: "praector_result_show", desc: "Show the durable structured result for a task.",
		schema: objectSchema(map[string]any{"task": stringProp}),
		call: func(s *Service, raw json.RawMessage) (string, error) {
			var a struct{ Task string }
			if err := bindArgs(raw, &a); err != nil {
				return "", err
			}
			out, e := s.ResultShow(context.Background(), ResultShowInput{TaskID: a.Task})
			if e != nil {
				return "", e
			}
			return MarshalPretty(out.Result), nil
		}},
	"result submit": {toolName: "praector_result_submit", desc: "Submit a durable structured task result (enforces review/plan_review verdicts).",
		schema: objectSchema(map[string]any{"task": stringProp, "status": stringProp, "summary": stringProp, "result": objProp}, "result"),
		call: func(s *Service, raw json.RawMessage) (string, error) {
			var a struct {
				Task, Status, Summary string
				Result                map[string]any
			}
			if err := bindArgs(raw, &a); err != nil {
				return "", err
			}
			res := Row{}
			for k, v := range a.Result {
				res[k] = v
			}
			if a.Status != "" {
				res["status"] = a.Status
			}
			if a.Summary != "" {
				res["summary"] = a.Summary
			}
			if len(res) == 0 {
				return "", fmt.Errorf("result is required")
			}
			out, e := s.ResultSubmit(context.Background(), ResultSubmitInput{TaskID: a.Task, Result: res})
			if e != nil {
				return "", e
			}
			return fmt.Sprintf("Submitted result; task status %s", out.Status), nil
		}},

	// --- memory ---
	"memory": {resourceURI: "praector://help/memory", resourceName: "memory commands", read: helpResource("memory")},
	"memory add": {toolName: "praector_memory_add", desc: "Add durable project/collection memory.",
		schema: objectSchema(map[string]any{"text": stringProp, "project": stringProp, "collection": stringProp, "task": stringProp, "scope": stringProp, "state": stringProp, "kind": stringProp}, "text"),
		call: func(s *Service, raw json.RawMessage) (string, error) {
			var a struct{ Text, Project, Collection, Task, Scope, State, Kind string }
			if err := bindArgs(raw, &a); err != nil {
				return "", err
			}
			out, e := s.MemoryAdd(context.Background(), MemoryAddInput{Text: a.Text, ProjectID: a.Project, CollectionID: a.Collection, TaskID: a.Task, Scope: a.Scope, State: a.State, Kind: a.Kind})
			if e != nil {
				return "", e
			}
			return fmt.Sprintf("Added memory %s", S(out.Memory, "id")), nil
		}},
	"memory list": {toolName: "praector_memory_list", desc: "List durable memory.",
		schema: objectSchema(map[string]any{"project": stringProp, "collection": stringProp, "state": stringProp}),
		call: func(s *Service, raw json.RawMessage) (string, error) {
			var a struct{ Project, Collection, State string }
			if err := bindArgs(raw, &a); err != nil {
				return "", err
			}
			out, e := s.MemoryList(context.Background(), MemoryListInput{ProjectID: a.Project, CollectionID: a.Collection, State: a.State})
			if e != nil {
				return "", e
			}
			return MarshalPretty(out.Memories), nil
		}},

	// --- decision ---
	"decision": {resourceURI: "praector://help/decision", resourceName: "decision commands", read: helpResource("decision")},
	"decision add": {toolName: "praector_decision_add", desc: "Add a durable decision.",
		schema: objectSchema(map[string]any{"statement": stringProp, "rationale": stringProp, "project": stringProp, "collection": stringProp, "task": stringProp}, "statement"),
		call: func(s *Service, raw json.RawMessage) (string, error) {
			var a struct{ Statement, Rationale, Project, Collection, Task string }
			if err := bindArgs(raw, &a); err != nil {
				return "", err
			}
			out, e := s.DecisionAdd(context.Background(), DecisionAddInput{Statement: a.Statement, Rationale: a.Rationale, ProjectID: a.Project, CollectionID: a.Collection, TaskID: a.Task})
			if e != nil {
				return "", e
			}
			return fmt.Sprintf("Added decision %s", S(out.Decision, "id")), nil
		}},
}

func collectionIDSchema() map[string]any {
	return objectSchema(map[string]any{"collection": stringProp}, "collection")
}

func collectionShowTool(s *Service, raw json.RawMessage) (string, error) {
	var a struct{ Collection string }
	if err := bindArgs(raw, &a); err != nil {
		return "", err
	}
	if a.Collection == "" {
		return "", fmt.Errorf("collection is required")
	}
	out, e := s.CollectionShow(context.Background(), CollectionShowInput{CollectionID: a.Collection})
	if e != nil {
		return "", e
	}
	return MarshalPretty(out.Collection), nil
}

// helpResource renders one command namespace's durable help text as a resource.
func helpResource(key string) resourceFunc {
	return func(*Service) (string, error) {
		text, ok := commandHelp[key]
		if !ok {
			return "", fmt.Errorf("no help for command %q", key)
		}
		return strings.TrimRight(text, "\n"), nil
	}
}

// ListTools implements mcp.ToolProvider. It enumerates commandHelp (the canonical
// CLI inventory) so every leaf domain command appears as one tool. Privileged
// collection-finalizing tools appear only when the independent opt-in is enabled.
func (b *mcpBridge) ListTools() []mcp.Tool {
	keys := make([]string, 0, len(commandHelp))
	for k := range commandHelp {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	seen := map[string]bool{}
	out := []mcp.Tool{}
	for _, key := range keys {
		op, ok := serviceOps[key]
		if !ok || op.kind != opDomain {
			continue // namespaces / launch-only commands surface as resources
		}
		e, ok := mcpSurface[key]
		if !ok || e.toolName == "" {
			continue // parity test flags an unprojected commandHelp entry
		}
		if e.privileged && !b.privileged {
			continue
		}
		if seen[e.toolName] {
			continue
		}
		seen[e.toolName] = true
		out = append(out, mcp.Tool{Name: e.toolName, Description: e.desc, InputSchema: e.schema})
	}
	return out
}

// CallTool implements mcp.ToolProvider. A lookup miss is a protocol error; a
// domain failure is returned as an error so the core reports it via isError. A
// privileged command's tool is not callable while the opt-in is disabled.
func (b *mcpBridge) CallTool(name string, args json.RawMessage) (mcp.ToolResult, error) {
	key, ok := mcpToolToCommand(name)
	if !ok {
		return mcp.ToolResult{}, fmt.Errorf("unknown tool: %s", name)
	}
	e := mcpSurface[key]
	if e.call == nil {
		return mcp.ToolResult{}, fmt.Errorf("unknown tool: %s", name)
	}
	if e.privileged && !b.privileged {
		return mcp.ToolResult{}, fmt.Errorf("tool %s is a privileged write and is disabled; enable [mcp] privileged / PRAECTOR_MCP_PRIVILEGED to expose it", name)
	}
	text, err := e.call(b.svc, args)
	if err != nil {
		return mcp.ToolResult{}, err
	}
	return mcp.ToolResult{Content: []mcp.ContentItem{{Type: "text", Text: text}}}, nil
}

// ListResources implements mcp.ResourceProvider. It enumerates commandHelp so
// every command namespace and launch-only command appears as one resource.
func (b *mcpBridge) ListResources() []mcp.Resource {
	keys := make([]string, 0, len(commandHelp))
	for k := range commandHelp {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := []mcp.Resource{}
	seen := map[string]bool{}
	for _, key := range keys {
		op, ok := serviceOps[key]
		if !ok || op.kind == opDomain {
			continue // leaf domain commands surface as tools
		}
		e, ok := mcpSurface[key]
		if !ok || e.resourceURI == "" || seen[e.resourceURI] {
			continue
		}
		seen[e.resourceURI] = true
		out = append(out, mcp.Resource{URI: e.resourceURI, Name: e.resourceName, Description: strings.TrimSpace(firstLine(commandHelp[key])), MIMEType: "application/json"})
	}
	return out
}

// ReadResource implements mcp.ResourceProvider.
func (b *mcpBridge) ReadResource(uri string) ([]mcp.ResourceContents, error) {
	key, ok := mcpResourceToCommand(uri)
	if !ok {
		return nil, fmt.Errorf("unknown resource: %s", uri)
	}
	e := mcpSurface[key]
	if e.read == nil {
		return nil, fmt.Errorf("unknown resource: %s", uri)
	}
	text, err := e.read(b.svc)
	if err != nil {
		return nil, err
	}
	return []mcp.ResourceContents{{URI: uri, MIMEType: "application/json", Text: text}}, nil
}

// mcpToolToCommand reverse-maps a tool name to its commandHelp key by scanning
// the surface, so the callable surface and commandHelp cannot diverge silently.
func mcpToolToCommand(name string) (string, bool) {
	for key, e := range mcpSurface {
		if e.toolName == name {
			return key, true
		}
	}
	return "", false
}

// mcpResourceToCommand reverse-maps a resource URI to its commandHelp key.
func mcpResourceToCommand(uri string) (string, bool) {
	for key, e := range mcpSurface {
		if e.resourceURI == uri {
			return key, true
		}
	}
	return "", false
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// newMCPServer builds the MCP core wired to the shared service layer. The
// returned server performs no process work; it delegates to Service ops only.
// privileged controls whether the collection-finalizing write tools are exposed.
func newMCPServer(svc *Service, privileged bool) *mcp.Server {
	b := &mcpBridge{svc: svc, privileged: privileged}
	return &mcp.Server{ServerName: "praector", ServerVersion: Version, Tools: b, Resources: b}
}

// startMCPServer constructs a loopback-only HTTP server for the MCP core using
// the caller's already-open store connection. It never creates a Scheduler and
// never runs a collection; callers own the returned server's lifecycle.
func startMCPServer(svc *Service) (*http.Server, error) {
	cfg := LoadMCPServerConfig(svc.cfg)
	srv, err := mcp.NewLoopbackServer(cfg.Addr, newMCPServer(svc, cfg.Privileged))
	if err != nil {
		return nil, err
	}
	return srv, nil
}

// bindArgs decodes tool arguments, tolerating an empty body and unknown fields.
func bindArgs(raw json.RawMessage, target any) error {
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return fmt.Errorf("invalid tool arguments: %w", err)
	}
	return nil
}
