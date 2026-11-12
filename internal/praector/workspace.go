package praector

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

type GitWorkspace struct {
	Store *Store
	Cfg   GlobalConfig
	Git   string
}

// WorkspaceSnapshot captures the Git identity that a read-only attempt must
// preserve. HeadReflog makes a checkout-away-and-back visible even when HEAD,
// branch attachment, and status look unchanged again at attempt completion.
type WorkspaceSnapshot struct {
	Head       string
	Branch     string
	Status     string
	HeadReflog string
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

func (w *GitWorkspace) WorkspaceSnapshot(worktree string) (WorkspaceSnapshot, error) {
	head, err := w.git(worktree, true, "rev-parse", "HEAD")
	if err != nil {
		return WorkspaceSnapshot{}, err
	}
	branch, err := w.git(worktree, false, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return WorkspaceSnapshot{}, err
	}
	if branch.Code != 0 && branch.Code != 1 {
		return WorkspaceSnapshot{}, fmt.Errorf("cannot resolve worktree branch in %s: %s", worktree, strings.TrimSpace(firstNonEmpty(branch.Stderr, branch.Stdout)))
	}
	status, err := w.git(worktree, true, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return WorkspaceSnapshot{}, err
	}
	reflog, err := w.git(worktree, false, "reflog", "show", "--format=%H", "HEAD")
	if err != nil {
		return WorkspaceSnapshot{}, err
	}
	if reflog.Code != 0 {
		return WorkspaceSnapshot{}, fmt.Errorf("cannot read worktree HEAD reflog in %s: %s", worktree, strings.TrimSpace(firstNonEmpty(reflog.Stderr, reflog.Stdout)))
	}
	return WorkspaceSnapshot{
		Head:       strings.TrimSpace(head.Stdout),
		Branch:     strings.TrimSpace(branch.Stdout),
		Status:     strings.TrimRight(status.Stdout, "\r\n"),
		HeadReflog: strings.TrimRight(reflog.Stdout, "\r\n"),
	}, nil
}

func (w *GitWorkspace) validateAttachedBranch(worktree, expected, label string) (WorkspaceSnapshot, error) {
	state, err := w.WorkspaceSnapshot(worktree)
	if err != nil {
		return WorkspaceSnapshot{}, err
	}
	if state.Branch != expected {
		actual := state.Branch
		if actual == "" {
			actual = "detached HEAD"
		}
		return WorkspaceSnapshot{}, fmt.Errorf("%s must remain on Praector-assigned branch %s; found %s at %s", label, expected, actual, state.Head)
	}
	return state, nil
}

// ValidateReadOnlyWorkspace proves that a read-only attempt did not mutate
// files, move HEAD, attach to a branch, or temporarily check out another commit.
func (w *GitWorkspace) ValidateReadOnlyWorkspace(worktree string, baseline WorkspaceSnapshot) error {
	current, err := w.WorkspaceSnapshot(worktree)
	if err != nil {
		return err
	}
	if current.Branch != baseline.Branch {
		return fmt.Errorf("read-only worktree branch identity changed from %q to %q", baseline.Branch, current.Branch)
	}
	if current.Head != baseline.Head {
		return fmt.Errorf("read-only worktree HEAD changed from %s to %s", baseline.Head, current.Head)
	}
	if current.Status != baseline.Status {
		return fmt.Errorf("read-only worktree file status changed")
	}
	if current.HeadReflog != baseline.HeadReflog {
		return fmt.Errorf("read-only worktree HEAD history changed; checkout, switch, reset, or commit operations are not allowed")
	}
	return nil
}

func (w *GitWorkspace) RestoreReadOnlyWorkspace(worktree string, baseline WorkspaceSnapshot) error {
	if _, err := w.git(worktree, true, "checkout", "--detach", "--force", baseline.Head); err != nil {
		return err
	}
	_, err := w.git(worktree, true, "clean", "-fd")
	return err
}

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
	if _, err := w.validateAttachedBranch(path, branch, "Collection integration worktree"); err != nil {
		return "", err
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
			// A prior read-only worker may have attached this worktree to an
			// unrelated branch. Detach before resetting so recovery never moves
			// or destroys that branch while restoring the assigned candidate.
			if _, err := w.git(taskPath, true, "checkout", "--detach", "--force", baseRef); err != nil {
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
		state, err := w.WorkspaceSnapshot(taskPath)
		if err != nil {
			return "", err
		}
		if state.Branch != "" {
			return "", fmt.Errorf("Read-only task %s must use detached HEAD; found branch %s", S(task, "id"), state.Branch)
		}
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
	state, err := w.validateAttachedBranch(taskPath, branch, "Implementation task "+S(task, "id"))
	if err != nil {
		return "", err
	}
	head := state.Head
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
	branch := S(task, "branch_name")
	if branch == "" {
		return "", fmt.Errorf("Task %s has no assigned branch to checkpoint", S(task, "id"))
	}
	if _, err := w.validateAttachedBranch(path, branch, "Implementation task "+S(task, "id")); err != nil {
		return "", err
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
	state, err := w.validateAttachedBranch(path, branch, "Implementation task "+S(task, "id"))
	if err != nil {
		return "", err
	}
	head := state.Head
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
	head := S(task, "current_head")
	if head == "" {
		p, err := w.git(path, true, "rev-parse", "HEAD")
		if err != nil {
			return err
		}
		head = strings.TrimSpace(p.Stdout)
	}
	if _, err := w.git(path, true, "checkout", "--detach", "--force", head); err != nil {
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
	currentHead := S(task, "current_head")
	if currentHead == "" {
		return "", fmt.Errorf("Task %s has no checkpoint commit to integrate", S(task, "id"))
	}
	path := S(task, "worktree_path")
	if path == "" || !exists(path) {
		return "", fmt.Errorf("Task %s has no available worktree to validate before integration", S(task, "id"))
	}
	state, err := w.validateAttachedBranch(path, branch, "Implementation task "+S(task, "id"))
	if err != nil {
		return "", err
	}
	if state.Head != currentHead {
		return "", fmt.Errorf("Task %s worktree HEAD %s does not match durable checkpoint %s", S(task, "id"), state.Head, currentHead)
	}
	repo := w.RepoRoot(project)
	branchHead, err := w.git(repo, true, "rev-parse", "refs/heads/"+branch)
	if err != nil {
		return "", err
	}
	if got := strings.TrimSpace(branchHead.Stdout); got != currentHead {
		return "", fmt.Errorf("Task %s assigned branch %s points to %s, not durable checkpoint %s", S(task, "id"), branch, got, currentHead)
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
			parentBranch := S(parent, "branch_name")
			if parentBranch == "" {
				return "", fmt.Errorf("Parent implementation task %s has no assigned branch", parentID)
			}
			if _, err := w.validateAttachedBranch(target, parentBranch, "Parent implementation task "+parentID); err != nil {
				return "", err
			}
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
	dirty, err := w.IsDirty(target)
	if err != nil {
		return "", err
	}
	if dirty {
		return "", fmt.Errorf("Integration target %s is dirty; refusing to merge task %s", target, S(task, "id"))
	}
	message := taskGitCommitMessage(task, "integration", 0, "Integrate")
	// Merge the immutable checkpoint SHA after proving the assigned branch
	// still names it. This closes the gap where a worker could switch to an
	// ad-hoc branch while Praector merged a stale managed branch successfully.
	p, err := w.git(target, false, "-c", "user.name=praector", "-c", "user.email=praector@local", "merge", "--no-ff", currentHead, "-m", message)
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
	contained, err := w.IsAncestor(target, currentHead, head)
	if err != nil {
		return "", err
	}
	if !contained {
		return "", fmt.Errorf("Integration verification failed for %s: checkpoint %s is not reachable from target HEAD %s", S(task, "id"), currentHead, head)
	}
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

// gitCLIFlags are CLI flags and subcommand names of git itself that the
// candidate extraction must never report as introduced configuration knobs:
// Praector's own diff plumbing (including this helper) adds them in added
// lines constantly, and they are tool invocations, not project settings.
var gitCLIFlags = map[string]bool{
	"-z": true, "--name-only": true, "--stat": true, "--no-ff": true, "-m": true,
	"--abort": true, "--is-ancestor": true, "--porcelain": true, "--verify": true,
	"--quiet": true, "-b": true, "-C": true, "-e": true, "-U0": true, "--unified=0": true,
	"-c": true, "--full-name": true, "--no-color": true, "--abbrev-commit": true,
	"--no-renames": true, "--git-dir": true, "--name-status": true, "--shortstat": true,
	"merge": true, "diff": true, "status": true, "log": true, "show": true, "add": true,
	"commit": true, "init": true, "checkout": true, "branch": true, "worktree": true,
	"rev-parse": true, "show-ref": true, "merge-base": true, "ls-files": true,
	"update-ref": true, "symbolic-ref": true, "stash": true, "reset": true, "rm": true,
}

// commonEnvVars are environment names so ubiquitous that reporting them as
// introduced knobs would be pure noise; the extraction skips them. The
// PRAECTOR_* set is the documented, pre-existing worker environment contract.
var commonEnvVars = map[string]bool{
	"PATH": true, "HOME": true, "USER": true, "SHELL": true, "TERM": true, "TMPDIR": true,
	"TEMP": true, "TMP": true, "LANG": true, "LC_ALL": true, "EDITOR": true, "PWD": true,
	"VIRTUAL_ENV": true, "GOPATH": true, "GOROOT": true, "PYTHONPATH": true, "CI": true,
	"PRAECTOR_HOME": true, "PRAECTOR_PROJECT_ID": true, "PRAECTOR_COLLECTION_ID": true,
	"PRAECTOR_TASK_ID": true, "PRAECTOR_ATTEMPT_ID": true, "PRAECTOR_SESSION_ID": true,
	"PRAECTOR_PROFILE": true, "PRAECTOR_WORKTREE": true, "PRAECTOR_CLI": true,
}

var (
	flagLiteralRe     = regexp.MustCompile(`"(-{1,2}[a-z][a-z0-9]+(?:-[a-z0-9]+)+)"`)
	envNameRe         = regexp.MustCompile(`\b([A-Z][A-Z0-9]*(?:_[A-Z0-9]+){1,})\b`)
	quotedKeyRe       = regexp.MustCompile(`"([a-z][a-z0-9]*(?:[_-][a-z0-9]+)+)"`)
	settingWordRe     = regexp.MustCompile(`(?i)(config|setting|flag|option|enabled|disabled|toggle|mode)`)
	derivationSkipExt = map[string]bool{".md": true, ".txt": true, ".rst": true, ".license": true, ".lock": true, ".sum": true}
)

// ConfigCandidateOutcome pairs a diff-derived candidate list with an explicit
// basis-availability outcome so a submit or acceptance seam can distinguish
// "this change introduced no knobs" from "no diff basis could be derived".
// BasisUnavailable is empty whenever a derivable three-dot diff basis existed
// (regardless of whether any candidates were found) and non-empty — naming the
// missing ref or state — when the project root is not a Git repository or the
// base/integration branch is absent. A git invocation error against an EXISTING
// basis is returned as a non-nil error and is never folded into this field, so
// a hard git failure can never masquerade as an empty candidate list.
type ConfigCandidateOutcome struct {
	Candidates       []DerivedConfigCandidate
	BasisUnavailable string
}

// DeriveConfigCandidates heuristically derives candidate configuration knobs
// introduced between the project base branch and the collection integration
// branch. The diff basis is deliberately the same three-dot merge-base form
// used by CollectionDiff (`base...integration_branch`, never a two-dot diff,
// which would also count base-branch-side changes as introduced), and the
// changed-path listing uses `-z --name-only` so core.quotePath quoting,
// spaces, or unusual characters cannot silently drop a changed file. The
// added-line scan runs against the added side of the diff regardless of
// rename-detection setting, so a knob introduced via a move/clone is still
// enumerated.
//
// The result is a deterministic, sorted candidate list a passing review must
// account for — NOT a completeness proof: the extraction is narrow
// (flag/key literals, environment-variable names, config/table keys, and
// setting-like struct/map keys) and human-falsifiable in both directions.
// When the repo or either branch simply does not exist (e.g. a collection that
// never created Git state) it returns no candidates with no error; that is an
// ABSENT basis rather than a genuine no-knob diff, so callers that must tell
// the two apart use DeriveConfigCandidatesWithBasis. A git invocation failure
// against an existing basis is returned loudly rather than degraded to an
// empty list.
func (w *GitWorkspace) DeriveConfigCandidates(project, collection Row) ([]DerivedConfigCandidate, error) {
	outcome, err := w.DeriveConfigCandidatesWithBasis(project, collection)
	return outcome.Candidates, err
}

// DeriveConfigCandidatesWithBasis is DeriveConfigCandidates plus an explicit
// absent-basis outcome. An absent basis (a non-Git project root or an absent
// base/integration branch) yields no candidates AND a named
// ConfigCandidateOutcome.BasisUnavailable so a seam can record it as auditable
// state instead of letting it read as a clean no-knob diff. The three-dot
// `base...integration` basis, the `-z` path listing, and the `--no-renames`
// added-side scan are used unchanged on every path that DOES compute
// candidates. A git invocation error against an existing basis is a hard error,
// never an absent-basis outcome.
func (w *GitWorkspace) DeriveConfigCandidatesWithBasis(project, collection Row) (ConfigCandidateOutcome, error) {
	repo := w.RepoRoot(project)
	base := S(project, "base_branch")
	branch := S(collection, "integration_branch")
	// Distinguish the three states explicitly: a git invocation error at the
	// rev-parse gate is a hard error; a git run that reports "not a repository"
	// (non-zero exit, no run error) is an absent basis; and a repository whose
	// base or integration branch is missing is an absent basis naming the ref.
	repoCheck, err := w.git(repo, false, "rev-parse", "--git-dir")
	if err != nil {
		return ConfigCandidateOutcome{}, err
	}
	if repoCheck.Code != 0 {
		return ConfigCandidateOutcome{BasisUnavailable: fmt.Sprintf("project root %s is not a git repository", repo)}, nil
	}
	baseOK, err := w.BranchExists(repo, base)
	if err != nil {
		return ConfigCandidateOutcome{}, err
	}
	if !baseOK {
		return ConfigCandidateOutcome{BasisUnavailable: fmt.Sprintf("base branch %q does not exist", base)}, nil
	}
	branchOK, err := w.BranchExists(repo, branch)
	if err != nil {
		return ConfigCandidateOutcome{}, err
	}
	if !branchOK {
		return ConfigCandidateOutcome{BasisUnavailable: fmt.Sprintf("integration branch %q does not exist", branch)}, nil
	}
	paths, err := w.ChangedPaths(repo, base, branch)
	if err != nil {
		return ConfigCandidateOutcome{}, err
	}
	p, err := w.git(repo, true, "diff", "-z", "--unified=0", "--no-renames", base+"..."+branch)
	if err != nil {
		return ConfigCandidateOutcome{}, err
	}
	return ConfigCandidateOutcome{Candidates: extractConfigCandidates(paths, p.Stdout)}, nil
}

// ChangedPaths lists the files changed between base and head using the
// same three-dot basis as CollectionDiff, NUL-separated on the git side so
// quoting cannot drop a path (CLM-NAMEONLY-Z).
func (w *GitWorkspace) ChangedPaths(repo, base, head string) ([]string, error) {
	p, err := w.git(repo, true, "diff", "-z", "--name-only", base+"..."+head)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, path := range strings.Split(p.Stdout, "\x00") {
		if path = strings.TrimSpace(path); path != "" {
			out = append(out, path)
		}
	}
	sort.Strings(out)
	return out, nil
}

// extractConfigCandidates is the pure, deterministic token extraction over
// the added lines of a unified diff, restricted to the changed paths whose
// extension can plausibly carry a setting. It is separate from the Git call
// so determinism is directly testable.
func extractConfigCandidates(paths []string, diffText string) []DerivedConfigCandidate {
	scannable := map[string]bool{}
	for _, path := range paths {
		if ext := strings.ToLower(filepath.Ext(path)); ext != "" && derivationSkipExt[ext] {
			continue
		}
		scannable[path] = true
	}
	seen := map[string]bool{}
	out := []DerivedConfigCandidate{}
	current := ""
	for _, line := range strings.Split(diffText, "\n") {
		if strings.HasPrefix(line, "diff --git") || strings.HasPrefix(line, "index ") || strings.HasPrefix(line, "@@") {
			continue
		}
		if strings.HasPrefix(line, "+++ ") {
			current = strings.TrimPrefix(strings.TrimSpace(strings.SplitN(line[4:], "\t", 2)[0]), "b/")
			continue
		}
		if strings.HasPrefix(line, "--- ") || !strings.HasPrefix(line, "+") {
			continue
		}
		if current != "" && !scannable[current] {
			continue
		}
		for _, c := range candidateTokensFromLine(current, line[1:]) {
			key := strings.ToLower(c.Token) + "\x00" + c.Path
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Token != out[j].Token {
			return out[i].Token < out[j].Token
		}
		return out[i].Path < out[j].Path
	})
	return out
}

// candidateTokensFromLine applies the narrow per-line heuristics: dashed
// flag literals, SCREAMING_SNAKE environment-variable names, and quoted
// snake_case key literals whose name suggests a setting (or whose file is a
// configuration format rather than Go source).
func candidateTokensFromLine(path, line string) []DerivedConfigCandidate {
	tokens := [][2]string{}
	isGo := strings.HasSuffix(strings.ToLower(path), ".go")
	for _, lit := range flagLiteralRe.FindAllStringSubmatch(line, -1) {
		if flag := lit[1]; !gitCLIFlags[flag] {
			tokens = append(tokens, [2]string{flag, path})
		}
	}
	for _, lit := range envNameRe.FindAllStringSubmatch(line, -1) {
		if name := lit[1]; !commonEnvVars[name] {
			tokens = append(tokens, [2]string{name, path})
		}
	}
	for _, lit := range quotedKeyRe.FindAllStringSubmatch(line, -1) {
		if key := lit[1]; settingWordRe.MatchString(key) || !isGo {
			tokens = append(tokens, [2]string{key, path})
		}
	}
	out := []DerivedConfigCandidate{}
	for _, t := range tokens {
		out = append(out, DerivedConfigCandidate{Token: t[0], Path: t[1]})
	}
	return out
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
