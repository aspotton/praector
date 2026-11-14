package praector

import (
	"strings"
)

// This file holds the pure decision cores behind the `praector status` read path
// and the single batched snapshot loader that feeds them (docs/PERFORMANCE.md
// Phase 2). Each status-path decision is split into a *core* that operates on
// already-loaded rows plus the existing cid wrapper that loads then calls the
// core (DEC-3db2c25cd9). The scheduler, claim, delivery-plan, and
// CollectionShow/CollectionRevise callers keep calling the unchanged cid wrapper
// methods, so they share exactly one decide implementation with the batched
// Status path and cannot drift from it.
//
// Dolt contract scope: the batched read is a single `dolt sql -r json -q`
// multi-SELECT built on queryManyLocked (see its contract). The verified
// document shape (one document per statement, `{}` for an empty member) is pinned
// to Dolt 2.7.2; the latest release 2.7.3 is contract-unchanged (research C-8).
// Intra-batch consistency rests solely on the OS store lock serializing writers;
// no BEGIN/transaction is introduced (research C-7). Phase 2 invokes only local
// dolt sql/add/commit subprocesses and exposes no network/remote/protocol
// surface, so the threat-model and external-conformance gates are waived per
// DEC-dc25765062 (research C-9) rather than silently dropped.

// collectionSnapshot is one collection's already-loaded rows: the batched
// snapshot a status-path core consumes instead of re-reading the same tables per
// decision. The loader fills every field; the reconcile cores then mutate the
// edges and openRevisions buckets IN PLACE to reflect the writes they perform
// (RepairSupersededReviewLineage AddEdge / ReconcileCollectionRevisions
// SATISFIED) so downstream cores observe the identical POST-write row set the
// per-query path would have read. flipped records the revision ids a reconcile
// just satisfied so the display path reflects those flips.
type collectionSnapshot struct {
	collection     Row
	tasks          Rows
	edges          Rows
	allRevisions   Rows
	openRevisions  Rows
	acceptance     Row
	pr             Row
	runnable       bool
	runningSession bool
	baselineEvents Rows
	flipped        []string
}

// ---------------------------------------------------------------------------
// Pure decision cores
// ---------------------------------------------------------------------------

// implementationsMissingPassingReviewFromRows is the decide body of
// Store.ImplementationsMissingPassingReview over already-loaded task/edge rows.
// The cid wrapper loads then calls it; the batched Status path calls it directly.
func implementationsMissingPassingReviewFromRows(tasks, edges Rows) []string {
	by := map[string]Row{}
	for _, t := range tasks {
		by[S(t, "id")] = t
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
	return missing
}

// repairSupersededReviewLineageFromRows is the decide body of
// Store.RepairSupersededReviewLineage. It performs the same AddEdge writes and
// the same REVIEW_LINEAGE_REPAIRED event, and appends each newly created edge to
// *edges so a caller holding an in-memory snapshot observes the post-repair edge
// set without re-reading task_edges. When *edges is a throwaway (the cid wrapper)
// the append is harmless; downstream per-query reads in that path stay fresh.
func (s *Store) repairSupersededReviewLineageFromRows(cid string, tasks Rows, edges *Rows) (int, error) {
	by := map[string]Row{}
	for _, t := range tasks {
		by[S(t, "id")] = t
	}
	keys := map[string]bool{}
	sup := Rows{}
	for _, e := range *edges {
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
				*edges = append(*edges, Row{"from_task_id": S(x, "from"), "to_task_id": S(rep, "id"), "relation": rel})
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

// reconcileRevisionsFromRows is the decide body of
// Store.ReconcileCollectionRevisions over already-loaded OPEN revisions and task
// /edge rows. The SATISFIED UPDATE and COLLECTION_REVISION_SATISFIED event are
// unchanged; satisfied revisions are removed from *openRevisions and their ids
// returned in flipped so a snapshot caller sees the post-reconcile OPEN set and
// can reflect the flip in its displayed (all) revisions. The cid wrapper ignores
// both side effects and re-reads OPEN fresh in the unchanged wrapper path.
func (s *Store) reconcileRevisionsFromRows(cid string, openRevisions *Rows, tasks, edges Rows) (int, []string, error) {
	revs := *openRevisions
	if len(revs) == 0 {
		return 0, nil, nil
	}
	by := map[string]Row{}
	for _, t := range tasks {
		by[S(t, "id")] = t
	}
	ver := map[string][]string{}
	for _, e := range edges {
		if S(e, "relation") == "verifies" {
			ver[S(e, "from_task_id")] = append(ver[S(e, "from_task_id")], S(e, "to_task_id"))
		}
	}
	term := map[string]bool{TaskComplete: true, TaskCancelled: true, TaskSuperseded: true}
	sat := 0
	var flipped []string
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
			return sat, flipped, er
		}
		c, _ := s.GetCollection(cid)
		ids := []string{}
		for _, t := range completed {
			ids = append(ids, S(t, "id"))
		}
		_ = s.Event(S(c, "project_id"), cid, "", "", "COLLECTION_REVISION_SATISFIED", Row{"revision_id": S(rev, "id"), "implementation_task_ids": ids, "review_task_ids": rids})
		sat++
		flipped = append(flipped, S(rev, "id"))
		// Reflect the SATISFIED flip in the in-memory OPEN bucket so the terminal
		// core sees the same post-reconcile OPEN set the per-query path would read.
		kept := (*openRevisions)[:0]
		for _, r := range *openRevisions {
			if S(r, "id") != S(rev, "id") {
				kept = append(kept, r)
			}
		}
		*openRevisions = kept
	}
	return sat, flipped, nil
}

// criticRevalidationFromRows is the decide body of Store.CriticRevalidationState
// over already-loaded collection/task/edge rows and the collection's
// CRITIC_PLAN_BASELINE events. It reuses the existing pure
// MaterialPlanFingerprint / MaterialPlanFingerprintV021 functions and re-derives
// the LATEST non-cancelled/non-superseded critic task (in created_at order) to
// select its sealed baseline — keyed by critic TASK id, not by a per-collection
// "latest baseline" (planner_directive:4).
func criticRevalidationFromRows(collection Row, tasks, edges Rows, baselineEvents Rows) Row {
	var latest Row
	for _, t := range tasks {
		if S(t, "profile") == "critic" && S(t, "status") != TaskCancelled && S(t, "status") != TaskSuperseded {
			latest = t
		}
	}
	obj := S(collection, "objective")
	cur := MaterialPlanFingerprint(obj, tasks, edges, false)
	legacy := MaterialPlanFingerprint(obj, tasks, edges, true)
	v021 := MaterialPlanFingerprintV021(obj, tasks, edges, false)
	v021Legacy := MaterialPlanFingerprintV021(obj, tasks, edges, true)
	var base Row
	if latest != nil {
		// baselineEvents arrive in created_at (ascending) order, so the last match
		// is the latest event for that critic task (== ORDER BY created_at DESC
		// LIMIT 1 in CriticPlanBaseline).
		for _, e := range baselineEvents {
			if S(e, "task_id") == S(latest, "id") {
				p := JSONMap(S(e, "payload_json"))
				if _, ok := p["created_at"]; !ok {
					p["created_at"] = S(e, "created_at")
				}
				base = p
			}
		}
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
	return Row{"latest_critic_task_id": S(latest, "id"), "latest_critic_status": S(latest, "status"), "current_material_plan_fingerprint": cur, "sealed_fingerprint": sealed, "baseline_source": S(base, "source"), "compatible_via": nullable(compatible), "stale": latest != nil && sealed != "" && sealed != cur && compatible == ""}
}

// collectionTerminalFlags computes the three aggregate task-status flags the
// terminal decide block consumes. It is a single named source so the core and
// the wrapper's read-optimization gate (which decides whether a session/critic
// probe is even needed) cannot drift from the branch condition the core uses.
func collectionTerminalFlags(tasks Rows) (allTerm, hasComplete, hasExec bool) {
	successful := map[string]bool{TaskComplete: true, TaskCancelled: true, TaskSuperseded: true}
	allTerm = true
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
	return
}

// terminalStateForCollection is the decide block of
// Store.UpdateCollectionTerminalState over already-loaded rows. It performs no
// writes and no reads; runnable/runningSession are the batched existence
// probes and criticRow is the output of criticRevalidationFromRows. The empty
// collection short-circuit matches the wrapper's `if len(tasks) == 0` return.
func terminalStateForCollection(collection Row, tasks, edges Rows, openRevisions Rows, runnable, runningSession bool, criticRow Row) string {
	if len(tasks) == 0 {
		return S(collection, "status")
	}
	allTerm, hasComplete, hasExec := collectionTerminalFlags(tasks)
	if !hasExec && !runnable && runningSession {
		hasExec = true
	}
	desired := "BLOCKED"
	if len(openRevisions) > 0 {
		if hasExec || runnable {
			desired = "RUNNING"
		}
	} else if allTerm && hasComplete {
		dp := ClassifyDeliveryPolicy(S(collection, "objective"))
		completedImpl := 0
		for _, t := range tasks {
			if S(t, "profile") == "implement" && S(t, "status") == TaskComplete {
				completedImpl++
			}
		}
		missingDelivery := dp.ImplementationRequired && completedImpl == 0
		mr := implementationsMissingPassingReviewFromRows(tasks, edges)
		pp := effectivePlanningPolicy(collection)
		stale := pp.CriticRequired && B(criticRow, "stale")
		if !missingDelivery && len(mr) == 0 && !stale {
			desired = "COMPLETE"
		}
	} else if hasExec || runnable {
		desired = "RUNNING"
	}
	return desired
}

// ---------------------------------------------------------------------------
// Batched snapshot loader
// ---------------------------------------------------------------------------

// loadCollectionRows loads every row set the status-path cores and display need
// for a bounded set of collections in ONE `dolt sql -r json -q` multi-SELECT via
// queryManyLocked, then buckets them back per collection in Go. Each statement
// reuses its standalone loader's deterministic ORDER BY so the cores observe the
// identical row sequence the per-query path would:
//   - tasks / edges: ORDER BY collection_id,created_at — for a fixed collection
//     this is exactly ListTasks' / ListEdges' `ORDER BY created_at`; the
//     fingerprint's created_at,id order is deliberately NOT used here (the two
//     loaders stay separate — planner_directive:3, research C-4).
//   - revisions / latest acceptance / latest PR: their own loaders' orders.
//   - runnable / running-session: per-collection existence booleans grouped by
//     collection_id with WHERE predicates identical to RunnableTasks /
//     HasRunningSession (planner_directive:5).
//   - CRITIC_PLAN_BASELINE events: keyed by collection_id; the core re-derives
//     the latest critic task id and selects its baseline (planner_directive:4).
//
// It must be called while the caller holds no store lock (it uses QueryMany).
func (s *Store) loadCollectionRows(ids []string) (map[string]*collectionSnapshot, error) {
	snap := map[string]*collectionSnapshot{}
	if len(ids) == 0 {
		return snap, nil
	}
	for _, id := range ids {
		snap[id] = &collectionSnapshot{}
	}
	in := sqlIDList(ids)
	runningWhere := "s.status='RUNNING'"
	tasksQ := "SELECT * FROM tasks WHERE collection_id IN " + in + " ORDER BY collection_id,created_at"
	edgesQ := "SELECT * FROM task_edges WHERE collection_id IN " + in + " ORDER BY collection_id,created_at"
	allRevQ := "SELECT * FROM collection_revisions WHERE collection_id IN " + in + " ORDER BY collection_id,created_at"
	openRevQ := "SELECT * FROM collection_revisions WHERE status='OPEN' AND collection_id IN " + in + " ORDER BY collection_id,created_at"
	acceptQ := "SELECT * FROM collection_acceptances WHERE collection_id IN " + in + " ORDER BY collection_id,created_at DESC"
	prQ := "SELECT * FROM collection_pull_requests WHERE provider='github' AND collection_id IN " + in + " ORDER BY collection_id,created_at DESC"
	runnableQ := "SELECT DISTINCT t.collection_id FROM tasks t WHERE t.status IN ('READY','PENDING') AND NOT EXISTS (SELECT 1 FROM task_edges e JOIN tasks dep ON dep.id=e.from_task_id WHERE e.to_task_id=t.id AND e.relation='depends_on' AND dep.status!='COMPLETE') AND NOT EXISTS (SELECT 1 FROM task_edges eb JOIN attempts pa ON pa.task_id=eb.to_task_id JOIN sessions ps ON ps.attempt_id=pa.id WHERE eb.from_task_id=t.id AND eb.to_task_id=t.parent_task_id AND eb.relation='depends_on' AND ps.status='RUNNING')"
	sessionQ := "SELECT DISTINCT t.collection_id FROM sessions s JOIN attempts a ON a.id=s.attempt_id JOIN tasks t ON t.id=a.task_id WHERE " + runningWhere
	baselineQ := "SELECT collection_id,task_id,payload_json,created_at FROM events WHERE event_type='CRITIC_PLAN_BASELINE' AND collection_id IN " + in + " ORDER BY collection_id,created_at"

	out, err := s.QueryMany(
		"SELECT * FROM collections WHERE id IN "+in,
		tasksQ,
		edgesQ,
		allRevQ,
		openRevQ,
		acceptQ,
		prQ,
		runnableQ,
		sessionQ,
		baselineQ,
	)
	if err != nil {
		return nil, err
	}
	// collections (statement 0), keyed by id.
	for _, r := range out[0] {
		if sn, ok := snap[S(r, "id")]; ok {
			sn.collection = r
		}
	}
	// tasks (statement 1): parse result_json into result exactly as ListTasks does.
	for _, r := range out[1] {
		r["result"] = JSONMap(S(r, "result_json"))
		if sn, ok := snap[S(r, "collection_id")]; ok {
			sn.tasks = append(sn.tasks, r)
		}
	}
	for _, r := range out[2] { // edges
		if sn, ok := snap[S(r, "collection_id")]; ok {
			sn.edges = append(sn.edges, r)
		}
	}
	for _, r := range out[3] { // all revisions
		if sn, ok := snap[S(r, "collection_id")]; ok {
			sn.allRevisions = append(sn.allRevisions, r)
		}
	}
	for _, r := range out[4] { // OPEN revisions
		if sn, ok := snap[S(r, "collection_id")]; ok {
			sn.openRevisions = append(sn.openRevisions, r)
		}
	}
	for _, r := range out[5] { // latest acceptance: first per collection under DESC
		if sn, ok := snap[S(r, "collection_id")]; ok && sn.acceptance == nil {
			sn.acceptance = r
		}
	}
	for _, r := range out[6] { // latest PR: first per collection under DESC
		if sn, ok := snap[S(r, "collection_id")]; ok && sn.pr == nil {
			sn.pr = r
		}
	}
	for _, r := range out[7] { // runnable existence
		if sn, ok := snap[S(r, "collection_id")]; ok {
			sn.runnable = true
		}
	}
	for _, r := range out[8] { // running-session existence
		if sn, ok := snap[S(r, "collection_id")]; ok {
			sn.runningSession = true
		}
	}
	for _, r := range out[9] { // critic baseline events
		if sn, ok := snap[S(r, "collection_id")]; ok {
			sn.baselineEvents = append(sn.baselineEvents, r)
		}
	}
	return snap, nil
}

// sqlIDList renders an internal id slice as a parenthesized, SQLLiteral-quoted IN
// list. Ids are Praector-issued (COL-*/T-*/...) and embedded via SQLLiteral plus
// fixed relation literals only, so no statement carries a `;` inside a string
// literal (research C-1/C-3).
func sqlIDList(ids []string) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = SQLLiteral(id)
	}
	return "(" + strings.Join(parts, ",") + ")"
}

// reconcileCollectionSnapshot applies the status-side writes to one snapshot in
// the exact order the reference UpdateCollectionTerminalState performs them
// (superseded-review-lineage repair, revision reconcile, terminal decide +
// SetCollectionStatus) and refreshes the affected in-memory buckets afterward so
// the terminal core observes the post-write row set. It emits exactly the same
// events and performs exactly the same writes as the per-query path.
func (s *Store) reconcileCollectionSnapshot(sn *collectionSnapshot) {
	cid := S(sn.collection, "id")
	_, _ = s.repairSupersededReviewLineageFromRows(cid, sn.tasks, &sn.edges)
	_, flipped, _ := s.reconcileRevisionsFromRows(cid, &sn.openRevisions, sn.tasks, sn.edges)
	for _, fid := range flipped {
		for _, r := range sn.allRevisions {
			if S(r, "id") == fid {
				r["status"] = "SATISFIED"
			}
		}
	}
	sn.flipped = flipped
	var criticRow Row
	allTerm, hasComplete, _ := collectionTerminalFlags(sn.tasks)
	if len(sn.tasks) > 0 && len(sn.openRevisions) == 0 && allTerm && hasComplete {
		criticRow = criticRevalidationFromRows(sn.collection, sn.tasks, sn.edges, sn.baselineEvents)
	}
	desired := terminalStateForCollection(sn.collection, sn.tasks, sn.edges, sn.openRevisions, sn.runnable, sn.runningSession, criticRow)
	if S(sn.collection, "status") != desired {
		_ = s.SetCollectionStatus(cid, desired, desired == "COMPLETE" || desired == "BLOCKED")
		sn.collection["status"] = desired
	}
}
