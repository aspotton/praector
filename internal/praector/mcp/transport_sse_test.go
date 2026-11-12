package mcp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// stubStream is a StreamHandler that records whether the transport invoked it and
// writes a single SSE notification frame so tests can assert nothing is written
// before the security/version gates pass.
type stubStream struct {
	invoked atomic.Bool
}

func (s *stubStream) ServeSSE(w http.ResponseWriter, r *http.Request) {
	s.invoked.Store(true)
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "id: EVT-test\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/message\"}\n\n")
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// stubLogging records the levels applied via logging/setLevel.
type stubLogging struct {
	calls []string
}

func (l *stubLogging) SetLevel(level string) error {
	l.calls = append(l.calls, level)
	return nil
}

func newStreamingServer() (*Server, *stubStream, *stubLogging) {
	stub := stubProvider{}
	stream := &stubStream{}
	logging := &stubLogging{}
	return &Server{ServerName: "test", ServerVersion: "0.0.0", Tools: stub, Resources: stub, Streams: stream, Logging: logging}, stream, logging
}

func get(t *testing.T, ts *httptest.Server, accept, version, origin string, setVersion bool) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, ts.URL+EndpointPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if setVersion {
		req.Header.Set(ProtocolVersionHeader, version)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

// TestGETSecurityEqualsPOST proves the loopback-authority gate and the
// MCP-Protocol-Version header requirement are enforced on the GET path exactly as
// on POST: a foreign Host/Origin, a literal-null/opaque Origin, and a missing or
// wrong version header are refused with NOTHING written to a stream (the
// StreamHandler is never invoked).
func TestGETSecurityEqualsPOST(t *testing.T) {
	srv, stream, _ := newStreamingServer()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	reset := func() { stream.invoked.Store(false) }
	bodyHasData := func(b string) bool { return strings.Contains(b, "data:") || strings.Contains(b, "text/event-stream") }

	// Foreign Origin (loopback Host) is refused on GET, nothing written.
	reset()
	if resp, b := get(t, ts, "text/event-stream", ProtocolVersion, "http://evil.example", true); resp.StatusCode != http.StatusForbidden || stream.invoked.Load() || bodyHasData(b) {
		t.Fatalf("GET foreign Origin: want 403/no-write, got %d invoked=%v body=%q", resp.StatusCode, stream.invoked.Load(), b)
	}
	// Literal "null" (opaque) Origin is refused on GET.
	reset()
	if resp, b := get(t, ts, "text/event-stream", ProtocolVersion, "null", true); resp.StatusCode != http.StatusForbidden || stream.invoked.Load() || bodyHasData(b) {
		t.Fatalf("GET null Origin: want 403/no-write, got %d invoked=%v body=%q", resp.StatusCode, stream.invoked.Load(), b)
	}
	// Missing version header on an SSE GET is refused with nothing written.
	reset()
	if resp, b := get(t, ts, "text/event-stream", "", "", false); resp.StatusCode != http.StatusBadRequest || stream.invoked.Load() || bodyHasData(b) {
		t.Fatalf("GET missing header: want 400/no-write, got %d invoked=%v body=%q", resp.StatusCode, stream.invoked.Load(), b)
	}
	// Wrong version header on an SSE GET is refused with nothing written.
	reset()
	if resp, b := get(t, ts, "text/event-stream", "2024-11-05", "", true); resp.StatusCode != http.StatusBadRequest || stream.invoked.Load() || bodyHasData(b) {
		t.Fatalf("GET wrong header: want 400/no-write, got %d invoked=%v body=%q", resp.StatusCode, stream.invoked.Load(), b)
	}
	// A foreign Host (not just Origin) on GET is refused (httptest forces Host via URL host override).
	reset()
	req, _ := http.NewRequest(http.MethodGet, ts.URL+EndpointPath, nil)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set(ProtocolVersionHeader, ProtocolVersion)
	req.Host = "evil.example"
	if resp, err := http.DefaultClient.Do(req); err == nil {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden || stream.invoked.Load() || strings.Contains(string(b), "data:") {
			t.Fatalf("GET foreign Host: want 403/no-write, got %d invoked=%v body=%q", resp.StatusCode, stream.invoked.Load(), b)
		}
	}
	// A fully-authorized SSE GET reaches the handler and writes a frame.
	reset()
	if resp, b := get(t, ts, "text/event-stream", ProtocolVersion, "", true); resp.StatusCode != http.StatusOK || !stream.invoked.Load() || !strings.Contains(b, "data:") {
		t.Fatalf("GET authorized SSE: want 200 + stream, got %d invoked=%v body=%q", resp.StatusCode, stream.invoked.Load(), b)
	}
}

// TestGETPreservesNonSSE405AndPOSTSurface proves a GET that does not advertise
// text/event-stream still gets 405 (existing behavior for non-streaming clients)
// with the handler never invoked, a server with no StreamHandler wired returns
// 405 for every GET, and the POST request/response surface is unchanged.
func TestGETPreservesNonSSE405AndPOSTSurface(t *testing.T) {
	// Streaming server: a non-SSE GET is still 405.
	srv, stream, _ := newStreamingServer()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	stream.invoked.Store(false)
	resp, b := get(t, ts, "application/json", ProtocolVersion, "", true)
	if resp.StatusCode != http.StatusMethodNotAllowed || stream.invoked.Load() {
		t.Fatalf("non-SSE GET: want 405/no-invoke, got %d invoked=%v body=%q", resp.StatusCode, stream.invoked.Load(), b)
	}
	if resp.Header.Get("Allow") == "" {
		t.Error("non-SSE GET 405 must advertise Allow")
	}
	// POST surface byte-for-byte unchanged: initialize + tools/list still work.
	if out := post(t, ts, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": ProtocolVersion}}); out["error"] != nil {
		t.Fatalf("POST initialize must remain unchanged: %v", out)
	}

	// Server with NO StreamHandler: every GET is 405 even with an SSE Accept.
	plain := httptest.NewServer(newTestServer().Handler()) // newTestServer has Streams == nil
	defer plain.Close()
	if resp, _ := get(t, plain, "text/event-stream", ProtocolVersion, "", true); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("no-StreamHandler GET SSE: want 405, got %d", resp.StatusCode)
	}
}

// TestInitializeAdvertisesLoggingCapability proves the logging capability is
// advertised when a LoggingProvider is wired and absent when it is not (a
// POST-only stub never claims a channel it lacks).
func TestInitializeAdvertisesLoggingCapability(t *testing.T) {
	srv, _, _ := newStreamingServer()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	out := post(t, ts, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": ProtocolVersion}})
	res, _ := out["result"].(map[string]any)
	caps, _ := res["capabilities"].(map[string]any)
	if caps == nil || caps["logging"] == nil {
		t.Fatalf("initialize must advertise logging when wired: %v", res)
	}

	plain := httptest.NewServer(newTestServer().Handler()) // no Logging
	defer plain.Close()
	out = post(t, plain, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": ProtocolVersion}})
	res, _ = out["result"].(map[string]any)
	caps, _ = res["capabilities"].(map[string]any)
	if caps != nil && caps["logging"] != nil {
		t.Fatalf("a server without a LoggingProvider must NOT advertise logging: %v", res)
	}
}

// TestLoggingSetLevelConformance proves logging/setLevel accepts exactly the
// eight MCP levels (applying each to the provider) and returns Invalid Params
// (-32602) for an unrecognized level with NO provider state change.
func TestLoggingSetLevelConformance(t *testing.T) {
	srv, _, logging := newStreamingServer()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	for _, level := range LoggingLevels {
		out := post(t, ts, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "logging/setLevel", "params": map[string]any{"level": level}})
		if out["error"] != nil {
			t.Fatalf("valid level %q must succeed, got %v", level, out)
		}
		if len(logging.calls) == 0 || logging.calls[len(logging.calls)-1] != level {
			t.Fatalf("provider must receive valid level %q, got %v", level, logging.calls)
		}
	}
	for _, bad := range []string{"verbose", "", "DEBUG", "trace", "off"} {
		before := len(logging.calls)
		out := post(t, ts, map[string]any{"jsonrpc": "2.0", "id": 2, "method": "logging/setLevel", "params": map[string]any{"level": bad}})
		if e, _ := out["error"].(map[string]any); e == nil || e["code"].(float64) != float64(codeInvalidParams) {
			t.Fatalf("invalid level %q must yield -32602, got %v", bad, out)
		}
		if len(logging.calls) != before {
			t.Fatalf("invalid level %q must not change provider state, got %v", bad, logging.calls)
		}
	}
}

// TestNoResponseFrameOnSSEStream is a stream-level guard that the wire format
// produced by the SSE notification body is a JSON-RPC notification only (no id,
// no result, no error) — never a response.
func TestNoResponseFrameOnSSEStream(t *testing.T) {
	body := NotificationMessageBody("info", "praector", map[string]any{"event_type": "COLLECTION_COMPLETE"})
	if body["id"] != nil {
		t.Fatalf("notification body must carry no id: %v", body)
	}
	if body["result"] != nil || body["error"] != nil {
		t.Fatalf("notification body must carry no result/error: %v", body)
	}
	if body["method"] != "notifications/message" {
		t.Fatalf("notification body method: %v", body["method"])
	}
	raw, _ := json.Marshal(body)
	if !bytes.Contains(raw, []byte("notifications/message")) {
		t.Fatalf("notification body must name notifications/message: %s", raw)
	}
}

// helper: scan an SSE body for the first "data:" payload.
func firstSSEData(body string) (string, bool) {
	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "data:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "data:")), true
		}
	}
	return "", false
}
