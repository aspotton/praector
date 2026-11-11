from praector.planning_policy import material_plan_fingerprint


def _tasks():
    return [
        {
            "id": "T-I", "profile": "implement", "title": "Implement auth",
            "objective": "Add Bearer auth", "reason": "required", "status": "READY",
            "workspace_mode": "collection", "base_commit": "abc", "updated_at": "one",
        },
        {
            "id": "T-C", "profile": "coordinator", "title": "Planner",
            "objective": "Plan", "reason": "entry", "status": "RUNNING", "updated_at": "one",
        },
    ]


def test_material_plan_fingerprint_ignores_status_timestamp_and_orchestration_edges():
    tasks = _tasks()
    base = material_plan_fingerprint("Add auth", tasks, [])
    tasks[0]["status"] = "RUNNING"
    tasks[0]["updated_at"] = "two"
    tasks[1]["objective"] = "Different coordinator narration"
    edges = [{"from_task_id": "T-C", "to_task_id": "T-I", "relation": "spawned_by"}]
    assert material_plan_fingerprint("Add auth", tasks, edges) == base


def test_material_plan_fingerprint_changes_for_contract_or_material_dependency_change():
    tasks = _tasks()
    base = material_plan_fingerprint("Add auth", tasks, [])
    tasks[0]["objective"] = "Add Bearer auth and rotate credentials"
    changed_contract = material_plan_fingerprint("Add auth", tasks, [])
    assert changed_contract != base

    tasks = _tasks()
    tasks.append({
        "id": "T-A", "profile": "advise", "title": "Architecture",
        "objective": "Choose credential storage", "reason": "security decision", "status": "COMPLETE",
        "workspace_mode": "collection", "base_commit": "",
    })
    changed_dependency = material_plan_fingerprint(
        "Add auth", tasks,
        [{"from_task_id": "T-A", "to_task_id": "T-I", "relation": "depends_on"}],
    )
    assert changed_dependency != base


def test_material_plan_fingerprint_ignores_runtime_git_pointer_updates():
    tasks = _tasks()
    tasks[0]["base_commit"] = None
    tasks[0]["current_head"] = None
    base = material_plan_fingerprint("Add auth", tasks, [])

    # Workspace preparation and execution populate/move these runtime pointers;
    # that must not make an already-reviewed plan look materially different.
    tasks[0]["base_commit"] = "abc123"
    tasks[0]["current_head"] = "def456"
    tasks[0]["branch_name"] = "praector/tasks/col-1/t-i"
    tasks[0]["worktree_path"] = "/tmp/T-I"
    assert material_plan_fingerprint("Add auth", tasks, []) == base


def test_material_plan_fingerprint_keeps_inherited_checkpoint_material():
    tasks = _tasks()
    tasks[0]["workspace_mode"] = "inherit"
    tasks[0]["base_commit"] = "abc"
    base = material_plan_fingerprint("Add auth", tasks, [])
    tasks[0]["base_commit"] = "def"
    assert material_plan_fingerprint("Add auth", tasks, []) != base
