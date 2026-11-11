from __future__ import annotations

from abc import ABC, abstractmethod
from pathlib import Path
from typing import Any, Callable

from ..models import HarnessResult


class Harness(ABC):
    """Execution boundary. Praector currently implements only OpenCode."""

    @abstractmethod
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
        raise NotImplementedError
