from praector.workspace import GitWorkspace


def test_task_git_commit_message_uses_human_subject_and_praector_trailers():
    task = {
        "id": "T-c5a2d04929",
        "collection_id": "COL-21bd3e5582",
        "title": "Add authenticated GET /admin/secure-ping endpoint",
        "objective": "Implement the endpoint",
        "result": {
            "summary": "Added the authenticated secure-ping endpoint with regression coverage."
        },
    }

    checkpoint = GitWorkspace._task_git_commit_message(
        task, operation="attempt-checkpoint", attempt_no=2
    )
    assert checkpoint.splitlines()[0] == "Add authenticated GET /admin/secure-ping endpoint"
    assert "Praector-Collection: COL-21bd3e5582" in checkpoint
    assert "Praector-Task: T-c5a2d04929" in checkpoint
    assert "Praector-Attempt: 2" in checkpoint
    assert "Praector-Operation: attempt-checkpoint" in checkpoint

    integration = GitWorkspace._task_git_commit_message(
        task, operation="integration", subject_prefix="Integrate"
    )
    assert integration.splitlines()[0] == "Integrate: Add authenticated GET /admin/secure-ping endpoint"
    assert "Added the authenticated secure-ping endpoint with regression coverage." in integration
