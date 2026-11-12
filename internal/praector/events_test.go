package praector

import (
	"context"
	"testing"
)

// seedEvents writes n events of one type for a collection and returns them.
func seedEvents(t *testing.T, s *Store, pid, cid, eventType string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if err := s.Event(pid, cid, "", "", eventType, map[string]any{"n": i}); err != nil {
			t.Fatal(err)
		}
	}
}

// TestEventsReadSurfaceDirectStore proves the events op reads the durable log
// directly from the store with NO daemon running (the fake store runs no
// scheduler), returns deterministic oldest-first rows, and exposes the ids,
// event_type, and payload on every row.
func TestEventsReadSurfaceDirectStore(t *testing.T) {
	s, cfg := newFakeStore(t)
	svc := &Service{store: s, cfg: cfg}
	p, err := s.RegisterProject(testProjectConfig(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	c, _, err := s.CreateCollectionWithCoordinator(S(p, "id"), "Events", "Observe", "Plan")
	if err != nil {
		t.Fatal(err)
	}
	seedEvents(t, s, S(p, "id"), S(c, "id"), "TEST_PAGE", 5)

	out, err := svc.Events(context.Background(), EventsInput{CollectionID: S(c, "id"), Type: "TEST_PAGE"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Rows) != 5 {
		t.Fatalf("want 5 rows, got %d", len(out.Rows))
	}
	if out.NextCursor == "" {
		t.Fatal("expected a next_cursor to be emitted")
	}
	for _, r := range out.Rows {
		if S(r, "id") == "" || S(r, "event_type") != "TEST_PAGE" || S(r, "collection_id") == "" {
			t.Fatalf("row missing ids/event_type: %#v", r)
		}
		if S(r, "payload_json") == "" {
			t.Fatalf("row missing payload: %#v", r)
		}
	}
	// Deterministic created_at,id ordering: created_at must be non-decreasing.
	for i := 1; i < len(out.Rows); i++ {
		if S(out.Rows[i-1], "created_at") > S(out.Rows[i], "created_at") {
			t.Fatalf("rows not oldest-first: %s after %s", S(out.Rows[i], "created_at"), S(out.Rows[i-1], "created_at"))
		}
	}
}

// TestEventsCursorPagingAdvance proves --since is an EXCLUSIVE composite cursor:
// paging with a small limit yields disjoint, jointly-complete pages and a
// strictly advancing cursor that the next call reuses unchanged.
func TestEventsCursorPagingAdvance(t *testing.T) {
	s, cfg := newFakeStore(t)
	svc := &Service{store: s, cfg: cfg}
	p, err := s.RegisterProject(testProjectConfig(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	c, _, err := s.CreateCollectionWithCoordinator(S(p, "id"), "Events", "Observe", "Plan")
	if err != nil {
		t.Fatal(err)
	}
	seedEvents(t, s, S(p, "id"), S(c, "id"), "TEST_PAGE", 5)

	seen := map[string]int{}
	cursor := ""
	pages := 0
	for pages < 10 {
		out, err := svc.Events(context.Background(), EventsInput{CollectionID: S(c, "id"), Type: "TEST_PAGE", Since: cursor, Limit: 2})
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, r := range out.Rows {
			seen[S(r, "id")]++
		}
		if out.NextCursor == "" {
			break
		}
		if out.NextCursor == cursor {
			t.Fatal("cursor did not advance")
		}
		cursor = out.NextCursor
		if len(out.Rows) == 0 {
			break
		}
	}
	if len(seen) != 5 {
		t.Fatalf("paging covered %d distinct rows, want 5", len(seen))
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("row %s appeared %d times across pages", id, n)
		}
	}
}

// TestEventsMalformedCursor proves a corrupt --since token fails closed rather
// than guessing a starting point.
func TestEventsMalformedCursor(t *testing.T) {
	s, cfg := newFakeStore(t)
	svc := &Service{store: s, cfg: cfg}
	if _, err := svc.Events(context.Background(), EventsInput{CollectionID: "COL-x", Since: "!!!not-base64!!!"}); err == nil {
		t.Fatal("expected malformed cursor to be rejected")
	}
}

// TestEventsProjectScopeBounded proves a project-scoped listing (no collection/
// task filter) is permitted but bounded by the finite limit, since there is no
// project_id index.
func TestEventsProjectScopeBounded(t *testing.T) {
	s, cfg := newFakeStore(t)
	svc := &Service{store: s, cfg: cfg}
	p, err := s.RegisterProject(testProjectConfig(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	c, _, err := s.CreateCollectionWithCoordinator(S(p, "id"), "Events", "Observe", "Plan")
	if err != nil {
		t.Fatal(err)
	}
	seedEvents(t, s, S(p, "id"), S(c, "id"), "TEST_PAGE", 4)

	out, err := svc.Events(context.Background(), EventsInput{ProjectID: S(p, "id"), Type: "TEST_PAGE", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Rows) > 2 {
		t.Fatalf("project-scope listing exceeded --limit: %d", len(out.Rows))
	}
	if out.NextCursor == "" {
		t.Fatal("expected a next_cursor to be emitted for a bounded page")
	}
}

// TestEventsTargetInference proves a COL-* target is inferred into the collection
// scope the way the other read commands do, without a --collection flag.
func TestEventsTargetInference(t *testing.T) {
	s, cfg := newFakeStore(t)
	svc := &Service{store: s, cfg: cfg}
	p, err := s.RegisterProject(testProjectConfig(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	c, _, err := s.CreateCollectionWithCoordinator(S(p, "id"), "Events", "Observe", "Plan")
	if err != nil {
		t.Fatal(err)
	}
	seedEvents(t, s, S(p, "id"), S(c, "id"), "TEST_PAGE", 3)

	out, err := svc.Events(context.Background(), EventsInput{Target: S(c, "id"), Type: "TEST_PAGE"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Rows) != 3 {
		t.Fatalf("want 3 rows via COL-* target inference, got %d", len(out.Rows))
	}
}
