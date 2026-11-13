package praector

import (
	"fmt"
	"strings"
)

// commandHelp keeps help handling independent of runtime/store initialization.
// Every command and subcommand accepts -h/--help without requiring Git, Dolt, a
// project marker, or any other runtime dependency. Preserve that contract in the
// native executable.
var commandHelp = map[string]string{
	"init": `Usage: praector init [--name NAME] [--base-branch BRANCH] [--opencode PATH]

Register the current Git repository as a Praector project.
`,
	"run": `Usage: praector run OBJECTIVE [--title TITLE] [--project ID] [--detach] [--max-workers N]

Create a collection and execute its evolving work graph.
`,
	"status": `Usage: praector status [TARGET] [--project ID]

Show project, collection, or task status.
`,
	"graph": `Usage: praector graph [COLLECTION] [--project ID]

Show the current durable task graph.
`,
	"daemon": `Usage: praector daemon [--once] [--max-workers N]

Run the durable task scheduler.
`,
	"doctor": `Usage: praector doctor

Validate local dependencies and project registration.
`,
	"logs": `Usage: praector logs TARGET [--tail N]

List attempt logs for a task or collection.
`,
	"events": `Usage: praector events [TARGET] [--project ID] [--collection COL-ID] [--type EVENT_TYPE] [--since CURSOR] [--limit N]

Read the durable event log directly from the store (works with no daemon running).
`,
	"collection": `Usage: praector collection {show|revise|diff|pr|accept|cleanup} ...

Collection lifecycle operations:
  show       Show a collection including acceptance/revision history
  revise     Reopen a completed, unaccepted collection for mandatory repair
  diff       Show collection changes relative to a target branch
  pr         Push a reviewed collection branch and create/reuse a GitHub PR
  accept     Accept reviewed collection work into a target branch
  cleanup    Remove completed collection/task worktrees
`,
	"collection show": `Usage: praector collection show COL-ID

Show a collection including acceptance/revision history.
`,
	"collection revise": `Usage: praector collection revise COL-ID --reason TEXT [--detach] [--max-workers N]

Reopen a completed, unaccepted collection for mandatory operator-requested repair.
`,
	"collection diff": `Usage: praector collection diff COL-ID [--target BRANCH] [--stat]

Show collection changes relative to a target branch.
`,
	"collection pr": `Usage: praector collection pr COL-ID [--draft] [--title TITLE] [--base BRANCH] [--remote REMOTE] [--dry-run] [--refresh-copy] [--open]

Push a reviewed collection branch and create/reuse a GitHub pull request.
`,
	"collection accept": `Usage: praector collection accept COL-ID [--target BRANCH] [--no-ff] [--dry-run] [--cleanup] [--delete-branches]

Accept reviewed collection work into a target branch.
`,
	"collection cleanup": `Usage: praector collection cleanup COL-ID [--delete-branches] [--force]

Remove completed collection/task worktrees.
`,
	"profile": `Usage: praector profile {list|show} ...

Inspect built-in runtime profile contracts:
  list       List runtime profiles and capability mode
  show       Print the inherited global and role-specific profile contracts
`,
	"profile list": `Usage: praector profile list

List runtime profiles and capability mode.
`,
	"profile show": `Usage: praector profile show PROFILE

Print the inherited global and role-specific contracts for a runtime profile.
`,
	"project": `Usage: praector project {list|show} ...

Project operations.
`,
	"project list": `Usage: praector project list

List registered projects.
`,
	"project show": `Usage: praector project show [--project ID]

Show a registered project.
`,
	"task": `Usage: praector task {show|list|retry|request|add-dependency|supersede} ...

Task operations:
  show            Show a task
  list            List tasks in a collection
  retry           Create a fresh replacement for a FAILED task
  request         Request durable delegated work
  add-dependency  Coordinator-only graph mutation
  supersede       Coordinator-only replacement/rewire
`,
	"task show": `Usage: praector task show TASK

Show a task.
`,
	"task list": `Usage: praector task list [--collection COL-ID] [--project ID]

List tasks in a collection (defaults to latest collection).
`,
	"task retry": `Usage: praector task retry TASK [--reason TEXT] [--detach] [--max-workers N]

Create a fresh replacement for a FAILED task and preserve prior attempts.
`,
	"task request": `Usage: praector task request --profile PROFILE --title TITLE --objective TEXT --reason TEXT [--task TASK] [--blocking] [--depends-on TASK]... [--workspace MODE]

Request durable delegated work.
`,
	"task add-dependency": `Usage: praector task add-dependency TASK --on DEPENDENCY [--caller TASK]

Coordinator-only graph mutation.
`,
	"task supersede": `Usage: praector task supersede OLD --with NEW --reason TEXT [--caller TASK]

Coordinator-only replacement/rewire.
`,
	"context": `Usage: praector context {show} ...

Durable context operations.
`,
	"context show": `Usage: praector context show [--task TASK]

Show durable task context.
`,
	"coordinator": `Usage: praector coordinator {attention} ...

Coordinator escalation.
`,
	"coordinator attention": `Usage: praector coordinator attention --reason TEXT [--task TASK] [--blocking]

Request coordinator attention.
`,
	"result": `Usage: praector result {show|submit} ...

Structured task results.
`,
	"result show": `Usage: praector result show [TASK]

Show the durable structured result for a task (defaults to current task).
`,
	"result submit": `Usage: praector result submit [--task TASK] [--stdin] [--file PATH] [--summary TEXT] [--status STATUS]

Submit a durable structured task result.
`,
	"memory": `Usage: praector memory {add|list} ...

Project/collection memory.
`,
	"memory add": `Usage: praector memory add --text TEXT [--project ID] [--collection COL-ID] [--task TASK] [--scope SCOPE] [--state STATE] [--kind KIND]

Add durable project/collection memory.
`,
	"memory list": `Usage: praector memory list [--project ID] [--collection COL-ID] [--state STATE]

List durable memory.
`,
	"decision": `Usage: praector decision {add} ...

Durable decisions.
`,
	"decision add": `Usage: praector decision add --statement TEXT [--rationale TEXT] [--project ID] [--collection COL-ID] [--task TASK]

Add a durable decision.
`,
}

func isHelpToken(s string) bool { return s == "-h" || s == "--help" }

// maybePrintCommandHelp handles argparse-compatible help before any command can
// initialize the store or inspect the project. It returns true when help was
// printed and the caller should exit successfully.
func maybePrintCommandHelp(args []string) bool {
	if len(args) < 2 || !isHelpToken(args[len(args)-1]) {
		return false
	}
	parts := []string{}
	for _, a := range args[:len(args)-1] {
		if strings.HasPrefix(a, "-") {
			break
		}
		parts = append(parts, a)
		if len(parts) == 2 {
			break
		}
	}
	key := strings.Join(parts, " ")
	if text, ok := commandHelp[key]; ok {
		fmt.Print(text)
		return true
	}
	if len(parts) > 0 {
		if text, ok := commandHelp[parts[0]]; ok {
			fmt.Print(text)
			return true
		}
	}
	return false
}
