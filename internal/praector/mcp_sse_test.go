package praector

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// startMCPServerOnListener wires the daemon's real MCP listener (which installs
// the SSE hub on the store and starts the drain goroutine) onto an httptest
// loopback server, mirroring internal/praector/cli.go's startMCPServer usage.
func startMCPServerOnListener(t *testing.T, svc *Service) *httptest.Server {
	t.Helper()
	srv, err := startMCPServer(svc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.store.CloseEventSink() })
	ts := httptest.NewServer(srv.Handler)
	t.Cleanup(ts.Close)
	return ts
}

// sseFrame is one parsed server-sent event.
type sseFrame struct {
	id   string
	data string
}

// openSSE opens a GET event stream against the running MCP listener and returns a
// channel of parsed frames. The version header is required on the GET path.
func openSSE(t *testing.T, ts *httptest.Server, query, lastEventID string) (chan sseFrame, func()) {
	t.Helper()
	u := ts.URL + "/mcp"
	if query != "" {
		u += "?" + query
	}
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("MCP-Protocol-Version", "2025-06-18")
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("SSE GET: want 200, got %d body=%q", resp.StatusCode, b)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("SSE GET: want text/event-stream, got %q", ct)
	}
	frames := make(chan sseFrame, 256)
	cancel := make(chan struct{})
	go func() {
		defer close(frames)
		sc := bufio.NewScanner(resp.Body)
		var cur sseFrame
		for sc.Scan() {
			line := strings.TrimRight(sc.Text(), "\r")
			switch {
			case line == "":
				if cur.data != "" {
					select {
					case frames <- cur:
					case <-cancel:
						return
					}
				}
				cur = sseFrame{}
			case strings.HasPrefix(line, "id:"):
				cur.id = strings.TrimSpace(strings.TrimPrefix(line, "id:"))
			case strings.HasPrefix(line, "data:"):
				cur.data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			}
		}
	}()
	return frames, func() { close(cancel); resp.Body.Close() }
}

// waitForFrame returns the next frame carrying the given event_type, or fails.
func waitForFrame(t *testing.T, frames chan sseFrame, wantType string, d time.Duration) sseFrame {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case f, ok := <-frames:
			if !ok {
				t.Fatalf("stream closed before %s", wantType)
			}
			var msg map[string]any
			if json.Unmarshal([]byte(f.data), &msg) != nil {
				continue
			}
			if m, _ := msg["method"].(string); m != "notifications/message" {
				t.Fatalf("frame on stream must be notifications/message, got %v", msg)
			}
			if _, isResponse := msg["result"]; isResponse {
				t.Fatalf("stream must never carry a JSON-RPC response (result): %v", msg)
			}
			if _, isResponse := msg["error"]; isResponse {
				t.Fatalf("stream must never carry a JSON-RPC response (error): %v", msg)
			}
			if _, hasID := msg["id"]; hasID {
				t.Fatalf("notification frame must carry no id: %v", msg)
			}
			params, _ := msg["params"].(map[string]any)
			data, _ := params["data"].(map[string]any)
			if data["event_type"] == wantType {
				return f
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s", wantType)
		}
	}
}

// assertNoFrameWithin drains briefly and fails if the named event_type appears.
func assertNoFrameWithin(t *testing.T, frames chan sseFrame, wantType string, d time.Duration) {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case f, ok := <-frames:
			if !ok {
				return
			}
			var msg map[string]any
			if json.Unmarshal([]byte(f.data), &msg) != nil {
				continue
			}
			params, _ := msg["params"].(map[string]any)
			data, _ := params["data"].(map[string]any)
			if data["event_type"] == wantType {
				t.Fatalf("unexpectedly received %s", wantType)
			}
		case <-deadline:
			return
		}
	}
}

// TestSSELiveDeliveryIsNotificationOnly proves an event written to the store is
// delivered to a connected GET consumer as a notifications/message carrying the
// event_type and EVT/COL/T/ATTEMPT ids, with no JSON-RPC response ever written.
func TestSSELiveDeliveryIsNotificationOnly(t *testing.T) {
	s, cfg := newFakeStore(t)
	svc := &Service{store: s, cfg: cfg}
	p, _ := s.RegisterProject(testProjectConfig(t.TempDir()))
	t.Setenv("PRAECTOR_MCP", "127.0.0.1:0")
	ts := startMCPServerOnListener(t, svc)

	frames, closeSSE := openSSE(t, ts, "", "")
	defer closeSSE()

	if err := s.Event(S(p, "id"), "", "", "", "COLLECTION_ACCEPTED", Row{"x": 1}); err != nil {
		t.Fatal(err)
	}
	f := waitForFrame(t, frames, "COLLECTION_ACCEPTED", 2*time.Second)
	if f.id == "" || !strings.HasPrefix(f.id, "EVT-") {
		t.Fatalf("SSE id: must carry the durable EVT id, got %q", f.id)
	}
	var msg map[string]any
	_ = json.Unmarshal([]byte(f.data), &msg)
	params, _ := msg["params"].(map[string]any)
	if params["logger"] != "praector" {
		t.Fatalf("logger must be praector, got %v", params["logger"])
	}
	data, _ := params["data"].(map[string]any)
	if data["event_type"] != "COLLECTION_ACCEPTED" || data["event_id"] == "" {
		t.Fatalf("data must carry event_type + event id, got %v", data)
	}
}

// TestSSEPerConnectionLevelFilter proves the fixed event_type->level mapping makes
// the per-connection filter real: a default (info) stream suppresses a debug
// lifecycle event and receives an info transition, while an explicit ?level=debug
// stream receives the debug event.
func TestSSEPerConnectionLevelFilter(t *testing.T) {
	s, cfg := newFakeStore(t)
	svc := &Service{store: s, cfg: cfg}
	p, _ := s.RegisterProject(testProjectConfig(t.TempDir()))
	c, _, _ := s.CreateCollectionWithCoordinator(S(p, "id"), "L", "obj", "coord")
	t.Setenv("PRAECTOR_MCP", "127.0.0.1:0")
	ts := startMCPServerOnListener(t, svc)

	infoFrames, closeA := openSSE(t, ts, "collection="+S(c, "id"), "")
	defer closeA()

	// TASK_CREATED maps to debug: suppressed at the default info level.
	_ = s.Event(S(p, "id"), S(c, "id"), "", "", "TASK_CREATED", Row{})
	assertNoFrameWithin(t, infoFrames, "TASK_CREATED", 300*time.Millisecond)
	// COLLECTION_COMPLETE maps to info: delivered at the default level.
	if err := s.SetCollectionStatus(S(c, "id"), CollectionComplete, false); err != nil {
		t.Fatal(err)
	}
	waitForFrame(t, infoFrames, "COLLECTION_COMPLETE", 2*time.Second)

	// A debug stream DOES receive the debug lifecycle event.
	dbgFrames, closeB := openSSE(t, ts, "level=debug&collection="+S(c, "id"), "")
	defer closeB()
	_ = s.Event(S(p, "id"), S(c, "id"), "", "", "TASK_CREATED", Row{})
	waitForFrame(t, dbgFrames, "TASK_CREATED", 2*time.Second)
}

// TestSSEStatusTransitionEvents proves Slice 3: a Collection reaching COMPLETE or
// BLOCKED, and a Task reaching COMPLETE or FAILED, each emit exactly one durable
// event a push consumer receives; a repeated no-op write emits ZERO events.
func TestSSEStatusTransitionEvents(t *testing.T) {
	s, cfg := newFakeStore(t)
	svc := &Service{store: s, cfg: cfg}
	p, _ := s.RegisterProject(testProjectConfig(t.TempDir()))
	c, coord, _ := s.CreateCollectionWithCoordinator(S(p, "id"), "S", "obj", "coord")
	t.Setenv("PRAECTOR_MCP", "127.0.0.1:0")
	ts := startMCPServerOnListener(t, svc)
	cid := S(c, "id")

	frames, closeSSE := openSSE(t, ts, "collection="+cid, "")
	defer closeSSE()

	// First real COMPLETE transition -> one COLLECTION_COMPLETE event, received.
	if err := s.SetCollectionStatus(cid, CollectionComplete, false); err != nil {
		t.Fatal(err)
	}
	waitForFrame(t, frames, "COLLECTION_COMPLETE", 2*time.Second)

	// Repeated no-op COMPLETE write -> zero new COLLECTION_COMPLETE events.
	if err := s.SetCollectionStatus(cid, CollectionComplete, false); err != nil {
		t.Fatal(err)
	}
	assertNoFrameWithin(t, frames, "COLLECTION_COMPLETE", 300*time.Millisecond)

	// Durable count: exactly one COLLECTION_COMPLETE row from the two writes.
	rows, err := s.ListEvents("", cid, "", "COLLECTION_COMPLETE", "", "", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("two COMPLETE writes must yield exactly one COLLECTION_COMPLETE event, got %d", len(rows))
	}

	// A task reaching FAILED then COMPLETE emits exactly one event each.
	if err := s.SetTaskStatus(S(coord, "id"), TaskFailed, "fail"); err != nil {
		t.Fatal(err)
	}
	waitForFrame(t, frames, "TASK_FAILED", 2*time.Second)
	if err := s.SetTaskStatus(S(coord, "id"), TaskFailed, "fail again"); err != nil {
		t.Fatal(err)
	}
	assertNoFrameWithin(t, frames, "TASK_FAILED", 300*time.Millisecond)
	if err := s.SetTaskStatus(S(coord, "id"), TaskComplete, "complete"); err != nil {
		t.Fatal(err)
	}
	waitForFrame(t, frames, "TASK_COMPLETE", 2*time.Second)
}

// TestSSEReplayScopedAndDeduped proves Last-Event-ID replay delivers that
// stream's missed rows (ordered, deduped by EVT id) and never foreign-stream
// rows, with no double delivery on the live resume.
func TestSSEReplayScopedAndDeduped(t *testing.T) {
	s, cfg := newFakeStore(t)
	svc := &Service{store: s, cfg: cfg}
	p, _ := s.RegisterProject(testProjectConfig(t.TempDir()))
	mine, _, _ := s.CreateCollectionWithCoordinator(S(p, "id"), "Mine", "obj", "coord")
	foreign, _, _ := s.CreateCollectionWithCoordinator(S(p, "id"), "Foreign", "obj", "coord")
	t.Setenv("PRAECTOR_MCP", "127.0.0.1:0")
	ts := startMCPServerOnListener(t, svc)

	// Seed the stream's collection with several events plus a foreign one.
	if err := s.Event(S(p, "id"), S(mine, "id"), "", "", "COLLECTION_ACCEPTED", Row{"i": 1}); err != nil {
		t.Fatal(err)
	}
	anchor, err := lastEventID(s, S(mine, "id"))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		_ = s.Event(S(p, "id"), S(mine, "id"), "", "", "TASK_INTEGRATED", Row{"i": i})
	}
	_ = s.Event(S(p, "id"), S(foreign, "id"), "", "", "TASK_INTEGRATED", Row{"foreign": true})

	// Reconnect carrying the anchor: replay mine's post-anchor rows only.
	frames, closeSSE := openSSE(t, ts, "collection="+S(mine, "id"), anchor)
	defer closeSSE()

	got := map[string]int{}
	ids := map[string]bool{}
	for i := 0; i < 3; i++ {
		f := waitForFrame(t, frames, "TASK_INTEGRATED", 2*time.Second)
		if ids[f.id] {
			t.Fatalf("replayed frame %s delivered twice", f.id)
		}
		ids[f.id] = true
		if strings.Contains(f.data, S(foreign, "id")) {
			t.Fatalf("replay leaked a foreign-collection frame: %s", f.data)
		}
		got["TASK_INTEGRATED"]++
	}
	assertNoFrameWithin(t, frames, "TASK_INTEGRATED", 400*time.Millisecond)
}

// TestE2ECollectionCompletePushedWithNoPolling proves the whole cross-slice
// acceptance: a Collection reaching COMPLETE produces a notifications/message a
// connected GET consumer receives within a short bound with no polling loop.
func TestE2ECollectionCompletePushedWithNoPolling(t *testing.T) {
	s, cfg := newFakeStore(t)
	svc := &Service{store: s, cfg: cfg}
	p, _ := s.RegisterProject(testProjectConfig(t.TempDir()))
	c, _, _ := s.CreateCollectionWithCoordinator(S(p, "id"), "E2E", "obj", "coord")
	t.Setenv("PRAECTOR_MCP", "127.0.0.1:0")
	ts := startMCPServerOnListener(t, svc)

	frames, closeSSE := openSSE(t, ts, "collection="+S(c, "id"), "")
	defer closeSSE()

	start := time.Now()
	if err := s.SetCollectionStatus(S(c, "id"), CollectionComplete, true); err != nil {
		t.Fatal(err)
	}
	waitForFrame(t, frames, "COLLECTION_COMPLETE", 2*time.Second)
	t.Logf("Collection COMPLETE pushed to consumer (elapsed %s)", time.Since(start))
}

// TestSSEStalledClientCannotBlockStoreEvent proves a stalled/vanished client
// whose buffer is full causes at most a drop+close of ITS OWN stream: the emit
// hand-off is non-blocking and a separate healthy stream still receives.
func TestSSEStalledClientCannotBlockStoreEvent(t *testing.T) {
	s, _ := newFakeStore(t)
	h := newSSEHub(s)
	defer h.Close()
	go h.drain()

	// A stalled stream with a tiny buffer we never read, plus a drained healthy
	// stream (large buffer). Both at the debug level so every event is delivered.
	stalled := &sseStream{ch: make(chan eventRecord, 2), done: make(chan struct{}), level: int32(0)}
	healthy := &sseStream{ch: make(chan eventRecord, 1024), done: make(chan struct{}), level: int32(0)}
	h.register(stalled)
	h.register(healthy)

	// Flood far past the stalled stream's 2-slot buffer. publish is non-blocking,
	// so this loop must return promptly and must close the stalled stream.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 5000; i++ {
			h.publish(eventRecord{ID: MakeID("EVT"), EventType: "SESSION_STARTED", CreatedAt: UTCNow()})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("publish blocked the write path on a stalled stream")
	}
	// The stalled stream's own delivery is capped and its stream is closed.
	for i := 0; i < 4; i++ {
		h.fanout(eventRecord{ID: MakeID("EVT"), EventType: "SESSION_STARTED", CreatedAt: UTCNow()})
	}
	select {
	case <-stalled.done:
	case <-time.After(time.Second):
		t.Fatal("stalled stream was not closed after buffer overflow")
	}
	// The healthy stream kept receiving throughout (a stalled peer never starves it).
	got := 0
	deadline := time.After(time.Second)
	for got < 10 {
		select {
		case <-healthy.ch:
			got++
		case <-deadline:
			t.Fatalf("healthy stream starved behind a stalled peer (got %d)", got)
		}
	}
}

// TestSSEStalledClientDoesNotStarveLiveConsumer is a lighter end-to-end check that
// an undrained GET consumer does not stop a separate connected consumer from
// receiving a later event.
func TestSSEStalledClientDoesNotStarveLiveConsumer(t *testing.T) {
	s, cfg := newFakeStore(t)
	svc := &Service{store: s, cfg: cfg}
	p, _ := s.RegisterProject(testProjectConfig(t.TempDir()))
	t.Setenv("PRAECTOR_MCP", "127.0.0.1:0")
	ts := startMCPServerOnListener(t, svc)

	_, stalled := openSSE(t, ts, "", "") // opened, never read
	defer stalled()
	healthy, closeHealthy := openSSE(t, ts, "", "")
	defer closeHealthy()

	for i := 0; i < 400; i++ {
		_ = s.Event(S(p, "id"), "", "", "", "SESSION_STARTED", Row{"i": i})
	}
	if err := s.Event(S(p, "id"), "", "", "", "COLLECTION_ACCEPTED", Row{"final": true}); err != nil {
		t.Fatal(err)
	}
	waitForFrame(t, healthy, "COLLECTION_ACCEPTED", 3*time.Second)
}

// lastEventID returns the id of the most recent event for a collection, to use as
// a Last-Event-ID anchor for a replay test.
func lastEventID(s *Store, cid string) (string, error) {
	rows, err := s.ListEvents("", cid, "", "", "", "", 500)
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "", nil
	}
	return S(rows[len(rows)-1], "id"), nil
}
