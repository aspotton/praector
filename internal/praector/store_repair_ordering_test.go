package praector

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// seedStandaloneCycleStore builds a bootstrapped store with one collection holding
// two repairable implement<->plan_review cycles plus one surviving acyclic
// critic->plan_review edge, seeded with a deliberately scrambled (non-sorted)
// edge-insertion order. It issues no repair; the caller chooses whether to drive
// the standalone RepairLegacyDependencyCycles path or read the shared helper.
func seedStandaloneCycleStore(t *testing.T, logPath string) *Store {
	t.Helper()
	cfg := GlobalConfig{Home: t.TempDir()}
	if logPath != "" {
		t.Setenv("PRAECTOR_FAKE_DOLT_LOG", logPath)
	}
	s, err := NewStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	now := UTCNow()
	statements := []string{
		"INSERT INTO projects (id,name,root_path,base_branch,config_json,created_at,updated_at) VALUES ('PRJ-std','s','/s','main','{}'," + SQLLiteral(now) + "," + SQLLiteral(now) + ")",
		"INSERT INTO collections (id,project_id,title,objective,status,created_at,updated_at) VALUES ('COL-std','PRJ-std','t','o','RUNNING'," + SQLLiteral(now) + "," + SQLLiteral(now) + ")",
		"INSERT INTO tasks (id,project_id,collection_id,profile,title,objective,status,requested_by_task_id,created_at,updated_at) VALUES ('T-i1','PRJ-std','COL-std','implement','i','o','READY',NULL," + SQLLiteral(now) + "," + SQLLiteral(now) + ")",
		"INSERT INTO tasks (id,project_id,collection_id,profile,title,objective,status,requested_by_task_id,created_at,updated_at) VALUES ('T-i2','PRJ-std','COL-std','implement','i','o','READY',NULL," + SQLLiteral(now) + "," + SQLLiteral(now) + ")",
		"INSERT INTO tasks (id,project_id,collection_id,profile,title,objective,status,requested_by_task_id,created_at,updated_at) VALUES ('T-p1','PRJ-std','COL-std','plan_review','p','o','READY',NULL," + SQLLiteral(now) + "," + SQLLiteral(now) + ")",
		"INSERT INTO tasks (id,project_id,collection_id,profile,title,objective,status,requested_by_task_id,created_at,updated_at) VALUES ('T-p2','PRJ-std','COL-std','plan_review','p','o','READY',NULL," + SQLLiteral(now) + "," + SQLLiteral(now) + ")",
		"INSERT INTO tasks (id,project_id,collection_id,profile,title,objective,status,requested_by_task_id,created_at,updated_at) VALUES ('T-crit','PRJ-std','COL-std','critic','k','o','COMPLETE',NULL," + SQLLiteral(now) + "," + SQLLiteral(now) + ")",
		"INSERT INTO tasks (id,project_id,collection_id,profile,title,objective,status,requested_by_task_id,created_at,updated_at) VALUES ('T-r1','PRJ-std','COL-std','plan_review','r','o','READY',NULL," + SQLLiteral(now) + "," + SQLLiteral(now) + ")",
		// deliberately bad (scrambled/reverse) edge-insertion order
		"INSERT INTO task_edges (from_task_id,to_task_id,collection_id,relation,created_at) VALUES ('T-p2','T-i2','COL-std','depends_on'," + SQLLiteral(now) + ")",
		"INSERT INTO task_edges (from_task_id,to_task_id,collection_id,relation,created_at) VALUES ('T-p1','T-i1','COL-std','depends_on'," + SQLLiteral(now) + ")",
		"INSERT INTO task_edges (from_task_id,to_task_id,collection_id,relation,created_at) VALUES ('T-crit','T-r1','COL-std','depends_on'," + SQLLiteral(now) + ")",
		"INSERT INTO task_edges (from_task_id,to_task_id,collection_id,relation,created_at) VALUES ('T-i2','T-p2','COL-std','depends_on'," + SQLLiteral(now) + ")",
		"INSERT INTO task_edges (from_task_id,to_task_id,collection_id,relation,created_at) VALUES ('T-i1','T-p1','COL-std','depends_on'," + SQLLiteral(now) + ")",
	}
	if err = s.Mutate(strings.Join(statements, ";"), "seed standalone cycle fixture"); err != nil {
		t.Fatal(err)
	}
	return s
}

// TestStandaloneCycleRepairDeterministicEmission proves the scheduler-time /
// standalone RepairLegacyDependencyCycles path is explicitly ordered (Part B,
// Option A). It drives Store.RepairLegacyDependencyCycles DIRECTLY (not Bootstrap,
// which the batched parity test TestBootstrapBatchedRepairMatchesStandaloneRepair
// owns) over two repairable cycles seeded in a deliberately bad edge-insertion
// order. It asserts — via the PRAECTOR_FAKE_DOLT_LOG seam plus countSQLMatching —
// that BOTH repair reads literally carry their new ORDER BY clause. This text
// presence, not an ascending DELETE order read off insertion order, is the
// fix-detecting check: the fake-Dolt harness is SQLite-backed and task_edges has a
// (from_task_id,to_task_id,relation) PRIMARY KEY, so the unfixed read can already
// return a stable order; reverting either ORDER BY in store.go makes this test
// fail. It also asserts the surviving-edge SET is the expected repaired outcome
// and that the emitted DELETE ordering is deterministic/ascending.
func TestStandaloneCycleRepairDeterministicEmission(t *testing.T) {
	installFakeDolt(t)
	logPath := filepath.Join(t.TempDir(), "sqlops.log")
	s := seedStandaloneCycleStore(t, logPath)

	// Clear the invocation log immediately before the repair so only the standalone
	// repair's own reads/writes are counted.
	if err := os.WriteFile(logPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	repaired, _, err := s.RepairLegacyDependencyCycles("COL-std")
	if err != nil {
		t.Fatal(err)
	}
	if repaired != 2 {
		t.Fatalf("expected 2 standalone cycle repairs, got %d", repaired)
	}

	// Fix-detecting assertion #1: the per-collection tasks read carries ORDER BY
	// created_at,id (standalone equivalent of the batched collection_id,created_at,id).
	if n := countSQLMatching(t, logPath, "SELECT id,project_id,profile,status FROM tasks WHERE collection_id='COL-std' ORDER BY created_at,id"); n != 1 {
		t.Fatalf("expected the standalone tasks read to carry ORDER BY created_at,id, got %d matches", n)
	}
	// Fix-detecting assertion #2: the shared dependencyEdgesLocked depends_on read
	// carries ORDER BY from_task_id,to_task_id (Option A).
	if n := countSQLMatching(t, logPath, "SELECT from_task_id,to_task_id FROM task_edges WHERE collection_id='COL-std' AND relation='depends_on' ORDER BY from_task_id,to_task_id"); n != 1 {
		t.Fatalf("expected the shared edge read to carry ORDER BY from_task_id,to_task_id, got %d matches", n)
	}

	// Surviving edge SET is the expected repaired outcome (both implement->plan_review
	// cycle edges removed; the reverse edges and the acyclic critic edge survive).
	edges, err := s.Query("SELECT from_task_id,to_task_id FROM task_edges WHERE collection_id='COL-std' AND relation='depends_on'")
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
		t.Fatalf("standalone surviving edge SET changed: got %v want %v", got, want)
	}

	// The emitted DELETE order is deterministic/ascending because the read is ordered
	// (secondary to the ORDER BY text-presence checks above).
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

// TestDependencyEdgesLockedOrdered proves the shared dependencyEdgesLocked helper
// returns a collection's depends_on edges ordered by (from_task_id,to_task_id)
// regardless of insertion order. This is the strongest of the Part B regressions:
// it asserts the helper output directly, independent of any upstream scan order.
func TestDependencyEdgesLockedOrdered(t *testing.T) {
	installFakeDolt(t)
	s := seedStandaloneCycleStore(t, "")

	var edges [][2]string
	if err := s.withLock(func() error {
		var err error
		edges, err = s.dependencyEdgesLocked("COL-std")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(edges) == 0 {
		t.Fatal("expected dependency edges")
	}
	sorted := append([][2]string(nil), edges...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i][0] != sorted[j][0] {
			return sorted[i][0] < sorted[j][0]
		}
		return sorted[i][1] < sorted[j][1]
	})
	for i := range edges {
		if edges[i] != sorted[i] {
			t.Fatalf("dependencyEdgesLocked output not ordered by (from,to): got %v want %v", edges, sorted)
		}
	}
}
