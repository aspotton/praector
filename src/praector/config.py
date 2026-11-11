from __future__ import annotations

import os
import tomllib
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

from .util import require_binary

PROFILE_NAMES = ("coordinator", "critic", "plan_review", "research", "implement", "review", "advise")
READ_ONLY_PROFILES = {"coordinator", "critic", "plan_review", "research", "review", "advise"}
WRITE_PROFILES = {"implement"}


@dataclass(slots=True)
class Limits:
    max_task_depth: int = 8
    max_tasks_per_collection: int = 100
    max_children_per_task: int = 10
    max_parallel_tasks: int = 4
    max_attempts_per_task: int = 3
    task_timeout_seconds: int = 3600


@dataclass(slots=True)
class ProjectConfig:
    project_id: str
    name: str
    root: Path
    base_branch: str
    harness: str = "opencode"
    opencode_command: str = "opencode"
    models: dict[str, str] = field(default_factory=dict)
    limits: Limits = field(default_factory=Limits)

    def model_for(self, profile: str) -> str | None:
        value = self.models.get(profile, "").strip()
        return value or None

    def resolved_opencode(self) -> str:
        return require_binary(self.opencode_command)


@dataclass(slots=True)
class GlobalConfig:
    home: Path

    @property
    def state_dir(self) -> Path:
        return self.home / "state"

    @property
    def lock_file(self) -> Path:
        return self.home / "state.lock"

    @property
    def scheduler_lock(self) -> Path:
        return self.home / "scheduler.lock"

    @property
    def worktrees_dir(self) -> Path:
        return self.home / "worktrees"

    @property
    def logs_dir(self) -> Path:
        return self.home / "logs"


def global_config() -> GlobalConfig:
    configured = os.environ.get("PRAECTOR_HOME")
    home = Path(configured or "~/.praector").expanduser().resolve()
    home.mkdir(parents=True, exist_ok=True)
    (home / "worktrees").mkdir(parents=True, exist_ok=True)
    (home / "logs").mkdir(parents=True, exist_ok=True)
    return GlobalConfig(home=home)


def find_project_marker(start: Path | None = None) -> Path | None:
    current = (start or Path.cwd()).resolve()
    if current.is_file():
        current = current.parent
    for directory in (current, *current.parents):
        candidate = directory / ".praector" / "project.toml"
        if candidate.is_file():
            return candidate
    return None


def read_project_marker(path: Path) -> ProjectConfig:
    with path.open("rb") as fh:
        data = tomllib.load(fh)
    project = data.get("project", {})
    workspace = data.get("workspace", {})
    harness = data.get("harness", {})
    models = data.get("models", {})
    limits_raw = data.get("limits", {})
    root_value = workspace.get("repository", "../..")
    root_path = (path.parent / root_value).resolve() if not Path(root_value).is_absolute() else Path(root_value).resolve()
    limits = Limits(
        max_task_depth=int(limits_raw.get("max_task_depth", 8)),
        max_tasks_per_collection=int(limits_raw.get("max_tasks_per_collection", 100)),
        max_children_per_task=int(limits_raw.get("max_children_per_task", 10)),
        max_parallel_tasks=int(limits_raw.get("max_parallel_tasks", 4)),
        max_attempts_per_task=int(limits_raw.get("max_attempts_per_task", 3)),
        task_timeout_seconds=int(limits_raw.get("task_timeout_seconds", 3600)),
    )
    return ProjectConfig(
        project_id=str(project["id"]),
        name=str(project["name"]),
        root=root_path,
        base_branch=str(workspace.get("base_branch", "main")),
        harness=str(harness.get("default", "opencode")),
        opencode_command=str(harness.get("command", "opencode")),
        models={name: str(models.get(name, "")) for name in PROFILE_NAMES},
        limits=limits,
    )


def _toml_string(value: str) -> str:
    return '"' + value.replace("\\", "\\\\").replace('"', '\\"') + '"'


def write_project_marker(config: ProjectConfig) -> Path:
    marker_dir = config.root / ".praector"
    marker_dir.mkdir(parents=True, exist_ok=True)
    marker = marker_dir / "project.toml"
    lines = [
        "version = 1",
        "",
        "[project]",
        f"id = {_toml_string(config.project_id)}",
        f"name = {_toml_string(config.name)}",
        "",
        "[workspace]",
        'type = "git"',
        'repository = ".."',
        f"base_branch = {_toml_string(config.base_branch)}",
        "",
        "[harness]",
        'default = "opencode"',
        f"command = {_toml_string(config.opencode_command)}",
        "",
        "[models]",
    ]
    for profile in PROFILE_NAMES:
        lines.append(f"{profile} = {_toml_string(config.models.get(profile, ''))}")
    lines.extend(
        [
            "",
            "[limits]",
            f"max_task_depth = {config.limits.max_task_depth}",
            f"max_tasks_per_collection = {config.limits.max_tasks_per_collection}",
            f"max_children_per_task = {config.limits.max_children_per_task}",
            f"max_parallel_tasks = {config.limits.max_parallel_tasks}",
            f"max_attempts_per_task = {config.limits.max_attempts_per_task}",
            f"task_timeout_seconds = {config.limits.task_timeout_seconds}",
            "",
        ]
    )
    marker.write_text("\n".join(lines), encoding="utf-8")
    return marker


def project_config_to_json(config: ProjectConfig) -> dict[str, Any]:
    return {
        "harness": config.harness,
        "opencode_command": config.opencode_command,
        "models": config.models,
        "limits": {
            "max_task_depth": config.limits.max_task_depth,
            "max_tasks_per_collection": config.limits.max_tasks_per_collection,
            "max_children_per_task": config.limits.max_children_per_task,
            "max_parallel_tasks": config.limits.max_parallel_tasks,
            "max_attempts_per_task": config.limits.max_attempts_per_task,
            "task_timeout_seconds": config.limits.task_timeout_seconds,
        },
    }


def project_config_from_row(row: dict[str, Any]) -> ProjectConfig:
    raw = row.get("config")
    if not isinstance(raw, dict):
        import json
        try:
            raw = json.loads(row.get("config_json") or "{}")
        except Exception:
            raw = {}
    limits_raw = raw.get("limits", {})
    return ProjectConfig(
        project_id=str(row["id"]),
        name=str(row["name"]),
        root=Path(row["root_path"]).resolve(),
        base_branch=str(row["base_branch"]),
        harness=str(raw.get("harness", "opencode")),
        opencode_command=str(raw.get("opencode_command", "opencode")),
        models={name: str(raw.get("models", {}).get(name, "")) for name in PROFILE_NAMES},
        limits=Limits(
            max_task_depth=int(limits_raw.get("max_task_depth", 8)),
            max_tasks_per_collection=int(limits_raw.get("max_tasks_per_collection", 100)),
            max_children_per_task=int(limits_raw.get("max_children_per_task", 10)),
            max_parallel_tasks=int(limits_raw.get("max_parallel_tasks", 4)),
            max_attempts_per_task=int(limits_raw.get("max_attempts_per_task", 3)),
            task_timeout_seconds=int(limits_raw.get("task_timeout_seconds", 3600)),
        ),
    )
