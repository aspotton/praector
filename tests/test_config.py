from pathlib import Path

from praector.config import Limits, ProjectCapabilities, ProjectConfig, project_config_from_row, read_project_marker, write_project_marker


def test_project_marker_round_trip(tmp_path):
    cfg = ProjectConfig(
        project_id="PRJ-123",
        name="demo",
        root=tmp_path,
        base_branch="main",
        opencode_command="/home/user/bin/opencode",
        models={"coordinator": "local/reason", "implement": "local/code"},
        limits=Limits(max_parallel_tasks=7),
    )
    marker = write_project_marker(cfg)
    loaded = read_project_marker(marker)
    assert loaded.project_id == cfg.project_id
    assert loaded.name == "demo"
    assert loaded.root == tmp_path.resolve()
    assert loaded.opencode_command == "/home/user/bin/opencode"
    assert loaded.models["coordinator"] == "local/reason"
    assert loaded.limits.max_parallel_tasks == 7


def test_global_config_uses_canonical_home(tmp_path, monkeypatch):
    from praector.config import global_config

    monkeypatch.setenv("HOME", str(tmp_path))
    monkeypatch.delenv("PRAECTOR_HOME", raising=False)
    assert global_config().home == (tmp_path / ".praector").resolve()


def test_global_config_uses_praector_home_override(tmp_path, monkeypatch):
    from praector.config import global_config

    custom = tmp_path / "custom-praector"
    monkeypatch.setenv("PRAECTOR_HOME", str(custom))
    assert global_config().home == custom.resolve()


def test_find_project_marker_uses_praector_namespace(tmp_path):
    from praector.config import find_project_marker

    nested = tmp_path / "a" / "b"
    nested.mkdir(parents=True)
    assert find_project_marker(nested) is None

    marker_dir = tmp_path / ".praector"
    marker_dir.mkdir()
    marker = marker_dir / "project.toml"
    marker.write_text("version = 1\n", encoding="utf-8")
    assert find_project_marker(nested) == marker


def test_project_research_capability_round_trip_and_legacy_default(tmp_path):
    cfg = ProjectConfig(
        project_id="PRJ-cap", name="cap", root=tmp_path, base_branch="main",
        capabilities=ProjectCapabilities(current_external_research=False),
    )
    marker = write_project_marker(cfg)
    loaded = read_project_marker(marker)
    assert loaded.capabilities.current_external_research is False

    legacy = project_config_from_row({
        "id": "PRJ-old", "name": "old", "root_path": str(tmp_path), "base_branch": "main", "config_json": "{}",
    })
    assert legacy.capabilities.current_external_research is True
