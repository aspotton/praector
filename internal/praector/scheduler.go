package praector

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Scheduler struct {
	Store               *Store
	Cfg                 GlobalConfig
	Workspace           *GitWorkspace
	MaxWorkersOverride  int
	quarantined         map[string]bool
	reportedQuarantines map[string]bool
}

func NewScheduler(store *Store, cfg GlobalConfig, maxWorkers int) (*Scheduler, error) {
	workspace, err := NewGitWorkspace(store, cfg)
	if err != nil {
		return nil, err
	}
	return &Scheduler{Store: store, Cfg: cfg, Workspace: workspace, MaxWorkersOverride: maxWorkers, quarantined: map[string]bool{}, reportedQuarantines: map[string]bool{}}, nil
}

func (s *Scheduler) ExclusiveScheduler() (*FileLock, error) {
	if err := os.MkdirAll(filepath.Dir(s.Cfg.SchedulerLock()), 0755); err != nil {
		return nil, err
	}
	lock, err := AcquireFileLock(s.Cfg.SchedulerLock(), true)
	if err != nil {
		return nil, fmt.Errorf("Another praector scheduler/daemon is already running")
	}
	return lock, nil
}

func (s *Scheduler) RecoverStaleSessions() (int, error) {
	rows, err := s.Store.RunningSessions()
	if err != nil {
		return 0, err
	}
	recovered := 0
	for _, row := range rows {
		if pid := I(row, "pid"); pid > 0 {
			terminateStalePID(pid)
		}
		if err := s.Store.AbortStaleSession(S(row, "id"), S(row, "attempt_id"), S(row, "task_id")); err != nil {
			return recovered, err
		}
		recovered++
	}
	return recovered, nil
}

func (s *Scheduler) limits(project Row) Limits { return ProjectConfigFromRow(project).Limits }

func reverseProfile(tasks Rows, profile string) Row {
	for i := len(tasks) - 1; i >= 0; i-- {
		if S(tasks[i], "profile") == profile {
			return tasks[i]
		}
	}
	return nil
}

func activeTasks(tasks Rows) Rows {
	out := Rows{}
	for _, task := range tasks {
		if S(task, "status") != TaskCancelled && S(task, "status") != TaskSuperseded {
			out = append(out, task)
		}
	}
	return out
}

func (s *Scheduler) materialFingerprint(collection Row, tasks Rows, legacy bool) (string, error) {
	edges, err := s.Store.ListEdges(S(collection, "id"))
	if err != nil {
		return "", err
	}
	return MaterialPlanFingerprint(S(collection, "objective"), tasks, edges, legacy), nil
}

func (s *Scheduler) ensureMaterialCriticRevalidation(collection Row, active Rows, policy PlanningPolicy, critic, planReview Row) (Row, bool, error) {
	if !policy.CriticRequired || critic == nil || S(critic, "status") != TaskComplete {
		return critic, false, nil
	}
	baseline, err := s.Store.CriticPlanBaseline(S(critic, "id"))
	if err != nil {
		return critic, false, err
	}
	if baseline == nil || S(baseline, "fingerprint") == "" {
		return critic, false, nil
	}
	current, err := s.materialFingerprint(collection, active, false)
	if err != nil {
		return critic, false, err
	}
	legacy, err := s.materialFingerprint(collection, active, true)
	if err != nil {
		return critic, false, err
	}
	if S(baseline, "fingerprint") == current || S(baseline, "fingerprint") == legacy {
		return critic, false, nil
	}
	for _, task := range active {
		st := S(task, "status")
		if S(task, "profile") == "coordinator" && (st == TaskPending || st == TaskReady || st == TaskRunning || st == TaskVerifying) {
			return critic, false, nil
		}
	}
	parent := reverseProfile(active, "coordinator")
	depth := 1
	parentID := ""
	if parent != nil {
		depth = I(parent, "depth") + 1
		parentID = S(parent, "id")
	}
	fresh, created, err := s.Store.CreateTask(CreateTaskOptions{
		ProjectID: S(collection, "project_id"), CollectionID: S(collection, "id"), Profile: "critic", Title: "Revalidate materially changed plan",
		Objective: "Re-run the independent planning preflight because the durable material plan changed after the previous critic was sealed. Inspect the CURRENT implementation/research/advice contracts and dependencies. Identify any new hidden requirements, security/state/concurrency hazards, scope changes, assumptions, and executable QA changes introduced by the new plan. Return concrete MUST/MUST NOT/VERIFY guidance. Do not implement or mutate the graph.",
		Reason:    fmt.Sprintf("Material plan fingerprint changed after critic %s: %s -> %s", S(critic, "id"), S(baseline, "fingerprint"), current), ParentTaskID: parentID,
		WorkspaceMode: "collection", Depth: depth, AllowDedupe: false,
	})
	if err != nil {
		return critic, false, err
	}
	if !created {
		return fresh, false, nil
	}
	if err := s.Store.RecordCriticPlanBaseline(S(fresh, "id"), current, "material_revalidation", ""); err != nil {
		return nil, false, err
	}
	if parent != nil {
		_ = s.Store.AddEdge(S(parent, "id"), S(fresh, "id"), S(collection, "id"), "spawned_by")
	}
	repair, _, err := s.Store.CreateTask(CreateTaskOptions{
		ProjectID: S(collection, "project_id"), CollectionID: S(collection, "id"), Profile: "coordinator", Title: "Apply critic revalidation",
		Objective: fmt.Sprintf("Consume critic %s after material plan change. Re-evaluate the current graph using its findings. If the findings require material changes, make them; Praector will require another critic round. If no material change is required, preserve the material contracts. Ensure there is exactly one final blocking plan-review gate; if an existing nonterminal plan_review already gates the graph, do not create a duplicate.", S(fresh, "id")),
		Reason:    fmt.Sprintf("Fresh critic %s must be incorporated before final plan review.", S(fresh, "id")), ParentTaskID: S(fresh, "id"), DependsOn: []string{S(fresh, "id")}, WorkspaceMode: "collection", Depth: I(fresh, "depth") + 1, AllowDedupe: false,
	})
	if err != nil {
		return nil, false, err
	}
	_ = s.Store.AddEdge(S(fresh, "id"), S(repair, "id"), S(collection, "id"), "spawned_by")
	if planReview != nil && S(planReview, "status") != TaskComplete && S(planReview, "status") != TaskCancelled && S(planReview, "status") != TaskSuperseded {
		if err := s.Store.AddDependency(S(planReview, "id"), S(fresh, "id")); err != nil {
			return nil, false, err
		}
		if err := s.Store.AddDependency(S(planReview, "id"), S(repair, "id")); err != nil {
			return nil, false, err
		}
	}
	_ = s.Store.Event(S(collection, "project_id"), S(collection, "id"), S(fresh, "id"), "", "CRITIC_REVALIDATION_REQUIRED", Row{
		"stale_critic_task_id": S(critic, "id"), "fresh_critic_task_id": S(fresh, "id"), "coordinator_task_id": S(repair, "id"), "previous_fingerprint": S(baseline, "fingerprint"), "current_fingerprint": current, "plan_review_task_id": func() any {
			if planReview != nil {
				return S(planReview, "id")
			}
			return nil
		}(),
	})
	return fresh, true, nil
}

func (s *Scheduler) enforceDeliveryPolicy(collectionID string) (bool, error) {
	collection, err := s.Store.GetCollection(collectionID)
	if err != nil {
		return false, err
	}
	policy := ClassifyDeliveryPolicy(S(collection, "objective"))
	if !policy.ImplementationRequired {
		return false, nil
	}
	tasks, err := s.Store.ListTasks(collectionID)
	if err != nil {
		return false, err
	}
	active := activeTasks(tasks)
	for _, t := range active {
		if S(t, "profile") == "implement" {
			return false, nil
		}
	}
	for _, t := range active {
		p, st := S(t, "profile"), S(t, "status")
		planning := p == "coordinator" || p == "explore" || p == "critic" || p == "plan_review" || p == "research" || p == "advise"
		if planning && (st == TaskPending || st == TaskReady || st == TaskRunning || st == TaskWaiting || st == TaskVerifying) {
			return false, nil
		}
	}
	for _, t := range tasks {
		if S(t, "profile") == "coordinator" && strings.HasPrefix(S(t, "reason"), "Deterministic delivery policy") {
			return false, nil
		}
	}
	latestCoordinator := reverseProfile(active, "coordinator")
	var latestCritic Row
	for i := len(active) - 1; i >= 0; i-- {
		if S(active[i], "profile") == "critic" && S(active[i], "status") == TaskComplete {
			latestCritic = active[i]
			break
		}
	}
	deps := []string{}
	if latestCritic != nil {
		deps = append(deps, S(latestCritic, "id"))
	}
	if latestCoordinator != nil && S(latestCoordinator, "status") == TaskComplete {
		deps = append(deps, S(latestCoordinator, "id"))
	}
	depth := 1
	parentID := ""
	if latestCoordinator != nil {
		depth = I(latestCoordinator, "depth") + 1
		parentID = S(latestCoordinator, "id")
	}
	repair, created, err := s.Store.CreateTask(CreateTaskOptions{ProjectID: S(collection, "project_id"), CollectionID: collectionID, Profile: "coordinator", Title: "Delivery policy repair", Objective: "The Collection objective is a direct change request, but the durable graph contains no implementation task. Use completed critic/coordinator evidence to create at least one decision-complete implement task and any required pre-write gates. Do not declare the Collection delivered until durable implementation work exists and can receive independent review.", Reason: "Deterministic delivery policy: implementation-required objective has no implementation task", ParentTaskID: parentID, DependsOn: deps, WorkspaceMode: "collection", Depth: depth, AllowDedupe: false})
	if err != nil {
		return false, err
	}
	if created && latestCoordinator != nil {
		_ = s.Store.AddEdge(S(latestCoordinator, "id"), S(repair, "id"), collectionID, "spawned_by")
	}
	_ = s.Store.Event(S(collection, "project_id"), collectionID, S(repair, "id"), "", "DELIVERY_POLICY_ENFORCED", Row{"implementation_required": true, "categories": policy.Categories, "repair_coordinator_task_id": S(repair, "id")})
	if S(collection, "status") == "BLOCKED" {
		_ = s.Store.SetCollectionStatus(collectionID, "RUNNING", false)
	}
	return true, nil
}

func containsTaskID(rows Rows, id string) bool {
	for _, r := range rows {
		if S(r, "id") == id {
			return true
		}
	}
	return false
}
func depsIDSet(rows Rows) map[string]bool {
	m := map[string]bool{}
	for _, r := range rows {
		m[S(r, "id")] = true
	}
	return m
}

func (s *Scheduler) enforcePlanningPolicy(collectionID string) (bool, error) {
	collection, err := s.Store.GetCollection(collectionID)
	if err != nil {
		return false, err
	}
	policy := ClassifyPlanningPolicy(S(collection, "objective"))
	if !policy.ExploreRequired && !policy.CriticRequired && !policy.PlanReviewRequired {
		return false, nil
	}
	tasks, err := s.Store.ListTasks(collectionID)
	if err != nil {
		return false, err
	}
	active := activeTasks(tasks)
	changed := false
	reasonText := strings.Join(policy.Reasons, "; ")
	if reasonText == "" {
		reasonText = "Deterministic planning policy requires pre-write gates."
	}

	// Implementation Collections begin with deterministic discovery and critique
	// before the coordinator is allowed to shape the material plan. This makes
	// current-system understanding a durable input instead of incidental planner
	// behavior. Reconciliation also applies to revisions, so the preflight is
	// refreshed when a completed Collection is reopened for additional work.
	var latestCoordinator Row
	for i := len(active) - 1; i >= 0; i-- {
		st := S(active[i], "status")
		if S(active[i], "profile") == "coordinator" && (S(active[i], "title") == "Initial coordination" || S(active[i], "title") == "Acceptance revision") && (st == TaskPending || st == TaskReady || st == TaskWaiting) {
			latestCoordinator = active[i]
			break
		}
	}
	latestForCoordinator := func(profile string) Row {
		for i := len(active) - 1; i >= 0; i-- {
			if S(active[i], "profile") != profile {
				continue
			}
			if latestCoordinator == nil || S(active[i], "requested_by_task_id") == S(latestCoordinator, "id") {
				return active[i]
			}
		}
		return nil
	}
	latestExplore := latestForCoordinator("explore")
	if policy.ExploreRequired && latestCoordinator != nil && latestExplore == nil {
		pid := ""
		depth := 1
		blocking := ""
		if latestCoordinator != nil {
			pid = S(latestCoordinator, "id")
			depth = I(latestCoordinator, "depth") + 1
			st := S(latestCoordinator, "status")
			if st == TaskPending || st == TaskReady || st == TaskWaiting {
				blocking = pid
			}
		}
		_, _, err = s.Store.CreateTask(CreateTaskOptions{
			ProjectID: S(collection, "project_id"), CollectionID: collectionID, Profile: "explore",
			Title:        "Explore current implementation surface",
			Objective:    "Before planning, inspect the existing repository/system surface relevant to the Collection objective. Map relevant files/modules, entry points, callers/dependencies, shared state, configuration/runtime boundaries, tests, documentation, similar existing patterns, and compatibility/concurrency/platform constraints. Return a concise evidence-grounded current-state map for the critic and coordinator. Do not design the solution or implement changes; for a greenfield repository, say so explicitly and stop early.",
			Reason:       "Deterministic planning policy: repository exploration is required before implementation planning.",
			ParentTaskID: pid, RequestedByTaskID: pid, WorkspaceMode: "collection", Depth: depth, AllowDedupe: false,
			SpawnedByTaskID: pid, BlockingCallerTaskID: blocking,
		})
		if err != nil {
			return false, err
		}
		changed = true
		tasks, _ = s.Store.ListTasks(collectionID)
		active = activeTasks(tasks)
		latestCoordinator = nil
		for i := len(active) - 1; i >= 0; i-- {
			st := S(active[i], "status")
			if S(active[i], "profile") == "coordinator" && (S(active[i], "title") == "Initial coordination" || S(active[i], "title") == "Acceptance revision") && (st == TaskPending || st == TaskReady || st == TaskWaiting) {
				latestCoordinator = active[i]
				break
			}
		}
		latestExplore = latestForCoordinator("explore")
	}

	latestCritic := latestForCoordinator("critic")
	if policy.CriticRequired && latestCoordinator != nil && latestCritic == nil {
		pid := ""
		depth := 1
		blocking := ""
		if latestCoordinator != nil {
			pid = S(latestCoordinator, "id")
			depth = I(latestCoordinator, "depth") + 1
			st := S(latestCoordinator, "status")
			if st == TaskPending || st == TaskReady || st == TaskWaiting {
				blocking = pid
			}
		}
		deps := []string{}
		if latestExplore != nil {
			deps = append(deps, S(latestExplore, "id"))
		}
		_, _, err = s.Store.CreateTask(CreateTaskOptions{
			ProjectID: S(collection, "project_id"), CollectionID: collectionID, Profile: "critic",
			Title: "Preflight implementation plan",
			Objective: fmt.Sprintf("Independently preflight the Collection objective before material planning. Consume the completed repository exploration as primary evidence when present. Deterministic policy categories: %s. Identify hidden requirements, scope hazards, security/state/concurrency semantics, assumptions, executable QA directives, and any acceptance contradiction. Return concrete MUST/MUST NOT/VERIFY guidance for the coordinator; do not implement or mutate the graph.", func() string {
				if len(policy.Categories) > 0 {
					return strings.Join(policy.Categories, ", ")
				}
				return "implementation"
			}()),
			Reason:       "Deterministic planning policy: every implementation Collection receives an independent critic.",
			ParentTaskID: pid, RequestedByTaskID: pid, DependsOn: deps, WorkspaceMode: "collection", Depth: depth, AllowDedupe: false,
			SpawnedByTaskID: pid, BlockingCallerTaskID: blocking,
		})
		if err != nil {
			return false, err
		}
		changed = true
		tasks, _ = s.Store.ListTasks(collectionID)
		active = activeTasks(tasks)
	}

	writeTasks := Rows{}
	for _, t := range active {
		if S(t, "profile") == "implement" && (S(t, "status") == TaskPending || S(t, "status") == TaskReady) {
			writeTasks = append(writeTasks, t)
		}
	}
	if len(writeTasks) == 0 {
		return changed, nil
	}
	planReviews := Rows{}
	for _, t := range active {
		if S(t, "profile") == "plan_review" {
			planReviews = append(planReviews, t)
		}
	}
	writeIDs := map[string]bool{}
	for _, t := range writeTasks {
		writeIDs[S(t, "id")] = true
	}
	// Remove coordinator->review edges that participate in a transitive cycle.
	type cycleRepair struct {
		review, coord Row
		path          []string
	}
	repairs := []cycleRepair{}
	for _, review := range planReviews {
		deps, _ := s.Store.Dependencies(S(review, "id"))
		for _, dep := range deps {
			if S(dep, "profile") != "coordinator" {
				continue
			}
			p, _ := s.Store.DependencyPath(S(review, "id"), S(dep, "id"))
			if p != nil {
				repairs = append(repairs, cycleRepair{review, dep, p})
			}
		}
	}
	for _, r := range repairs {
		if err := s.Store.RemoveDependency(S(r.review, "id"), S(r.coord, "id")); err != nil {
			return false, err
		}
		_ = s.Store.Event(S(collection, "project_id"), collectionID, S(r.review, "id"), "", "PLAN_REVIEW_COORDINATOR_CYCLE_REPAIRED", Row{"plan_review_task_id": S(r.review, "id"), "coordinator_task_id": S(r.coord, "id"), "removed_edge": S(r.coord, "id") + "->" + S(r.review, "id"), "existing_return_path": r.path})
		changed = true
	}
	unstarted := Rows{}
	for _, t := range planReviews {
		st := S(t, "status")
		if st == TaskPending || st == TaskReady || st == TaskWaiting {
			unstarted = append(unstarted, t)
		}
	}
	if len(unstarted) > 1 {
		survivor := unstarted[len(unstarted)-1]
		for _, obsolete := range unstarted[:len(unstarted)-1] {
			if err := s.Store.SupersedeTask(S(obsolete, "id"), S(survivor, "id"), "Collapse redundant unstarted final plan-review gates after graph repair."); err != nil {
				return false, err
			}
			_ = s.Store.Event(S(collection, "project_id"), collectionID, S(obsolete, "id"), "", "REDUNDANT_PLAN_REVIEW_COLLAPSED", Row{"obsolete_plan_review_task_id": S(obsolete, "id"), "surviving_plan_review_task_id": S(survivor, "id")})
			changed = true
		}
		tasks, _ = s.Store.ListTasks(collectionID)
		active = activeTasks(tasks)
		writeTasks = Rows{}
		writeIDs = map[string]bool{}
		planReviews = Rows{}
		for _, t := range active {
			if S(t, "profile") == "implement" && (S(t, "status") == TaskPending || S(t, "status") == TaskReady) {
				writeTasks = append(writeTasks, t)
				writeIDs[S(t, "id")] = true
			}
			if S(t, "profile") == "plan_review" {
				planReviews = append(planReviews, t)
			}
		}
	}
	for _, review := range planReviews {
		deps, _ := s.Store.Dependencies(S(review, "id"))
		reviewIDs := depsIDSet(deps)
		for implID := range writeIDs {
			if !reviewIDs[implID] {
				continue
			}
			ideps, _ := s.Store.Dependencies(implID)
			if !depsIDSet(ideps)[S(review, "id")] {
				continue
			}
			if err := s.Store.RemoveDependency(S(review, "id"), implID); err != nil {
				return false, err
			}
			_ = s.Store.Event(S(collection, "project_id"), collectionID, S(review, "id"), "", "PLAN_REVIEW_DEPENDENCY_CYCLE_REPAIRED", Row{"plan_review_task_id": S(review, "id"), "implement_task_id": implID, "removed_edge": implID + "->" + S(review, "id"), "preserved_gate": S(review, "id") + "->" + implID})
			changed = true
		}
	}
	critics := Rows{}
	for _, t := range active {
		if S(t, "profile") == "critic" {
			critics = append(critics, t)
		}
	}
	var critic Row
	if len(critics) > 0 {
		critic = critics[len(critics)-1]
	}
	if policy.CriticRequired && critic == nil {
		parent := reverseProfile(active, "coordinator")
		depth := 1
		pid := ""
		if parent != nil {
			depth = I(parent, "depth") + 1
			pid = S(parent, "id")
		}
		critic, _, err = s.Store.CreateTask(CreateTaskOptions{ProjectID: S(collection, "project_id"), CollectionID: collectionID, Profile: "critic", Title: "Policy preflight critic", Objective: fmt.Sprintf("Independently preflight the Collection objective and current work graph before write work begins. Deterministic policy categories: %s. Identify hidden requirements, security/state semantics, scope hazards, assumptions, executable QA directives, and any task contract that knowingly violates a Collection MUST/MUST NOT or acceptance requirement. Treat known acceptance violations as planning blockers; do not accept a plan merely because a later review could catch or repair them. Return concrete MUST/MUST NOT/VERIFY guidance for the planner and plan reviewer; do not implement or mutate the graph.", func() string {
			if len(policy.Categories) > 0 {
				return strings.Join(policy.Categories, ", ")
			}
			return "risk"
		}()), Reason: "Deterministic planning policy: " + reasonText, ParentTaskID: pid, WorkspaceMode: "collection", Depth: depth, AllowDedupe: false})
		if err != nil {
			return false, err
		}
		if parent != nil {
			_ = s.Store.AddEdge(S(parent, "id"), S(critic, "id"), collectionID, "spawned_by")
		}
		changed = true
	}
	planReviews = Rows{}
	for _, t := range active {
		if S(t, "profile") == "plan_review" {
			planReviews = append(planReviews, t)
		}
	}
	var planReview Row
	if len(planReviews) > 0 {
		planReview = planReviews[len(planReviews)-1]
	}
	if policy.CriticRequired && critic != nil && S(critic, "status") == TaskComplete && planReview != nil {
		base, e := s.Store.CriticPlanBaseline(S(critic, "id"))
		if e != nil {
			return false, e
		}
		if base == nil {
			fp, e := s.materialFingerprint(collection, active, false)
			if e != nil {
				return false, e
			}
			if e = s.Store.RecordCriticPlanBaseline(S(critic, "id"), fp, "compatibility_plan_seal", S(planReview, "id")); e != nil {
				return false, e
			}
			changed = true
		}
	}
	critic, reval, err := s.ensureMaterialCriticRevalidation(collection, active, policy, critic, planReview)
	if err != nil {
		return false, err
	}
	if reval {
		changed = true
		tasks, _ = s.Store.ListTasks(collectionID)
		active = activeTasks(tasks)
		critics = Rows{}
		planReviews = Rows{}
		writeTasks = Rows{}
		for _, t := range active {
			if S(t, "profile") == "critic" {
				critics = append(critics, t)
			}
			if S(t, "profile") == "plan_review" {
				planReviews = append(planReviews, t)
			}
			if S(t, "profile") == "implement" && (S(t, "status") == TaskPending || S(t, "status") == TaskReady) {
				writeTasks = append(writeTasks, t)
			}
		}
		if len(critics) > 0 {
			critic = critics[len(critics)-1]
		}
		if len(planReviews) > 0 {
			planReview = planReviews[len(planReviews)-1]
		}
	}
	if planReview != nil && S(planReview, "status") == TaskComplete {
		reviewed := S(planReview, "updated_at")
		newestWrite := ""
		for _, t := range writeTasks {
			if S(t, "created_at") > newestWrite {
				newestWrite = S(t, "created_at")
			}
		}
		newestCoord := ""
		for _, t := range active {
			st := S(t, "status")
			if S(t, "profile") == "coordinator" && st != TaskComplete && st != TaskCancelled && st != TaskSuperseded && S(t, "updated_at") > newestCoord {
				newestCoord = S(t, "updated_at")
			}
		}
		criticNotIncluded := policy.CriticRequired && critic != nil && (S(critic, "status") != TaskComplete || S(critic, "updated_at") > reviewed)
		if reviewed < newestWrite || reviewed < newestCoord || criticNotIncluded {
			planReview = nil
		}
	}
	if planReview != nil {
		deps, _ := s.Store.Dependencies(S(planReview, "id"))
		ids := depsIDSet(deps)
		for _, t := range writeTasks {
			if ids[S(t, "id")] {
				planReview = nil
				break
			}
		}
	}
	if planReview != nil && S(planReview, "status") != TaskComplete {
		deps, _ := s.Store.Dependencies(S(planReview, "id"))
		reviewDeps := depsIDSet(deps)
		if policy.CriticRequired && critic != nil && !reviewDeps[S(critic, "id")] {
			if err := s.Store.AddDependency(S(planReview, "id"), S(critic, "id")); err != nil {
				return false, err
			}
			reviewDeps[S(critic, "id")] = true
			changed = true
		}
		for _, coord := range active {
			st := S(coord, "status")
			if S(coord, "profile") != "coordinator" || !(st == TaskPending || st == TaskReady || st == TaskRunning || st == TaskVerifying) {
				continue
			}
			path, _ := s.Store.DependencyPath(S(planReview, "id"), S(coord, "id"))
			if path != nil {
				continue
			}
			if !reviewDeps[S(coord, "id")] {
				if err := s.Store.AddDependency(S(planReview, "id"), S(coord, "id")); err != nil {
					var cyc *DependencyCycleError
					if errors.As(err, &cyc) {
						_ = s.Store.Event(S(collection, "project_id"), collectionID, S(planReview, "id"), "", "DEPENDENCY_CYCLE_REJECTED", Row{"from_task_id": S(coord, "id"), "to_task_id": S(planReview, "id"), "context": "planning_policy_strengthen", "error": err.Error()})
						continue
					}
					return false, err
				}
				reviewDeps[S(coord, "id")] = true
				changed = true
			}
		}
	}
	activeCoordinators := Rows{}
	for _, t := range active {
		st := S(t, "status")
		if S(t, "profile") == "coordinator" && (st == TaskPending || st == TaskReady || st == TaskRunning || st == TaskWaiting || st == TaskVerifying) {
			activeCoordinators = append(activeCoordinators, t)
		}
	}
	if policy.PlanReviewRequired && planReview == nil && len(activeCoordinators) == 0 {
		deps := []string{}
		if critic != nil {
			deps = append(deps, S(critic, "id"))
		}
		parent := reverseProfile(active, "coordinator")
		depth := 1
		pid := ""
		if parent != nil {
			depth = I(parent, "depth") + 1
			pid = S(parent, "id")
		}
		var bc Row
		fp, source := "", ""
		if critic != nil && S(critic, "status") == TaskComplete {
			base, _ := s.Store.CriticPlanBaseline(S(critic, "id"))
			if base == nil {
				bc = critic
				fp, _ = s.materialFingerprint(collection, active, false)
				source = "initial_policy_plan_seal"
			}
		}
		planReview, _, err = s.Store.CreateTask(CreateTaskOptions{ProjectID: S(collection, "project_id"), CollectionID: collectionID, Profile: "plan_review", Title: "Policy plan review", Objective: fmt.Sprintf("Independently review the current durable work graph before any implementation runs. Deterministic policy categories: %s. Use completed critic directives as required input. Verify that implementation tasks are decision-complete, security/state semantics are explicit, dependencies are coherent, verification is executable, and executing the graph exactly as written can satisfy every Collection MUST/MUST NOT and acceptance requirement without knowingly requiring a later repair for an already-visible defect. Return verdict pass/fail; a known acceptance violation is a true blocker. Name at most three actionable blockers.", func() string {
			if len(policy.Categories) > 0 {
				return strings.Join(policy.Categories, ", ")
			}
			return "risk"
		}()), Reason: "Deterministic planning policy: " + reasonText, ParentTaskID: pid, DependsOn: deps, WorkspaceMode: "collection", Depth: depth, AllowDedupe: false, GateImplementTasks: true, CriticBaselineTaskID: S(bc, "id"), CriticBaselineFingerprint: fp, CriticBaselineSource: source})
		if err != nil {
			return false, err
		}
		if parent != nil {
			_ = s.Store.AddEdge(S(parent, "id"), S(planReview, "id"), collectionID, "spawned_by")
		}
		changed = true
	}
	if policy.PlanReviewRequired && planReview != nil {
		for _, wt := range writeTasks {
			deps, _ := s.Store.Dependencies(S(wt, "id"))
			if !depsIDSet(deps)[S(planReview, "id")] {
				if err := s.Store.AddDependency(S(wt, "id"), S(planReview, "id")); err != nil {
					return false, err
				}
				changed = true
			}
		}
	}
	if changed {
		_ = s.Store.Event(S(collection, "project_id"), collectionID, "", "", "PLANNING_POLICY_ENFORCED", Row{"explore_required": policy.ExploreRequired, "critic_required": policy.CriticRequired, "plan_review_required": policy.PlanReviewRequired, "categories": policy.Categories, "critic_task_id": func() any {
			if critic != nil {
				return S(critic, "id")
			}
			return nil
		}(), "plan_review_task_id": func() any {
			if planReview != nil {
				return S(planReview, "id")
			}
			return nil
		}(), "gated_implement_tasks": func() []string {
			x := []string{}
			for _, t := range writeTasks {
				x = append(x, S(t, "id"))
			}
			return x
		}()})
		if S(collection, "status") == "BLOCKED" {
			_ = s.Store.SetCollectionStatus(collectionID, "RUNNING", false)
		}
	}
	return changed, nil
}

func (s *Scheduler) deferCompletion(taskID, attemptID string) (bool, error) {
	current, err := s.Store.GetTask(taskID)
	if err != nil {
		return false, err
	}
	if S(current, "status") != TaskVerifying {
		return false, nil
	}
	deps, err := s.Store.Dependencies(taskID)
	if err != nil {
		return false, err
	}
	unmet := []string{}
	for _, d := range deps {
		if S(d, "status") != TaskComplete {
			unmet = append(unmet, S(d, "id"))
		}
	}
	if len(unmet) == 0 {
		return false, nil
	}
	if err = s.Store.SetTaskStatus(taskID, TaskWaiting, "task "+taskID+": completion deferred for dependencies "+strings.Join(unmet, ",")); err != nil {
		return false, err
	}
	_ = s.Store.Event(S(current, "project_id"), S(current, "collection_id"), taskID, attemptID, "TASK_COMPLETION_DEFERRED", Row{"unmet_dependencies": unmet})
	return true, nil
}

func (s *Scheduler) handleMissingReviewVerdict(cfg ProjectConfig, task, attempt Row) string {
	msg := "Review task exited without submitting an explicit structured pass/fail verdict."
	if I(attempt, "attempt_no") < cfg.Limits.MaxAttemptsPerTask {
		_ = s.Store.SetTaskStatus(S(task, "id"), TaskReady, "task "+S(task, "id")+": retry after missing review verdict")
		_ = s.Store.Event(S(task, "project_id"), S(task, "collection_id"), S(task, "id"), S(attempt, "id"), "REVIEW_PROTOCOL_RETRY", Row{"attempt_no": I(attempt, "attempt_no"), "reason": msg})
	} else {
		_, _ = s.Store.SubmitResult(S(task, "id"), Row{"status": "failed", "summary": msg, "risks": []any{"Missing review verdict after retry budget exhausted"}})
	}
	return msg
}

func (s *Scheduler) handleReadOnlyCapabilityViolation(cfg ProjectConfig, task, attempt Row) string {
	msg := "Read-only task mutated its workspace; changes were discarded."
	if I(attempt, "attempt_no") < cfg.Limits.MaxAttemptsPerTask {
		_ = s.Store.SetTaskStatus(S(task, "id"), TaskReady, "task "+S(task, "id")+": retry after read-only capability violation")
		_ = s.Store.Event(S(task, "project_id"), S(task, "collection_id"), S(task, "id"), S(attempt, "id"), "READ_ONLY_CAPABILITY_RETRY", Row{"attempt_no": I(attempt, "attempt_no"), "reason": msg})
	} else {
		_, _ = s.Store.SubmitResult(S(task, "id"), Row{"status": "failed", "summary": msg, "artifacts": []any{}, "risks": []any{"Capability boundary violation after retry budget exhausted"}})
	}
	return msg
}

func (s *Scheduler) processResultMetadata(task Row) {
	refreshed, err := s.Store.GetTask(S(task, "id"))
	if err != nil {
		return
	}
	result := M(refreshed, "result")
	for _, raw := range Slice(result, "memory_candidates") {
		text, kind := "", "finding"
		switch x := raw.(type) {
		case string:
			text = x
		case map[string]any:
			r := Row(x)
			text = strings.TrimSpace(firstNonEmpty(S(r, "text"), S(r, "content")))
			if k := firstNonEmpty(S(r, "kind"), S(r, "type")); k != "" {
				kind = k
			}
		case Row:
			text = strings.TrimSpace(firstNonEmpty(S(x, "text"), S(x, "content")))
			if k := firstNonEmpty(S(x, "kind"), S(x, "type")); k != "" {
				kind = k
			}
		}
		if text != "" {
			_, _ = s.Store.AddMemory(S(task, "project_id"), S(task, "collection_id"), S(task, "id"), "collection", "candidate", kind, text, nil)
		}
	}
	for _, raw := range Slice(result, "decisions") {
		statement, rationale := "", ""
		var evidence, alts any = []any{}, []any{}
		switch x := raw.(type) {
		case string:
			statement = x
		case map[string]any:
			r := Row(x)
			statement = strings.TrimSpace(S(r, "statement"))
			rationale = strings.TrimSpace(S(r, "rationale"))
			if r["evidence"] != nil {
				evidence = r["evidence"]
			}
			if r["alternatives"] != nil {
				alts = r["alternatives"]
			}
		case Row:
			statement = strings.TrimSpace(S(x, "statement"))
			rationale = strings.TrimSpace(S(x, "rationale"))
			if x["evidence"] != nil {
				evidence = x["evidence"]
			}
			if x["alternatives"] != nil {
				alts = x["alternatives"]
			}
		}
		if statement != "" {
			_, _ = s.Store.AddDecision(S(task, "project_id"), S(task, "collection_id"), S(task, "id"), statement, rationale, evidence, alts)
		}
	}
}

func (s *Scheduler) ensureReview(project, collection, impl Row) error {
	var failedParentReview Row
	if parentID := S(impl, "parent_task_id"); parentID != "" {
		parent, err := s.Store.GetTask(parentID)
		if err == nil && S(parent, "profile") == "review" && strings.ToLower(strings.TrimSpace(S(M(parent, "result"), "verdict"))) == "fail" {
			failedParentReview = parent
		}
	}
	existing, err := s.Store.FindRelatedReview(S(impl, "id"))
	if err != nil {
		return err
	}
	if existing != nil {
		return nil
	}
	deps := []string{S(impl, "id")}
	title := "Review " + S(impl, "title")
	objective := fmt.Sprintf("Independently review implementation task %s against its objective and acceptance intent. Inspect the integrated code, run appropriate checks, and return verdict pass/fail. If repair is required, request a blocking implement task rather than changing code yourself.", S(impl, "id"))
	reason := "Praector policy requires independent review of every implementation task."
	originalVerificationSources := []string{}
	if failedParentReview != nil {
		edges, e := s.Store.ListEdges(S(impl, "collection_id"))
		if e != nil {
			return e
		}
		for _, edge := range edges {
			if S(edge, "relation") == "verifies" && S(edge, "to_task_id") == S(failedParentReview, "id") {
				source := S(edge, "from_task_id")
				if source != "" && !containsString(deps, source) {
					deps = append(deps, source)
					originalVerificationSources = append(originalVerificationSources, source)
				}
			}
		}
		title = "Fresh review after repair " + S(impl, "title")
		objective = fmt.Sprintf("Perform a fresh independent post-repair review. Repair implementation %s was created because review %s returned verdict=fail. Re-evaluate the repaired integrated candidate from scratch against the original acceptance intent and the repair objective; do not rely on the prior review's conclusions. Return verdict pass/fail.", S(impl, "id"), S(failedParentReview, "id"))
		reason = "A semantic review failure was repaired; policy requires a fresh independent review task/context."
	}
	review, _, err := s.Store.CreateTask(CreateTaskOptions{
		ProjectID: S(impl, "project_id"), CollectionID: S(impl, "collection_id"), Profile: "review", Title: title,
		Objective: objective,
		Reason:    reason, ParentTaskID: S(impl, "id"), DependsOn: deps, WorkspaceMode: "collection", Depth: I(impl, "depth") + 1, AllowDedupe: true,
	})
	if err != nil {
		return err
	}
	if err := s.Store.AddEdge(S(impl, "id"), S(review, "id"), S(collection, "id"), "verifies"); err != nil {
		return err
	}
	for _, source := range originalVerificationSources {
		if err := s.Store.AddEdge(source, S(review, "id"), S(collection, "id"), "verifies"); err != nil {
			return err
		}
	}
	if failedParentReview != nil {
		_ = s.Store.Event(S(impl, "project_id"), S(impl, "collection_id"), S(review, "id"), "", "POST_REPAIR_REVIEW_CREATED", Row{"failed_review_task_id": S(failedParentReview, "id"), "repair_task_id": S(impl, "id"), "original_implementation_task_ids": originalVerificationSources})
	}
	if S(impl, "workspace_mode") == "inherit" && S(impl, "parent_task_id") != "" {
		parent, err := s.Store.GetTask(S(impl, "parent_task_id"))
		if err == nil && S(parent, "status") == TaskWaiting {
			if err := s.Store.AddEdge(S(review, "id"), S(parent, "id"), S(collection, "id"), "depends_on"); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Scheduler) handleFailedReview(project, task Row) error {
	current, err := s.Store.GetTask(S(task, "id"))
	if err != nil {
		return err
	}
	result := M(current, "result")
	if S(current, "profile") != "review" || S(current, "status") != TaskVerifying || strings.ToLower(S(result, "verdict")) != "fail" {
		return nil
	}
	summary := S(result, "summary")
	if summary == "" {
		summary = "Review found defects requiring repair."
	}
	repair, _, err := s.Store.RequestTask(S(current, "id"), RequestTaskOptions{Profile: "implement", Title: "Repair findings from " + S(current, "id"), Objective: fmt.Sprintf("Address the review findings from %s. Review summary: %s", S(current, "id"), summary), Reason: "Independent review returned verdict=fail. The failed review will complete as historical evidence; this repair must receive a fresh independent review task after integration.", Blocking: false, DependsOn: []string{S(current, "id")}, WorkspaceMode: "collection", Limits: s.limits(project)})
	if err != nil {
		return err
	}
	_ = s.Store.Event(S(current, "project_id"), S(current, "collection_id"), S(current, "id"), "", "REVIEW_REPAIR_CREATED", Row{"repair_task_id": S(repair, "id")})
	return nil
}

func (s *Scheduler) handleFailedPlanReview(project, task Row) error {
	current, err := s.Store.GetTask(S(task, "id"))
	if err != nil {
		return err
	}
	result := M(current, "result")
	if S(current, "profile") != "plan_review" || S(current, "status") != TaskVerifying || strings.ToLower(S(result, "verdict")) != "fail" {
		return nil
	}
	summary := S(result, "summary")
	if summary == "" {
		summary = "Plan review found blocking gaps."
	}
	coord, _, err := s.Store.RequestCoordinatorAttention(S(current, "id"), fmt.Sprintf("Plan review %s rejected the current graph. Repair the plan before execution continues. Summary: %s", S(current, "id"), summary), true, s.limits(project))
	if err != nil {
		return err
	}
	_ = s.Store.Event(S(current, "project_id"), S(current, "collection_id"), S(current, "id"), "", "PLAN_REVIEW_REPAIR_REQUESTED", Row{"coordinator_task_id": S(coord, "id")})
	return nil
}

func (s *Scheduler) finalizeBlockingCoordinatorAfterPlanReview(planReview Row) (bool, error) {
	ids := []string{}
	if p := firstNonEmpty(S(planReview, "requested_by_task_id"), S(planReview, "parent_task_id")); p != "" {
		ids = append(ids, p)
	}
	deps, err := s.Store.Dependents(S(planReview, "id"))
	if err != nil {
		return false, err
	}
	for _, d := range deps {
		if S(d, "profile") == "coordinator" {
			ids = append(ids, S(d, "id"))
		}
	}
	seen := map[string]bool{}
	finalized := false
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		coord, err := s.Store.GetTask(id)
		if err != nil {
			continue
		}
		if S(coord, "profile") != "coordinator" || S(coord, "status") != TaskWaiting {
			continue
		}
		hard, err := s.Store.Dependencies(id)
		if err != nil {
			return finalized, err
		}
		hasReview := false
		unmet := false
		for _, d := range hard {
			if S(d, "id") == S(planReview, "id") {
				hasReview = true
			}
			if S(d, "status") != TaskComplete {
				unmet = true
			}
		}
		if !hasReview || unmet {
			continue
		}
		if len(M(coord, "result")) == 0 {
			_, _ = s.Store.SubmitResult(id, Row{"status": "complete", "summary": fmt.Sprintf("Planning graph accepted by plan review %s.", S(planReview, "id")), "findings": []any{}, "evidence": []any{fmt.Sprintf("plan_review %s verdict=pass", S(planReview, "id"))}, "artifacts": []any{}, "risks": []any{}, "unresolved": []any{}, "memory_candidates": []any{}, "decisions": []any{}})
		}
		if err := s.Store.SetTaskStatus(id, TaskComplete, fmt.Sprintf("task %s: finalized after passing plan review %s", id, S(planReview, "id"))); err != nil {
			return finalized, err
		}
		_ = s.Store.Event(S(coord, "project_id"), S(coord, "collection_id"), id, "", "COORDINATOR_FINALIZED_AFTER_PLAN_REVIEW", Row{"plan_review_task_id": S(planReview, "id")})
		finalized = true
	}
	return finalized, nil
}

func tailText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

func (s *Scheduler) ExecuteTask(taskID string) {
	initial, err := s.Store.GetTask(taskID)
	if err != nil {
		return
	}
	project, err := s.Store.GetProject(S(initial, "project_id"))
	if err != nil {
		return
	}
	projectCfg := ProjectConfigFromRow(project)
	task, attempt, err := s.Store.ClaimTask(taskID, projectCfg.Limits.MaxAttemptsPerTask)
	if err != nil || task == nil || attempt == nil {
		return
	}
	collection, err := s.Store.GetCollection(S(task, "collection_id"))
	if err != nil {
		return
	}
	logPath := filepath.Join(s.Cfg.LogsDir(), S(collection, "id"), S(attempt, "id")+".log")
	_ = s.Store.SetAttemptLogPath(S(attempt, "id"), logPath)
	var session Row
	protocolFailure := ""
	workdir := ""
	var harnessResult HarnessResult
	defer func() {
		if rec := recover(); rec != nil {
			fmt.Fprintf(os.Stderr, "praector: executor panic for %s: %v\n", taskID, rec)
		}
	}()

	handleErr := func(execErr error) {
		if session != nil {
			_ = s.Store.FinishSession(S(session, "id"), "", "FAILED")
		}
		current, e := s.Store.GetTask(taskID)
		if e == nil && S(current, "status") == TaskRunning {
			if I(attempt, "attempt_no") < projectCfg.Limits.MaxAttemptsPerTask {
				_ = s.Store.SetTaskStatus(taskID, TaskReady, "task "+taskID+": retry after executor error")
			} else {
				_, _ = s.Store.SubmitResult(taskID, Row{"status": "failed", "summary": "Executor error: " + execErr.Error()})
			}
		}
		_ = s.Store.FinishAttempt(S(attempt, "id"), AttemptFailed, nil, Row{"error": execErr.Error()}, "", logPath)
		_ = s.Store.Event(S(task, "project_id"), S(task, "collection_id"), taskID, S(attempt, "id"), "EXECUTOR_ERROR", Row{"error": execErr.Error()})
	}

	workdir, err = s.Workspace.PrepareTask(project, collection, task)
	if err != nil {
		handleErr(err)
		return
	}
	task, err = s.Store.GetTask(taskID)
	if err != nil {
		handleErr(err)
		return
	}
	session, err = s.Store.CreateSession(S(attempt, "id"), projectCfg.Harness, workdir)
	if err != nil {
		handleErr(err)
		return
	}
	prompt, err := BuildTaskEnvelope(s.Store, project, collection, task, attempt)
	if err != nil {
		handleErr(err)
		return
	}
	harness, err := CreateHarness(projectCfg.Harness, projectCfg.OpenCodeCommand)
	if err != nil {
		handleErr(err)
		return
	}
	env := map[string]string{
		"PRAECTOR_HOME": s.Cfg.Home, "PRAECTOR_PROJECT_ID": S(task, "project_id"), "PRAECTOR_COLLECTION_ID": S(task, "collection_id"), "PRAECTOR_TASK_ID": taskID, "PRAECTOR_ATTEMPT_ID": S(attempt, "id"), "PRAECTOR_SESSION_ID": S(session, "id"), "PRAECTOR_PROFILE": S(task, "profile"), "PRAECTOR_WORKTREE": workdir,
	}
	_ = s.Store.Event(S(task, "project_id"), S(task, "collection_id"), taskID, S(attempt, "id"), "SESSION_STARTED", Row{"workdir": workdir})
	harnessResult, err = harness.RunTask(prompt, workdir, S(task, "profile"), projectCfg.ModelFor(S(task, "profile")), env, logPath, time.Duration(projectCfg.Limits.TaskTimeoutSeconds)*time.Second, func(pid int) { _ = s.Store.SetSessionPID(S(session, "id"), pid) })
	if err != nil {
		handleErr(err)
		return
	}
	sessionStatus := "FAILED"
	if harnessResult.TimedOut {
		sessionStatus = "TIMED_OUT"
	} else if harnessResult.ExitCode == 0 {
		sessionStatus = "COMPLETE"
	}
	_ = s.Store.FinishSession(S(session, "id"), harnessResult.ExternalSessionID, sessionStatus)
	current, err := s.Store.GetTask(taskID)
	if err != nil {
		handleErr(err)
		return
	}
	if ReadOnlyProfiles[S(task, "profile")] {
		dirty, e := s.Workspace.IsDirty(workdir)
		if e == nil && dirty {
			_ = s.Workspace.RollbackReadOnlyChanges(current)
			protocolFailure = s.handleReadOnlyCapabilityViolation(projectCfg, task, attempt)
			current, _ = s.Store.GetTask(taskID)
		}
	}
	if S(current, "status") == TaskRunning {
		if harnessResult.ExitCode == 0 {
			if S(task, "profile") == "review" || S(task, "profile") == "plan_review" {
				protocolFailure = s.handleMissingReviewVerdict(projectCfg, task, attempt)
			} else {
				summary := harnessResult.FinalText
				if summary == "" {
					summary = "OpenCode task completed without an explicit structured result."
				}
				_, _ = s.Store.SubmitResult(taskID, Row{"status": "complete", "summary": summary, "findings": []any{}, "evidence": []any{}, "artifacts": []any{}, "risks": []any{}, "unresolved": []any{}, "memory_candidates": []any{}, "decisions": []any{}})
			}
		} else {
			if I(attempt, "attempt_no") < projectCfg.Limits.MaxAttemptsPerTask {
				_ = s.Store.SetTaskStatus(taskID, TaskReady, "task "+taskID+": retry after harness failure")
			} else {
				risk := "Harness failure"
				if harnessResult.Stderr != "" {
					risk = tailText(harnessResult.Stderr, 4000)
				}
				_, _ = s.Store.SubmitResult(taskID, Row{"status": "failed", "summary": fmt.Sprintf("OpenCode exited with code %d.", harnessResult.ExitCode), "risks": []any{risk}})
			}
		}
	}
	current, _ = s.Store.GetTask(taskID)
	if deferred, _ := s.deferCompletion(taskID, S(attempt, "id")); deferred {
		current, _ = s.Store.GetTask(taskID)
	}
	profile := S(task, "profile")
	if profile == "implement" {
		head, e := s.Workspace.Checkpoint(current, I(attempt, "attempt_no"), "attempt-checkpoint")
		if e != nil {
			handleErr(e)
			return
		}
		if head != "" {
			_ = s.Store.SetTaskWorkspace(taskID, nil, nil, nil, strptr(head))
		}
		current, _ = s.Store.GetTask(taskID)
		if S(current, "status") == TaskVerifying {
			integrated, e := s.Workspace.IntegrateTask(project, collection, current)
			if e == nil {
				_ = s.Store.Event(S(current, "project_id"), S(current, "collection_id"), taskID, S(attempt, "id"), "TASK_INTEGRATED", Row{"head": integrated})
				e = s.ensureReview(project, collection, current)
			}
			if e == nil {
				_ = s.Store.SetTaskStatus(taskID, TaskComplete, "task "+taskID+": integration complete")
			} else {
				_, _ = s.Store.SubmitResult(taskID, Row{"status": "failed", "summary": "Implementation completed but integration failed: " + e.Error(), "risks": []any{"Git integration conflict"}})
				_, _, _ = s.Store.RequestCoordinatorAttention(taskID, "Integration failed for "+taskID+": "+e.Error(), false, s.limits(project))
			}
		}
	} else if profile == "review" {
		_ = s.handleFailedReview(project, current)
		current, _ = s.Store.GetTask(taskID)
		if S(current, "status") == TaskVerifying {
			_ = s.Store.SetTaskStatus(taskID, TaskComplete, "task "+taskID+": review complete")
		}
	} else if profile == "plan_review" {
		_ = s.handleFailedPlanReview(project, current)
		current, _ = s.Store.GetTask(taskID)
		if S(current, "status") == TaskVerifying {
			_ = s.Store.SetTaskStatus(taskID, TaskComplete, "task "+taskID+": plan review complete")
			fresh, _ := s.Store.GetTask(taskID)
			_, _ = s.finalizeBlockingCoordinatorAfterPlanReview(fresh)
		}
	} else if S(current, "status") == TaskVerifying {
		_ = s.Store.SetTaskStatus(taskID, TaskComplete, "task "+taskID+": complete")
	}
	s.processResultMetadata(task)
	finalTask, _ := s.Store.GetTask(taskID)
	if S(finalTask, "status") == TaskFailed && S(finalTask, "profile") != "coordinator" {
		_, _, _ = s.Store.RequestCoordinatorAttention(taskID, fmt.Sprintf("Task %s (%s) failed and may require replacement or graph repair.", taskID, S(finalTask, "title")), false, s.limits(project))
	}
	attemptStatus := AttemptSucceeded
	if harnessResult.TimedOut {
		attemptStatus = AttemptTimedOut
	} else if harnessResult.ExitCode != 0 || protocolFailure != "" || S(finalTask, "status") == TaskFailed {
		attemptStatus = AttemptFailed
	}
	attemptResult := M(finalTask, "result")
	if len(attemptResult) == 0 && protocolFailure != "" {
		attemptResult = Row{"error": protocolFailure}
	}
	ec := harnessResult.ExitCode
	_ = s.Store.FinishAttempt(S(attempt, "id"), attemptStatus, &ec, attemptResult, harnessResult.ExternalSessionID, logPath)
	_ = s.Store.Event(S(finalTask, "project_id"), S(finalTask, "collection_id"), taskID, S(attempt, "id"), "ATTEMPT_FINISHED", Row{"task_status": S(finalTask, "status"), "attempt_status": attemptStatus})
}

func (s *Scheduler) maxWorkersFor(collectionID string) int {
	if s.MaxWorkersOverride > 0 {
		return s.MaxWorkersOverride
	}
	if collectionID != "" {
		c, e := s.Store.GetCollection(collectionID)
		if e == nil {
			p, e := s.Store.GetProject(S(c, "project_id"))
			if e == nil {
				return ProjectConfigFromRow(p).Limits.MaxParallelTasks
			}
		}
	}
	projects, e := s.Store.ListProjects()
	if e != nil || len(projects) == 0 {
		return 4
	}
	max := 1
	for _, p := range projects {
		if n := ProjectConfigFromRow(p).Limits.MaxParallelTasks; n > max {
			max = n
		}
	}
	return max
}

func (s *Scheduler) reconcileCollection(collectionID string) (string, error) {
	_, cycle, err := s.Store.RepairLegacyDependencyCycles(collectionID)
	if err != nil {
		return "", err
	}
	if cycle != nil {
		return "", &DependencyCycleError{Msg: fmt.Sprintf("Existing hard-dependency cycle in %s: %s", collectionID, strings.Join(cycle, " -> "))}
	}
	if _, err = s.Store.EnsureCollectionEntrypoint(collectionID); err != nil {
		return "", err
	}
	if _, err = s.Store.RefreshWaitingTasks(collectionID); err != nil {
		return "", err
	}
	if _, err = s.enforcePlanningPolicy(collectionID); err != nil {
		return "", err
	}
	if _, err = s.Store.RefreshWaitingTasks(collectionID); err != nil {
		return "", err
	}
	if _, err = s.enforceDeliveryPolicy(collectionID); err != nil {
		return "", err
	}
	if _, err = s.Store.RefreshWaitingTasks(collectionID); err != nil {
		return "", err
	}
	return s.Store.UpdateCollectionTerminalState(collectionID)
}

func (s *Scheduler) reconcileCollectionSafely(collectionID string) string {
	state, err := s.reconcileCollection(collectionID)
	if err == nil {
		delete(s.quarantined, collectionID)
		return state
	}
	var cyc *DependencyCycleError
	if !errors.As(err, &cyc) {
		fmt.Fprintf(os.Stderr, "praector: reconciliation error for %s: %v\n", collectionID, err)
		return "BLOCKED"
	}
	s.quarantined[collectionID] = true
	c, _ := s.Store.GetCollection(collectionID)
	if S(c, "status") != "BLOCKED" {
		_ = s.Store.SetCollectionStatus(collectionID, "BLOCKED", true)
	}
	events, _ := s.Store.EventsForCollection(collectionID, 50)
	recorded := false
	for _, e := range events {
		if S(e, "event_type") != "DEPENDENCY_CYCLE_QUARANTINED" {
			continue
		}
		p := JSONMap(S(e, "payload_json"))
		if S(p, "error") == err.Error() {
			recorded = true
			break
		}
	}
	if !recorded {
		_ = s.Store.Event(S(c, "project_id"), collectionID, "", "", "DEPENDENCY_CYCLE_QUARANTINED", Row{"error": err.Error()})
	}
	if !s.reportedQuarantines[collectionID] {
		fmt.Printf("praector: collection %s quarantined by dependency cycle: %v\n", collectionID, err)
		s.reportedQuarantines[collectionID] = true
	}
	return "BLOCKED"
}

type taskDone struct{ id string }

func (s *Scheduler) RunCollection(collectionID string, poll time.Duration) (string, error) {
	lock, err := s.ExclusiveScheduler()
	if err != nil {
		return "", err
	}
	defer lock.Close()
	_, _ = s.RecoverStaleSessions()
	maxWorkers := s.maxWorkersFor(collectionID)
	sem := make(chan struct{}, maxWorkers)
	done := make(chan taskDone, maxWorkers*4)
	running := map[string]bool{}
	var mu sync.Mutex
	launch := func(id string) {
		mu.Lock()
		if running[id] {
			mu.Unlock()
			return
		}
		running[id] = true
		mu.Unlock()
		sem <- struct{}{}
		go func() { defer func() { <-sem; done <- taskDone{id: id} }(); s.ExecuteTask(id) }()
	}
	for {
		state := s.reconcileCollectionSafely(collectionID)
		if state == "COMPLETE" {
			return state, nil
		}
		if s.quarantined[collectionID] {
			return "BLOCKED", nil
		}
		drain := true
		for drain {
			select {
			case d := <-done:
				mu.Lock()
				delete(running, d.id)
				mu.Unlock()
			default:
				drain = false
			}
		}
		mu.Lock()
		capacity := maxWorkers - len(running)
		mu.Unlock()
		if capacity > 0 {
			tasks, _ := s.Store.RunnableTasks(collectionID, capacity)
			for _, t := range tasks {
				launch(S(t, "id"))
			}
		}
		mu.Lock()
		count := len(running)
		mu.Unlock()
		if count == 0 {
			r, _ := s.Store.RunnableTasks(collectionID, 1)
			if len(r) == 0 {
				if state != "COMPLETE" {
					_ = s.Store.SetCollectionStatus(collectionID, "BLOCKED", true)
				}
				return "BLOCKED", nil
			}
		}
		if poll <= 0 {
			poll = 500 * time.Millisecond
		}
		time.Sleep(poll)
	}
}

func (s *Scheduler) Daemon(once bool, poll time.Duration) error {
	lock, err := s.ExclusiveScheduler()
	if err != nil {
		return err
	}
	defer lock.Close()
	_, _ = s.RecoverStaleSessions()
	maxWorkers := s.maxWorkersFor("")
	sem := make(chan struct{}, maxWorkers)
	done := make(chan taskDone, maxWorkers*4)
	running := map[string]bool{}
	var mu sync.Mutex
	launch := func(id string) {
		mu.Lock()
		if running[id] {
			mu.Unlock()
			return
		}
		running[id] = true
		mu.Unlock()
		sem <- struct{}{}
		go func() { defer func() { <-sem; done <- taskDone{id: id} }(); s.ExecuteTask(id) }()
	}
	for {
		activeIDs := []string{}
		collections, e := s.Store.ListCollections("")
		if e != nil {
			return e
		}
		for _, c := range collections {
			if S(c, "status") != "RUNNING" && S(c, "status") != "BLOCKED" {
				continue
			}
			_ = s.reconcileCollectionSafely(S(c, "id"))
			if s.quarantined[S(c, "id")] {
				continue
			}
			activeIDs = append(activeIDs, S(c, "id"))
			r, _ := s.Store.RunnableTasks(S(c, "id"), 1)
			if len(r) > 0 && S(c, "status") == "BLOCKED" {
				_ = s.Store.SetCollectionStatus(S(c, "id"), "RUNNING", false)
			}
		}
		drain := true
		for drain {
			select {
			case d := <-done:
				mu.Lock()
				delete(running, d.id)
				mu.Unlock()
			default:
				drain = false
			}
		}
		mu.Lock()
		capacity := maxWorkers - len(running)
		mu.Unlock()
		for _, cid := range activeIDs {
			if capacity <= 0 {
				break
			}
			tasks, _ := s.Store.RunnableTasks(cid, capacity)
			for _, t := range tasks {
				mu.Lock()
				already := running[S(t, "id")]
				mu.Unlock()
				if already {
					continue
				}
				launch(S(t, "id"))
				capacity--
				if capacity <= 0 {
					break
				}
			}
		}
		if once {
			mu.Lock()
			count := len(running)
			mu.Unlock()
			if count == 0 {
				has := false
				for _, cid := range activeIDs {
					r, _ := s.Store.RunnableTasks(cid, 1)
					if len(r) > 0 {
						has = true
						break
					}
				}
				if !has {
					return nil
				}
			}
		}
		if poll <= 0 {
			poll = time.Second
		}
		time.Sleep(poll)
	}
}
