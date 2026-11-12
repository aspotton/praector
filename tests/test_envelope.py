from praector.envelope import build_task_envelope


class FakeStore:
    def dependencies(self, task_id):
        return [{
            "id": "T-DEP", "profile": "research", "title": "Research", "status": "COMPLETE",
            "objective": "Find X", "result_json": '{"summary":"X found","findings":["x"],"artifacts":[]}'
        }]

    def get_parent(self, task):
        return None

    def list_memories(self, project_id, collection_id):
        return [{"kind": "constraint", "text": "Never push directly", "scope": "project"}]

    def list_decisions(self, project_id, collection_id):
        return [{"id": "DEC-1", "statement": "Use Git worktrees", "rationale": "Isolation"}]


def test_task_envelope_contains_delegation_contract():
    prompt = build_task_envelope(
        FakeStore(),
        {"id": "PRJ-1", "name": "demo", "root_path": "/repo", "base_branch": "main"},
        {"id": "COL-1", "title": "Feature", "objective": "Build feature", "status": "RUNNING"},
        {"id": "T-1", "profile": "implement", "title": "Implement", "objective": "Do it", "reason": "plan", "workspace_mode": "collection"},
        {"id": "A-1", "attempt_no": 1},
    )
    assert "Agents do not spawn agents" in prompt
    assert "praector task request" in prompt
    assert "OWN" in prompt and "DELEGATE" in prompt
    assert "Never push directly" in prompt
    assert "X found" in prompt
    assert '"pr_title"' in prompt
    assert '"pr_summary"' in prompt
    assert '"changes"' in prompt
    assert "human/reviewer-facing bullets" in prompt
    assert "T-PLAN-REVIEW" not in prompt
    assert "never invent or pass symbolic `T-*` placeholders" in prompt


class PlanningStore(FakeStore):
    def dependencies(self, task_id):
        return [{
            "id": "T-CRITIC", "profile": "critic", "title": "Pre-plan critique", "status": "COMPLETE",
            "objective": "Find planning blockers",
            "result_json": '{"summary":"One blocker","intent_classification":"architecture","planner_directives":["Preserve API compatibility"],"qa_directives":["Run contract tests"],"assumptions":["No migration"]}'
        }]

    def list_tasks(self, collection_id):
        return [
            {"id": "T-COORD", "profile": "coordinator", "status": "COMPLETE", "title": "Plan", "objective": "Plan it", "reason": "entry", "depth": 0},
            {"id": "T-PLAN", "profile": "plan_review", "status": "RUNNING", "title": "Review plan", "objective": "Review graph", "reason": "gate", "depth": 1},
        ]

    def list_edges(self, collection_id):
        return [{"from_task_id": "T-COORD", "to_task_id": "T-PLAN", "relation": "spawned_by"}]


def test_planning_envelope_contains_role_contract_graph_and_rich_dependency_context():
    prompt = build_task_envelope(
        PlanningStore(),
        {"id": "PRJ-1", "name": "demo", "root_path": "/repo", "base_branch": "main"},
        {"id": "COL-1", "title": "Feature", "objective": "Build feature", "status": "RUNNING"},
        {"id": "T-PLAN", "profile": "plan_review", "title": "Review plan", "objective": "Review graph", "reason": "gate", "workspace_mode": "collection"},
        {"id": "A-2", "attempt_no": 1},
    )
    assert "Plan reviewer operating contract" in prompt
    assert '"collection_graph"' in prompt
    assert '"relation": "spawned_by"' in prompt
    assert '"intent_classification": "architecture"' in prompt
    assert "Preserve API compatibility" in prompt
    assert "Run contract tests" in prompt


class GlobalContractStore(FakeStore):
    def list_tasks(self, collection_id):
        return []

    def list_edges(self, collection_id):
        return []


def _envelope_for_profile(profile: str) -> str:
    return build_task_envelope(
        GlobalContractStore(),
        {"id": "PRJ-1", "name": "demo", "root_path": "/repo", "base_branch": "main"},
        {"id": "COL-1", "title": "Feature", "objective": "Build feature", "status": "RUNNING"},
        {
            "id": f"T-{profile}",
            "profile": profile,
            "title": profile,
            "objective": "Do assigned work",
            "reason": "test",
            "workspace_mode": "collection",
        },
        {"id": f"A-{profile}", "attempt_no": 1},
    )


def test_every_profile_inherits_global_engineering_contract():
    for profile in ("coordinator", "explore", "critic", "plan_review", "research", "implement", "review", "advise"):
        prompt = _envelope_for_profile(profile)
        assert "## Global engineering contract" in prompt
        assert "Prefer the smallest coherent change" in prompt
        assert "README.md" in prompt and "AGENTS.md" in prompt
        assert "Use docstrings wherever they add durable understanding" in prompt
        assert prompt.index("## Global engineering contract") < prompt.index("## Role-specific operating contract")


def test_delegated_fresh_worker_inherits_same_global_contract_without_objective_duplication():
    coordinator = _envelope_for_profile("coordinator")
    implement = _envelope_for_profile("implement")
    phrase = "Praector automatically injects this global contract into every task envelope"
    assert phrase in coordinator
    assert phrase in implement
    assert "Do assigned work" in implement


def test_read_only_profiles_get_workspace_hygiene_contract_but_implement_does_not():
    for profile in ("coordinator", "explore", "critic", "plan_review", "research", "review", "advise"):
        prompt = _envelope_for_profile(profile)
        assert "Treat **no net workspace mutation** as part of the task's completion contract" in prompt
        assert "git status --porcelain=v1 --untracked-files=all" in prompt
        assert "PYTHONDONTWRITEBYTECODE=1" in prompt
        assert "final worktree state matches the starting baseline" in prompt
        assert "Never discard pre-existing changes" in prompt

    implement = _envelope_for_profile("implement")
    assert "Treat **no net workspace mutation**" not in implement
    assert "write capability within its assigned implementation scope" in implement
