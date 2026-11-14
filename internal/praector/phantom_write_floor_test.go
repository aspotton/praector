package praector

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hasEvent reports whether a durable event of eventType for task tid exists in the
// collection event log (task+attempt-scoped audit surface: ListEvents / praector
// events).
func hasEvent(t *testing.T, s *Store, cid, tid, eventType string) bool {
	t.Helper()
	events, err := s.EventsForCollection(cid, 500)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if S(ev, "event_type") == eventType && S(ev, "task_id") == tid {
			return true
		}
	}
	return false
}

func runningImplement(t *testing.T, s *Store, pcfg ProjectConfig, maxAttempts int) (Row, Row) {
	t.Helper()
	pcfgCopy := pcfg
	pcfgCopy.Limits.MaxAttemptsPerTask = maxAttempts
	p, err := s.RegisterProject(pcfgCopy)
	if err != nil {
		t.Fatal(err)
	}
	c, coord, err := s.CreateCollectionWithCoordinator(S(p, "id"), "Feature", "Add endpoint and change code", "Plan")
	if err != nil {
		t.Fatal(err)
	}
	impl, _, err := s.CreateTask(CreateTaskOptions{
		ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "implement", Title: "Implement", Objective: "Do work",
		Reason: "plan", ParentTaskID: S(coord, "id"), RequestedByTaskID: S(coord, "id"), WorkspaceMode: "collection", Depth: 1, AllowDedupe: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetTaskStatus(S(impl, "id"), TaskRunning, "test running"); err != nil {
		t.Fatal(err)
	}
	return impl, c
}

// TestPhantomImplementationRetriesSameTaskBeforeBudget: an implement attempt that
// exited 0 with a clean success, no structured result, and zero new commits is a
// phantom, not a completion — the task returns to the SAME task as READY with a
// dedicated IMPLEMENT_PHANTOM_COMPLETION_REJECTED event carrying a tail of the final
// text, and creates no coordinator-attention work (which only fires on a terminal
// FAILED). This is the sibling-handler contract mirrored from the read-only floor.
func TestPhantomImplementationRetriesSameTaskBeforeBudget(t *testing.T) {
	s, cfg := newFakeStore(t)
	impl, c := runningImplement(t, s, testProjectConfig(t.TempDir()), 3)
	sched, err := NewScheduler(s, cfg, 1)
	if err != nil {
		t.Fatal(err)
	}
	msg := sched.handlePhantomImplementation(testProjectConfig(t.TempDir()), impl, Row{"id": "A-1", "attempt_no": 1}, "transcript claims it was done but wrote nothing")
	if msg == "" {
		t.Fatal("expected protocol failure message")
	}
	got, err := s.GetTask(S(impl, "id"))
	if err != nil {
		t.Fatal(err)
	}
	if S(got, "status") != TaskReady {
		t.Fatalf("expected same implement task READY for retry, got %s", S(got, "status"))
	}
	if !hasEvent(t, s, S(c, "id"), S(impl, "id"), "IMPLEMENT_PHANTOM_COMPLETION_REJECTED") {
		t.Fatal("missing IMPLEMENT_PHANTOM_COMPLETION_REJECTED event")
	}
	tasks, _ := s.ListTasks(S(c, "id"))
	for _, task := range tasks {
		if S(task, "profile") == "coordinator" && S(task, "title") == "Coordinator attention" {
			t.Fatalf("phantom retry must not create coordinator attention: %v", task)
		}
	}
}

// TestPhantomImplementationFailsAfterRetryBudget: on attempt-budget exhaustion the
// phantom attempt submits a failed result (the shared ExecuteTask coordinator-attention
// tail, guarded on final status FAILED && profile != coordinator, then fires). The
// task is FAILED with an exhausted-budget risk.
func TestPhantomImplementationFailsAfterRetryBudget(t *testing.T) {
	s, cfg := newFakeStore(t)
	impl, _ := runningImplement(t, s, testProjectConfig(t.TempDir()), 3)
	sched, err := NewScheduler(s, cfg, 1)
	if err != nil {
		t.Fatal(err)
	}
	sched.handlePhantomImplementation(testProjectConfig(t.TempDir()), impl, Row{"id": "A-3", "attempt_no": 3}, "silent zero-write completion")
	got, err := s.GetTask(S(impl, "id"))
	if err != nil {
		t.Fatal(err)
	}
	if S(got, "status") != TaskFailed {
		t.Fatalf("expected FAILED after phantom retry budget, got %s", S(got, "status"))
	}
	if !strings.Contains(JSONDumps(M(got, "result")), "retry budget exhausted") {
		t.Fatalf("expected exhausted-budget risk, got %v", M(got, "result"))
	}
}

// TestStructuredZeroCommitImplementIsNotPhantomFlagged documents and proves the
// honored exemption: a structured completion is never mechanically phantom-flagged,
// even with zero commits. The mechanical floor lives only inside the no-structured-
// result fallback, which is reachable only while the task is still RUNNING. A worker
// that submits a structured result vacates RUNNING, so the completion lands on the
// normal integration path (VERIFYING), never the phantom READY-retry path.
func TestStructuredZeroCommitImplementIsNotPhantomFlagged(t *testing.T) {
	s, _ := newFakeStore(t)
	impl, c := runningImplement(t, s, testProjectConfig(t.TempDir()), 3)
	if _, err := s.SubmitResult(S(impl, "id"), Row{"status": "complete", "summary": "structured completion with zero commits", "findings": []any{}, "evidence": []any{}}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetTask(S(impl, "id"))
	if err != nil {
		t.Fatal(err)
	}
	if S(got, "status") == TaskReady {
		t.Fatal("a structured completion must never be phantom-flagged into a READY retry")
	}
	if hasEvent(t, s, S(c, "id"), S(impl, "id"), "IMPLEMENT_PHANTOM_COMPLETION_REJECTED") {
		t.Fatal("structured completion must not emit a phantom-rejection event")
	}
}

// TestFallbackTextCompletionMarksResultDurably proves the legitimate exit-0 auto-
// complete path (submitFallbackTextCompletion) stores result_json.fallback_text_result
// = true and emits an audit-level RESULT_FALLBACK_TEXT event, while a worker-submitted
// structured result carries neither marker.
func TestFallbackTextCompletionMarksResultDurably(t *testing.T) {
	s, cfg := newFakeStore(t)
	fallback, c := runningImplement(t, s, testProjectConfig(t.TempDir()), 3)
	sched, err := NewScheduler(s, cfg, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, e := sched.submitFallbackTextCompletion(fallback, Row{"id": "A-1"}, "OpenCode finished without a structured result"); e != nil {
		t.Fatal(e)
	}
	got, err := s.GetTask(S(fallback, "id"))
	if err != nil {
		t.Fatal(err)
	}
	if M(got, "result")["fallback_text_result"] != true {
		t.Fatalf("expected durable fallback_text_result=true, got %v", M(got, "result"))
	}
	if !hasEvent(t, s, S(c, "id"), S(fallback, "id"), "RESULT_FALLBACK_TEXT") {
		t.Fatal("missing RESULT_FALLBACK_TEXT event")
	}

	// A worker-submitted structured result carries neither the marker nor the event.
	structured, _, err := s.CreateTask(CreateTaskOptions{
		ProjectID: S(getProjectOf(t, s, c), "id"), CollectionID: S(c, "id"), Profile: "explore", Title: "Explore", Objective: "Map",
		Reason: "plan", WorkspaceMode: "collection", Depth: 1, AllowDedupe: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, e := s.SubmitResult(S(structured, "id"), Row{"status": "complete", "summary": "structured", "findings": []any{}}); e != nil {
		t.Fatal(e)
	}
	gotStructured, err := s.GetTask(S(structured, "id"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := M(gotStructured, "result")["fallback_text_result"]; ok {
		t.Fatalf("a structured result must not carry the fallback marker: %v", M(gotStructured, "result"))
	}
	if hasEvent(t, s, S(c, "id"), S(structured, "id"), "RESULT_FALLBACK_TEXT") {
		t.Fatal("a structured result must not emit RESULT_FALLBACK_TEXT")
	}
}

func getProjectOf(t *testing.T, s *Store, c Row) Row {
	t.Helper()
	p, err := s.GetProject(S(c, "project_id"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestNewCommitsFromAndResetAreCommitReachability drives the real Workspace seam:
// PrepareTask records the fresh collection-mode implement's durable base_commit; the
// probe is commit reachability (a dirty-but-zero-commit worktree is still zero), and
// the phantom reset returns the attached branch to base AND removes untracked leftovers.
func TestNewCommitsFromAndResetAreCommitReachability(t *testing.T) {
	_, workspace, _, _, task, path, baseHead := prepareWorkspaceTask(t, "implement")

	// PrepareTask now records the fork commit as the durable base for collection mode.
	if got := S(task, "base_commit"); got != baseHead {
		t.Fatalf("PrepareTask must record base_commit at fresh creation: got %q want %q", got, baseHead)
	}

	hasNew, err := workspace.NewCommitsFrom(path, S(task, "base_commit"))
	if err != nil || hasNew {
		t.Fatalf("a fresh implement worktree has zero new commits from base: hasNew=%v err=%v", hasNew, err)
	}

	// A dirty-but-zero-commit attempt (an untracked leftover, no commit) is still the
	// phantom case: commit reachability, not a working-tree diff.
	if err := os.WriteFile(filepath.Join(path, "phantom-leftover.txt"), []byte("transient\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if dirty := gitTest(t, path, "status", "--porcelain", "--untracked-files=all"); dirty == "" {
		t.Fatal("expected an untracked leftover before reset")
	}
	if hasNew, err = workspace.NewCommitsFrom(path, S(task, "base_commit")); err != nil || hasNew {
		t.Fatalf("dirty-but-zero-commit worktree must not read as having new commits: hasNew=%v err=%v", hasNew, err)
	}

	// The phantom reset returns the branch to base AND removes untracked leftovers.
	if err := workspace.ResetImplementTaskToBase(task); err != nil {
		t.Fatal(err)
	}
	if status := gitTest(t, path, "status", "--porcelain", "--untracked-files=all"); status != "" {
		t.Fatalf("reset must clear tracked edits and untracked files, got status %q", status)
	}
	if _, err := os.Stat(filepath.Join(path, "phantom-leftover.txt")); !os.IsNotExist(err) {
		t.Fatalf("reset must remove untracked leftovers via clean -fd: stat err=%v", err)
	}
	if head := gitTest(t, path, "rev-parse", "HEAD"); head != baseHead {
		t.Fatalf("reset must return the attached branch to base_commit: got %q want %q", head, baseHead)
	}

	// Once a real commit exists, the probe reports new commits (no longer phantom).
	if err := os.WriteFile(filepath.Join(path, "real.txt"), []byte("work\n"), 0644); err != nil {
		t.Fatal(err)
	}
	gitTest(t, path, "add", "-A")
	gitTest(t, path, "-c", "user.name=praector", "-c", "user.email=praector@local", "commit", "-m", "real work")
	if hasNew, err = workspace.NewCommitsFrom(path, S(task, "base_commit")); err != nil || !hasNew {
		t.Fatalf("a committed worktree must report new commits: hasNew=%v err=%v", hasNew, err)
	}
}
