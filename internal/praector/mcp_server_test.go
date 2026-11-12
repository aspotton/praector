package praector

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMCPDisabledByDefault asserts the MCP listener is off unless explicitly
// enabled, and its default address is loopback-only.
func TestMCPDisabledByDefault(t *testing.T) {
	t.Setenv("PRAECTOR_MCP", "")
	cfg := LoadMCPServerConfig(GlobalConfig{Home: t.TempDir()})
	if cfg.Enabled {
		t.Fatal("MCP must be disabled by default")
	}
	if !mcpIsLoopback(cfg.Addr) {
		t.Fatalf("default MCP addr must be loopback, got %q", cfg.Addr)
	}
}

func TestMCPConfigEnvOverride(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PRAECTOR_MCP", "1")
	if cfg := LoadMCPServerConfig(GlobalConfig{Home: home}); !cfg.Enabled {
		t.Fatal("PRAECTOR_MCP=1 should enable")
	}
	t.Setenv("PRAECTOR_MCP", "off")
	if cfg := LoadMCPServerConfig(GlobalConfig{Home: home}); cfg.Enabled {
		t.Fatal("PRAECTOR_MCP=off should disable")
	}
	t.Setenv("PRAECTOR_MCP", "127.0.0.1:9001")
	cfg := LoadMCPServerConfig(GlobalConfig{Home: home})
	if !cfg.Enabled || cfg.Addr != "127.0.0.1:9001" {
		t.Fatalf("addr override: %+v", cfg)
	}
}

func TestMCPConfigFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PRAECTOR_MCP", "")
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("[mcp]\nenabled = true\naddr = \"127.0.0.1:8899\"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	cfg := LoadMCPServerConfig(GlobalConfig{Home: home})
	if !cfg.Enabled || cfg.Addr != "127.0.0.1:8899" {
		t.Fatalf("config file not honored: %+v", cfg)
	}
}

func TestMCPConfigFileDisabled(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PRAECTOR_MCP", "")
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("[mcp]\nenabled = false\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if cfg := LoadMCPServerConfig(GlobalConfig{Home: home}); cfg.Enabled {
		t.Fatal("explicit enabled=false must stay disabled")
	}
}

func TestMCPRefusesNonLoopback(t *testing.T) {
	s, cfg := newFakeStore(t)
	svc := &Service{store: s, cfg: cfg}
	t.Setenv("PRAECTOR_MCP", "0.0.0.0:9999")
	if _, err := startMCPServer(svc); err == nil {
		t.Fatal("startMCPServer must refuse a non-loopback bind address")
	}
}

// TestMCPRoundTripOverLoopback starts the exact loopback MCP server the daemon
// would host (via startMCPServer) and completes initialize + tools/list +
// tools/call + resources/list + resources/read over real loopback HTTP. It uses
// the shared Service seam over a fake store and launches no process.
func TestMCPRoundTripOverLoopback(t *testing.T) {
	s, cfg := newFakeStore(t)
	svc := &Service{store: s, cfg: cfg}
	t.Setenv("PRAECTOR_MCP", "127.0.0.1:0")
	srv, err := startMCPServer(svc)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	defer srv.Close()
	base := "http://" + ln.Addr().String()

	call := func(payload map[string]any) map[string]any {
		body, _ := json.Marshal(payload)
		req, err := http.NewRequest(http.MethodPost, base+mcpEndpoint(), bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("MCP-Protocol-Version", "2025-06-18")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d for %v", resp.StatusCode, payload["method"])
		}
		var out map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	init := call(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": "2025-06-18"}})
	if init["error"] != nil {
		t.Fatalf("initialize: %v", init)
	}
	tl := call(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
	res, _ := tl["result"].(map[string]any)
	tools, _ := res["tools"].([]any)
	if len(tools) == 0 {
		t.Fatalf("tools/list empty: %v", tl)
	}
	if !hasToolNamed(tools, "praector_project_list") {
		t.Fatalf("expected praector_project_list tool: %v", tools)
	}
	// A read-only tool backed by the shared service layer.
	tc := call(map[string]any{"jsonrpc": "2.0", "id": 3, "method": "tools/call", "params": map[string]any{"name": "praector_project_list", "arguments": map[string]any{}}})
	if tr, _ := tc["result"].(map[string]any); tr["isError"] == true {
		t.Fatalf("project_list tool errored: %v", tc)
	}
	rl := call(map[string]any{"jsonrpc": "2.0", "id": 4, "method": "resources/list"})
	if rr, _ := rl["result"].(map[string]any); len(rr["resources"].([]any)) == 0 {
		t.Fatalf("resources/list empty: %v", rl)
	}
	rd := call(map[string]any{"jsonrpc": "2.0", "id": 5, "method": "resources/read", "params": map[string]any{"uri": "praector://profiles"}})
	if rr, _ := rd["result"].(map[string]any); rr["contents"] == nil {
		t.Fatalf("resources/read: %v", rd)
	}
}

// TestMCPPrivilegedConfigDefaultOff asserts the privileged-write surface is a
// separate opt-in that stays OFF unless explicitly enabled via [mcp] privileged or
// PRAECTOR_MCP_PRIVILEGED, independent of whether the listener is enabled.
func TestMCPPrivilegedConfigDefaultOff(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PRAECTOR_MCP", "1")
	t.Setenv("PRAECTOR_MCP_PRIVILEGED", "")
	if cfg := LoadMCPServerConfig(GlobalConfig{Home: home}); cfg.Privileged {
		t.Fatal("privileged writes must be off by default even when the listener is enabled")
	}
	t.Setenv("PRAECTOR_MCP_PRIVILEGED", "1")
	if cfg := LoadMCPServerConfig(GlobalConfig{Home: home}); !cfg.Privileged {
		t.Fatal("PRAECTOR_MCP_PRIVILEGED=1 should enable privileged writes")
	}
	t.Setenv("PRAECTOR_MCP_PRIVILEGED", "")
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("[mcp]\nenabled = true\nprivileged = true\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if cfg := LoadMCPServerConfig(GlobalConfig{Home: home}); !cfg.Privileged {
		t.Fatal("[mcp] privileged = true must enable the privileged surface")
	}
}

// TestMCPPrivilegedToolsUnavailableWhileDisabled proves the loopback server started
// with the default (unprivileged) config advertises no privileged write tool and
// refuses a direct call to one, while still serving read/queue tools.
func TestMCPPrivilegedToolsUnavailableWhileDisabled(t *testing.T) {
	s, cfg := newFakeStore(t)
	svc := &Service{store: s, cfg: cfg}
	t.Setenv("PRAECTOR_MCP", "127.0.0.1:0")
	t.Setenv("PRAECTOR_MCP_PRIVILEGED", "")
	srv, err := startMCPServer(svc)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	defer srv.Close()
	call := func(payload map[string]any) map[string]any {
		body, _ := json.Marshal(payload)
		req, err := http.NewRequest(http.MethodPost, "http://"+ln.Addr().String()+mcpEndpoint(), bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("MCP-Protocol-Version", "2025-06-18")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return out
	}
	tl := call(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
	res, _ := tl["result"].(map[string]any)
	tools, _ := res["tools"].([]any)
	for _, want := range []string{"praector_collection_pr", "praector_collection_accept", "praector_collection_cleanup", "praector_init"} {
		if hasToolNamed(tools, want) {
			t.Fatalf("privileged tool %q must not be listed while disabled", want)
		}
	}
	if !hasToolNamed(tools, "praector_project_list") {
		t.Fatal("read-only parity tool must remain listed while privileged writes are disabled")
	}
	tc := call(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{"name": "praector_collection_accept", "arguments": map[string]any{"collection": "COL-none"}}})
	if tr, _ := tc["result"].(map[string]any); !strings.Contains(asText(tr), "privileged") {
		t.Fatalf("direct call to a disabled privileged tool must be refused: %v", tc)
	}
}

func asText(tr map[string]any) string {
	content, _ := tr["content"].([]any)
	if len(content) == 0 {
		return ""
	}
	c, _ := content[0].(map[string]any)
	s, _ := c["text"].(string)
	return s
}

// TestMCPBridgeNeverLaunchesProcesses is the invariant guard for DEC-1580dff0e0 /
// DEC-aacb7bfc23: neither the MCP core package nor the bridge wiring may
// construct a Scheduler or run a collection. Enqueue-only execution is enforced
// at the source level so no MCP path can bypass the single-owner invariant.
func TestMCPBridgeNeverLaunchesProcesses(t *testing.T) {
	dirs := []string{
		filepath.Join("mcp"),
	}
	scan := map[string]bool{"protocol.go": true, "server.go": true, "transport.go": true}
	forbidden := []string{"RunCollection", "NewScheduler", "ExclusiveScheduler", "Scheduler{", "ExecuteTask"}
	var files []string
	for _, d := range dirs {
		entries, err := os.ReadDir(d)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".go") && !strings.HasSuffix(e.Name(), "_test.go") && scan[e.Name()] {
				files = append(files, filepath.Join(d, e.Name()))
			}
		}
	}
	files = append(files, "mcp_server.go")
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, bad := range forbidden {
			if strings.Contains(string(b), bad) {
				t.Errorf("%s references forbidden symbol %q (MCP path must not launch processes)", f, bad)
			}
		}
	}
}

// helpers (avoid importing the mcp package value surface in this test file).
func mcpIsLoopback(addr string) bool {
	host, _, ok := strings.Cut(addr, ":")
	if !ok || host == "" || host == "localhost" || strings.HasPrefix(host, "127.") {
		return true
	}
	return host == "::1"
}

func mcpEndpoint() string { return "/mcp" }

func hasToolNamed(tools []any, name string) bool {
	for _, t := range tools {
		if m, _ := t.(map[string]any); m["name"] == name {
			return true
		}
	}
	return false
}
