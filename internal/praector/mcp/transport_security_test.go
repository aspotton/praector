package mcp

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// newReq builds a POST to the endpoint with the given Host and optional Origin,
// naming a supported protocol version so it reaches dispatch.
func newReq(host, origin string, body []byte) *http.Request {
	r := httptest.NewRequest(http.MethodPost, EndpointPath, bytes.NewReader(body))
	r.Host = host
	r.Header.Set(ProtocolVersionHeader, ProtocolVersion)
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	return r
}

// TestRequestAuthorizedLoopback proves the DNS-rebinding gate accepts loopback
// authorities (localhost / 127.0.0.1 / ::1, with and without a port) with or
// without a loopback Origin, and rejects foreign authorities / Origins.
func TestRequestAuthorizedLoopback(t *testing.T) {
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	// Loopback Hosts with no Origin are accepted (Host check only).
	for _, host := range []string{"127.0.0.1", "127.0.0.1:8765", "localhost", "localhost:8765", "[::1]:8765", "127.0.0.53:1"} {
		if !requestAuthorized(newReq(host, "", body)) {
			t.Errorf("Host %q must be accepted (loopback, no Origin)", host)
		}
	}
	// A loopback Origin whose port matches the Host port is accepted.
	for _, pair := range [][2]string{
		{"127.0.0.1:8765", "http://localhost:8765"},
		{"127.0.0.1:8765", "http://127.0.0.1:8765"},
		{"127.0.0.1:8765", "http://[::1]:8765"},
		{"localhost:8765", "http://localhost:8765"},
		{"[::1]:8765", "http://[::1]:8765"},
		{"127.0.0.1:8765", "null"},
	} {
		if !requestAuthorized(newReq(pair[0], pair[1], body)) {
			t.Errorf("Host %q + Origin %q must be accepted (loopback + matching port)", pair[0], pair[1])
		}
	}
	// Foreign authority or Origin, or a port mismatch, is refused.
	if requestAuthorized(newReq("evil.example:8765", "", body)) {
		t.Error("foreign Host must be refused")
	}
	if requestAuthorized(newReq("127.0.0.1:8765", "http://evil.example:8765", body)) {
		t.Error("foreign Origin must be refused")
	}
	if requestAuthorized(newReq("127.0.0.1:8765", "http://127.0.0.1:9999", body)) {
		t.Error("Origin with mismatched port must be refused (fail closed)")
	}
	if requestAuthorized(newReq("127.0.0.1", "http://evil.example", body)) {
		t.Error("foreign Origin on a loopback Host must be refused")
	}
}

// TestServeRejectsForeignAuthorityBeforeDispatch proves a foreign Host/Origin is a
// 403 that never reaches a tool/resource handler.
func TestServeRejectsForeignAuthorityBeforeDispatch(t *testing.T) {
	ts := httptest.NewServer(newTestServer().Handler())
	defer ts.Close()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "echo", "arguments": map[string]any{"a": 1}}})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+EndpointPath, io.NopCloser(bytes.NewReader(body)))
	req.Header.Set(ProtocolVersionHeader, ProtocolVersion)
	req.Header.Set("Origin", "http://evil.example")
	// Force the Host the server sees via the request URL host override.
	req.Host = "evil.example"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign authority must be 403 before dispatch, got %d", resp.StatusCode)
	}
}

// TestVersionNegotiationRefusesUnsupported proves initialize never echoes an
// unsupported revision: a supported/empty version advertises the implemented
// revision, while any other revision is refused with -32602 and a supported list.
func TestVersionNegotiationRefusesUnsupported(t *testing.T) {
	ts := httptest.NewServer(newTestServer().Handler())
	defer ts.Close()
	out := post(t, ts, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": "1999-01-01"}})
	e, _ := out["error"].(map[string]any)
	if e == nil || e["code"].(float64) != codeInvalidParams {
		t.Fatalf("unsupported version must yield -32602, got %v", out)
	}
	data, _ := e["data"].(map[string]any)
	supported, _ := data["supported"].([]any)
	if len(supported) != 1 || supported[0].(string) != ProtocolVersion {
		t.Fatalf("error must advertise the honest supported set, got %v", data)
	}
	if data["requested"] != "1999-01-01" {
		t.Fatalf("error should echo the requested value in data, got %v", data)
	}
	// An empty requested version still succeeds and advertises the implemented one.
	out = post(t, ts, map[string]any{"jsonrpc": "2.0", "id": 2, "method": "initialize", "params": map[string]any{}})
	res, _ := out["result"].(map[string]any)
	if res["protocolVersion"] != ProtocolVersion {
		t.Fatalf("empty version must advertise implemented revision, got %v", out)
	}
}

// TestProtocolVersionHeaderRequired proves a post-initialize request without a
// valid MCP-Protocol-Version header is a plain HTTP 400, while initialize itself
// may omit it.
func TestProtocolVersionHeaderRequired(t *testing.T) {
	ts := httptest.NewServer(newTestServer().Handler())
	defer ts.Close()

	postNoHeader := func(msg map[string]any) int {
		body, _ := json.Marshal(msg)
		req, _ := http.NewRequest(http.MethodPost, ts.URL+EndpointPath, bytes.NewReader(body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}
	if code := postNoHeader(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"}); code != http.StatusBadRequest {
		t.Fatalf("missing header on a tool call must be 400, got %d", code)
	}
	if code := postNoHeader(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "initialize", "params": map[string]any{"protocolVersion": ProtocolVersion}}); code == http.StatusBadRequest {
		t.Fatalf("initialize must be allowed to omit the header")
	}
	// A wrong header value is also 400.
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 3, "method": "tools/list"})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+EndpointPath, bytes.NewReader(body))
	req.Header.Set(ProtocolVersionHeader, "2024-11-05")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unsupported header value must be 400, got %d", resp.StatusCode)
	}
}

func TestLoopbackAuthorityUnit(t *testing.T) {
	for _, a := range []string{"127.0.0.1:1", "localhost", "[::1]:1", "127.0.0.1", "LocalHost:80"} {
		if !loopbackAuthority(a) {
			t.Errorf("loopbackAuthority(%q)=false, want true", a)
		}
	}
	for _, a := range []string{"evil.example", "evil.example:80", "0.0.0.0:80", "10.0.0.1:1", "8.8.8.8"} {
		if loopbackAuthority(a) {
			t.Errorf("loopbackAuthority(%q)=true, want false", a)
		}
	}
	if authorityPort("127.0.0.1:8765") != "8765" || authorityPort("127.0.0.1") != "" {
		t.Errorf("authorityPort mismatch")
	}
}
