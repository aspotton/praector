package praector

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strings"

	"github.com/datascape/praector/internal/assets"
)

// Service is the shared typed domain seam used by BOTH the CLI adapters (cmd*
// funcs in cli.go) and the MCP adapter. Every durable domain operation the CLI
// performs flows through a Service method, giving command/surface coverage parity
// ("now and always") rather than a claim of unrestricted behavioral parity: a few
// privileged ops (collection pr/accept, explicit-root init) return validation
// results or plans and leave final push/merge/acceptance to the operator adapter.
//
// Ownership boundary (decision DEC-28fcd38435 / DEC-aacb7bfc23):
//   - A Service op owns domain work only: Store reads/durable mutations via the
//     existing *Store, GitWorkspace reads (plans/diffs/remote resolution), and
//     deterministic policy classification.
//   - An adapter owns argument/flag parsing, stdout formatting, exit codes, and
//     process launching. Scheduler/daemon process execution and external PR
//     provider delivery are intentionally NOT part of this layer: the scheduler
//     remains the sole process launcher and privileged writes stay opt-in.
//
// Service never launches a process and never introduces a second store or
// orchestration implementation.
type Service struct {
	store *Store
	cfg   GlobalConfig
}

// newService opens the durable store exactly once and binds it to the shared
// service layer. Command adapters that need to launch the scheduler reuse
// svc.store/svc.cfg so a single store connection is shared across the domain op
// and the launch.
func newService() (*Service, error) {
	s, cfg, err := newStore()
	if err != nil {
		return nil, err
	}
	return &Service{store: s, cfg: cfg}, nil
}

// serviceOpKind classifies how a commandHelp entry maps to the service layer.
type serviceOpKind int

const (
	// opDomain: the command has a typed Service domain op that the adapter calls.
	opDomain serviceOpKind = iota
	// opGroup: the entry is a command namespace (dispatch-only; adapter routes
	// to a leaf op; there is no standalone domain action).
	opGroup
	// opLaunchOnly: the command's only effect is running the scheduler loop
	// (process launching, owned by the scheduler/adapter); it has no durable
	// domain op in the service layer.
	opLaunchOnly
)

// serviceOp is the parity metadata for one commandHelp entry. The registry is
// the authoritative 1:1 map between the canonical CLI inventory (commandHelp)
// and the shared service layer; service_parity_test.go enforces that mapping.
//
// call holds the op's method expression (for opDomain entries) so adding a CLI
// command without wiring a domain op is a compile-visible omission rather than
// a silent drift.
type serviceOp struct {
	name string
	kind serviceOpKind
	call any
}

// serviceOps maps every commandHelp key to its shared service operation. Keep
// this in lockstep with commandHelp in help.go; the parity test fails CI on any
// drift in either direction.
var serviceOps = map[string]serviceOp{
	"init":                  {name: "Init", kind: opDomain, call: (*Service).Init},
	"run":                   {name: "Run", kind: opDomain, call: (*Service).Run},
	"status":                {name: "Status", kind: opDomain, call: (*Service).Status},
	"graph":                 {name: "Graph", kind: opDomain, call: (*Service).Graph},
	"daemon":                {name: "daemon-launch", kind: opLaunchOnly},
	"doctor":                {name: "Doctor", kind: opDomain, call: (*Service).Doctor},
	"logs":                  {name: "Logs", kind: opDomain, call: (*Service).Logs},
	"events":                {name: "Events", kind: opDomain, call: (*Service).Events},
	"collection":            {name: "collection", kind: opGroup},
	"collection show":       {name: "CollectionShow", kind: opDomain, call: (*Service).CollectionShow},
	"collection revise":     {name: "CollectionRevise", kind: opDomain, call: (*Service).CollectionRevise},
	"collection diff":       {name: "CollectionDiff", kind: opDomain, call: (*Service).CollectionDiff},
	"collection pr":         {name: "CollectionPRPlan", kind: opDomain, call: (*Service).CollectionPRPlan},
	"collection accept":     {name: "CollectionAccept", kind: opDomain, call: (*Service).CollectionAccept},
	"collection cleanup":    {name: "CollectionCleanup", kind: opDomain, call: (*Service).CollectionCleanup},
	"profile":               {name: "profile", kind: opGroup},
	"profile list":          {name: "ProfileList", kind: opDomain, call: (*Service).ProfileList},
	"profile show":          {name: "ProfileShow", kind: opDomain, call: (*Service).ProfileShow},
	"project":               {name: "project", kind: opGroup},
	"project list":          {name: "ProjectList", kind: opDomain, call: (*Service).ProjectList},
	"project show":          {name: "ProjectShow", kind: opDomain, call: (*Service).ProjectShow},
	"task":                  {name: "task", kind: opGroup},
	"task show":             {name: "TaskShow", kind: opDomain, call: (*Service).TaskShow},
	"task list":             {name: "TaskList", kind: opDomain, call: (*Service).TaskList},
	"task retry":            {name: "TaskRetry", kind: opDomain, call: (*Service).TaskRetry},
	"task request":          {name: "TaskRequest", kind: opDomain, call: (*Service).TaskRequest},
	"task add-dependency":   {name: "TaskAddDependency", kind: opDomain, call: (*Service).TaskAddDependency},
	"task supersede":        {name: "TaskSupersede", kind: opDomain, call: (*Service).TaskSupersede},
	"context":               {name: "context", kind: opGroup},
	"context show":          {name: "ContextShow", kind: opDomain, call: (*Service).ContextShow},
	"coordinator":           {name: "coordinator", kind: opGroup},
	"coordinator attention": {name: "CoordinatorAttention", kind: opDomain, call: (*Service).CoordinatorAttention},
	"result":                {name: "result", kind: opGroup},
	"result show":           {name: "ResultShow", kind: opDomain, call: (*Service).ResultShow},
	"result submit":         {name: "ResultSubmit", kind: opDomain, call: (*Service).ResultSubmit},
	"memory":                {name: "memory", kind: opGroup},
	"memory add":            {name: "MemoryAdd", kind: opDomain, call: (*Service).MemoryAdd},
	"memory list":           {name: "MemoryList", kind: opDomain, call: (*Service).MemoryList},
	"decision":              {name: "decision", kind: opGroup},
	"decision add":          {name: "DecisionAdd", kind: opDomain, call: (*Service).DecisionAdd},
}

// ---------------------------------------------------------------------------
// Profile ops
// ---------------------------------------------------------------------------

// ProfileInfo is one entry of `praector profile list`.
type ProfileInfo struct {
	Name  string
	Mode  string // "write" | "read-only"
	Title string
}

// ProfileListInput is the (empty) input for `praector profile list`.
type ProfileListInput struct{}

// ProfileListOutput is the typed result for `praector profile list`.
type ProfileListOutput struct {
	Profiles []ProfileInfo
}

// ProfileList returns the built-in runtime profiles and their capability mode.
// It performs no store or process work.
func (s *Service) ProfileList(ctx context.Context, in ProfileListInput) (ProfileListOutput, error) {
	out := ProfileListOutput{}
	for _, n := range ProfileNames {
		mode := "write"
		if ReadOnlyProfiles[n] {
			mode = "read-only"
		}
		out.Profiles = append(out.Profiles, ProfileInfo{Name: n, Mode: mode, Title: ProfileTitles[n]})
	}
	return out, nil
}

// ProfileShowInput is the input for `praector profile show PROFILE`.
type ProfileShowInput struct {
	Profile string
}

// ProfileShowOutput carries the inherited global + role contracts.
type ProfileShowOutput struct {
	Global string
	Role   string
}

// ProfileShow returns the inherited global and role-specific contracts for a
// built-in profile. It validates the profile name against ProfileNames.
func (s *Service) ProfileShow(ctx context.Context, in ProfileShowInput) (ProfileShowOutput, error) {
	valid := false
	for _, n := range ProfileNames {
		if in.Profile == n {
			valid = true
		}
	}
	if !valid {
		return ProfileShowOutput{}, fmt.Errorf("unknown profile: %s", in.Profile)
	}
	g, _ := assets.FS.ReadFile("prompts/global.md")
	r, _ := assets.FS.ReadFile("prompts/" + in.Profile + ".md")
	return ProfileShowOutput{
		Global: strings.TrimSpace(string(g)),
		Role:   strings.TrimSpace(string(r)),
	}, nil
}

// ---------------------------------------------------------------------------
// Project ops
// ---------------------------------------------------------------------------

// ProjectListInput is the (empty) input for `praector project list`.
type ProjectListInput struct{}

// ProjectListOutput is the typed result for `praector project list`.
type ProjectListOutput struct {
	Projects Rows
}

// ProjectList lists registered projects.
func (s *Service) ProjectList(ctx context.Context, in ProjectListInput) (ProjectListOutput, error) {
	xs, e := s.store.ListProjects()
	if e != nil {
		return ProjectListOutput{}, e
	}
	return ProjectListOutput{Projects: xs}, nil
}

// ProjectShowInput is the input for `praector project show`.
type ProjectShowInput struct {
	ProjectID string
}

// ProjectShowOutput is the typed result for `praector project show`.
type ProjectShowOutput struct {
	Project Row
}

// ProjectShow resolves and returns a registered project from context or flag.
func (s *Service) ProjectShow(ctx context.Context, in ProjectShowInput) (ProjectShowOutput, error) {
	r, e := projectFromContext(s.store, in.ProjectID)
	if e != nil {
		return ProjectShowOutput{}, e
	}
	return ProjectShowOutput{Project: r}, nil
}

// ---------------------------------------------------------------------------
// Collection status
// ---------------------------------------------------------------------------

// CollectionStatus carries the durable display fields for one collection,
// gathered once so every adapter formats identically.
type CollectionStatus struct {
	ID         string
	Status     string
	Title      string
	Objective  string
	Branch     string
	Acceptance Row
	PR         Row
	Revisions  Rows
	TaskCounts []KV
	Active     Rows
}

// KV is a sorted status -> count pair for collection task/revision summaries.
type KV struct {
	Key   string
	Count int
}

// collectionStatus gathers the collection display data from the store. It does
// no mutation and no printing.
func (s *Service) collectionStatus(c Row) CollectionStatus {
	cid := S(c, "id")
	st := CollectionStatus{
		ID:        cid,
		Status:    S(c, "status"),
		Title:     S(c, "title"),
		Objective: S(c, "objective"),
		Branch:    firstNonEmpty(S(c, "integration_branch"), "-"),
	}
	tasks, _ := s.store.ListTasks(cid)
	st.TaskCounts = sortedCounts(tasks, func(t Row) string { return S(t, "status") })
	for _, t := range tasks {
		if map[string]bool{TaskFailed: true, TaskRunning: true, TaskWaiting: true, TaskVerifying: true, TaskReady: true, TaskPending: true}[S(t, "status")] {
			st.Active = append(st.Active, t)
		}
	}
	st.Acceptance, _ = s.store.GetCollectionAcceptance(cid, "")
	st.PR, _ = s.store.GetCollectionPullRequest(cid, "github")
	revs, _ := s.store.ListCollectionRevisions(cid, "")
	st.Revisions = revs
	return st
}

// sortedCounts tallies a value function over rows into a key-sorted slice.
func sortedCounts(rows Rows, key func(Row) string) []KV {
	counts := map[string]int{}
	for _, r := range rows {
		counts[key(r)]++
	}
	ks := make([]string, 0, len(counts))
	for k := range counts {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	out := []KV{}
	for _, k := range ks {
		out = append(out, KV{Key: k, Count: counts[k]})
	}
	return out
}

// ---------------------------------------------------------------------------
// Status op (project | collection | task)
// ---------------------------------------------------------------------------

// StatusInput is the input for `praector status [TARGET]`.
type StatusInput struct {
	Target    string // may be empty, COL-…, or T-…
	ProjectID string // --project override when Target is not a resource id
}

// StatusOutput is the polymorphic typed result for `praector status`.
type StatusOutput struct {
	Kind        string // "collection" | "task" | "project"
	Collection  *CollectionStatus
	Task        Row
	ProjectName string
	ProjectID   string
	Collections []*CollectionStatus
}

// Status resolves the status target (collection, task, or project) and gathers
// the durable display data. Collection terminal-state reconciliation happens
// here (a durable state read/update), never in an adapter.
func (s *Service) Status(ctx context.Context, in StatusInput) (StatusOutput, error) {
	target := in.Target
	if strings.HasPrefix(target, "COL-") {
		_, _ = s.store.UpdateCollectionTerminalState(target)
		c, e := s.store.GetCollection(target)
		if e != nil {
			return StatusOutput{}, e
		}
		cs := s.collectionStatus(c)
		return StatusOutput{Kind: "collection", Collection: &cs}, nil
	}
	if strings.HasPrefix(target, "T-") {
		t, e := s.store.GetTask(target)
		if e != nil {
			return StatusOutput{}, e
		}
		return StatusOutput{Kind: "task", Task: t}, nil
	}
	id := in.ProjectID
	if id == "" {
		id = target
	}
	project, e := projectFromContext(s.store, id)
	if e != nil {
		return StatusOutput{}, e
	}
	cs, e := s.store.ListCollections(S(project, "id"))
	if e != nil {
		return StatusOutput{}, e
	}
	out := StatusOutput{Kind: "project", ProjectName: S(project, "name"), ProjectID: S(project, "id")}
	for _, c := range cs {
		_, _ = s.store.UpdateCollectionTerminalState(S(c, "id"))
		fresh, _ := s.store.GetCollection(S(c, "id"))
		st := s.collectionStatus(fresh)
		out.Collections = append(out.Collections, &st)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Graph op
// ---------------------------------------------------------------------------

// GraphInput is the input for `praector graph [COLLECTION]`.
type GraphInput struct {
	Collection string // optional explicit collection id
	ProjectID  string // --project override when Collection is empty
}

// GraphOutput is the typed result for `praector graph`.
type GraphOutput struct {
	Collection Row
	Tasks      Rows
	Deps       map[string][]string
}

// Graph resolves the target collection and returns its tasks with resolved
// depends_on edges.
func (s *Service) Graph(ctx context.Context, in GraphInput) (GraphOutput, error) {
	var c Row
	var e error
	if in.Collection != "" {
		c, e = s.store.GetCollection(in.Collection)
	} else {
		pr, pe := projectFromContext(s.store, in.ProjectID)
		if pe == nil {
			c, e = latestCollection(s.store, S(pr, "id"))
		} else {
			e = pe
		}
	}
	if e != nil {
		return GraphOutput{}, e
	}
	tasks, _ := s.store.ListTasks(S(c, "id"))
	edges, _ := s.store.ListEdges(S(c, "id"))
	deps := map[string][]string{}
	for _, ed := range edges {
		if S(ed, "relation") == "depends_on" {
			deps[S(ed, "to_task_id")] = append(deps[S(ed, "to_task_id")], S(ed, "from_task_id"))
		}
	}
	return GraphOutput{Collection: c, Tasks: tasks, Deps: deps}, nil
}

// ---------------------------------------------------------------------------
// Logs op
// ---------------------------------------------------------------------------

// LogsInput is the input for `praector logs TARGET [--tail N]`. Tail line
// rendering is a presentation concern; the op returns attempt metadata and the
// adapter tails the final log file.
type LogsInput struct {
	Target string
}

// TaskLog is the attempt log summary for one task.
type TaskLog struct {
	ID       string
	Status   string
	Profile  string
	Title    string
	Attempts Rows
}

// LogsOutput is the polymorphic typed result for `praector logs`.
type LogsOutput struct {
	Kind         string // "collection" | "task"
	CollectionID string
	Title        string
	Status       string
	TaskLogs     []TaskLog
	NoTasks      bool
}

// Logs gathers attempt-log metadata for a task or the tasks of a collection.
func (s *Service) Logs(ctx context.Context, in LogsInput) (LogsOutput, error) {
	if strings.HasPrefix(in.Target, "COL-") {
		c, e := s.store.GetCollection(in.Target)
		if e != nil {
			return LogsOutput{}, e
		}
		ts, _ := s.store.ListTasks(S(c, "id"))
		out := LogsOutput{Kind: "collection", CollectionID: S(c, "id"), Title: S(c, "title"), Status: S(c, "status"), NoTasks: len(ts) == 0}
		for _, t := range ts {
			out.TaskLogs = append(out.TaskLogs, s.taskLog(t))
		}
		return out, nil
	}
	t, e := s.store.GetTask(in.Target)
	if e != nil {
		return LogsOutput{}, e
	}
	return LogsOutput{Kind: "task", TaskLogs: []TaskLog{s.taskLog(t)}}, nil
}

// taskLog gathers one task's attempt rows.
func (s *Service) taskLog(t Row) TaskLog {
	rows, _ := s.store.Query("SELECT * FROM attempts WHERE task_id=" + SQLLiteral(S(t, "id")) + " ORDER BY attempt_no")
	return TaskLog{ID: S(t, "id"), Status: S(t, "status"), Profile: S(t, "profile"), Title: S(t, "title"), Attempts: rows}
}

// ---------------------------------------------------------------------------
// Events op
// ---------------------------------------------------------------------------

// EventsInput is the input for `praector events [TARGET]`. Target reuses the
// COL-*/T-*/PRJ-* inference the other read commands use; the flags override it
// where set. Since is an opaque, exclusive (created_at,id) cursor previously
// emitted by this command. A non-positive Limit uses DefaultEventLimit so a
// project-only scan (which has no project_id index) is always bounded.
type EventsInput struct {
	Target       string // COL-*/T-*/PRJ-* or empty
	ProjectID    string // --project override when Target is not a resource id
	CollectionID string // --collection filter
	TaskID       string // reserved: task filter when not given as Target
	Type         string // --type event_type filter
	Since        string // --since opaque cursor
	Limit        int    // --limit finite cap
}

// EventsOutput is the typed result for `praector events`. Rows are the durable
// event rows in oldest-first created_at,id order; NextCursor is the exclusive
// cursor to pass back as Since on the next call (empty when no rows are read).
type EventsOutput struct {
	Rows       Rows
	NextCursor string
}

// Events reads the durable event log directly from the store. It performs store
// reads only and launches no process, so it works with no daemon running. Scope
// resolution mirrors the other read commands: an explicit --project/--collection
// wins, a COL-*/T-*/PRJ-* Target infers the scope, and an otherwise-empty scope
// falls back to the ambient project context.
func (s *Service) Events(ctx context.Context, in EventsInput) (EventsOutput, error) {
	pid, cid, tid := in.ProjectID, in.CollectionID, in.TaskID
	if t := strings.TrimSpace(in.Target); t != "" {
		switch {
		case strings.HasPrefix(t, "COL-"):
			if cid == "" {
				cid = t
			}
		case strings.HasPrefix(t, "T-"):
			if tid == "" {
				tid = t
			}
		case strings.HasPrefix(t, "PRJ-"):
			if pid == "" {
				pid = t
			}
		}
	}
	// A scope with no collection/task filter still resolves the ambient project
	// (CLI cwd convenience), matching MemoryList; the project scan is bounded by
	// the finite Limit. A durable COL-*/T-* filter never inherits the daemon cwd.
	if pid == "" && cid == "" && tid == "" {
		pr, e := s.projectForScope("", "")
		if e != nil {
			return EventsOutput{}, e
		}
		pid = S(pr, "id")
	}
	sinceCreatedAt, sinceID, e := decodeEventCursor(in.Since)
	if e != nil {
		return EventsOutput{}, e
	}
	rows, e := s.store.ListEvents(pid, cid, tid, in.Type, sinceCreatedAt, sinceID, in.Limit)
	if e != nil {
		return EventsOutput{}, e
	}
	out := EventsOutput{Rows: rows}
	if n := len(rows); n > 0 {
		out.NextCursor = encodeEventCursor(S(rows[n-1], "created_at"), S(rows[n-1], "id"))
	}
	return out, nil
}

// encodeEventCursor packs a (created_at,id) pair into one opaque, URL-safe token
// the caller passes back unchanged as --since. The unit separator keeps the split
// unambiguous because neither field contains it.
func encodeEventCursor(createdAt, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(createdAt + "\x1f" + id))
}

// decodeEventCursor reverses encodeEventCursor, mapping an empty token to no
// filter and rejecting a malformed one rather than guessing a starting point.
func decodeEventCursor(token string) (createdAt, id string, err error) {
	if strings.TrimSpace(token) == "" {
		return "", "", nil
	}
	raw, e := base64.RawURLEncoding.DecodeString(strings.TrimSpace(token))
	if e != nil {
		return "", "", fmt.Errorf("invalid --since cursor")
	}
	created, part, found := strings.Cut(string(raw), "\x1f")
	if !found {
		return "", "", fmt.Errorf("invalid --since cursor")
	}
	return created, part, nil
}

// ---------------------------------------------------------------------------
// Run op
// ---------------------------------------------------------------------------

// RunInput is the durable-mutation input for `praector run OBJECTIVE`.
type RunInput struct {
	Objective string
	Title     string // empty => derived from objective
	ProjectID string // --project override
}

// RunOutput is the created collection/coordinator for `praector run`. Scheduler
// launching (if any) is an adapter concern layered after this op.
type RunOutput struct {
	ProjectName string
	Collection  Row
	Coordinator Row
}

// Run creates a collection with its initial coordinator task. It preflights the
// project runtime first, exactly as the CLI does today.
func (s *Service) Run(ctx context.Context, in RunInput) (RunOutput, error) {
	project, e := projectFromContext(s.store, in.ProjectID)
	if e != nil {
		return RunOutput{}, e
	}
	if e = preflightProjectRuntime(project); e != nil {
		return RunOutput{}, e
	}
	title := in.Title
	if title == "" {
		title = HumanTitle(in.Objective, 80)
	}
	coordObj := fmt.Sprintf("Own the initial decomposition of the new collection: %s. Consume the deterministic explore/critic preflight when present. Create durable plan_review/research/implement/review/advise tasks using praector as appropriate; request additional explore/critic work only when genuinely needed. Do not perform implementation or substantive specialist work yourself. Ensure implementation tasks are sequenced by dependencies; independent review is automatically enforced for implementation tasks.", in.Objective)
	c, coord, e := s.store.CreateCollectionWithCoordinator(S(project, "id"), title, in.Objective, coordObj)
	if e != nil {
		return RunOutput{}, e
	}
	return RunOutput{ProjectName: S(project, "name"), Collection: c, Coordinator: coord}, nil
}

// ---------------------------------------------------------------------------
// Collection ops
// ---------------------------------------------------------------------------

// CollectionShowInput is the input for `praector collection show COL-ID`.
type CollectionShowInput struct {
	CollectionID string
}

// CollectionShowOutput is the enriched collection row for `collection show`.
type CollectionShowOutput struct {
	Collection Row
}

// CollectionShow enriches a collection with policy, acceptance, PR, revision,
// task, and critic-revalidation data for display.
func (s *Service) CollectionShow(ctx context.Context, in CollectionShowInput) (CollectionShowOutput, error) {
	c, e := s.store.GetCollection(in.CollectionID)
	if e != nil {
		return CollectionShowOutput{}, e
	}
	c["planning_policy"] = ClassifyPlanningPolicy(S(c, "objective")).AsRow()
	c["delivery_policy"] = ClassifyDeliveryPolicy(S(c, "objective")).AsRow()
	c["acceptances"], _ = s.store.ListCollectionAcceptances(S(c, "id"))
	c["pull_requests"], _ = s.store.ListCollectionPullRequests(S(c, "id"))
	c["revisions"], _ = s.store.ListCollectionRevisions(S(c, "id"), "")
	c["tasks"], _ = s.store.ListTasks(S(c, "id"))
	c["critic_revalidation"], _ = s.store.CriticRevalidationState(S(c, "id"))
	return CollectionShowOutput{Collection: c}, nil
}

// CollectionReviseInput is the input for `praector collection revise COL-ID`.
type CollectionReviseInput struct {
	CollectionID string
	Reason       string
}

// CollectionReviseOutput is the opened revision and coordinator task.
type CollectionReviseOutput struct {
	Collection  Row
	Revision    Row
	Coordinator Row
}

// CollectionRevise reopens a COMPLETE, unaccepted collection for mandatory
// repair. Scheduler launching is an adapter concern.
func (s *Service) CollectionRevise(ctx context.Context, in CollectionReviseInput) (CollectionReviseOutput, error) {
	c, e := s.store.GetCollection(in.CollectionID)
	if e != nil {
		return CollectionReviseOutput{}, e
	}
	project, e := s.store.GetProject(S(c, "project_id"))
	if e != nil {
		return CollectionReviseOutput{}, e
	}
	if e = preflightProjectRuntime(project); e != nil {
		return CollectionReviseOutput{}, e
	}
	state, e := s.store.UpdateCollectionTerminalState(S(c, "id"))
	if e != nil {
		return CollectionReviseOutput{}, e
	}
	if state != "COMPLETE" {
		return CollectionReviseOutput{}, fmt.Errorf("Collection %s is %s, not COMPLETE", S(c, "id"), state)
	}
	rev, coord, e := s.store.RequestCollectionRevision(S(c, "id"), in.Reason)
	if e != nil {
		return CollectionReviseOutput{}, e
	}
	return CollectionReviseOutput{Collection: c, Revision: rev, Coordinator: coord}, nil
}

// CollectionDiffInput is the input for `praector collection diff COL-ID`.
type CollectionDiffInput struct {
	CollectionID string
	Target       string // empty => project base branch
	Stat         bool
}

// CollectionDiffOutput carries the resolved target branch and diff text.
type CollectionDiffOutput struct {
	Stdout string
}

// CollectionDiff shows collection changes relative to a target branch.
func (s *Service) CollectionDiff(ctx context.Context, in CollectionDiffInput) (CollectionDiffOutput, error) {
	c, e := s.store.GetCollection(in.CollectionID)
	if e != nil {
		return CollectionDiffOutput{}, e
	}
	pr, e := s.store.GetProject(S(c, "project_id"))
	if e != nil {
		return CollectionDiffOutput{}, e
	}
	target := in.Target
	if target == "" {
		target = S(pr, "base_branch")
	}
	git, e := RequireBinary("git")
	if e != nil {
		return CollectionDiffOutput{}, e
	}
	cmd := []string{git, "-C", S(pr, "root_path"), "diff"}
	if in.Stat {
		cmd = append(cmd, "--stat")
	}
	cmd = append(cmd, target+"..."+S(c, "integration_branch"))
	r, e := Run(ctx, cmd, "", nil, "", true)
	if e != nil {
		return CollectionDiffOutput{}, e
	}
	return CollectionDiffOutput{Stdout: r.Stdout}, nil
}

// CollectionPRPlanInput is the input for `praector collection pr COL-ID`.
type CollectionPRPlanInput struct {
	CollectionID string
	Base         string // empty => existing PR base, else project base
	Title        string // empty => existing PR title, else default
	Remote       string // empty => existing PR remote, else auto-resolve
	Draft        bool
	RefreshCopy  bool
	ExistingSet  bool // unused placeholder retained for parity symmetry
}

// CollectionPRPlanOutput is the deterministic PR plan used by the adapter for
// the dry-run display and for the subsequent provider delivery (push/create/
// reuse). External PR provider calls and the branch push are adapter concerns.
type CollectionPRPlanOutput struct {
	Collection Row
	Tasks      Rows
	Project    Row
	Plan       Row
	Existing   Row
	Base       string
	Remote     string
	Title      string
	Body       string
	Repository Row // nil until provider resolves it (adapter fills)
	Resolved   bool
}

// CollectionPRPlan validates acceptance, resolves base/remote/title, builds the
// PR body, and surfaces any pre-delivery refusal. It performs no network call
// and no process launch; the adapter performs push/provider delivery.
func (s *Service) CollectionPRPlan(ctx context.Context, in CollectionPRPlanInput) (CollectionPRPlanOutput, error) {
	c, e := s.store.GetCollection(in.CollectionID)
	if e != nil {
		return CollectionPRPlanOutput{}, e
	}
	project, e := s.store.GetProject(S(c, "project_id"))
	if e != nil {
		return CollectionPRPlanOutput{}, e
	}
	acceptanceCandidates, e := s.deriveConfigCandidates(project, c)
	if e != nil {
		return CollectionPRPlanOutput{}, e
	}
	c, tasks, e := validateCollectionForAcceptance(s.store, c, acceptanceCandidates...)
	if e != nil {
		return CollectionPRPlanOutput{}, e
	}
	existing, _ := s.store.GetCollectionPullRequest(S(c, "id"), "github")
	base := in.Base
	if base == "" && existing != nil {
		base = S(existing, "base_branch")
	}
	if base == "" {
		base = S(project, "base_branch")
	}
	w, e := NewGitWorkspace(s.store, s.cfg)
	if e != nil {
		return CollectionPRPlanOutput{}, e
	}
	if ip := S(c, "integration_worktree"); ip != "" {
		if _, er := os.Stat(ip); er == nil {
			dirty, er := w.IsDirty(ip)
			if er == nil && dirty {
				return CollectionPRPlanOutput{}, fmt.Errorf("Collection integration worktree is dirty: %s. Resolve or checkpoint those changes before creating/updating a PR.", ip)
			}
		}
	}
	plan, e := w.AcceptancePlan(project, c, base)
	if e != nil {
		return CollectionPRPlanOutput{}, e
	}
	repo, _ := filepath.Abs(S(project, "root_path"))
	remote := in.Remote
	if remote == "" && existing != nil {
		remote = S(existing, "remote_name")
	}
	remote, e = resolvePRRemote(repo, base, remote)
	if e != nil {
		return CollectionPRPlanOutput{}, e
	}
	title := in.Title
	if title == "" && existing != nil {
		title = S(existing, "title")
	}
	if title == "" {
		title = DefaultPullRequestTitle(c, tasks)
	}
	body := BuildPullRequestBody(c, tasks, S(plan, "source_commit"))
	return CollectionPRPlanOutput{
		Collection: c, Tasks: tasks, Project: project, Plan: plan, Existing: existing,
		Base: base, Remote: remote, Title: title, Body: body,
	}, nil
}

// CollectionAcceptInput is the input for `praector collection accept COL-ID`.
type CollectionAcceptInput struct {
	CollectionID string
	Target       string // empty => project base branch
}

// CollectionAcceptOutput is the acceptance plan surfaced for display and for
// the adapter-driven merge. The merge/record/cleanup execution is an adapter
// concern (privileged write; process/git delivery boundary).
type CollectionAcceptOutput struct {
	Collection Row
	Project    Row
	Target     string
	Plan       Row
	Existing   Row
}

// CollectionAccept validates acceptance, resolves the target, and builds the
// acceptance plan. It performs no merge and no record write.
func (s *Service) CollectionAccept(ctx context.Context, in CollectionAcceptInput) (CollectionAcceptOutput, error) {
	c, e := s.store.GetCollection(in.CollectionID)
	if e != nil {
		return CollectionAcceptOutput{}, e
	}
	project, e := s.store.GetProject(S(c, "project_id"))
	if e != nil {
		return CollectionAcceptOutput{}, e
	}
	acceptanceCandidates, e := s.deriveConfigCandidates(project, c)
	if e != nil {
		return CollectionAcceptOutput{}, e
	}
	if _, _, e = validateCollectionForAcceptance(s.store, c, acceptanceCandidates...); e != nil {
		return CollectionAcceptOutput{}, e
	}
	target := in.Target
	if target == "" {
		target = S(project, "base_branch")
	}
	existing, _ := s.store.GetCollectionAcceptance(S(c, "id"), target)
	w, e := NewGitWorkspace(s.store, s.cfg)
	if e != nil {
		return CollectionAcceptOutput{}, e
	}
	plan, e := w.AcceptancePlan(project, c, target)
	if e != nil {
		return CollectionAcceptOutput{}, e
	}
	return CollectionAcceptOutput{Collection: c, Project: project, Target: target, Plan: plan, Existing: existing}, nil
}

// CollectionCleanupInput is the input for `praector collection cleanup COL-ID`.
type CollectionCleanupInput struct {
	CollectionID   string
	DeleteBranches bool
	Force          bool
}

// CollectionCleanupOutput is the cleanup result for display.
type CollectionCleanupOutput struct {
	Worktrees []any
	Branches  []any
}

// CollectionCleanup removes completed collection/task worktrees. It enforces the
// COMPLETE + accepted (or --force) precondition durably.
func (s *Service) CollectionCleanup(ctx context.Context, in CollectionCleanupInput) (CollectionCleanupOutput, error) {
	c, e := s.store.GetCollection(in.CollectionID)
	if e != nil {
		return CollectionCleanupOutput{}, e
	}
	pr, e := s.store.GetProject(S(c, "project_id"))
	if e != nil {
		return CollectionCleanupOutput{}, e
	}
	if S(c, "status") != "COMPLETE" {
		return CollectionCleanupOutput{}, fmt.Errorf("Collection cleanup requires status COMPLETE")
	}
	acc, _ := s.store.GetCollectionAcceptance(S(c, "id"), "")
	if acc == nil && !in.Force {
		return CollectionCleanupOutput{}, fmt.Errorf("Collection has not been accepted. Pass --force to remove worktrees anyway.")
	}
	w, e := NewGitWorkspace(s.store, s.cfg)
	if e != nil {
		return CollectionCleanupOutput{}, e
	}
	cl, e := w.CleanupCollection(pr, c, in.DeleteBranches)
	if e != nil {
		return CollectionCleanupOutput{}, e
	}
	return CollectionCleanupOutput{Worktrees: Slice(cl, "worktrees"), Branches: Slice(cl, "branches")}, nil
}

// ---------------------------------------------------------------------------
// Task ops
// ---------------------------------------------------------------------------

// TaskShowInput is the input for `praector task show TASK`.
type TaskShowInput struct {
	TaskID string
}

// TaskShowOutput is the task with resolved dependencies/dependents.
type TaskShowOutput struct {
	Task Row
}

// TaskShow returns a task with its dependency and dependent id lists attached.
func (s *Service) TaskShow(ctx context.Context, in TaskShowInput) (TaskShowOutput, error) {
	t, e := s.store.GetTask(in.TaskID)
	if e != nil {
		return TaskShowOutput{}, e
	}
	ds, _ := s.store.Dependencies(in.TaskID)
	dps, _ := s.store.Dependents(in.TaskID)
	a, b := []string{}, []string{}
	for _, x := range ds {
		a = append(a, S(x, "id"))
	}
	for _, x := range dps {
		b = append(b, S(x, "id"))
	}
	t["dependencies"] = a
	t["dependents"] = b
	return TaskShowOutput{Task: t}, nil
}

// TaskListInput is the input for `praector task list`.
type TaskListInput struct {
	Collection string
	ProjectID  string
}

// TaskListEntry is one task row with a resolved dependency id list.
type TaskListEntry struct {
	Task Row
	Deps []string
}

// TaskListOutput is the typed result for `praector task list`.
type TaskListOutput struct {
	Tasks []TaskListEntry
}

// TaskList lists tasks in the resolved collection with dependency ids.
func (s *Service) TaskList(ctx context.Context, in TaskListInput) (TaskListOutput, error) {
	var c Row
	var e error
	if in.Collection != "" {
		c, e = s.store.GetCollection(in.Collection)
	} else {
		pr, pe := projectFromContext(s.store, in.ProjectID)
		if pe == nil {
			c, e = latestCollection(s.store, S(pr, "id"))
		} else {
			e = pe
		}
	}
	if e != nil {
		return TaskListOutput{}, e
	}
	ts, _ := s.store.ListTasks(S(c, "id"))
	out := TaskListOutput{}
	for _, t := range ts {
		ds, _ := s.store.Dependencies(S(t, "id"))
		ids := []string{}
		for _, d := range ds {
			ids = append(ids, S(d, "id"))
		}
		out.Tasks = append(out.Tasks, TaskListEntry{Task: t, Deps: ids})
	}
	return out, nil
}

// TaskRetryInput is the input for `praector task retry TASK`.
type TaskRetryInput struct {
	TaskID string
	Reason string // empty => default operator-retry reason
}

// TaskRetryOutput is the replacement task for a FAILED task.
type TaskRetryOutput struct {
	Original     Row
	Replacement  Row
	CollectionID string
}

// TaskRetry creates a fresh replacement for a FAILED task after preflight.
// Scheduler launching is an adapter concern.
func (s *Service) TaskRetry(ctx context.Context, in TaskRetryInput) (TaskRetryOutput, error) {
	failed, e := s.store.GetTask(in.TaskID)
	if e != nil {
		return TaskRetryOutput{}, e
	}
	project, e := s.store.GetProject(S(failed, "project_id"))
	if e != nil {
		return TaskRetryOutput{}, e
	}
	if e = preflightProjectRuntime(project); e != nil {
		return TaskRetryOutput{}, e
	}
	reason := in.Reason
	if reason == "" {
		reason = "Operator-requested retry after task failure"
	}
	replacement, e := s.store.RetryFailedTask(in.TaskID, reason)
	if e != nil {
		return TaskRetryOutput{}, e
	}
	return TaskRetryOutput{Original: failed, Replacement: replacement, CollectionID: S(failed, "collection_id")}, nil
}

// TaskRequestInput is the input for `praector task request`.
type TaskRequestInput struct {
	CallerID  string // empty => PRAECTOR_TASK_ID
	Profile   string
	Title     string
	Objective string
	Reason    string
	Blocking  bool
	DependsOn []string
	Workspace string
}

// TaskRequestOutput is the requested/created task and dedup status.
type TaskRequestOutput struct {
	Task    Row
	Created bool
}

// TaskRequest requests durable delegated work from the caller task. Inherited
// implementation children are forced blocking so their branch merges back safely.
func (s *Service) TaskRequest(ctx context.Context, in TaskRequestInput) (TaskRequestOutput, error) {
	if in.Profile == "coordinator" {
		return TaskRequestOutput{}, fmt.Errorf("--profile coordinator is not valid for task request; use coordinator attention")
	}
	callerID, e := currentTaskID(in.CallerID)
	if e != nil {
		return TaskRequestOutput{}, e
	}
	caller, e := s.store.GetTask(callerID)
	if e != nil {
		return TaskRequestOutput{}, e
	}
	mode := in.Workspace
	if mode == "" || mode == "auto" {
		if S(caller, "profile") == "implement" {
			mode = "inherit"
		} else {
			mode = "collection"
		}
	}
	if S(caller, "profile") == "implement" && in.Profile == "implement" && mode == "inherit" && !in.Blocking {
		return TaskRequestOutput{}, fmt.Errorf("An inherited implementation child must be --blocking so its branch can safely merge back into the parent.")
	}
	base := ""
	if mode == "inherit" {
		w, e := NewGitWorkspace(s.store, s.cfg)
		if e != nil {
			return TaskRequestOutput{}, e
		}
		base, e = w.CheckpointForDelegation(callerID)
		if e != nil {
			return TaskRequestOutput{}, e
		}
	}
	lim, e := limitsForProject(s.store, S(caller, "project_id"))
	if e != nil {
		return TaskRequestOutput{}, e
	}
	t, created, e := s.store.RequestTask(callerID, RequestTaskOptions{Profile: in.Profile, Title: in.Title, Objective: in.Objective, Reason: in.Reason, Blocking: in.Blocking, DependsOn: in.DependsOn, WorkspaceMode: mode, BaseCommit: base, Limits: lim})
	if e != nil {
		return TaskRequestOutput{}, e
	}
	return TaskRequestOutput{Task: t, Created: created}, nil
}

// TaskAddDependencyInput is the input for `praector task add-dependency`.
type TaskAddDependencyInput struct {
	TaskID string
	OnID   string
	Caller string // empty => current task; must be a coordinator
}

// TaskAddDependencyOutput is the resolved dependency edge.
type TaskAddDependencyOutput struct {
	Task Row
	Dep  Row
}

// TaskAddDependency performs the coordinator-only dependency edge mutation,
// enforcing coordinator profile and same-collection scope.
func (s *Service) TaskAddDependency(ctx context.Context, in TaskAddDependencyInput) (TaskAddDependencyOutput, error) {
	caller, e := requireCoordinator(s.store, in.Caller)
	if e != nil {
		return TaskAddDependencyOutput{}, e
	}
	t, e := s.store.GetTask(in.TaskID)
	if e != nil {
		return TaskAddDependencyOutput{}, e
	}
	d, e := s.store.GetTask(in.OnID)
	if e != nil {
		return TaskAddDependencyOutput{}, e
	}
	if S(t, "collection_id") != S(caller, "collection_id") || S(d, "collection_id") != S(caller, "collection_id") {
		return TaskAddDependencyOutput{}, fmt.Errorf("Coordinator may only mutate its current collection")
	}
	if e = s.store.AddDependency(S(t, "id"), S(d, "id")); e != nil {
		return TaskAddDependencyOutput{}, e
	}
	return TaskAddDependencyOutput{Task: t, Dep: d}, nil
}

// TaskSupersedeInput is the input for `praector task supersede`.
type TaskSupersedeInput struct {
	OldID  string
	NewID  string
	Reason string
	Caller string
}

// TaskSupersedeOutput is the old/replacement task pair.
type TaskSupersedeOutput struct {
	Old Row
	New Row
}

// TaskSupersede performs the coordinator-only replacement/rewire, enforcing
// coordinator profile and same-collection scope.
func (s *Service) TaskSupersede(ctx context.Context, in TaskSupersedeInput) (TaskSupersedeOutput, error) {
	caller, e := requireCoordinator(s.store, in.Caller)
	if e != nil {
		return TaskSupersedeOutput{}, e
	}
	old, e := s.store.GetTask(in.OldID)
	if e != nil {
		return TaskSupersedeOutput{}, e
	}
	rep, e := s.store.GetTask(in.NewID)
	if e != nil {
		return TaskSupersedeOutput{}, e
	}
	if S(old, "collection_id") != S(caller, "collection_id") || S(rep, "collection_id") != S(caller, "collection_id") {
		return TaskSupersedeOutput{}, fmt.Errorf("Coordinator may only mutate its current collection")
	}
	if e = s.store.SupersedeTask(S(old, "id"), S(rep, "id"), in.Reason); e != nil {
		return TaskSupersedeOutput{}, e
	}
	return TaskSupersedeOutput{Old: old, New: rep}, nil
}

// ---------------------------------------------------------------------------
// Context / coordinator / result / memory / decision ops
// ---------------------------------------------------------------------------

// ContextShowInput is the input for `praector context show`.
type ContextShowInput struct {
	TaskID string // empty => PRAECTOR_TASK_ID
}

// ContextShowOutput is the durable context payload.
type ContextShowOutput struct {
	Payload Row
}

// ContextShow assembles the durable task context payload.
func (s *Service) ContextShow(ctx context.Context, in ContextShowInput) (ContextShowOutput, error) {
	tid, e := currentTaskID(in.TaskID)
	if e != nil {
		return ContextShowOutput{}, e
	}
	t, e := s.store.GetTask(tid)
	if e != nil {
		return ContextShowOutput{}, e
	}
	pr, _ := s.store.GetProject(S(t, "project_id"))
	c, _ := s.store.GetCollection(S(t, "collection_id"))
	parent, _ := s.store.GetParent(t)
	deps, _ := s.store.Dependencies(tid)
	tasks, _ := s.store.ListTasks(S(c, "id"))
	small := Rows{}
	for _, x := range tasks {
		small = append(small, Row{"id": S(x, "id"), "profile": S(x, "profile"), "title": S(x, "title"), "status": S(x, "status"), "result": M(x, "result")})
	}
	mem, _ := s.store.ListMemories(S(pr, "id"), S(c, "id"), "")
	dec, _ := s.store.ListDecisions(S(pr, "id"), S(c, "id"))
	payload := Row{"execution": Row{"assigned_worktree": firstNonEmpty(S(t, "worktree_path"), os.Getenv("PRAECTOR_WORKTREE")), "rule": "Use assigned_worktree for all repository/file/shell operations; project.root_path is reference metadata only."}, "project": pr, "collection": c, "task": t, "parent": parent, "dependencies": deps, "tasks": small, "memories": mem, "decisions": dec}
	return ContextShowOutput{Payload: payload}, nil
}

// CoordinatorAttentionInput is the input for `praector coordinator attention`.
type CoordinatorAttentionInput struct {
	TaskID   string
	Reason   string
	Blocking bool
}

// CoordinatorAttentionOutput is the escalation task.
type CoordinatorAttentionOutput struct {
	Task Row
}

// CoordinatorAttention requests coordinator attention as durable work.
func (s *Service) CoordinatorAttention(ctx context.Context, in CoordinatorAttentionInput) (CoordinatorAttentionOutput, error) {
	if in.Reason == "" {
		return CoordinatorAttentionOutput{}, fmt.Errorf("--reason is required")
	}
	tid, e := currentTaskID(in.TaskID)
	if e != nil {
		return CoordinatorAttentionOutput{}, e
	}
	caller, e := s.store.GetTask(tid)
	if e != nil {
		return CoordinatorAttentionOutput{}, e
	}
	lim, e := limitsForProject(s.store, S(caller, "project_id"))
	if e != nil {
		return CoordinatorAttentionOutput{}, e
	}
	t, _, e := s.store.RequestCoordinatorAttention(tid, in.Reason, in.Blocking, lim)
	if e != nil {
		return CoordinatorAttentionOutput{}, e
	}
	return CoordinatorAttentionOutput{Task: t}, nil
}

// ResultShowInput is the input for `praector result show [TASK]`.
type ResultShowInput struct {
	TaskID string // empty => PRAECTOR_TASK_ID
}

// ResultShowOutput is the durable structured result.
type ResultShowOutput struct {
	Result Row
}

// ResultShow returns the durable structured result for a task.
func (s *Service) ResultShow(ctx context.Context, in ResultShowInput) (ResultShowOutput, error) {
	tid, e := currentTaskID(in.TaskID)
	if e != nil {
		return ResultShowOutput{}, e
	}
	t, e := s.store.GetTask(tid)
	if e != nil {
		return ResultShowOutput{}, e
	}
	return ResultShowOutput{Result: M(t, "result")}, nil
}

// ResultSubmitInput is the input for `praector result submit`. The result Row is
// already parsed/validated by the adapter (JSON stdin/file/summary).
type ResultSubmitInput struct {
	TaskID string
	Result Row
}

// ResultSubmitOutput is the task after the result was submitted.
type ResultSubmitOutput struct {
	Status string
}

// ResultSubmit validates review/plan_review verdicts and durably submits the
// structured result.
func (s *Service) ResultSubmit(ctx context.Context, in ResultSubmitInput) (ResultSubmitOutput, error) {
	tid, e := currentTaskID(in.TaskID)
	if e != nil {
		return ResultSubmitOutput{}, e
	}
	before, e := s.store.GetTask(tid)
	if e != nil {
		return ResultSubmitOutput{}, e
	}
	st := strings.ToLower(S(in.Result, "status"))
	if (S(before, "profile") == "review" || S(before, "profile") == "plan_review") && (st == "complete" || st == "completed") {
		v := strings.ToLower(strings.TrimSpace(S(in.Result, "verdict")))
		if v != "pass" && v != "fail" {
			return ResultSubmitOutput{}, fmt.Errorf("A completed review/plan_review result must include verdict='pass' or verdict='fail'")
		}
		in.Result["verdict"] = v
	}
	var candidates []DerivedConfigCandidate
	if S(before, "profile") == "review" && (st == "complete" || st == "completed") && strings.ToLower(strings.TrimSpace(S(in.Result, "verdict"))) == "pass" {
		c, e := s.store.GetCollection(S(before, "collection_id"))
		if e != nil {
			return ResultSubmitOutput{}, e
		}
		project, e := s.store.GetProject(S(c, "project_id"))
		if e != nil {
			return ResultSubmitOutput{}, e
		}
		candidates, e = s.deriveConfigCandidates(project, c)
		if e != nil {
			return ResultSubmitOutput{}, e
		}
	}
	t, e := s.store.SubmitResult(tid, in.Result, candidates...)
	if e != nil {
		return ResultSubmitOutput{}, e
	}
	return ResultSubmitOutput{Status: S(t, "status")}, nil
}

// deriveConfigCandidates computes the diff-derived configuration-knob
// candidate list (base...collection integration branch at the moment of the
// check) that the submit and acceptance seams thread into the pure policy
// validators. Git access lives here, at the seam, never inside the Store or
// policy.go. A git failure against an existing basis is returned loudly. When
// the diff basis cannot be derived at all (a non-Git project root or an absent
// base/integration branch) it yields no candidates AND records the absent
// basis as one deduped durable event so it is not mistaken for a genuine
// no-knob diff.
func (s *Service) deriveConfigCandidates(project, collection Row) ([]DerivedConfigCandidate, error) {
	w, e := NewGitWorkspace(s.store, s.cfg)
	if e != nil {
		return nil, e
	}
	outcome, e := w.DeriveConfigCandidatesWithBasis(project, collection)
	if e != nil {
		return nil, e
	}
	if outcome.BasisUnavailable != "" {
		s.recordAbsentConfigBasis(project, collection, outcome.BasisUnavailable)
	}
	return outcome.Candidates, nil
}

// recordAbsentConfigBasis surfaces an absent diff basis as at most one durable
// CONFIG_BASIS_UNAVAILABLE event per collection, naming the collection and the
// reason the three-dot candidate derivation could not proceed (which ref or
// state is missing). This makes an absent/never-created integration branch (or
// a non-Git root) auditable and distinguishable from a change that genuinely
// introduced no knobs. It is observability only — the caller continues with an
// empty candidate list — and never blocks or rejects. These seams hold the Git
// workspace outside the store lock, so Store.Event is safe, and the existing
// per-collection event scan keeps retries and repeated sweeps from spamming.
func (s *Service) recordAbsentConfigBasis(project, collection Row, reason string) {
	if s.store == nil {
		return
	}
	if events, e := s.store.EventsForCollection(S(collection, "id"), 200); e == nil {
		for _, ev := range events {
			if S(ev, "event_type") == "CONFIG_BASIS_UNAVAILABLE" {
				return
			}
		}
	}
	_ = s.store.Event(S(project, "id"), S(collection, "id"), "", "", "CONFIG_BASIS_UNAVAILABLE", Row{"reason": reason})
}

// projectForScope resolves the owning project for a memory/decision scope. An
// explicit project id resolves directly; when only a collection id is supplied it
// resolves the collection's stored owning project via the fail-closed
// ProjectForCollection store helper; and when neither is given it falls back to
// the ambient projectFromContext working-directory resolution (unchanged CLI
// behavior). Unlike a bare projectFromContext("") call, a durable COL-* id is
// NEVER resolved through the daemon working directory: an unknown or
// project-less collection fails closed instead of silently inheriting cwd.
func (s *Service) projectForScope(projectID, collectionID string) (Row, error) {
	if projectID != "" {
		return s.store.GetProject(projectID)
	}
	if collectionID != "" {
		return s.store.ProjectForCollection(collectionID)
	}
	return projectFromContext(s.store, "")
}

// MemoryAddInput is the input for `praector memory add`.
type MemoryAddInput struct {
	ProjectID    string
	CollectionID string
	TaskID       string // empty => PRAECTOR_TASK_ID
	Scope        string
	State        string
	Kind         string
	Text         string
}

// MemoryAddOutput is the created memory row.
type MemoryAddOutput struct {
	Memory Row
}

// MemoryAdd adds durable project/collection memory, resolving scope from the
// task or project context.
func (s *Service) MemoryAdd(ctx context.Context, in MemoryAddInput) (MemoryAddOutput, error) {
	if in.Text == "" {
		return MemoryAddOutput{}, fmt.Errorf("--text is required")
	}
	taskID := in.TaskID
	if taskID == "" {
		taskID = os.Getenv("PRAECTOR_TASK_ID")
	}
	pid, cid := "", ""
	if taskID != "" {
		t, e := s.store.GetTask(taskID)
		if e != nil {
			return MemoryAddOutput{}, e
		}
		pid, cid = S(t, "project_id"), S(t, "collection_id")
	} else {
		pr, e := s.projectForScope(in.ProjectID, in.CollectionID)
		if e != nil {
			return MemoryAddOutput{}, e
		}
		pid, cid = S(pr, "id"), in.CollectionID
	}
	scope := in.Scope
	if scope == "" {
		scope = "collection"
	}
	state := in.State
	if state == "" {
		state = "candidate"
	}
	kind := in.Kind
	if kind == "" {
		kind = "finding"
	}
	if scope != "collection" {
		cid = ""
	}
	r, e := s.store.AddMemory(pid, cid, taskID, scope, state, kind, in.Text, nil)
	if e != nil {
		return MemoryAddOutput{}, e
	}
	return MemoryAddOutput{Memory: r}, nil
}

// MemoryListInput is the input for `praector memory list`.
type MemoryListInput struct {
	ProjectID    string
	CollectionID string
	State        string
}

// MemoryListOutput is the typed result for `praector memory list`.
type MemoryListOutput struct {
	Memories Rows
}

// MemoryList lists durable memory for the resolved project.
func (s *Service) MemoryList(ctx context.Context, in MemoryListInput) (MemoryListOutput, error) {
	pr, e := s.projectForScope(in.ProjectID, in.CollectionID)
	if e != nil {
		return MemoryListOutput{}, e
	}
	state := in.State
	if state == "" {
		state = "accepted"
	}
	xs, e := s.store.ListMemories(S(pr, "id"), in.CollectionID, state)
	if e != nil {
		return MemoryListOutput{}, e
	}
	return MemoryListOutput{Memories: xs}, nil
}

// DecisionAddInput is the input for `praector decision add`.
type DecisionAddInput struct {
	ProjectID    string
	CollectionID string
	TaskID       string
	Statement    string
	Rationale    string
}

// DecisionAddOutput is the created decision row.
type DecisionAddOutput struct {
	Decision Row
}

// DecisionAdd adds a durable decision, resolving scope from task or project.
func (s *Service) DecisionAdd(ctx context.Context, in DecisionAddInput) (DecisionAddOutput, error) {
	if in.Statement == "" {
		return DecisionAddOutput{}, fmt.Errorf("--statement is required")
	}
	taskID := in.TaskID
	if taskID == "" {
		taskID = os.Getenv("PRAECTOR_TASK_ID")
	}
	pid, cid := "", ""
	if taskID != "" {
		t, e := s.store.GetTask(taskID)
		if e != nil {
			return DecisionAddOutput{}, e
		}
		pid, cid = S(t, "project_id"), S(t, "collection_id")
	} else {
		pr, e := s.projectForScope(in.ProjectID, in.CollectionID)
		if e != nil {
			return DecisionAddOutput{}, e
		}
		pid, cid = S(pr, "id"), in.CollectionID
	}
	r, e := s.store.AddDecision(pid, cid, taskID, in.Statement, in.Rationale, nil, nil)
	if e != nil {
		return DecisionAddOutput{}, e
	}
	return DecisionAddOutput{Decision: r}, nil
}

// ---------------------------------------------------------------------------
// Init op
// ---------------------------------------------------------------------------

// InitInput is the input for `praector init`.
//
// Root is an optional explicit target repository path. The CLI leaves it empty and
// keeps its cwd-based behavior; the MCP adapter requires an explicit root so a
// remote-init request is scoped to that requested target rather than the daemon
// process working directory.
type InitInput struct {
	Name       string
	BaseBranch string
	OpenCode   string
	Root       string
}

// InitOutput carries the registered project fields for display.
type InitOutput struct {
	Name            string
	ProjectID       string
	Marker          string
	BaseBranch      string
	OpenCodeCommand string
}

// Init registers a Git repository as a Praector project. When InitInput.Root is
// set, every filesystem/git write is scoped to that explicitly requested target;
// when it is empty the operation resolves the current working directory, matching
// the CLI's cwd-based behavior. Store registration is performed here at the same
// point the CLI does it today.
func (s *Service) Init(ctx context.Context, in InitInput) (InitOutput, error) {
	git, e := RequireBinary("git")
	if e != nil {
		return InitOutput{}, e
	}
	if _, e = RequireBinary("dolt"); e != nil {
		return InitOutput{}, e
	}
	rootArgs := []string{git, "rev-parse", "--show-toplevel"}
	if in.Root != "" {
		rootArgs = []string{git, "-C", in.Root, "rev-parse", "--show-toplevel"}
	}
	r, e := Run(ctx, rootArgs, "", nil, "", true)
	if e != nil {
		if in.Root != "" {
			return InitOutput{}, fmt.Errorf("target %q is not inside a Git work tree: %w", in.Root, e)
		}
		return InitOutput{}, e
	}
	root, e := filepath.Abs(strings.TrimSpace(r.Stdout))
	if e != nil {
		return InitOutput{}, e
	}
	marker := filepath.Join(root, ".praector", "project.toml")
	var cfg ProjectConfig
	if _, e = os.Stat(marker); e == nil {
		cfg, e = ReadProjectMarker(marker)
		if e != nil {
			return InitOutput{}, e
		}
	} else {
		br := in.BaseBranch
		if br == "" {
			q, _ := Run(ctx, []string{git, "-C", root, "branch", "--show-current"}, "", nil, "", false)
			br = strings.TrimSpace(q.Stdout)
			if br == "" {
				br = "main"
			}
		}
		oc := in.OpenCode
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
	if in.Name != "" {
		cfg.Name = in.Name
	}
	if in.BaseBranch != "" {
		cfg.BaseBranch = in.BaseBranch
	}
	if in.OpenCode != "" {
		cfg.OpenCodeCommand = in.OpenCode
	}
	marker, e = WriteProjectMarker(cfg)
	if e != nil {
		return InitOutput{}, e
	}
	q, e := Run(ctx, []string{git, "-C", root, "rev-parse", "--git-path", "info/exclude"}, "", nil, "", true)
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
		return InitOutput{}, e
	}
	s.store = store
	if _, e = store.RegisterProject(cfg); e != nil {
		return InitOutput{}, e
	}
	return InitOutput{Name: cfg.Name, ProjectID: cfg.ProjectID, Marker: marker, BaseBranch: cfg.BaseBranch, OpenCodeCommand: cfg.OpenCodeCommand}, nil
}

// ---------------------------------------------------------------------------
// Doctor op
// ---------------------------------------------------------------------------

// DoctorCheck is one diagnostic line plus its severity.
type DoctorCheck struct {
	Text   string
	Status string // "ok" | "warn" | "fail"
}

// DoctorInput is the input for `praector doctor`. An empty ProjectID preserves the
// CLI's cwd-based project-marker discovery; an explicit ProjectID (used by the MCP
// bridge, which has no safe implicit cwd) resolves that registered project from
// durable state instead of inspecting the process working directory.
type DoctorInput struct {
	ProjectID string
}

// DoctorOutput is the ordered diagnostic results plus the failure count.
type DoctorOutput struct {
	Checks   []DoctorCheck
	Failures int
}

// Doctor validates local dependencies and project registration. The store is
// opened non-fatally (a Dolt failure is one diagnostic, not a hard abort).
func (s *Service) Doctor(ctx context.Context, in DoctorInput) (DoctorOutput, error) {
	out := DoctorOutput{}
	add := func(status, text string) {
		out.Checks = append(out.Checks, DoctorCheck{Text: text, Status: status})
		if status == "fail" {
			out.Failures++
		}
	}
	for _, b := range []string{"git", "dolt", "opencode"} {
		if p, e := RequireBinary(b); e == nil {
			add("ok", fmt.Sprintf("%s: %s", b, p))
		} else {
			add("fail", fmt.Sprintf("%s: not found", b))
		}
	}
	if p, e := RequireBinary("gh"); e == nil {
		add("ok", fmt.Sprintf("gh: %s (collection PR delivery available)", p))
	} else {
		add("warn", "gh: not found (required only for `praector collection pr`)")
	}
	var store *Store
	if st, _, e := newStore(); e == nil {
		store = st
		add("ok", fmt.Sprintf("Dolt state: %s", st.StateDir))
	} else {
		add("fail", fmt.Sprintf("Dolt state: %v", e))
	}
	// An explicit project identifier (MCP bridge) resolves the registered project
	// from durable state and never inspects the process working directory.
	if in.ProjectID != "" {
		if store == nil {
			add("fail", fmt.Sprintf("project %s: Dolt state unavailable", in.ProjectID))
			return out, nil
		}
		pr, e := store.GetProject(in.ProjectID)
		if e != nil {
			add("fail", fmt.Sprintf("project %s: %v", in.ProjectID, e))
			return out, nil
		}
		pc := ProjectConfigFromRow(pr)
		add("ok", fmt.Sprintf("project: %s (%s)", pc.Name, pc.ProjectID))
		if st, e := os.Stat(pc.Root); e != nil || !st.IsDir() {
			add("fail", fmt.Sprintf("project repository does not exist: %s", pc.Root))
		} else {
			add("ok", fmt.Sprintf("project repository: %s", pc.Root))
		}
		if _, e := pc.ResolvedOpenCode(); e != nil {
			add("fail", fmt.Sprintf("project OpenCode command: %v", e))
		} else {
			add("ok", fmt.Sprintf("project OpenCode command: %s", pc.OpenCodeCommand))
		}
		return out, nil
	}
	if marker, ok := FindProjectMarker(""); ok {
		pc, e := ReadProjectMarker(marker)
		if e != nil {
			add("fail", fmt.Sprintf("project marker: %v", e))
		} else {
			add("ok", fmt.Sprintf("project: %s (%s)", pc.Name, pc.ProjectID))
			if store != nil {
				if _, e = store.GetProject(pc.ProjectID); e == nil {
					add("ok", "project registered in Dolt")
				} else {
					add("fail", fmt.Sprintf("project registered in Dolt: %v", e))
				}
			}
			if _, e = pc.ResolvedOpenCode(); e == nil {
				add("ok", fmt.Sprintf("project OpenCode command: %s", pc.OpenCodeCommand))
			} else {
				add("fail", fmt.Sprintf("project OpenCode command: %v", e))
			}
			if v := os.Getenv("VIRTUAL_ENV"); v != "" {
				va, _ := filepath.Abs(v)
				ra, _ := filepath.Abs(pc.Root)
				rel, _ := filepath.Rel(ra, va)
				if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
					add("warn", fmt.Sprintf("active virtualenv outside project will be stripped from managed worker commands by default: %s", va))
					out.Checks = append(out.Checks, DoctorCheck{Text: "Prefer running Praector from a neutral shell/tool install. Set PRAECTOR_SANITIZE_PARENT_VENV=0 only if your OpenCode installation intentionally needs that parent venv.", Status: "note"})
				}
			}
		}
	} else {
		add("warn", "no project marker in current directory tree")
	}
	return out, nil
}

// acceptActor is shared by the accept/pr delivery adapters to attribute an
// acceptance or PR record to the operating user.
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
