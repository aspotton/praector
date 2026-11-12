package praector

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// TestMCPToolsRequireExplicitProject is the daemon-cwd leak regression: an MCP
// call that would otherwise resolve a project via projectFromContext's cwd
// fallback must instead be refused with an explicit-project error, regardless of
// the daemon process working directory. It covers run, status, graph, doctor,
// task list, memory list, project show, and decision/memory adds.
func TestMCPToolsRequireExplicitProject(t *testing.T) {
	s, cfg := newFakeStore(t)
	svc := &Service{store: s, cfg: cfg}
	b := &mcpBridge{svc: svc, privileged: false}

	// A daemon working directory with NO project marker: the CLI cwd path would
	// already fail here, so an explicit-project refusal proves the bridge never
	// even consults the cwd.
	cwd, _ := os.Getwd()
	dir := t.TempDir()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(cwd)

	cases := map[string]map[string]any{
		"praector_run":          {"objective": "x"},
		"praector_status":       {},
		"praector_graph":        {},
		"praector_doctor":       {},
		"praector_task_list":    {},
		"praector_memory_list":  {},
		"praector_memory_add":   {"text": "note"},
		"praector_decision_add": {"statement": "s"},
		"praector_project_show": {},
	}
	for name, args := range cases {
		raw, _ := json.Marshal(args)
		_, err := b.CallTool(name, raw)
		if err == nil || !strings.Contains(err.Error(), "explicit project") {
			t.Errorf("%s with no project identifier must be refused (not bound to daemon cwd): %v", name, err)
		}
	}
}

// TestMCPDurableIDToolsSkipProjectGate proves the explicit-project gate is about
// daemon-cwd, not a blanket requirement: tools given a resolvable durable id
// (COL-*/T-*) or an explicit project proceed past the gate (they may still fail
// for other reasons, but never with the "explicit project" message).
func TestMCPDurableIDToolsSkipProjectGate(t *testing.T) {
	s, cfg := newFakeStore(t)
	svc := &Service{store: s, cfg: cfg}
	p, err := s.RegisterProject(testProjectConfig(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	c, _, err := s.CreateCollectionWithCoordinator(S(p, "id"), "C", "obj", "Plan")
	if err != nil {
		t.Fatal(err)
	}
	b := &mcpBridge{svc: svc, privileged: false}

	cases := map[string]map[string]any{
		"praector_graph":     {"collection": S(c, "id")},
		"praector_task_list": {"collection": S(c, "id")},
		"praector_status":    {"target": S(c, "id")},
		"praector_run":       {"objective": "x", "project": S(p, "id")},
	}
	for name, args := range cases {
		raw, _ := json.Marshal(args)
		_, err := b.CallTool(name, raw)
		if err != nil && strings.Contains(err.Error(), "explicit project") {
			t.Errorf("%s with a durable id/explicit project must pass the cwd gate: %v", name, err)
		}
	}
}

// TestMCPInitRequiresAbsolutePath proves MCP praector_init refuses relative and
// empty root_path values whose meaning would depend on the daemon cwd.
func TestMCPInitRequiresAbsolutePath(t *testing.T) {
	s, cfg := newFakeStore(t)
	b := &mcpBridge{svc: &Service{store: s, cfg: cfg}, privileged: true}
	for _, bad := range []string{"", ".", "..", "some/repo", "./repo", "../repo"} {
		raw, _ := json.Marshal(map[string]any{"root_path": bad})
		_, err := b.CallTool("praector_init", raw)
		if err == nil {
			t.Errorf("MCP init accepted root_path %q; relative/empty must be refused", bad)
			continue
		}
		if !strings.Contains(err.Error(), "root_path") {
			t.Errorf("MCP init rejected %q for the wrong reason: %v", bad, err)
		}
	}
}

// TestMCPResourceContentTypes proves JSON-backed resources advertise
// application/json while CLI/help-text resources advertise text/plain, both in
// resources/list metadata and in the resources/read content.
func TestMCPResourceContentTypes(t *testing.T) {
	s, cfg := newFakeStore(t)
	b := &mcpBridge{svc: &Service{store: s, cfg: cfg}, privileged: true}

	jsonResources := map[string]bool{"praector://profiles": true, "praector://projects": true}
	sawJSON, sawText := false, false
	for _, r := range b.ListResources() {
		want := "text/plain"
		if jsonResources[r.URI] {
			want = "application/json"
		}
		if r.MIMEType != want {
			t.Errorf("resource %s advertised %q, want %q", r.URI, r.MIMEType, want)
		}
		if r.MIMEType == "application/json" {
			sawJSON = true
		}
		if r.MIMEType == "text/plain" {
			sawText = true
		}
	}
	if !sawJSON || !sawText {
		t.Fatalf("expected both application/json and text/plain resources")
	}

	for _, uri := range []string{"praector://help/task", "praector://projects"} {
		contents, err := b.ReadResource(uri)
		if err != nil || len(contents) == 0 {
			t.Fatalf("read %s: %v", uri, err)
		}
		want := "text/plain"
		if uri == "praector://projects" {
			want = "application/json"
		}
		if contents[0].MIMEType != want {
			t.Errorf("read %s content type %q, want %q", uri, contents[0].MIMEType, want)
		}
	}
}
