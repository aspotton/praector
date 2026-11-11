#!/usr/bin/env python3
"""Black-box compatibility checks between native Go Praector and Python reference.

This intentionally compares only contracts that should be byte-stable across
implementations. Stateful scheduler/store parity is covered by the native Go
integration tests against the unchanged schema plus the Python regression suite.
"""
from __future__ import annotations

import argparse
import os
from pathlib import Path
import re
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[1]
PROFILES = ["coordinator", "critic", "plan_review", "research", "implement", "review", "advise"]
EXPECTED_COMMANDS = {
    "init", "run", "status", "graph", "daemon", "doctor", "logs", "collection",
    "profile", "project", "task", "context", "coordinator", "result", "memory", "decision",
}


def run(cmd: list[str], *, env: dict[str, str] | None = None) -> subprocess.CompletedProcess[str]:
    return subprocess.run(cmd, cwd=ROOT, env=env, text=True, capture_output=True, check=False)


def py_cmd(*args: str) -> subprocess.CompletedProcess[str]:
    env = os.environ.copy()
    env["PYTHONPATH"] = str(ROOT / "src")
    return run([sys.executable, "-m", "praector", *args], env=env)


def go_cmd(binary: Path, *args: str) -> subprocess.CompletedProcess[str]:
    return run([str(binary), *args])


def require_equal(name: str, a: subprocess.CompletedProcess[str], b: subprocess.CompletedProcess[str]) -> None:
    if a.returncode != b.returncode or a.stdout != b.stdout:
        raise SystemExit(
            f"parity mismatch: {name}\n"
            f"python rc={a.returncode} stdout={a.stdout!r} stderr={a.stderr!r}\n"
            f"go     rc={b.returncode} stdout={b.stdout!r} stderr={b.stderr!r}"
        )


def command_names(help_text: str) -> set[str]:
    found = set()
    for cmd in EXPECTED_COMMANDS:
        if re.search(rf"(?m)(?:^|[{{,\s]){re.escape(cmd)}(?:[}},\s]|$)", help_text):
            found.add(cmd)
    return found


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--go", required=True, type=Path)
    ns = ap.parse_args()
    binary = ns.go.resolve()
    if not binary.is_file():
        raise SystemExit(f"native binary not found: {binary}")

    require_equal("--version", py_cmd("--version"), go_cmd(binary, "--version"))
    require_equal("profile list", py_cmd("profile", "list"), go_cmd(binary, "profile", "list"))
    for profile in PROFILES:
        require_equal(
            f"profile show {profile}",
            py_cmd("profile", "show", profile),
            go_cmd(binary, "profile", "show", profile),
        )

    # Argparse-style help must remain available at every command layer without
    # initializing Dolt/Git/project state. Compare the stable command/option
    # vocabulary rather than argparse's terminal-width-dependent wrapping.
    help_paths = [
        ("collection",), ("collection", "pr"), ("collection", "accept"),
        ("task",), ("task", "request"), ("task", "retry"),
        ("result",), ("result", "submit"), ("memory",), ("decision",),
        ("context",), ("coordinator",), ("project",), ("profile",),
        ("run",), ("daemon",), ("doctor",), ("logs",),
    ]
    option_re = re.compile(r"--[a-z][a-z0-9-]*")

    def usage_options(help_text: str) -> set[str]:
        usage = help_text.split("\n\n", 1)[0]
        return set(option_re.findall(usage)) - {"--help"}
    for path in help_paths:
        py = py_cmd(*path, "--help")
        go = go_cmd(binary, *path, "--help")
        if py.returncode != 0 or go.returncode != 0:
            raise SystemExit(f"help failed for {' '.join(path)}: python={py.returncode} go={go.returncode}")
        py_options = usage_options(py.stdout)
        go_options = usage_options(go.stdout)
        if not py_options.issubset(go_options):
            raise SystemExit(
                f"help option mismatch for {' '.join(path)}: missing in Go {sorted(py_options-go_options)}\n"
                f"python={py.stdout!r}\ngo={go.stdout!r}"
            )

    py_help = py_cmd("--help")
    go_help = go_cmd(binary, "--help")
    if py_help.returncode != 0 or go_help.returncode != 0:
        raise SystemExit("top-level --help failed in one implementation")
    if command_names(py_help.stdout) != EXPECTED_COMMANDS:
        raise SystemExit(f"Python reference command surface changed unexpectedly: {command_names(py_help.stdout)}")
    if command_names(go_help.stdout) != EXPECTED_COMMANDS:
        raise SystemExit(f"Go command surface mismatch: {command_names(go_help.stdout)}")

    print("Native/Python parity checks passed")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
