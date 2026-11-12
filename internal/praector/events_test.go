package praector

import (
	"context"
	"strings"
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

// registerProject registers a second, distinct project so scope resolution can
// be exercised against real durable rows.
func registerProject(t *testing.T, s *Store, id, name string) string {
	t.Helper()
	cfg := testProjectConfig(t.TempDir())
	cfg.ProjectID, cfg.Name = id, name
	p, err := s.RegisterProject(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return S(p, "id")
}

// newEventsService wires a service over a fresh fake store.
func newEventsService(t *testing.T) (*Store, *Service) {
	t.Helper()
	s, cfg := newFakeStore(t)
	return s, &Service{store: s, cfg: cfg}
}

// assertEventsError requires the events op to fail with the exact wording the
// sibling read commands produce for the same mistake, which is the store
// getter's own message rather than events-specific text.
func assertEventsError(t *testing.T, label string, svc *Service, in EventsInput, want string) {
	t.Helper()
	_, err := svc.Events(context.Background(), in)
	if err == nil {
		t.Fatalf("%s: expected an error, got an empty success", label)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("%s: want error containing %q, got %q", label, want, err.Error())
	}
}

// TestEventsUnknownScopeFailsClosed proves every supplied scope id form is
// resolved through the same store getters the sibling read commands use, so a
// mistyped id is an error instead of a successful empty page. The positional
// TARGET and the equivalent flag form must produce the same wording, and two
// distinct unknown ids report exactly one error in the order project,
// collection, task.
func TestEventsUnknownScopeFailsClosed(t *testing.T) {
	_, svc := newEventsService(t)
	assertEventsError(t, "COL-* target", svc, EventsInput{Target: "COL-DEADBEEF"}, "collection not found: COL-DEADBEEF")
	assertEventsError(t, "--collection flag", svc, EventsInput{CollectionID: "COL-DEADBEEF"}, "collection not found: COL-DEADBEEF")
	assertEventsError(t, "T-* target", svc, EventsInput{Target: "T-deadbeef"}, "task not found: T-deadbeef")
	assertEventsError(t, "PRJ-* target", svc, EventsInput{Target: "PRJ-nope"}, "project not found: PRJ-nope")
	assertEventsError(t, "--project flag", svc, EventsInput{ProjectID: "PRJ-nope"}, "project not found: PRJ-nope")

	// Deterministic single-error order: project before collection.
	assertEventsError(t, "project before collection", svc, EventsInput{ProjectID: "PRJ-nope", CollectionID: "COL-DEADBEEF"}, "project not found: PRJ-nope")
}

// TestEventsScopeResolvedOnce proves a TARGET and a flag naming the SAME entity
// resolve exactly once (deduplicated) rather than double-reporting, and that a
// valid target still reads its rows after the resolution seam.
func TestEventsScopeResolvedOnce(t *testing.T) {
	s, svc := newEventsService(t)
	pid := registerProject(t, s, "PRJ-once", "once")
	c, _, err := s.CreateCollectionWithCoordinator(pid, "Events", "Observe", "Plan")
	if err != nil {
		t.Fatal(err)
	}
	seedEvents(t, s, pid, S(c, "id"), "TEST_PAGE", 2)

	out, err := svc.Events(context.Background(), EventsInput{Target: S(c, "id"), CollectionID: S(c, "id"), Type: "TEST_PAGE"})
	if err != nil {
		t.Fatalf("target and flag naming one collection must not double-report: %v", err)
	}
	if len(out.Rows) != 2 {
		t.Fatalf("want 2 rows, got %d", len(out.Rows))
	}
}

// TestEventsContradictoryScopePair proves a project and a collection that do not
// belong to each other are rejected instead of returning an empty page. This is
// the case projectForScope cannot catch, because a supplied project short-circuits
// before the collection is read.
func TestEventsContradictoryScopePair(t *testing.T) {
	s, svc := newEventsService(t)
	pidA := registerProject(t, s, "PRJ-alpha", "alpha")
	pidB := registerProject(t, s, "PRJ-beta", "beta")
	cB, _, err := s.CreateCollectionWithCoordinator(pidB, "Events", "Observe", "Plan")
	if err != nil {
		t.Fatal(err)
	}
	if pidA == pidB {
		t.Fatal("expected two distinct projects")
	}

	_, err = svc.Events(context.Background(), EventsInput{ProjectID: pidA, CollectionID: S(cB, "id")})
	if err == nil {
		t.Fatal("mismatched --project/--collection pair returned an empty success")
	}
	if !strings.Contains(err.Error(), S(cB, "id")) || !strings.Contains(err.Error(), pidA) {
		t.Fatalf("error must name both ids, got %q", err.Error())
	}

	// The coherent pair must still work.
	if _, err := svc.Events(context.Background(), EventsInput{ProjectID: pidB, CollectionID: S(cB, "id")}); err != nil {
		t.Fatalf("matching project+collection pair must not error: %v", err)
	}
}

// TestEventsEmptyButValidScope is the non-regression anchor: a valid scope whose
// page is genuinely empty stays a successful empty result with no cursor, so a
// scripted --since walk over a quiet scope still terminates cleanly instead of
// looking like a failure, while a valid scope with events is unaffected. The
// --type filter makes the empty page explicit without depending on whether the
// durable lifecycle happened to write any rows.
func TestEventsEmptyButValidScope(t *testing.T) {
	s, svc := newEventsService(t)
	pid := registerProject(t, s, "PRJ-quiet", "quiet")
	quiet, _, err := s.CreateCollectionWithCoordinator(pid, "Quiet", "Observe", "Plan")
	if err != nil {
		t.Fatal(err)
	}
	loud, _, err := s.CreateCollectionWithCoordinator(pid, "Loud", "Observe", "Plan")
	if err != nil {
		t.Fatal(err)
	}
	seedEvents(t, s, pid, S(loud, "id"), "TEST_PAGE", 3)

	for _, in := range []EventsInput{
		{CollectionID: S(quiet, "id"), Type: "TEST_PAGE"},
		{Target: S(quiet, "id"), Type: "TEST_PAGE"},
		{ProjectID: pid, CollectionID: S(quiet, "id"), Type: "TEST_PAGE"},
	} {
		out, err := svc.Events(context.Background(), in)
		if err != nil {
			t.Fatalf("valid zero-event scope must not error: %v", err)
		}
		if len(out.Rows) != 0 {
			t.Fatalf("valid zero-event scope returned %d rows", len(out.Rows))
		}
		if out.NextCursor != "" {
			t.Fatalf("valid zero-event scope must not emit a cursor, got %q", out.NextCursor)
		}
	}

	out, err := svc.Events(context.Background(), EventsInput{CollectionID: S(loud, "id"), Type: "TEST_PAGE"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Rows) != 3 || out.NextCursor == "" {
		t.Fatalf("valid scope with events changed: %d rows, cursor %q", len(out.Rows), out.NextCursor)
	}
}

// TestEventsProjectNameScope pins the canonicalization decision explicitly:
// GetProject also matches a project name or root_path, so the project filter uses
// the RESOLVED row id (the MemoryList/DecisionAdd precedent) and a project named
// on the command line actually returns its rows instead of a silently empty page.
func TestEventsProjectNameScope(t *testing.T) {
	s, svc := newEventsService(t)
	pid := registerProject(t, s, "PRJ-named", "named-scope")
	c, _, err := s.CreateCollectionWithCoordinator(pid, "Events", "Observe", "Plan")
	if err != nil {
		t.Fatal(err)
	}
	seedEvents(t, s, pid, S(c, "id"), "TEST_PAGE", 3)

	for _, scope := range []string{"named-scope", cfgRootOf(t, s, pid)} {
		out, err := svc.Events(context.Background(), EventsInput{ProjectID: scope, Type: "TEST_PAGE", Limit: 10})
		if err != nil {
			t.Fatalf("project scope %q rejected: %v", scope, err)
		}
		if len(out.Rows) != 3 {
			t.Fatalf("project scope %q returned %d rows, want 3", scope, len(out.Rows))
		}
	}
}

// cfgRootOf returns a project's registered root_path, the third way GetProject
// resolves a project scope.
func cfgRootOf(t *testing.T, s *Store, pid string) string {
	t.Helper()
	p, err := s.GetProject(pid)
	if err != nil {
		t.Fatal(err)
	}
	return S(p, "root_path")
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
// than guessing a starting point, and that it keeps error precedence over scope
// resolution: a malformed cursor against an unresolvable collection is still the
// cursor error, not a not-found error.
func TestEventsMalformedCursor(t *testing.T) {
	_, svc := newEventsService(t)
	for _, in := range []EventsInput{
		{CollectionID: "COL-x", Since: "!!!not-base64!!!"},
		{ProjectID: "PRJ-x", CollectionID: "COL-x", Since: "!!!not-base64!!!"},
		{Target: "T-x", Since: "!!!not-base64!!!"},
	} {
		_, err := svc.Events(context.Background(), in)
		if err == nil {
			t.Fatalf("%+v: expected malformed cursor to be rejected", in)
		}
		if !strings.Contains(err.Error(), "invalid --since cursor") {
			t.Fatalf("%+v: want the cursor error to win over scope resolution, got %q", in, err.Error())
		}
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
