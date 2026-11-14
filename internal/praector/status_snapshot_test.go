package praector

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// statusFixtureTimes are fixed, strictly-increasing DATETIME(6) literals so the
// reconcile revision-baseline ordering (implement/review created after the OPEN
// revision) and the latest-critic selection (latest critic task by created_at) are
// deterministic and identical across the two identically-seeded stores.
const (
	fxT0 = "2026-01-01 00:00:00.000000" // coordinators
	fxT1 = "2026-01-01 00:00:01.000000" // COL-a OPEN revision, COL-b first critic
	fxT2 = "2026-01-01 00:00:02.000000" // COL-a implement, COL-b second critic
	fxT3 = "2026-01-01 00:00:03.000000" // COL-a review (pass), COL-b implement
	fxT4 = "2026-01-01 00:00:04.000000" // COL-b review (pass), COL-c old review
	fxT5 = "2026-01-01 00:00:05.000000" // COL-c new review (pass)
)

// seedStatusFixture seeds one project PRJ-s with three collections that each
// exercise a distinct status-read mechanism:
//
//	COL-a: an OPEN revision that reconcile flips SATISFIED (implement + a later
//	       passing verifying review created after the revision), flipping the
//	       stored BLOCKED collection to COMPLETE.
//	COL-b: a stale critic baseline (security keyword objective => critic gate)
//	       with MULTIPLE critic tasks, so the core must re-derive the LATEST
//	       critic task id to pick the sealed fingerprint; keeps the collection
//	       BLOCKED.
//	COL-c: a superseded_by review pair where an incoming depends_on edge into the
//	       old review forces RepairSupersededReviewLineage to ADD an edge to the
//	       new review.
func seedStatusFixture(t *testing.T, s *Store) {
	t.Helper()
	pj := "PRJ-s"
	stmts := []string{
		"INSERT INTO projects (id,name,root_path,base_branch,config_json,created_at,updated_at) VALUES (" + SQLLiteral(pj) + ",'s','/s','main','{}'," + SQLLiteral(fxT0) + "," + SQLLiteral(fxT0) + ")",

		// COL-a — reconcile SATISFIED + COMPLETE flip.
		"INSERT INTO collections (id,project_id,title,objective,status,created_at,updated_at) VALUES ('COL-a','" + pj + "','Alpha','Alpha status fixture','BLOCKED'," + SQLLiteral(fxT0) + "," + SQLLiteral(fxT0) + ")",
		"INSERT INTO tasks (id,project_id,collection_id,profile,title,objective,status,created_at,updated_at) VALUES ('T-a-coord','" + pj + "','COL-a','coordinator','c','o','COMPLETE'," + SQLLiteral(fxT0) + "," + SQLLiteral(fxT0) + ")",
		"INSERT INTO tasks (id,project_id,collection_id,profile,title,objective,status,created_at,updated_at) VALUES ('T-a-impl','" + pj + "','COL-a','implement','i','o','COMPLETE'," + SQLLiteral(fxT2) + "," + SQLLiteral(fxT2) + ")",
		"INSERT INTO tasks (id,project_id,collection_id,profile,title,objective,status,result_json,created_at,updated_at) VALUES ('T-a-rev','" + pj + "','COL-a','review','r','o','COMPLETE'," + SQLLiteral(`{"verdict":"pass"}`) + "," + SQLLiteral(fxT3) + "," + SQLLiteral(fxT3) + ")",
		"INSERT INTO task_edges (from_task_id,to_task_id,collection_id,relation,created_at) VALUES ('T-a-impl','T-a-rev','COL-a','verifies'," + SQLLiteral(fxT3) + ")",
		"INSERT INTO collection_revisions (id,project_id,collection_id,reason,status,coordinator_task_id,created_at) VALUES ('REV-a','" + pj + "','COL-a','rev','OPEN','T-a-coord'," + SQLLiteral(fxT1) + ")",

		// COL-b — stale critic baseline (multiple critic tasks) + BLOCKED.
		"INSERT INTO collections (id,project_id,title,objective,status,created_at,updated_at) VALUES ('COL-b','" + pj + "','Beta','Add authentication hardening','RUNNING'," + SQLLiteral(fxT0) + "," + SQLLiteral(fxT0) + ")",
		"INSERT INTO tasks (id,project_id,collection_id,profile,title,objective,status,created_at,updated_at) VALUES ('T-b-coord','" + pj + "','COL-b','coordinator','c','o','COMPLETE'," + SQLLiteral(fxT0) + "," + SQLLiteral(fxT0) + ")",
		"INSERT INTO tasks (id,project_id,collection_id,profile,title,objective,status,created_at,updated_at) VALUES ('T-b-c1','" + pj + "','COL-b','critic','k','o','COMPLETE'," + SQLLiteral(fxT1) + "," + SQLLiteral(fxT1) + ")",
		"INSERT INTO tasks (id,project_id,collection_id,profile,title,objective,status,created_at,updated_at) VALUES ('T-b-c2','" + pj + "','COL-b','critic','k','o','COMPLETE'," + SQLLiteral(fxT2) + "," + SQLLiteral(fxT2) + ")",
		"INSERT INTO tasks (id,project_id,collection_id,profile,title,objective,status,created_at,updated_at) VALUES ('T-b-impl','" + pj + "','COL-b','implement','i','o','COMPLETE'," + SQLLiteral(fxT3) + "," + SQLLiteral(fxT3) + ")",
		"INSERT INTO tasks (id,project_id,collection_id,profile,title,objective,status,result_json,created_at,updated_at) VALUES ('T-b-rev','" + pj + "','COL-b','review','r','o','COMPLETE'," + SQLLiteral(`{"verdict":"pass"}`) + "," + SQLLiteral(fxT4) + "," + SQLLiteral(fxT4) + ")",
		"INSERT INTO task_edges (from_task_id,to_task_id,collection_id,relation,created_at) VALUES ('T-b-impl','T-b-rev','COL-b','verifies'," + SQLLiteral(fxT4) + ")",
		"INSERT INTO events (id,project_id,collection_id,task_id,attempt_id,event_type,payload_json,created_at) VALUES ('EVT-b1','" + pj + "','COL-b','T-b-c1',NULL,'CRITIC_PLAN_BASELINE'," + SQLLiteral(`{"source":"initial_plan_seal","fingerprint":"WRONG-1"}`) + "," + SQLLiteral(fxT1) + ")",
		"INSERT INTO events (id,project_id,collection_id,task_id,attempt_id,event_type,payload_json,created_at) VALUES ('EVT-b2','" + pj + "','COL-b','T-b-c2',NULL,'CRITIC_PLAN_BASELINE'," + SQLLiteral(`{"source":"initial_plan_seal","fingerprint":"WRONG-2"}`) + "," + SQLLiteral(fxT2) + ")",

		// COL-c — superseded review lineage repair (adds a depends_on edge).
		"INSERT INTO collections (id,project_id,title,objective,status,created_at,updated_at) VALUES ('COL-c','" + pj + "','Gamma','Gamma status fixture','BLOCKED'," + SQLLiteral(fxT0) + "," + SQLLiteral(fxT0) + ")",
		"INSERT INTO tasks (id,project_id,collection_id,profile,title,objective,status,created_at,updated_at) VALUES ('T-c-coord','" + pj + "','COL-c','coordinator','c','o','COMPLETE'," + SQLLiteral(fxT0) + "," + SQLLiteral(fxT0) + ")",
		"INSERT INTO tasks (id,project_id,collection_id,profile,title,objective,status,created_at,updated_at) VALUES ('T-c-impl','" + pj + "','COL-c','implement','i','o','COMPLETE'," + SQLLiteral(fxT2) + "," + SQLLiteral(fxT2) + ")",
		"INSERT INTO tasks (id,project_id,collection_id,profile,title,objective,status,result_json,created_at,updated_at) VALUES ('T-c-old','" + pj + "','COL-c','review','r','o','COMPLETE'," + SQLLiteral(`{"verdict":"fail"}`) + "," + SQLLiteral(fxT4) + "," + SQLLiteral(fxT4) + ")",
		"INSERT INTO tasks (id,project_id,collection_id,profile,title,objective,status,result_json,created_at,updated_at) VALUES ('T-c-new','" + pj + "','COL-c','review','r','o','COMPLETE'," + SQLLiteral(`{"verdict":"pass"}`) + "," + SQLLiteral(fxT5) + "," + SQLLiteral(fxT5) + ")",
		"INSERT INTO task_edges (from_task_id,to_task_id,collection_id,relation,created_at) VALUES ('T-c-impl','T-c-old','COL-c','depends_on'," + SQLLiteral(fxT4) + ")",
		"INSERT INTO task_edges (from_task_id,to_task_id,collection_id,relation,created_at) VALUES ('T-c-old','T-c-new','COL-c','superseded_by'," + SQLLiteral(fxT5) + ")",
		"INSERT INTO task_edges (from_task_id,to_task_id,collection_id,relation,created_at) VALUES ('T-c-impl','T-c-new','COL-c','verifies'," + SQLLiteral(fxT5) + ")",
	}
	if err := s.Mutate(strings.Join(stmts, ";"), "seed status fixture"); err != nil {
		t.Fatal(err)
	}
}

// statusStoreSnapshot renders the durable status-relevant state, ignoring random
// event ids and timestamps (payloads are deterministic, following the
// repairSnapshot precedent).
func statusStoreSnapshot(t *testing.T, s *Store) string {
	t.Helper()
	var sb strings.Builder
	cols, err := s.Query("SELECT id,status FROM collections ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	sb.WriteString("collections:\n")
	for _, r := range cols {
		sb.WriteString("  " + S(r, "id") + " " + S(r, "status") + "\n")
	}
	revs, err := s.Query("SELECT id,status FROM collection_revisions ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	sb.WriteString("revisions:\n")
	for _, r := range revs {
		sb.WriteString("  " + S(r, "id") + " " + S(r, "status") + "\n")
	}
	tasks, err := s.Query("SELECT id,status FROM tasks ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	sb.WriteString("tasks:\n")
	for _, r := range tasks {
		sb.WriteString("  " + S(r, "id") + " " + S(r, "status") + "\n")
	}
	edges, err := s.Query("SELECT from_task_id,to_task_id,relation FROM task_edges ORDER BY from_task_id,to_task_id,relation")
	if err != nil {
		t.Fatal(err)
	}
	sb.WriteString("edges:\n")
	for _, r := range edges {
		sb.WriteString("  " + S(r, "from_task_id") + " " + S(r, "to_task_id") + " " + S(r, "relation") + "\n")
	}
	evs, err := s.Query("SELECT collection_id,event_type,payload_json FROM events ORDER BY collection_id,event_type,payload_json")
	if err != nil {
		t.Fatal(err)
	}
	sb.WriteString("events:\n")
	for _, r := range evs {
		sb.WriteString("  " + S(r, "collection_id") + " " + S(r, "event_type") + " " + S(r, "payload_json") + "\n")
	}
	return sb.String()
}

// renderCollectionStatuses renders a deterministic, comparable view of a set of
// CollectionStatus values (the display surface parity must preserve).
func renderCollectionStatuses(list []*CollectionStatus) string {
	var sb strings.Builder
	for _, cs := range list {
		sb.WriteString("col " + cs.ID + " status=" + cs.Status + " title=" + cs.Title + " branch=" + cs.Branch + " objective=" + cs.Objective + "\n")
		sb.WriteString("  counts:")
		for _, kv := range cs.TaskCounts {
			sb.WriteString(" " + kv.Key + "=" + itoa(kv.Count))
		}
		sb.WriteString("\n")
		sb.WriteString("  active:")
		for _, t := range cs.Active {
			sb.WriteString(" " + S(t, "id") + "/" + S(t, "status"))
		}
		sb.WriteString("\n")
		sb.WriteString("  acceptance=" + rowIDOrDash(cs.Acceptance) + " pr=" + rowIDOrDash(cs.PR) + "\n")
		sb.WriteString("  revisions:")
		for _, r := range cs.Revisions {
			sb.WriteString(" " + S(r, "id") + "=" + S(r, "status"))
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

func rowIDOrDash(r Row) string {
	if r == nil {
		return "-"
	}
	return S(r, "id")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// statusReferenceProject runs the ORIGINAL Status project path (UpdateCollectionTerminalState
// + GetCollection + collectionStatus) so the batched Status path can be compared
// against exactly the behavior it must preserve.
func statusReferenceProject(t *testing.T, s *Store, svc *Service, pid string) []*CollectionStatus {
	t.Helper()
	cs, err := s.ListCollections(pid)
	if err != nil {
		t.Fatal(err)
	}
	var out []*CollectionStatus
	for _, c := range cs {
		_, _ = s.UpdateCollectionTerminalState(S(c, "id"))
		fresh, _ := s.GetCollection(S(c, "id"))
		st := svc.collectionStatus(fresh)
		out = append(out, &st)
	}
	return out
}

// TestStatusBatchedMatchesReference is the central Phase 2 parity gate: the new
// batched project Status path yields an identical CollectionStatus display,
// identical final table rows and collection status, and an identical event stream
// compared with the original UpdateCollectionTerminalState + collectionStatus
// path, across seeds that include (a) an OPEN revision reconcile flips SATISFIED,
// (b) a stale critic baseline, (c) a collection with multiple critic tasks, and
// (d) a collection where RepairSupersededReviewLineage adds at least one edge.
func TestStatusBatchedMatchesReference(t *testing.T) {
	installFakeDolt(t)

	// Store A: the unchanged reference path.
	a, cfgA := newFakeStore(t)
	seedStatusFixture(t, a)
	svcA := &Service{store: a, cfg: cfgA}
	refOut := statusReferenceProject(t, a, svcA, "PRJ-s")
	refSnap := statusStoreSnapshot(t, a)

	// Store B: the new batched Status path.
	b, cfgB := newFakeStore(t)
	seedStatusFixture(t, b)
	svcB := &Service{store: b, cfg: cfgB}
	newOut, e := svcB.Status(context.Background(), StatusInput{ProjectID: "PRJ-s"})
	if e != nil {
		t.Fatal(e)
	}
	newSnap := statusStoreSnapshot(t, b)

	if rs, ns := renderCollectionStatuses(refOut), renderCollectionStatuses(newOut.Collections); rs != ns {
		t.Fatalf("batched CollectionStatus display diverged from reference:\n--- reference ---\n%s\n--- batched ---\n%s", rs, ns)
	}
	if refSnap != newSnap {
		t.Fatalf("batched status final state / event stream diverged from reference:\n--- reference ---\n%s\n--- batched ---\n%s", refSnap, newSnap)
	}

	// Confirm the fixture genuinely exercised every named mechanism rather than
	// comparing two trivially-equal states.
	if !strings.Contains(newSnap, "REV-a SATISFIED") {
		t.Fatalf("seed (a) expected reconcile to satisfy an OPEN revision:\n%s", newSnap)
	}
	if !strings.Contains(newSnap, "REVIEW_LINEAGE_REPAIRED") {
		t.Fatalf("seed (d) expected RepairSupersededReviewLineage to add an edge:\n%s", newSnap)
	}
	if !strings.Contains(newSnap, "COL-b BLOCKED") {
		t.Fatalf("seeds (b)/(c) expected the stale-critic collection to stay BLOCKED:\n%s", newSnap)
	}
	// COL-b must be BLOCKED specifically because the (latest) critic baseline is
	// stale: a compatible_via match or a wrongly-keyed baseline would flip it.
	cs, err := b.CriticRevalidationState("COL-b")
	if err != nil {
		t.Fatal(err)
	}
	if !B(cs, "stale") {
		t.Fatalf("seed (b) expected COL-b critic state to be stale (latest critic baseline selected): %+v", cs)
	}
	if S(cs, "latest_critic_task_id") != "T-b-c2" {
		t.Fatalf("seed (c) expected the LATEST critic task's baseline to be selected, got %q", S(cs, "latest_critic_task_id"))
	}
}

// TestStatusSingleCollectionMatchesReference proves the single-collection (COL-…)
// Status shape agrees with the reference for a collection that reconciles to a
// status flip.
func TestStatusSingleCollectionMatchesReference(t *testing.T) {
	installFakeDolt(t)

	a, cfgA := newFakeStore(t)
	seedStatusFixture(t, a)
	svcA := &Service{store: a, cfg: cfgA}
	_, _ = a.UpdateCollectionTerminalState("COL-a")
	fresh, _ := a.GetCollection("COL-a")
	st := svcA.collectionStatus(fresh)
	refSnap := statusStoreSnapshot(t, a)

	b, cfgB := newFakeStore(t)
	seedStatusFixture(t, b)
	svcB := &Service{store: b, cfg: cfgB}
	out, e := svcB.Status(context.Background(), StatusInput{Target: "COL-a"})
	if e != nil {
		t.Fatal(e)
	}
	newSnap := statusStoreSnapshot(t, b)

	if rs, ns := renderCollectionStatuses([]*CollectionStatus{&st}), renderCollectionStatuses([]*CollectionStatus{out.Collection}); rs != ns {
		t.Fatalf("single-collection display diverged:\n--- reference ---\n%s--- batched ---\n%s", rs, ns)
	}
	if refSnap != newSnap {
		t.Fatalf("single-collection state diverged:\n--- reference ---\n%s--- batched ---\n%s", refSnap, newSnap)
	}
	if out.Collection == nil || out.Collection.Status != CollectionComplete {
		t.Fatalf("expected COL-a to reconcile to COMPLETE, got %+v", out.Collection)
	}
}

// TestStatusDefaultFlipsAndNoReconcileLeavesStateProvesZeroWrites proves: a
// default project status still performs the needed SetCollectionStatus flip write,
// while `--no-reconcile` leaves the durable state unchanged with no write
// (add/commit/UPDATE) and no event, while still rendering the stored statuses.
func TestStatusDefaultFlipsAndNoReconcileLeavesStateProvesZeroWrites(t *testing.T) {
	installFakeDolt(t)
	logPath := filepath.Join(t.TempDir(), "sqlops.log")
	t.Setenv("PRAECTOR_FAKE_DOLT_LOG", logPath)

	// Default reconcile path performs a status-flip write.
	d, cfgD := newFakeStore(t)
	seedStatusFixture(t, d)
	svcD := &Service{store: d, cfg: cfgD}
	if err := os.WriteFile(logPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, e := svcD.Status(context.Background(), StatusInput{ProjectID: "PRJ-s"}); e != nil {
		t.Fatal(e)
	}
	cols, _ := d.Query("SELECT id,status FROM collections ORDER BY id")
	if !hasStatus(cols, "COL-a", CollectionComplete) {
		t.Fatalf("default status must flip COL-a to COMPLETE via a write")
	}
	if n := countSQLMatching(t, logPath, "UPDATE collections SET status"); n < 1 {
		t.Fatalf("default status must perform a SetCollectionStatus flip write (UPDATE collections SET status), got %d", n)
	}

	// Non-reconciling path renders stored state with zero writes.
	n, cfgN := newFakeStore(t)
	seedStatusFixture(t, n)
	svcN := &Service{store: n, cfg: cfgN}
	before, _ := n.Query("SELECT id,status FROM collections ORDER BY id")
	if err := os.WriteFile(logPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	out, e := svcN.Status(context.Background(), StatusInput{ProjectID: "PRJ-s", NoReconcile: true})
	if e != nil {
		t.Fatal(e)
	}
	after, _ := n.Query("SELECT id,status FROM collections ORDER BY id")
	if statusRows(before) != statusRows(after) {
		t.Fatalf("--no-reconcile must leave collection status unchanged:\nbefore=%safter=%s", statusRows(before), statusRows(after))
	}
	// Zero write path: no dolt sql invocation lacking `-r json` (i.e. no UPDATE /
	// INSERT / DELETE mutation), so no add/commit and no status flip.
	if w := countWriteSQLOperations(t, logPath); w != 0 {
		t.Fatalf("--no-reconcile must issue zero write dolt sql, got %d", w)
	}
	if n := countSQLMatching(t, logPath, "UPDATE collections SET status"); n != 0 {
		t.Fatalf("--no-reconcile must not flip status, got %d status writes", n)
	}
	// The stored status (BLOCKED for COL-a) is rendered, not the reconciled one.
	for _, c := range out.Collections {
		if c.ID == "COL-a" && c.Status != "BLOCKED" {
			t.Fatalf("--no-reconcile must render the STORED status for COL-a, got %q", c.Status)
		}
	}
}

func hasStatus(rows Rows, id, status string) bool {
	for _, r := range rows {
		if S(r, "id") == id {
			return S(r, "status") == status
		}
	}
	return false
}

func statusRows(rows Rows) string {
	var b strings.Builder
	for _, r := range rows {
		b.WriteString("  " + S(r, "id") + " " + S(r, "status") + "\n")
	}
	return b.String()
}

// countWriteSQLOperations counts logged `dolt sql` invocations that are NOT JSON
// reads (the mutation path uses `dolt sql -q` without `-r json`), so it separates
// durable writes (UPDATE/INSERT/DELETE via Mutate/AddEdge/SetCollectionStatus)
// from the batched reads.
func countWriteSQLOperations(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatal(err)
	}
	n := 0
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "sql") && !strings.Contains(line, "json") {
			n++
		}
	}
	return n
}

// TestStatusReadCountConstantVsCollectionCount proves the batched read path keeps
// project-wide status `dolt sql` invocations at a small constant independent of
// the collection count, measured via the fake-Dolt count seam (the batched reads
// plus the per-collection status-flip write are the only SQL; the display issues
// no extra per-collection process).
func TestStatusReadCountConstantVsCollectionCount(t *testing.T) {
	installFakeDolt(t)
	logPath := filepath.Join(t.TempDir(), "sqlops.log")
	t.Setenv("PRAECTOR_FAKE_DOLT_LOG", logPath)

	const small, big = 2, 18
	smallSQL := statusProjectSQLCount(t, logPath, "PRJ-small", small)
	bigSQL := statusProjectSQLCount(t, logPath, "PRJ-big", big)

	// The read count must not scale with collection count. The only count-dependent
	// SQL is the status-flip write (one SetCollectionStatus mutation per flipping
	// collection), so assert the DELTA is bounded and the large count stays small
	// (well under the ~491 baseline), rather than hard-coding a live figure.
	if bigSQL-smallSQL > big {
		t.Fatalf("status read count scaled with collection count: small(2)=%d big(18)=%d", smallSQL, bigSQL)
	}
	if bigSQL > 4*big {
		t.Fatalf("project-wide status for 18 collections used %d dolt sql invocations; expected a small constant, not the ~491 baseline", bigSQL)
	}
	t.Logf("status dolt sql invocations: 2 collections=%d, 18 collections=%d", smallSQL, bigSQL)
}

func statusProjectSQLCount(t *testing.T, logPath, pid string, n int) int {
	t.Helper()
	s, cfg := newFakeStore(t)
	if err := s.Mutate("INSERT INTO projects (id,name,root_path,base_branch,config_json,created_at,updated_at) VALUES ("+SQLLiteral(pid)+","+SQLLiteral(pid)+",'/"+pid+"','main','{}',"+SQLLiteral(fxT0)+","+SQLLiteral(fxT0)+")", "seed project"); err != nil {
		t.Fatal(err)
	}
	// Stable, already-terminal COMPLETE collections that need NO write: an
	// implement + a later passing verifying review, no OPEN revision, no critic
	// gate. These exercise the display + terminal read without any status flip.
	for i := 0; i < n; i++ {
		sfx := string(rune('a'+i%26)) + string(rune('a'+i/26))
		cid := "COL-" + pid + "-" + sfx
		stmts := []string{
			"INSERT INTO collections (id,project_id,title,objective,status,created_at,updated_at) VALUES (" + SQLLiteral(cid) + "," + SQLLiteral(pid) + ",'t','A plain status fixture','COMPLETE'," + SQLLiteral(fxT0) + "," + SQLLiteral(fxT0) + ")",
			"INSERT INTO tasks (id,project_id,collection_id,profile,title,objective,status,created_at,updated_at) VALUES (" + SQLLiteral("T-i-"+sfx) + "," + SQLLiteral(pid) + "," + SQLLiteral(cid) + ",'implement','i','o','COMPLETE'," + SQLLiteral(fxT2) + "," + SQLLiteral(fxT2) + ")",
			"INSERT INTO tasks (id,project_id,collection_id,profile,title,objective,status,result_json,created_at,updated_at) VALUES (" + SQLLiteral("T-r-"+sfx) + "," + SQLLiteral(pid) + "," + SQLLiteral(cid) + ",'review','r','o','COMPLETE'," + SQLLiteral(`{"verdict":"pass"}`) + "," + SQLLiteral(fxT3) + "," + SQLLiteral(fxT3) + ")",
			"INSERT INTO task_edges (from_task_id,to_task_id,collection_id,relation,created_at) VALUES (" + SQLLiteral("T-i-"+sfx) + "," + SQLLiteral("T-r-"+sfx) + "," + SQLLiteral(cid) + ",'verifies'," + SQLLiteral(fxT3) + ")",
		}
		if err := s.Mutate(strings.Join(stmts, ";"), "seed stable collection"); err != nil {
			t.Fatal(err)
		}
	}
	svc := &Service{store: s, cfg: cfg}
	if err := os.WriteFile(logPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, e := svc.Status(context.Background(), StatusInput{ProjectID: pid}); e != nil {
		t.Fatal(e)
	}
	return countSQLOperations(t, logPath)
}

// TestStatusSingleAndNoReconcileReadCount measures the dolt sql invocation counts
// behind a single-collection `status` and `status --no-reconcile` via the fake
// count seam, as the docs/PERFORMANCE.md Phase 2 capture figures. A single
// collection reconciles from ONE batched read plus only the status-flip write; the
// non-reconciling render performs that one batched read with ZERO writes.
func TestStatusSingleAndNoReconcileReadCount(t *testing.T) {
	installFakeDolt(t)
	logPath := filepath.Join(t.TempDir(), "sqlops.log")
	t.Setenv("PRAECTOR_FAKE_DOLT_LOG", logPath)

	mk := func() (*Store, *Service) {
		s, cfg := newFakeStore(t)
		seedStatusFixture(t, s)
		return s, &Service{store: s, cfg: cfg}
	}

	_, svc := mk()
	if err := os.WriteFile(logPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, e := svc.Status(context.Background(), StatusInput{Target: "COL-a"}); e != nil {
		t.Fatal(e)
	}
	singleReads := countSQLOperations(t, logPath)
	singleWrites := countWriteSQLOperations(t, logPath)

	_, svc2 := mk()
	if err := os.WriteFile(logPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, e := svc2.Status(context.Background(), StatusInput{Target: "COL-a", NoReconcile: true}); e != nil {
		t.Fatal(e)
	}
	nrReads := countSQLOperations(t, logPath)
	nrWrites := countWriteSQLOperations(t, logPath)

	// One batched read regardless of the ~31-query single-collection baseline.
	if singleReads > 8 {
		t.Fatalf("single-collection status used %d dolt sql; expected a small batched count", singleReads)
	}
	// --no-reconcile is a pure read path: zero mutations, one batched read.
	if nrWrites != 0 {
		t.Fatalf("--no-reconcile must issue zero writes, got %d", nrWrites)
	}
	if nrReads > 4 {
		t.Fatalf("--no-reconcile used %d dolt sql; expected a single batched read", nrReads)
	}
	t.Logf("single-collection status: %d dolt sql total (%d write); --no-reconcile: %d dolt sql (%d write)", singleReads, singleWrites, nrReads, nrWrites)
}

// TestBatchedDocumentShapeAgainstRealDolt is the real-dolt 2.7.2 oracle for the
// batched `dolt sql -r json -q` multi-SELECT document shape (research C-10): the
// loader's document shape must be one document per statement, `{}` decoding to an
// empty Rows mid-batch, validated against the installed real binary rather than
// only the sqlite3 emulation. Skips when the real dolt binary is unavailable.
func TestBatchedDocumentShapeAgainstRealDolt(t *testing.T) {
	dolt, err := exec.LookPath("dolt")
	if err != nil {
		t.Skip("real dolt binary unavailable; the sqlite3 fake harness is not a document-shape conformance oracle")
	}
	_ = dolt
	cfg := GlobalConfig{Home: t.TempDir()}
	s, err := NewStore(cfg)
	if err != nil {
		t.Skipf("real dolt store unavailable: %v", err)
	}
	if err := s.Bootstrap(); err != nil {
		t.Skipf("real dolt bootstrap unavailable: %v", err)
	}
	if err := s.Mutate("INSERT INTO projects (id,name,root_path,base_branch,config_json,created_at,updated_at) VALUES ('PRJ-orc','o','/o','main','{}',"+SQLLiteral(fxT0)+","+SQLLiteral(fxT0)+")", "seed oracle project"); err != nil {
		t.Fatal(err)
	}
	if err := s.Mutate("INSERT INTO collections (id,project_id,title,objective,status,created_at,updated_at) VALUES ('COL-orc','PRJ-orc','t','A plain status fixture','COMPLETE',"+SQLLiteral(fxT0)+","+SQLLiteral(fxT0)+")", "seed oracle collection"); err != nil {
		t.Fatal(err)
	}
	// One empty member mid-batch (`{}`) must decode to empty Rows without shifting
	// the following statements' indices, and a populated collections read must map
	// to statement 0 — the exact contract loadCollectionRows relies on.
	out, err := s.QueryMany(
		"SELECT id FROM collections WHERE id='COL-orc'",
		"SELECT id FROM tasks WHERE collection_id='COL-orc'",
		"SELECT id FROM collection_revisions WHERE 0=1",
		"SELECT 2 AS b",
	)
	if err != nil {
		t.Fatalf("real-dolt multi-SELECT: %v", err)
	}
	if len(out) != 4 {
		t.Fatalf("real dolt must emit one document per statement: got %d", len(out))
	}
	if len(out[0]) != 1 || S(out[0][0], "id") != "COL-orc" {
		t.Fatalf("statement 0 (collections) rows wrong: %v", out[0])
	}
	if len(out[2]) != 0 {
		t.Fatalf("empty mid-batch member must decode to empty Rows via {} on real dolt, got %v", out[2])
	}
	if len(out[3]) != 1 {
		t.Fatalf("statement 3 rows wrong: %v", out[3])
	}
	// And the actual loader document shape decodes a real collection end-to-end.
	snaps, err := s.loadCollectionRows([]string{"COL-orc"})
	if err != nil {
		t.Fatalf("loadCollectionRows on real dolt: %v", err)
	}
	sn := snaps["COL-orc"]
	if sn == nil || sn.collection == nil || S(sn.collection, "id") != "COL-orc" {
		t.Fatalf("loadCollectionRows did not decode the collection from the batched document shape: %+v", sn)
	}
}
