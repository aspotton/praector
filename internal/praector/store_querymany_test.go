package praector

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestQueryManyJSONLOrdering proves a semicolon-joined batch returns one Rows
// document per statement in positional order, and that a zero-row statement
// surfaces as empty Rows via the `{}` document real Dolt 2.7.2 emits.
func TestQueryManyJSONLOrdering(t *testing.T) {
	s, _ := newFakeStore(t)
	if err := s.Mutate("INSERT INTO collections (id,project_id,title,objective,status,created_at,updated_at) VALUES ('COL-order','PRJ-order','t','o','READY','"+UTCNow()+"','"+UTCNow()+"')", "seed collections"); err != nil {
		t.Fatal(err)
	}
	out, err := s.QueryMany(
		"SELECT 1 AS a",
		"SELECT id FROM collections WHERE 0=1",
		"SELECT id FROM collections WHERE id='COL-order'",
		"SELECT 2 AS b",
	)
	if err != nil {
		t.Fatalf("QueryMany: %v", err)
	}
	if len(out) != 4 {
		t.Fatalf("expected 4 result sets, got %d: %v", len(out), out)
	}
	if len(out[0]) != 1 || S(out[0][0], "a") != "1" {
		t.Fatalf("statement 0 rows wrong: %v", out[0])
	}
	if len(out[1]) != 0 {
		t.Fatalf("zero-row statement 1 must map to empty Rows via {}, got %v", out[1])
	}
	if len(out[2]) != 1 || S(out[2][0], "id") != "COL-order" {
		t.Fatalf("statement 2 rows wrong: %v", out[2])
	}
	if len(out[3]) != 1 || S(out[3][0], "b") != "2" {
		t.Fatalf("statement 3 rows wrong: %v", out[3])
	}
}

// TestQueryManySingleProcessAndBootstrapBound uses the fake-Dolt invocation-count
// seam to prove a batched read launches exactly one `dolt sql` process and that a
// bootstrap with several collections stays at a small bounded invocation count
// rather than one query pair per collection.
func TestQueryManySingleProcessAndBootstrapBound(t *testing.T) {
	installFakeDolt(t)
	logPath := filepath.Join(t.TempDir(), "sqlops.log")
	t.Setenv("PRAECTOR_FAKE_DOLT_LOG", logPath)
	cfg := GlobalConfig{Home: t.TempDir()}
	s, err := NewStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	if err = s.Mutate("INSERT INTO projects (id,name,root_path,base_branch,config_json,created_at,updated_at) VALUES ('PRJ-b','b','/b','main','{}','"+UTCNow()+"','"+UTCNow()+"')", "seed project"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		cid := "COL-b" + string(rune('a'+i))
		if err = s.Mutate("INSERT INTO collections (id,project_id,title,objective,status,created_at,updated_at) VALUES ("+SQLLiteral(cid)+",'PRJ-b','t','o','READY','"+UTCNow()+"','"+UTCNow()+"')", "seed collection"); err != nil {
			t.Fatal(err)
		}
	}
	if err = os.WriteFile(logPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err = s.QueryMany("SELECT 1 AS a", "SELECT id FROM collections WHERE 0=1", "SELECT 2 AS b"); err != nil {
		t.Fatalf("QueryMany: %v", err)
	}
	if n := countSQLOperations(t, logPath); n != 1 {
		t.Fatalf("a batched read must launch exactly one dolt sql, got %d", n)
	}
	if err = os.WriteFile(logPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err = s.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	if n := countSQLOperations(t, logPath); n > 4 {
		t.Fatalf("bootstrap used %d dolt sql invocations for 6 collections; expected a small bounded number", n)
	}
}

// TestQueryManyStatementIndexedError proves a mid-batch failure reports the
// failing statement index instead of silently truncating or shifting indices.
func TestQueryManyStatementIndexedError(t *testing.T) {
	s, _ := newFakeStore(t)
	_, err := s.QueryMany("SELECT 1 AS a", "SELECT * FROM no_such_table_xyz", "SELECT 2 AS b")
	if err == nil {
		t.Fatal("expected a statement-indexed error")
	}
	if !strings.Contains(err.Error(), "statement 1") {
		t.Fatalf("error must name failing statement index 1, got: %v", err)
	}
}

// TestBootstrapBatchedRepairMatchesStandaloneRepair seeds a database that triggers
// all three bootstrap repairs (inconsistent-attempt UPDATE, revision-baseline
// provenance correction, and legacy dependency-cycle repair) and asserts the
// batched bootstrap produces the same durable outcome as the unchanged standalone
// per-collection / queryLocked repair path. Event ids and timestamps are random, so
// the comparison normalizes to (event_type, task_id, payload) sets, the surviving
// edge set, and per-attempt status.
func TestBootstrapBatchedRepairMatchesStandaloneRepair(t *testing.T) {
	installFakeDolt(t)

	// Store A: identical seed repaired through the batched bootstrap seam.
	a := newRepairFixtureStore(t)
	if err := a.Bootstrap(); err != nil { // re-run: batched repair over the seeded rows
		t.Fatal(err)
	}

	// Store B: identical seed repaired through the unchanged standalone path.
	b := newRepairFixtureStore(t)
	if err := b.withLock(func() error {
		rows, err := b.queryLocked("SELECT id FROM attempts WHERE status='SUCCEEDED' AND exit_code IS NOT NULL AND exit_code <> 0")
		if err != nil {
			return err
		}
		if len(rows) > 0 {
			if err = b.mutateLocked("UPDATE attempts SET status='FAILED' WHERE status='SUCCEEDED' AND exit_code IS NOT NULL AND exit_code <> 0"); err != nil {
				return err
			}
			if err = b.semanticCommitLocked("standalone inconsistent attempts"); err != nil {
				return err
			}
		}
		if _, err = b.repairRevisionBaselineProvenanceLocked(); err != nil {
			return err
		}
		if _, _, err = b.repairLegacyDependencyCyclesLocked("COL-rep"); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	sa := repairSnapshot(t, a)
	sb := repairSnapshot(t, b)
	if sa != sb {
		t.Fatalf("batched bootstrap repair diverged from standalone repair:\n batched=%s\n standalone=%s", sa, sb)
	}
	// Confirm the fixture genuinely exercised all three repairs rather than
	// comparing two trivially-empty states.
	if strings.Contains(sa, "T-impl T-pr depends_on") || !strings.Contains(sa, "T-pr T-impl depends_on") {
		t.Fatalf("expected the implement->plan_review cycle edge removed and plan_review->implement kept:\n%s", sa)
	}
	if !strings.Contains(sa, "revision_plan_seal") || !strings.Contains(sa, "corrected_from=compatibility_plan_seal") {
		t.Fatalf("expected a revision-baseline provenance correction:\n%s", sa)
	}
	if !strings.Contains(sa, "A-1 FAILED") || !strings.Contains(sa, "A-2 FAILED") {
		t.Fatalf("expected inconsistent attempts repaired to FAILED:\n%s", sa)
	}
}

// newRepairFixtureStore builds a bootstrapped store seeded with: two inconsistent
// attempts, a compatibility_plan_seal critic baseline event tied to a critic +
// plan_review + coordinator and a collection revision, and a legacy
// implement<->plan_review dependency cycle. It does NOT run any repair.
func newRepairFixtureStore(t *testing.T) *Store {
	t.Helper()
	cfg := GlobalConfig{Home: t.TempDir()}
	s, err := NewStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	now := UTCNow()
	statements := []string{
		"INSERT INTO projects (id,name,root_path,base_branch,config_json,created_at,updated_at) VALUES ('PRJ-rep','rep','/rep','main','{}'," + SQLLiteral(now) + "," + SQLLiteral(now) + ")",
		"INSERT INTO collections (id,project_id,title,objective,status,created_at,updated_at) VALUES ('COL-rep','PRJ-rep','t','o','RUNNING'," + SQLLiteral(now) + "," + SQLLiteral(now) + ")",
		// coordinator, implement, plan_review
		"INSERT INTO tasks (id,project_id,collection_id,profile,title,objective,status,requested_by_task_id,created_at,updated_at) VALUES ('T-coord','PRJ-rep','COL-rep','coordinator','c','o','COMPLETE',NULL," + SQLLiteral(now) + "," + SQLLiteral(now) + ")",
		"INSERT INTO tasks (id,project_id,collection_id,profile,title,objective,status,requested_by_task_id,created_at,updated_at) VALUES ('T-impl','PRJ-rep','COL-rep','implement','i','o','READY','T-coord'," + SQLLiteral(now) + "," + SQLLiteral(now) + ")",
		"INSERT INTO tasks (id,project_id,collection_id,profile,title,objective,status,requested_by_task_id,created_at,updated_at) VALUES ('T-pr','PRJ-rep','COL-rep','plan_review','p','o','READY','T-coord'," + SQLLiteral(now) + "," + SQLLiteral(now) + ")",
		// a second critic/plan_review pair for the revision-baseline correction
		"INSERT INTO tasks (id,project_id,collection_id,profile,title,objective,status,requested_by_task_id,created_at,updated_at) VALUES ('T-critic','PRJ-rep','COL-rep','critic','k','o','COMPLETE','T-coord'," + SQLLiteral(now) + "," + SQLLiteral(now) + ")",
		"INSERT INTO tasks (id,project_id,collection_id,profile,title,objective,status,requested_by_task_id,created_at,updated_at) VALUES ('T-review','PRJ-rep','COL-rep','plan_review','r','o','COMPLETE','T-coord'," + SQLLiteral(now) + "," + SQLLiteral(now) + ")",
		// legacy dependency cycle: implement -> plan_review and plan_review -> implement
		"INSERT INTO task_edges (from_task_id,to_task_id,collection_id,relation,created_at) VALUES ('T-impl','T-pr','COL-rep','depends_on'," + SQLLiteral(now) + ")",
		"INSERT INTO task_edges (from_task_id,to_task_id,collection_id,relation,created_at) VALUES ('T-pr','T-impl','COL-rep','depends_on'," + SQLLiteral(now) + ")",
		// a surviving acyclic edge so the DELETE is provably targeted
		"INSERT INTO task_edges (from_task_id,to_task_id,collection_id,relation,created_at) VALUES ('T-critic','T-review','COL-rep','depends_on'," + SQLLiteral(now) + ")",
		// inconsistent attempts
		"INSERT INTO attempts (id,task_id,attempt_no,status,exit_code,created_at) VALUES ('A-1','T-impl',1,'SUCCEEDED',7," + SQLLiteral(now) + ")",
		"INSERT INTO attempts (id,task_id,attempt_no,status,exit_code,created_at) VALUES ('A-2','T-coord',1,'SUCCEEDED',3," + SQLLiteral(now) + ")",
		// a collection revision keyed by the coordinator, and the compatibility baseline event
		"INSERT INTO collection_revisions (id,project_id,collection_id,reason,status,coordinator_task_id,created_at) VALUES ('REV-1','PRJ-rep','COL-rep','rev','OPEN','T-coord'," + SQLLiteral(now) + ")",
		"INSERT INTO events (id,project_id,collection_id,task_id,attempt_id,event_type,payload_json,created_at) VALUES ('EVT-BASE','PRJ-rep','COL-rep','T-critic',NULL,'CRITIC_PLAN_BASELINE'," + SQLLiteral(`{"source":"compatibility_plan_seal","plan_review_task_id":"T-review","fingerprint":"fp1"}`) + "," + SQLLiteral(now) + ")",
	}
	if err = s.Mutate(strings.Join(statements, ";"), "seed repair fixture"); err != nil {
		t.Fatal(err)
	}
	return s
}

// repairSnapshot renders the repair-relevant durable state in a form that ignores
// random event ids and correction timestamps.
func repairSnapshot(t *testing.T, s *Store) string {
	t.Helper()
	var sb strings.Builder
	attempts, err := s.Query("SELECT id,status,exit_code FROM attempts ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	sb.WriteString("attempts:\n")
	for _, r := range attempts {
		sb.WriteString("  " + S(r, "id") + " " + S(r, "status") + " " + S(r, "exit_code") + "\n")
	}
	edges, err := s.Query("SELECT from_task_id,to_task_id,relation FROM task_edges ORDER BY from_task_id,to_task_id,relation")
	if err != nil {
		t.Fatal(err)
	}
	sb.WriteString("edges:\n")
	for _, r := range edges {
		sb.WriteString("  " + S(r, "from_task_id") + " " + S(r, "to_task_id") + " " + S(r, "relation") + "\n")
	}
	repairEvents, err := s.Query("SELECT task_id,payload_json FROM events WHERE event_type='LEGACY_DEPENDENCY_CYCLE_REPAIRED' ORDER BY payload_json")
	if err != nil {
		t.Fatal(err)
	}
	sb.WriteString("cycle_events:\n")
	for _, r := range repairEvents {
		sb.WriteString("  " + S(r, "task_id") + " " + S(r, "payload_json") + "\n")
	}
	corrections, err := s.Query("SELECT task_id,payload_json FROM events WHERE event_type='CRITIC_PLAN_BASELINE' AND payload_json LIKE '%corrected_from%' ORDER BY payload_json")
	if err != nil {
		t.Fatal(err)
	}
	sb.WriteString("corrections:\n")
	for _, r := range corrections {
		payload := JSONMap(S(r, "payload_json"))
		// Drop the per-run revision id (random event fields already excluded).
		sb.WriteString("  " + S(r, "task_id") + " source=" + S(payload, "source") + " corrected_from=" + S(payload, "corrected_from") + " fingerprint=" + S(payload, "fingerprint") + " plan_review=" + S(payload, "plan_review_task_id") + "\n")
	}
	return sb.String()
}

func countSQLOperations(t *testing.T, path string) int {
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
		if strings.HasPrefix(strings.TrimSpace(line), "sql") {
			n++
		}
	}
	return n
}
