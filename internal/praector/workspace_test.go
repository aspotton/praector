package praector

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func gitTest(t *testing.T, repo string, args ...string) string {
	t.Helper()
	git, e := RequireBinary("git")
	if e != nil {
		t.Skip("git unavailable")
	}
	r, e := Run(context.Background(), append([]string{git, "-C", repo}, args...), "", nil, "", true)
	if e != nil {
		t.Fatal(e)
	}
	return strings.TrimSpace(r.Stdout)
}

func prepareWorkspaceTask(t *testing.T, profile string) (*Store, *GitWorkspace, Row, Row, Row, string, string) {
	t.Helper()
	repo := t.TempDir()
	git, err := RequireBinary("git")
	if err != nil {
		t.Skip("git unavailable")
	}
	if _, err = Run(context.Background(), []string{git, "init", "-b", "main", repo}, "", nil, "", true); err != nil {
		t.Fatal(err)
	}
	gitTest(t, repo, "config", "user.email", "praector@test")
	gitTest(t, repo, "config", "user.name", "Praector Test")
	if err = os.WriteFile(filepath.Join(repo, "base.txt"), []byte("base\n"), 0644); err != nil {
		t.Fatal(err)
	}
	gitTest(t, repo, "add", ".")
	gitTest(t, repo, "commit", "-m", "base")
	if err = os.WriteFile(filepath.Join(repo, "base.txt"), []byte("base\nsecond\n"), 0644); err != nil {
		t.Fatal(err)
	}
	gitTest(t, repo, "add", ".")
	gitTest(t, repo, "commit", "-m", "second base")
	baseHead := gitTest(t, repo, "rev-parse", "HEAD")

	store, cfg := newFakeStore(t)
	project, err := store.RegisterProject(testProjectConfig(repo))
	if err != nil {
		t.Fatal(err)
	}
	collection, coordinator, err := store.CreateCollectionWithCoordinator(S(project, "id"), "Workspace invariant", "Exercise Git workspace invariants", "Plan")
	if err != nil {
		t.Fatal(err)
	}
	task, _, err := store.CreateTask(CreateTaskOptions{
		ProjectID: S(project, "id"), CollectionID: S(collection, "id"), Profile: profile,
		Title: "Workspace task", Objective: "Exercise workspace", Reason: "test",
		ParentTaskID: S(coordinator, "id"), RequestedByTaskID: S(coordinator, "id"),
		WorkspaceMode: "collection", Depth: 1, AllowDedupe: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := NewGitWorkspace(store, cfg)
	if err != nil {
		t.Fatal(err)
	}
	path, err := workspace.PrepareTask(project, collection, task)
	if err != nil {
		t.Fatal(err)
	}
	task, err = store.GetTask(S(task, "id"))
	if err != nil {
		t.Fatal(err)
	}
	collection, err = store.GetCollection(S(collection, "id"))
	if err != nil {
		t.Fatal(err)
	}
	return store, workspace, project, collection, task, path, baseHead
}

func TestImplementationBranchEscapeFailsClosedBeforeIntegration(t *testing.T) {
	store, workspace, project, collection, task, path, baseHead := prepareWorkspaceTask(t, "implement")
	assigned := S(task, "branch_name")
	gitTest(t, path, "checkout", "-b", "fix/escaped-worker-branch")
	if err := os.WriteFile(filepath.Join(path, "feature.txt"), []byte("candidate\n"), 0644); err != nil {
		t.Fatal(err)
	}
	gitTest(t, path, "add", ".")
	gitTest(t, path, "commit", "-m", "candidate on escaped branch")
	escapedHead := gitTest(t, path, "rev-parse", "HEAD")

	if _, err := workspace.Checkpoint(task, 1, "attempt-checkpoint"); err == nil || !strings.Contains(err.Error(), "must remain on Praector-assigned branch") {
		t.Fatalf("checkpoint must reject escaped branch, got %v", err)
	}
	if got := gitTest(t, S(project, "root_path"), "rev-parse", assigned); got != baseHead {
		t.Fatalf("assigned branch moved despite rejected checkpoint: got %s want %s", got, baseHead)
	}

	// Reproduce the inconsistent durable row produced by the old implementation:
	// current_head followed the worktree while branch_name stayed on the base.
	if err := store.SetTaskWorkspace(S(task, "id"), nil, nil, nil, strptr(escapedHead)); err != nil {
		t.Fatal(err)
	}
	task, _ = store.GetTask(S(task, "id"))
	if _, err := workspace.IntegrateTask(project, collection, task); err == nil || !strings.Contains(err.Error(), "must remain on Praector-assigned branch") {
		t.Fatalf("integration must reject escaped branch/current_head mismatch, got %v", err)
	}
	if got := gitTest(t, S(project, "root_path"), "rev-parse", S(collection, "integration_branch")); got != baseHead {
		t.Fatalf("collection advanced after rejected integration: got %s want %s", got, baseHead)
	}
}

func TestValidatedCheckpointIsIntegratedByImmutableCommit(t *testing.T) {
	store, workspace, project, collection, task, path, _ := prepareWorkspaceTask(t, "implement")
	if err := os.WriteFile(filepath.Join(path, "feature.txt"), []byte("candidate\n"), 0644); err != nil {
		t.Fatal(err)
	}
	head, err := workspace.Checkpoint(task, 1, "attempt-checkpoint")
	if err != nil {
		t.Fatal(err)
	}
	task, _ = store.GetTask(S(task, "id"))
	integrated, err := workspace.IntegrateTask(project, collection, task)
	if err != nil {
		t.Fatal(err)
	}
	ok, err := workspace.IsAncestor(S(project, "root_path"), head, integrated)
	if err != nil || !ok {
		t.Fatalf("checkpoint %s is not contained in integration head %s: %v", head, integrated, err)
	}
	if _, err := os.Stat(filepath.Join(S(collection, "integration_worktree"), "feature.txt")); err != nil {
		t.Fatalf("integrated file missing: %v", err)
	}
}

func TestReadOnlySnapshotDetectsCheckoutAwayAndBack(t *testing.T) {
	_, workspace, _, _, _, path, baseHead := prepareWorkspaceTask(t, "review")
	baseline, err := workspace.WorkspaceSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	gitTest(t, path, "checkout", "--detach", "HEAD^")
	gitTest(t, path, "checkout", "--detach", baseHead)
	if err := workspace.ValidateReadOnlyWorkspace(path, baseline); err == nil || !strings.Contains(err.Error(), "HEAD history changed") {
		t.Fatalf("transient checkout must violate read-only identity, got %v", err)
	}
	if err := workspace.RestoreReadOnlyWorkspace(path, baseline); err != nil {
		t.Fatal(err)
	}
	state, err := workspace.WorkspaceSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	if state.Head != baseline.Head || state.Branch != "" || state.Status != "" {
		t.Fatalf("read-only restore did not return to detached clean baseline: %#v", state)
	}
}
func TestAcceptancePlanAndFastForward(t *testing.T) {
	repo := t.TempDir()
	git, e := RequireBinary("git")
	if e != nil {
		t.Skip("git unavailable")
	}
	if _, e = Run(context.Background(), []string{git, "init", "-b", "main", repo}, "", nil, "", true); e != nil {
		t.Fatal(e)
	}
	gitTest(t, repo, "config", "user.email", "praector@test")
	gitTest(t, repo, "config", "user.name", "Praector Test")
	if e = os.WriteFile(filepath.Join(repo, "README.md"), []byte("base\n"), 0644); e != nil {
		t.Fatal(e)
	}
	gitTest(t, repo, "add", ".")
	gitTest(t, repo, "commit", "-m", "base")
	gitTest(t, repo, "branch", "praector/collections/col-1")
	gitTest(t, repo, "checkout", "praector/collections/col-1")
	if e = os.WriteFile(filepath.Join(repo, "feature.txt"), []byte("feature\n"), 0644); e != nil {
		t.Fatal(e)
	}
	gitTest(t, repo, "add", ".")
	gitTest(t, repo, "commit", "-m", "feature")
	gitTest(t, repo, "checkout", "main")
	cfg := GlobalConfig{Home: t.TempDir()}
	w := &GitWorkspace{Cfg: cfg, Git: git}
	project := Row{"id": "PRJ-1", "root_path": repo, "base_branch": "main"}
	collection := Row{"id": "COL-1", "title": "Feature", "integration_branch": "praector/collections/col-1"}
	plan, e := w.AcceptancePlan(project, collection, "main")
	if e != nil {
		t.Fatal(e)
	}
	if !B(plan, "fast_forward") || B(plan, "already_contained") {
		t.Fatalf("unexpected plan: %v", plan)
	}
	m, e := w.AcceptCollection(project, collection, "main", false)
	if e != nil {
		t.Fatal(e)
	}
	if S(m, "merge_mode") != "ff-only" {
		t.Fatalf("merge: %v", m)
	}
	if !strings.Contains(gitTest(t, repo, "log", "-1", "--pretty=%s"), "feature") {
		t.Fatal("target not advanced")
	}
}

func twoBranchFixture(t *testing.T) (string, *GitWorkspace, Row, Row) {
	t.Helper()
	git, err := RequireBinary("git")
	if err != nil {
		t.Skip("git unavailable")
	}
	repo := t.TempDir()
	if _, err = Run(context.Background(), []string{git, "init", "-b", "main", repo}, "", nil, "", true); err != nil {
		t.Fatal(err)
	}
	gitTest(t, repo, "config", "user.email", "praector@test")
	gitTest(t, repo, "config", "user.name", "Praector Test")
	if err = os.WriteFile(filepath.Join(repo, "base.txt"), []byte("base\n"), 0644); err != nil {
		t.Fatal(err)
	}
	gitTest(t, repo, "add", ".")
	gitTest(t, repo, "commit", "-m", "base")
	gitTest(t, repo, "checkout", "-b", "praector/collections/col-derive")
	if err = os.WriteFile(filepath.Join(repo, "cmd.go"), []byte("package main\n\nvar settingFlag = \"--derive-test-knob\"\nvar settingEnv = \"DERIVE_TEST_SETTING\"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	gitTest(t, repo, "add", ".")
	gitTest(t, repo, "commit", "-m", "candidate")
	gitTest(t, repo, "checkout", "main")
	return repo, &GitWorkspace{Cfg: GlobalConfig{Home: t.TempDir()}, Git: git},
		Row{"id": "PRJ-derive", "root_path": repo, "base_branch": "main"},
		Row{"id": "COL-derive", "integration_branch": "praector/collections/col-derive"}
}

func TestDeriveConfigCandidatesIsDeterministicAndFailsLoudly(t *testing.T) {
	repo, w, project, collection := twoBranchFixture(t)
	first, err := w.DeriveConfigCandidates(project, collection)
	if err != nil {
		t.Fatal(err)
	}
	second, err := w.DeriveConfigCandidates(project, collection)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) == 0 || JSONDumps(first) != JSONDumps(second) {
		t.Fatalf("candidate derivation must yield an identical sorted list across invocations: %v vs %v", first, second)
	}
	tokens := map[string]bool{}
	for _, candidate := range first {
		tokens[candidate.Token] = true
	}
	if !tokens["--derive-test-knob"] || !tokens["DERIVE_TEST_SETTING"] {
		t.Fatalf("added flag and env-var literals must be enumerated: %v", first)
	}
	// The three-dot basis must not count base-branch-side changes: an added
	// token that exists only on main must never appear.
	gitTest(t, repo, "checkout", "main")
	if err = os.WriteFile(filepath.Join(repo, "mainonly.go"), []byte("package main\n\nvar flag = \"--base-side-only\"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	gitTest(t, repo, "add", ".")
	gitTest(t, repo, "commit", "-m", "base side change")
	gitTest(t, repo, "checkout", "praector/collections/col-derive")
	gitTest(t, repo, "checkout", "main")
	after, err := w.DeriveConfigCandidates(project, collection)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range after {
		if candidate.Token == "--base-side-only" {
			t.Fatalf("a base-branch-only token must not be derived as introduced (two-dot drift): %v", after)
		}
	}
	// An absent diff basis (a missing branch, or a non-repository root) is an
	// explicitly surfaced basis-unavailable outcome, not a silent empty
	// candidate list, so it cannot be mistaken for a genuine no-knob diff; a
	// git error against an existing basis still fails loudly.
	missingBranch := Row{"id": "COL-missing", "integration_branch": "praector/collections/col-missing"}
	none, err := w.DeriveConfigCandidatesWithBasis(project, missingBranch)
	if err != nil || len(none.Candidates) != 0 || none.BasisUnavailable == "" {
		t.Fatalf("an absent integration branch must be a named basis-unavailable outcome with no candidates: %#v %v", none, err)
	}
	if !strings.Contains(none.BasisUnavailable, "praector/collections/col-missing") {
		t.Fatalf("the absent-basis outcome must name the missing ref: %#v", none)
	}
	if _, err = w.ChangedPaths(filepath.Join(t.TempDir(), "not-a-repo"), "main", "main"); err == nil {
		t.Fatal("ChangedPaths must fail loudly on a git error, never return an empty list silently")
	}
	broken := Row{"id": "PRJ-broken", "root_path": filepath.Join(t.TempDir(), "not-a-repo"), "base_branch": "main"}
	quiet, err := w.DeriveConfigCandidatesWithBasis(broken, collection)
	if err != nil || len(quiet.Candidates) != 0 || quiet.BasisUnavailable == "" {
		t.Fatalf("a non-repository root must be a named basis-unavailable outcome, not a git error: %#v %v", quiet, err)
	}
	// A derivable basis with a real diff is NOT basis-unavailable, so the new
	// visible state stays distinct from a genuine candidate-bearing diff.
	present, err := w.DeriveConfigCandidatesWithBasis(project, collection)
	if err != nil || present.BasisUnavailable != "" {
		t.Fatalf("a derivable diff basis must not report a basis-unavailable outcome: %#v %v", present, err)
	}
}

func TestChangedPathsUsesNameOnlyZDelimitedBasis(t *testing.T) {
	repo, w, project, collection := twoBranchFixture(t)
	weird := "spaced dir/with\"quote--knob.go"
	gitTest(t, repo, "checkout", "praector/collections/col-derive")
	if err := os.MkdirAll(filepath.Join(repo, "spaced dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "spaced dir", "with\"quote--knob.go"), []byte("package x\n\nvar flag = \"--space-derivable-knob\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitTest(t, repo, "add", ".")
	gitTest(t, repo, "commit", "-m", "weird path")
	gitTest(t, repo, "checkout", "main")
	_ = project
	paths, err := w.ChangedPaths(repo, S(project, "base_branch"), S(collection, "integration_branch"))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, path := range paths {
		if path == weird {
			found = true
		}
	}
	if !found {
		t.Fatalf("core.quotePath must not drop a quoted/spaced path from the -z listing: %v", paths)
	}
}

func TestTaskGitCommitMessageUsesHumanSubjectAndPraectorTrailers(t *testing.T) {
	task := Row{
		"id":            "T-c5a2d04929",
		"collection_id": "COL-21bd3e5582",
		"title":         "Add authenticated GET /admin/secure-ping endpoint",
		"objective":     "Implement the endpoint",
		"result": Row{
			"summary": "Added the authenticated secure-ping endpoint with regression coverage.",
		},
	}
	checkpoint := taskGitCommitMessage(task, "attempt-checkpoint", 2, "")
	lines := strings.Split(checkpoint, "\n")
	if lines[0] != "Add authenticated GET /admin/secure-ping endpoint" {
		t.Fatalf("unexpected checkpoint subject: %q", lines[0])
	}
	for _, want := range []string{
		"Praector-Collection: COL-21bd3e5582",
		"Praector-Task: T-c5a2d04929",
		"Praector-Attempt: 2",
		"Praector-Operation: attempt-checkpoint",
	} {
		if !strings.Contains(checkpoint, want) {
			t.Fatalf("checkpoint missing %q: %s", want, checkpoint)
		}
	}
	integration := taskGitCommitMessage(task, "integration", 0, "Integrate")
	if got := strings.Split(integration, "\n")[0]; got != "Integrate: Add authenticated GET /admin/secure-ping endpoint" {
		t.Fatalf("unexpected integration subject: %q", got)
	}
	if !strings.Contains(integration, "Added the authenticated secure-ping endpoint with regression coverage.") {
		t.Fatalf("integration message should retain result summary: %s", integration)
	}
}
