package praector

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestMCPCommandHelpParity is the MCP half of the CLI/MCP parity contract from
// DEC-aacb7bfc23: EVERY commandHelp entry must project to exactly one MCP surface
// item — a tool for a leaf domain command (serviceOps kind opDomain), or a
// resource for a command namespace / launch-only command — wired through the
// shared service layer. Because the projection is driven off the SAME serviceOps
// registry that service_parity_test.go binds to commandHelp, any drift between
// the CLI surface and the MCP surface fails here, in either direction ("now and
// always"). It also asserts the tool/resource sets are internally consistent and
// uniquely reverse-map to their commandHelp key.
func TestMCPCommandHelpParity(t *testing.T) {
	for key := range commandHelp {
		op, ok := serviceOps[key]
		if !ok {
			t.Errorf("commandHelp entry %q has no service op registration", key)
			continue
		}
		e, ok := mcpSurface[key]
		if !ok {
			t.Errorf("commandHelp entry %q has no MCP surface projection", key)
			continue
		}
		switch op.kind {
		case opDomain:
			if e.toolName == "" || e.call == nil {
				t.Errorf("domain command %q must project to a callable MCP tool", key)
				continue
			}
			if e.resourceURI != "" {
				t.Errorf("domain command %q must not also project to a resource", key)
			}
			if got, ok := mcpToolToCommand(e.toolName); !ok || got != key {
				t.Errorf("tool %q does not reverse-map to commandHelp %q", e.toolName, key)
			}
		case opGroup, opLaunchOnly:
			if e.resourceURI == "" || e.read == nil {
				t.Errorf("command %q (kind %d) must project to a readable MCP resource", key, op.kind)
				continue
			}
			if e.toolName != "" {
				t.Errorf("command %q (kind %d) must not also project to a tool", key, op.kind)
			}
			if got, ok := mcpResourceToCommand(e.resourceURI); !ok || got != key {
				t.Errorf("resource %q does not reverse-map to commandHelp %q", e.resourceURI, key)
			}
		default:
			t.Errorf("command %q has an invalid service op kind %d", key, op.kind)
		}
	}
	// Reverse direction: no MCP surface entry may exist without a commandHelp entry.
	for key := range mcpSurface {
		if _, ok := commandHelp[key]; !ok {
			t.Errorf("MCP surface entry %q has no commandHelp entry", key)
		}
	}
}

// TestMCPSurfaceToolResourceCoverage proves the bridge enumerates the surface
// straight from commandHelp/serviceOps: with privileged writes enabled, the
// advertised tools + resources count equals the commandHelp inventory, and with
// privileged writes disabled the only difference is the privileged tools.
func TestMCPSurfaceToolResourceCoverage(t *testing.T) {
	s, cfg := newFakeStore(t)
	svc := &Service{store: s, cfg: cfg}
	full := &mcpBridge{svc: svc, privileged: true}
	tools := map[string]bool{}
	for _, tl := range full.ListTools() {
		if tools[tl.Name] {
			t.Fatalf("duplicate tool advertised: %s", tl.Name)
		}
		tools[tl.Name] = true
	}
	res := map[string]bool{}
	for _, r := range full.ListResources() {
		if res[r.URI] {
			t.Fatalf("duplicate resource advertised: %s", r.URI)
		}
		res[r.URI] = true
	}
	if len(tools)+len(res) != len(commandHelp) {
		t.Fatalf("MCP surface size %d (tools %d + resources %d) != commandHelp size %d",
			len(tools)+len(res), len(tools), len(res), len(commandHelp))
	}
	// Every opDomain commandHelp entry must have exactly one advertised tool.
	for key, op := range serviceOps {
		if op.kind != opDomain {
			continue
		}
		e := mcpSurface[key]
		if !tools[e.toolName] {
			t.Errorf("domain command %q (tool %q) not advertised by ListTools", key, e.toolName)
		}
	}
}

// TestMCPPrivilegedToolsDefaultOff proves the collection-finalizing tools
// (collection pr/accept/cleanup) are NOT exposed while the independent privileged
// opt-in is disabled, and are available once it is enabled — without affecting the
// read-only + queue-write parity surface.
func TestMCPPrivilegedToolsDefaultOff(t *testing.T) {
	s, cfg := newFakeStore(t)
	svc := &Service{store: s, cfg: cfg}
	privilegedTools := map[string]bool{}
	for key, e := range mcpSurface {
		if e.privileged && e.toolName != "" {
			privilegedTools[e.toolName] = true
			// Sanity: privileged tools are exactly the acceptance/PR/cleanup trio.
			if !strings.HasPrefix(key, "collection ") {
				t.Errorf("unexpected privileged command %q (only collection pr/accept/cleanup are privileged)", key)
			}
		}
	}
	if len(privilegedTools) == 0 {
		t.Fatal("expected privileged tools to be declared")
	}

	off := &mcpBridge{svc: svc, privileged: false}
	on := &mcpBridge{svc: svc, privileged: true}
	offTools := advertised(off)
	onTools := advertised(on)

	for name := range privilegedTools {
		if offTools[name] {
			t.Errorf("privileged tool %q must be hidden while opt-in is disabled", name)
		}
		if !onTools[name] {
			t.Errorf("privileged tool %q must be advertised once opt-in is enabled", name)
		}
	}
	// A privileged tool call is refused while disabled (not merely hidden).
	if _, err := off.CallTool("praector_collection_accept", json.RawMessage(`{"collection":"COL-x"}`)); err == nil || !strings.Contains(err.Error(), "privileged") {
		t.Errorf("disabled privileged tool must be refused: %v", err)
	}
	// The non-privileged parity surface is identical with opt-in on or off.
	for name := range offTools {
		if privilegedTools[name] {
			continue
		}
		if !onTools[name] {
			t.Errorf("non-privileged tool %q disappeared when privileged opt-in was enabled", name)
		}
	}
}

// TestMCPQueueWriteMatchesServiceOp is the pass-through parity proof: an MCP
// queue-write tool call (task request) yields the SAME durable task effect and the
// SAME audit event as invoking the shared Service op directly (which is exactly
// what the CLI command delegates to), because both reuse the identical gated
// Store.RequestTask path.
func TestMCPQueueWriteMatchesServiceOp(t *testing.T) {
	s, cfg := newFakeStore(t)
	svc := &Service{store: s, cfg: cfg}
	p, err := s.RegisterProject(testProjectConfig(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	// One collection drives the MCP tool path; another drives the Service op.
	cTool, coordTool, err := s.CreateCollectionWithCoordinator(S(p, "id"), "ToolPath", "Add endpoint", "Plan")
	if err != nil {
		t.Fatal(err)
	}
	cSvc, coordSvc, err := s.CreateCollectionWithCoordinator(S(p, "id"), "SvcPath", "Add endpoint", "Plan")
	if err != nil {
		t.Fatal(err)
	}
	b := &mcpBridge{svc: svc, privileged: false}

	args, _ := json.Marshal(map[string]any{
		"caller": S(coordTool, "id"), "profile": "research",
		"title": "Investigate", "objective": "Determine X with evidence", "reason": "needed",
	})
	res, e := b.CallTool("praector_task_request", args)
	if e != nil {
		t.Fatalf("tool call: %v", e)
	}
	if len(res.Content) == 0 {
		t.Fatal("empty tool result")
	}
	if !strings.Contains(res.Content[0].Text, "task T-") {
		t.Fatalf("tool result missing created task id: %q", res.Content[0].Text)
	}

	// The durable effect of the Service op directly, for comparison.
	svcOut, e := svc.TaskRequest(context.Background(), TaskRequestInput{CallerID: S(coordSvc, "id"), Profile: "research", Title: "Investigate", Objective: "Determine X with evidence", Reason: "needed"})
	if e != nil {
		t.Fatalf("service op: %v", e)
	}

	toolTasks, e := s.ListTasks(S(cTool, "id"))
	if e != nil {
		t.Fatal(e)
	}
	var toolTask Row
	for _, tk := range toolTasks {
		if S(tk, "profile") == "research" {
			toolTask = tk
		}
	}
	if toolTask == nil {
		t.Fatal("MCP tool call created no durable research task")
	}

	// The child's durable shape must match the Service op's child (minus generated
	// ids/timestamps that differ per collection).
	for _, f := range []string{"profile", "status", "workspace_mode"} {
		if S(toolTask, f) != S(svcOut.Task, f) {
			t.Errorf("field %q differs: tool=%q service=%q", f, S(toolTask, f), S(svcOut.Task, f))
		}
	}
	// Both paths record the same gating audit event referencing the created task.
	if !hasTaskRequestedEvent(s, S(cTool, "id"), S(toolTask, "id")) {
		t.Error("MCP tool path recorded no TASK_REQUESTED event")
	}
	if !hasTaskRequestedEvent(s, S(cSvc, "id"), S(svcOut.Task, "id")) {
		t.Error("service-op path recorded no TASK_REQUESTED event")
	}
}

func advertised(b *mcpBridge) map[string]bool {
	out := map[string]bool{}
	for _, tl := range b.ListTools() {
		out[tl.Name] = true
	}
	return out
}

func hasTaskRequestedEvent(s *Store, cid, taskID string) bool {
	rows, err := s.EventsForCollection(cid, 200)
	if err != nil {
		return false
	}
	for _, r := range rows {
		if S(r, "event_type") != "TASK_REQUESTED" {
			continue
		}
		if payloadHas(S(r, "payload_json"), "requested_task_id", taskID) {
			return true
		}
	}
	return false
}

func payloadHas(payload, key, want string) bool {
	var m map[string]any
	if json.Unmarshal([]byte(payload), &m) != nil {
		return false
	}
	v, _ := m[key].(string)
	return v == want
}
