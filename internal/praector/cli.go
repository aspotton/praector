package praector

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type parsedArgs struct {
	Pos []string
	Opt map[string][]string
}

func parseArgs(args []string, spec map[string]bool) (parsedArgs, error) {
	p := parsedArgs{Opt: map[string][]string{}}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			p.Pos = append(p.Pos, args[i+1:]...)
			break
		}
		if strings.HasPrefix(a, "--") {
			name := strings.TrimPrefix(a, "--")
			value := ""
			if n, v, ok := strings.Cut(name, "="); ok {
				name, value = n, v
			}
			takes, known := spec[name]
			if !known {
				return p, fmt.Errorf("unknown option --%s", name)
			}
			if takes && value == "" {
				if i+1 >= len(args) {
					return p, fmt.Errorf("option --%s requires a value", name)
				}
				i++
				value = args[i]
			}
			if !takes && value != "" {
				return p, fmt.Errorf("option --%s does not take a value", name)
			}
			if !takes {
				value = "true"
			}
			p.Opt[name] = append(p.Opt[name], value)
			continue
		}
		p.Pos = append(p.Pos, a)
	}
	return p, nil
}
func (p parsedArgs) str(k string) string {
	xs := p.Opt[k]
	if len(xs) == 0 {
		return ""
	}
	return xs[len(xs)-1]
}
func (p parsedArgs) strs(k string) []string { return append([]string{}, p.Opt[k]...) }
func (p parsedArgs) bool(k string) bool     { return len(p.Opt[k]) > 0 }
func (p parsedArgs) integer(k string, def int) (int, error) {
	s := p.str(k)
	if s == "" {
		return def, nil
	}
	n, e := strconv.Atoi(s)
	if e != nil {
		return 0, fmt.Errorf("--%s must be an integer", k)
	}
	return n, nil
}
func requirePos(p parsedArgs, n int, usage string) error {
	if len(p.Pos) != n {
		return fmt.Errorf("usage: %s", usage)
	}
	return nil
}

func newStore() (*Store, GlobalConfig, error) {
	cfg, err := Global()
	if err != nil {
		return nil, cfg, err
	}
	s, err := NewStore(cfg)
	if err != nil {
		return nil, cfg, err
	}
	if err = s.Bootstrap(); err != nil {
		return nil, cfg, err
	}
	return s, cfg, nil
}
func projectFromContext(store *Store, identifier string) (Row, error) {
	if identifier != "" {
		return store.GetProject(identifier)
	}
	if id := os.Getenv("PRAECTOR_PROJECT_ID"); id != "" {
		return store.GetProject(id)
	}
	marker, ok := FindProjectMarker("")
	if !ok {
		return nil, fmt.Errorf("No Praector project found. Run `praector init` in a Git repository or pass --project.")
	}
	cfg, err := ReadProjectMarker(marker)
	if err != nil {
		return nil, err
	}
	return store.RegisterProject(cfg)
}
func currentTaskID(explicit string) (string, error) {
	id := explicit
	if id == "" {
		id = os.Getenv("PRAECTOR_TASK_ID")
	}
	if id == "" {
		return "", fmt.Errorf("No current task. Pass --task or run this command from a Praector-managed OpenCode task.")
	}
	return id, nil
}
func latestCollection(store *Store, pid string) (Row, error) {
	r, err := store.ListCollections(pid)
	if err != nil {
		return nil, err
	}
	if len(r) == 0 {
		return nil, fmt.Errorf("Project has no collections yet")
	}
	return r[0], nil
}
func preflightProjectRuntime(project Row) error {
	for _, b := range []string{"git", "dolt"} {
		if _, err := RequireBinary(b); err != nil {
			return err
		}
	}
	cfg := ProjectConfigFromRow(project)
	if cfg.Harness != "opencode" {
		return fmt.Errorf("Unsupported harness in the current release: %q (expected 'opencode')", cfg.Harness)
	}
	if _, err := cfg.ResolvedOpenCode(); err != nil {
		return err
	}
	if st, err := os.Stat(cfg.Root); err != nil || !st.IsDir() {
		return fmt.Errorf("Project repository does not exist: %s", cfg.Root)
	}
	git, _ := RequireBinary("git")
	_, err := Run(context.Background(), []string{git, "-C", cfg.Root, "rev-parse", "--is-inside-work-tree"}, "", nil, "", true)
	return err
}
func pretty(v any) string { b, _ := json.MarshalIndent(v, "", "  "); return string(b) }
func shortSHA(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func printBlockedDiagnostics(store *Store, cid string) {
	tasks, _ := store.ListTasks(cid)
	active := Rows{}
	for _, t := range tasks {
		if !map[string]bool{TaskComplete: true, TaskCancelled: true, TaskSuperseded: true}[S(t, "status")] {
			active = append(active, t)
		}
	}
	if len(active) == 0 {
		return
	}
	fmt.Fprintln(os.Stderr, "Blocked work:")
	for _, t := range active {
		sum := strings.TrimSpace(S(M(t, "result"), "summary"))
		suffix := ""
		if sum != "" {
			suffix = ": " + sum
		}
		fmt.Fprintf(os.Stderr, "  %s [%s/%s] %s%s\n", S(t, "id"), S(t, "status"), S(t, "profile"), S(t, "title"), suffix)
		a, _ := store.Query("SELECT * FROM attempts WHERE task_id=" + SQLLiteral(S(t, "id")) + " ORDER BY attempt_no DESC LIMIT 1")
		if len(a) > 0 {
			fmt.Fprintf(os.Stderr, "    latest attempt: %s status=%s exit=%s log=%s\n", S(a[0], "id"), S(a[0], "status"), S(a[0], "exit_code"), firstNonEmpty(S(a[0], "log_path"), "-"))
		}
	}
}
func limitsForProject(store *Store, pid string) (Limits, error) {
	p, e := store.GetProject(pid)
	if e != nil {
		return Limits{}, e
	}
	return ProjectConfigFromRow(p).Limits, nil
}
func requireCoordinator(store *Store, explicit string) (Row, error) {
	id, e := currentTaskID(explicit)
	if e != nil {
		return nil, e
	}
	t, e := store.GetTask(id)
	if e != nil {
		return nil, e
	}
	if S(t, "profile") != "coordinator" {
		return nil, fmt.Errorf("This graph mutation is restricted to the coordinator profile")
	}
	return t, nil
}

func cmdInit(args []string) int {
	p, e := parseArgs(args, map[string]bool{"name": true, "base-branch": true, "opencode": true})
	if e != nil {
		return fail(e)
	}
	if len(p.Pos) > 0 {
		return fail(fmt.Errorf("usage: praector init [--name NAME] [--base-branch BRANCH] [--opencode PATH]"))
	}
	svc := &Service{}
	out, e := svc.Init(context.Background(), InitInput{Name: p.str("name"), BaseBranch: p.str("base-branch"), OpenCode: p.str("opencode")})
	if e != nil {
		return fail(e)
	}
	fmt.Printf("Initialized Praector project %s (%s)\nProject marker: %s\nBase branch: %s\nOpenCode: %s\n", out.Name, out.ProjectID, out.Marker, out.BaseBranch, out.OpenCodeCommand)
	return 0
}

func cmdProfile(args []string) int {
	if len(args) == 0 {
		return fail(fmt.Errorf("usage: praector profile {list|show}"))
	}
	svc := &Service{}
	switch args[0] {
	case "list":
		if len(args) != 1 {
			return fail(fmt.Errorf("usage: praector profile list"))
		}
		out, e := svc.ProfileList(context.Background(), ProfileListInput{})
		if e != nil {
			return fail(e)
		}
		for _, pf := range out.Profiles {
			fmt.Printf("%-12s %-9s %s\n", pf.Name, pf.Mode, pf.Title)
		}
		return 0
	case "show":
		if len(args) != 2 {
			return fail(fmt.Errorf("usage: praector profile show PROFILE"))
		}
		out, e := svc.ProfileShow(context.Background(), ProfileShowInput{Profile: args[1]})
		if e != nil {
			return fail(e)
		}
		fmt.Printf("# Inherited global engineering contract\n\n%s\n\n# Role-specific operating contract\n\n%s\n", out.Global, out.Role)
		return 0
	}
	return fail(fmt.Errorf("unknown profile command: %s", args[0]))
}

func cmdProject(args []string) int {
	if len(args) == 0 {
		return fail(fmt.Errorf("usage: praector project {list|show}"))
	}
	svc, e := newService()
	if e != nil {
		return fail(e)
	}
	switch args[0] {
	case "list":
		if len(args) != 1 {
			return fail(fmt.Errorf("usage: praector project list"))
		}
		out, e := svc.ProjectList(context.Background(), ProjectListInput{})
		if e != nil {
			return fail(e)
		}
		for _, r := range out.Projects {
			fmt.Printf("%s  %s  %s\n", S(r, "id"), S(r, "name"), S(r, "root_path"))
		}
		return 0
	case "show":
		p, e := parseArgs(args[1:], map[string]bool{"project": true})
		if e != nil {
			return fail(e)
		}
		if len(p.Pos) > 0 {
			return fail(fmt.Errorf("usage: praector project show [--project ID]"))
		}
		out, e := svc.ProjectShow(context.Background(), ProjectShowInput{ProjectID: p.str("project")})
		if e != nil {
			return fail(e)
		}
		fmt.Println(pretty(out.Project))
		return 0
	}
	return fail(fmt.Errorf("unknown project command: %s", args[0]))
}

func cmdRun(args []string) int {
	p, e := parseArgs(args, map[string]bool{"title": true, "project": true, "detach": false, "max-workers": true})
	if e != nil {
		return fail(e)
	}
	if len(p.Pos) != 1 {
		return fail(fmt.Errorf("usage: praector run OBJECTIVE [--title TITLE] [--project ID] [--detach] [--max-workers N]"))
	}
	mw, e := p.integer("max-workers", 0)
	if e != nil {
		return fail(e)
	}
	svc, e := newService()
	if e != nil {
		return fail(e)
	}
	out, e := svc.Run(context.Background(), RunInput{Objective: p.Pos[0], Title: p.str("title"), ProjectID: p.str("project")})
	if e != nil {
		return fail(e)
	}
	c, coord := out.Collection, out.Coordinator
	fmt.Printf("Created %s in project %s\nObjective: %s\nIntegration branch: %s\nCoordinator task: %s\n", S(c, "id"), out.ProjectName, S(c, "objective"), S(c, "integration_branch"), S(coord, "id"))
	if p.bool("detach") {
		fmt.Println("Queued. Run `praector daemon` to execute pending work.")
		return 0
	}
	sched, e := NewScheduler(svc.store, svc.cfg, mw)
	if e != nil {
		return fail(e)
	}
	state, e := sched.RunCollection(S(c, "id"), 500*time.Millisecond)
	if e != nil {
		return fail(e)
	}
	fmt.Printf("Collection %s: %s\n", S(c, "id"), state)
	if state == "BLOCKED" {
		printBlockedDiagnostics(svc.store, S(c, "id"))
	}
	if state == "COMPLETE" {
		return 0
	}
	return 2
}

func formatCollectionStatus(c CollectionStatus) {
	parts := []string{}
	for _, kv := range c.TaskCounts {
		parts = append(parts, fmt.Sprintf("%s=%d", kv.Key, kv.Count))
	}
	summary := strings.Join(parts, ", ")
	if summary == "" {
		summary = "no tasks (invalid/orphaned; `praector daemon --once` will repair)"
	}
	fmt.Printf("%s  %s  %s\n  objective: %s\n  branch: %s\n", c.ID, c.Status, c.Title, c.Objective, c.Branch)
	if c.Acceptance != nil {
		fmt.Printf("  accepted: %s @ %s (%s, %s)\n", S(c.Acceptance, "target_branch"), shortSHA(S(c.Acceptance, "accepted_commit")), S(c.Acceptance, "merge_mode"), S(c.Acceptance, "created_at"))
	}
	if c.PR != nil {
		draft := ""
		if B(c.PR, "is_draft") {
			draft = " draft"
		}
		fmt.Printf("  pr: %s #%s [%s%s] %s\n", S(c.PR, "provider"), S(c.PR, "pr_number"), S(c.PR, "state"), draft, S(c.PR, "url"))
	}
	if len(c.Revisions) > 0 {
		rp := []string{}
		for _, kv := range sortedCounts(c.Revisions, func(r Row) string { return S(r, "status") }) {
			rp = append(rp, fmt.Sprintf("%s=%d", kv.Key, kv.Count))
		}
		fmt.Println("  revisions: " + strings.Join(rp, ", "))
		for _, r := range c.Revisions {
			if S(r, "status") == "OPEN" {
				fmt.Printf("    %s [OPEN] %s\n", S(r, "id"), S(r, "reason"))
			}
		}
	}
	fmt.Println("  tasks: " + summary)
	for _, t := range c.Active {
		fmt.Printf("    %s [%s/%s] %s\n", S(t, "id"), S(t, "status"), S(t, "profile"), S(t, "title"))
	}
}
func cmdStatus(args []string) int {
	p, e := parseArgs(args, map[string]bool{"project": true})
	if e != nil {
		return fail(e)
	}
	if len(p.Pos) > 1 {
		return fail(fmt.Errorf("usage: praector status [TARGET] [--project ID]"))
	}
	svc, e := newService()
	if e != nil {
		return fail(e)
	}
	target := ""
	if len(p.Pos) > 0 {
		target = p.Pos[0]
	}
	out, e := svc.Status(context.Background(), StatusInput{Target: target, ProjectID: p.str("project")})
	if e != nil {
		return fail(e)
	}
	switch out.Kind {
	case "collection":
		formatCollectionStatus(*out.Collection)
	case "task":
		fmt.Println(pretty(out.Task))
	default:
		fmt.Printf("Project %s (%s)\n", out.ProjectName, out.ProjectID)
		for _, c := range out.Collections {
			formatCollectionStatus(*c)
		}
	}
	return 0
}
func cmdGraph(args []string) int {
	p, e := parseArgs(args, map[string]bool{"project": true})
	if e != nil {
		return fail(e)
	}
	if len(p.Pos) > 1 {
		return fail(fmt.Errorf("usage: praector graph [COLLECTION] [--project ID]"))
	}
	svc, e := newService()
	if e != nil {
		return fail(e)
	}
	coll := ""
	if len(p.Pos) > 0 {
		coll = p.Pos[0]
	}
	out, e := svc.Graph(context.Background(), GraphInput{Collection: coll, ProjectID: p.str("project")})
	if e != nil {
		return fail(e)
	}
	c := out.Collection
	fmt.Printf("%s %s [%s]\n", S(c, "id"), S(c, "title"), S(c, "status"))
	for _, t := range out.Tasks {
		d := strings.Join(out.Deps[S(t, "id")], ",")
		if d == "" {
			d = "-"
		}
		fmt.Printf("  %s [%-10s] %-11s %s  deps=%s\n", S(t, "id"), S(t, "status"), S(t, "profile"), S(t, "title"), d)
	}
	return 0
}
func cmdDaemon(args []string) int {
	p, e := parseArgs(args, map[string]bool{"once": false, "max-workers": true})
	if e != nil {
		return fail(e)
	}
	if len(p.Pos) > 0 {
		return fail(fmt.Errorf("usage: praector daemon [--once] [--max-workers N]"))
	}
	mw, e := p.integer("max-workers", 0)
	if e != nil {
		return fail(e)
	}
	store, cfg, e := newStore()
	if e != nil {
		return fail(e)
	}
	sched, e := NewScheduler(store, cfg, mw)
	if e != nil {
		return fail(e)
	}
	// The MCP listener is a separate, loopback-only HTTP goroutine decoupled from
	// the reconcile loop and from scheduler.lock: it performs only Store reads and
	// durable enqueues on the per-call state lock, and never launches processes or
	// runs a collection. It shares the daemon's already-open store connection.
	if LoadMCPServerConfig(cfg).Enabled {
		svc := &Service{store: store, cfg: cfg}
		mcpSrv, e := startMCPServer(svc)
		if e != nil {
			return fail(e)
		}
		defer mcpSrv.Close()
		go func() {
			if err := mcpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				fmt.Fprintf(os.Stderr, "mcp listener: %v\n", err)
			}
		}()
		fmt.Printf("MCP listener enabled on %s (loopback only)\n", mcpSrv.Addr)
	}
	fmt.Println("praector daemon running (Ctrl-C to stop)")
	if e = sched.Daemon(p.bool("once"), time.Second); e != nil {
		return fail(e)
	}
	return 0
}

func cmdTask(args []string) int {
	if len(args) == 0 {
		return fail(fmt.Errorf("usage: praector task {show|list|retry|request|add-dependency|supersede}"))
	}
	switch args[0] {
	case "show":
		if len(args) != 2 {
			return fail(fmt.Errorf("usage: praector task show TASK"))
		}
		svc, e := newService()
		if e != nil {
			return fail(e)
		}
		out, e := svc.TaskShow(context.Background(), TaskShowInput{TaskID: args[1]})
		if e != nil {
			return fail(e)
		}
		fmt.Println(pretty(out.Task))
		return 0
	case "list":
		p, e := parseArgs(args[1:], map[string]bool{"collection": true, "project": true})
		if e != nil {
			return fail(e)
		}
		svc, e := newService()
		if e != nil {
			return fail(e)
		}
		out, e := svc.TaskList(context.Background(), TaskListInput{Collection: p.str("collection"), ProjectID: p.str("project")})
		if e != nil {
			return fail(e)
		}
		for _, entry := range out.Tasks {
			t := entry.Task
			dt := strings.Join(entry.Deps, ",")
			if dt == "" {
				dt = "-"
			}
			fmt.Printf("%s [%s/%s] %s deps=%s\n", S(t, "id"), S(t, "status"), S(t, "profile"), S(t, "title"), dt)
		}
		return 0
	case "retry":
		return cmdTaskRetry(args[1:])
	case "request":
		return cmdTaskRequest(args[1:])
	case "add-dependency":
		return cmdTaskAddDependency(args[1:])
	case "supersede":
		return cmdTaskSupersede(args[1:])
	}
	return fail(fmt.Errorf("unknown task command: %s", args[0]))
}
func cmdTaskRetry(args []string) int {
	p, e := parseArgs(args, map[string]bool{"reason": true, "detach": false, "max-workers": true})
	if e != nil {
		return fail(e)
	}
	if len(p.Pos) != 1 {
		return fail(fmt.Errorf("usage: praector task retry TASK [--reason TEXT] [--detach] [--max-workers N]"))
	}
	mw, e := p.integer("max-workers", 0)
	if e != nil {
		return fail(e)
	}
	svc, e := newService()
	if e != nil {
		return fail(e)
	}
	out, e := svc.TaskRetry(context.Background(), TaskRetryInput{TaskID: p.Pos[0], Reason: p.str("reason")})
	if e != nil {
		return fail(e)
	}
	replacement, cid := out.Replacement, out.CollectionID
	fmt.Printf("%s -> %s\nCollection %s resumed\n", p.Pos[0], S(replacement, "id"), cid)
	if p.bool("detach") {
		fmt.Println("Queued. Run `praector daemon` to execute pending work.")
		return 0
	}
	sched, e := NewScheduler(svc.store, svc.cfg, mw)
	if e != nil {
		return fail(e)
	}
	state, e := sched.RunCollection(cid, 500*time.Millisecond)
	if e != nil {
		return fail(e)
	}
	fmt.Printf("Collection %s: %s\n", cid, state)
	if state == "BLOCKED" {
		printBlockedDiagnostics(svc.store, cid)
	}
	if state == "COMPLETE" {
		return 0
	}
	return 2
}
func cmdTaskAddDependency(args []string) int {
	p, e := parseArgs(args, map[string]bool{"on": true, "caller": true})
	if e != nil {
		return fail(e)
	}
	if len(p.Pos) != 1 || p.str("on") == "" {
		return fail(fmt.Errorf("usage: praector task add-dependency TASK --on DEPENDENCY [--caller TASK]"))
	}
	svc, e := newService()
	if e != nil {
		return fail(e)
	}
	out, e := svc.TaskAddDependency(context.Background(), TaskAddDependencyInput{TaskID: p.Pos[0], OnID: p.str("on"), Caller: p.str("caller")})
	if e != nil {
		return fail(e)
	}
	fmt.Printf("%s depends_on %s\n", S(out.Task, "id"), S(out.Dep, "id"))
	return 0
}
func cmdTaskSupersede(args []string) int {
	p, e := parseArgs(args, map[string]bool{"with": true, "reason": true, "caller": true})
	if e != nil {
		return fail(e)
	}
	if len(p.Pos) != 1 || p.str("with") == "" || p.str("reason") == "" {
		return fail(fmt.Errorf("usage: praector task supersede OLD --with NEW --reason TEXT [--caller TASK]"))
	}
	svc, e := newService()
	if e != nil {
		return fail(e)
	}
	out, e := svc.TaskSupersede(context.Background(), TaskSupersedeInput{OldID: p.Pos[0], NewID: p.str("with"), Reason: p.str("reason"), Caller: p.str("caller")})
	if e != nil {
		return fail(e)
	}
	fmt.Printf("%s -> %s\n", S(out.Old, "id"), S(out.New, "id"))
	return 0
}
func cmdTaskRequest(args []string) int {
	p, e := parseArgs(args, map[string]bool{"task": true, "profile": true, "title": true, "objective": true, "reason": true, "blocking": false, "depends-on": true, "workspace": true})
	if e != nil {
		return fail(e)
	}
	if len(p.Pos) > 0 || p.str("profile") == "" || p.str("title") == "" || p.str("objective") == "" || p.str("reason") == "" {
		return fail(fmt.Errorf("usage: praector task request --profile PROFILE --title TITLE --objective TEXT --reason TEXT [--blocking] [--depends-on TASK] [--workspace MODE]"))
	}
	if p.str("profile") == "coordinator" {
		return fail(fmt.Errorf("--profile coordinator is not valid for task request; use coordinator attention"))
	}
	svc, e := newService()
	if e != nil {
		return fail(e)
	}
	out, e := svc.TaskRequest(context.Background(), TaskRequestInput{CallerID: p.str("task"), Profile: p.str("profile"), Title: p.str("title"), Objective: p.str("objective"), Reason: p.str("reason"), Blocking: p.bool("blocking"), DependsOn: p.strs("depends-on"), Workspace: p.str("workspace")})
	if e != nil {
		return fail(e)
	}
	fmt.Println(S(out.Task, "id"))
	if !out.Created {
		fmt.Fprintln(os.Stderr, "deduplicated")
	}
	return 0
}

func cmdContext(args []string) int {
	if len(args) == 0 || args[0] != "show" {
		return fail(fmt.Errorf("usage: praector context show [--task TASK]"))
	}
	p, e := parseArgs(args[1:], map[string]bool{"task": true})
	if e != nil {
		return fail(e)
	}
	svc, e := newService()
	if e != nil {
		return fail(e)
	}
	out, e := svc.ContextShow(context.Background(), ContextShowInput{TaskID: p.str("task")})
	if e != nil {
		return fail(e)
	}
	fmt.Println(pretty(out.Payload))
	return 0
}
func cmdCoordinator(args []string) int {
	if len(args) == 0 || args[0] != "attention" {
		return fail(fmt.Errorf("usage: praector coordinator attention --reason TEXT [--task TASK] [--blocking]"))
	}
	p, e := parseArgs(args[1:], map[string]bool{"task": true, "reason": true, "blocking": false})
	if e != nil {
		return fail(e)
	}
	if p.str("reason") == "" {
		return fail(fmt.Errorf("--reason is required"))
	}
	svc, e := newService()
	if e != nil {
		return fail(e)
	}
	out, e := svc.CoordinatorAttention(context.Background(), CoordinatorAttentionInput{TaskID: p.str("task"), Reason: p.str("reason"), Blocking: p.bool("blocking")})
	if e != nil {
		return fail(e)
	}
	fmt.Println(S(out.Task, "id"))
	return 0
}

func cmdResult(args []string) int {
	if len(args) == 0 {
		return fail(fmt.Errorf("usage: praector result {show|submit}"))
	}
	svc, e := newService()
	if e != nil {
		return fail(e)
	}
	switch args[0] {
	case "show":
		if len(args) > 2 {
			return fail(fmt.Errorf("usage: praector result show [TASK]"))
		}
		explicit := ""
		if len(args) == 2 {
			explicit = args[1]
		}
		out, e := svc.ResultShow(context.Background(), ResultShowInput{TaskID: explicit})
		if e != nil {
			return fail(e)
		}
		fmt.Println(pretty(out.Result))
		return 0
	case "submit":
		p, e := parseArgs(args[1:], map[string]bool{"task": true, "stdin": false, "file": true, "summary": true, "status": true})
		if e != nil {
			return fail(e)
		}
		tid, e := currentTaskID(p.str("task"))
		if e != nil {
			return fail(e)
		}
		status := p.str("status")
		if status == "" {
			status = "complete"
		}
		var result Row
		if p.bool("stdin") {
			b, e := io.ReadAll(os.Stdin)
			if e != nil {
				return fail(e)
			}
			if e = json.Unmarshal(b, &result); e != nil {
				return fail(e)
			}
		} else if file := p.str("file"); file != "" {
			b, e := os.ReadFile(file)
			if e != nil {
				return fail(e)
			}
			if e = json.Unmarshal(b, &result); e != nil {
				return fail(e)
			}
		} else if sum := p.str("summary"); sum != "" {
			result = Row{"status": status, "summary": sum}
		} else {
			return fail(fmt.Errorf("Use --stdin, --file, or --summary"))
		}
		if S(result, "status") == "" {
			result["status"] = status
		}
		out, e := svc.ResultSubmit(context.Background(), ResultSubmitInput{TaskID: tid, Result: result})
		if e != nil {
			return fail(e)
		}
		fmt.Println(out.Status)
		return 0
	}
	return fail(fmt.Errorf("unknown result command: %s", args[0]))
}

func cmdMemory(args []string) int {
	if len(args) == 0 {
		return fail(fmt.Errorf("usage: praector memory {add|list}"))
	}
	svc, e := newService()
	if e != nil {
		return fail(e)
	}
	switch args[0] {
	case "add":
		p, e := parseArgs(args[1:], map[string]bool{"project": true, "collection": true, "task": true, "scope": true, "state": true, "kind": true, "text": true})
		if e != nil {
			return fail(e)
		}
		if p.str("text") == "" {
			return fail(fmt.Errorf("--text is required"))
		}
		out, e := svc.MemoryAdd(context.Background(), MemoryAddInput{ProjectID: p.str("project"), CollectionID: p.str("collection"), TaskID: p.str("task"), Scope: p.str("scope"), State: p.str("state"), Kind: p.str("kind"), Text: p.str("text")})
		if e != nil {
			return fail(e)
		}
		fmt.Println(S(out.Memory, "id"))
		return 0
	case "list":
		p, e := parseArgs(args[1:], map[string]bool{"project": true, "collection": true, "state": true})
		if e != nil {
			return fail(e)
		}
		out, e := svc.MemoryList(context.Background(), MemoryListInput{ProjectID: p.str("project"), CollectionID: p.str("collection"), State: p.str("state")})
		if e != nil {
			return fail(e)
		}
		for _, m := range out.Memories {
			fmt.Printf("%s [%s/%s/%s] %s\n", S(m, "id"), S(m, "scope"), S(m, "state"), S(m, "kind"), S(m, "text"))
		}
		return 0
	}
	return fail(fmt.Errorf("unknown memory command: %s", args[0]))
}
func cmdDecision(args []string) int {
	if len(args) == 0 || args[0] != "add" {
		return fail(fmt.Errorf("usage: praector decision add --statement TEXT [--rationale TEXT]"))
	}
	p, e := parseArgs(args[1:], map[string]bool{"project": true, "collection": true, "task": true, "statement": true, "rationale": true})
	if e != nil {
		return fail(e)
	}
	if p.str("statement") == "" {
		return fail(fmt.Errorf("--statement is required"))
	}
	svc, e := newService()
	if e != nil {
		return fail(e)
	}
	out, e := svc.DecisionAdd(context.Background(), DecisionAddInput{ProjectID: p.str("project"), CollectionID: p.str("collection"), TaskID: p.str("task"), Statement: p.str("statement"), Rationale: p.str("rationale")})
	if e != nil {
		return fail(e)
	}
	fmt.Println(S(out.Decision, "id"))
	return 0
}

// validateCollectionForAcceptance applies every durable precondition of
// acceptance. derivedCandidates is the seam-computed diff-derived config-knob
// candidate list (base...collection integration branch at the moment of the
// check); callers that can reach the Git workspace compute it and callers
// without Git access pass nil, which preserves the historical behavior. If
// integration introduces a NEW candidate after a review already passed at
// submit, acceptance recomputes the list and correctly requires it: that is a
// genuine new knob, not spurious diff drift.
func validateCollectionForAcceptance(store *Store, c Row, derivedCandidates ...DerivedConfigCandidate) (Row, Rows, error) {
	state, e := store.UpdateCollectionTerminalState(S(c, "id"))
	if e != nil {
		return nil, nil, e
	}
	c, e = store.GetCollection(S(c, "id"))
	if e != nil {
		return nil, nil, e
	}
	critic, _ := store.CriticRevalidationState(S(c, "id"))
	policy := ClassifyPlanningPolicy(S(c, "objective"))
	if state != TaskComplete && state != "COMPLETE" {
		if policy.CriticRequired && B(critic, "stale") {
			return nil, nil, fmt.Errorf("Collection %s has a stale required critic: sealed material-plan fingerprint %s != current %s. Re-run orchestration to complete critic revalidation before acceptance.", S(c, "id"), S(critic, "sealed_fingerprint"), S(critic, "current_material_plan_fingerprint"))
		}
		return nil, nil, fmt.Errorf("Collection %s is %s, not COMPLETE", S(c, "id"), state)
	}
	if policy.CriticRequired && B(critic, "stale") {
		return nil, nil, fmt.Errorf("Collection %s has a stale required critic; acceptance requires a fresh critic seal.", S(c, "id"))
	}
	tasks, e := store.ListTasks(S(c, "id"))
	if e != nil {
		return nil, nil, e
	}
	edges, e := store.ListEdges(S(c, "id"))
	if e != nil {
		return nil, nil, e
	}
	by := map[string]Row{}
	for _, t := range tasks {
		by[S(t, "id")] = t
		if S(t, "profile") == "review" && S(t, "status") == TaskComplete {
			v := strings.ToLower(strings.TrimSpace(S(M(t, "result"), "verdict")))
			if v != "pass" && v != "fail" {
				if v == "" {
					v = "<missing>"
				}
				return nil, nil, fmt.Errorf("Review %s is COMPLETE but has verdict=%s; acceptance requires an explicit pass/fail verdict", S(t, "id"), v)
			}
		}
	}
	verifies := map[string][]string{}
	for _, ed := range edges {
		if S(ed, "relation") == "verifies" {
			verifies[S(ed, "from_task_id")] = append(verifies[S(ed, "from_task_id")], S(ed, "to_task_id"))
		}
	}
	legacyDecisions, e := store.ListDecisions(S(c, "project_id"), S(c, "id"))
	if e != nil {
		return nil, nil, e
	}
	legacyIDs := LegacyGateTraceabilityExceptionIDs(legacyDecisions, S(c, "id"))
	for _, t := range tasks {
		if S(t, "profile") != "review" && S(t, "profile") != "plan_review" {
			continue
		}
		if S(t, "status") != TaskComplete || strings.ToLower(strings.TrimSpace(S(M(t, "result"), "verdict"))) != "pass" {
			continue
		}
		if ancestors, e := store.DirectiveAncestors(S(t, "id")); e == nil {
			if directives := InheritedDirectives(ancestors); len(directives) > 0 {
				if e := ValidateDirectiveTraceability(M(t, "result"), directives, true, legacyIDs); e != nil {
					return nil, nil, fmt.Errorf("Collection %s cannot be accepted: %w", S(c, "id"), e)
				}
			}
		} else {
			return nil, nil, e
		}
		if S(t, "profile") == "review" {
			if e := ValidateReviewImpactEnumeration(M(t, "result"), legacyIDs, derivedCandidates); e != nil {
				return nil, nil, fmt.Errorf("Collection %s cannot be accepted: %w", S(c, "id"), e)
			}
			if decisionIDs := CandidateDecisionIDs(M(t, "result")); len(decisionIDs) > 0 {
				known := map[string]bool{}
				for _, decision := range legacyDecisions {
					known[S(decision, "id")] = true
				}
				for _, decisionID := range decisionIDs {
					if !known[decisionID] {
						return nil, nil, fmt.Errorf("Collection %s cannot be accepted: candidate disposition decision_id %s is not a durable decision in this Collection", S(c, "id"), decisionID)
					}
				}
			}
		}
	}
	for _, t := range tasks {
		if S(t, "profile") != "implement" || S(t, "status") != TaskComplete {
			continue
		}
		cand := append([]string{}, verifies[S(t, "id")]...)
		pid := S(t, "parent_task_id")
		if pid != "" && S(by[pid], "profile") == "review" && strings.ToLower(strings.TrimSpace(S(M(by[pid], "result"), "verdict"))) == "pass" {
			cand = append(cand, pid)
		}
		passed := false
		for _, rid := range cand {
			r := by[rid]
			if r != nil && S(r, "status") == TaskComplete && strings.ToLower(S(M(r, "result"), "verdict")) == "pass" {
				passed = true
				break
			}
		}
		if !passed {
			return nil, nil, fmt.Errorf("Implementation %s has no completed independent review with verdict=pass", S(t, "id"))
		}
	}
	return c, tasks, nil
}

func cmdCollection(args []string) int {
	if len(args) == 0 {
		return fail(fmt.Errorf("usage: praector collection {show|revise|diff|pr|accept|cleanup}"))
	}
	switch args[0] {
	case "show":
		return cmdCollectionShow(args[1:])
	case "revise":
		return cmdCollectionRevise(args[1:])
	case "diff":
		return cmdCollectionDiff(args[1:])
	case "pr":
		return cmdCollectionPR(args[1:])
	case "accept":
		return cmdCollectionAccept(args[1:])
	case "cleanup":
		return cmdCollectionCleanup(args[1:])
	}
	return fail(fmt.Errorf("unknown collection command: %s", args[0]))
}
func cmdCollectionShow(args []string) int {
	if len(args) != 1 {
		return fail(fmt.Errorf("usage: praector collection show COL-ID"))
	}
	svc, e := newService()
	if e != nil {
		return fail(e)
	}
	out, e := svc.CollectionShow(context.Background(), CollectionShowInput{CollectionID: args[0]})
	if e != nil {
		return fail(e)
	}
	fmt.Println(pretty(out.Collection))
	return 0
}
func cmdCollectionRevise(args []string) int {
	p, e := parseArgs(args, map[string]bool{"reason": true, "detach": false, "max-workers": true})
	if e != nil {
		return fail(e)
	}
	if len(p.Pos) != 1 || p.str("reason") == "" {
		return fail(fmt.Errorf("usage: praector collection revise COL-ID --reason TEXT [--detach] [--max-workers N]"))
	}
	mw, e := p.integer("max-workers", 0)
	if e != nil {
		return fail(e)
	}
	svc, e := newService()
	if e != nil {
		return fail(e)
	}
	out, e := svc.CollectionRevise(context.Background(), CollectionReviseInput{CollectionID: p.Pos[0], Reason: p.str("reason")})
	if e != nil {
		return fail(e)
	}
	rev, coord, c := out.Revision, out.Coordinator, out.Collection
	fmt.Printf("Revision %s opened for %s\nCoordinator task: %s\nReason: %s\n", S(rev, "id"), S(c, "id"), S(coord, "id"), S(rev, "reason"))
	if p.bool("detach") {
		fmt.Println("Queued. Run `praector daemon` to execute revision work.")
		return 0
	}
	sched, e := NewScheduler(svc.store, svc.cfg, mw)
	if e != nil {
		return fail(e)
	}
	state, e := sched.RunCollection(S(c, "id"), 500*time.Millisecond)
	if e != nil {
		return fail(e)
	}
	fmt.Printf("Collection %s: %s\n", S(c, "id"), state)
	if state == "BLOCKED" {
		printBlockedDiagnostics(svc.store, S(c, "id"))
	}
	if state == "COMPLETE" {
		return 0
	}
	return 2
}
func cmdCollectionDiff(args []string) int {
	p, e := parseArgs(args, map[string]bool{"target": true, "stat": false})
	if e != nil {
		return fail(e)
	}
	if len(p.Pos) != 1 {
		return fail(fmt.Errorf("usage: praector collection diff COL-ID [--target BRANCH] [--stat]"))
	}
	svc, e := newService()
	if e != nil {
		return fail(e)
	}
	out, e := svc.CollectionDiff(context.Background(), CollectionDiffInput{CollectionID: p.Pos[0], Target: p.str("target"), Stat: p.bool("stat")})
	if e != nil {
		return fail(e)
	}
	fmt.Print(out.Stdout)
	return 0
}
func resolvePRRemote(repo, base, explicit string) (string, error) {
	git, e := RequireBinary("git")
	if e != nil {
		return "", e
	}
	r, e := Run(context.Background(), []string{git, "-C", repo, "remote"}, "", nil, "", true)
	if e != nil {
		return "", e
	}
	remotes := strings.Fields(r.Stdout)
	has := func(x string) bool {
		for _, v := range remotes {
			if v == x {
				return true
			}
		}
		return false
	}
	if explicit != "" {
		if !has(explicit) {
			return "", fmt.Errorf("Git remote not found: %s", explicit)
		}
		return explicit, nil
	}
	r, _ = Run(context.Background(), []string{git, "-C", repo, "config", "--get", "branch." + base + ".remote"}, "", nil, "", false)
	configured := strings.TrimSpace(r.Stdout)
	if configured != "" && configured != "." && has(configured) {
		return configured, nil
	}
	if has("origin") {
		return "origin", nil
	}
	if len(remotes) == 1 {
		return remotes[0], nil
	}
	if len(remotes) == 0 {
		return "", fmt.Errorf("Repository has no Git remote. Add a GitHub remote before creating a PR.")
	}
	return "", fmt.Errorf("Unable to choose a Git remote. Pass --remote explicitly.")
}
func printPRPlan(c Row, plan Row, base, remote, title, body string, draft bool, existing Row) {
	fmt.Printf("Collection: %s  %s\nHead:       %s @ %s\nBase:       %s\nRemote:     %s\n", S(c, "id"), S(c, "title"), S(plan, "source_branch"), shortSHA(S(plan, "source_commit")), base, remote)
	if existing != nil {
		fmt.Printf("Existing:   %s #%s [%s] %s\n", S(existing, "provider"), S(existing, "pr_number"), S(existing, "state"), S(existing, "url"))
	} else {
		d := "no"
		if draft {
			d = "yes"
		}
		fmt.Println("Draft:      " + d)
	}
	fmt.Println("Title:      " + title)
	fmt.Println("Body:")
	fmt.Println(strings.TrimRight(body, "\n"))
}
func cmdCollectionPR(args []string) int {
	p, e := parseArgs(args, map[string]bool{"draft": false, "title": true, "base": true, "remote": true, "dry-run": false, "refresh-copy": false, "open": false})
	if e != nil {
		return fail(e)
	}
	if len(p.Pos) != 1 {
		return fail(fmt.Errorf("usage: praector collection pr COL-ID [--draft] [--title TITLE] [--base BRANCH] [--remote REMOTE] [--dry-run] [--refresh-copy] [--open]"))
	}
	svc, e := newService()
	if e != nil {
		return fail(e)
	}
	store := svc.store
	planOut, e := svc.CollectionPRPlan(context.Background(), CollectionPRPlanInput{CollectionID: p.Pos[0], Base: p.str("base"), Title: p.str("title"), Remote: p.str("remote"), Draft: p.bool("draft")})
	if e != nil {
		return fail(e)
	}
	c, project, existing, plan := planOut.Collection, planOut.Project, planOut.Existing, planOut.Plan
	base, remote, title, body := planOut.Base, planOut.Remote, planOut.Title, planOut.Body
	repo, _ := filepath.Abs(S(project, "root_path"))
	if p.bool("dry-run") {
		acc, _ := store.GetCollectionAcceptance(S(c, "id"), "")
		if existing == nil && (acc != nil || B(plan, "already_contained")) {
			return fail(fmt.Errorf("Collection branch %s is already contained in %s; there is no new candidate diff from which to create a PR.", S(plan, "source_branch"), base))
		}
		printPRPlan(c, plan, base, remote, title, body, p.bool("draft"), existing)
		fmt.Println("Dry run: no branch was pushed and no pull request was created or changed.")
		return 0
	}
	provider, e := NewGitHubCLIProvider(repo)
	if e != nil {
		return fail(e)
	}
	repository, e := provider.Repository()
	if e != nil {
		return fail(e)
	}
	actor := acceptActor(project)
	var remotePR *PullRequest
	if existing != nil {
		remotePR, e = provider.View(I(existing, "pr_number"))
		if e != nil {
			return fail(e)
		}
		if remotePR.HeadBranch != S(plan, "source_branch") {
			return fail(fmt.Errorf("Recorded PR #%d uses head %s, not collection branch %s", remotePR.Number, remotePR.HeadBranch, S(plan, "source_branch")))
		}
		if remotePR.BaseBranch != base {
			return fail(fmt.Errorf("Recorded PR #%d targets %s, not %s. Rerun with --base %s to use the existing PR.", remotePR.Number, remotePR.BaseBranch, base, remotePR.BaseBranch))
		}
		if p.str("title") != "" && p.str("title") != remotePR.Title {
			return fail(fmt.Errorf("--title is creation-only for an existing PR; edit the PR in GitHub or omit --title."))
		}
		if p.bool("draft") && !remotePR.IsDraft {
			return fail(fmt.Errorf("--draft is creation-only and the existing PR is already ready for review."))
		}
	} else {
		remotePR, e = provider.FindForHead(S(plan, "source_branch"))
		if e != nil {
			return fail(e)
		}
		if remotePR != nil && remotePR.BaseBranch != base {
			return fail(fmt.Errorf("Existing GitHub PR #%d for %s targets %s, not %s. Rerun with --base %s to adopt it.", remotePR.Number, S(plan, "source_branch"), remotePR.BaseBranch, base, remotePR.BaseBranch))
		}
	}
	recordPR := func(pr *PullRequest) (Row, error) {
		return store.RecordCollectionPullRequest(PRRecord{ProjectID: S(project, "id"), CollectionID: S(c, "id"), Provider: "github", RemoteName: remote, Repository: repository["name"], Number: pr.Number, URL: pr.URL, State: pr.State, IsDraft: pr.IsDraft, BaseBranch: pr.BaseBranch, HeadBranch: pr.HeadBranch, SourceCommit: S(plan, "source_commit"), Title: pr.Title, Actor: actor})
	}
	if remotePR != nil && remotePR.State != "OPEN" {
		if existing != nil && S(existing, "source_commit") != S(plan, "source_commit") {
			return fail(fmt.Errorf("Existing PR #%d is %s but the collection branch has changed from %s to %s. Create a new collection for follow-up work rather than reusing a closed/merged PR.", remotePR.Number, remotePR.State, shortSHA(S(existing, "source_commit")), shortSHA(S(plan, "source_commit"))))
		}
		rec, e := recordPR(remotePR)
		if e != nil {
			return fail(e)
		}
		fmt.Printf("PR already exists: #%s [%s]\n%s\n", S(rec, "pr_number"), S(rec, "state"), S(rec, "url"))
		if p.bool("open") {
			_ = provider.Open(I(rec, "pr_number"))
		}
		return 0
	}
	if remotePR == nil {
		acc, _ := store.GetCollectionAcceptance(S(c, "id"), "")
		if acc != nil {
			return fail(fmt.Errorf("Collection has already been accepted directly; refusing to create a redundant PR."))
		}
		if B(plan, "already_contained") {
			return fail(fmt.Errorf("Collection branch %s is already contained in %s; GitHub would have no new diff to review.", S(plan, "source_branch"), base))
		}
	}
	git, _ := RequireBinary("git")
	_, e = Run(context.Background(), []string{git, "-C", repo, "push", "--set-upstream", remote, S(plan, "source_branch") + ":" + S(plan, "source_branch")}, "", nil, "", true)
	if e != nil {
		return fail(e)
	}
	created := false
	if remotePR == nil {
		remotePR, e = provider.Create(base, S(plan, "source_branch"), title, body, p.bool("draft"))
		created = true
	} else if p.bool("refresh-copy") {
		remotePR, e = provider.UpdateBody(remotePR.Number, body)
	} else {
		remotePR, e = provider.View(remotePR.Number)
	}
	if e != nil {
		return fail(e)
	}
	rec, e := recordPR(remotePR)
	if e != nil {
		return fail(e)
	}
	verb := "already exists"
	if created {
		verb = "created"
	}
	draft := ""
	if B(rec, "is_draft") {
		draft = " draft"
	}
	fmt.Printf("PR #%s %s: [%s%s]\n%s\n", S(rec, "pr_number"), verb, S(rec, "state"), draft, S(rec, "url"))
	if p.bool("open") {
		_ = provider.Open(I(rec, "pr_number"))
	}
	return 0
}
func cmdCollectionAccept(args []string) int {
	p, e := parseArgs(args, map[string]bool{"target": true, "no-ff": false, "dry-run": false, "cleanup": false, "delete-branches": false})
	if e != nil {
		return fail(e)
	}
	if len(p.Pos) != 1 {
		return fail(fmt.Errorf("usage: praector collection accept COL-ID [--target BRANCH] [--no-ff] [--dry-run] [--cleanup] [--delete-branches]"))
	}
	if p.bool("delete-branches") && !p.bool("cleanup") {
		return fail(fmt.Errorf("--delete-branches requires --cleanup"))
	}
	svc, e := newService()
	if e != nil {
		return fail(e)
	}
	store := svc.store
	accOut, e := svc.CollectionAccept(context.Background(), CollectionAcceptInput{CollectionID: p.Pos[0], Target: p.str("target")})
	if e != nil {
		return fail(e)
	}
	c, project, target, existing, plan := accOut.Collection, accOut.Project, accOut.Target, accOut.Existing, accOut.Plan
	w, e := NewGitWorkspace(store, svc.cfg)
	if e != nil {
		return fail(e)
	}
	fmt.Printf("Collection: %s  %s\nSource:     %s @ %s\nTarget:     %s @ %s\n", S(c, "id"), S(c, "title"), S(plan, "source_branch"), shortSHA(S(plan, "source_commit")), S(plan, "target_branch"), shortSHA(S(plan, "target_commit")))
	if stat := S(plan, "diff_stat"); stat != "" {
		fmt.Println("Changes:")
		for _, l := range strings.Split(strings.TrimRight(stat, "\n"), "\n") {
			fmt.Println("  " + l)
		}
	} else {
		fmt.Println("Changes:    none relative to target")
	}
	if existing != nil {
		if S(existing, "source_commit") != S(plan, "source_commit") {
			return fail(fmt.Errorf("Collection was previously accepted from %s, but its branch now points to %s. Refusing to silently re-accept mutated completed work.", shortSHA(S(existing, "source_commit")), shortSHA(S(plan, "source_commit"))))
		}
		if !B(plan, "already_contained") {
			return fail(fmt.Errorf("Acceptance record exists for %s, but the target no longer contains the accepted source commit", target))
		}
		fmt.Printf("Already accepted into %s at %s (%s).\n", target, shortSHA(S(existing, "accepted_commit")), S(existing, "merge_mode"))
		return 0
	}
	if p.bool("dry-run") {
		mode := "ff-only"
		if B(plan, "already_contained") {
			mode = "already-contained"
		} else if p.bool("no-ff") {
			mode = "no-ff"
		}
		if mode == "ff-only" && !B(plan, "fast_forward") {
			return fail(fmt.Errorf("Target %s has diverged; dry-run shows that --ff-only acceptance would fail. Use --no-ff only after reviewing the divergence.", target))
		}
		fmt.Printf("Dry run: acceptance is valid; merge mode would be %s.\n", mode)
		return 0
	}
	merge, e := w.AcceptCollection(project, c, target, p.bool("no-ff"))
	if e != nil {
		return fail(e)
	}
	acc, e := store.RecordCollectionAcceptance(S(project, "id"), S(c, "id"), S(merge, "target_branch"), S(merge, "source_branch"), S(merge, "source_commit"), S(merge, "target_before_commit"), S(merge, "accepted_commit"), S(merge, "merge_mode"), acceptActor(project))
	if e != nil {
		return fail(e)
	}
	fmt.Printf("Accepted %s into %s at %s (%s).\n", S(c, "id"), S(acc, "target_branch"), shortSHA(S(acc, "accepted_commit")), S(acc, "merge_mode"))
	if p.bool("cleanup") {
		cl, e := w.CleanupCollection(project, c, p.bool("delete-branches"))
		if e != nil {
			return fail(e)
		}
		fmt.Printf("Cleaned worktrees: %d\n", len(Slice(cl, "worktrees")))
		if p.bool("delete-branches") {
			fmt.Printf("Deleted merged branches: %d\n", len(Slice(cl, "branches")))
		}
	}
	return 0
}
func cmdCollectionCleanup(args []string) int {
	p, e := parseArgs(args, map[string]bool{"delete-branches": false, "force": false})
	if e != nil {
		return fail(e)
	}
	if len(p.Pos) != 1 {
		return fail(fmt.Errorf("usage: praector collection cleanup COL-ID [--delete-branches] [--force]"))
	}
	svc, e := newService()
	if e != nil {
		return fail(e)
	}
	out, e := svc.CollectionCleanup(context.Background(), CollectionCleanupInput{CollectionID: p.Pos[0], DeleteBranches: p.bool("delete-branches"), Force: p.bool("force")})
	if e != nil {
		return fail(e)
	}
	for _, x := range out.Worktrees {
		fmt.Printf("removed worktree %v\n", x)
	}
	for _, x := range out.Branches {
		fmt.Printf("deleted branch %v\n", x)
	}
	if len(out.Worktrees) == 0 && len(out.Branches) == 0 {
		fmt.Println("Nothing to clean.")
	}
	return 0
}

func tailFile(path string, n int, prefix string) {
	if path == "" || n <= 0 {
		return
	}
	f, e := os.Open(path)
	if e != nil {
		fmt.Printf("%s[log file missing: %s]\n", prefix, path)
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	lines := []string{}
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	start := 0
	if len(lines) > n {
		start = len(lines) - n
	}
	fmt.Printf("%s--- last %d log lines: %s ---\n", prefix, len(lines)-start, path)
	for _, l := range lines[start:] {
		fmt.Println(prefix + l)
	}
}
func printTaskLog(tl TaskLog, heading bool, tail int) int {
	rows := tl.Attempts
	prefix := ""
	if heading {
		fmt.Printf("%s [%s/%s] %s\n", tl.ID, tl.Status, tl.Profile, tl.Title)
		prefix = "  "
	}
	if len(rows) == 0 {
		if heading {
			fmt.Println("  no attempts")
		} else {
			fmt.Println("no attempts")
		}
		return 0
	}
	for _, r := range rows {
		fmt.Printf("%s%s attempt=%s status=%s exit=%s log=%s\n", prefix, S(r, "id"), S(r, "attempt_no"), S(r, "status"), S(r, "exit_code"), firstNonEmpty(S(r, "log_path"), "-"))
	}
	if tail > 0 {
		tailFile(S(rows[len(rows)-1], "log_path"), tail, prefix)
	}
	return len(rows)
}
func cmdLogs(args []string) int {
	p, e := parseArgs(args, map[string]bool{"tail": true})
	if e != nil {
		return fail(e)
	}
	if len(p.Pos) != 1 {
		return fail(fmt.Errorf("usage: praector logs TARGET [--tail N]"))
	}
	tail, e := p.integer("tail", 0)
	if e != nil {
		return fail(e)
	}
	svc, e := newService()
	if e != nil {
		return fail(e)
	}
	out, e := svc.Logs(context.Background(), LogsInput{Target: p.Pos[0]})
	if e != nil {
		return fail(e)
	}
	if out.Kind == "collection" {
		fmt.Printf("%s %s [%s]\n", out.CollectionID, out.Title, out.Status)
		if out.NoTasks {
			fmt.Println("  no tasks")
			return 0
		}
		total := 0
		for _, tl := range out.TaskLogs {
			total += printTaskLog(tl, true, tail)
		}
		if total == 0 {
			fmt.Println("No task attempts have been recorded for this collection.")
		}
		return 0
	}
	printTaskLog(out.TaskLogs[0], false, tail)
	return 0
}
func cmdDoctor(args []string) int {
	if len(args) > 0 {
		return fail(fmt.Errorf("usage: praector doctor"))
	}
	svc := &Service{}
	out, e := svc.Doctor(context.Background(), DoctorInput{})
	if e != nil {
		return fail(e)
	}
	for _, c := range out.Checks {
		switch c.Status {
		case "ok":
			fmt.Printf("✓ %s\n", c.Text)
		case "fail":
			fmt.Printf("✗ %s\n", c.Text)
		case "warn":
			fmt.Printf("⚠ %s\n", c.Text)
		default:
			fmt.Println("  " + c.Text)
		}
	}
	if out.Failures > 0 {
		return 1
	}
	return 0
}

func fail(err error) int { fmt.Fprintln(os.Stderr, "praector:", err); return 1 }
func printHelp() {
	fmt.Print(`Durable, policy-gated agent orchestration

Usage:
  praector <command> [options]

Commands:
  init        Register the current Git repository as a Praector project
  run         Create a collection and execute its evolving work graph
  status      Show project, collection, or task status
  graph       Show the current durable task graph
  daemon      Run the durable task scheduler
  doctor      Validate local dependencies and project registration
  logs        List attempt logs for a task or collection
  collection  Collection lifecycle operations
  profile     Inspect built-in runtime profile contracts
  project     Project operations
  task        Task operations
  context     Durable context operations
  coordinator Coordinator escalation
  result      Structured task results
  memory      Project/collection memory
  decision    Durable decisions

Use praector --version for the native build version.
`)
}

func Main(args []string) int {
	if maybePrintCommandHelp(args) {
		return 0
	}
	if len(args) == 0 {
		printHelp()
		return 1
	}
	if args[0] == "--version" || args[0] == "version" {
		fmt.Println("praector " + Version)
		return 0
	}
	if args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		printHelp()
		return 0
	}
	switch args[0] {
	case "init":
		return cmdInit(args[1:])
	case "profile":
		return cmdProfile(args[1:])
	case "project":
		return cmdProject(args[1:])
	case "run":
		return cmdRun(args[1:])
	case "status":
		return cmdStatus(args[1:])
	case "graph":
		return cmdGraph(args[1:])
	case "daemon":
		return cmdDaemon(args[1:])
	case "task":
		return cmdTask(args[1:])
	case "context":
		return cmdContext(args[1:])
	case "coordinator":
		return cmdCoordinator(args[1:])
	case "result":
		return cmdResult(args[1:])
	case "memory":
		return cmdMemory(args[1:])
	case "decision":
		return cmdDecision(args[1:])
	case "collection":
		return cmdCollection(args[1:])
	case "logs":
		return cmdLogs(args[1:])
	case "doctor":
		return cmdDoctor(args[1:])
	}
	return fail(fmt.Errorf("unknown command: %s", args[0]))
}
