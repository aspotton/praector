from __future__ import annotations

from importlib.resources import files


PROFILE_TITLES = {
    "coordinator": "Coordinator / Planner",
    "critic": "Pre-planning Critic",
    "plan_review": "Plan Reviewer",
    "research": "Researcher",
    "implement": "Implementation Worker",
    "review": "Implementation Reviewer",
    "advise": "Strategic Advisor",
}


def load_global_prompt() -> str:
    """Load the mandatory engineering contract inherited by every runtime profile."""
    resource = files("praector.prompts").joinpath("global.md")
    return resource.read_text(encoding="utf-8").strip()


def load_profile_prompt(profile: str) -> str:
    """Load the mandatory built-in operating contract for a runtime profile."""
    if profile not in PROFILE_TITLES:
        raise KeyError(f"Unknown profile prompt: {profile}")
    resource = files("praector.prompts").joinpath(f"{profile}.md")
    return resource.read_text(encoding="utf-8").strip()
