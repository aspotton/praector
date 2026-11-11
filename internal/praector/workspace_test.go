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
