package praector

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/datascape/praector/internal/praector/mcp"
)

// mcpBridge adapts the shared typed Service layer to the dependency-light MCP
// core in internal/praector/mcp. It implements only the tool/resource provider
// interfaces, so every exposed MCP operation reuses the exact same durable Store
// reads and enqueue paths the CLI uses (DEC-28fcd38435 / DEC-aacb7bfc23).
//
// Ownership boundary: this bridge NEVER constructs a scheduler and NEVER runs a
// collection inline. "Execute"-style actions enqueue durable work through the
// same CreateCollectionWithCoordinator / CreateTask Store paths the CLI's
// --detach path uses, and rely on the already-running daemon to execute it. A
// full, commandHelp-driven parity mapping of every CLI command to tools/resources
// is layered on top of this bridge by the MCP-parity task.
type mcpBridge struct{ svc *Service }

// mcpTool is one callable tool: a JSON-Schema-shaped descriptor plus a handler
// that maps raw JSON arguments onto a typed Service op.
type mcpTool struct {
	name        string
	description string
	schema      map[string]any
	call        func(*Service, json.RawMessage) (string, error)
}

func objectSchema(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

var stringProp = map[string]any{"type": "string"}

// mcpTools is the read-only + enqueue-only subset exposed by this bridge. Each
// handler calls exactly one typed Service op and never launches a process.
var mcpTools = []mcpTool{
	{name: "praector_run", description: "Enqueue a new collection with its coordinator task (the running daemon executes it). Does not run inline.",
		schema: objectSchema(map[string]any{"objective": stringProp, "title": stringProp, "project": stringProp}, "objective"),
		call: func(s *Service, raw json.RawMessage) (string, error) {
			var a struct {
				Objective, Title, Project string
			}
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
			return fmt.Sprintf("Queued collection %s (coordinator %s). Run `praector daemon` to execute pending work.", S(out.Collection, "id"), S(out.Coordinator, "id")), nil
		}},
	{name: "praector_task_request", description: "Enqueue durable delegated work against a caller task (the running daemon executes it).",
		schema: objectSchema(map[string]any{"caller": stringProp, "profile": stringProp, "title": stringProp, "objective": stringProp, "reason": stringProp}, "caller", "profile", "title", "objective", "reason"),
		call: func(s *Service, raw json.RawMessage) (string, error) {
			var a struct{ Caller, Profile, Title, Objective, Reason string }
			if err := bindArgs(raw, &a); err != nil {
				return "", err
			}
			for k, v := range map[string]string{"caller": a.Caller, "profile": a.Profile, "title": a.Title, "objective": a.Objective, "reason": a.Reason} {
				if v == "" {
					return "", fmt.Errorf("%s is required", k)
				}
			}
			out, e := s.TaskRequest(context.Background(), TaskRequestInput{CallerID: a.Caller, Profile: a.Profile, Title: a.Title, Objective: a.Objective, Reason: a.Reason})
			if e != nil {
				return "", e
			}
			id := "deduplicated"
			if out.Created {
				id = "created"
			}
			return fmt.Sprintf("%s task %s", id, S(out.Task, "id")), nil
		}},
	{name: "praector_status", description: "Show project, collection, or task status.",
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
	{name: "praector_graph", description: "Show the current durable task graph.",
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
	{name: "praector_task_list", description: "List tasks in a collection with dependencies.",
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
	{name: "praector_task_show", description: "Show one task with resolved dependencies/dependents.",
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
	{name: "praector_project_list", description: "List registered projects.",
		schema: objectSchema(map[string]any{}),
		call: func(s *Service, _ json.RawMessage) (string, error) {
			out, e := s.ProjectList(context.Background(), ProjectListInput{})
			if e != nil {
				return "", e
			}
			return MarshalPretty(out.Projects), nil
		}},
}

// ListTools implements mcp.ToolProvider.
func (b *mcpBridge) ListTools() []mcp.Tool {
	out := make([]mcp.Tool, 0, len(mcpTools))
	for _, t := range mcpTools {
		out = append(out, mcp.Tool{Name: t.name, Description: t.description, InputSchema: t.schema})
	}
	return out
}

// CallTool implements mcp.ToolProvider. A lookup miss is a protocol error; a
// domain failure is returned as an error so the core reports it via isError.
func (b *mcpBridge) CallTool(name string, args json.RawMessage) (mcp.ToolResult, error) {
	for _, t := range mcpTools {
		if t.name == name {
			text, err := t.call(b.svc, args)
			if err != nil {
				return mcp.ToolResult{}, err
			}
			return mcp.ToolResult{Content: []mcp.ContentItem{{Type: "text", Text: text}}}, nil
		}
	}
	return mcp.ToolResult{}, fmt.Errorf("unknown tool: %s", name)
}

// ListResources implements mcp.ResourceProvider.
func (b *mcpBridge) ListResources() []mcp.Resource {
	return []mcp.Resource{
		{URI: "praector://profiles", Name: "Built-in profiles", Description: "Runtime profiles and their capability mode", MIMEType: "application/json"},
		{URI: "praector://projects", Name: "Registered projects", Description: "Projects registered with this Praector home", MIMEType: "application/json"},
	}
}

// ReadResource implements mcp.ResourceProvider.
func (b *mcpBridge) ReadResource(uri string) ([]mcp.ResourceContents, error) {
	switch uri {
	case "praector://profiles":
		out, e := b.svc.ProfileList(context.Background(), ProfileListInput{})
		if e != nil {
			return nil, e
		}
		return []mcp.ResourceContents{{URI: uri, MIMEType: "application/json", Text: MarshalPretty(out.Profiles)}}, nil
	case "praector://projects":
		out, e := b.svc.ProjectList(context.Background(), ProjectListInput{})
		if e != nil {
			return nil, e
		}
		return []mcp.ResourceContents{{URI: uri, MIMEType: "application/json", Text: MarshalPretty(out.Projects)}}, nil
	default:
		return nil, fmt.Errorf("unknown resource: %s", uri)
	}
}

// newMCPServer builds the MCP core wired to the shared service layer. The
// returned server performs no process work; it delegates to Service ops only.
func newMCPServer(svc *Service) *mcp.Server {
	b := &mcpBridge{svc: svc}
	return &mcp.Server{ServerName: "praector", ServerVersion: Version, Tools: b, Resources: b}
}

// startMCPServer constructs a loopback-only HTTP server for the MCP core using
// the caller's already-open store connection. It never creates a Scheduler and
// never runs a collection; callers own the returned server's lifecycle.
func startMCPServer(svc *Service) (*http.Server, error) {
	cfg := LoadMCPServerConfig(svc.cfg)
	srv, err := mcp.NewLoopbackServer(cfg.Addr, newMCPServer(svc))
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
