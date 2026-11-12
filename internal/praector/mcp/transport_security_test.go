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
	} {
		if !requestAuthorized(newReq(pair[0], pair[1], body)) {
			t.Errorf("Host %q + Origin %q must be accepted (loopback + matching port)", pair[0], pair[1])
		}
	}
	// A literal "null" (opaque/unique) origin names no loopback authority and is
	// refused even on a loopback Host (never treated as an absent Origin).
	for _, opaque := range []string{"null", "file://", "bad uri", "javascript:alert(1)", "http://:8765", "http://evil.example:8765", "http://127.0.0.1:9999"} {
		if requestAuthorized(newReq("127.0.0.1:8765", opaque, body)) {
			t.Errorf("opaque/foreign Origin %q must be refused on a loopback Host", opaque)
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

// TestServeRejectsOpaqueOriginBeforeDispatch proves a literal Origin "null" is a
// 403 that never reaches a tool/resource handler, while an absent Origin on a
// loopback Host is allowed through to dispatch (Host check only).
func TestServeRejectsOpaqueOriginBeforeDispatch(t *testing.T) {
	ts := httptest.NewServer(newTestServer().Handler())
	defer ts.Close()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
	postWithOrigin := func(origin string) int {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+EndpointPath, bytes.NewReader(body))
		req.Header.Set(ProtocolVersionHeader, ProtocolVersion)
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}
	// httptest serves on a 127.0.0.1 Host, so an absent Origin passes the Host
	// check and reaches dispatch (200), proving absent Origin is not refused.
	if code := postWithOrigin(""); code != http.StatusOK {
		t.Fatalf("absent Origin on a loopback Host must reach dispatch (200), got %d", code)
	}
	if code := postWithOrigin("null"); code != http.StatusForbidden {
		t.Fatalf("Origin \"null\" must be 403 before dispatch, got %d", code)
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

// TestVersionNegotiationCountersOffers proves 2025-06-18 negotiation
// (DEC-0edf905672): a well-formed initialize naming a non-empty older, newer, or
// unknown revision is met with a SUCCESSFUL result advertising the one
// implemented revision (2025-06-18) and NEVER echoes the client's value. The
// Invalid-params error -32602 is returned for a missing/empty protocolVersion, a
// present-but-non-string protocolVersion, or a malformed body — the missing/empty
// edge is a fail-closed hardening (DEC-12a3ff57db), not a spec mandate.
func TestVersionNegotiationCountersOffers(t *testing.T) {
	ts := httptest.NewServer(newTestServer().Handler())
	defer ts.Close()
	for _, version := range []string{ProtocolVersion, "1999-01-01", "2024-11-05", "2099-12-31", "bogus"} {
		out := post(t, ts, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": version}})
		if out["error"] != nil {
			t.Fatalf("well-formed unsupported version %q must counter-offer (success), got %v", version, out)
		}
		res, _ := out["result"].(map[string]any)
		if res["protocolVersion"] != ProtocolVersion {
			t.Fatalf("counter-offer for %q must advertise the implemented revision, got %v", version, out)
		}
		if res["protocolVersion"] == version && version != ProtocolVersion {
			t.Fatalf("server must never echo the client revision %q", version)
		}
	}
	// An empty-string protocolVersion is a fail-closed Invalid-params error, not a
	// counter-offer success (DEC-12a3ff57db).
	out := post(t, ts, map[string]any{"jsonrpc": "2.0", "id": 2, "method": "initialize", "params": map[string]any{"protocolVersion": ""}})
	if e, _ := out["error"].(map[string]any); e == nil || e["code"].(float64) != codeInvalidParams {
		t.Fatalf("empty protocolVersion must yield -32602, got %v", out)
	}
	// A present-but-non-string protocolVersion is a malformed Invalid-params error,
	// not a counter-offer success.
	out = post(t, ts, map[string]any{"jsonrpc": "2.0", "id": 3, "method": "initialize", "params": map[string]any{"protocolVersion": 12345}})
	if e, _ := out["error"].(map[string]any); e == nil || e["code"].(float64) != codeInvalidParams {
		t.Fatalf("non-string protocolVersion must yield -32602, got %v", out)
	}
}

// TestVersionNegotiationMissingProtocolVersion proves a MISSING protocolVersion
// field (empty params, or params without the key) is a fail-closed -32602
// Invalid-params error (DEC-12a3ff57db), not a counter-offer success.
func TestVersionNegotiationMissingProtocolVersion(t *testing.T) {
	ts := httptest.NewServer(newTestServer().Handler())
	defer ts.Close()
	for _, params := range []any{
		map[string]any{},
		map[string]any{"other": "x"},
	} {
		out := post(t, ts, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": params})
		if e, _ := out["error"].(map[string]any); e == nil || e["code"].(float64) != codeInvalidParams {
			t.Fatalf("missing protocolVersion %#v must yield -32602, got %v", params, out)
		}
	}
}

// TestVersionNegotiationMalformedBody proves a non-string protocolVersion is a
// -32602 Invalid-params error and never reaches the counter-offer success path
// (server.go no longer swallows a malformed body as empty params).
func TestVersionNegotiationMalformedBody(t *testing.T) {
	ts := httptest.NewServer(newTestServer().Handler())
	defer ts.Close()
	for _, params := range []any{
		map[string]any{"protocolVersion": []string{"2025-06-18"}},
		map[string]any{"protocolVersion": map[string]any{"v": "x"}},
		map[string]any{"protocolVersion": true},
	} {
		out := post(t, ts, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": params})
		if e, _ := out["error"].(map[string]any); e == nil || e["code"].(float64) != codeInvalidParams {
			t.Fatalf("malformed protocolVersion %#v must yield -32602, got %v", params, out)
		}
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
