package praector

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type GitWorkspace struct {
	Store *Store
	Cfg   GlobalConfig
	Git   string
}

func NewGitWorkspace(store *Store, cfg GlobalConfig) (*GitWorkspace, error) {
	git, err := RequireBinary("git")
	if err != nil {
		return nil, err
	}
	return &GitWorkspace{Store: store, Cfg: cfg, Git: git}, nil
}

func (w *GitWorkspace) git(repo string, check bool, args ...string) (CmdResult, error) {
	cmd := append([]string{w.Git, "-C", repo}, args...)
	return Run(context.Background(), cmd, "", nil, "", check)
}

func (w *GitWorkspace) RepoRoot(project Row) string {
	root := S(project, "root_path")
	if abs, err := filepath.Abs(root); err == nil {
		return filepath.Clean(abs)
	}
	return filepath.Clean(root)
}

func dirNonEmpty(path string) bool {
	entries, err := os.ReadDir(path)
	return err == nil && len(entries) > 0
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func strptr(v string) *string { return &v }

func (w *GitWorkspace) EnsureCollection(project, collection Row) (string, error) {
	repo := w.RepoRoot(project)
	branch := S(collection, "integration_branch")
	path := filepath.Join(w.Cfg.WorktreesDir(), S(project, "id"), S(collection, "id"), "integration")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return "", err
	}
	branchExists, err := w.BranchExists(repo, branch)
	if err != nil {
		return "", err
	}
	if !branchExists {
		baseBranch := S(project, "base_branch")
		p, err := w.git(repo, true, "rev-parse", baseBranch)
		if err != nil {
			return "", err
		}
		base := strings.TrimSpace(p.Stdout)
		if _, err = w.git(repo, true, "branch", branch, base); err != nil {
			return "", err
		}
	}
	if !exists(filepath.Join(path, ".git")) {
		if exists(path) && dirNonEmpty(path) {
			return "", fmt.Errorf("Collection worktree path is not empty: %s", path)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return "", err
		}
		if _, err := w.git(repo, true, "worktree", "add", path, branch); err != nil {
			return "", err
		}
	}
	if err := w.Store.UpdateCollectionWorkspace(S(collection, "id"), path); err != nil {
		return "", err
	}
	return path, nil
}

func (w *GitWorkspace) taskPath(project, collection, task Row) string {
	return filepath.Join(w.Cfg.WorktreesDir(), S(project, "id"), S(collection, "id"), S(task, "id"))
}

func (w *GitWorkspace) PrepareTask(project, collection, task Row) (string, error) {
	repo := w.RepoRoot(project)
	if _, err := w.EnsureCollection(project, collection); err != nil {
		return "", err
	}
	taskPath := w.taskPath(project, collection, task)
	if err := os.MkdirAll(filepath.Dir(taskPath), 0755); err != nil {
		return "", err
	}
	baseRef := S(task, "base_commit")
	if baseRef == "" {
		if S(task, "workspace_mode") == "isolated" {
			baseRef = S(project, "base_branch")
		} else {
			baseRef = S(collection, "integration_branch")
		}
	}

	if ReadOnlyProfiles[S(task, "profile")] {
		if !exists(filepath.Join(taskPath, ".git")) {
			if exists(taskPath) && dirNonEmpty(taskPath) {
				return "", fmt.Errorf("Task worktree path is not empty: %s", taskPath)
			}
			if _, err := w.git(repo, true, "worktree", "add", "--detach", taskPath, baseRef); err != nil {
				return "", err
			}
		} else {
			if _, err := w.git(taskPath, true, "reset", "--hard", baseRef); err != nil {
				return "", err
			}
			if _, err := w.git(taskPath, true, "clean", "-fd"); err != nil {
				return "", err
			}
		}
		h, err := w.git(taskPath, true, "rev-parse", "HEAD")
		if err != nil {
			return "", err
		}
		head := strings.TrimSpace(h.Stdout)
		var base *string
		if S(task, "base_commit") != "" {
			v := S(task, "base_commit")
			base = &v
		}
		if err := w.Store.SetTaskWorkspace(S(task, "id"), strptr(taskPath), nil, base, strptr(head)); err != nil {
			return "", err
		}
		return taskPath, nil
	}

	branch := S(task, "branch_name")
	if branch == "" {
		branch = fmt.Sprintf("praector/tasks/%s/%s", strings.ToLower(S(collection, "id")), strings.ToLower(S(task, "id")))
	}
	branchExists, err := w.BranchExists(repo, branch)
	if err != nil {
		return "", err
	}
	if !exists(filepath.Join(taskPath, ".git")) {
		if exists(taskPath) && dirNonEmpty(taskPath) {
			return "", fmt.Errorf("Task worktree path is not empty: %s", taskPath)
		}
		if branchExists {
			if _, err = w.git(repo, true, "worktree", "add", taskPath, branch); err != nil {
				return "", err
			}
		} else if _, err = w.git(repo, true, "worktree", "add", "-b", branch, taskPath, baseRef); err != nil {
			return "", err
		}
	}
	h, err := w.git(taskPath, true, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	head := strings.TrimSpace(h.Stdout)
	var base *string
	if S(task, "base_commit") != "" {
		v := S(task, "base_commit")
		base = &v
	}
	if err := w.Store.SetTaskWorkspace(S(task, "id"), strptr(taskPath), strptr(branch), base, strptr(head)); err != nil {
		return "", err
	}
	return taskPath, nil
}

func taskGitTitle(task Row) string {
	title := compactText(S(task, "title"), 100)
	if title == "" {
		title = compactText(S(task, "objective"), 100)
	}
	if title == "" {
		title = "Praector task " + S(task, "id")
	}
	return title
}

func taskGitCommitMessage(task Row, operation string, attemptNo int, subjectPrefix string) string {
	subject := taskGitTitle(task)
	if subjectPrefix != "" {
		subject = subjectPrefix + ": " + subject
	}
	lines := []string{subject}
	result := M(task, "result")
	summary := compactText(firstNonEmpty(S(result, "pr_summary"), S(result, "summary")), 700)
	if summary != "" && !strings.EqualFold(summary, taskGitTitle(task)) {
		lines = append(lines, "", summary)
	}
	lines = append(lines, "", "Praector-Collection: "+S(task, "collection_id"), "Praector-Task: "+S(task, "id"))
	if attemptNo > 0 {
		lines = append(lines, fmt.Sprintf("Praector-Attempt: %d", attemptNo))
	}
	if operation != "" {
		lines = append(lines, "Praector-Operation: "+operation)
	}
	return strings.Join(lines, "\n")
}

func (w *GitWorkspace) IsDirty(worktree string) (bool, error) {
	p, err := w.git(worktree, true, "status", "--porcelain")
	return strings.TrimSpace(p.Stdout) != "", err
}

func (w *GitWorkspace) Checkpoint(task Row, attemptNo int, operation string) (string, error) {
	path := S(task, "worktree_path")
	if path == "" || !exists(path) {
		return firstNonEmpty(S(task, "current_head"), S(task, "base_commit")), nil
	}
	if ReadOnlyProfiles[S(task, "profile")] {
		p, err := w.git(path, true, "rev-parse", "HEAD")
		return strings.TrimSpace(p.Stdout), err
	}
	dirty, err := w.IsDirty(path)
	if err != nil {
		return "", err
	}
	if dirty {
		if _, err = w.git(path, true, "add", "-A"); err != nil {
			return "", err
		}
		message := taskGitCommitMessage(task, operation, attemptNo, "")
		if _, err = w.git(path, true, "-c", "user.name=praector", "-c", "user.email=praector@local", "commit", "-m", message); err != nil {
			return "", err
		}
	}
	p, err := w.git(path, true, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	head := strings.TrimSpace(p.Stdout)
	if err := w.Store.SetTaskWorkspace(S(task, "id"), nil, nil, nil, strptr(head)); err != nil {
		return "", err
	}
	return head, nil
}

func (w *GitWorkspace) RollbackReadOnlyChanges(task Row) error {
	path := S(task, "worktree_path")
	if path == "" || !exists(path) {
		return nil
	}
	if _, err := w.git(path, true, "reset", "--hard", "HEAD"); err != nil {
		return err
	}
	_, err := w.git(path, true, "clean", "-fd")
	return err
}

func (w *GitWorkspace) IntegrateTask(project, collection, task Row) (string, error) {
	if ReadOnlyProfiles[S(task, "profile")] {
		integration, err := w.EnsureCollection(project, collection)
		if err != nil {
			return "", err
		}
		p, err := w.git(integration, true, "rev-parse", "HEAD")
		return strings.TrimSpace(p.Stdout), err
	}
	branch := S(task, "branch_name")
	if branch == "" {
		return "", fmt.Errorf("Task %s has no branch to integrate", S(task, "id"))
	}
	target, label := "", ""
	parentID := S(task, "parent_task_id")
	if S(task, "workspace_mode") == "inherit" && parentID != "" {
		parent, err := w.Store.GetTask(parentID)
		if err != nil {
			return "", err
		}
		if S(parent, "profile") == "implement" && S(parent, "worktree_path") != "" {
			target, label = S(parent, "worktree_path"), parentID
		}
	}
	if target == "" {
		var err error
		target, err = w.EnsureCollection(project, collection)
		if err != nil {
			return "", err
		}
		label = S(collection, "id")
	}
	message := taskGitCommitMessage(task, "integration", 0, "Integrate")
	p, err := w.git(target, false, "-c", "user.name=praector", "-c", "user.email=praector@local", "merge", "--no-ff", branch, "-m", message)
	if err != nil || p.Code != 0 {
		_, _ = w.git(target, false, "merge", "--abort")
		detail := strings.TrimSpace(firstNonEmpty(p.Stderr, p.Stdout))
		if err != nil && detail == "" {
			detail = err.Error()
		}
		return "", fmt.Errorf("Integration conflict for %s: %s", S(task, "id"), detail)
	}
	h, err := w.git(target, true, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	head := strings.TrimSpace(h.Stdout)
	if label != S(collection, "id") {
		if err := w.Store.SetTaskWorkspace(label, nil, nil, nil, strptr(head)); err != nil {
			return "", err
		}
	}
	return head, nil
}

func (w *GitWorkspace) CheckpointForDelegation(taskID string) (string, error) {
	task, err := w.Store.GetTask(taskID)
	if err != nil {
		return "", err
	}
	if ReadOnlyProfiles[S(task, "profile")] {
		return firstNonEmpty(S(task, "current_head"), S(task, "base_commit")), nil
	}
	return w.Checkpoint(task, 0, "delegation-checkpoint")
}

func (w *GitWorkspace) CollectionDiff(project, collection Row, stat bool, target string) (string, error) {
	repo := w.RepoRoot(project)
	base := target
	if base == "" {
		base = S(project, "base_branch")
	}
	branch := S(collection, "integration_branch")
	args := []string{"diff"}
	if stat {
		args = append(args, "--stat")
	}
	args = append(args, base+"..."+branch)
	p, err := w.git(repo, true, args...)
	return p.Stdout, err
}

func (w *GitWorkspace) BranchWorktree(repo, branch string) (string, error) {
	p, err := w.git(repo, true, "worktree", "list", "--porcelain")
	if err != nil {
		return "", err
	}
	current := ""
	for _, line := range strings.Split(p.Stdout, "\n") {
		if strings.HasPrefix(line, "worktree ") {
			current = strings.TrimSpace(strings.TrimPrefix(line, "worktree "))
			if abs, e := filepath.Abs(current); e == nil {
				current = abs
			}
		} else if line == "branch refs/heads/"+branch && current != "" {
			return current, nil
		} else if strings.TrimSpace(line) == "" {
			current = ""
		}
	}
	return "", nil
}

func (w *GitWorkspace) BranchExists(repo, branch string) (bool, error) {
	p, err := w.git(repo, false, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	if err != nil {
		return false, err
	}
	return p.Code == 0, nil
}

func (w *GitWorkspace) IsAncestor(repo, ancestor, descendant string) (bool, error) {
	p, err := w.git(repo, false, "merge-base", "--is-ancestor", ancestor, descendant)
	if err != nil {
		return false, err
	}
	return p.Code == 0, nil
}

func (w *GitWorkspace) AcceptancePlan(project, collection Row, targetBranch string) (Row, error) {
	repo := w.RepoRoot(project)
	source := S(collection, "integration_branch")
	target := targetBranch
	if target == "" {
		target = S(project, "base_branch")
	}
	for _, b := range []string{source, target} {
		ok, err := w.BranchExists(repo, b)
		if err != nil {
			return nil, err
		}
		if !ok {
			if b == source {
				return nil, fmt.Errorf("Collection branch does not exist: %s", b)
			}
			return nil, fmt.Errorf("Target branch does not exist: %s", b)
		}
	}
	sp, err := w.git(repo, true, "rev-parse", source)
	if err != nil {
		return nil, err
	}
	tp, err := w.git(repo, true, "rev-parse", target)
	if err != nil {
		return nil, err
	}
	sc, tc := strings.TrimSpace(sp.Stdout), strings.TrimSpace(tp.Stdout)
	contained, err := w.IsAncestor(repo, sc, tc)
	if err != nil {
		return nil, err
	}
	ff, err := w.IsAncestor(repo, tc, sc)
	if err != nil {
		return nil, err
	}
	d, err := w.git(repo, true, "diff", "--stat", target+"..."+source)
	if err != nil {
		return nil, err
	}
	return Row{"source_branch": source, "source_commit": sc, "target_branch": target, "target_commit": tc, "already_contained": contained, "fast_forward": ff, "diff_stat": strings.TrimRight(d.Stdout, "\r\n")}, nil
}

func (w *GitWorkspace) AcceptCollection(project, collection Row, targetBranch string, noFF bool) (Row, error) {
	repo := w.RepoRoot(project)
	source := S(collection, "integration_branch")
	target := targetBranch
	if target == "" {
		target = S(project, "base_branch")
	}
	for _, b := range []string{source, target} {
		ok, err := w.BranchExists(repo, b)
		if err != nil {
			return nil, err
		}
		if !ok {
			if b == source {
				return nil, fmt.Errorf("Collection branch does not exist: %s", b)
			}
			return nil, fmt.Errorf("Target branch does not exist: %s", b)
		}
	}
	sp, err := w.git(repo, true, "rev-parse", source)
	if err != nil {
		return nil, err
	}
	tp, err := w.git(repo, true, "rev-parse", target)
	if err != nil {
		return nil, err
	}
	sc, before := strings.TrimSpace(sp.Stdout), strings.TrimSpace(tp.Stdout)
	if ip := S(collection, "integration_worktree"); ip != "" && exists(ip) {
		dirty, err := w.IsDirty(ip)
		if err != nil {
			return nil, err
		}
		if dirty {
			return nil, fmt.Errorf("Collection integration worktree is dirty: %s. Resolve or checkpoint those changes before acceptance.", ip)
		}
	}
	contained, err := w.IsAncestor(repo, sc, before)
	if err != nil {
		return nil, err
	}
	if contained {
		return Row{"source_branch": source, "source_commit": sc, "target_branch": target, "target_before_commit": before, "accepted_commit": before, "merge_mode": "already-contained"}, nil
	}
	ff, err := w.IsAncestor(repo, before, sc)
	if err != nil {
		return nil, err
	}
	if !noFF && !ff {
		return nil, fmt.Errorf("Target branch %s has diverged from %s; refusing the safe fast-forward. Review the divergence and retry with `praector collection accept %s --target %s --no-ff` only if a merge commit is intended.", target, source, S(collection, "id"), target)
	}
	targetWT, err := w.BranchWorktree(repo, target)
	if err != nil {
		return nil, err
	}
	temporary := false
	if targetWT == "" {
		re := regexp.MustCompile(`[^A-Za-z0-9._-]+`)
		safe := strings.Trim(re.ReplaceAllString(target, "-"), "-")
		if safe == "" {
			safe = "target"
		}
		targetWT = filepath.Join(w.Cfg.WorktreesDir(), S(project, "id"), S(collection, "id"), "accept-"+safe)
		if err := os.MkdirAll(filepath.Dir(targetWT), 0755); err != nil {
			return nil, err
		}
		if exists(targetWT) && dirNonEmpty(targetWT) {
			return nil, fmt.Errorf("Acceptance worktree path is not empty: %s", targetWT)
		}
		if _, err = w.git(repo, true, "worktree", "add", targetWT, target); err != nil {
			return nil, err
		}
		temporary = true
	}
	if temporary {
		defer func() {
			_, _ = w.git(repo, false, "worktree", "remove", "--force", targetWT)
			_, _ = w.git(repo, false, "worktree", "prune")
		}()
	}
	dirty, err := w.IsDirty(targetWT)
	if err != nil {
		return nil, err
	}
	if dirty {
		return nil, fmt.Errorf("Target branch %s has uncommitted changes in %s; acceptance will not modify a dirty worktree.", target, targetWT)
	}
	mode := "ff-only"
	if noFF {
		p, mergeErr := w.git(targetWT, false, "merge", "--no-ff", source, "-m", fmt.Sprintf("Accept %s: %s", S(collection, "id"), S(collection, "title")))
		if mergeErr != nil || p.Code != 0 {
			_, _ = w.git(targetWT, false, "merge", "--abort")
			return nil, fmt.Errorf("Acceptance merge conflict for %s into %s: %s", S(collection, "id"), target, strings.TrimSpace(firstNonEmpty(p.Stderr, p.Stdout)))
		}
		mode = "no-ff"
	} else if _, err = w.git(targetWT, true, "merge", "--ff-only", source); err != nil {
		return nil, err
	}
	ap, err := w.git(targetWT, true, "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	return Row{"source_branch": source, "source_commit": sc, "target_branch": target, "target_before_commit": before, "accepted_commit": strings.TrimSpace(ap.Stdout), "merge_mode": mode}, nil
}

func (w *GitWorkspace) CleanupCollection(project, collection Row, deleteBranches bool) (Row, error) {
	repo := w.RepoRoot(project)
	removedWT, removedBranches := []any{}, []any{}
	tasks, err := w.Store.ListTasks(S(collection, "id"))
	if err != nil {
		return nil, err
	}
	paths := []string{}
	for _, task := range tasks {
		if p := S(task, "worktree_path"); p != "" {
			paths = append(paths, p)
		}
	}
	if p := S(collection, "integration_worktree"); p != "" {
		paths = append(paths, p)
	}
	seen := map[string]bool{}
	for _, path := range paths {
		key := filepath.Clean(path)
		if abs, e := filepath.Abs(path); e == nil && exists(path) {
			key = abs
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		if exists(path) {
			_, _ = w.git(repo, false, "worktree", "remove", "--force", path)
			if exists(path) {
				_ = os.RemoveAll(path)
			}
			removedWT = append(removedWT, path)
		}
	}
	_, _ = w.git(repo, false, "worktree", "prune")

	if deleteBranches {
		acceptance, err := w.Store.GetCollectionAcceptance(S(collection, "id"), "")
		if err != nil {
			return nil, err
		}
		if acceptance == nil {
			return nil, fmt.Errorf("Refusing branch cleanup before the collection has been accepted")
		}
		target := S(acceptance, "target_branch")
		branches := []string{}
		for _, task := range tasks {
			if b := S(task, "branch_name"); b != "" {
				branches = append(branches, b)
			}
		}
		branches = append(branches, S(collection, "integration_branch"))
		for _, branch := range branches {
			if branch == "" || branch == target {
				continue
			}
			be, err := w.BranchExists(repo, branch)
			if err != nil || !be {
				continue
			}
			bp, e1 := w.git(repo, true, "rev-parse", branch)
			tp, e2 := w.git(repo, true, "rev-parse", target)
			if e1 != nil || e2 != nil {
				continue
			}
			ok, e := w.IsAncestor(repo, strings.TrimSpace(bp.Stdout), strings.TrimSpace(tp.Stdout))
			if e != nil || !ok {
				continue
			}
			p, _ := w.git(repo, false, "branch", "-d", branch)
			if p.Code == 0 {
				removedBranches = append(removedBranches, branch)
			}
		}
	}
	return Row{"worktrees": removedWT, "branches": removedBranches}, nil
}
