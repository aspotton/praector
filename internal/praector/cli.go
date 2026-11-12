package praector

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/datascape/praector/internal/assets"
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
	git, e := RequireBinary("git")
	if e != nil {
		return fail(e)
	}
	if _, e = RequireBinary("dolt"); e != nil {
		return fail(e)
	}
	r, e := Run(context.Background(), []string{git, "rev-parse", "--show-toplevel"}, "", nil, "", true)
	if e != nil {
		return fail(e)
	}
	root, _ := filepath.Abs(strings.TrimSpace(r.Stdout))
	marker := filepath.Join(root, ".praector", "project.toml")
	var cfg ProjectConfig
	if _, e = os.Stat(marker); e == nil {
		cfg, e = ReadProjectMarker(marker)
		if e != nil {
			return fail(e)
		}
	} else {
		br := p.str("base-branch")
		if br == "" {
			q, _ := Run(context.Background(), []string{git, "-C", root, "branch", "--show-current"}, "", nil, "", false)
			br = strings.TrimSpace(q.Stdout)
			if br == "" {
				br = "main"
			}
		}
		oc := p.str("opencode")
		if oc == "" {
			if x, er := RequireBinary("opencode"); er == nil {
				oc = x
			} else {
				oc = "opencode"
			}
		}
		models := map[string]string{}
		for _, n := range ProfileNames {
			models[n] = ""
		}
		cfg = ProjectConfig{ProjectID: MakeID("PRJ"), Name: filepath.Base(root), Root: root, BaseBranch: br, Harness: "opencode", OpenCodeCommand: oc, Models: models, Limits: DefaultLimits(), Capabilities: DefaultProjectCapabilities()}
	}
	if x := p.str("name"); x != "" {
		cfg.Name = x
	}
	if x := p.str("base-branch"); x != "" {
		cfg.BaseBranch = x
	}
	if x := p.str("opencode"); x != "" {
		cfg.OpenCodeCommand = x
	}
	marker, e = WriteProjectMarker(cfg)
	if e != nil {
		return fail(e)
	}
	q, e := Run(context.Background(), []string{git, "-C", root, "rev-parse", "--git-path", "info/exclude"}, "", nil, "", true)
	if e == nil {
		ex := strings.TrimSpace(q.Stdout)
		if !filepath.IsAbs(ex) {
			ex = filepath.Join(root, ex)
		}
		_ = os.MkdirAll(filepath.Dir(ex), 0755)
		data, _ := os.ReadFile(ex)
		lines := strings.Split(string(data), "\n")
		found := false
		for _, l := range lines {
			if l == ".praector/" {
				found = true
			}
		}
		if !found {
			f, _ := os.OpenFile(ex, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
			if f != nil {
				if len(data) > 0 && !strings.HasSuffix(string(data), "\n") {
					_, _ = f.WriteString("\n")
				}
				_, _ = f.WriteString(".praector/\n")
				_ = f.Close()
			}
		}
	}
	store, _, e := newStore()
	if e != nil {
		return fail(e)
	}
	if _, e = store.RegisterProject(cfg); e != nil {
		return fail(e)
	}
	fmt.Printf("Initialized Praector project %s (%s)\nProject marker: %s\nBase branch: %s\nOpenCode: %s\n", cfg.Name, cfg.ProjectID, marker, cfg.BaseBranch, cfg.OpenCodeCommand)
	return 0
}

func cmdProfile(args []string) int {
	if len(args) == 0 {
		return fail(fmt.Errorf("usage: praector profile {list|show}"))
	}
	switch args[0] {
	case "list":
		if len(args) != 1 {
			return fail(fmt.Errorf("usage: praector profile list"))
		}
		for _, n := range ProfileNames {
			mode := "write"
			if ReadOnlyProfiles[n] {
				mode = "read-only"
			}
			fmt.Printf("%-12s %-9s %s\n", n, mode, ProfileTitles[n])
		}
		return 0
	case "show":
		if len(args) != 2 {
			return fail(fmt.Errorf("usage: praector profile show PROFILE"))
		}
		valid := false
		for _, n := range ProfileNames {
			if args[1] == n {
				valid = true
			}
		}
		if !valid {
			return fail(fmt.Errorf("unknown profile: %s", args[1]))
		}
		g, _ := assets.FS.ReadFile("prompts/global.md")
		r, _ := assets.FS.ReadFile("prompts/" + args[1] + ".md")
		fmt.Printf("# Inherited global engineering contract\n\n%s\n\n# Role-specific operating contract\n\n%s\n", strings.TrimSpace(string(g)), strings.TrimSpace(string(r)))
		return 0
	}
	return fail(fmt.Errorf("unknown profile command: %s", args[0]))
}

func cmdProject(args []string) int {
	if len(args) == 0 {
		return fail(fmt.Errorf("usage: praector project {list|show}"))
	}
	store, _, e := newStore()
	if e != nil {
		return fail(e)
	}
	switch args[0] {
	case "list":
		if len(args) != 1 {
			return fail(fmt.Errorf("usage: praector project list"))
		}
		xs, e := store.ListProjects()
		if e != nil {
			return fail(e)
		}
		for _, r := range xs {
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
		r, e := projectFromContext(store, p.str("project"))
		if e != nil {
			return fail(e)
		}
		fmt.Println(pretty(r))
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
	store, cfg, e := newStore()
	if e != nil {
		return fail(e)
	}
	project, e := projectFromContext(store, p.str("project"))
	if e != nil {
		return fail(e)
	}
	if e = preflightProjectRuntime(project); e != nil {
		return fail(e)
	}
	objective := p.Pos[0]
	title := p.str("title")
	if title == "" {
		title = HumanTitle(objective, 80)
	}
	coordObj := fmt.Sprintf("Own the initial decomposition of the new collection: %s. Consume the deterministic explore/critic preflight when present. Create durable plan_review/research/implement/review/advise tasks using praector as appropriate; request additional explore/critic work only when genuinely needed. Do not perform implementation or substantive specialist work yourself. Ensure implementation tasks are sequenced by dependencies; independent review is automatically enforced for implementation tasks.", objective)
	c, coord, e := store.CreateCollectionWithCoordinator(S(project, "id"), title, objective, coordObj)
	if e != nil {
		return fail(e)
	}
	fmt.Printf("Created %s in project %s\nObjective: %s\nIntegration branch: %s\nCoordinator task: %s\n", S(c, "id"), S(project, "name"), S(c, "objective"), S(c, "integration_branch"), S(coord, "id"))
	if p.bool("detach") {
		fmt.Println("Queued. Run `praector daemon` to execute pending work.")
		return 0
	}
	sched, e := NewScheduler(store, cfg, mw)
	if e != nil {
		return fail(e)
	}
	state, e := sched.RunCollection(S(c, "id"), 500*time.Millisecond)
	if e != nil {
		return fail(e)
	}
	fmt.Printf("Collection %s: %s\n", S(c, "id"), state)
	if state == "BLOCKED" {
		printBlockedDiagnostics(store, S(c, "id"))
	}
	if state == "COMPLETE" {
		return 0
	}
	return 2
}

func printCollectionStatus(store *Store, c Row) {
	tasks, _ := store.ListTasks(S(c, "id"))
	counts := map[string]int{}
	for _, t := range tasks {
		counts[S(t, "status")]++
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := []string{}
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, counts[k]))
	}
	summary := strings.Join(parts, ", ")
	if summary == "" {
		summary = "no tasks (invalid/orphaned; `praector daemon --once` will repair)"
	}
	fmt.Printf("%s  %s  %s\n  objective: %s\n  branch: %s\n", S(c, "id"), S(c, "status"), S(c, "title"), S(c, "objective"), firstNonEmpty(S(c, "integration_branch"), "-"))
	if a, _ := store.GetCollectionAcceptance(S(c, "id"), ""); a != nil {
		fmt.Printf("  accepted: %s @ %s (%s, %s)\n", S(a, "target_branch"), shortSHA(S(a, "accepted_commit")), S(a, "merge_mode"), S(a, "created_at"))
	}
	if pr, _ := store.GetCollectionPullRequest(S(c, "id"), "github"); pr != nil {
		draft := ""
		if B(pr, "is_draft") {
			draft = " draft"
		}
		fmt.Printf("  pr: %s #%s [%s%s] %s\n", S(pr, "provider"), S(pr, "pr_number"), S(pr, "state"), draft, S(pr, "url"))
	}
	revs, _ := store.ListCollectionRevisions(S(c, "id"), "")
	if len(revs) > 0 {
		rc := map[string]int{}
		for _, r := range revs {
			rc[S(r, "status")]++
		}
		rk := make([]string, 0, len(rc))
		for k := range rc {
			rk = append(rk, k)
		}
		sort.Strings(rk)
		rp := []string{}
		for _, k := range rk {
			rp = append(rp, fmt.Sprintf("%s=%d", k, rc[k]))
		}
		fmt.Println("  revisions: " + strings.Join(rp, ", "))
		for _, r := range revs {
			if S(r, "status") == "OPEN" {
				fmt.Printf("    %s [OPEN] %s\n", S(r, "id"), S(r, "reason"))
			}
		}
	}
	fmt.Println("  tasks: " + summary)
	for _, t := range tasks {
		if map[string]bool{TaskFailed: true, TaskRunning: true, TaskWaiting: true, TaskVerifying: true, TaskReady: true, TaskPending: true}[S(t, "status")] {
			fmt.Printf("    %s [%s/%s] %s\n", S(t, "id"), S(t, "status"), S(t, "profile"), S(t, "title"))
		}
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
	store, _, e := newStore()
	if e != nil {
		return fail(e)
	}
	target := ""
	if len(p.Pos) > 0 {
		target = p.Pos[0]
	}
	if strings.HasPrefix(target, "COL-") {
		_, _ = store.UpdateCollectionTerminalState(target)
		c, e := store.GetCollection(target)
		if e != nil {
			return fail(e)
		}
		printCollectionStatus(store, c)
		return 0
	}
	if strings.HasPrefix(target, "T-") {
		t, e := store.GetTask(target)
		if e != nil {
			return fail(e)
		}
		fmt.Println(pretty(t))
		return 0
	}
	id := p.str("project")
	if id == "" {
		id = target
	}
	project, e := projectFromContext(store, id)
	if e != nil {
		return fail(e)
	}
	fmt.Printf("Project %s (%s)\n", S(project, "name"), S(project, "id"))
	cs, e := store.ListCollections(S(project, "id"))
	if e != nil {
		return fail(e)
	}
	for _, c := range cs {
		_, _ = store.UpdateCollectionTerminalState(S(c, "id"))
		fresh, _ := store.GetCollection(S(c, "id"))
		printCollectionStatus(store, fresh)
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
	store, _, e := newStore()
	if e != nil {
		return fail(e)
	}
	var c Row
	if len(p.Pos) > 0 {
		c, e = store.GetCollection(p.Pos[0])
	} else {
		var pr Row
		pr, e = projectFromContext(store, p.str("project"))
		if e == nil {
			c, e = latestCollection(store, S(pr, "id"))
		}
	}
	if e != nil {
		return fail(e)
	}
	tasks, _ := store.ListTasks(S(c, "id"))
	edges, _ := store.ListEdges(S(c, "id"))
	deps := map[string][]string{}
	for _, ed := range edges {
		if S(ed, "relation") == "depends_on" {
			deps[S(ed, "to_task_id")] = append(deps[S(ed, "to_task_id")], S(ed, "from_task_id"))
		}
	}
	fmt.Printf("%s %s [%s]\n", S(c, "id"), S(c, "title"), S(c, "status"))
	for _, t := range tasks {
		d := strings.Join(deps[S(t, "id")], ",")
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
		store, _, e := newStore()
		if e != nil {
			return fail(e)
		}
		t, e := store.GetTask(args[1])
		if e != nil {
			return fail(e)
		}
		ds, _ := store.Dependencies(args[1])
		dps, _ := store.Dependents(args[1])
		a, b := []string{}, []string{}
		for _, x := range ds {
			a = append(a, S(x, "id"))
		}
		for _, x := range dps {
			b = append(b, S(x, "id"))
		}
		t["dependencies"] = a
		t["dependents"] = b
		fmt.Println(pretty(t))
		return 0
	case "list":
		p, e := parseArgs(args[1:], map[string]bool{"collection": true, "project": true})
		if e != nil {
			return fail(e)
		}
		store, _, e := newStore()
		if e != nil {
			return fail(e)
		}
		var c Row
		if x := p.str("collection"); x != "" {
			c, e = store.GetCollection(x)
		} else {
			var pr Row
			pr, e = projectFromContext(store, p.str("project"))
			if e == nil {
				c, e = latestCollection(store, S(pr, "id"))
			}
		}
		if e != nil {
			return fail(e)
		}
		ts, _ := store.ListTasks(S(c, "id"))
		for _, t := range ts {
			ds, _ := store.Dependencies(S(t, "id"))
			ids := []string{}
			for _, d := range ds {
				ids = append(ids, S(d, "id"))
			}
			dt := strings.Join(ids, ",")
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
	reason := p.str("reason")
	if reason == "" {
		reason = "Operator-requested retry after task failure"
	}
	store, cfg, e := newStore()
	if e != nil {
		return fail(e)
	}
	failed, e := store.GetTask(p.Pos[0])
	if e != nil {
		return fail(e)
	}
	project, e := store.GetProject(S(failed, "project_id"))
	if e != nil {
		return fail(e)
	}
	if e = preflightProjectRuntime(project); e != nil {
		return fail(e)
	}
	replacement, e := store.RetryFailedTask(p.Pos[0], reason)
	if e != nil {
		return fail(e)
	}
	fmt.Printf("%s -> %s\nCollection %s resumed\n", p.Pos[0], S(replacement, "id"), S(failed, "collection_id"))
	if p.bool("detach") {
		fmt.Println("Queued. Run `praector daemon` to execute pending work.")
		return 0
	}
	sched, e := NewScheduler(store, cfg, mw)
	if e != nil {
		return fail(e)
	}
	state, e := sched.RunCollection(S(failed, "collection_id"), 500*time.Millisecond)
	if e != nil {
		return fail(e)
	}
	fmt.Printf("Collection %s: %s\n", S(failed, "collection_id"), state)
	if state == "BLOCKED" {
		printBlockedDiagnostics(store, S(failed, "collection_id"))
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
	store, _, e := newStore()
	if e != nil {
		return fail(e)
	}
	caller, e := requireCoordinator(store, p.str("caller"))
	if e != nil {
		return fail(e)
	}
	t, e := store.GetTask(p.Pos[0])
	if e != nil {
		return fail(e)
	}
	d, e := store.GetTask(p.str("on"))
	if e != nil {
		return fail(e)
	}
	if S(t, "collection_id") != S(caller, "collection_id") || S(d, "collection_id") != S(caller, "collection_id") {
		return fail(fmt.Errorf("Coordinator may only mutate its current collection"))
	}
	if e = store.AddDependency(S(t, "id"), S(d, "id")); e != nil {
		return fail(e)
	}
	fmt.Printf("%s depends_on %s\n", S(t, "id"), S(d, "id"))
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
	store, _, e := newStore()
	if e != nil {
		return fail(e)
	}
	caller, e := requireCoordinator(store, p.str("caller"))
	if e != nil {
		return fail(e)
	}
	old, e := store.GetTask(p.Pos[0])
	if e != nil {
		return fail(e)
	}
	rep, e := store.GetTask(p.str("with"))
	if e != nil {
		return fail(e)
	}
	if S(old, "collection_id") != S(caller, "collection_id") || S(rep, "collection_id") != S(caller, "collection_id") {
		return fail(fmt.Errorf("Coordinator may only mutate its current collection"))
	}
	if e = store.SupersedeTask(S(old, "id"), S(rep, "id"), p.str("reason")); e != nil {
		return fail(e)
	}
	fmt.Printf("%s -> %s\n", S(old, "id"), S(rep, "id"))
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
	store, cfg, e := newStore()
	if e != nil {
		return fail(e)
	}
	callerID, e := currentTaskID(p.str("task"))
	if e != nil {
		return fail(e)
	}
	caller, e := store.GetTask(callerID)
	if e != nil {
		return fail(e)
	}
	mode := p.str("workspace")
	if mode == "" || mode == "auto" {
		if S(caller, "profile") == "implement" {
			mode = "inherit"
		} else {
			mode = "collection"
		}
	}
	if S(caller, "profile") == "implement" && p.str("profile") == "implement" && mode == "inherit" && !p.bool("blocking") {
		return fail(fmt.Errorf("An inherited implementation child must be --blocking so its branch can safely merge back into the parent."))
	}
	base := ""
	if mode == "inherit" {
		w, e := NewGitWorkspace(store, cfg)
		if e != nil {
			return fail(e)
		}
		base, e = w.CheckpointForDelegation(callerID)
		if e != nil {
			return fail(e)
		}
	}
	lim, e := limitsForProject(store, S(caller, "project_id"))
	if e != nil {
		return fail(e)
	}
	t, created, e := store.RequestTask(callerID, RequestTaskOptions{Profile: p.str("profile"), Title: p.str("title"), Objective: p.str("objective"), Reason: p.str("reason"), Blocking: p.bool("blocking"), DependsOn: p.strs("depends-on"), WorkspaceMode: mode, BaseCommit: base, Limits: lim})
	if e != nil {
		return fail(e)
	}
	fmt.Println(S(t, "id"))
	if !created {
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
	store, _, e := newStore()
	if e != nil {
		return fail(e)
	}
	tid, e := currentTaskID(p.str("task"))
	if e != nil {
		return fail(e)
	}
	t, e := store.GetTask(tid)
	if e != nil {
		return fail(e)
	}
	pr, _ := store.GetProject(S(t, "project_id"))
	c, _ := store.GetCollection(S(t, "collection_id"))
	parent, _ := store.GetParent(t)
	deps, _ := store.Dependencies(tid)
	tasks, _ := store.ListTasks(S(c, "id"))
	small := Rows{}
	for _, x := range tasks {
		small = append(small, Row{"id": S(x, "id"), "profile": S(x, "profile"), "title": S(x, "title"), "status": S(x, "status"), "result": M(x, "result")})
	}
	mem, _ := store.ListMemories(S(pr, "id"), S(c, "id"), "")
	dec, _ := store.ListDecisions(S(pr, "id"), S(c, "id"))
	payload := Row{"execution": Row{"assigned_worktree": firstNonEmpty(S(t, "worktree_path"), os.Getenv("PRAECTOR_WORKTREE")), "rule": "Use assigned_worktree for all repository/file/shell operations; project.root_path is reference metadata only."}, "project": pr, "collection": c, "task": t, "parent": parent, "dependencies": deps, "tasks": small, "memories": mem, "decisions": dec}
	fmt.Println(pretty(payload))
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
	store, _, e := newStore()
	if e != nil {
		return fail(e)
	}
	tid, e := currentTaskID(p.str("task"))
	if e != nil {
		return fail(e)
	}
	caller, e := store.GetTask(tid)
	if e != nil {
		return fail(e)
	}
	lim, e := limitsForProject(store, S(caller, "project_id"))
	if e != nil {
		return fail(e)
	}
	t, _, e := store.RequestCoordinatorAttention(tid, p.str("reason"), p.bool("blocking"), lim)
	if e != nil {
		return fail(e)
	}
	fmt.Println(S(t, "id"))
	return 0
}

func cmdResult(args []string) int {
	if len(args) == 0 {
		return fail(fmt.Errorf("usage: praector result {show|submit}"))
	}
	store, _, e := newStore()
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
		tid, e := currentTaskID(explicit)
		if e != nil {
			return fail(e)
		}
		t, e := store.GetTask(tid)
		if e != nil {
			return fail(e)
		}
		fmt.Println(pretty(M(t, "result")))
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
		before, e := store.GetTask(tid)
		if e != nil {
			return fail(e)
		}
		st := strings.ToLower(S(result, "status"))
		if (S(before, "profile") == "review" || S(before, "profile") == "plan_review") && (st == "complete" || st == "completed") {
			v := strings.ToLower(strings.TrimSpace(S(result, "verdict")))
			if v != "pass" && v != "fail" {
				return fail(fmt.Errorf("A completed review/plan_review result must include verdict='pass' or verdict='fail'"))
			}
			result["verdict"] = v
		}
		t, e := store.SubmitResult(tid, result)
		if e != nil {
			return fail(e)
		}
		fmt.Println(S(t, "status"))
		return 0
	}
	return fail(fmt.Errorf("unknown result command: %s", args[0]))
}

func cmdMemory(args []string) int {
	if len(args) == 0 {
		return fail(fmt.Errorf("usage: praector memory {add|list}"))
	}
	store, _, e := newStore()
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
		taskID := p.str("task")
		if taskID == "" {
			taskID = os.Getenv("PRAECTOR_TASK_ID")
		}
		pid, cid := "", ""
		if taskID != "" {
			t, e := store.GetTask(taskID)
			if e != nil {
				return fail(e)
			}
			pid, cid = S(t, "project_id"), S(t, "collection_id")
		} else {
			pr, e := projectFromContext(store, p.str("project"))
			if e != nil {
				return fail(e)
			}
			pid, cid = S(pr, "id"), p.str("collection")
		}
		scope := p.str("scope")
		if scope == "" {
			scope = "collection"
		}
		state := p.str("state")
		if state == "" {
			state = "candidate"
		}
		kind := p.str("kind")
		if kind == "" {
			kind = "finding"
		}
		if scope != "collection" {
			cid = ""
		}
		r, e := store.AddMemory(pid, cid, taskID, scope, state, kind, p.str("text"), nil)
		if e != nil {
			return fail(e)
		}
		fmt.Println(S(r, "id"))
		return 0
	case "list":
		p, e := parseArgs(args[1:], map[string]bool{"project": true, "collection": true, "state": true})
		if e != nil {
			return fail(e)
		}
		pr, e := projectFromContext(store, p.str("project"))
		if e != nil {
			return fail(e)
		}
		state := p.str("state")
		if state == "" {
			state = "accepted"
		}
		xs, e := store.ListMemories(S(pr, "id"), p.str("collection"), state)
		if e != nil {
			return fail(e)
		}
		for _, m := range xs {
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
	store, _, e := newStore()
	if e != nil {
		return fail(e)
	}
	taskID := p.str("task")
	if taskID == "" {
		taskID = os.Getenv("PRAECTOR_TASK_ID")
	}
	pid, cid := "", ""
	if taskID != "" {
		t, e := store.GetTask(taskID)
		if e != nil {
			return fail(e)
		}
		pid, cid = S(t, "project_id"), S(t, "collection_id")
	} else {
		pr, e := projectFromContext(store, p.str("project"))
		if e != nil {
			return fail(e)
		}
		pid, cid = S(pr, "id"), p.str("collection")
	}
	r, e := store.AddDecision(pid, cid, taskID, p.str("statement"), p.str("rationale"), nil, nil)
	if e != nil {
		return fail(e)
	}
	fmt.Println(S(r, "id"))
	return 0
}

func validateCollectionForAcceptance(store *Store, c Row) (Row, Rows, error) {
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
func acceptActor(project Row) string {
	git, er := RequireBinary("git")
	if er == nil {
		r, _ := Run(context.Background(), []string{git, "-C", S(project, "root_path"), "config", "user.name"}, "", nil, "", false)
		if v := strings.TrimSpace(r.Stdout); v != "" {
			return v
		}
	}
	u, er := user.Current()
	if er == nil && u.Username != "" {
		return u.Username
	}
	return os.Getenv("USER")
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
	store, _, e := newStore()
	if e != nil {
		return fail(e)
	}
	c, e := store.GetCollection(args[0])
	if e != nil {
		return fail(e)
	}
	c["planning_policy"] = ClassifyPlanningPolicy(S(c, "objective")).AsRow()
	c["delivery_policy"] = ClassifyDeliveryPolicy(S(c, "objective")).AsRow()
	c["acceptances"], _ = store.ListCollectionAcceptances(S(c, "id"))
	c["pull_requests"], _ = store.ListCollectionPullRequests(S(c, "id"))
	c["revisions"], _ = store.ListCollectionRevisions(S(c, "id"), "")
	c["tasks"], _ = store.ListTasks(S(c, "id"))
	c["critic_revalidation"], _ = store.CriticRevalidationState(S(c, "id"))
	fmt.Println(pretty(c))
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
	store, cfg, e := newStore()
	if e != nil {
		return fail(e)
	}
	c, e := store.GetCollection(p.Pos[0])
	if e != nil {
		return fail(e)
	}
	project, e := store.GetProject(S(c, "project_id"))
	if e != nil {
		return fail(e)
	}
	if e = preflightProjectRuntime(project); e != nil {
		return fail(e)
	}
	state, e := store.UpdateCollectionTerminalState(S(c, "id"))
	if e != nil {
		return fail(e)
	}
	if state != "COMPLETE" {
		return fail(fmt.Errorf("Collection %s is %s, not COMPLETE", S(c, "id"), state))
	}
	rev, coord, e := store.RequestCollectionRevision(S(c, "id"), p.str("reason"))
	if e != nil {
		return fail(e)
	}
	fmt.Printf("Revision %s opened for %s\nCoordinator task: %s\nReason: %s\n", S(rev, "id"), S(c, "id"), S(coord, "id"), S(rev, "reason"))
	if p.bool("detach") {
		fmt.Println("Queued. Run `praector daemon` to execute revision work.")
		return 0
	}
	sched, e := NewScheduler(store, cfg, mw)
	if e != nil {
		return fail(e)
	}
	state, e = sched.RunCollection(S(c, "id"), 500*time.Millisecond)
	if e != nil {
		return fail(e)
	}
	fmt.Printf("Collection %s: %s\n", S(c, "id"), state)
	if state == "BLOCKED" {
		printBlockedDiagnostics(store, S(c, "id"))
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
	store, _, e := newStore()
	if e != nil {
		return fail(e)
	}
	c, e := store.GetCollection(p.Pos[0])
	if e != nil {
		return fail(e)
	}
	pr, e := store.GetProject(S(c, "project_id"))
	if e != nil {
		return fail(e)
	}
	target := p.str("target")
	if target == "" {
		target = S(pr, "base_branch")
	}
	git, e := RequireBinary("git")
	if e != nil {
		return fail(e)
	}
	cmd := []string{git, "-C", S(pr, "root_path"), "diff"}
	if p.bool("stat") {
		cmd = append(cmd, "--stat")
	}
	cmd = append(cmd, target+"..."+S(c, "integration_branch"))
	r, e := Run(context.Background(), cmd, "", nil, "", true)
	if e != nil {
		return fail(e)
	}
	fmt.Print(r.Stdout)
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
	store, cfg, e := newStore()
	if e != nil {
		return fail(e)
	}
	c, e := store.GetCollection(p.Pos[0])
	if e != nil {
		return fail(e)
	}
	project, e := store.GetProject(S(c, "project_id"))
	if e != nil {
		return fail(e)
	}
	c, tasks, e := validateCollectionForAcceptance(store, c)
	if e != nil {
		return fail(e)
	}
	existing, _ := store.GetCollectionPullRequest(S(c, "id"), "github")
	base := p.str("base")
	if base == "" && existing != nil {
		base = S(existing, "base_branch")
	}
	if base == "" {
		base = S(project, "base_branch")
	}
	w, e := NewGitWorkspace(store, cfg)
	if e != nil {
		return fail(e)
	}
	if ip := S(c, "integration_worktree"); ip != "" {
		if _, er := os.Stat(ip); er == nil {
			dirty, er := w.IsDirty(ip)
			if er == nil && dirty {
				return fail(fmt.Errorf("Collection integration worktree is dirty: %s. Resolve or checkpoint those changes before creating/updating a PR.", ip))
			}
		}
	}
	plan, e := w.AcceptancePlan(project, c, base)
	if e != nil {
		return fail(e)
	}
	repo, _ := filepath.Abs(S(project, "root_path"))
	remote := p.str("remote")
	if remote == "" && existing != nil {
		remote = S(existing, "remote_name")
	}
	remote, e = resolvePRRemote(repo, base, remote)
	if e != nil {
		return fail(e)
	}
	title := p.str("title")
	if title == "" && existing != nil {
		title = S(existing, "title")
	}
	if title == "" {
		title = DefaultPullRequestTitle(c, tasks)
	}
	body := BuildPullRequestBody(c, tasks, S(plan, "source_commit"))
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
	store, cfg, e := newStore()
	if e != nil {
		return fail(e)
	}
	c, e := store.GetCollection(p.Pos[0])
	if e != nil {
		return fail(e)
	}
	project, e := store.GetProject(S(c, "project_id"))
	if e != nil {
		return fail(e)
	}
	if _, _, e = validateCollectionForAcceptance(store, c); e != nil {
		return fail(e)
	}
	target := p.str("target")
	if target == "" {
		target = S(project, "base_branch")
	}
	existing, _ := store.GetCollectionAcceptance(S(c, "id"), target)
	w, e := NewGitWorkspace(store, cfg)
	if e != nil {
		return fail(e)
	}
	plan, e := w.AcceptancePlan(project, c, target)
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
	store, cfg, e := newStore()
	if e != nil {
		return fail(e)
	}
	c, e := store.GetCollection(p.Pos[0])
	if e != nil {
		return fail(e)
	}
	pr, e := store.GetProject(S(c, "project_id"))
	if e != nil {
		return fail(e)
	}
	if S(c, "status") != "COMPLETE" {
		return fail(fmt.Errorf("Collection cleanup requires status COMPLETE"))
	}
	acc, _ := store.GetCollectionAcceptance(S(c, "id"), "")
	if acc == nil && !p.bool("force") {
		return fail(fmt.Errorf("Collection has not been accepted. Pass --force to remove worktrees anyway."))
	}
	w, e := NewGitWorkspace(store, cfg)
	if e != nil {
		return fail(e)
	}
	cl, e := w.CleanupCollection(pr, c, p.bool("delete-branches"))
	if e != nil {
		return fail(e)
	}
	for _, x := range Slice(cl, "worktrees") {
		fmt.Printf("removed worktree %v\n", x)
	}
	for _, x := range Slice(cl, "branches") {
		fmt.Printf("deleted branch %v\n", x)
	}
	if len(Slice(cl, "worktrees")) == 0 && len(Slice(cl, "branches")) == 0 {
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
func printTaskLogs(store *Store, t Row, heading bool, tail int) int {
	rows, _ := store.Query("SELECT * FROM attempts WHERE task_id=" + SQLLiteral(S(t, "id")) + " ORDER BY attempt_no")
	prefix := ""
	if heading {
		fmt.Printf("%s [%s/%s] %s\n", S(t, "id"), S(t, "status"), S(t, "profile"), S(t, "title"))
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
	store, _, e := newStore()
	if e != nil {
		return fail(e)
	}
	target := p.Pos[0]
	if strings.HasPrefix(target, "COL-") {
		c, e := store.GetCollection(target)
		if e != nil {
			return fail(e)
		}
		ts, _ := store.ListTasks(S(c, "id"))
		fmt.Printf("%s %s [%s]\n", S(c, "id"), S(c, "title"), S(c, "status"))
		if len(ts) == 0 {
			fmt.Println("  no tasks")
			return 0
		}
		total := 0
		for _, t := range ts {
			total += printTaskLogs(store, t, true, tail)
		}
		if total == 0 {
			fmt.Println("No task attempts have been recorded for this collection.")
		}
		return 0
	}
	t, e := store.GetTask(target)
	if e != nil {
		return fail(e)
	}
	printTaskLogs(store, t, false, tail)
	return 0
}
func cmdDoctor(args []string) int {
	if len(args) > 0 {
		return fail(fmt.Errorf("usage: praector doctor"))
	}
	failures := 0
	for _, b := range []string{"git", "dolt", "opencode"} {
		if p, e := RequireBinary(b); e == nil {
			fmt.Printf("✓ %s: %s\n", b, p)
		} else {
			fmt.Printf("✗ %s: not found\n", b)
			failures++
		}
	}
	if p, e := RequireBinary("gh"); e == nil {
		fmt.Printf("✓ gh: %s (collection PR delivery available)\n", p)
	} else {
		fmt.Println("⚠ gh: not found (required only for `praector collection pr`)")
	}
	store, cfg, e := newStore()
	if e == nil {
		fmt.Printf("✓ Dolt state: %s\n", store.StateDir)
	} else {
		fmt.Printf("✗ Dolt state: %v\n", e)
		failures++
		store = nil
	}
	if marker, ok := FindProjectMarker(""); ok {
		pc, e := ReadProjectMarker(marker)
		if e != nil {
			fmt.Printf("✗ project marker: %v\n", e)
			failures++
		} else {
			fmt.Printf("✓ project: %s (%s)\n", pc.Name, pc.ProjectID)
			if store != nil {
				if _, e = store.GetProject(pc.ProjectID); e == nil {
					fmt.Println("✓ project registered in Dolt")
				} else {
					fmt.Printf("✗ project registered in Dolt: %v\n", e)
					failures++
				}
			}
			if _, e = pc.ResolvedOpenCode(); e == nil {
				fmt.Printf("✓ project OpenCode command: %s\n", pc.OpenCodeCommand)
			} else {
				fmt.Printf("✗ project OpenCode command: %v\n", e)
				failures++
			}
			if v := os.Getenv("VIRTUAL_ENV"); v != "" {
				va, _ := filepath.Abs(v)
				ra, _ := filepath.Abs(pc.Root)
				rel, _ := filepath.Rel(ra, va)
				if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
					fmt.Printf("⚠ active virtualenv outside project will be stripped from managed worker commands by default: %s\n", va)
					fmt.Println("  Prefer running Praector from a neutral shell/tool install. Set PRAECTOR_SANITIZE_PARENT_VENV=0 only if your OpenCode installation intentionally needs that parent venv.")
				}
			}
		}
	} else {
		fmt.Println("⚠ no project marker in current directory tree")
	}
	_ = cfg
	if failures > 0 {
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
