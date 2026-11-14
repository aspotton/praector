package praector

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
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

// countSQLMatching counts `dolt sql` invocation-log lines whose recorded argument
// string contains substr, so a test can separate one query shape (the removed
// per-candidate task-pair read, or a retained collection_revisions / collections
// read) from the other SQL a bootstrap runs.
func countSQLMatching(t *testing.T, path, substr string) int {
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
		if strings.HasPrefix(strings.TrimSpace(line), "sql") && strings.Contains(line, substr) {
			n++
		}
	}
	return n
}

// TestBootstrapRevisionBaselineDropsTaskPairRead proves the Phase 1.1 reduction of
// the revision-baseline provenance repair: on the batched bootstrap path the
// per-candidate `tasks WHERE id IN (criticID,planReviewID)` read is gone (its rows
// come from loadInvariantRowsLocked's snapshot), while the per-candidate
// collection_revisions LIMIT 1 selection and the collections project_id fallback
// REMAIN. It seeds three matching critic/plan_review candidates (the third with a
// project_id-less revision to exercise the collections fallback) and asserts a
// strictly lower dolt sql count than the pre-Phase-1.1 shape by exactly one fewer
// read per candidate, not zero per-candidate reads.
func TestBootstrapRevisionBaselineDropsTaskPairRead(t *testing.T) {
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
	now := UTCNow()
	statements := []string{
		"INSERT INTO projects (id,name,root_path,base_branch,config_json,created_at,updated_at) VALUES ('PRJ-b','b','/b','main','{}'," + SQLLiteral(now) + "," + SQLLiteral(now) + ")",
		"INSERT INTO collections (id,project_id,title,objective,status,created_at,updated_at) VALUES ('COL-count','PRJ-b','t','o','RUNNING'," + SQLLiteral(now) + "," + SQLLiteral(now) + ")",
	}
	// Three independent coordinator/critic/plan_review candidate triples.
	for i := 1; i <= 3; i++ {
		k := "T-k" + string(rune('0'+i))
		c := "T-c" + string(rune('0'+i))
		r := "T-r" + string(rune('0'+i))
		statements = append(statements,
			"INSERT INTO tasks (id,project_id,collection_id,profile,title,objective,status,requested_by_task_id,created_at,updated_at) VALUES ("+SQLLiteral(k)+",'PRJ-b','COL-count','coordinator','c','o','COMPLETE',NULL,"+SQLLiteral(now)+","+SQLLiteral(now)+")",
			"INSERT INTO tasks (id,project_id,collection_id,profile,title,objective,status,requested_by_task_id,created_at,updated_at) VALUES ("+SQLLiteral(c)+",'PRJ-b','COL-count','critic','k','o','COMPLETE',"+SQLLiteral(k)+","+SQLLiteral(now)+","+SQLLiteral(now)+")",
			"INSERT INTO tasks (id,project_id,collection_id,profile,title,objective,status,requested_by_task_id,created_at,updated_at) VALUES ("+SQLLiteral(r)+",'PRJ-b','COL-count','plan_review','r','o','COMPLETE',"+SQLLiteral(k)+","+SQLLiteral(now)+","+SQLLiteral(now)+")",
		)
		// Candidate 3 uses a project_id-less revision to exercise the collections fallback.
		proj := "'PRJ-b'"
		if i == 3 {
			proj = "''"
		}
		statements = append(statements,
			"INSERT INTO collection_revisions (id,project_id,collection_id,reason,status,coordinator_task_id,created_at) VALUES ("+SQLLiteral("REV-"+string(rune('0'+i)))+","+proj+",'COL-count','rev','OPEN',"+SQLLiteral(k)+","+SQLLiteral(now)+")",
			"INSERT INTO events (id,project_id,collection_id,task_id,attempt_id,event_type,payload_json,created_at) VALUES ("+SQLLiteral("EVT-"+string(rune('0'+i)))+",'PRJ-b','COL-count',"+SQLLiteral(c)+",NULL,'CRITIC_PLAN_BASELINE',"+SQLLiteral(`{"source":"compatibility_plan_seal","plan_review_task_id":"`+r+`","fingerprint":"fp`+string(rune('0'+i))+`"}`)+","+SQLLiteral(now)+")",
		)
	}
	if err = s.Mutate(strings.Join(statements, ";"), "seed revision-baseline count fixture"); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(logPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err = s.Bootstrap(); err != nil { // batched repair over the seeded rows
		t.Fatal(err)
	}

	// GOAL #1: the per-candidate tasks-pair read is entirely gone.
	if n := countSQLMatching(t, logPath, "FROM tasks WHERE id IN ("); n != 0 {
		t.Fatalf("expected zero per-candidate tasks WHERE id IN reads after Phase 1.1, got %d", n)
	}
	// The batched snapshot read (with the Phase 1.1 ORDER BY) is the source of the
	// task rows instead.
	if n := countSQLMatching(t, logPath, "FROM tasks ORDER BY collection_id,created_at,id"); n != 1 {
		t.Fatalf("expected exactly one batched snapshot tasks read, got %d", n)
	}
	// Retained per-candidate reads (assert the delta, not zero): one
	// collection_revisions LIMIT 1 per matching candidate.
	if n := countSQLMatching(t, logPath, "FROM collection_revisions WHERE"); n != 3 {
		t.Fatalf("expected 3 retained per-candidate collection_revisions reads, got %d", n)
	}
	// And the collections project_id fallback read remains for the project_id-less
	// candidate (exactly one).
	if n := countSQLMatching(t, logPath, "FROM collections WHERE id="); n != 1 {
		t.Fatalf("expected 1 retained collections fallback read, got %d", n)
	}
}

// TestComputeLegacyRepairsOrderIndependent proves the repaired edge SET is
// invariant to task/edge read order: computeLegacyRepairs reaches a fixpoint, so
// shuffling the input edge order cannot change the surviving-edge SET or the set of
// removed edges. This is the SET-membership half of GOAL #6b (raw statement/event
// order is intentionally not asserted, per the repairSnapshot precedent).
func TestComputeLegacyRepairsOrderIndependent(t *testing.T) {
	taskByID := map[string]Row{
		"T-i1": {"id": "T-i1", "profile": "implement"},
		"T-i2": {"id": "T-i2", "profile": "implement"},
		"T-p1": {"id": "T-p1", "profile": "plan_review"},
		"T-p2": {"id": "T-p2", "profile": "plan_review"},
	}
	base := [][2]string{
		{"T-i1", "T-p1"}, {"T-p1", "T-i1"},
		{"T-i2", "T-p2"}, {"T-p2", "T-i2"},
	}
	setKey := func(edges [][2]string) []string {
		out := make([]string, 0, len(edges))
		for _, e := range edges {
			out = append(out, e[0]+">"+e[1])
		}
		sort.Strings(out)
		return out
	}
	eq := func(a, b []string) bool {
		if len(a) != len(b) {
			return false
		}
		for i := range a {
			if a[i] != b[i] {
				return false
			}
		}
		return true
	}

	// Enumerate every permutation of the four edges and assert the surviving SET and
	// the removed SET stay identical across every read order.
	var permute func([][2]string, int)
	var survivingSets, removedSets [][]string
	permute = func(e [][2]string, k int) {
		if k == len(e) {
			repairs, remaining := computeLegacyRepairs(taskByID, append([][2]string(nil), e...))
			survivingSets = append(survivingSets, setKey(remaining))
			var rs []string
			for _, r := range repairs {
				rs = append(rs, r.from+">"+r.to)
			}
			sort.Strings(rs)
			removedSets = append(removedSets, rs)
			return
		}
		for i := k; i < len(e); i++ {
			e[k], e[i] = e[i], e[k]
			permute(e, k+1)
			e[k], e[i] = e[i], e[k]
		}
	}
	permute(append([][2]string(nil), base...), 0)

	wantSurviving := []string{"T-p1>T-i1", "T-p2>T-i2"}
	wantRemoved := []string{"T-i1>T-p1", "T-i2>T-p2"}
	if len(survivingSets) == 0 {
		t.Fatal("no permutations evaluated")
	}
	for i := range survivingSets {
		if !eq(survivingSets[i], wantSurviving) {
			t.Fatalf("permutation %d surviving edge SET changed: got %v want %v", i, survivingSets[i], wantSurviving)
		}
		if !eq(removedSets[i], wantRemoved) {
			t.Fatalf("permutation %d removed edge SET changed: got %v want %v", i, removedSets[i], wantRemoved)
		}
	}
}

// cycleDeleteRe matches one cycle-repair DELETE statement's endpoints inside a
// semicolon-joined mutate batch, in the order the repairs were emitted.
var cycleDeleteRe = regexp.MustCompile(`DELETE FROM task_edges[^;]*?from_task_id='([^']*)' AND to_task_id='([^']*)'`)

// TestBootstrapCycleRepairDeterministicEmission proves GOAL #2/#6b on the batched
// path: the edge read carries the explicit ORDER BY so the emitted cycle-repair
// DELETE ordering is deterministic (ascending), and the surviving edge SET matches
// the expected repaired outcome. Emission determinism is read off the real
// bootstrap mutate via the PATH-shim log, and surviving SET membership via Query.
func TestBootstrapCycleRepairDeterministicEmission(t *testing.T) {
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
	now := UTCNow()
	statements := []string{
		"INSERT INTO projects (id,name,root_path,base_branch,config_json,created_at,updated_at) VALUES ('PRJ-c','c','/c','main','{}'," + SQLLiteral(now) + "," + SQLLiteral(now) + ")",
		"INSERT INTO collections (id,project_id,title,objective,status,created_at,updated_at) VALUES ('COL-c','PRJ-c','t','o','RUNNING'," + SQLLiteral(now) + "," + SQLLiteral(now) + ")",
		"INSERT INTO tasks (id,project_id,collection_id,profile,title,objective,status,requested_by_task_id,created_at,updated_at) VALUES ('T-i1','PRJ-c','COL-c','implement','i','o','READY',NULL," + SQLLiteral(now) + "," + SQLLiteral(now) + ")",
		"INSERT INTO tasks (id,project_id,collection_id,profile,title,objective,status,requested_by_task_id,created_at,updated_at) VALUES ('T-i2','PRJ-c','COL-c','implement','i','o','READY',NULL," + SQLLiteral(now) + "," + SQLLiteral(now) + ")",
		"INSERT INTO tasks (id,project_id,collection_id,profile,title,objective,status,requested_by_task_id,created_at,updated_at) VALUES ('T-p1','PRJ-c','COL-c','plan_review','p','o','READY',NULL," + SQLLiteral(now) + "," + SQLLiteral(now) + ")",
		"INSERT INTO tasks (id,project_id,collection_id,profile,title,objective,status,requested_by_task_id,created_at,updated_at) VALUES ('T-p2','PRJ-c','COL-c','plan_review','p','o','READY',NULL," + SQLLiteral(now) + "," + SQLLiteral(now) + ")",
		"INSERT INTO tasks (id,project_id,collection_id,profile,title,objective,status,requested_by_task_id,created_at,updated_at) VALUES ('T-crit','PRJ-c','COL-c','critic','k','o','COMPLETE',NULL," + SQLLiteral(now) + "," + SQLLiteral(now) + ")",
		"INSERT INTO tasks (id,project_id,collection_id,profile,title,objective,status,requested_by_task_id,created_at,updated_at) VALUES ('T-r1','PRJ-c','COL-c','plan_review','r','o','READY',NULL," + SQLLiteral(now) + "," + SQLLiteral(now) + ")",
		// two repairable cycles + one surviving acyclic edge
		"INSERT INTO task_edges (from_task_id,to_task_id,collection_id,relation,created_at) VALUES ('T-i1','T-p1','COL-c','depends_on'," + SQLLiteral(now) + ")",
		"INSERT INTO task_edges (from_task_id,to_task_id,collection_id,relation,created_at) VALUES ('T-p1','T-i1','COL-c','depends_on'," + SQLLiteral(now) + ")",
		"INSERT INTO task_edges (from_task_id,to_task_id,collection_id,relation,created_at) VALUES ('T-i2','T-p2','COL-c','depends_on'," + SQLLiteral(now) + ")",
		"INSERT INTO task_edges (from_task_id,to_task_id,collection_id,relation,created_at) VALUES ('T-p2','T-i2','COL-c','depends_on'," + SQLLiteral(now) + ")",
		"INSERT INTO task_edges (from_task_id,to_task_id,collection_id,relation,created_at) VALUES ('T-crit','T-r1','COL-c','depends_on'," + SQLLiteral(now) + ")",
	}
	if err = s.Mutate(strings.Join(statements, ";"), "seed cycle fixture"); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(logPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err = s.Bootstrap(); err != nil {
		t.Fatal(err)
	}

	// The batched edge read carries the deterministic ORDER BY (GOAL #2).
	if n := countSQLMatching(t, logPath, "FROM task_edges WHERE relation='depends_on' ORDER BY collection_id,from_task_id,to_task_id"); n != 1 {
		t.Fatalf("expected one batched edge read with the deterministic ORDER BY, got %d", n)
	}

	// Surviving edge SET is exactly the repaired outcome (cycles removed, the two
	// reverse edges and the acyclic critic edge survive).
	edges, err := s.Query("SELECT from_task_id,to_task_id FROM task_edges WHERE relation='depends_on'")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range edges {
		got = append(got, S(r, "from_task_id")+">"+S(r, "to_task_id"))
	}
	sort.Strings(got)
	want := []string{"T-crit>T-r1", "T-p1>T-i1", "T-p2>T-i2"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("surviving edge SET changed: got %v want %v", got, want)
	}

	// Emitted DELETE ordering is deterministic (ascending from_task_id) because the
	// read is ordered.
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	var seq []string
	for _, line := range strings.Split(string(data), "\n") {
		for _, m := range cycleDeleteRe.FindAllStringSubmatch(line, -1) {
			seq = append(seq, m[1])
		}
	}
	if len(seq) != 2 {
		t.Fatalf("expected two cycle-repair DELETEs, got %v", seq)
	}
	if !(seq[0] <= seq[1]) {
		t.Fatalf("emitted DELETE order not deterministic/ascending: %v", seq)
	}
}
