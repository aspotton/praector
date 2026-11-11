from __future__ import annotations

import hashlib
import json
import os
import signal
import subprocess
import shutil
import shlex
import sys
from pathlib import Path
from typing import Any, Callable

from .base import Harness
from ..config import READ_ONLY_PROFILES
from ..models import HarnessResult
from ..util import deep_merge, require_binary


def _recursive_find_session_id(value: Any) -> str | None:
    if isinstance(value, dict):
        for key in ("sessionID", "session_id", "sessionId"):
            candidate = value.get(key)
            if isinstance(candidate, str) and candidate:
                return candidate
        for child in value.values():
            found = _recursive_find_session_id(child)
            if found:
                return found
    elif isinstance(value, list):
        for child in value:
            found = _recursive_find_session_id(child)
            if found:
                return found
    return None


def _extract_text(value: Any) -> list[str]:
    found: list[str] = []
    if isinstance(value, dict):
        # Prefer explicit text payloads and avoid IDs / metadata strings.
        if isinstance(value.get("text"), str):
            found.append(value["text"])
        for key, child in value.items():
            if key != "text":
                found.extend(_extract_text(child))
    elif isinstance(value, list):
        for child in value:
            found.extend(_extract_text(child))
    return found


def _clean_inherited_worker_env(source: dict[str, str]) -> dict[str, str]:
    """Return a copy of the parent environment without Praector's active venv.

    Praector is commonly installed/runs from its own Python virtualenv. Passing that
    activation through to OpenCode makes worker shell commands resolve
    ``python``/``pip`` against Praector's private runtime rather than the project's
    normal environment. Keep the user's normal PATH, but remove only the active
    virtualenv's bin directory and activation marker. Explicit task env values
    are applied by ``run_task`` *after* this cleanup, so a future task-specific
    environment can intentionally opt back in.
    """
    cleaned = dict(source)
    active_venv = cleaned.pop("VIRTUAL_ENV", None)
    cleaned.pop("VIRTUAL_ENV_PROMPT", None)
    if active_venv:
        venv_bin = os.path.realpath(os.path.join(active_venv, "bin"))
        path_parts = cleaned.get("PATH", "").split(os.pathsep)
        cleaned["PATH"] = os.pathsep.join(
            part
            for part in path_parts
            if part and os.path.realpath(part) != venv_bin
        )
    return cleaned


def _find_praector_cli(parent_env: dict[str, str]) -> str | None:
    """Locate the control-plane CLI before worker PATH sanitization."""
    found = shutil.which("praector", path=parent_env.get("PATH"))
    if found:
        return os.path.realpath(found)
    sibling = Path(sys.executable).with_name("praector")
    if sibling.is_file():
        return str(sibling.resolve())
    argv0 = Path(sys.argv[0])
    if argv0.name == "praector" and argv0.exists():
        return str(argv0.resolve())
    return None


def _install_praector_control_shim(process_env: dict[str, str], parent_env: dict[str, str], task_env: dict[str, str]) -> str | None:
    """Expose only the Praector control command, not Praector's whole Python venv.

    Worker shell commands should resolve project ``python``/``pip`` normally,
    while durable orchestration commands (``praector task request``, ``praector result
    submit``) must remain available. A tiny wrapper directory provides just the
    ``praector`` executable and is prepended after parent-venv sanitization.
    """
    praector_cli = _find_praector_cli(parent_env)
    if not praector_cli:
        return None
    home_raw = (task_env.get("PRAECTOR_HOME") or process_env.get("PRAECTOR_HOME"))
    if not home_raw:
        return praector_cli
    shim_dir = Path(home_raw).expanduser() / "control-bin"
    shim_dir.mkdir(parents=True, exist_ok=True)
    shim = shim_dir / "praector"
    wrapper = f"#!/bin/sh\nexec {shlex.quote(praector_cli)} \"$@\"\n"
    if not shim.exists() or shim.read_text(encoding="utf-8", errors="ignore") != wrapper:
        tmp = shim.with_suffix(".tmp")
        tmp.write_text(wrapper, encoding="utf-8")
        tmp.chmod(0o755)
        tmp.replace(shim)
    parts = [part for part in process_env.get("PATH", "").split(os.pathsep) if part]
    shim_text = str(shim_dir)
    process_env["PATH"] = os.pathsep.join([shim_text] + [part for part in parts if part != shim_text])
    process_env["PRAECTOR_CLI"] = str(shim)
    return str(shim)


class OpenCodeHarness(Harness):
    def __init__(self, command: str = "opencode"):
        self.command = require_binary(command)

    def _inline_config(self, profile: str) -> dict[str, Any]:
        # Disable OpenCode's in-session subagent delegation. All delegation must
        # cross the praector boundary through `praector task request`.
        common_permission: dict[str, Any] = {
            "task": "deny",
            "question": "deny",
            "external_directory": "deny",
        }
        if profile in READ_ONLY_PROFILES:
            common_permission["edit"] = "deny"
            common_permission["bash"] = {
                "*": "allow",
                "git push*": "deny",
            }
        else:
            common_permission["edit"] = "allow"
            common_permission["bash"] = {
                "*": "allow",
                "git push*": "deny",
            }
        return {
            "$schema": "https://opencode.ai/config.json",
            "permission": common_permission,
            "agent": {
                "build": {
                    "permission": common_permission,
                }
            },
        }

    def run_task(
        self,
        *,
        prompt: str,
        workdir: Path,
        profile: str,
        model: str | None,
        env: dict[str, str],
        log_path: Path,
        timeout_seconds: int,
        on_start: Callable[[int], None] | None = None,
    ) -> HarnessResult:
        # Praector is commonly launched from its own Python virtualenv. Do not leak
        # that activation into project worker commands: it makes `python`/`pip`
        # resolve against Praector's private runtime instead of the task/project
        # environment. Sanitization is the safe default; installations that
        # intentionally need the parent venv may explicitly opt out with
        # PRAECTOR_SANITIZE_PARENT_VENV=0. Provider/user environment variables are
        # otherwise preserved.
        sanitize_raw = os.environ.get("PRAECTOR_SANITIZE_PARENT_VENV", "1").strip().lower()
        sanitize_parent_venv = sanitize_raw not in {"0", "false", "no", "off"}
        parent_env = dict(os.environ)
        process_env = (
            _clean_inherited_worker_env(parent_env)
            if sanitize_parent_venv
            else parent_env
        )
        process_env.update(env)
        # Sanitizing Praector's private virtualenv must not remove the durable
        # control-plane command itself. Expose a dedicated shim that contains
        # only `praector`; project `python`/`pip` continue to resolve outside the
        # Praector venv.
        praector_control = _install_praector_control_shim(process_env, parent_env, env)
        # Keep the shell-visible working directory synchronized with the real
        # subprocess cwd. OpenCode's headless `run` path has historically read
        # both process cwd and $PWD while resolving project/session context; a
        # stale inherited PWD can make it initialize against two directories
        # and surface only a generic UnknownError.
        process_env["PWD"] = str(workdir)
        injected = self._inline_config(profile)
        existing_raw = process_env.get("OPENCODE_CONFIG_CONTENT")
        if existing_raw:
            try:
                existing = json.loads(existing_raw)
            except json.JSONDecodeError:
                existing = {}
            injected = deep_merge(existing, injected)
        process_env["OPENCODE_CONFIG_CONTENT"] = json.dumps(injected)

        args = [
            self.command,
            "run",
            "--pure",
            "--format",
            "json",
            "--auto",
            "--agent",
            "build",
            "--title",
            f"praector {env.get('PRAECTOR_TASK_ID', 'task')}",
            "--dir",
            str(workdir),
        ]
        if model:
            args.extend(["--model", model])
        args.append(prompt)

        log_path.parent.mkdir(parents=True, exist_ok=True)
        proc = subprocess.Popen(
            args,
            cwd=str(workdir),
            env=process_env,
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            start_new_session=True,
        )
        if on_start:
            on_start(proc.pid)
        timed_out = False
        try:
            stdout, stderr = proc.communicate(timeout=timeout_seconds)
        except subprocess.TimeoutExpired:
            timed_out = True
            try:
                os.killpg(proc.pid, signal.SIGTERM)
            except ProcessLookupError:
                pass
            try:
                stdout, stderr = proc.communicate(timeout=10)
            except subprocess.TimeoutExpired:
                try:
                    os.killpg(proc.pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
                stdout, stderr = proc.communicate()

        prompt_bytes = len(prompt.encode("utf-8"))
        prompt_sha256 = hashlib.sha256(prompt.encode("utf-8")).hexdigest()
        harness_header = (
            "# HARNESS\n"
            f"command={self.command}\n"
            f"profile={profile}\n"
            f"workdir={workdir}\n"
            f"model={model or ''}\n"
            f"prompt_bytes={prompt_bytes}\n"
            f"prompt_sha256={prompt_sha256}\n"
            f"sanitize_parent_venv={str(sanitize_parent_venv).lower()}\n"
            f"virtual_env={process_env.get('VIRTUAL_ENV', '')}\n"
            f"praector_control={praector_control or ''}\n"
        )
        log_path.write_text(
            harness_header + "# STDOUT\n" + (stdout or "") + "\n# STDERR\n" + (stderr or ""),
            encoding="utf-8",
        )

        external_session_id: str | None = None
        text_fragments: list[str] = []
        for line in (stdout or "").splitlines():
            try:
                event = json.loads(line)
            except json.JSONDecodeError:
                continue
            external_session_id = external_session_id or _recursive_find_session_id(event)
            text_fragments.extend(_extract_text(event))
        # Deduplicate adjacent identical fragments common in event streams.
        final_parts: list[str] = []
        for text in text_fragments:
            if not final_parts or final_parts[-1] != text:
                final_parts.append(text)
        final_text = final_parts[-1] if final_parts else (stdout or "").strip()
        return HarnessResult(
            exit_code=124 if timed_out else int(proc.returncode or 0),
            stdout=stdout or "",
            stderr=stderr or "",
            final_text=final_text,
            external_session_id=external_session_id,
            timed_out=timed_out,
        )
