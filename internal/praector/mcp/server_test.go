package mcp

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

type stubProvider struct{}

func (stubProvider) ListTools() []Tool {
	return []Tool{{Name: "echo", Description: "echoes", InputSchema: map[string]any{"type": "object"}}}
}

func (stubProvider) CallTool(name string, args json.RawMessage) (ToolResult, error) {
	if name != "echo" {
		return ToolResult{}, errUnknownTool(name)
	}
	return ToolResult{Content: []ContentItem{{Type: "text", Text: string(args)}}}, nil
}

func (stubProvider) ListResources() []Resource {
	return []Resource{{URI: "stub://x", Name: "x"}}
}

func (stubProvider) ReadResource(uri string) ([]ResourceContents, error) {
	if uri != "stub://x" {
		return nil, errUnknownResource(uri)
	}
	return []ResourceContents{{URI: uri, Text: "hello"}}, nil
}

type errString string

func (e errString) Error() string { return string(e) }

func errUnknownTool(n string) error     { return errString("unknown tool: " + n) }
func errUnknownResource(u string) error { return errString("unknown resource: " + u) }

func newTestServer() *Server {
	s := stubProvider{}
	return &Server{ServerName: "test", ServerVersion: "0.0.0", Tools: s, Resources: s}
}

// post sends one JSON-RPC message to the streamable handler and decodes the
// single JSON-RPC response.
func post(t *testing.T, srv *httptest.Server, msg map[string]any) map[string]any {
	t.Helper()
	body, _ := json.Marshal(msg)
	req, err := http.NewRequest(http.MethodPost, srv.URL+EndpointPath, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	// Post-initialize requests must name the protocol revision; Praector does not
	// implement pre-2025-06-18 clients and rejects a missing header.
	req.Header.Set(ProtocolVersionHeader, ProtocolVersion)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d for %v", resp.StatusCode, msg["method"])
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestMCPInitializeRoundTrip(t *testing.T) {
	ts := httptest.NewServer(newTestServer().Handler())
	defer ts.Close()
	out := post(t, ts, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": ProtocolVersion}})
	if out["error"] != nil {
		t.Fatalf("initialize error: %v", out["error"])
	}
	res, _ := out["result"].(map[string]any)
	if res["protocolVersion"] != ProtocolVersion {
		t.Fatalf("protocolVersion: %v", res)
	}
	si, _ := res["serverInfo"].(map[string]any)
	if si["name"] != "test" {
		t.Fatalf("serverInfo: %v", res)
	}
}

func TestMCPToolsListAndCall(t *testing.T) {
	ts := httptest.NewServer(newTestServer().Handler())
	defer ts.Close()
	out := post(t, ts, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
	res, _ := out["result"].(map[string]any)
	tools, _ := res["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != "echo" {
		t.Fatalf("tools/list: %v", out)
	}
	out = post(t, ts, map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{"name": "echo", "arguments": map[string]any{"a": 1}}})
	res, _ = out["result"].(map[string]any)
	if res["isError"] == true {
		t.Fatalf("unexpected tool error: %v", out)
	}
	out = post(t, ts, map[string]any{"jsonrpc": "2.0", "id": 3, "method": "tools/call", "params": map[string]any{"name": "nope"}})
	res, _ = out["result"].(map[string]any)
	if res["isError"] != true {
		t.Fatalf("unknown tool must report isError: %v", out)
	}
}

func TestMCPResourcesAndPingAndMethodNotFound(t *testing.T) {
	ts := httptest.NewServer(newTestServer().Handler())
	defer ts.Close()
	out := post(t, ts, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "ping"})
	if out["error"] != nil {
		t.Fatalf("ping: %v", out)
	}
	out = post(t, ts, map[string]any{"jsonrpc": "2.0", "id": 2, "method": "resources/read", "params": map[string]any{"uri": "stub://x"}})
	res, _ := out["result"].(map[string]any)
	c, _ := res["contents"].([]any)
	if len(c) != 1 || c[0].(map[string]any)["text"] != "hello" {
		t.Fatalf("resources/read: %v", out)
	}
	out = post(t, ts, map[string]any{"jsonrpc": "2.0", "id": 3, "method": "bogus/method"})
	if e, _ := out["error"].(map[string]any); e == nil || e["code"].(float64) != codeMethodNotFound {
		t.Fatalf("expected method-not-found: %v", out)
	}
}

func TestMCPNotificationReturnsNoBody(t *testing.T) {
	ts := httptest.NewServer(newTestServer().Handler())
	defer ts.Close()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	req, err := http.NewRequest(http.MethodPost, ts.URL+EndpointPath, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(ProtocolVersionHeader, ProtocolVersion)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("notification status: %d", resp.StatusCode)
	}
}

func TestIsLoopbackAddr(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:8080": true,
		"127.0.0.1:0":    true,
		"localhost:8080": true,
		"[::1]:8080":     true,  // bracketed IPv6 loopback
		":8080":          false, // empty host binds the wildcard (all interfaces)
		":0":             false, // empty host binds the wildcard (all interfaces)
		"::1:8080":       false, // malformed host:port for SplitHostPort; host becomes weird
		"0.0.0.0:8080":   false,
		"10.0.0.5:8080":  false,
		"example.com:80": false,
	}
	for addr, want := range cases {
		if got := IsLoopbackAddr(addr); got != want {
			t.Errorf("IsLoopbackAddr(%q)=%v want %v", addr, got, want)
		}
	}
}

// TestEmptyHostAddrNeverBindsLoopbackOnly guards the loopback-only security
// guarantee (DEC-aacb7bfc23): a bare ":port" (empty host) is bound by Go to the
// wildcard 0.0.0.0/[::] (every interface), so the daemon's sole construction
// gate must refuse it rather than start an unauthenticated listener reachable
// beyond the loopback interface.
func TestEmptyHostAddrNeverBindsLoopbackOnly(t *testing.T) {
	// First confirm the premise on this platform: Go binds an empty-host listen
	// to a non-loopback interface. If a platform happens to bind it to loopback,
	// there is nothing to guard and the check is skipped rather than flaky.
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Skipf("cannot bind ephemeral empty-host listener: %v", err)
	}
	premiseNonLoopback := true
	if ta, ok := ln.Addr().(*net.TCPAddr); ok {
		if ip := net.ParseIP(ta.IP.String()); ip != nil && ip.IsLoopback() {
			premiseNonLoopback = false
		}
	}
	_ = ln.Close()
	if !premiseNonLoopback {
		t.Skip("platform bound an empty-host listener to loopback; nothing to guard")
	}

	for _, addr := range []string{":8080", ":0"} {
		if IsLoopbackAddr(addr) {
			t.Errorf("IsLoopbackAddr(%q)=true; an empty host binds the wildcard and must be refused", addr)
		}
		if _, err := NewLoopbackServer(addr, newTestServer()); err == nil {
			t.Errorf("NewLoopbackServer(%q) accepted an empty-host (wildcard) bind; the daemon could reach a non-loopback interface", addr)
		}
	}
}

func TestNewLoopbackServerRefusesNonLoopback(t *testing.T) {
	if _, err := NewLoopbackServer("0.0.0.0:9999", newTestServer()); err == nil {
		t.Fatal("expected refusal for non-loopback bind")
	}
	if _, err := NewLoopbackServer("127.0.0.1:0", newTestServer()); err != nil {
		t.Fatalf("expected acceptance for loopback bind: %v", err)
	}
}
