from __future__ import annotations

from dataclasses import dataclass
from enum import StrEnum
from typing import Any


class TaskStatus(StrEnum):
    PENDING = "PENDING"
    READY = "READY"
    RUNNING = "RUNNING"
    WAITING = "WAITING"
    VERIFYING = "VERIFYING"
    COMPLETE = "COMPLETE"
    FAILED = "FAILED"
    CANCELLED = "CANCELLED"
    SUPERSEDED = "SUPERSEDED"


class AttemptStatus(StrEnum):
    CREATED = "CREATED"
    STARTING = "STARTING"
    RUNNING = "RUNNING"
    SUCCEEDED = "SUCCEEDED"
    FAILED = "FAILED"
    TIMED_OUT = "TIMED_OUT"
    ABORTED = "ABORTED"


TERMINAL_TASK_STATUSES = {
    TaskStatus.COMPLETE.value,
    TaskStatus.FAILED.value,
    TaskStatus.CANCELLED.value,
    TaskStatus.SUPERSEDED.value,
}

ACTIVE_TASK_STATUSES = {
    TaskStatus.PENDING.value,
    TaskStatus.READY.value,
    TaskStatus.RUNNING.value,
    TaskStatus.WAITING.value,
    TaskStatus.VERIFYING.value,
}


@dataclass(slots=True)
class HarnessResult:
    exit_code: int
    stdout: str
    stderr: str
    final_text: str
    external_session_id: str | None = None
    timed_out: bool = False


@dataclass(slots=True)
class TaskRequest:
    profile: str
    title: str
    objective: str
    reason: str
    blocking: bool = False
    depends_on: list[str] | None = None
    workspace_mode: str | None = None


@dataclass(slots=True)
class ExecutionContext:
    project: dict[str, Any]
    collection: dict[str, Any]
    task: dict[str, Any]
    attempt: dict[str, Any]
    dependencies: list[dict[str, Any]]
    parent: dict[str, Any] | None
    memories: list[dict[str, Any]]
    decisions: list[dict[str, Any]]
