from __future__ import annotations

import hashlib
import json
import os
import re
import shutil
import subprocess
import uuid
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Iterable


def utcnow() -> str:
    return datetime.now(timezone.utc).replace(tzinfo=None).isoformat(timespec="microseconds")


def make_id(prefix: str) -> str:
    return f"{prefix}-{uuid.uuid4().hex[:10]}"


def normalize_text(value: str) -> str:
    return re.sub(r"\s+", " ", value.strip().lower())


def dedupe_key(profile: str, objective: str) -> str:
    payload = f"{profile}\n{normalize_text(objective)}".encode()
    return hashlib.sha256(payload).hexdigest()[:32]


def json_dumps(value: Any) -> str:
    return json.dumps(value, ensure_ascii=False, separators=(",", ":"))


def json_loads(value: str | None, default: Any = None) -> Any:
    if not value:
        return default
    try:
        return json.loads(value)
    except (json.JSONDecodeError, TypeError):
        return default


def sql_literal(value: Any) -> str:
    if value is None:
        return "NULL"
    if isinstance(value, bool):
        return "1" if value else "0"
    if isinstance(value, (int, float)):
        return str(value)
    text = str(value).replace("\\", "\\\\").replace("'", "''")
    return f"'{text}'"


def require_binary(name_or_path: str) -> str:
    if os.path.sep in name_or_path:
        path = Path(name_or_path).expanduser()
        if not path.is_file() or not os.access(path, os.X_OK):
            raise RuntimeError(f"Required executable not found or not executable: {path}")
        return str(path)
    found = shutil.which(name_or_path)
    if not found:
        raise RuntimeError(f"Required executable not found on PATH: {name_or_path}")
    return found


def run(
    args: Iterable[str],
    *,
    cwd: str | Path | None = None,
    env: dict[str, str] | None = None,
    check: bool = True,
    input_text: str | None = None,
    timeout: float | None = None,
) -> subprocess.CompletedProcess[str]:
    proc = subprocess.run(
        list(args),
        cwd=str(cwd) if cwd is not None else None,
        env=env,
        text=True,
        input=input_text,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        timeout=timeout,
    )
    if check and proc.returncode != 0:
        cmd = " ".join(map(str, args))
        detail = proc.stderr.strip() or proc.stdout.strip()
        raise RuntimeError(f"Command failed ({proc.returncode}): {cmd}\n{detail}")
    return proc


def deep_merge(base: dict[str, Any], override: dict[str, Any]) -> dict[str, Any]:
    result = dict(base)
    for key, value in override.items():
        if isinstance(value, dict) and isinstance(result.get(key), dict):
            result[key] = deep_merge(result[key], value)
        else:
            result[key] = value
    return result
