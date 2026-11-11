from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
DOCS = ROOT / "docs"


REQUIRED_DOCS = {
    "README.md",
    "ARCHITECTURE.md",
    "PROFILES.md",
    "AGENT_PROTOCOL.md",
    "CLI.md",
    "WORKSPACES.md",
    "STATE_AND_RECOVERY.md",
    "OPERATIONS.md",
    "DEVELOPMENT.md",
}


def test_documentation_map_exists():
    assert DOCS.is_dir()
    assert REQUIRED_DOCS <= {p.name for p in DOCS.glob("*.md")}


def test_human_entrypoint_links_to_docs_map_and_core_docs():
    text = (ROOT / "README.md").read_text(encoding="utf-8")
    for link in (
        "docs/README.md",
        "docs/ARCHITECTURE.md",
        "docs/PROFILES.md",
        "docs/AGENT_PROTOCOL.md",
        "docs/CLI.md",
        "docs/WORKSPACES.md",
        "docs/STATE_AND_RECOVERY.md",
        "docs/OPERATIONS.md",
        "docs/DEVELOPMENT.md",
        "ROADMAP.md",
        "AGENTS.md",
    ):
        assert link in text


def test_agent_entrypoint_links_to_architecture_and_development_docs():
    text = (ROOT / "AGENTS.md").read_text(encoding="utf-8")
    for link in (
        "docs/README.md",
        "docs/ARCHITECTURE.md",
        "docs/PROFILES.md",
        "docs/AGENT_PROTOCOL.md",
        "docs/DEVELOPMENT.md",
    ):
        assert link in text


def test_release_versions_live_in_changelog_not_readme_or_focused_docs():
    import re
    import tomllib

    version_pattern = re.compile(r"\b\d+\.\d+\.\d+\b")
    package_version = tomllib.loads((ROOT / "pyproject.toml").read_text(encoding="utf-8"))["project"]["version"]

    init_text = (ROOT / "src" / "praector" / "__init__.py").read_text(encoding="utf-8")
    assert f'__version__ = "{package_version}"' in init_text

    go_version_text = (ROOT / "internal" / "praector" / "version.go").read_text(encoding="utf-8")
    assert f'const Version = "{package_version}"' in go_version_text

    changelog = (ROOT / "CHANGELOG.md").read_text(encoding="utf-8")
    assert "## Unreleased" in changelog
    assert f"## {package_version}" in changelog
    assert "every repository change must update this file" in changelog.lower()

    version_neutral_files = [ROOT / "README.md", ROOT / "AGENTS.md", *sorted(DOCS.glob("*.md"))]
    for path in version_neutral_files:
        assert not version_pattern.search(path.read_text(encoding="utf-8")), path


def test_human_and_agent_entrypoints_link_to_changelog():
    assert "CHANGELOG.md" in (ROOT / "README.md").read_text(encoding="utf-8")
    assert "CHANGELOG.md" in (ROOT / "AGENTS.md").read_text(encoding="utf-8")
