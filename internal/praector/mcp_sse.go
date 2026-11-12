package praector

import (
	"bytes"
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/datascape/praector/internal/praector/mcp"
)

// sseHub is the Praector-side wiring that gives the domain-agnostic MCP core an
// observation-only SSE stream (DEC-17fa4b4aff: all stream/level/replay state
// lives here in the wiring layer, never inside internal/praector/mcp). It is:
//
//   - the mcp.StreamHandler the transport hands a gate-passed GET to;
//   - the mcp.LoggingProvider that applies logging/setLevel to active streams;
//   - the store's eventSink that receives the non-blocking fan-out hand-off.
//
// Delivery is observation-only: opening a stream never implies a mutation, never
// launches a scheduler/process, and never bypasses policy/review/acceptance. The
// loopback-authority and MCP-Protocol-Version gates are enforced by the transport
// before ServeSSE is ever called, identical to POST.
type sseHub struct {
	store *Store

	// pending is the single bounded side-channel every event writer hands a
	// fan-out payload to. publish never blocks on it; the drain goroutine is the
	// only place that touches per-stream buffers, and it runs OUTSIDE the Dolt
	// file lock, so a write path never holds the lock across a stream publish.
	pending chan eventRecord
	closed  chan struct{}

	mu      sync.Mutex
	streams map[*sseStream]struct{}

	// Caps form a local DoS guard (loopback is still multi-socket): bounded
	// concurrent streams, bounded per-stream buffer, bounded per-reconnect replay.
	maxStreams   int
	streamBuffer int
	replayLimit  int
}

type sseStream struct {
	ch   chan eventRecord
	done chan struct{}

	// level is the effective logging level rank (mcp.LevelRank), atomic so
	// logging/setLevel can retune a live stream without a lock.
	level int32

	// replay cursor (created_at,id): live records at or below it are deduped
	// against the Last-Event-ID replay already written for this stream.
	cursorTime string
	cursorID   string

	// optional durable scope filters, applied to both replay and live delivery so
	// a stream never receives another stream's foreign rows.
	filter struct{ project, collection, eventType string }
}

// SSE stream tuning (overridable in tests).
const (
	defaultMaxStreams   = 64
	defaultStreamBuffer = 128
	defaultReplayLimit  = 2000
	defaultSSELevel     = "info"
	sseLoggerName       = "praector"
)

func newSSEHub(store *Store) *sseHub {
	return &sseHub{
		store:        store,
		pending:      make(chan eventRecord, defaultStreamBuffer*8),
		closed:       make(chan struct{}),
		streams:      map[*sseStream]struct{}{},
		maxStreams:   defaultMaxStreams,
		streamBuffer: defaultStreamBuffer,
		replayLimit:  defaultReplayLimit,
	}
}

// drain is the single fan-out goroutine: it reads the bounded side-channel and
// performs the per-stream, non-blocking delivery. It is the only code that
// touches stream buffers, and it runs concurrently with (never inside) the store
// write path, so the Dolt file lock is never held across a publish.
func (h *sseHub) drain() {
	for {
		select {
		case <-h.closed:
			return
		case rec := <-h.pending:
			h.fanout(rec)
		}
	}
}

// fanout delivers one record to every stream whose level filter accepts it. A
// stalled/slow/vanished client that cannot accept into its bounded buffer is
// dropped and closed, never stalling the others.
func (h *sseHub) fanout(rec eventRecord) {
	h.mu.Lock()
	targets := make([]*sseStream, 0, len(h.streams))
	stalled := make([]*sseStream, 0)
	for s := range h.streams {
		targets = append(targets, s)
	}
	h.mu.Unlock()

	lvl := eventLevel(rec.EventType)
	rank := int32(mcp.LevelRank(lvl))
	for _, s := range targets {
		if rank < atomic.LoadInt32(&s.level) {
			continue // below this stream's effective level
		}
		if !s.matches(rec) {
			continue // out of this stream's durable scope
		}
		select {
		case s.ch <- rec:
		default:
			stalled = append(stalled, s)
		}
	}
	for _, s := range stalled {
		h.closeStream(s) // drop + close: a stalled client costs at most its own stream
	}
}

// publish is the store emit seam: a strictly non-blocking hand-off. If the
// side-channel is momentarily full the payload is dropped (durability comes from
// the events table + Last-Event-ID replay), never a stall on the write path.
func (h *sseHub) publish(rec eventRecord) {
	select {
	case <-h.closed:
		return
	default:
	}
	select {
	case h.pending <- rec:
	default:
		// Best-effort live delivery only; a slow drain must never block a writer.
	}
}

// SetLevel implements mcp.LoggingProvider: apply the verbosity to the currently
// attributable active stream(s). It is exact for a single observer and
// documented best-effort (last-writer-wins broadcast) across many, with NO
// session-id/auth/correlation routing token (explicit MUST NOT).
func (h *sseHub) SetLevel(level string) error {
	if !mcp.ValidLoggingLevel(level) {
		return nil // the core already rejects an invalid level; keep state unchanged
	}
	rank := int32(mcp.LevelRank(level))
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.streams {
		atomic.StoreInt32(&s.level, rank)
	}
	return nil
}

func (h *sseHub) register(s *sseStream) {
	h.mu.Lock()
	h.streams[s] = struct{}{}
	h.mu.Unlock()
}

// closeStream removes a stream from the registry and signals its pump to stop. It
// never closes s.ch (the drain may still hold a send), only s.done.
func (h *sseHub) closeStream(s *sseStream) {
	h.mu.Lock()
	if _, ok := h.streams[s]; ok {
		delete(h.streams, s)
		close(s.done)
	}
	h.mu.Unlock()
}

// Close implements the store's optional eventSink closer: it stops the drain and
// terminates every open stream so daemon shutdown does not leak GET handlers
// (http.Server.Close does not unblock a long-lived GET handler).
func (h *sseHub) Close() {
	select {
	case <-h.closed:
		return
	default:
		close(h.closed)
	}
	h.mu.Lock()
	list := make([]*sseStream, 0, len(h.streams))
	for s := range h.streams {
		list = append(list, s)
	}
	h.mu.Unlock()
	for _, s := range list {
		h.closeStream(s)
	}
}

// ServeSSE implements mcp.StreamHandler. The transport has already enforced the
// loopback-authority gate and the MCP-Protocol-Version header before calling
// this, so it never writes a byte for an unauthorized request.
func (h *sseHub) ServeSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	s := &sseStream{
		ch:    make(chan eventRecord, h.streamBuffer),
		done:  make(chan struct{}),
		level: int32(mcp.LevelRank(defaultSSELevel)),
	}
	s.filter.project = r.URL.Query().Get("project")
	s.filter.collection = r.URL.Query().Get("collection")
	s.filter.eventType = r.URL.Query().Get("type")
	if lvl := r.URL.Query().Get("level"); mcp.ValidLoggingLevel(lvl) {
		atomic.StoreInt32(&s.level, int32(mcp.LevelRank(lvl)))
	}

	// Local DoS guard on concurrent streams.
	if !h.tryRegister(s) {
		http.Error(w, "too many event streams", http.StatusServiceUnavailable)
		return
	}
	defer h.closeStream(s)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	// A priming comment flushes headers so the client sees the 200 immediately
	// (a comment, never a JSON-RPC response frame).
	_, _ = w.Write([]byte(": connected\n\n"))
	flusher.Flush()

	// Replay anything this stream missed since its Last-Event-ID, scoped strictly
	// to the stream's own durable filter and deduped by id.
	h.replay(w, flusher, s, r.Header.Get("Last-Event-ID"))

	// Then stream live records until the client disconnects or the daemon stops.
	for {
		select {
		case <-r.Context().Done():
			return
		case <-h.closed:
			return
		case <-s.done:
			return
		case rec := <-s.ch:
			if s.beforeCursor(rec) {
				continue // already delivered by replay
			}
			if !writeSSE(w, rec) {
				return
			}
			flusher.Flush()
		}
	}
}

// tryRegister enforces the concurrent-stream cap.
func (h *sseHub) tryRegister(s *sseStream) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	select {
	case <-h.closed:
		return false
	default:
	}
	if len(h.streams) >= h.maxStreams {
		return false
	}
	h.streams[s] = struct{}{}
	return true
}

// replay reads the events table for this stream's scope after the Last-Event-ID
// and writes each matching notification. A last id that cannot be resolved (or is
// absent) replays nothing rather than guessing a cursor (fail closed).
func (h *sseHub) replay(w http.ResponseWriter, flusher http.Flusher, s *sseStream, lastID string) {
	if lastID == "" {
		return
	}
	createdAt, found, err := h.store.EventCreatedAt(lastID)
	if err != nil || !found {
		return
	}
	rows, err := h.store.ListEvents(s.filter.project, s.filter.collection, "", s.filter.eventType, createdAt, lastID, h.replayLimit)
	if err != nil {
		return
	}
	s.cursorTime = createdAt
	s.cursorID = lastID
	seen := map[string]bool{}
	for _, row := range rows {
		rec := eventRecord{
			ID:           S(row, "id"),
			ProjectID:    S(row, "project_id"),
			CollectionID: S(row, "collection_id"),
			TaskID:       S(row, "task_id"),
			AttemptID:    S(row, "attempt_id"),
			EventType:    S(row, "event_type"),
			CreatedAt:    S(row, "created_at"),
		}
		if rec.ID == "" || seen[rec.ID] {
			continue
		}
		seen[rec.ID] = true
		if mcp.LevelRank(eventLevel(rec.EventType)) < int(atomic.LoadInt32(&s.level)) {
			continue
		}
		if !writeSSE(w, rec) {
			return
		}
	}
	flusher.Flush()
}

// matches reports whether a record is inside the stream's optional durable scope.
func (s *sseStream) matches(rec eventRecord) bool {
	if s.filter.project != "" && rec.ProjectID != s.filter.project {
		return false
	}
	if s.filter.collection != "" && rec.CollectionID != s.filter.collection {
		return false
	}
	if s.filter.eventType != "" && rec.EventType != s.filter.eventType {
		return false
	}
	return true
}

// beforeCursor reports whether a live record is at or below the replay cursor and
// so already covered by the Last-Event-ID replay (dedupe by (created_at,id)).
func (s *sseStream) beforeCursor(rec eventRecord) bool {
	if s.cursorTime == "" {
		return false
	}
	if rec.CreatedAt < s.cursorTime {
		return true
	}
	if rec.CreatedAt == s.cursorTime && s.cursorID != "" {
		return rec.ID <= s.cursorID
	}
	return false
}

// writeSSE writes one SSE frame carrying ONLY a JSON-RPC notification (never a
// response). The SSE id: line is the durable EVT id so a reconnect can carry it
// back as Last-Event-ID.
func writeSSE(w http.ResponseWriter, rec eventRecord) bool {
	body := mcp.NotificationMessageBody(eventLevel(rec.EventType), sseLoggerName, map[string]any{
		"event_type": rec.EventType,
		"event_id":   rec.ID,
		"project_id": rec.ProjectID,
		"collection": rec.CollectionID,
		"task":       rec.TaskID,
		"attempt":    rec.AttemptID,
		"created_at": rec.CreatedAt,
	})
	buf, err := json.Marshal(body)
	if err != nil {
		return true // skip an unencodable payload; never break the stream on it
	}
	var frame bytes.Buffer
	frame.Grow(len(buf) + 64)
	frame.WriteString("id: ")
	frame.WriteString(rec.ID)
	frame.WriteString("\nevent: message\ndata: ")
	frame.Write(buf)
	frame.WriteString("\n\n")
	_, err = w.Write(frame.Bytes())
	return err == nil
}

// eventLevel is the fixed, deterministic event_type -> MCP logging level mapping
// (DEC-19f6a85625) that makes a per-connection level filter meaningful. Terminal
// /audit transitions are >= the default `info` level so the E2E COMPLETE/BLOCKED
// acceptance is always receivable; routine lifecycle/lineage is `debug`.
func eventLevel(eventType string) string {
	switch eventType {
	// error/critical: protocol / authorization violations.
	case "IMPLEMENTATION_WORKSPACE_VIOLATION", "READ_ONLY_CAPABILITY_RETRY":
		return "error"
	// warning: repair / reconciliation / blocked signals.
	case "CRITIC_REVALIDATION_REQUIRED", "COLLECTION_BLOCKED", "TASK_FAILED",
		"PLAN_REVIEW_CYCLE_REPAIRED", "PLAN_REVIEW_REPAIR_CREATED", "LEGACY_DEPENDENCY_CYCLE_REPAIRED":
		return "warning"
	// info: decision-relevant terminal / audit transitions.
	case "COLLECTION_COMPLETE", "COLLECTION_ACCEPTED", "COLLECTION_PR_RECORDED", "COLLECTION_PR_UPDATED",
		"COLLECTION_REVISION_SATISFIED", "COLLECTION_ENTRYPOINT_RECOVERED",
		"TASK_COMPLETE", "TASK_INTEGRATED", "TASK_SUPERSEDED", "TASK_MANUAL_RETRY_CREATED":
		return "info"
	// debug: routine lifecycle / lineage.
	default:
		return "debug"
	}
}

// compile-time guard that the hub satisfies the interfaces it is wired for.
var (
	_ mcp.StreamHandler   = (*sseHub)(nil)
	_ mcp.LoggingProvider = (*sseHub)(nil)
	_ eventSink           = (*sseHub)(nil)
)
