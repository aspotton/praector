from praector.config import PROFILE_NAMES, READ_ONLY_PROFILES, WRITE_PROFILES
from argparse import Namespace

from praector.cli import cmd_profile_show
from praector.profile_prompts import PROFILE_TITLES, load_global_prompt, load_profile_prompt


def test_profile_taxonomy_and_capabilities():
    assert PROFILE_NAMES == (
        "coordinator",
        "explore",
        "critic",
        "plan_review",
        "research",
        "implement",
        "review",
        "advise",
    )
    assert WRITE_PROFILES == {"implement"}
    assert READ_ONLY_PROFILES == {
        "coordinator", "explore", "critic", "plan_review", "research", "review", "advise"
    }
    assert set(PROFILE_TITLES) == set(PROFILE_NAMES)


def test_every_profile_has_a_substantive_packaged_prompt():
    required_phrases = {
        "coordinator": "durable work graph",
        "explore": "current-state map",
        "critic": "pre-planning",
        "plan_review": "blocker",
        "research": "bounded question",
        "implement": "bounded executor",
        "review": "independent post-implementation verifier",
        "advise": "senior technical advisor",
    }
    for profile in PROFILE_NAMES:
        prompt = load_profile_prompt(profile)
        assert len(prompt) > 300
        assert required_phrases[profile].lower() in prompt.lower()


def test_global_engineering_contract_is_substantive_and_packaged():
    prompt = load_global_prompt()
    assert len(prompt) > 1200
    for phrase in (
        "Durable repository knowledge",
        "smallest coherent change",
        "parent, child, sibling",
        "recommendations, risks, or questions",
        "Match the style",
        "Use docstrings",
        "Praector automatically injects this global contract",
    ):
        assert phrase.lower() in prompt.lower()


def test_role_prompts_reinforce_global_rules_where_enforced():
    coordinator = load_profile_prompt("coordinator")
    implement = load_profile_prompt("implement")
    critic = load_profile_prompt("critic")
    plan_review = load_profile_prompt("plan_review")
    review = load_profile_prompt("review")

    assert "Global engineering-rule propagation" in coordinator
    assert "surrounding compatibility" in coordinator
    assert "Change-impact discipline" in implement
    assert "Durable documentation check" in implement
    assert "Lead with behavior, not code structure" in implement
    assert "locally correct" in critic
    assert "Change discipline" in plan_review
    assert "meaningful impact surface" in review


def test_profile_show_prints_effective_global_then_role_contract(capsys):
    assert cmd_profile_show(Namespace(profile="implement")) == 0
    output = capsys.readouterr().out
    assert "# Inherited global engineering contract" in output
    assert "# Role-specific operating contract" in output
    assert "# Global engineering rules" in output
    assert "# Implementation worker operating contract" in output
    assert output.index("# Global engineering rules") < output.index("# Implementation worker operating contract")


def test_planning_gates_reject_known_acceptance_violations_before_write_work():
    coordinator = load_profile_prompt("coordinator")
    critic = load_profile_prompt("critic")
    plan_review = load_profile_prompt("plan_review")

    assert "Do not intentionally create implementation work that violates a known Collection" in coordinator
    assert "Treat this as a planning blocker" in critic
    assert "review will catch it later" in plan_review
    assert "requires `verdict: fail`" in plan_review
