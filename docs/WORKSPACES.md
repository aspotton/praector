# Git workspaces, integration, review, and acceptance

[Documentation index](README.md) · [Architecture](ARCHITECTURE.md) · [Operations](OPERATIONS.md)

Git is Praector's durable store for project-file state. Dolt stores references to Git state and organizational metadata, not a second copy of repository contents.

## Safety objective

The user's original checkout is not an agent workspace.

Given a repository such as:

```text
/home/user/src/my-project
```

Praector-managed mutation occurs under:

```text
$PRAECTOR_HOME/worktrees/<project>/<collection>/...
```

This avoids switching the user's branch or placing uncommitted agent changes in the user's working tree.

## Collection integration branch

Each Collection owns an integration branch:

```text
praector/collections/<collection-id-lowercase>
```

It is created from the Project's configured base branch when the Collection workspace is first prepared.

Physical integration worktree:

```text
$PRAECTOR_HOME/worktrees/<project-id>/<collection-id>/integration
```

The Collection branch is the assembled candidate result. It remains separate from `main`/`master` until explicit acceptance.

## Task branches

Write-capable implementation Tasks receive a branch:

```text
praector/tasks/<collection-id-lowercase>/<task-id-lowercase>
```

and worktree:

```text
$PRAECTOR_HOME/worktrees/<project-id>/<collection-id>/<task-id>
```

Read-only profiles receive detached worktrees instead of task branches.

## Workspace modes

Tasks carry a `workspace_mode`.

### `collection`

Start from the current Collection integration branch.

Use for normal Collection-level work that should build on already integrated prerequisites.

### `inherit`

Start from an explicit parent/base checkpoint.

Use when a blocking child must work on the parent's in-progress code before that parent has integrated at Collection level.

### `isolated`

Start from the Project base branch, independent of the current Collection integration state.

Useful for checks that should not inherit unrelated Collection changes.

### `auto`

CLI request mode that lets Praector choose the appropriate durable workspace relationship.

For most runtime delegation, prefer `auto` unless the semantics are explicitly known.

## Task preparation

### Read-only Task

For `coordinator`, `explore`, `critic`, `plan_review`, `research`, `review`, or `advise`:

1. create/reuse detached worktree;
2. select base reference from workspace mode/base commit;
3. hard-reset/clean reused worktree;
4. persist worktree/base/current-head references in Dolt;
5. launch OpenCode with that worktree as `--dir`.

After the run, Git mutation is checked. Unauthorized modifications are rolled back and fail the Task.

### Implementation Task

For `implement`:

1. create/reuse task branch/worktree;
2. base it on the appropriate Collection/base/inherited commit;
3. persist branch/worktree/head references;
4. run the implementation attempt;
5. checkpoint dirty state at attempt boundary;
6. integrate after successful verification.

## Checkpoints

Praector checkpoints implementation work by staging all changes and committing them when the worktree is dirty. New Praector-generated commits use human-readable Task titles as their subjects, while durable provenance stays in the commit body. A typical checkpoint looks like:

```text
Add authenticated GET /admin/secure-ping endpoint

Added the authenticated secure-ping endpoint with regression coverage.

Praector-Collection: COL-...
Praector-Task: T-...
Praector-Attempt: 1
Praector-Operation: attempt-checkpoint
```

Important checkpoint boundaries include:

- attempt completion;
- before blocking delegation;
- retry/recovery boundaries as needed.

Git, not model conversation text, is the durable representation of in-progress code.

## Blocking inherited implementation

The important parent/child case is:

```text
Parent implement Task
      │
      ├── uncommitted/in-progress code
      │
      ├── blocking delegated implement child
      ▼
parent checkpoint commit C
      │
      └──── Child branch starts from C
                  │
                  ▼
             child commits
                  │
                  ▼
        merge into Parent task branch
                  │
                  ▼
fresh Parent attempt resumes with child code
```

The child does **not** merge directly into the Collection branch. It is a contribution to the parent Task's responsibility.

When the parent later completes, the combined parent branch integrates into the Collection branch.

## Collection-level implementation integration

A normal implementation Task merges into the Collection integration worktree using an explicit merge commit with a human-readable subject:

```text
Integrate: Add authenticated GET /admin/secure-ping endpoint
```

The merge commit body carries the implementation summary when available plus `Praector-Collection`, `Praector-Task`, and `Praector-Operation: integration` provenance. Task IDs therefore remain machine-traceable without making `git log --oneline` depend on Praector context.

If integration conflicts, Praector aborts the merge, marks the implementation result failed, and requests coordinator attention for graph repair.

## Independent review workspace

Review Tasks are read-only and normally use a detached worktree representing the integrated Collection candidate.

The reviewer should inspect the actual integrated code, run appropriate checks/tests, and return an explicit `pass`/`fail` verdict. It should not trust the implementer's claims as proof.

Because verification commands themselves can create bytecode, test caches, coverage output, generated files, or other artifacts, read-only workers capture the starting worktree status and must finish with the same status. Prefer non-mutating verification modes or caches outside the worktree where practical. If a check does touch the repository, restore only the incidental changes caused by that check; never erase candidate or pre-existing state to manufacture a clean status.

A reviewer may create an ephemeral tool/test environment outside the repository if necessary, but must not leave modifications in the candidate project worktree.

## The original checkout

The user's original checkout should remain untouched while Praector works.

A useful operational verification is:

```bash
git status
```

on the original checkout while a Collection runs. Implementation changes should instead be visible in the Praector task/Collection worktrees and branches.

## Pull-request delivery

For GitHub review workflows, a completed candidate should normally be published from the Collection integration branch rather than an individual Task branch:

```bash
praector collection pr COL-...
```

The command requires the same acceptance-grade `COMPLETE` state as `collection accept`, pushes the Collection integration branch, and creates or reuses a GitHub PR whose head is that branch. Individual `praector/tasks/...` branches are implementation details and are not the PR boundary.

PR delivery does not merge or accept the Collection. It records delivery metadata in Dolt while the Collection remains `COMPLETE`. If human review requests a change, use:

```bash
praector collection revise COL-... --reason "Concrete requested correction"
```

After fresh implementation/review returns the Collection to `COMPLETE`, rerun `praector collection pr COL-...`; Praector pushes the same integration branch so the open PR updates naturally.

`--dry-run` previews base/head/title/body without pushing. `--draft`, `--title`, `--base`, `--remote`, and `--open` cover the initial GitHub publication workflow.

If the PR is eventually merged externally, `praector collection accept COL-...` can still record acceptance using the existing `already-contained` path because the target now contains the exact Collection source commit.

## Acceptance

Collection execution and base-branch acceptance are deliberately separate. Human inspection can reopen completed, unaccepted work before merge:

```bash
praector collection revise COL-... --reason "Concrete acceptance defect"
```

The Collection returns to `RUNNING`; an open revision requires fresh repaired implementation plus a passing independent review before `COMPLETE` is restored. Once a Collection has an acceptance record, it is immutable and follow-up changes belong in a new Collection.


### Preconditions

`praector collection accept` validates that:

- Collection status is `COMPLETE`;
- every completed review has an explicit `pass|fail` verdict;
- every completed implementation is covered by a completed independent passing review; historical failed reviews may remain as durable evidence after a later fresh post-repair review passes;
- the Collection integration branch exists;
- the Collection integration worktree is clean;
- the target branch exists;
- the target branch worktree, if checked out, is clean.

### Preview

```bash
praector collection diff COL-... --stat
praector collection accept COL-... --dry-run
```

### Default: fast-forward-only

```bash
praector collection accept COL-...
```

The target defaults to the Project base branch.

If target is an ancestor of the Collection source, Praector performs:

```text
git merge --ff-only <collection-branch>
```

This is the safe default because no merge decision is being silently invented.

### Diverged target

If the target advanced independently, default acceptance refuses.

After reviewing the divergence, explicitly opt into a merge commit:

```bash
praector collection accept COL-... --no-ff
```

Praector uses a message like:

```text
Accept COL-...: <collection title>
```

If the merge conflicts, Praector aborts it and leaves acceptance unrecorded.

### Acceptance without switching the user's checkout

If the target branch is already checked out in a worktree, Praector uses that worktree after checking it is clean.

If it is not checked out anywhere, Praector creates a temporary acceptance worktree under `$PRAECTOR_HOME/worktrees/...`, performs the merge there, then removes it.

### Idempotency and mutation defense

If the target already contains the exact Collection source commit, acceptance is a no-op from Git's perspective and records/recognizes `already-contained` semantics.

If a Collection was previously accepted but its source branch later points at a different commit, Praector refuses to silently re-accept mutated completed work.

Acceptance records include:

- source branch/commit;
- target branch/target-before commit;
- resulting accepted commit;
- merge mode;
- actor;
- timestamp.

See [STATE_AND_RECOVERY.md](STATE_AND_RECOVERY.md#acceptance-records).

## Cleanup

After acceptance:

```bash
praector collection cleanup COL-...
```

removes Task and integration worktrees.

To also delete merged Praector branches:

```bash
praector collection cleanup COL-... --delete-branches
```

Branch deletion is conservative:

- an acceptance record is required;
- the accepted target is protected;
- a branch is deleted only if its tip is reachable from the accepted target.

Before acceptance, `--force` can remove worktrees for a completed Collection, but does not make unaccepted branch deletion safe.

## Inspecting Git state

Useful commands:

```bash
git worktree list

git branch --list 'praector/*'

git log --oneline --decorate praector/collections/<collection-id> -20

git diff <base>...praector/collections/<collection-id>
```

For operator-facing convenience prefer:

```bash
praector collection diff COL-...
praector collection show COL-...
```
