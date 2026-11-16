package praector

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Durable-behavior tests for the lemmings-retro control-plane hardening fixes 3-5:
//
//   - fix 5: one root failure yields exactly ONE Coordinator attention (per-attempt
//     single-attention latch at the scheduler call sites);
//   - fix 3: a terminal BLOCKED Collection names its mechanical holding predicate
//     (COVERAGE_GAP with the offending implement ids) instead of the speculative
//     "may be stuck on a recovery or floor-predicate bug" string, and verifies carry
//     edges are exposed read-only on the graph surface;
//   - fix 4: an integration git merge conflict drives at most ONE automatic
//     re-integration gated by the working-tree-free `git merge-tree --write-tree`
//     exit-code probe, keeping FAILED-with-detail on a persistent conflict and never
//     weakening fast-forward-safe acceptance.

// ---- fix 5: attention dedupe latch --------------------------------------

func attentionTaskCount(t *testing.T, s *Store, c Row) int {
	t.Helper()
	tasks, err := s.ListTasks(S(c, "id"))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, task := range tasks {
		if S(task, "profile") == "coordinator" && S(task, "title") == "Coordinator attention" {
			n++
		}
	}
	return n
}

// (5a) The latch collapses two attention requests for the SAME attempt/root failure
// into exactly one Coordinator attention task; a fresh latch (a distinct later
// failure) still raises its own attention.
func TestTaskFailureAttentionLatchRaisesOneAttention(t *testing.T) {
	s, cfg := newFakeStore(t)
	p, err := s.RegisterProject(testProjectConfig(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	c, coord, err := s.CreateCollectionWithCoordinator(S(p, "id"), "Attention dedupe", "Add feature", "Plan")
	if err != nil {
		t.Fatal(err)
	}
	impl, _, err := s.CreateTask(CreateTaskOptions{ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "implement", Title: "Impl", Objective: "o", Reason: "r", ParentTaskID: S(coord, "id"), WorkspaceMode: "collection", Depth: 1, AllowDedupe: false})
	if err != nil {
		t.Fatal(err)
	}
	sch, err := NewScheduler(s, cfg, 1)
	if err != nil {
		t.Fatal(err)
	}
	// Integration-failed site then generic task-failed site for the SAME task+cause.
	raised := false
	sch.raiseTaskFailureAttention(&raised, p, S(impl, "id"), "Integration failed for "+S(impl, "id")+": conflict")
	sch.raiseTaskFailureAttention(&raised, p, S(impl, "id"), "Task failed and may require replacement")
	if got := attentionTaskCount(t, s, c); got != 1 {
		t.Fatalf("one root failure must yield exactly one attention across both sites, got %d", got)
	}
	// A distinct later failure (fresh latch) still raises.
	raised2 := false
	sch.raiseTaskFailureAttention(&raised2, p, S(impl, "id"), "A different root cause")
	if got := attentionTaskCount(t, s, c); got != 2 {
		t.Fatalf("a distinct later failure must still raise its own attention, got %d", got)
	}
}

// (5b) A nil latch (an isolated site) always raises; the latch is opt-in per call.
func TestTaskFailureAttentionNilLatchAlwaysRaises(t *testing.T) {
	s, cfg := newFakeStore(t)
	p, _ := s.RegisterProject(testProjectConfig(t.TempDir()))
	c, coord, _ := s.CreateCollectionWithCoordinator(S(p, "id"), "Attention nil latch", "Add feature", "Plan")
	impl, _, _ := s.CreateTask(CreateTaskOptions{ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "implement", Title: "Impl", Objective: "o", Reason: "r", ParentTaskID: S(coord, "id"), WorkspaceMode: "collection", Depth: 1, AllowDedupe: false})
	sch, _ := NewScheduler(s, cfg, 1)
	sch.raiseTaskFailureAttention(nil, p, S(impl, "id"), "cause A")
	sch.raiseTaskFailureAttention(nil, p, S(impl, "id"), "cause B")
	if got := attentionTaskCount(t, s, c); got != 2 {
		t.Fatalf("without a latch each distinct request raises, got %d", got)
	}
}

// ---- fix 3: BLOCKED-reason observability --------------------------------

// coverageGapBlockedCollection builds a coherent (post-cutoff) terminal Collection
// whose single implement is covered by NOTHING but a passing review that carries no
// verifies edge and is not a review parent — the incident coverage-gap shape that
// keeps the collection BLOCKED.
func coverageGapBlockedCollection(t *testing.T) (*Store, Row, Row, Row) {
	t.Helper()
	s, _ := newFakeStore(t)
	p, err := s.RegisterProject(testProjectConfig(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	c, coord, err := s.CreateCollectionWithCoordinator(S(p, "id"), "Blocked reason", "Add a feature to the product", "Plan")
	if err != nil {
		t.Fatal(err)
	}
	impl, _, err := s.CreateTask(CreateTaskOptions{ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "implement", Title: "Impl", Objective: "o", Reason: "r", ParentTaskID: S(coord, "id"), WorkspaceMode: "collection", Depth: 1, AllowDedupe: false})
	if err != nil {
		t.Fatal(err)
	}
	review, _, err := s.CreateTask(CreateTaskOptions{ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "review", Title: "Review", Objective: "review", Reason: "review", ParentTaskID: S(coord, "id"), DependsOn: []string{S(impl, "id")}, WorkspaceMode: "collection", Depth: 1, AllowDedupe: false})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetTaskStatus(S(impl, "id"), TaskComplete, "impl complete"); err != nil {
		t.Fatal(err)
	}
	// A passing review with NO verifies edge onto the implement (the DEFERRED root
	// cause): the coverage rule still reports the implement uncovered.
	res := JSONDumps(Row{"status": "complete", "summary": "reviewed", "verdict": "pass"})
	if err = s.Mutate("UPDATE tasks SET status="+SQLLiteral(TaskComplete)+",result_json="+SQLLiteral(res)+",updated_at="+SQLLiteral(UTCNow())+" WHERE id="+SQLLiteral(S(review, "id")), "pass review without verifies edge"); err != nil {
		t.Fatal(err)
	}
	if err = s.SetTaskStatus(S(coord, "id"), TaskComplete, "coord complete"); err != nil {
		t.Fatal(err)
	}
	if err = s.SetCollectionStatus(S(c, "id"), "BLOCKED", true); err != nil {
		t.Fatal(err)
	}
	return s, p, c, impl
}

// (3a) The single shared projection names COVERAGE_GAP with the offending implement
// id, and the status read surfaces it; the collection is genuinely BLOCKED.
func TestBlockedReasonNamesCoverageGapWithImplementIDs(t *testing.T) {
	s, _, c, impl := coverageGapBlockedCollection(t)
	predicate, offending := s.CollectionBlockedReason(S(c, "id"))
	if predicate != "COVERAGE_GAP" {
		t.Fatalf("a coherent coverage-gap BLOCKED collection must name COVERAGE_GAP, got %q", predicate)
	}
	if !containsString(strings.Split(offending, ","), S(impl, "id")) {
		t.Fatalf("the blocked reason must name the offending implement id %s, got %q", S(impl, "id"), offending)
	}
	svc := &Service{store: s}
	fresh, ferr := s.GetCollection(S(c, "id"))
	if ferr != nil {
		t.Fatal(ferr)
	}
	cs := svc.collectionStatus(fresh)
	if cs.BlockedReason != "COVERAGE_GAP" {
		t.Fatalf("the status snapshot must surface the named blocked reason, got %q", cs.BlockedReason)
	}
}

// (3b) Byte-identity: a non-BLOCKED or healthy COMPLETE collection projects an empty
// reason, so unchanged status renders gain no output.
func TestBlockedReasonEmptyWhenNotCoverageBlocked(t *testing.T) {
	s, cfg, c, coord := gateFreeFloorCollection(t, colReplayObjective)
	impl := floorImplement(t, s, c, coord)
	review, _, err := s.CreateTask(CreateTaskOptions{ProjectID: S(c, "project_id"), CollectionID: S(c, "id"), Profile: "review", Title: "Independent review", Objective: "review", Reason: "review", ParentTaskID: S(coord, "id"), DependsOn: []string{S(impl, "id")}, WorkspaceMode: "collection", Depth: 1, AllowDedupe: false})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.AddEdge(S(impl, "id"), S(review, "id"), S(c, "id"), "verifies"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SubmitResult(S(review, "id"), Row{"status": "complete", "summary": "ok", "verdict": "pass", "impact_enumeration": passingImpactEnumeration()}); err != nil {
		t.Fatal(err)
	}
	for _, task := range []Row{impl, review, coord} {
		if err = s.SetTaskStatus(S(task, "id"), TaskComplete, "terminal"); err != nil {
			t.Fatal(err)
		}
	}
	_ = cfg
	if predicate, _ := s.CollectionBlockedReason(S(c, "id")); predicate != "" {
		t.Fatalf("a collection the cores are not blocking projects no reason, got %q", predicate)
	}
}

// (3c) A passing review that verifies an implement exposes the carry edge through the
// graph read surface without raw SQL.
func TestGraphSurfaceExposesVerifiesEdges(t *testing.T) {
	s, _ := newFakeStore(t)
	p, err := s.RegisterProject(testProjectConfig(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	c, coord, err := s.CreateCollectionWithCoordinator(S(p, "id"), "Verify edges", "Add feature", "Plan")
	if err != nil {
		t.Fatal(err)
	}
	impl, _, err := s.CreateTask(CreateTaskOptions{ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "implement", Title: "Impl", Objective: "o", Reason: "r", ParentTaskID: S(coord, "id"), WorkspaceMode: "collection", Depth: 1, AllowDedupe: false})
	if err != nil {
		t.Fatal(err)
	}
	review, _, err := s.CreateTask(CreateTaskOptions{ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "review", Title: "Review", Objective: "review", Reason: "review", ParentTaskID: S(coord, "id"), DependsOn: []string{S(impl, "id")}, WorkspaceMode: "collection", Depth: 1, AllowDedupe: false})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.AddEdge(S(impl, "id"), S(review, "id"), S(c, "id"), "verifies"); err != nil {
		t.Fatal(err)
	}
	svc := &Service{store: s}
	out, err := svc.Graph(context.Background(), GraphInput{Collection: S(c, "id")})
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(out.Verifies[S(review, "id")], S(impl, "id")) {
		t.Fatalf("GraphOutput must expose the verifies carry edge, got %v", out.Verifies)
	}
}

// (3d) Source-shape guard: the speculative string is gone from scheduler.go and the
// named projection replaces it.
func TestBlockedReasonSpeculativeStringRemoved(t *testing.T) {
	src, err := os.ReadFile("scheduler.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(src), "may be stuck on a recovery or floor-predicate bug") {
		t.Fatal("scheduler.go must not retain the speculative blocked-reason string")
	}
	snap, err := os.ReadFile("store_status_snapshot.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(snap), "func collectionBlockedReason(") {
		t.Fatal("the single shared blocked-reason projection must exist over the decision cores")
	}
}

// ---- fix 4: integration-conflict resilience (git oracle) ----------------

// conflictIntegrateFixture builds a real Git project + collection + implement task.
// The collection integration branch is always advanced with a README-only sibling
// edit. When conflict is true the task edits the SAME README line differently (the
// incident content conflict); when false the task adds an independent file only.
func conflictIntegrateFixture(t *testing.T, conflict bool) (*Store, *GitWorkspace, Row, Row, Row) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	repo := t.TempDir()
	run := func(dir string, args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run(repo, "init", "-b", "main", repo)
	run(repo, "config", "user.email", "praector@test")
	run(repo, "config", "user.name", "Praector Test")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("base\n"), 0644); err != nil {
		t.Fatal(err)
	}
	run(repo, "add", ".")
	run(repo, "commit", "-m", "base")

	store, cfg := newFakeStore(t)
	project, err := store.RegisterProject(testProjectConfig(repo))
	if err != nil {
		t.Fatal(err)
	}
	collection, coordinator, err := store.CreateCollectionWithCoordinator(S(project, "id"), "Conflict resilience", "Edit the README", "Plan")
	if err != nil {
		t.Fatal(err)
	}
	task, _, err := store.CreateTask(CreateTaskOptions{ProjectID: S(project, "id"), CollectionID: S(collection, "id"), Profile: "implement", Title: "Impl", Objective: "edit readme", Reason: "test", ParentTaskID: S(coordinator, "id"), RequestedByTaskID: S(coordinator, "id"), WorkspaceMode: "collection", Depth: 1, AllowDedupe: false})
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := NewGitWorkspace(store, cfg)
	if err != nil {
		t.Fatal(err)
	}
	taskPath, err := workspace.PrepareTask(project, collection, task)
	if err != nil {
		t.Fatal(err)
	}
	task, _ = store.GetTask(S(task, "id")) // worktree_path is populated only after PrepareTask
	integrationPath, err := workspace.EnsureCollection(project, collection)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(integrationPath, "README.md"), []byte("base\nsibling line\n"), 0644); err != nil {
		t.Fatal(err)
	}
	run(integrationPath, "add", ".")
	run(integrationPath, "commit", "-m", "sibling integration edit")
	collection, _ = store.GetCollection(S(collection, "id"))
	if conflict {
		if err = os.WriteFile(filepath.Join(taskPath, "README.md"), []byte("base\ntask line\n"), 0644); err != nil {
			t.Fatal(err)
		}
	} else {
		if err = os.WriteFile(filepath.Join(taskPath, "feature.txt"), []byte("new file\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = workspace.Checkpoint(task, 1, "attempt-checkpoint"); err != nil {
		t.Fatal(err)
	}
	task, _ = store.GetTask(S(task, "id"))
	return store, workspace, project, collection, task
}

// (4a) The probe uses the git merge-tree exit status as the sole predicate: a
// conflicting re-integration reports ReintegrateConflict (exit 1).
func TestProbeReintegrationReportsConflict(t *testing.T) {
	_, workspace, project, collection, task := conflictIntegrateFixture(t, true)
	verdict, err := workspace.ProbeReintegration(project, collection, task)
	if err != nil {
		t.Fatalf("probe must not error on a conflicting graph: %v", err)
	}
	if verdict != ReintegrateConflict {
		t.Fatalf("a README-only content conflict must probe as ReintegrateConflict, got %d", verdict)
	}
}

// (4b) A clean (non-conflicting) checkpoint probes as ReintegrateClean (exit 0) and
// the integrate succeeds.
func TestProbeReintegrationCleanAndIntegrateSucceeds(t *testing.T) {
	store, workspace, project, collection, task := conflictIntegrateFixture(t, false)
	verdict, err := workspace.ProbeReintegration(project, collection, task)
	if err != nil {
		t.Fatalf("probe must not error on a clean graph: %v", err)
	}
	if verdict != ReintegrateClean {
		t.Fatalf("an independent-file checkpoint must probe clean, got %d", verdict)
	}
	sch, err := NewScheduler(store, GlobalConfig{Home: t.TempDir()}, 1)
	if err != nil {
		t.Fatal(err)
	}
	sch.Workspace = workspace
	integrated, ierr := sch.integrateWithConflictRetry(project, collection, task, "A-1")
	if ierr != nil {
		t.Fatalf("an independent-file integration must succeed, got %v", ierr)
	}
	ok, aerr := workspace.IsAncestor(S(project, "root_path"), S(task, "current_head"), integrated)
	if aerr != nil || !ok {
		t.Fatalf("checkpoint must be reachable from the integration head: %v", aerr)
	}
}

// (4c) A persistent conflict keeps FAILED-with-conflict-detail and triggers AT MOST
// one automatic re-integration: because the probe reports the persistent conflict,
// NO INTEGRATION_RETRY event is emitted and the merge is not blindly retried.
func TestPersistentConflictKeepsFailedNoSpuriousRetry(t *testing.T) {
	store, workspace, project, collection, task := conflictIntegrateFixture(t, true)
	sch, err := NewScheduler(store, GlobalConfig{Home: t.TempDir()}, 1)
	if err != nil {
		t.Fatal(err)
	}
	sch.Workspace = workspace
	_, ierr := sch.integrateWithConflictRetry(project, collection, task, "A-1")
	if ierr == nil || !strings.Contains(ierr.Error(), "Integration conflict for") {
		t.Fatalf("a persistent conflict must keep FAILED with conflict detail, got %v", ierr)
	}
	evts := collectionEventsFor(t, store, collection, "INTEGRATION_RETRY")
	if len(evts) != 0 {
		t.Fatalf("a probe-detected persistent conflict must not blindly re-integrate, got %d retry events", len(evts))
	}
}

// (4d) Fast-forward-safe acceptance is byte-identical: the ff-only acceptance path is
// unchanged by the probe (AcceptCollection still reports merge_mode ff-only).
func TestConflictProbeDoesNotWeakenFastForwardAcceptance(t *testing.T) {
	repo := t.TempDir()
	git, e := exec.LookPath("git")
	if e != nil {
		t.Skip("git unavailable")
	}
	if _, e = exec.Command(git, "init", "-b", "main", repo).CombinedOutput(); e != nil {
		t.Fatal(e)
	}
	rw := &GitWorkspace{Cfg: GlobalConfig{Home: t.TempDir()}, Git: git}
	if e = os.WriteFile(filepath.Join(repo, "base.txt"), []byte("base\n"), 0644); e != nil {
		t.Fatal(e)
	}
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command(git, append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	run("config", "user.email", "praector@test")
	run("config", "user.name", "Praector Test")
	run("add", ".")
	run("commit", "-m", "base")
	run("branch", "praector/collections/col-ff")
	run("checkout", "praector/collections/col-ff")
	if e = os.WriteFile(filepath.Join(repo, "feature.txt"), []byte("feature\n"), 0644); e != nil {
		t.Fatal(e)
	}
	run("add", ".")
	run("commit", "-m", "feature")
	run("checkout", "main")
	project := Row{"id": "PRJ-FF", "root_path": repo, "base_branch": "main"}
	collection := Row{"id": "COL-FF", "integration_branch": "praector/collections/col-ff"}
	m, e := rw.AcceptCollection(project, collection, "main", false)
	if e != nil {
		t.Fatal(e)
	}
	if S(m, "merge_mode") != "ff-only" {
		t.Fatalf("fast-forward-safe acceptance must stay ff-only, got %v", S(m, "merge_mode"))
	}
}
