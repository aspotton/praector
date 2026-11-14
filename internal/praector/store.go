package praector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/datascape/praector/internal/assets"
)

type DependencyCycleError struct{ Msg string }

func (e *DependencyCycleError) Error() string { return e.Msg }

type Store struct {
	Cfg      GlobalConfig
	StateDir string
	Dolt     string

	// emitMu guards emitter, the optional event fan-out sink wired by the MCP
	// server (DEC-48d49461e1). It is nil on the CLI/`events` path, so an ordinary
	// store write performs no fan-out and stays byte-equivalent to before.
	emitMu  sync.RWMutex
	emitter eventSink
}

// eventRecord is the store emit seam's view of one durable events row: the
// identifiers and ordering key a push consumer needs (no payload JSON). The SSE
// layer reconstructs the notification data from these fields, so the store does
// not push payload bytes onto the write path.
type eventRecord struct {
	ID           string
	ProjectID    string
	CollectionID string
	TaskID       string
	AttemptID    string
	EventType    string
	CreatedAt    string
}

// eventSink is the optional fan-out target wired onto a running daemon's store.
// publish must be non-blocking: the store write path may only hand off a pending
// payload, never block on delivery (the SSE layer flushes it off the file lock).
type eventSink interface {
	publish(eventRecord)
}

// SetEventSink installs (or, with nil, clears) the event fan-out sink. It is
// called by the MCP server wiring, never by ordinary store callers, so the CLI
// event read path is unaffected.
func (s *Store) SetEventSink(sink eventSink) {
	s.emitMu.Lock()
	s.emitter = sink
	s.emitMu.Unlock()
}

// CloseEventSink clears the sink and, if it exposes Close, shuts it down so open
// SSE streams and the drain goroutine terminate on daemon shutdown.
func (s *Store) CloseEventSink() {
	s.emitMu.Lock()
	sink := s.emitter
	s.emitter = nil
	s.emitMu.Unlock()
	if c, ok := sink.(interface{ Close() }); ok {
		c.Close()
	}
}

// emitEvent hands one already-inserted event's fan-out payload to the installed
// sink. It is the single shared emit helper every event writer calls after its
// row is committed (FORK B / DEC-48d49461e1). It never blocks: the sink's publish
// is a non-blocking hand-off and the actual stream delivery runs outside the
// process-wide Dolt lock. With no sink (CLI, tests) it is a no-op.
func (s *Store) emitEvent(rec eventRecord) {
	s.emitMu.RLock()
	sink := s.emitter
	s.emitMu.RUnlock()
	if sink != nil {
		sink.publish(rec)
	}
}

func NewStore(cfg GlobalConfig) (*Store, error) {
	d, err := RequireBinary("dolt")
	if err != nil {
		return nil, err
	}
	return &Store{Cfg: cfg, StateDir: cfg.StateDir(), Dolt: d}, nil
}

func (s *Store) withLock(fn func() error) error {
	if err := os.MkdirAll(s.Cfg.Home, 0755); err != nil {
		return err
	}
	l, err := AcquireFileLock(s.Cfg.LockFile(), false)
	if err != nil {
		return err
	}
	defer l.Close()
	return fn()
}

func (s *Store) run(args []string, input string, check bool) (CmdResult, error) {
	return Run(context.Background(), args, s.StateDir, nil, input, check)
}
func (s *Store) queryLocked(q string) (Rows, error) {
	r, err := s.run([]string{s.Dolt, "sql", "-r", "json", "-q", q}, "", true)
	if err != nil {
		return nil, err
	}
	txt := strings.TrimSpace(r.Stdout)
	if txt == "" {
		return Rows{}, nil
	}
	dec := json.NewDecoder(strings.NewReader(txt))
	dec.UseNumber()
	var p struct {
		Rows Rows `json:"rows"`
	}
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("unable to parse Dolt JSON output for query:\n%s\nOutput:\n%s: %w", q, txt, err)
	}
	return p.Rows, nil
}
func (s *Store) Query(q string) (Rows, error) {
	var rows Rows
	err := s.withLock(func() error { var e error; rows, e = s.queryLocked(q); return e })
	return rows, err
}
func (s *Store) mutateLocked(q string) error {
	_, err := s.run([]string{s.Dolt, "sql", "-q", q}, "", true)
	return err
}
func (s *Store) semanticCommitLocked(msg string) error {
	_, _ = s.run([]string{s.Dolt, "add", "-A"}, "", false)
	r, err := s.run([]string{s.Dolt, "commit", "-m", msg}, "", false)
	if err != nil {
		return err
	}
	if r.Code != 0 {
		d := strings.ToLower(r.Stderr + "\n" + r.Stdout)
		if !strings.Contains(d, "nothing to commit") && !strings.Contains(d, "no changes") {
			return fmt.Errorf("Dolt commit failed: %s", strings.TrimSpace(firstNonEmpty(r.Stderr, r.Stdout)))
		}
	}
	return nil
}
func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}
func (s *Store) Mutate(q, commit string) error {
	return s.withLock(func() error {
		if err := s.mutateLocked(q); err != nil {
			return err
		}
		if commit != "" {
			return s.semanticCommitLocked(commit)
		}
		return nil
	})
}

func (s *Store) Bootstrap() error {
	if err := os.MkdirAll(s.StateDir, 0755); err != nil {
		return err
	}
	return s.withLock(func() error {
		if _, err := os.Stat(filepath.Join(s.StateDir, ".dolt")); os.IsNotExist(err) {
			if _, e := s.run([]string{s.Dolt, "init"}, "", true); e != nil {
				return e
			}
			_, _ = s.run([]string{s.Dolt, "config", "--local", "--add", "user.name", "praector"}, "", false)
			_, _ = s.run([]string{s.Dolt, "config", "--local", "--add", "user.email", "praector@local"}, "", false)
		}
		schema, err := assets.FS.ReadFile("schema.sql")
		if err != nil {
			return err
		}
		if _, err = s.run([]string{s.Dolt, "sql"}, string(schema), true); err != nil {
			return err
		}
		if err = s.semanticCommitLocked("praector: initialize schema"); err != nil {
			return err
		}
		return s.repairInvariantsLocked()
	})
}

func (s *Store) repairInvariantsLocked() error {
	rows, err := s.queryLocked("SELECT id FROM attempts WHERE status='SUCCEEDED' AND exit_code IS NOT NULL AND exit_code <> 0")
	if err != nil {
		return err
	}
	if len(rows) > 0 {
		if err = s.mutateLocked("UPDATE attempts SET status='FAILED' WHERE status='SUCCEEDED' AND exit_code IS NOT NULL AND exit_code <> 0"); err != nil {
			return err
		}
		if err = s.semanticCommitLocked(fmt.Sprintf("praector: repair %d inconsistent attempt status(es)", len(rows))); err != nil {
			return err
		}
	}
	if _, err = s.repairRevisionBaselineProvenanceLocked(); err != nil {
		return err
	}
	_, err = s.repairLegacyDependencyCyclesLockedAll()
	return err
}

func dependencyCycle(edges [][2]string) []string {
	adj := map[string][]string{}
	nodes := map[string]bool{}
	for _, e := range edges {
		adj[e[0]] = append(adj[e[0]], e[1])
		nodes[e[0]] = true
		nodes[e[1]] = true
	}
	visiting := map[string]bool{}
	visited := map[string]bool{}
	stack := []string{}
	index := map[string]int{}
	var visit func(string) []string
	visit = func(n string) []string {
		if visited[n] {
			return nil
		}
		if visiting[n] {
			i := index[n]
			out := append([]string{}, stack[i:]...)
			return append(out, n)
		}
		visiting[n] = true
		index[n] = len(stack)
		stack = append(stack, n)
		for _, next := range adj[n] {
			if c := visit(next); c != nil {
				return c
			}
		}
		stack = stack[:len(stack)-1]
		delete(index, n)
		delete(visiting, n)
		visited[n] = true
		return nil
	}
	keys := []string{}
	for n := range nodes {
		keys = append(keys, n)
	}
	sort.Strings(keys)
	for _, n := range keys {
		if c := visit(n); c != nil {
			return c
		}
	}
	return nil
}

func (s *Store) dependencyEdgesLocked(cid string) ([][2]string, error) {
	rows, err := s.queryLocked("SELECT from_task_id,to_task_id FROM task_edges WHERE collection_id=" + SQLLiteral(cid) + " AND relation='depends_on'")
	if err != nil {
		return nil, err
	}
	out := make([][2]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, [2]string{S(r, "from_task_id"), S(r, "to_task_id")})
	}
	return out, nil
}
func dependencyPathInEdges(edges [][2]string, start, target string) []string {
	if start == target {
		return []string{start}
	}
	adj := map[string][]string{}
	for _, e := range edges {
		adj[e[0]] = append(adj[e[0]], e[1])
	}
	type item struct {
		n string
		p []string
	}
	q := []item{{start, []string{start}}}
	seen := map[string]bool{start: true}
	for len(q) > 0 {
		x := q[0]
		q = q[1:]
		for _, n := range adj[x.n] {
			if n == target {
				return append(append([]string{}, x.p...), n)
			}
			if !seen[n] {
				seen[n] = true
				p := append(append([]string{}, x.p...), n)
				q = append(q, item{n, p})
			}
		}
	}
	return nil
}
func (s *Store) assertDependencyAcyclicLocked(cid string, pros [][2]string) error {
	edges, err := s.dependencyEdgesLocked(cid)
	if err != nil {
		return err
	}
	for _, e := range pros {
		if e[0] == e[1] {
			return &DependencyCycleError{fmt.Sprintf("Dependency %s->%s would create a self-cycle in %s", e[0], e[1], cid)}
		}
		exists := false
		for _, x := range edges {
			if x == e {
				exists = true
				break
			}
		}
		if exists {
			continue
		}
		if p := dependencyPathInEdges(edges, e[1], e[0]); p != nil {
			cycle := append([]string{e[0]}, p...)
			return &DependencyCycleError{fmt.Sprintf("Dependency %s->%s would create a cycle in %s: %s", e[0], e[1], cid, strings.Join(cycle, " -> "))}
		}
		edges = append(edges, e)
	}
	return nil
}

// assertDependencyTasksInCollectionLocked prevents unresolved or cross-Collection
// hard dependencies from entering durable state. Runnable-task queries join
// dependency edges back to tasks, so a dangling edge would otherwise disappear
// from scheduler gating and could make blocked work incorrectly claimable.
func (s *Store) assertDependencyTasksInCollectionLocked(cid string, taskIDs []string) error {
	for _, taskID := range uniqueStrings(taskIDs) {
		if taskID == "" {
			continue
		}
		rows, err := s.queryLocked("SELECT id,collection_id FROM tasks WHERE id=" + SQLLiteral(taskID) + " LIMIT 1")
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return fmt.Errorf("dependency task not found: %s", taskID)
		}
		actual := S(rows[0], "collection_id")
		if actual != cid {
			return fmt.Errorf("dependency task %s belongs to collection %s, not %s", taskID, actual, cid)
		}
	}
	return nil
}

func (s *Store) repairLegacyDependencyCyclesLockedAll() (int, error) {
	rows, err := s.queryLocked("SELECT id FROM collections ORDER BY created_at")
	if err != nil {
		return 0, err
	}
	repaired := 0
	for _, row := range rows {
		count, _, err := s.repairLegacyDependencyCyclesLocked(S(row, "id"))
		if err != nil {
			return repaired, err
		}
		repaired += count
	}
	return repaired, nil
}

func (s *Store) repairLegacyDependencyCyclesLocked(collectionID string) (int, []string, error) {
	taskRows, err := s.queryLocked("SELECT id,project_id,profile,status FROM tasks WHERE collection_id=" + SQLLiteral(collectionID))
	if err != nil {
		return 0, nil, err
	}
	taskByID := map[string]Row{}
	projectID := ""
	for _, row := range taskRows {
		taskByID[S(row, "id")] = row
		if projectID == "" {
			projectID = S(row, "project_id")
		}
	}
	edges, err := s.dependencyEdgesLocked(collectionID)
	if err != nil {
		return 0, nil, err
	}
	type repair struct{ from, to, reason string }
	var repairs []repair

	for {
		var candidate *repair
		for _, edge := range edges {
			source := taskByID[edge[0]]
			target := taskByID[edge[1]]
			reason := ""
			if S(target, "profile") == "plan_review" && S(source, "profile") == "implement" {
				reason = "legacy_plan_review_waited_for_implementation"
			} else if S(target, "profile") == "plan_review" && S(source, "profile") == "coordinator" {
				reason = "legacy_plan_review_waited_for_coordinator"
			}
			if reason == "" || dependencyPathInEdges(edges, edge[1], edge[0]) == nil {
				continue
			}
			r := repair{from: edge[0], to: edge[1], reason: reason}
			candidate = &r
			break
		}
		if candidate == nil {
			break
		}
		filtered := make([][2]string, 0, len(edges)-1)
		for _, edge := range edges {
			if edge[0] == candidate.from && edge[1] == candidate.to {
				continue
			}
			filtered = append(filtered, edge)
		}
		edges = filtered
		repairs = append(repairs, *candidate)
	}

	unresolved := dependencyCycle(edges)
	if len(repairs) == 0 {
		return 0, unresolved, nil
	}
	now := UTCNow()
	statements := make([]string, 0, len(repairs)*2)
	var repairedEvents []eventRecord
	for _, r := range repairs {
		statements = append(statements,
			"DELETE FROM task_edges WHERE collection_id="+SQLLiteral(collectionID)+" AND relation='depends_on' AND from_task_id="+SQLLiteral(r.from)+" AND to_task_id="+SQLLiteral(r.to),
		)
		if projectID != "" {
			payload := JSONDumps(Row{"from_task_id": r.from, "to_task_id": r.to, "reason": r.reason})
			eid := MakeID("EVT")
			statements = append(statements,
				"INSERT INTO events (id,project_id,collection_id,task_id,attempt_id,event_type,payload_json,created_at) VALUES ("+
					SQLLiteral(eid)+","+SQLLiteral(projectID)+","+SQLLiteral(collectionID)+","+SQLLiteral(r.to)+",NULL,'LEGACY_DEPENDENCY_CYCLE_REPAIRED',"+SQLLiteral(payload)+","+SQLLiteral(now)+")",
			)
			repairedEvents = append(repairedEvents, eventRecord{ID: eid, ProjectID: projectID, CollectionID: collectionID, TaskID: r.to, EventType: "LEGACY_DEPENDENCY_CYCLE_REPAIRED", CreatedAt: now})
		}
	}
	if err := s.mutateLocked(strings.Join(statements, ";")); err != nil {
		return 0, unresolved, err
	}
	if err := s.semanticCommitLocked(fmt.Sprintf("praector: repair %d legacy dependency cycle edge(s) in %s", len(repairs), collectionID)); err != nil {
		return 0, unresolved, err
	}
	for _, rec := range repairedEvents {
		s.emitEvent(rec)
	}
	return len(repairs), unresolved, nil
}

// RepairLegacyDependencyCycles normalizes the two hard-dependency cycle shapes
// known to have been emitted by legacy Praector releases. Unknown cycles remain in
// place for collection-local quarantine by the scheduler.
func (s *Store) RepairLegacyDependencyCycles(collectionID string) (int, []string, error) {
	var repaired int
	var unresolved []string
	err := s.withLock(func() error {
		var err error
		repaired, unresolved, err = s.repairLegacyDependencyCyclesLocked(collectionID)
		return err
	})
	return repaired, unresolved, err
}

func (s *Store) repairRevisionBaselineProvenanceLocked() (int, error) {
	rows, err := s.queryLocked("SELECT task_id,payload_json,created_at FROM events WHERE event_type='CRITIC_PLAN_BASELINE' ORDER BY created_at")
	if err != nil {
		return 0, err
	}
	latest := map[string]Row{}
	for _, row := range rows {
		if id := S(row, "task_id"); id != "" {
			latest[id] = row
		}
	}
	var statements []string
	var baselineEvents []eventRecord
	for criticID, row := range latest {
		payload := JSONMap(S(row, "payload_json"))
		if S(payload, "source") != "compatibility_plan_seal" {
			continue
		}
		planReviewID := S(payload, "plan_review_task_id")
		fingerprint := S(payload, "fingerprint")
		if planReviewID == "" || fingerprint == "" {
			continue
		}
		tasks, err := s.queryLocked("SELECT id,collection_id,requested_by_task_id FROM tasks WHERE id IN (" + SQLLiteral(criticID) + "," + SQLLiteral(planReviewID) + ")")
		if err != nil {
			return 0, err
		}
		byID := map[string]Row{}
		for _, task := range tasks {
			byID[S(task, "id")] = task
		}
		critic, okCritic := byID[criticID]
		planReview, okReview := byID[planReviewID]
		if !okCritic || !okReview {
			continue
		}
		coordinatorID := S(critic, "requested_by_task_id")
		if coordinatorID == "" || S(planReview, "requested_by_task_id") != coordinatorID {
			continue
		}
		collectionID := S(critic, "collection_id")
		revisions, err := s.queryLocked("SELECT id,project_id,coordinator_task_id FROM collection_revisions WHERE collection_id=" + SQLLiteral(collectionID) + " AND coordinator_task_id=" + SQLLiteral(coordinatorID) + " LIMIT 1")
		if err != nil {
			return 0, err
		}
		if len(revisions) == 0 {
			continue
		}
		revision := revisions[0]
		projectID := S(revision, "project_id")
		if projectID == "" {
			collections, err := s.queryLocked("SELECT project_id FROM collections WHERE id=" + SQLLiteral(collectionID) + " LIMIT 1")
			if err != nil {
				return 0, err
			}
			if len(collections) > 0 {
				projectID = S(collections[0], "project_id")
			}
		}
		if projectID == "" {
			continue
		}
		correction := Row{
			"critic_task_id":      criticID,
			"fingerprint":         fingerprint,
			"source":              "revision_plan_seal",
			"plan_review_task_id": planReviewID,
			"corrected_from":      "compatibility_plan_seal",
			"revision_id":         revision["id"],
		}
		eid := MakeID("EVT")
		eat := UTCNow()
		statements = append(statements,
			"INSERT INTO events (id,project_id,collection_id,task_id,attempt_id,event_type,payload_json,created_at) VALUES ("+
				SQLLiteral(eid)+","+SQLLiteral(projectID)+","+SQLLiteral(collectionID)+","+SQLLiteral(criticID)+",NULL,'CRITIC_PLAN_BASELINE',"+SQLLiteral(JSONDumps(correction))+","+SQLLiteral(eat)+")",
		)
		baselineEvents = append(baselineEvents, eventRecord{ID: eid, ProjectID: projectID, CollectionID: collectionID, TaskID: criticID, EventType: "CRITIC_PLAN_BASELINE", CreatedAt: eat})
	}
	if len(statements) == 0 {
		return 0, nil
	}
	if err := s.mutateLocked(strings.Join(statements, ";")); err != nil {
		return 0, err
	}
	if err := s.semanticCommitLocked(fmt.Sprintf("praector: repair %d revision critic baseline provenance label(s)", len(statements))); err != nil {
		return 0, err
	}
	for _, rec := range baselineEvents {
		s.emitEvent(rec)
	}
	return len(statements), nil
}

// Project operations.
func (s *Store) RegisterProject(cfg ProjectConfig) (Row, error) {
	now := UTCNow()
	rows, err := s.Query("SELECT * FROM projects WHERE id=" + SQLLiteral(cfg.ProjectID) + " OR root_path=" + SQLLiteral(cfg.Root) + " LIMIT 1")
	if err != nil {
		return nil, err
	}
	cj := JSONDumps(ProjectConfigJSON(cfg))
	if len(rows) > 0 {
		cur := rows[0]
		id := S(cur, "id")
		if S(cur, "name") == cfg.Name && S(cur, "root_path") == cfg.Root && S(cur, "base_branch") == cfg.BaseBranch && S(cur, "config_json") == cj {
			return s.GetProject(id)
		}
		q := fmt.Sprintf("UPDATE projects SET name=%s, root_path=%s, base_branch=%s, config_json=%s, updated_at=%s WHERE id=%s", SQLLiteral(cfg.Name), SQLLiteral(cfg.Root), SQLLiteral(cfg.BaseBranch), SQLLiteral(cj), SQLLiteral(now), SQLLiteral(id))
		if err = s.Mutate(q, "project "+id+": refresh configuration"); err != nil {
			return nil, err
		}
		return s.GetProject(id)
	}
	q := fmt.Sprintf("INSERT INTO projects (id,name,root_path,base_branch,config_json,created_at,updated_at) VALUES (%s,%s,%s,%s,%s,%s,%s)", SQLLiteral(cfg.ProjectID), SQLLiteral(cfg.Name), SQLLiteral(cfg.Root), SQLLiteral(cfg.BaseBranch), SQLLiteral(cj), SQLLiteral(now), SQLLiteral(now))
	if err = s.Mutate(q, "project "+cfg.ProjectID+": register "+cfg.Name); err != nil {
		return nil, err
	}
	return s.GetProject(cfg.ProjectID)
}
func (s *Store) GetProject(id string) (Row, error) {
	rows, err := s.Query("SELECT * FROM projects WHERE id=" + SQLLiteral(id) + " OR name=" + SQLLiteral(id) + " OR root_path=" + SQLLiteral(id) + " LIMIT 1")
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("project not found: %s", id)
	}
	r := rows[0]
	r["config"] = JSONMap(S(r, "config_json"))
	return r, nil
}
func (s *Store) ListProjects() (Rows, error) {
	return s.Query("SELECT * FROM projects ORDER BY created_at")
}

// Collection operations.
func (s *Store) CreateCollection(projectID, title, obj string) (Row, error) {
	id := MakeID("COL")
	branch := "praector/collections/" + strings.ToLower(id)
	now := UTCNow()
	q := fmt.Sprintf("INSERT INTO collections (id,project_id,title,objective,status,integration_branch,created_at,updated_at) VALUES (%s,%s,%s,%s,'RUNNING',%s,%s,%s)", SQLLiteral(id), SQLLiteral(projectID), SQLLiteral(title), SQLLiteral(obj), SQLLiteral(branch), SQLLiteral(now), SQLLiteral(now))
	if err := s.Mutate(q, "collection "+id+": create "+title); err != nil {
		return nil, err
	}
	_ = s.Event(projectID, id, "", "", "COLLECTION_CREATED", Row{"title": title, "objective": obj})
	return s.GetCollection(id)
}
func (s *Store) CreateCollectionWithCoordinator(projectID, title, obj, coordObj string) (Row, Row, error) {
	cid := MakeID("COL")
	tid := MakeID("T")
	branch := "praector/collections/" + strings.ToLower(cid)
	now := UTCNow()
	key := DedupeKey("coordinator", coordObj)
	cp := JSONDumps(Row{"title": title, "objective": obj})
	tp := JSONDumps(Row{"profile": "coordinator", "title": "Initial coordination", "reason": "Collection entry point"})
	cevt := MakeID("EVT")
	tevt := MakeID("EVT")
	err := s.withLock(func() error {
		q := fmt.Sprintf("INSERT INTO collections (id,project_id,title,objective,status,integration_branch,created_at,updated_at) VALUES (%s,%s,%s,%s,'RUNNING',%s,%s,%s);INSERT INTO tasks (id,project_id,collection_id,parent_task_id,requested_by_task_id,profile,title,objective,reason,status,priority,depth,workspace_mode,base_commit,dedupe_key,created_at,updated_at) VALUES (%s,%s,%s,NULL,NULL,'coordinator','Initial coordination',%s,'Collection entry point','READY',50,0,'collection',NULL,%s,%s,%s);INSERT INTO events (id,project_id,collection_id,task_id,attempt_id,event_type,payload_json,created_at) VALUES (%s,%s,%s,NULL,NULL,'COLLECTION_CREATED',%s,%s);INSERT INTO events (id,project_id,collection_id,task_id,attempt_id,event_type,payload_json,created_at) VALUES (%s,%s,%s,%s,NULL,'TASK_CREATED',%s,%s)", SQLLiteral(cid), SQLLiteral(projectID), SQLLiteral(title), SQLLiteral(obj), SQLLiteral(branch), SQLLiteral(now), SQLLiteral(now), SQLLiteral(tid), SQLLiteral(projectID), SQLLiteral(cid), SQLLiteral(coordObj), SQLLiteral(key), SQLLiteral(now), SQLLiteral(now), SQLLiteral(cevt), SQLLiteral(projectID), SQLLiteral(cid), SQLLiteral(cp), SQLLiteral(now), SQLLiteral(tevt), SQLLiteral(projectID), SQLLiteral(cid), SQLLiteral(tid), SQLLiteral(tp), SQLLiteral(now))
		if e := s.mutateLocked(q); e != nil {
			return e
		}
		return s.semanticCommitLocked("collection " + cid + ": create with coordinator " + tid)
	})
	if err != nil {
		return nil, nil, err
	}
	// Fan out the two creation rows after the file lock releases. (COLLECTION_CREATED
	// usually fires before a listener is wired, so it is replay-only there; the
	// TASK_CREATED row reaches a connected observer live.)
	s.emitEvent(eventRecord{ID: cevt, ProjectID: projectID, CollectionID: cid, EventType: "COLLECTION_CREATED", CreatedAt: now})
	s.emitEvent(eventRecord{ID: tevt, ProjectID: projectID, CollectionID: cid, TaskID: tid, EventType: "TASK_CREATED", CreatedAt: now})
	c, err := s.GetCollection(cid)
	if err != nil {
		return nil, nil, err
	}
	t, err := s.GetTask(tid)
	return c, t, err
}
func (s *Store) GetCollection(id string) (Row, error) {
	rows, err := s.Query("SELECT * FROM collections WHERE id=" + SQLLiteral(id) + " LIMIT 1")
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("collection not found: %s", id)
	}
	return rows[0], nil
}

// ProjectForCollection resolves the project that OWNS a collection directly from
// the collection's stored project_id (the same `SELECT project_id FROM
// collections WHERE id=?` pattern used elsewhere in the store). It fails closed:
// an unknown collection or a collection that references no resolvable project is
// an error rather than an empty result, so a caller that was handed only a COL-*
// id can resolve its owning project without ever falling back to an ambient
// process working directory.
func (s *Store) ProjectForCollection(collectionID string) (Row, error) {
	rows, err := s.Query("SELECT project_id FROM collections WHERE id=" + SQLLiteral(collectionID) + " LIMIT 1")
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("collection not found: %s", collectionID)
	}
	projectID := S(rows[0], "project_id")
	if projectID == "" {
		return nil, fmt.Errorf("collection %s has no owning project", collectionID)
	}
	return s.GetProject(projectID)
}
func (s *Store) ListCollections(projectID string) (Rows, error) {
	where := ""
	if projectID != "" {
		where = " WHERE project_id=" + SQLLiteral(projectID)
	}
	return s.Query("SELECT * FROM collections" + where + " ORDER BY created_at DESC")
}
func (s *Store) ListCollectionRevisions(cid, status string) (Rows, error) {
	where := "collection_id=" + SQLLiteral(cid)
	if status != "" {
		where += " AND status=" + SQLLiteral(status)
	}
	return s.Query("SELECT * FROM collection_revisions WHERE " + where + " ORDER BY created_at")
}
func (s *Store) UpdateCollectionWorkspace(cid, wt string) error {
	return s.Mutate("UPDATE collections SET integration_worktree="+SQLLiteral(wt)+",updated_at="+SQLLiteral(UTCNow())+" WHERE id="+SQLLiteral(cid), "")
}
func (s *Store) SetCollectionStatus(cid, status string, commit bool) error {
	msg := ""
	if commit {
		msg = "collection " + cid + ": " + strings.ToLower(status)
	}
	// Slice 3: emit a durable event for a real transition into COMPLETE or
	// BLOCKED so a push consumer learns of the terminal/decision state. A
	// repeated no-op write (same status) emits ZERO events: the before value is
	// read inside the same lock and the INSERT is only appended on a real
	// transition. The existing Dolt commit-message behavior is unchanged.
	track := status == CollectionComplete || status == CollectionBlocked
	var evt *eventRecord
	err := s.withLock(func() error {
		stmts := []string{"UPDATE collections SET status=" + SQLLiteral(status) + ",updated_at=" + SQLLiteral(UTCNow()) + " WHERE id=" + SQLLiteral(cid)}
		if track {
			cur, e := s.queryLocked("SELECT project_id,status FROM collections WHERE id=" + SQLLiteral(cid))
			if e != nil {
				return e
			}
			if len(cur) > 0 && S(cur[0], "status") != status {
				id := MakeID("EVT")
				now := UTCNow()
				payload := JSONDumps(Row{"from": S(cur[0], "status"), "to": status})
				stmts = append(stmts, "INSERT INTO events (id,project_id,collection_id,task_id,attempt_id,event_type,payload_json,created_at) VALUES ("+SQLLiteral(id)+","+nullString(S(cur[0], "project_id"))+","+SQLLiteral(cid)+",NULL,NULL,"+SQLLiteral(statusCollectionEvent(status))+","+SQLLiteral(payload)+","+SQLLiteral(now)+")")
				evt = &eventRecord{ID: id, ProjectID: S(cur[0], "project_id"), CollectionID: cid, EventType: statusCollectionEvent(status), CreatedAt: now}
			}
		}
		if e := s.mutateLocked(strings.Join(stmts, ";")); e != nil {
			return e
		}
		if msg != "" {
			return s.semanticCommitLocked(msg)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if evt != nil {
		s.emitEvent(*evt)
	}
	return nil
}

// statusCollectionEvent maps a collection terminal status to its event type,
// consistent with the existing COLLECTION_* naming convention.
func statusCollectionEvent(status string) string {
	if status == CollectionBlocked {
		return "COLLECTION_BLOCKED"
	}
	return "COLLECTION_COMPLETE"
}

// statusTaskEvent maps a task terminal status to its event type, consistent with
// the existing TASK_* naming convention.
func statusTaskEvent(status string) string {
	if status == TaskFailed {
		return "TASK_FAILED"
	}
	return "TASK_COMPLETE"
}

func (s *Store) CountTasks(cid string) (int, error) {
	r, e := s.Query("SELECT COUNT(*) AS n FROM tasks WHERE collection_id=" + SQLLiteral(cid))
	if e != nil || len(r) == 0 {
		return 0, e
	}
	return I(r[0], "n"), nil
}
func (s *Store) ChildCount(tid string) (int, error) {
	r, e := s.Query("SELECT COUNT(*) AS n FROM tasks WHERE parent_task_id=" + SQLLiteral(tid))
	if e != nil || len(r) == 0 {
		return 0, e
	}
	return I(r[0], "n"), nil
}

type CreateTaskOptions struct {
	ProjectID, CollectionID, Profile, Title, Objective, Reason            string
	ParentTaskID, RequestedByTaskID                                       string
	DependsOn                                                             []string
	WorkspaceMode, BaseCommit                                             string
	Priority, Depth                                                       int
	AllowDedupe                                                           bool
	SpawnedByTaskID, BlockingCallerTaskID                                 string
	GateImplementTasks                                                    bool
	CriticBaselineTaskID, CriticBaselineFingerprint, CriticBaselineSource string
}

func profileKnown(p string) bool {
	for _, n := range ProfileNames {
		if n == p {
			return true
		}
	}
	return false
}
func uniqueStrings(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, x := range in {
		if x != "" && !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

func (s *Store) CreateTask(o CreateTaskOptions) (Row, bool, error) {
	if !profileKnown(o.Profile) {
		return nil, false, fmt.Errorf("unknown profile %q; expected one of: %s", o.Profile, strings.Join(ProfileNames, ", "))
	}
	if o.WorkspaceMode == "" {
		o.WorkspaceMode = "collection"
	}
	if o.Priority == 0 {
		o.Priority = 50
	}
	deps := uniqueStrings(o.DependsOn)
	key := DedupeKey(o.Profile, o.Objective)
	now := UTCNow()
	parts := []string{o.CriticBaselineTaskID, o.CriticBaselineFingerprint, o.CriticBaselineSource}
	any := false
	all := true
	for _, p := range parts {
		if p != "" {
			any = true
		} else {
			all = false
		}
	}
	if any && !all {
		return nil, false, errors.New("critic baseline task, fingerprint, and source must be provided together")
	}
	created := false
	taskID := ""
	var baselineEvt string
	err := s.withLock(func() error {
		dependencyRefs := append([]string{}, deps...)
		if o.BlockingCallerTaskID != "" {
			dependencyRefs = append(dependencyRefs, o.BlockingCallerTaskID)
		}
		if err := s.assertDependencyTasksInCollectionLocked(o.CollectionID, dependencyRefs); err != nil {
			return err
		}

		var existing Rows
		var err error
		if o.AllowDedupe {
			existing, err = s.queryLocked("SELECT * FROM tasks WHERE collection_id=" + SQLLiteral(o.CollectionID) + " AND dedupe_key=" + SQLLiteral(key) + " AND status NOT IN ('CANCELLED','SUPERSEDED') ORDER BY created_at LIMIT 1")
			if err != nil {
				return err
			}
		}
		if len(existing) > 0 {
			taskID = S(existing[0], "id")
		} else {
			taskID = MakeID("T")
		}
		prospective := [][2]string{}
		for _, d := range deps {
			prospective = append(prospective, [2]string{d, taskID})
		}
		if o.BlockingCallerTaskID != "" && (len(existing) == 0 || S(existing[0], "status") != TaskComplete) {
			prospective = append(prospective, [2]string{taskID, o.BlockingCallerTaskID})
		}
		var implementRows Rows
		if o.GateImplementTasks {
			implementRows, err = s.queryLocked("SELECT id FROM tasks WHERE collection_id=" + SQLLiteral(o.CollectionID) + " AND profile='implement' AND status NOT IN ('CANCELLED','SUPERSEDED','COMPLETE')")
			if err != nil {
				return err
			}
			for _, r := range implementRows {
				prospective = append(prospective, [2]string{taskID, S(r, "id")})
			}
		}
		if err = s.assertDependencyAcyclicLocked(o.CollectionID, prospective); err != nil {
			return err
		}
		stmts := []string{}
		if len(existing) == 0 {
			stmts = append(stmts, fmt.Sprintf("INSERT INTO tasks (id,project_id,collection_id,parent_task_id,requested_by_task_id,profile,title,objective,reason,status,priority,depth,workspace_mode,base_commit,dedupe_key,created_at,updated_at) VALUES (%s,%s,%s,%s,%s,%s,%s,%s,%s,'READY',%d,%d,%s,%s,%s,%s,%s)", SQLLiteral(taskID), SQLLiteral(o.ProjectID), SQLLiteral(o.CollectionID), nullString(o.ParentTaskID), nullString(o.RequestedByTaskID), SQLLiteral(o.Profile), SQLLiteral(o.Title), SQLLiteral(o.Objective), SQLLiteral(o.Reason), o.Priority, o.Depth, SQLLiteral(o.WorkspaceMode), nullString(o.BaseCommit), SQLLiteral(key), SQLLiteral(now), SQLLiteral(now)))
			created = true
		}
		for _, d := range deps {
			stmts = append(stmts, fmt.Sprintf("INSERT IGNORE INTO task_edges (from_task_id,to_task_id,collection_id,relation,created_at) VALUES (%s,%s,%s,'depends_on',%s)", SQLLiteral(d), SQLLiteral(taskID), SQLLiteral(o.CollectionID), SQLLiteral(now)))
		}
		if o.SpawnedByTaskID != "" {
			stmts = append(stmts, fmt.Sprintf("INSERT IGNORE INTO task_edges (from_task_id,to_task_id,collection_id,relation,created_at) VALUES (%s,%s,%s,'spawned_by',%s)", SQLLiteral(o.SpawnedByTaskID), SQLLiteral(taskID), SQLLiteral(o.CollectionID), SQLLiteral(now)))
		}
		if o.BlockingCallerTaskID != "" && (len(existing) == 0 || S(existing[0], "status") != TaskComplete) {
			stmts = append(stmts, fmt.Sprintf("INSERT IGNORE INTO task_edges (from_task_id,to_task_id,collection_id,relation,created_at) VALUES (%s,%s,%s,'depends_on',%s)", SQLLiteral(taskID), SQLLiteral(o.BlockingCallerTaskID), SQLLiteral(o.CollectionID), SQLLiteral(now)))
			stmts = append(stmts, "UPDATE tasks SET status='WAITING',updated_at="+SQLLiteral(now)+" WHERE id="+SQLLiteral(o.BlockingCallerTaskID))
		}
		if o.GateImplementTasks {
			stmts = append(stmts, fmt.Sprintf("INSERT IGNORE INTO task_edges (from_task_id,to_task_id,collection_id,relation,created_at) SELECT %s,t.id,%s,'depends_on',%s FROM tasks t WHERE t.collection_id=%s AND t.profile='implement' AND t.status NOT IN ('CANCELLED','SUPERSEDED','COMPLETE')", SQLLiteral(taskID), SQLLiteral(o.CollectionID), SQLLiteral(now), SQLLiteral(o.CollectionID)))
		}
		if o.CriticBaselineTaskID != "" {
			b, er := s.queryLocked("SELECT 1 AS ok FROM events WHERE task_id=" + SQLLiteral(o.CriticBaselineTaskID) + " AND event_type='CRITIC_PLAN_BASELINE' LIMIT 1")
			if er != nil {
				return er
			}
			if len(b) == 0 {
				payload := JSONDumps(Row{"critic_task_id": o.CriticBaselineTaskID, "fingerprint": o.CriticBaselineFingerprint, "source": o.CriticBaselineSource, "plan_review_task_id": taskID})
				baselineEvt = MakeID("EVT")
				stmts = append(stmts, fmt.Sprintf("INSERT INTO events (id,project_id,collection_id,task_id,attempt_id,event_type,payload_json,created_at) VALUES (%s,%s,%s,%s,NULL,'CRITIC_PLAN_BASELINE',%s,%s)", SQLLiteral(baselineEvt), SQLLiteral(o.ProjectID), SQLLiteral(o.CollectionID), SQLLiteral(o.CriticBaselineTaskID), SQLLiteral(payload), SQLLiteral(now)))
			}
		}
		if len(stmts) > 0 {
			if err = s.mutateLocked(strings.Join(stmts, ";")); err != nil {
				return err
			}
			msg := "task " + taskID + ": attach initial graph contract"
			if created {
				msg = "task " + taskID + ": create " + o.Profile + " task with initial graph contract"
			}
			if err = s.semanticCommitLocked(msg); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	if baselineEvt != "" {
		s.emitEvent(eventRecord{ID: baselineEvt, ProjectID: o.ProjectID, CollectionID: o.CollectionID, TaskID: o.CriticBaselineTaskID, EventType: "CRITIC_PLAN_BASELINE", CreatedAt: now})
	}
	if created {
		_ = s.Event(o.ProjectID, o.CollectionID, taskID, "", "TASK_CREATED", Row{"profile": o.Profile, "title": o.Title, "reason": o.Reason})
	}
	r, err := s.GetTask(taskID)
	return r, created, err
}
func nullString(s string) string {
	if s == "" {
		return "NULL"
	}
	return SQLLiteral(s)
}

func (s *Store) EnsureCollectionEntrypoint(cid string) (Row, error) {
	n, err := s.CountTasks(cid)
	if err != nil {
		return nil, err
	}
	if n > 0 {
		return nil, nil
	}
	c, err := s.GetCollection(cid)
	if err != nil {
		return nil, err
	}
	if st := S(c, "status"); st != "RUNNING" && st != "BLOCKED" {
		return nil, nil
	}
	obj := fmt.Sprintf("Own the initial decomposition of collection %s: %s. Consume the deterministic explore/critic preflight when present. Create durable plan_review/research/implement/review/advise tasks using praector as appropriate; request additional explore/critic work only when genuinely needed. Do not perform implementation or substantive specialist work yourself. Ensure implementation tasks are sequenced by dependencies; independent review is automatically enforced for implementation tasks.", cid, S(c, "objective"))
	t, _, err := s.CreateTask(CreateTaskOptions{ProjectID: S(c, "project_id"), CollectionID: cid, Profile: "coordinator", Title: "Initial coordination", Objective: obj, Reason: "Recovered missing collection entry point", WorkspaceMode: "collection", Depth: 0, AllowDedupe: false})
	if err != nil {
		return nil, err
	}
	_ = s.SetCollectionStatus(cid, "RUNNING", true)
	_ = s.Event(S(c, "project_id"), cid, S(t, "id"), "", "COLLECTION_ENTRYPOINT_RECOVERED", Row{"task_id": S(t, "id")})
	return t, nil
}

func (s *Store) RequestCollectionRevision(cid, reason string) (Row, Row, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return nil, nil, errors.New("revision reason must not be empty")
	}
	c, err := s.GetCollection(cid)
	if err != nil {
		return nil, nil, err
	}
	if S(c, "status") != TaskComplete {
		return nil, nil, fmt.Errorf("collection %s is %s, not COMPLETE", cid, S(c, "status"))
	}
	acc, _ := s.GetCollectionAcceptance(cid, "")
	if acc != nil {
		return nil, nil, errors.New("accepted collections are immutable. Create a new collection for follow-up changes instead of revising accepted history.")
	}
	open, err := s.ListCollectionRevisions(cid, "OPEN")
	if err != nil {
		return nil, nil, err
	}
	if len(open) > 0 {
		return nil, nil, fmt.Errorf("collection already has open revision %s", S(open[len(open)-1], "id"))
	}
	rid := MakeID("REV")
	tid := MakeID("T")
	now := UTCNow()
	obj := fmt.Sprintf("Operator acceptance rejected completed collection %s. This is a mandatory revision, not a non-blocking suggestion. Operator reason: %s\n\nInspect the current integrated candidate and prior task/review results. Create the smallest bounded repair implementation work that directly resolves the operator reason, with executable regression coverage where practical. Do not merely document or downgrade the reported defect when it conflicts with the collection objective. Ensure repaired write work receives a fresh independent review. If the requested correction cannot be satisfied without changing the collection objective, submit a failed coordinator result explaining why.", cid, reason)
	key := DedupeKey("coordinator", obj)
	rp := JSONDumps(Row{"revision_id": rid, "reason": reason, "coordinator_task_id": tid})
	tp := JSONDumps(Row{"profile": "coordinator", "title": "Acceptance revision", "reason": reason})
	revEvt := MakeID("EVT")
	taskEvt := MakeID("EVT")
	err = s.withLock(func() error {
		q := fmt.Sprintf("INSERT INTO collection_revisions (id,project_id,collection_id,reason,status,coordinator_task_id,created_at) VALUES (%s,%s,%s,%s,'OPEN',%s,%s);INSERT INTO tasks (id,project_id,collection_id,parent_task_id,requested_by_task_id,profile,title,objective,reason,status,priority,depth,workspace_mode,base_commit,dedupe_key,created_at,updated_at) VALUES (%s,%s,%s,NULL,NULL,'coordinator','Acceptance revision',%s,%s,'READY',50,0,'collection',NULL,%s,%s,%s);UPDATE collections SET status='RUNNING',updated_at=%s WHERE id=%s;INSERT INTO events (id,project_id,collection_id,task_id,attempt_id,event_type,payload_json,created_at) VALUES (%s,%s,%s,%s,NULL,'COLLECTION_REVISION_REQUESTED',%s,%s);INSERT INTO events (id,project_id,collection_id,task_id,attempt_id,event_type,payload_json,created_at) VALUES (%s,%s,%s,%s,NULL,'TASK_CREATED',%s,%s)", SQLLiteral(rid), SQLLiteral(S(c, "project_id")), SQLLiteral(cid), SQLLiteral(reason), SQLLiteral(tid), SQLLiteral(now), SQLLiteral(tid), SQLLiteral(S(c, "project_id")), SQLLiteral(cid), SQLLiteral(obj), SQLLiteral(reason), SQLLiteral(key), SQLLiteral(now), SQLLiteral(now), SQLLiteral(now), SQLLiteral(cid), SQLLiteral(revEvt), SQLLiteral(S(c, "project_id")), SQLLiteral(cid), SQLLiteral(tid), SQLLiteral(rp), SQLLiteral(now), SQLLiteral(taskEvt), SQLLiteral(S(c, "project_id")), SQLLiteral(cid), SQLLiteral(tid), SQLLiteral(tp), SQLLiteral(now))
		if e := s.mutateLocked(q); e != nil {
			return e
		}
		return s.semanticCommitLocked("collection " + cid + ": open revision " + rid)
	})
	if err != nil {
		return nil, nil, err
	}
	s.emitEvent(eventRecord{ID: revEvt, ProjectID: S(c, "project_id"), CollectionID: cid, TaskID: tid, EventType: "COLLECTION_REVISION_REQUESTED", CreatedAt: now})
	s.emitEvent(eventRecord{ID: taskEvt, ProjectID: S(c, "project_id"), CollectionID: cid, TaskID: tid, EventType: "TASK_CREATED", CreatedAt: now})
	rr, err := s.Query("SELECT * FROM collection_revisions WHERE id=" + SQLLiteral(rid) + " LIMIT 1")
	if err != nil {
		return nil, nil, err
	}
	t, err := s.GetTask(tid)
	if err != nil {
		return nil, nil, err
	}
	return rr[0], t, nil
}

func (s *Store) GetTask(id string) (Row, error) {
	rows, err := s.Query("SELECT * FROM tasks WHERE id=" + SQLLiteral(id) + " LIMIT 1")
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("task not found: %s", id)
	}
	r := rows[0]
	r["result"] = JSONMap(S(r, "result_json"))
	return r, nil
}
func (s *Store) ListTasks(cid string) (Rows, error) {
	rows, err := s.Query("SELECT * FROM tasks WHERE collection_id=" + SQLLiteral(cid) + " ORDER BY created_at")
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		r["result"] = JSONMap(S(r, "result_json"))
	}
	return rows, nil
}
func (s *Store) DependencyPath(from, to string) ([]string, error) {
	a, err := s.GetTask(from)
	if err != nil {
		return nil, err
	}
	b, err := s.GetTask(to)
	if err != nil {
		return nil, err
	}
	if S(a, "collection_id") != S(b, "collection_id") {
		return nil, nil
	}
	var edges [][2]string
	err = s.withLock(func() error { var e error; edges, e = s.dependencyEdgesLocked(S(a, "collection_id")); return e })
	if err != nil {
		return nil, err
	}
	return dependencyPathInEdges(edges, from, to), nil
}
func (s *Store) AddEdge(from, to, cid, rel string) error {
	if rel == "" {
		rel = "depends_on"
	}
	now := UTCNow()
	q := fmt.Sprintf("INSERT IGNORE INTO task_edges (from_task_id,to_task_id,collection_id,relation,created_at) VALUES (%s,%s,%s,%s,%s)", SQLLiteral(from), SQLLiteral(to), SQLLiteral(cid), SQLLiteral(rel), SQLLiteral(now))
	if rel != "depends_on" {
		return s.Mutate(q, fmt.Sprintf("graph %s: %s %s->%s", cid, rel, from, to))
	}
	return s.withLock(func() error {
		if err := s.assertDependencyTasksInCollectionLocked(cid, []string{from, to}); err != nil {
			return err
		}
		if err := s.assertDependencyAcyclicLocked(cid, [][2]string{{from, to}}); err != nil {
			return err
		}
		if err := s.mutateLocked(q); err != nil {
			return err
		}
		return s.semanticCommitLocked(fmt.Sprintf("graph %s: %s %s->%s", cid, rel, from, to))
	})
}
func (s *Store) ListEdges(cid string) (Rows, error) {
	return s.Query("SELECT * FROM task_edges WHERE collection_id=" + SQLLiteral(cid) + " ORDER BY created_at")
}
func (s *Store) MaterialPlanFingerprint(cid string, legacy bool) (string, error) {
	c, err := s.GetCollection(cid)
	if err != nil {
		return "", err
	}
	t, err := s.ListTasks(cid)
	if err != nil {
		return "", err
	}
	e, err := s.ListEdges(cid)
	if err != nil {
		return "", err
	}
	return MaterialPlanFingerprint(S(c, "objective"), t, e, legacy), nil
}
func (s *Store) CriticPlanBaseline(tid string) (Row, error) {
	r, err := s.Query("SELECT payload_json,created_at FROM events WHERE task_id=" + SQLLiteral(tid) + " AND event_type='CRITIC_PLAN_BASELINE' ORDER BY created_at DESC LIMIT 1")
	if err != nil || len(r) == 0 {
		return nil, err
	}
	p := JSONMap(S(r[0], "payload_json"))
	if _, ok := p["created_at"]; !ok {
		p["created_at"] = r[0]["created_at"]
	}
	return p, nil
}
func (s *Store) criticPlanBaselineLocked(tid string) (Row, error) {
	r, err := s.queryLocked("SELECT payload_json,created_at FROM events WHERE task_id=" + SQLLiteral(tid) + " AND event_type='CRITIC_PLAN_BASELINE' ORDER BY created_at DESC LIMIT 1")
	if err != nil || len(r) == 0 {
		return nil, err
	}
	p := JSONMap(S(r[0], "payload_json"))
	if _, ok := p["created_at"]; !ok {
		p["created_at"] = r[0]["created_at"]
	}
	return p, nil
}
func (s *Store) RecordCriticPlanBaseline(tid, fp, source, reviewID string) error {
	t, err := s.GetTask(tid)
	if err != nil {
		return err
	}
	return s.Event(S(t, "project_id"), S(t, "collection_id"), tid, "", "CRITIC_PLAN_BASELINE", Row{"critic_task_id": tid, "fingerprint": fp, "source": source, "plan_review_task_id": nullable(reviewID)})
}
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
func (s *Store) materialPlanFingerprintLocked(cid, obj string, legacy bool) (string, error) {
	t, err := s.queryLocked("SELECT * FROM tasks WHERE collection_id=" + SQLLiteral(cid) + " ORDER BY created_at,id")
	if err != nil {
		return "", err
	}
	e, err := s.queryLocked("SELECT * FROM task_edges WHERE collection_id=" + SQLLiteral(cid) + " ORDER BY created_at")
	if err != nil {
		return "", err
	}
	return MaterialPlanFingerprint(obj, t, e, legacy), nil
}
func (s *Store) CriticRevalidationState(cid string) (Row, error) {
	tasks, err := s.ListTasks(cid)
	if err != nil {
		return nil, err
	}
	var latest Row
	for _, t := range tasks {
		if S(t, "profile") == "critic" && S(t, "status") != TaskCancelled && S(t, "status") != TaskSuperseded {
			latest = t
		}
	}
	cur, err := s.MaterialPlanFingerprint(cid, false)
	if err != nil {
		return nil, err
	}
	legacy, err := s.MaterialPlanFingerprint(cid, true)
	if err != nil {
		return nil, err
	}
	tasksForCompat, err := s.ListTasks(cid)
	if err != nil {
		return nil, err
	}
	edgesForCompat, err := s.ListEdges(cid)
	if err != nil {
		return nil, err
	}
	collForCompat, err := s.GetCollection(cid)
	if err != nil {
		return nil, err
	}
	v021 := MaterialPlanFingerprintV021(S(collForCompat, "objective"), tasksForCompat, edgesForCompat, false)
	v021Legacy := MaterialPlanFingerprintV021(S(collForCompat, "objective"), tasksForCompat, edgesForCompat, true)
	var base Row
	if latest != nil {
		base, _ = s.CriticPlanBaseline(S(latest, "id"))
	}
	sealed := S(base, "fingerprint")
	compatible := ""
	if sealed != "" && sealed != cur {
		switch {
		case sealed == legacy:
			compatible = "legacy_raw_base_commit"
		case sealed == v021:
			compatible = "v0.2.1_material_plan"
		case sealed == v021Legacy:
			compatible = "v0.2.1_legacy_raw_base_commit"
		}
	}
	return Row{"latest_critic_task_id": S(latest, "id"), "latest_critic_status": S(latest, "status"), "current_material_plan_fingerprint": cur, "sealed_fingerprint": sealed, "baseline_source": S(base, "source"), "compatible_via": nullable(compatible), "stale": latest != nil && sealed != "" && sealed != cur && compatible == ""}, nil
}
func (s *Store) Dependencies(tid string) (Rows, error) {
	return s.Query("SELECT t.* FROM task_edges e JOIN tasks t ON t.id=e.from_task_id WHERE e.to_task_id=" + SQLLiteral(tid) + " AND e.relation='depends_on' ORDER BY e.created_at")
}

// ResearchAncestors returns every completed research Task reachable through the
// hard dependency DAG. Research constraints are lineage properties: an
// implementation that depends on a plan review which depends on research must
// inherit that research even when the research Task is not a direct dependency.
func (s *Store) ResearchAncestors(tid string) (Rows, error) {
	seen := map[string]bool{tid: true}
	queue := []string{tid}
	byID := map[string]Row{}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		deps, err := s.Dependencies(cur)
		if err != nil {
			return nil, err
		}
		for _, dep := range deps {
			id := S(dep, "id")
			if id == "" {
				continue
			}
			if S(dep, "profile") == "research" && S(dep, "status") == TaskComplete {
				dep["result"] = JSONMap(S(dep, "result_json"))
				byID[id] = dep
			}
			if !seen[id] {
				seen[id] = true
				queue = append(queue, id)
			}
		}
	}
	out := make(Rows, 0, len(byID))
	for _, row := range byID {
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := S(out[i], "created_at"), S(out[j], "created_at")
		if a == b {
			return S(out[i], "id") < S(out[j], "id")
		}
		return a < b
	})
	return out, nil
}

// researchAncestorsLocked is the claim-time variant used while the Store lock
// is already held. It mirrors ResearchAncestors without recursively acquiring
// the filesystem lock.
func (s *Store) researchAncestorsLocked(tid string) (Rows, error) {
	seen := map[string]bool{tid: true}
	queue := []string{tid}
	byID := map[string]Row{}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		deps, err := s.queryLocked("SELECT t.* FROM task_edges e JOIN tasks t ON t.id=e.from_task_id WHERE e.to_task_id=" + SQLLiteral(cur) + " AND e.relation='depends_on' ORDER BY e.created_at")
		if err != nil {
			return nil, err
		}
		for _, dep := range deps {
			id := S(dep, "id")
			if id == "" {
				continue
			}
			if S(dep, "profile") == "research" && S(dep, "status") == TaskComplete {
				dep["result"] = JSONMap(S(dep, "result_json"))
				byID[id] = dep
			}
			if !seen[id] {
				seen[id] = true
				queue = append(queue, id)
			}
		}
	}
	out := make(Rows, 0, len(byID))
	for _, row := range byID {
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := S(out[i], "created_at"), S(out[j], "created_at")
		if a == b {
			return S(out[i], "id") < S(out[j], "id")
		}
		return a < b
	})
	return out, nil
}

// DirectiveAncestors returns every completed critic/plan_review/advise Task
// reachable through the hard dependency DAG. Like research constraints,
// planning directives are lineage properties: a review that depends on a plan
// review which depends on a critic inherits that critic's directives even
// when the critic Task is not a direct dependency. The traversal intentionally
// mirrors ResearchAncestors so both mechanisms behave identically.
func (s *Store) DirectiveAncestors(tid string) (Rows, error) {
	seen := map[string]bool{tid: true}
	queue := []string{tid}
	byID := map[string]Row{}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		deps, err := s.Dependencies(cur)
		if err != nil {
			return nil, err
		}
		for _, dep := range deps {
			id := S(dep, "id")
			if id == "" {
				continue
			}
			if directiveSourceProfiles[S(dep, "profile")] && S(dep, "status") == TaskComplete {
				dep["result"] = JSONMap(S(dep, "result_json"))
				byID[id] = dep
			}
			if !seen[id] {
				seen[id] = true
				queue = append(queue, id)
			}
		}
	}
	out := make(Rows, 0, len(byID))
	for _, row := range byID {
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := S(out[i], "created_at"), S(out[j], "created_at")
		if a == b {
			return S(out[i], "id") < S(out[j], "id")
		}
		return a < b
	})
	return out, nil
}

// directiveAncestorsLocked is the claim-time variant used while the Store lock
// is already held. It mirrors DirectiveAncestors without recursively acquiring
// the filesystem lock, matching the researchAncestorsLocked convention.
func (s *Store) directiveAncestorsLocked(tid string) (Rows, error) {
	seen := map[string]bool{tid: true}
	queue := []string{tid}
	byID := map[string]Row{}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		deps, err := s.queryLocked("SELECT t.* FROM task_edges e JOIN tasks t ON t.id=e.from_task_id WHERE e.to_task_id=" + SQLLiteral(cur) + " AND e.relation='depends_on' ORDER BY e.created_at")
		if err != nil {
			return nil, err
		}
		for _, dep := range deps {
			id := S(dep, "id")
			if id == "" {
				continue
			}
			if directiveSourceProfiles[S(dep, "profile")] && S(dep, "status") == TaskComplete {
				dep["result"] = JSONMap(S(dep, "result_json"))
				byID[id] = dep
			}
			if !seen[id] {
				seen[id] = true
				queue = append(queue, id)
			}
		}
	}
	out := make(Rows, 0, len(byID))
	for _, row := range byID {
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := S(out[i], "created_at"), S(out[j], "created_at")
		if a == b {
			return S(out[i], "id") < S(out[j], "id")
		}
		return a < b
	})
	return out, nil
}
func (s *Store) Dependents(tid string) (Rows, error) {
	return s.Query("SELECT t.* FROM task_edges e JOIN tasks t ON t.id=e.to_task_id WHERE e.from_task_id=" + SQLLiteral(tid) + " AND e.relation='depends_on' ORDER BY e.created_at")
}
func (s *Store) FindRelatedReview(tid string) (Row, error) {
	r, err := s.Query("SELECT t.* FROM task_edges e JOIN tasks t ON t.id=e.to_task_id WHERE e.from_task_id=" + SQLLiteral(tid) + " AND e.relation='depends_on' AND t.profile='review' AND t.status NOT IN ('CANCELLED','SUPERSEDED') LIMIT 1")
	if err != nil || len(r) == 0 {
		return nil, err
	}
	return r[0], nil
}
func (s *Store) AddDependency(tid, depID string) error {
	t, err := s.GetTask(tid)
	if err != nil {
		return err
	}
	d, err := s.GetTask(depID)
	if err != nil {
		return err
	}
	if S(t, "collection_id") != S(d, "collection_id") {
		return errors.New("dependencies must belong to the same collection in the current release")
	}
	return s.AddEdge(depID, tid, S(t, "collection_id"), "depends_on")
}
func (s *Store) RemoveDependency(tid, depID string) error {
	t, err := s.GetTask(tid)
	if err != nil {
		return err
	}
	d, err := s.GetTask(depID)
	if err != nil {
		return err
	}
	if S(t, "collection_id") != S(d, "collection_id") {
		return errors.New("dependencies must belong to the same collection in the current release")
	}
	return s.Mutate("DELETE FROM task_edges WHERE from_task_id="+SQLLiteral(depID)+" AND to_task_id="+SQLLiteral(tid)+" AND relation='depends_on'", fmt.Sprintf("graph %s: remove dependency %s->%s", S(t, "collection_id"), depID, tid))
}

func (s *Store) attachExistingPlanReview(reviewID, callerID string, deps []string, criticID, fp, source string) (Row, error) {
	review, err := s.GetTask(reviewID)
	if err != nil {
		return nil, err
	}
	caller, err := s.GetTask(callerID)
	if err != nil {
		return nil, err
	}
	if S(review, "collection_id") != S(caller, "collection_id") {
		return nil, errors.New("plan review and caller must belong to the same collection")
	}
	if S(review, "profile") != "plan_review" {
		return nil, errors.New("reusable final gate must be a plan_review task")
	}
	if TerminalTaskStatuses[S(review, "status")] || S(review, "status") == TaskFailed {
		return nil, errors.New("cannot reuse a terminal plan_review task")
	}
	cid := S(review, "collection_id")
	pid := S(review, "project_id")
	now := UTCNow()
	deps = uniqueStrings(deps)
	var baselineEvt string
	err = s.withLock(func() error {
		if er := s.assertDependencyTasksInCollectionLocked(cid, deps); er != nil {
			return er
		}
		impl, er := s.queryLocked("SELECT id FROM tasks WHERE collection_id=" + SQLLiteral(cid) + " AND profile='implement' AND status NOT IN ('CANCELLED','SUPERSEDED','COMPLETE')")
		if er != nil {
			return er
		}
		edges, er := s.dependencyEdgesLocked(cid)
		if er != nil {
			return er
		}
		callerPrereq := false
		for _, e := range edges {
			if e[0] == callerID && e[1] == reviewID {
				callerPrereq = true
			}
		}
		validation := [][2]string{}
		for _, e := range edges {
			if callerPrereq && e[0] == callerID && e[1] == reviewID {
				continue
			}
			validation = append(validation, e)
		}
		pros := [][2]string{}
		for _, d := range deps {
			pros = append(pros, [2]string{d, reviewID})
		}
		if S(caller, "status") != TaskComplete {
			pros = append(pros, [2]string{reviewID, callerID})
		}
		for _, r := range impl {
			pros = append(pros, [2]string{reviewID, S(r, "id")})
		}
		for _, e := range pros {
			exists := false
			for _, x := range validation {
				if x == e {
					exists = true
				}
			}
			if exists {
				continue
			}
			if p := dependencyPathInEdges(validation, e[1], e[0]); p != nil {
				return &DependencyCycleError{fmt.Sprintf("Dependency %s->%s would create a cycle in %s: %s", e[0], e[1], cid, strings.Join(append([]string{e[0]}, p...), " -> "))}
			}
			validation = append(validation, e)
		}
		st := []string{}
		if callerPrereq {
			st = append(st, "DELETE FROM task_edges WHERE collection_id="+SQLLiteral(cid)+" AND relation='depends_on' AND from_task_id="+SQLLiteral(callerID)+" AND to_task_id="+SQLLiteral(reviewID))
		}
		for _, d := range deps {
			if d == callerID {
				continue
			}
			st = append(st, fmt.Sprintf("INSERT IGNORE INTO task_edges (from_task_id,to_task_id,collection_id,relation,created_at) VALUES (%s,%s,%s,'depends_on',%s)", SQLLiteral(d), SQLLiteral(reviewID), SQLLiteral(cid), SQLLiteral(now)))
		}
		if S(caller, "status") != TaskComplete {
			st = append(st, fmt.Sprintf("INSERT IGNORE INTO task_edges (from_task_id,to_task_id,collection_id,relation,created_at) VALUES (%s,%s,%s,'depends_on',%s)", SQLLiteral(reviewID), SQLLiteral(callerID), SQLLiteral(cid), SQLLiteral(now)), "UPDATE tasks SET status='WAITING',updated_at="+SQLLiteral(now)+" WHERE id="+SQLLiteral(callerID))
		}
		st = append(st, fmt.Sprintf("INSERT IGNORE INTO task_edges (from_task_id,to_task_id,collection_id,relation,created_at) SELECT %s,t.id,%s,'depends_on',%s FROM tasks t WHERE t.collection_id=%s AND t.profile='implement' AND t.status NOT IN ('CANCELLED','SUPERSEDED','COMPLETE')", SQLLiteral(reviewID), SQLLiteral(cid), SQLLiteral(now), SQLLiteral(cid)))
		if criticID != "" {
			ex, er := s.queryLocked("SELECT 1 AS ok FROM events WHERE task_id=" + SQLLiteral(criticID) + " AND event_type='CRITIC_PLAN_BASELINE' LIMIT 1")
			if er != nil {
				return er
			}
			if len(ex) == 0 {
				payload := JSONDumps(Row{"critic_task_id": criticID, "fingerprint": fp, "source": source, "plan_review_task_id": reviewID})
				baselineEvt = MakeID("EVT")
				st = append(st, fmt.Sprintf("INSERT INTO events (id,project_id,collection_id,task_id,attempt_id,event_type,payload_json,created_at) VALUES (%s,%s,%s,%s,NULL,'CRITIC_PLAN_BASELINE',%s,%s)", SQLLiteral(baselineEvt), SQLLiteral(pid), SQLLiteral(cid), SQLLiteral(criticID), SQLLiteral(payload), SQLLiteral(now)))
			}
		}
		if len(st) > 0 {
			if er = s.mutateLocked(strings.Join(st, ";")); er != nil {
				return er
			}
			return s.semanticCommitLocked("task " + reviewID + ": reuse final plan-review handoff for " + callerID)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if baselineEvt != "" {
		s.emitEvent(eventRecord{ID: baselineEvt, ProjectID: pid, CollectionID: cid, TaskID: criticID, EventType: "CRITIC_PLAN_BASELINE", CreatedAt: now})
	}
	return s.GetTask(reviewID)
}

func (s *Store) SupersedeTask(oldID, newID, reason string) error {
	old, err := s.GetTask(oldID)
	if err != nil {
		return err
	}
	rep, err := s.GetTask(newID)
	if err != nil {
		return err
	}
	if S(old, "collection_id") != S(rep, "collection_id") {
		return errors.New("replacement task must belong to the same collection")
	}
	if oldID == newID {
		return errors.New("a task cannot supersede itself")
	}
	deps, err := s.Query("SELECT to_task_id FROM task_edges WHERE from_task_id=" + SQLLiteral(oldID) + " AND relation='depends_on'")
	if err != nil {
		return err
	}
	for _, r := range deps {
		did := S(r, "to_task_id")
		if e := s.AddEdge(newID, did, S(old, "collection_id"), "depends_on"); e != nil {
			if S(old, "profile") != "plan_review" || S(rep, "profile") != "plan_review" {
				return e
			}
			_ = s.Event(S(old, "project_id"), S(old, "collection_id"), oldID, "", "DEPENDENCY_CYCLE_REJECTED", Row{"from_task_id": newID, "to_task_id": did, "context": "supersede_rewire"})
		}
	}
	if S(old, "profile") == S(rep, "profile") && (S(old, "profile") == "review" || S(old, "profile") == "plan_review") {
		pre, er := s.Query("SELECT from_task_id FROM task_edges WHERE to_task_id=" + SQLLiteral(oldID) + " AND relation='depends_on'")
		if er != nil {
			return er
		}
		for _, r := range pre {
			from := S(r, "from_task_id")
			if e := s.AddEdge(from, newID, S(old, "collection_id"), "depends_on"); e != nil {
				if S(old, "profile") != "plan_review" {
					return e
				}
				_ = s.Event(S(old, "project_id"), S(old, "collection_id"), oldID, "", "DEPENDENCY_CYCLE_REJECTED", Row{"from_task_id": from, "to_task_id": newID, "context": "supersede_prerequisite"})
			}
		}
		if S(old, "profile") == "review" {
			vr, er := s.Query("SELECT from_task_id FROM task_edges WHERE to_task_id=" + SQLLiteral(oldID) + " AND relation='verifies'")
			if er != nil {
				return er
			}
			for _, r := range vr {
				if e := s.AddEdge(S(r, "from_task_id"), newID, S(old, "collection_id"), "verifies"); e != nil {
					return e
				}
			}
		}
	}
	if err = s.Mutate("DELETE FROM task_edges WHERE from_task_id="+SQLLiteral(oldID)+" AND relation='depends_on';UPDATE tasks SET status='SUPERSEDED',updated_at="+SQLLiteral(UTCNow())+" WHERE id="+SQLLiteral(oldID), "task "+oldID+": superseded by "+newID); err != nil {
		return err
	}
	if err = s.AddEdge(oldID, newID, S(old, "collection_id"), "superseded_by"); err != nil {
		return err
	}
	return s.Event(S(old, "project_id"), S(old, "collection_id"), oldID, "", "TASK_SUPERSEDED", Row{"replacement_task_id": newID, "reason": reason})
}

func (s *Store) RetryFailedTask(tid, reason string) (Row, error) {
	old, err := s.GetTask(tid)
	if err != nil {
		return nil, err
	}
	if S(old, "status") != TaskFailed {
		return nil, fmt.Errorf("only FAILED tasks can be retried; %s is %s", tid, S(old, "status"))
	}
	dr, err := s.Dependencies(tid)
	if err != nil {
		return nil, err
	}
	deps := []string{}
	for _, r := range dr {
		deps = append(deps, S(r, "id"))
	}
	base := S(old, "current_head")
	if base == "" {
		base = S(old, "base_commit")
	}
	rep, _, err := s.CreateTask(CreateTaskOptions{ProjectID: S(old, "project_id"), CollectionID: S(old, "collection_id"), Profile: S(old, "profile"), Title: S(old, "title"), Objective: S(old, "objective"), Reason: "Manual retry of " + tid + ": " + reason, ParentTaskID: S(old, "parent_task_id"), RequestedByTaskID: S(old, "requested_by_task_id"), DependsOn: deps, WorkspaceMode: S(old, "workspace_mode"), BaseCommit: base, Priority: I(old, "priority"), Depth: I(old, "depth"), AllowDedupe: false})
	if err != nil {
		return nil, err
	}
	if err = s.SupersedeTask(tid, S(rep, "id"), reason); err != nil {
		return nil, err
	}
	_ = s.SetCollectionStatus(S(old, "collection_id"), "RUNNING", true)
	_ = s.Event(S(old, "project_id"), S(old, "collection_id"), tid, "", "TASK_MANUAL_RETRY_CREATED", Row{"replacement_task_id": S(rep, "id"), "reason": reason})
	return s.GetTask(S(rep, "id"))
}

func (s *Store) SetTaskWorkspace(tid string, wt, branch, base, head *string) error {
	a := []string{"updated_at=" + SQLLiteral(UTCNow())}
	if wt != nil {
		a = append(a, "worktree_path="+SQLLiteral(*wt))
	}
	if branch != nil {
		a = append(a, "branch_name="+SQLLiteral(*branch))
	}
	if base != nil {
		a = append(a, "base_commit="+SQLLiteral(*base))
	}
	if head != nil {
		a = append(a, "current_head="+SQLLiteral(*head))
	}
	return s.Mutate("UPDATE tasks SET "+strings.Join(a, ",")+" WHERE id="+SQLLiteral(tid), "")
}
func (s *Store) SetTaskStatus(tid, status, msg string) error {
	// Slice 3: emit a durable event for a real transition into a terminal task
	// state (COMPLETE / FAILED) so a push consumer learns of it. A repeated no-op
	// write emits ZERO events (guarded on the before value read in-lock). The
	// existing Dolt commit-message behavior (msg) is unchanged.
	track := status == TaskComplete || status == TaskFailed
	var evt *eventRecord
	err := s.withLock(func() error {
		stmts := []string{"UPDATE tasks SET status=" + SQLLiteral(status) + ",updated_at=" + SQLLiteral(UTCNow()) + " WHERE id=" + SQLLiteral(tid)}
		if track {
			cur, e := s.queryLocked("SELECT project_id,collection_id,status FROM tasks WHERE id=" + SQLLiteral(tid))
			if e != nil {
				return e
			}
			if len(cur) > 0 && S(cur[0], "status") != status {
				id := MakeID("EVT")
				now := UTCNow()
				payload := JSONDumps(Row{"from": S(cur[0], "status"), "to": status})
				stmts = append(stmts, "INSERT INTO events (id,project_id,collection_id,task_id,attempt_id,event_type,payload_json,created_at) VALUES ("+SQLLiteral(id)+","+nullString(S(cur[0], "project_id"))+","+nullString(S(cur[0], "collection_id"))+","+SQLLiteral(tid)+",NULL,"+SQLLiteral(statusTaskEvent(status))+","+SQLLiteral(payload)+","+SQLLiteral(now)+")")
				evt = &eventRecord{ID: id, ProjectID: S(cur[0], "project_id"), CollectionID: S(cur[0], "collection_id"), TaskID: tid, EventType: statusTaskEvent(status), CreatedAt: now}
			}
		}
		if e := s.mutateLocked(strings.Join(stmts, ";")); e != nil {
			return e
		}
		if msg != "" {
			return s.semanticCommitLocked(msg)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if evt != nil {
		s.emitEvent(*evt)
	}
	return nil
}

// SubmitResult records a worker result. derivedConfigCandidates is an
// optional, seam-computed diff-derived configuration-knob candidate list
// (base...collection integration branch) that a passing review's impact
// enumeration must account for. Callers without Git workspace access pass
// nothing and get exactly the historical behavior; the submit seam in the
// service layer computes the list so no git access enters the Store or the
// pure policy validators.
func (s *Store) SubmitResult(tid string, result Row, derivedConfigCandidates ...DerivedConfigCandidate) (Row, error) {
	before, err := s.GetTask(tid)
	if err != nil {
		return nil, err
	}
	st := strings.ToLower(strings.TrimSpace(S(result, "status")))
	if st == "" {
		st = "complete"
	}
	mapping := map[string]string{"complete": TaskVerifying, "completed": TaskVerifying, "waiting": TaskWaiting, "failed": TaskFailed}
	to, ok := mapping[st]
	if !ok {
		return nil, errors.New("result status must be one of: complete, waiting, failed")
	}
	if S(before, "profile") == "research" && (st == "complete" || st == "completed") {
		strict := strings.Contains(strings.ToLower(S(before, "title")), "fresh external research") || strings.Contains(strings.ToLower(S(before, "reason")), "fresh external evidence") || strings.HasPrefix(strings.ToLower(S(before, "title")), "research requested by")
		if strict {
			collection, e := s.GetCollection(S(before, "collection_id"))
			if e != nil {
				return nil, e
			}
			policy := effectivePlanningPolicy(collection)
			if e := ValidateDeterministicResearchResult(result, policy.ExternalConformanceRequired); e != nil {
				return nil, e
			}
			if exceptionIDs := ResearchExceptionDecisionIDs(result); len(exceptionIDs) > 0 {
				decisions, e := s.ListDecisions(S(before, "project_id"), S(before, "collection_id"))
				if e != nil {
					return nil, e
				}
				known := map[string]bool{}
				for _, decision := range decisions {
					known[S(decision, "id")] = true
				}
				for _, decisionID := range exceptionIDs {
					if !known[decisionID] {
						return nil, fmt.Errorf("research exception_decision_id %s is not a durable decision in this Collection", decisionID)
					}
				}
			}
		}
	}
	if st == "complete" || st == "completed" {
		profile := S(before, "profile")
		collection, e := s.GetCollection(S(before, "collection_id"))
		if e != nil {
			return nil, e
		}
		policy := effectivePlanningPolicy(collection)
		if profile == "critic" || profile == "plan_review" || profile == "review" {
			researchDeps, e := s.ResearchAncestors(tid)
			if e != nil {
				return nil, e
			}
			requirePass := (profile == "plan_review" || profile == "review") && strings.ToLower(strings.TrimSpace(S(result, "verdict"))) == "pass"
			if len(researchDeps) > 0 {
				if e := ValidateResearchConsumerResult(result, researchDeps, requirePass); e != nil {
					return nil, e
				}
				if exceptionIDs := ResearchConsumerExceptionDecisionIDs(result); len(exceptionIDs) > 0 {
					decisions, e := s.ListDecisions(S(before, "project_id"), S(before, "collection_id"))
					if e != nil {
						return nil, e
					}
					known := map[string]bool{}
					for _, decision := range decisions {
						known[S(decision, "id")] = true
					}
					for _, decisionID := range exceptionIDs {
						if !known[decisionID] {
							return nil, fmt.Errorf("research consistency exception_decision_id %s is not a durable decision in this Collection", decisionID)
						}
					}
				}
			}
			if profile == "plan_review" || profile == "review" {
				directiveAncestors, e := s.DirectiveAncestors(tid)
				if e != nil {
					return nil, e
				}
				if directives := InheritedDirectives(directiveAncestors); len(directives) > 0 {
					if e := ValidateDirectiveTraceability(result, directives, requirePass, nil); e != nil {
						return nil, e
					}
					decisionIDs := []string{}
					for _, raw := range Slice(result, "directive_dispositions") {
						var item Row
						switch x := raw.(type) {
						case map[string]any:
							item = Row(x)
						case Row:
							item = x
						default:
							continue
						}
						if decisionID := strings.TrimSpace(S(item, "decision_id")); decisionID != "" {
							decisionIDs = append(decisionIDs, decisionID)
						}
					}
					if len(decisionIDs) > 0 {
						decisions, e := s.ListDecisions(S(before, "project_id"), S(before, "collection_id"))
						if e != nil {
							return nil, e
						}
						known := map[string]bool{}
						for _, decision := range decisions {
							known[S(decision, "id")] = true
						}
						for _, decisionID := range decisionIDs {
							if !known[decisionID] {
								return nil, fmt.Errorf("directive disposition decision_id %s is not a durable decision in this Collection", decisionID)
							}
						}
					}
				}
				if profile == "review" {
					if e := ValidateReviewImpactEnumeration(result, nil, derivedConfigCandidates); e != nil {
						return nil, e
					}
					if decisionIDs := CandidateDecisionIDs(result); len(decisionIDs) > 0 {
						decisions, e := s.ListDecisions(S(before, "project_id"), S(before, "collection_id"))
						if e != nil {
							return nil, e
						}
						known := map[string]bool{}
						for _, decision := range decisions {
							known[S(decision, "id")] = true
						}
						for _, decisionID := range decisionIDs {
							if !known[decisionID] {
								return nil, fmt.Errorf("candidate disposition decision_id %s is not a durable decision in this Collection", decisionID)
							}
						}
					}
				}
			}
		}
		if profile == "review" && policy.ExternalConformanceRequired {
			if e := ValidateExternalConformanceReviewResult(result); e != nil {
				return nil, e
			}
			if decisionID := strings.TrimSpace(S(result, "oracle_exception_decision_id")); decisionID != "" {
				decisions, e := s.ListDecisions(S(before, "project_id"), S(before, "collection_id"))
				if e != nil {
					return nil, e
				}
				found := false
				for _, decision := range decisions {
					if S(decision, "id") == decisionID {
						found = true
						break
					}
				}
				if !found {
					return nil, fmt.Errorf("oracle_exception_decision_id %s is not a durable decision in this Collection", decisionID)
				}
			}
		}
	}
	if err := s.Mutate("UPDATE tasks SET status="+SQLLiteral(to)+",result_json="+SQLLiteral(JSONDumps(result))+",updated_at="+SQLLiteral(UTCNow())+" WHERE id="+SQLLiteral(tid), "task "+tid+": result "+strings.ToLower(to)); err != nil {
		return nil, err
	}
	t, err := s.GetTask(tid)
	if err != nil {
		return nil, err
	}
	_ = s.Event(S(t, "project_id"), S(t, "collection_id"), tid, "", "TASK_RESULT_SUBMITTED", result)
	return t, nil
}

type RequestTaskOptions struct {
	Profile, Title, Objective, Reason string
	Blocking                          bool
	DependsOn                         []string
	WorkspaceMode, BaseCommit         string
	Limits                            Limits
}

func containsString(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
func removeStrings(xs []string, rm map[string]bool) []string {
	out := []string{}
	for _, x := range xs {
		if !rm[x] {
			out = append(out, x)
		}
	}
	return out
}
func (s *Store) RequestTask(callerID string, o RequestTaskOptions) (Row, bool, error) {
	caller, err := s.GetTask(callerID)
	if err != nil {
		return nil, false, err
	}
	if o.Profile == "coordinator" && S(caller, "profile") != "coordinator" {
		return nil, false, errors.New("workers may not create coordinator tasks directly; use `praector coordinator attention`")
	}
	n, err := s.CountTasks(S(caller, "collection_id"))
	if err != nil {
		return nil, false, err
	}
	if n >= o.Limits.MaxTasksPerCollection {
		return nil, false, errors.New("collection task limit reached")
	}
	depth := I(caller, "depth") + 1
	if depth > o.Limits.MaxTaskDepth {
		return nil, false, errors.New("maximum task depth reached")
	}
	cc, err := s.ChildCount(callerID)
	if err != nil {
		return nil, false, err
	}
	if cc >= o.Limits.MaxChildrenPerTask {
		return nil, false, errors.New("maximum children for this task reached")
	}
	deps := uniqueStrings(o.DependsOn)
	effective := o.Blocking
	gate := false
	var baselineCritic Row
	baselineFP, baselineSource := "", ""
	ignoredImpl := []string{}
	ignoredCaller := false
	var reusable Row
	var tasks Rows
	if o.Profile == "plan_review" {
		tasks, err = s.ListTasks(S(caller, "collection_id"))
		if err != nil {
			return nil, false, err
		}
		implIDs := map[string]bool{}
		for _, t := range tasks {
			if S(t, "profile") == "implement" && S(t, "status") != TaskCancelled && S(t, "status") != TaskSuperseded {
				implIDs[S(t, "id")] = true
			}
		}
		for _, d := range deps {
			if implIDs[d] {
				ignoredImpl = append(ignoredImpl, d)
			}
		}
		if len(ignoredImpl) > 0 {
			deps = removeStrings(deps, implIDs)
		}
		if S(caller, "profile") == "coordinator" && containsString(deps, callerID) {
			deps = removeStrings(deps, map[string]bool{callerID: true})
			ignoredCaller = true
		}
	}
	if S(caller, "profile") == "coordinator" && o.Profile == "plan_review" {
		effective = true
		gate = true
		critics := Rows{}
		for _, t := range tasks {
			if S(t, "profile") == "critic" && S(t, "status") != TaskCancelled && S(t, "status") != TaskSuperseded {
				critics = append(critics, t)
			}
		}
		if len(critics) > 0 {
			lc := critics[len(critics)-1]
			if !containsString(deps, S(lc, "id")) {
				deps = append(deps, S(lc, "id"))
			}
			base, _ := s.CriticPlanBaseline(S(lc, "id"))
			if S(lc, "status") == TaskComplete && base == nil {
				baselineCritic = lc
				baselineFP, _ = s.MaterialPlanFingerprint(S(caller, "collection_id"), false)
				revs, _ := s.ListCollectionRevisions(S(caller, "collection_id"), "OPEN")
				baselineSource = "initial_plan_seal"
				for _, r := range revs {
					if S(r, "coordinator_task_id") == callerID {
						baselineSource = "revision_plan_seal"
					}
				}
			}
		}
		candidates := Rows{}
		for _, t := range tasks {
			st := S(t, "status")
			verdict := strings.ToLower(S(M(t, "result"), "verdict"))
			if S(t, "profile") == "plan_review" && (st == TaskPending || st == TaskReady || st == TaskRunning || st == TaskWaiting || st == TaskVerifying) && verdict != "fail" {
				candidates = append(candidates, t)
			}
		}
		if len(candidates) > 0 {
			reusable = candidates[len(candidates)-1]
		}
	}
	var task Row
	created := false
	if reusable != nil {
		task, err = s.attachExistingPlanReview(S(reusable, "id"), callerID, deps, S(baselineCritic, "id"), baselineFP, baselineSource)
		if err != nil {
			return nil, false, err
		}
		_ = s.Event(S(caller, "project_id"), S(caller, "collection_id"), callerID, "", "PLAN_REVIEW_REUSED", Row{"plan_review_task_id": S(task, "id"), "reason": "single-flight final plan-review gate"})
	} else {
		task, created, err = s.CreateTask(CreateTaskOptions{ProjectID: S(caller, "project_id"), CollectionID: S(caller, "collection_id"), Profile: o.Profile, Title: o.Title, Objective: o.Objective, Reason: o.Reason, ParentTaskID: callerID, RequestedByTaskID: callerID, DependsOn: deps, WorkspaceMode: o.WorkspaceMode, BaseCommit: o.BaseCommit, Depth: depth, AllowDedupe: true, SpawnedByTaskID: callerID, BlockingCallerTaskID: func() string {
			if effective {
				return callerID
			}
			return ""
		}(), GateImplementTasks: gate, CriticBaselineTaskID: S(baselineCritic, "id"), CriticBaselineFingerprint: baselineFP, CriticBaselineSource: baselineSource})
		if err != nil {
			return nil, false, err
		}
	}
	_ = s.Event(S(caller, "project_id"), S(caller, "collection_id"), callerID, "", "TASK_REQUESTED", Row{"requested_task_id": S(task, "id"), "profile": o.Profile, "blocking": effective, "created": created})
	if len(ignoredImpl) > 0 {
		_ = s.Event(S(caller, "project_id"), S(caller, "collection_id"), callerID, "", "PLAN_REVIEW_IMPLEMENT_DEPENDENCIES_IGNORED", Row{"plan_review_task_id": S(task, "id"), "implement_task_ids": ignoredImpl, "reason": "plan_review is a pre-write gate and cannot depend on implementation execution"})
	}
	if ignoredCaller {
		_ = s.Event(S(caller, "project_id"), S(caller, "collection_id"), callerID, "", "PLAN_REVIEW_CALLER_DEPENDENCY_IGNORED", Row{"plan_review_task_id": S(task, "id"), "coordinator_task_id": callerID, "reason": "blocking plan-review handoff makes the coordinator depend on the review"})
	}
	return task, created, nil
}

func (s *Store) RequestCoordinatorAttention(callerID, reason string, blocking bool, limits Limits) (Row, bool, error) {
	caller, err := s.GetTask(callerID)
	if err != nil {
		return nil, false, err
	}
	if S(caller, "profile") == "coordinator" {
		return nil, false, errors.New("coordinator already owns graph-level reasoning; it should modify/request work directly rather than spawning another coordinator")
	}
	n, _ := s.CountTasks(S(caller, "collection_id"))
	if n >= limits.MaxTasksPerCollection {
		return nil, false, errors.New("collection task limit reached")
	}
	depth := I(caller, "depth") + 1
	if depth > limits.MaxTaskDepth {
		return nil, false, errors.New("maximum task depth reached")
	}
	t, created, err := s.CreateTask(CreateTaskOptions{ProjectID: S(caller, "project_id"), CollectionID: S(caller, "collection_id"), Profile: "coordinator", Title: "Coordinator attention", Objective: "Re-evaluate the collection plan because: " + reason, Reason: reason, ParentTaskID: callerID, RequestedByTaskID: callerID, WorkspaceMode: "collection", Depth: depth, AllowDedupe: false, SpawnedByTaskID: callerID, BlockingCallerTaskID: func() string {
		if blocking {
			return callerID
		}
		return ""
	}()})
	if err != nil {
		return nil, false, err
	}
	_ = s.Event(S(caller, "project_id"), S(caller, "collection_id"), callerID, "", "COORDINATOR_ATTENTION_REQUESTED", Row{"coordinator_task_id": S(t, "id"), "reason": reason, "blocking": blocking})
	return t, created, nil
}

func (s *Store) RefreshWaitingTasks(cid string) ([]string, error) {
	where := "status='WAITING'"
	if cid != "" {
		where += " AND collection_id=" + SQLLiteral(cid)
	}
	w, err := s.Query("SELECT id FROM tasks WHERE " + where)
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, r := range w {
		id := S(r, "id")
		u, er := s.Query("SELECT COUNT(*) AS n FROM task_edges e JOIN tasks t ON t.id=e.from_task_id WHERE e.to_task_id=" + SQLLiteral(id) + " AND e.relation='depends_on' AND t.status!='COMPLETE'")
		if er != nil {
			return nil, er
		}
		if len(u) > 0 && I(u[0], "n") == 0 {
			if er = s.SetTaskStatus(id, TaskReady, "task "+id+": dependencies satisfied"); er != nil {
				return nil, er
			}
			out = append(out, id)
		}
	}
	return out, nil
}
func (s *Store) RunnableTasks(cid string, limit int) (Rows, error) {
	where := "t.status IN ('READY','PENDING')"
	if cid != "" {
		where += " AND t.collection_id=" + SQLLiteral(cid)
	}
	q := "SELECT t.* FROM tasks t WHERE " + where + " AND NOT EXISTS (SELECT 1 FROM task_edges e JOIN tasks dep ON dep.id=e.from_task_id WHERE e.to_task_id=t.id AND e.relation='depends_on' AND dep.status!='COMPLETE') AND NOT EXISTS (SELECT 1 FROM task_edges eb JOIN attempts pa ON pa.task_id=eb.to_task_id JOIN sessions ps ON ps.attempt_id=pa.id WHERE eb.from_task_id=t.id AND eb.to_task_id=t.parent_task_id AND eb.relation='depends_on' AND ps.status='RUNNING') ORDER BY t.priority DESC,t.created_at LIMIT " + strconv.Itoa(limit)
	return s.Query(q)
}

func (s *Store) planningGateAllowsClaimLocked(task Row) (bool, error) {
	profile := S(task, "profile")
	if profile != "implement" && profile != "plan_review" {
		return true, nil
	}
	cr, err := s.queryLocked("SELECT objective, created_at FROM collections WHERE id=" + SQLLiteral(S(task, "collection_id")) + " LIMIT 1")
	if err != nil {
		return false, err
	}
	obj := ""
	col := Row{}
	if len(cr) > 0 {
		obj = S(cr[0], "objective")
		col = cr[0]
	}
	policy := effectivePlanningPolicy(col)
	if profile == "plan_review" {
		if !policy.CriticRequired {
			return true, nil
		}
		crit, err := s.queryLocked("SELECT * FROM tasks WHERE collection_id=" + SQLLiteral(S(task, "collection_id")) + " AND profile='critic' AND status NOT IN ('CANCELLED','SUPERSEDED') ORDER BY created_at,id")
		if err != nil {
			return false, err
		}
		if len(crit) == 0 {
			return false, nil
		}
		c := crit[len(crit)-1]
		if S(c, "status") != TaskComplete {
			return false, nil
		}
		if policy.ResearchRequired {
			research, e := s.researchAncestorsLocked(S(c, "id"))
			if e != nil {
				return false, e
			}
			if len(research) == 0 {
				return false, nil
			}
			if e := ValidateResearchConsumerResult(JSONMap(S(c, "result_json")), research, false); e != nil {
				return false, nil
			}
		}
		if ok, e := s.directiveTraceabilityGateLocked(S(c, "id"), c, true); e != nil || !ok {
			return ok, e
		}
		ed, err := s.queryLocked("SELECT 1 AS ok FROM task_edges WHERE from_task_id=" + SQLLiteral(S(c, "id")) + " AND to_task_id=" + SQLLiteral(S(task, "id")) + " AND relation='depends_on' LIMIT 1")
		if err != nil || len(ed) == 0 {
			return false, err
		}
		base, err := s.criticPlanBaselineLocked(S(c, "id"))
		if err != nil || base == nil || S(base, "fingerprint") == "" {
			return false, err
		}
		cur, err := s.materialPlanFingerprintLocked(S(task, "collection_id"), obj, false)
		if err != nil {
			return false, err
		}
		legacy, err := s.materialPlanFingerprintLocked(S(task, "collection_id"), obj, true)
		if err != nil {
			return false, err
		}
		fp := S(base, "fingerprint")
		return fp == cur || fp == legacy, nil
	}
	if !policy.PlanReviewRequired {
		// Artifact-derived plan-review floor. A COHERENT (post-cutoff) Collection
		// must never hand out an implement claim before its latest plan_review has
		// reached a terminal decision, regardless of what the objective-text
		// classifier inferred: the gate is an invariant of the graph, not only a
		// planning_policy boolean. This reuses the SAME latest-review lookup the
		// policy-required path below uses; it is a mode branch, not a second
		// parallel predicate. Pre-cutoff (non-coherent) Collections keep the exact
		// pre-floor behavior so forward-scope/non-retroactivity holds unchanged.
		if !planningGateCoherent(col) {
			return true, nil
		}
		// The preflight symmetry floor is ANDed into this SAME mode branch rather
		// than added as a parallel predicate: the implement releases only when BOTH
		// the plan-review decision and this generation's explore/critic preflight
		// have reached terminal decisions. It is scoped to gate-free coherent
		// collections (see preflightFloorAllowsClaimLocked), so a policy-required
		// coherent collection keeps the classifier-driven checks below untouched.
		prOK, err := s.planReviewFloorAllowsClaimLocked(task)
		if err != nil || !prOK {
			return prOK, err
		}
		return s.preflightFloorAllowsClaimLocked(task, col)
	}
	reviews, err := s.queryLocked("SELECT * FROM tasks WHERE collection_id=" + SQLLiteral(S(task, "collection_id")) + " AND profile='plan_review' AND status NOT IN ('CANCELLED','SUPERSEDED') ORDER BY created_at,id")
	if err != nil {
		return false, err
	}
	if len(reviews) == 0 {
		return false, nil
	}
	review := reviews[len(reviews)-1]
	if S(review, "status") != TaskComplete || S(review, "updated_at") < S(task, "created_at") {
		return false, nil
	}
	reviewResult := JSONMap(S(review, "result_json"))
	if strings.ToLower(strings.TrimSpace(S(reviewResult, "verdict"))) != "pass" {
		return false, nil
	}
	if policy.ResearchRequired {
		research, e := s.researchAncestorsLocked(S(review, "id"))
		if e != nil {
			return false, e
		}
		if len(research) == 0 {
			return false, nil
		}
		if e := ValidateResearchConsumerResult(reviewResult, research, true); e != nil {
			return false, nil
		}
	}
	if ok, e := s.directiveTraceabilityGateLocked(S(review, "id"), review, true); e != nil || !ok {
		return ok, e
	}
	edge, err := s.queryLocked("SELECT 1 AS ok FROM task_edges WHERE from_task_id=" + SQLLiteral(S(review, "id")) + " AND to_task_id=" + SQLLiteral(S(task, "id")) + " AND relation='depends_on' LIMIT 1")
	if err != nil || len(edge) == 0 {
		return false, err
	}
	if policy.CriticRequired {
		crit, er := s.queryLocked("SELECT * FROM tasks WHERE collection_id=" + SQLLiteral(S(task, "collection_id")) + " AND profile='critic' AND status NOT IN ('CANCELLED','SUPERSEDED') ORDER BY created_at,id")
		if er != nil {
			return false, er
		}
		if len(crit) == 0 {
			return false, nil
		}
		c := crit[len(crit)-1]
		if S(c, "status") != TaskComplete {
			return false, nil
		}
		if policy.ResearchRequired {
			research, er := s.researchAncestorsLocked(S(c, "id"))
			if er != nil {
				return false, er
			}
			if len(research) == 0 {
				return false, nil
			}
			if er := ValidateResearchConsumerResult(JSONMap(S(c, "result_json")), research, false); er != nil {
				return false, nil
			}
		}
		if ok, e := s.directiveTraceabilityGateLocked(S(review, "id"), review, true); e != nil || !ok {
			return ok, e
		}
		base, er := s.criticPlanBaselineLocked(S(c, "id"))
		if er != nil || base == nil || S(base, "fingerprint") == "" {
			return false, er
		}
		cur, er := s.materialPlanFingerprintLocked(S(task, "collection_id"), obj, false)
		if er != nil {
			return false, er
		}
		legacy, er := s.materialPlanFingerprintLocked(S(task, "collection_id"), obj, true)
		if er != nil {
			return false, er
		}
		fp := S(base, "fingerprint")
		if fp != cur && fp != legacy {
			return false, nil
		}
		if S(c, "updated_at") > S(review, "updated_at") {
			return false, nil
		}
		ce, er := s.queryLocked("SELECT 1 AS ok FROM task_edges WHERE from_task_id=" + SQLLiteral(S(c, "id")) + " AND to_task_id=" + SQLLiteral(S(review, "id")) + " AND relation='depends_on' LIMIT 1")
		if er != nil || len(ce) == 0 {
			return false, er
		}
	}
	ac, err := s.queryLocked("SELECT COUNT(*) AS n FROM tasks WHERE collection_id=" + SQLLiteral(S(task, "collection_id")) + " AND profile='coordinator' AND status IN ('PENDING','READY','RUNNING','WAITING','VERIFYING')")
	if err != nil {
		return false, err
	}
	return len(ac) == 0 || I(ac[0], "n") == 0, nil
}

// planReviewFloorAllowsClaimLocked is the artifact-derived plan-review floor
// reached only for a COHERENT Collection whose objective-text classifier did
// not require a plan review. It proves a plan-review DECISION exists, nothing
// more: an implement claim is blocked while the Collection has no
// non-cancelled/non-superseded plan_review row at all or its latest review is
// still nonterminal, and is released once that latest review reaches a terminal
// COMPLETE with a passing verdict. It deliberately omits the policy-required
// path's research, directive-traceability, fingerprint, dependency-edge, and
// coordinator-inactivity extras, and it reuses the SAME latest-review lookup so
// the two modes are one code path. The verdict check is NOT one of the omitted
// extras: a terminal `verdict=fail` is a decision that implementation must not
// proceed under, so the floor holds on a refused plan exactly as the policy path
// does rather than trusting the fail-review repair lineage to have already
// recorded the NEWER (nonterminal) review that would otherwise release it.
func (s *Store) planReviewFloorAllowsClaimLocked(task Row) (bool, error) {
	reviews, err := s.queryLocked("SELECT * FROM tasks WHERE collection_id=" + SQLLiteral(S(task, "collection_id")) + " AND profile='plan_review' AND status NOT IN ('CANCELLED','SUPERSEDED') ORDER BY created_at,id")
	if err != nil {
		return false, err
	}
	if len(reviews) == 0 {
		return false, nil
	}
	review := reviews[len(reviews)-1]
	if S(review, "status") != TaskComplete {
		return false, nil
	}
	return strings.ToLower(strings.TrimSpace(S(JSONMap(S(review, "result_json")), "verdict"))) == "pass", nil
}

// preflightFloorAllowsClaimLocked is the artifact-derived preflight symmetry
// floor: a sibling of planReviewFloorAllowsClaimLocked reached only from the SAME
// coherent, not-plan-review-required mode branch in planningGateAllowsClaimLocked.
// It proves the repository-exploration and critic preflight DECISIONS exist for
// THIS coordinator generation, exactly as the plan-review floor proves a
// plan-review decision exists. An implement claim is held until this generation's
// explore AND critic have EACH reached a terminal COMPLETE status. Terminal
// completion only: it deliberately does NOT invent a critic pass/fail verdict
// (critic output is advisory guidance, not a gate verdict). "This generation" is
// resolved by the SAME title-based generation predicate the scheduler staging
// floor uses (scheduler.go gateFree floorCoordinator): the latest coordinator
// whose title is a generation-creating title ('Initial coordination' or
// 'Acceptance revision'), excluding CANCELLED/SUPERSEDED. Every other coordinator
// title (attention, critic/research revalidation, delivery-policy repair) is a
// within-generation helper that neither creates a generation nor moves this
// anchor, so the claim floor and the staging floor agree byte-for-byte on which
// coordinator owns the generation. Because the per-profile check below is a
// terminal-COMPLETE-only test, a FAILED (terminal non-COMPLETE) generation gate
// does NOT self-release the floor: release happens only via the failed task's own
// retry-then-coordinator-attention path, the fail-review repair lineage for plan
// reviews, or an operator collection revision. It is
// a mode branch that the caller ANDs with the plan-review floor, NOT a second
// parallel gate, and is scoped to GATE-FREE collections: any collection whose
// classifier already required a gate keeps the existing policy-driven claim checks
// below unchanged, and pre-cutoff (non-coherent) collections never reach it. This
// is the claim-time complement to PREFLIGHT_FLOOR_STAGED, closing the live
// gate-free-coherent gap (COL-4274d1d8d9-shaped) where an implement could be
// claimed on plan_review evidence alone with zero preflight evidence.
func (s *Store) preflightFloorAllowsClaimLocked(task Row, col Row) (bool, error) {
	p := effectivePlanningPolicy(col)
	if p.ExploreRequired || p.ResearchRequired || p.CriticRequired || p.PlanReviewRequired {
		// Not gate-free: the classifier already drives the policy-required claim
		// checks elsewhere, so the floor adds no hold here.
		return true, nil
	}
	coords, err := s.queryLocked("SELECT id FROM tasks WHERE collection_id=" + SQLLiteral(S(task, "collection_id")) + " AND profile='coordinator' AND status NOT IN ('CANCELLED','SUPERSEDED') AND (title='Initial coordination' OR title='Acceptance revision') ORDER BY created_at,id")
	if err != nil {
		return false, err
	}
	if len(coords) == 0 {
		// No coordinator generation to anchor this floor; the plan-review floor is
		// the governing artifact and has already released.
		return true, nil
	}
	coordID := S(coords[len(coords)-1], "id")
	for _, profile := range []string{"explore", "critic"} {
		gen, e := s.queryLocked("SELECT status FROM tasks WHERE collection_id=" + SQLLiteral(S(task, "collection_id")) + " AND profile=" + SQLLiteral(profile) + " AND requested_by_task_id=" + SQLLiteral(coordID) + " AND status NOT IN ('CANCELLED','SUPERSEDED') ORDER BY created_at,id")
		if e != nil {
			return false, e
		}
		if len(gen) == 0 || S(gen[len(gen)-1], "status") != TaskComplete {
			return false, nil
		}
	}
	return true, nil
}

// claimGateIsFloorMode reports whether an implement's claim decision was made
// under the artifact floor rather than the policy-required path. It reuses the
// same effectivePlanningPolicy / planningGateCoherent seam as the gate and makes
// no allow/deny decision of its own, so it is not a second parallel gate.
// ClaimTask consults it only to label an already-refused claim in the durable
// audit trail, distinguishing a floor-held write task from a policy-held one.
func (s *Store) claimGateIsFloorModeLocked(task Row) (bool, error) {
	if S(task, "profile") != "implement" {
		return false, nil
	}
	cr, err := s.queryLocked("SELECT objective, created_at FROM collections WHERE id=" + SQLLiteral(S(task, "collection_id")) + " LIMIT 1")
	if err != nil {
		return false, err
	}
	col := Row{}
	if len(cr) > 0 {
		col = cr[0]
	}
	if !planningGateCoherent(col) || effectivePlanningPolicy(col).PlanReviewRequired {
		return false, nil
	}
	return true, nil
}

// directiveTraceabilityGateLocked is the claim/read-time gate mirroring the
// research-consistency claim gates: a completed dispositioning consumer
// (review or plan_review) must have dispositioned every inherited planning
// directive. Consumer producers (critic) are not gated on their own output.
// Pre-existing history that predates the validator is exempted only through
// the documented, collection-scoped single-decision legacy exception, never
// silently. This is the LIVE claim-time seam; impact enumeration by contrast
// is enforced only at submit and acceptance, so there is deliberately no
// claim-time impact-enumeration gate here.
func (s *Store) directiveTraceabilityGateLocked(ancestorID string, consumer Row, requirePass bool) (bool, error) {
	if S(consumer, "status") != TaskComplete {
		return true, nil
	}
	ancestors, err := s.directiveAncestorsLocked(ancestorID)
	if err != nil {
		return false, err
	}
	directives := InheritedDirectives(ancestors)
	if len(directives) == 0 {
		return true, nil
	}
	result := M(consumer, "result")
	if len(result) == 0 {
		result = JSONMap(S(consumer, "result_json"))
	}
	if ValidateDirectiveTraceability(result, directives, requirePass, nil) == nil {
		return true, nil
	}
	decisions, err := s.decisionsLocked(S(consumer, "project_id"), S(consumer, "collection_id"))
	if err != nil {
		return false, err
	}
	return len(LegacyGateTraceabilityExceptionIDs(decisions, S(consumer, "collection_id"))) > 0, nil
}

// decisionsLocked returns durable decisions scoped to the project or the
// given collection that have not been retired via superseded_by.
func (s *Store) decisionsLocked(projectID, collectionID string) (Rows, error) {
	return s.queryLocked("SELECT * FROM decisions WHERE (project_id=" + SQLLiteral(projectID) + " AND (collection_id IS NULL OR collection_id=" + SQLLiteral(collectionID) + ")) AND superseded_by IS NULL ORDER BY created_at")
}

func (s *Store) ClaimTask(tid string, maxAttempts int) (Row, Row, error) {
	var task, attempt Row
	floorHeld := false
	floorPreflight := false
	floorProjectID, floorCollectionID := "", ""
	err := s.withLock(func() error {
		rows, e := s.queryLocked("SELECT * FROM tasks WHERE id=" + SQLLiteral(tid) + " LIMIT 1")
		if e != nil {
			return e
		}
		if len(rows) == 0 || (S(rows[0], "status") != TaskReady && S(rows[0], "status") != TaskPending) {
			return nil
		}
		ok, e := s.planningGateAllowsClaimLocked(rows[0])
		if e != nil {
			return e
		}
		if !ok {
			// A refused claim is held either by the policy-required path or by the
			// artifact plan-review floor. The decision above is the only gate; this
			// only labels which mode refused so an operator can tell why a READY
			// implement is not starting. Fan-out runs after the lock releases.
			if isFloor, fe := s.claimGateIsFloorModeLocked(rows[0]); fe == nil && isFloor {
				floorHeld = true
				floorProjectID, floorCollectionID = S(rows[0], "project_id"), S(rows[0], "collection_id")
				// The combined floor released the plan-review half but refused overall,
				// so the preflight symmetry floor is the holding reason; otherwise the
				// refusal is the plan-review floor. This only labels the already-made
				// decision; it re-uses the same pure Locked check and gates nothing.
				if prOK, pe := s.planReviewFloorAllowsClaimLocked(rows[0]); pe == nil && prOK {
					floorPreflight = true
				}
			}
			return nil
		}
		unmet, e := s.queryLocked("SELECT COUNT(*) AS n FROM task_edges e JOIN tasks dep ON dep.id=e.from_task_id WHERE e.to_task_id=" + SQLLiteral(tid) + " AND e.relation='depends_on' AND dep.status!='COMPLETE'")
		if e != nil {
			return e
		}
		if len(unmet) > 0 && I(unmet[0], "n") != 0 {
			return nil
		}
		ar, e := s.queryLocked("SELECT COUNT(*) AS n FROM attempts WHERE task_id=" + SQLLiteral(tid))
		if e != nil {
			return e
		}
		no := 1
		if len(ar) > 0 {
			no = I(ar[0], "n") + 1
		}
		if no > maxAttempts {
			if e = s.mutateLocked("UPDATE tasks SET status='FAILED',updated_at=" + SQLLiteral(UTCNow()) + " WHERE id=" + SQLLiteral(tid)); e != nil {
				return e
			}
			return s.semanticCommitLocked("task " + tid + ": exhausted attempts")
		}
		aid := MakeID("A")
		now := UTCNow()
		q := fmt.Sprintf("UPDATE tasks SET status='RUNNING',updated_at=%s WHERE id=%s;INSERT INTO attempts (id,task_id,attempt_no,status,started_at,created_at) VALUES (%s,%s,%d,'RUNNING',%s,%s)", SQLLiteral(now), SQLLiteral(tid), SQLLiteral(aid), SQLLiteral(tid), no, SQLLiteral(now), SQLLiteral(now))
		if e = s.mutateLocked(q); e != nil {
			return e
		}
		if e = s.semanticCommitLocked(fmt.Sprintf("task %s: start attempt %d", tid, no)); e != nil {
			return e
		}
		tr, e := s.queryLocked("SELECT * FROM tasks WHERE id=" + SQLLiteral(tid) + " LIMIT 1")
		if e != nil {
			return e
		}
		at, e := s.queryLocked("SELECT * FROM attempts WHERE id=" + SQLLiteral(aid) + " LIMIT 1")
		if e != nil {
			return e
		}
		task = tr[0]
		attempt = at[0]
		return nil
	})
	if err != nil {
		return task, attempt, err
	}
	if floorHeld {
		if floorPreflight {
			_ = s.Event(floorProjectID, floorCollectionID, tid, "", "PREFLIGHT_FLOOR_HELD", Row{
				"hold_kind": "preflight_floor",
				"reason":    "implement claim held by the artifact-derived preflight floor: this coordinator generation's explore and critic preflight have not both reached terminal COMPLETE yet",
			})
		} else {
			_ = s.Event(floorProjectID, floorCollectionID, tid, "", "PLAN_REVIEW_FLOOR_HELD", Row{
				"hold_kind": "plan_review_floor",
				"reason":    "implement claim held by the artifact-derived plan-review floor: the Collection has no terminal plan_review decision yet",
			})
		}
	}
	return task, attempt, err
}
func (s *Store) SetAttemptLogPath(aid, path string) error {
	return s.Mutate("UPDATE attempts SET log_path="+SQLLiteral(path)+" WHERE id="+SQLLiteral(aid), "")
}
func (s *Store) FinishAttempt(aid, status string, exitCode *int, result Row, ext, log string) error {
	var ec any = nil
	if exitCode != nil {
		ec = *exitCode
	}
	return s.Mutate("UPDATE attempts SET status="+SQLLiteral(status)+",exit_code="+SQLLiteral(ec)+",external_session_id="+nullString(ext)+",log_path="+nullString(log)+",result_json="+SQLLiteral(JSONDumps(result))+",ended_at="+SQLLiteral(UTCNow())+" WHERE id="+SQLLiteral(aid), "")
}
func (s *Store) CreateSession(aid, harness, workdir string) (Row, error) {
	id := MakeID("S")
	now := UTCNow()
	q := fmt.Sprintf("INSERT INTO sessions (id,attempt_id,harness,status,workdir,started_at,created_at) VALUES (%s,%s,%s,'RUNNING',%s,%s,%s)", SQLLiteral(id), SQLLiteral(aid), SQLLiteral(harness), SQLLiteral(workdir), SQLLiteral(now), SQLLiteral(now))
	if err := s.Mutate(q, ""); err != nil {
		return nil, err
	}
	r, err := s.Query("SELECT * FROM sessions WHERE id=" + SQLLiteral(id) + " LIMIT 1")
	if err != nil {
		return nil, err
	}
	return r[0], nil
}
func (s *Store) SetSessionPID(sid string, pid int) error {
	return s.Mutate("UPDATE sessions SET pid="+strconv.Itoa(pid)+" WHERE id="+SQLLiteral(sid), "")
}
func (s *Store) RunningSessions() (Rows, error) {
	return s.Query("SELECT s.*,a.task_id,a.status AS attempt_status,t.status AS task_status FROM sessions s JOIN attempts a ON a.id=s.attempt_id JOIN tasks t ON t.id=a.task_id WHERE s.status='RUNNING' ORDER BY s.started_at")
}
func (s *Store) HasRunningSession(collectionID string) (bool, error) {
	rows, err := s.Query("SELECT s.id FROM sessions s JOIN attempts a ON a.id=s.attempt_id JOIN tasks t ON t.id=a.task_id WHERE s.status='RUNNING' AND t.collection_id=" + SQLLiteral(collectionID) + " LIMIT 1")
	if err != nil {
		return false, err
	}
	return len(rows) > 0, nil
}
func (s *Store) AbortStaleSession(sid, aid, tid string) error {
	now := UTCNow()
	return s.withLock(func() error {
		q := "UPDATE sessions SET status='ABORTED',ended_at=" + SQLLiteral(now) + " WHERE id=" + SQLLiteral(sid) + ";UPDATE attempts SET status='ABORTED',ended_at=" + SQLLiteral(now) + " WHERE id=" + SQLLiteral(aid) + ";UPDATE tasks SET status=CASE WHEN status IN ('RUNNING','VERIFYING') THEN 'READY' ELSE status END,updated_at=" + SQLLiteral(now) + " WHERE id=" + SQLLiteral(tid)
		if err := s.mutateLocked(q); err != nil {
			return err
		}
		return s.semanticCommitLocked("recovery: abort stale session " + sid)
	})
}
func (s *Store) FinishSession(sid, ext, status string) error {
	return s.Mutate("UPDATE sessions SET status="+SQLLiteral(status)+",external_session_id="+nullString(ext)+",ended_at="+SQLLiteral(UTCNow())+" WHERE id="+SQLLiteral(sid), "")
}

func (s *Store) GetParent(task Row) (Row, error) {
	id := S(task, "parent_task_id")
	if id == "" {
		return nil, nil
	}
	return s.GetTask(id)
}
func (s *Store) ListMemories(pid, cid, state string) (Rows, error) {
	if state == "" {
		state = "accepted"
	}
	where := "project_id=" + SQLLiteral(pid) + " AND state=" + SQLLiteral(state)
	if cid != "" {
		where += " AND (scope='project' OR collection_id=" + SQLLiteral(cid) + ")"
	} else {
		where += " AND scope='project'"
	}
	return s.Query("SELECT * FROM memories WHERE " + where + " ORDER BY created_at")
}
func (s *Store) AddMemory(pid, cid, tid, scope, state, kind, text string, evidence any) (Row, error) {
	id := MakeID("MEM")
	now := UTCNow()
	if evidence == nil {
		evidence = []any{}
	}
	q := fmt.Sprintf("INSERT INTO memories (id,project_id,collection_id,task_id,scope,state,kind,text,evidence_json,created_at,updated_at) VALUES (%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s)", SQLLiteral(id), SQLLiteral(pid), nullString(cid), nullString(tid), SQLLiteral(scope), SQLLiteral(state), SQLLiteral(kind), SQLLiteral(text), SQLLiteral(JSONDumps(evidence)), SQLLiteral(now), SQLLiteral(now))
	if err := s.Mutate(q, "memory "+id+": add "+scope+" "+kind); err != nil {
		return nil, err
	}
	r, err := s.Query("SELECT * FROM memories WHERE id=" + SQLLiteral(id) + " LIMIT 1")
	if err != nil {
		return nil, err
	}
	return r[0], nil
}
func (s *Store) ListDecisions(pid, cid string) (Rows, error) {
	where := "project_id=" + SQLLiteral(pid)
	if cid != "" {
		where += " AND (collection_id IS NULL OR collection_id=" + SQLLiteral(cid) + ")"
	}
	return s.Query("SELECT * FROM decisions WHERE " + where + " AND superseded_by IS NULL ORDER BY created_at")
}
func (s *Store) AddDecision(pid, cid, tid, statement, rationale string, evidence, alts any) (Row, error) {
	id := MakeID("DEC")
	if evidence == nil {
		evidence = []any{}
	}
	if alts == nil {
		alts = []any{}
	}
	q := fmt.Sprintf("INSERT INTO decisions (id,project_id,collection_id,task_id,statement,rationale,evidence_json,alternatives_json,created_at) VALUES (%s,%s,%s,%s,%s,%s,%s,%s,%s)", SQLLiteral(id), SQLLiteral(pid), nullString(cid), nullString(tid), SQLLiteral(statement), SQLLiteral(rationale), SQLLiteral(JSONDumps(evidence)), SQLLiteral(JSONDumps(alts)), SQLLiteral(UTCNow()))
	short := statement
	if len(short) > 80 {
		short = short[:80]
	}
	if err := s.Mutate(q, "decision "+id+": "+short); err != nil {
		return nil, err
	}
	r, err := s.Query("SELECT * FROM decisions WHERE id=" + SQLLiteral(id) + " LIMIT 1")
	if err != nil {
		return nil, err
	}
	return r[0], nil
}
func (s *Store) Event(pid, cid, tid, aid, eventType string, payload any) error {
	id := MakeID("EVT")
	now := UTCNow()
	q := fmt.Sprintf("INSERT INTO events (id,project_id,collection_id,task_id,attempt_id,event_type,payload_json,created_at) VALUES (%s,%s,%s,%s,%s,%s,%s,%s)", SQLLiteral(id), nullString(pid), nullString(cid), nullString(tid), nullString(aid), SQLLiteral(eventType), SQLLiteral(JSONDumps(payload)), SQLLiteral(now))
	if err := s.Mutate(q, ""); err != nil {
		return err
	}
	// Fan out only after Mutate's file lock has released, so the write path never
	// holds the Dolt lock across a stream publish.
	s.emitEvent(eventRecord{ID: id, ProjectID: pid, CollectionID: cid, TaskID: tid, AttemptID: aid, EventType: eventType, CreatedAt: now})
	return nil
}

// RecordEmptyDirectiveHarvest emits the DIRECTIVE_TRACEABILITY_EMPTY_HARVEST
// event naming the consumer task (in the task_id column) and the directive-
// bearing source ancestors that were checked but yielded zero harvestable
// directives (in the payload). It is observability, not a block, and the
// consumer-side dedup is the caller's responsibility; this is the single
// INSERT site so both emitting seams produce an identically-shaped event. It
// goes through Store.Event, which re-enters the lock via Mutate, so it MUST
// only be called OUTSIDE the store lock (the scheduler observer and the
// acceptance CLI seam) and NEVER from a *Locked claim context.
func (s *Store) RecordEmptyDirectiveHarvest(projectID, collectionID, consumerID string, sourceIDs []string) error {
	ids := append([]string{}, sourceIDs...)
	sort.Strings(ids)
	return s.Event(projectID, collectionID, consumerID, "", "DIRECTIVE_TRACEABILITY_EMPTY_HARVEST", Row{"source_task_ids": ids})
}
func (s *Store) TaskInputLineage(tid string) (Row, error) {
	r, err := s.Query("SELECT payload_json,attempt_id,created_at FROM events WHERE task_id=" + SQLLiteral(tid) + " AND event_type='TASK_INPUT_LINEAGE' ORDER BY created_at DESC LIMIT 1")
	if err != nil || len(r) == 0 {
		return nil, err
	}
	p := JSONMap(S(r[0], "payload_json"))
	if _, ok := p["attempt_id"]; !ok {
		p["attempt_id"] = r[0]["attempt_id"]
	}
	if _, ok := p["created_at"]; !ok {
		p["created_at"] = r[0]["created_at"]
	}
	return p, nil
}

func (s *Store) EventsForCollection(cid string, limit int) (Rows, error) {
	return s.Query("SELECT * FROM events WHERE collection_id=" + SQLLiteral(cid) + " ORDER BY created_at DESC LIMIT " + strconv.Itoa(limit))
}

// DefaultEventLimit bounds a scope scan that has no collection/task index (for
// example a project-only listing, which has no project_id index). A non-positive
// limit falls back to this finite cap so a bare event read can never trigger an
// unbounded scan.
const DefaultEventLimit = 500

// ListEvents reads the durable event log directly for the resolved scope. It is
// read-only and needs no running daemon. Ordering is deterministic and
// oldest-first: created_at then id. (created_at, id) form an EXCLUSIVE composite
// cursor: only rows strictly after (sinceCreatedAt, sinceID) are returned, so the
// caller can pass the previously emitted cursor back unchanged. Empty scope
// arguments impose no filter on that column; limit bounds the scan and a
// non-positive limit uses DefaultEventLimit. created_at DATETIME(6) is the
// monotonic ordering key while EVT ids are random hex, hence the (created_at,id)
// pair rather than the id alone.
func (s *Store) ListEvents(pid, cid, tid, eventType, sinceCreatedAt, sinceID string, limit int) (Rows, error) {
	if limit <= 0 {
		limit = DefaultEventLimit
	}
	where := []string{}
	if pid != "" {
		where = append(where, "project_id="+SQLLiteral(pid))
	}
	if cid != "" {
		where = append(where, "collection_id="+SQLLiteral(cid))
	}
	if tid != "" {
		where = append(where, "task_id="+SQLLiteral(tid))
	}
	if eventType != "" {
		where = append(where, "event_type="+SQLLiteral(eventType))
	}
	if sinceCreatedAt != "" {
		if sinceID != "" {
			where = append(where, "(created_at>"+SQLLiteral(sinceCreatedAt)+" OR (created_at="+SQLLiteral(sinceCreatedAt)+" AND id>"+SQLLiteral(sinceID)+"))")
		} else {
			where = append(where, "created_at>"+SQLLiteral(sinceCreatedAt))
		}
	}
	q := "SELECT * FROM events"
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY created_at, id LIMIT " + strconv.Itoa(limit)
	return s.Query(q)
}

// EventCreatedAt resolves the created_at of a single events row by id. The SSE
// layer uses it to turn a Last-Event-ID (an EVT id, which is not monotonic) into
// the (created_at,id) cursor for a scoped replay via ListEvents. found is false
// when the id is absent so the caller can fail closed (replay nothing) rather than
// assume a cursor.
func (s *Store) EventCreatedAt(id string) (createdAt string, found bool, err error) {
	if id == "" {
		return "", false, nil
	}
	r, err := s.Query("SELECT created_at FROM events WHERE id=" + SQLLiteral(id) + " LIMIT 1")
	if err != nil || len(r) == 0 {
		return "", false, err
	}
	return S(r[0], "created_at"), true, nil
}

func (s *Store) RepairSupersededReviewLineage(cid string) (int, error) {
	tasks, err := s.ListTasks(cid)
	if err != nil {
		return 0, err
	}
	by := map[string]Row{}
	for _, t := range tasks {
		by[S(t, "id")] = t
	}
	edges, err := s.ListEdges(cid)
	if err != nil {
		return 0, err
	}
	keys := map[string]bool{}
	sup := Rows{}
	for _, e := range edges {
		k := S(e, "from_task_id") + "\x00" + S(e, "to_task_id") + "\x00" + S(e, "relation")
		keys[k] = true
		if S(e, "relation") == "superseded_by" {
			sup = append(sup, e)
		}
	}
	repaired := 0
	changed := true
	for changed {
		changed = false
		incoming := []Row{}
		for k := range keys {
			p := strings.Split(k, "\x00")
			incoming = append(incoming, Row{"from": p[0], "to": p[1], "rel": p[2]})
		}
		for _, e := range sup {
			old := by[S(e, "from_task_id")]
			rep := by[S(e, "to_task_id")]
			if old == nil || rep == nil || S(old, "profile") != S(rep, "profile") || (S(old, "profile") != "review" && S(old, "profile") != "plan_review") {
				continue
			}
			for _, x := range incoming {
				if S(x, "to") != S(old, "id") {
					continue
				}
				rel := S(x, "rel")
				if rel != "depends_on" && !(rel == "verifies" && S(old, "profile") == "review") {
					continue
				}
				k := S(x, "from") + "\x00" + S(rep, "id") + "\x00" + rel
				if keys[k] {
					continue
				}
				if er := s.AddEdge(S(x, "from"), S(rep, "id"), cid, rel); er != nil {
					return repaired, er
				}
				keys[k] = true
				repaired++
				changed = true
			}
		}
	}
	if repaired > 0 {
		c, _ := s.GetCollection(cid)
		_ = s.Event(S(c, "project_id"), cid, "", "", "REVIEW_LINEAGE_REPAIRED", Row{"edges_added": repaired})
	}
	return repaired, nil
}
func (s *Store) ImplementationsMissingPassingReview(cid string) ([]string, error) {
	tasks, err := s.ListTasks(cid)
	if err != nil {
		return nil, err
	}
	by := map[string]Row{}
	for _, t := range tasks {
		by[S(t, "id")] = t
	}
	edges, err := s.ListEdges(cid)
	if err != nil {
		return nil, err
	}
	ver := map[string][]string{}
	for _, e := range edges {
		if S(e, "relation") == "verifies" {
			ver[S(e, "from_task_id")] = append(ver[S(e, "from_task_id")], S(e, "to_task_id"))
		}
	}
	missing := []string{}
	for _, t := range tasks {
		if S(t, "profile") != "implement" || S(t, "status") != TaskComplete {
			continue
		}
		cand := append([]string{}, ver[S(t, "id")]...)
		pid := S(t, "parent_task_id")
		if pid != "" && S(by[pid], "profile") == "review" {
			cand = append(cand, pid)
		}
		pass := false
		for _, rid := range cand {
			r := by[rid]
			if r != nil && S(r, "status") == TaskComplete && strings.ToLower(strings.TrimSpace(S(M(r, "result"), "verdict"))) == "pass" {
				pass = true
				break
			}
		}
		if !pass {
			missing = append(missing, S(t, "id"))
		}
	}
	return missing, nil
}
func (s *Store) ReconcileCollectionRevisions(cid string) (int, error) {
	revs, err := s.ListCollectionRevisions(cid, "OPEN")
	if err != nil || len(revs) == 0 {
		return 0, err
	}
	tasks, err := s.ListTasks(cid)
	if err != nil {
		return 0, err
	}
	by := map[string]Row{}
	for _, t := range tasks {
		by[S(t, "id")] = t
	}
	edges, err := s.ListEdges(cid)
	if err != nil {
		return 0, err
	}
	ver := map[string][]string{}
	for _, e := range edges {
		if S(e, "relation") == "verifies" {
			ver[S(e, "from_task_id")] = append(ver[S(e, "from_task_id")], S(e, "to_task_id"))
		}
	}
	term := map[string]bool{TaskComplete: true, TaskCancelled: true, TaskSuperseded: true}
	sat := 0
	for _, rev := range revs {
		created := S(rev, "created_at")
		post := Rows{}
		for _, t := range tasks {
			if S(t, "created_at") > created {
				post = append(post, t)
			}
		}
		impl := Rows{}
		bad := false
		for _, t := range post {
			if S(t, "profile") == "implement" {
				impl = append(impl, t)
			}
			if !term[S(t, "status")] {
				bad = true
			}
		}
		if len(impl) == 0 || bad {
			continue
		}
		completed := Rows{}
		for _, t := range impl {
			if S(t, "status") == TaskComplete {
				completed = append(completed, t)
			}
		}
		if len(completed) == 0 {
			continue
		}
		rids := []string{}
		all := true
		for _, im := range completed {
			passed := ""
			for _, rid := range ver[S(im, "id")] {
				r := by[rid]
				if r != nil && S(r, "status") == TaskComplete && S(r, "created_at") > created && strings.ToLower(strings.TrimSpace(S(M(r, "result"), "verdict"))) == "pass" {
					passed = rid
					break
				}
			}
			if passed == "" {
				all = false
				break
			}
			rids = append(rids, passed)
		}
		if !all {
			continue
		}
		now := UTCNow()
		if er := s.Mutate("UPDATE collection_revisions SET status='SATISFIED',satisfied_at="+SQLLiteral(now)+" WHERE id="+SQLLiteral(S(rev, "id"))+" AND status='OPEN'", "collection "+cid+": satisfy revision "+S(rev, "id")); er != nil {
			return sat, er
		}
		c, _ := s.GetCollection(cid)
		ids := []string{}
		for _, t := range completed {
			ids = append(ids, S(t, "id"))
		}
		_ = s.Event(S(c, "project_id"), cid, "", "", "COLLECTION_REVISION_SATISFIED", Row{"revision_id": S(rev, "id"), "implementation_task_ids": ids, "review_task_ids": rids})
		sat++
	}
	return sat, nil
}
func (s *Store) UpdateCollectionTerminalState(cid string) (string, error) {
	_, _ = s.RepairSupersededReviewLineage(cid)
	_, _ = s.ReconcileCollectionRevisions(cid)
	tasks, err := s.ListTasks(cid)
	if err != nil {
		return "", err
	}
	if len(tasks) == 0 {
		c, e := s.GetCollection(cid)
		return S(c, "status"), e
	}
	open, _ := s.ListCollectionRevisions(cid, "OPEN")
	successful := map[string]bool{TaskComplete: true, TaskCancelled: true, TaskSuperseded: true}
	allTerm := true
	hasComplete := false
	hasExec := false
	for _, t := range tasks {
		st := S(t, "status")
		if !successful[st] {
			allTerm = false
		}
		if st == TaskComplete {
			hasComplete = true
		}
		if st == TaskRunning || st == TaskVerifying {
			hasExec = true
		}
	}
	run, _ := s.RunnableTasks(cid, 1)
	if !hasExec && len(run) == 0 {
		if activeSession, sessionErr := s.HasRunningSession(cid); sessionErr == nil && activeSession {
			hasExec = true
		}
	}
	desired := "BLOCKED"
	if len(open) > 0 {
		if hasExec || len(run) > 0 {
			desired = "RUNNING"
		}
	} else if allTerm && hasComplete {
		c, _ := s.GetCollection(cid)
		dp := ClassifyDeliveryPolicy(S(c, "objective"))
		completedImpl := 0
		for _, t := range tasks {
			if S(t, "profile") == "implement" && S(t, "status") == TaskComplete {
				completedImpl++
			}
		}
		missingDelivery := dp.ImplementationRequired && completedImpl == 0
		mr, _ := s.ImplementationsMissingPassingReview(cid)
		pp := effectivePlanningPolicy(c)
		cs, _ := s.CriticRevalidationState(cid)
		stale := pp.CriticRequired && B(cs, "stale")
		if !missingDelivery && len(mr) == 0 && !stale {
			desired = "COMPLETE"
		}
	} else if hasExec || len(run) > 0 {
		desired = "RUNNING"
	}
	c, _ := s.GetCollection(cid)
	if S(c, "status") != desired {
		if err = s.SetCollectionStatus(cid, desired, desired == "COMPLETE" || desired == "BLOCKED"); err != nil {
			return "", err
		}
	}
	return desired, nil
}

func (s *Store) GetCollectionAcceptance(cid, target string) (Row, error) {
	where := "collection_id=" + SQLLiteral(cid)
	if target != "" {
		where += " AND target_branch=" + SQLLiteral(target)
	}
	r, err := s.Query("SELECT * FROM collection_acceptances WHERE " + where + " ORDER BY created_at DESC LIMIT 1")
	if err != nil || len(r) == 0 {
		return nil, err
	}
	return r[0], nil
}
func (s *Store) ListCollectionAcceptances(cid string) (Rows, error) {
	return s.Query("SELECT * FROM collection_acceptances WHERE collection_id=" + SQLLiteral(cid) + " ORDER BY created_at")
}
func (s *Store) RecordCollectionAcceptance(pid, cid, target, source, sourceCommit, targetBefore, accepted, mode, actor string) (Row, error) {
	ex, err := s.GetCollectionAcceptance(cid, target)
	if err != nil {
		return nil, err
	}
	if ex != nil {
		return ex, nil
	}
	id := MakeID("ACC")
	now := UTCNow()
	q := fmt.Sprintf("INSERT INTO collection_acceptances (id,project_id,collection_id,target_branch,source_branch,source_commit,target_before_commit,accepted_commit,merge_mode,actor,created_at) VALUES (%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s)", SQLLiteral(id), SQLLiteral(pid), SQLLiteral(cid), SQLLiteral(target), SQLLiteral(source), SQLLiteral(sourceCommit), SQLLiteral(targetBefore), SQLLiteral(accepted), SQLLiteral(mode), nullString(actor), SQLLiteral(now))
	short := accepted
	if len(short) > 12 {
		short = short[:12]
	}
	if err = s.Mutate(q, "collection "+cid+": accepted into "+target+" at "+short); err != nil {
		return nil, err
	}
	_ = s.Event(pid, cid, "", "", "COLLECTION_ACCEPTED", Row{"target_branch": target, "source_branch": source, "source_commit": sourceCommit, "target_before_commit": targetBefore, "accepted_commit": accepted, "merge_mode": mode, "actor": nullable(actor)})
	return s.GetCollectionAcceptance(cid, target)
}
func (s *Store) GetCollectionPullRequest(cid, provider string) (Row, error) {
	if provider == "" {
		provider = "github"
	}
	r, err := s.Query("SELECT * FROM collection_pull_requests WHERE collection_id=" + SQLLiteral(cid) + " AND provider=" + SQLLiteral(provider) + " ORDER BY created_at DESC LIMIT 1")
	if err != nil || len(r) == 0 {
		return nil, err
	}
	return r[0], nil
}
func (s *Store) ListCollectionPullRequests(cid string) (Rows, error) {
	return s.Query("SELECT * FROM collection_pull_requests WHERE collection_id=" + SQLLiteral(cid) + " ORDER BY created_at")
}

type PRRecord struct {
	ProjectID, CollectionID, Provider, RemoteName, Repository, URL, State, BaseBranch, HeadBranch, SourceCommit, Title, Actor string
	Number                                                                                                                    int
	IsDraft                                                                                                                   bool
}

func (s *Store) RecordCollectionPullRequest(x PRRecord) (Row, error) {
	ex, err := s.GetCollectionPullRequest(x.CollectionID, x.Provider)
	if err != nil {
		return nil, err
	}
	now := UTCNow()
	event := ""
	if ex != nil {
		q := fmt.Sprintf("UPDATE collection_pull_requests SET remote_name=%s,repository=%s,pr_number=%d,url=%s,state=%s,is_draft=%s,base_branch=%s,head_branch=%s,source_commit=%s,title=%s,actor=%s,updated_at=%s WHERE id=%s", SQLLiteral(x.RemoteName), nullString(x.Repository), x.Number, SQLLiteral(x.URL), SQLLiteral(x.State), SQLLiteral(x.IsDraft), SQLLiteral(x.BaseBranch), SQLLiteral(x.HeadBranch), SQLLiteral(x.SourceCommit), SQLLiteral(x.Title), nullString(x.Actor), SQLLiteral(now), SQLLiteral(S(ex, "id")))
		if err = s.Mutate(q, fmt.Sprintf("collection %s: refresh %s PR #%d", x.CollectionID, x.Provider, x.Number)); err != nil {
			return nil, err
		}
		event = "COLLECTION_PR_UPDATED"
	} else {
		id := MakeID("PR")
		q := fmt.Sprintf("INSERT INTO collection_pull_requests (id,project_id,collection_id,provider,remote_name,repository,pr_number,url,state,is_draft,base_branch,head_branch,source_commit,title,actor,created_at,updated_at) VALUES (%s,%s,%s,%s,%s,%s,%d,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s)", SQLLiteral(id), SQLLiteral(x.ProjectID), SQLLiteral(x.CollectionID), SQLLiteral(x.Provider), SQLLiteral(x.RemoteName), nullString(x.Repository), x.Number, SQLLiteral(x.URL), SQLLiteral(x.State), SQLLiteral(x.IsDraft), SQLLiteral(x.BaseBranch), SQLLiteral(x.HeadBranch), SQLLiteral(x.SourceCommit), SQLLiteral(x.Title), nullString(x.Actor), SQLLiteral(now), SQLLiteral(now))
		if err = s.Mutate(q, fmt.Sprintf("collection %s: record %s PR #%d", x.CollectionID, x.Provider, x.Number)); err != nil {
			return nil, err
		}
		event = "COLLECTION_PR_RECORDED"
	}
	_ = s.Event(x.ProjectID, x.CollectionID, "", "", event, Row{"provider": x.Provider, "remote_name": x.RemoteName, "repository": nullable(x.Repository), "pr_number": x.Number, "url": x.URL, "state": x.State, "is_draft": x.IsDraft, "base_branch": x.BaseBranch, "head_branch": x.HeadBranch, "source_commit": x.SourceCommit, "title": x.Title, "actor": nullable(x.Actor)})
	return s.GetCollectionPullRequest(x.CollectionID, x.Provider)
}
