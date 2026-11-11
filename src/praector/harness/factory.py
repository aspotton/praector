from __future__ import annotations

from .base import Harness
from .opencode import OpenCodeHarness


def create_harness(name: str, *, opencode_command: str = "opencode") -> Harness:
    """Resolve a harness implementation.

    Praector currently ships only OpenCode. Keeping construction behind this
    boundary prevents scheduler/task semantics from depending on OpenCode.
    """
    normalized = name.strip().lower()
    if normalized == "opencode":
        return OpenCodeHarness(opencode_command)
    raise ValueError(f"Unsupported harness {name!r}. Praector currently supports only: opencode")
