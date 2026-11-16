package praector

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This file covers the operator/scheduler hygiene fixes from the lemmings retro:
// objective hygiene (fix 6), the depth-budget accounting (fix 7), and repair
// seeding (fix 8). Each test drives the durable behavior, not just a helper.

// withStdin swaps os.Stdin for a temp file carrying the given bytes and restores it.
func withStdin(t *testing.T, content string) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	prev := os.Stdin
	os.Stdin = f
	t.Cleanup(func() {
		os.Stdin = prev
		f.Close()
	})
}

// Fix 6: --objective-file / stdin objectives are stored byte-exactly (including
// backticks, $(), newlines, and trailing whitespace), exactly one source is
// required, and a missing/unreadable source fails closed without producing text.
func TestTaskRequestObjectiveSourceByteExact(t *testing.T) {
	const tricky = "build `date` and $(whoami)\nmulti-line objective   "
	dir := t.TempDir()
	path := filepath.Join(dir, "obj.txt")
	if err := os.WriteFile(path, []byte(tricky), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		args   []string
		stdin  string
		useStd bool
		want   string
		fails  bool
	}{
		{name: "flag", args: []string{"--objective", tricky}, want: tricky},
		{name: "file", args: []string{"--objective-file", path}, want: tricky},
		{name: "stdin-flag", args: []string{"--objective-file", "-"}, stdin: tricky, useStd: true, want: tricky},
		{name: "stdin-lone-dash", args: []string{"-"}, stdin: tricky, useStd: true, want: tricky},
		{name: "two-sources", args: []string{"--objective", "x", "--objective-file", path}, fails: true},
		{name: "flag-plus-dash", args: []string{"--objective", "x", "-"}, stdin: "y", useStd: true, fails: true},
		{name: "none", args: []string{}, fails: true},
		{name: "missing-file", args: []string{"--objective-file", filepath.Join(dir, "nope")}, fails: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.useStd {
				withStdin(t, tc.stdin)
			}
			p, err := parseArgs(append([]string{"--profile", "implement", "--title", "T", "--reason", "R"}, tc.args...),
				map[string]bool{"task": true, "profile": true, "title": true, "objective": true, "objective-file": true, "reason": true, "blocking": false, "depends-on": true, "workspace": true})
			if err != nil {
				t.Fatalf("parseArgs: %v", err)
			}
			got, err := resolveTaskObjective(p)
			if tc.fails {
				if err == nil {
					t.Fatalf("expected fail-closed, got objective %q", got)
				}
				if got != "" {
					t.Fatalf("failed source must yield no text, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveTaskObjective: %v", err)
			}
			if got != tc.want {
				t.Fatalf("objective not byte-exact:\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}

// Fix 7: the request-seam depth arithmetic has no off-by-one and the scaffolding
// entry point is deliberately excluded from the budget, so a legitimate repair
// created late (via CreateTask/supersede depth) stays creatable even past the
// request-seam ceiling.
func TestDepthBudgetRequestSeamOnly(t *testing.T) {
	// requestSeamTaskDepth is caller depth + 1 (the single definition the two
	// request-seam checks share).
	if requestSeamTaskDepth(4) != 5 || requestSeamTaskDepth(0) != 1 {
		t.Fatalf("requestSeamTaskDepth semantics drift: %d %d", requestSeamTaskDepth(4), requestSeamTaskDepth(0))
	}

	s, _ := newFakeStore(t)
	p, err := s.RegisterProject(testProjectConfig(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	c, coord, err := s.CreateCollectionWithCoordinator(S(p, "id"), "depth budget", "obj", "plan")
	if err != nil {
		t.Fatal(err)
	}
	limits := DefaultLimits()
	limits.MaxTaskDepth = 3
	limits.MaxTasksPerCollection = 1000
	limits.MaxChildrenPerTask = 1000

	// A caller at depth == MaxTaskDepth must have its request-seam child BLOCKED
	// (depth == Max+1 > Max): the ceiling is enforced at the request seam with no
	// off-by-one (a child at exactly Max would be allowed).
	deep, _, err := s.CreateTask(CreateTaskOptions{ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "implement", Title: "deep", Objective: "o", Reason: "r", ParentTaskID: S(coord, "id"), WorkspaceMode: "collection", Depth: limits.MaxTaskDepth, AllowDedupe: false})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RequestTask(S(deep, "id"), RequestTaskOptions{Profile: "implement", Title: "too deep", Objective: "o", Reason: "r", WorkspaceMode: "collection", Limits: limits}); err == nil || !strings.Contains(err.Error(), "depth") {
		t.Fatalf("request-seam child of a max-depth caller must be blocked by the depth budget, got %v", err)
	}
	// A caller at depth == Max-1 must succeed (child depth == Max is allowed).
	ok, _, err := s.CreateTask(CreateTaskOptions{ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "implement", Title: "ok", Objective: "o", Reason: "r", ParentTaskID: S(coord, "id"), WorkspaceMode: "collection", Depth: limits.MaxTaskDepth - 1, AllowDedupe: false})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RequestTask(S(ok, "id"), RequestTaskOptions{Profile: "implement", Title: "fits", Objective: "o", Reason: "r", WorkspaceMode: "collection", Limits: limits}); err != nil {
		t.Fatalf("request-seam child at depth == Max must be creatable: %v", err)
	}

	// Scaffolding/repair replacement via CreateTask with an explicit high Depth is
	// NOT budget-checked, so a late repair stays creatable past the request-seam
	// ceiling (the incident's blocked supersede path).
	if _, _, err := s.CreateTask(CreateTaskOptions{ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "implement", Title: "late repair", Objective: "o", Reason: "r", ParentTaskID: S(deep, "id"), WorkspaceMode: "collection", Depth: limits.MaxTaskDepth + 6, AllowDedupe: false}); err != nil {
		t.Fatalf("scheduler-staged scaffolding/repair must be excluded from the depth budget: %v", err)
	}
}

// Fix 8: handleFailedReview seeds the auto-created repair objective with the
// failed review's findings/blockers verbatim, deterministically bounded to the
// top 10 with an omission marker, instead of the bare summary text.
func TestHandleFailedReviewSeedsBoundedFindings(t *testing.T) {
	s, _ := newFakeStore(t)
	p, err := s.RegisterProject(testProjectConfig(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	c, coord, err := s.CreateCollectionWithCoordinator(S(p, "id"), "seeding", "obj", "plan")
	if err != nil {
		t.Fatal(err)
	}
	impl, _, err := s.CreateTask(CreateTaskOptions{ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "implement", Title: "impl", Objective: "o", Reason: "r", ParentTaskID: S(coord, "id"), WorkspaceMode: "collection", Depth: 1, AllowDedupe: false})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetTaskStatus(S(impl, "id"), TaskComplete, "impl complete"); err != nil {
		t.Fatal(err)
	}
	review, _, err := s.CreateTask(CreateTaskOptions{ProjectID: S(p, "id"), CollectionID: S(c, "id"), Profile: "review", Title: "review", Objective: "r", Reason: "policy", ParentTaskID: S(coord, "id"), DependsOn: []string{S(impl, "id")}, WorkspaceMode: "collection", Depth: 2, AllowDedupe: false})
	if err != nil {
		t.Fatal(err)
	}
	findings := make([]any, 0, 12)
	for i := 1; i <= 12; i++ {
		findings = append(findings, fmt.Sprintf("finding-%02d: concrete defect %d", i, i))
	}
	if _, err := s.SubmitResult(S(review, "id"), Row{"status": "complete", "summary": "many defects", "verdict": "fail", "findings": findings, "blockers": []any{"blocker-alpha must fail closed"}}); err != nil {
		t.Fatal(err)
	}
	sch := &Scheduler{Store: s}
	if err := sch.handleFailedReview(p, review); err != nil {
		t.Fatal(err)
	}
	tasks, _ := s.ListTasks(S(c, "id"))
	var repair Row
	for _, task := range tasks {
		if S(task, "profile") == "implement" && S(task, "parent_task_id") == S(review, "id") {
			repair = task
		}
	}
	if repair == nil {
		t.Fatal("failed review did not create a repair implementation")
	}
	obj := S(repair, "objective")
	if !strings.Contains(obj, "finding-01: concrete defect 1") {
		t.Fatalf("repair objective must embed findings verbatim; got:\n%s", obj)
	}
	if !strings.Contains(obj, "blocker-alpha must fail closed") {
		t.Fatalf("repair objective must embed blockers verbatim; got:\n%s", obj)
	}
	// Bound: blockers first then findings, deduped; the top-10 keeps finding-09
	// and omits finding-10 onward with an explicit omission marker.
	if !strings.Contains(obj, "finding-09:") {
		t.Fatalf("top-10 bound should keep finding-09; got:\n%s", obj)
	}
	if strings.Contains(obj, "finding-10:") {
		t.Fatalf("top-10 bound must omit finding-10 (after the blocker + 9 findings); got:\n%s", obj)
	}
	if !strings.Contains(obj, "omitted by the repair-seeding cap") {
		t.Fatalf("omission marker must name the omitted findings; got:\n%s", obj)
	}
}
