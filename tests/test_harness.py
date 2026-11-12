from pathlib import Path
from praector.harness.opencode import OpenCodeHarness


def test_permissions_disable_native_subagents(monkeypatch):
    monkeypatch.setattr("praector.harness.opencode.require_binary", lambda x: x)
    h = OpenCodeHarness("opencode")
    for profile in ("coordinator", "explore", "critic", "plan_review", "research", "review", "advise"):
        cfg = h._inline_config(profile)
        assert cfg["permission"]["task"] == "deny"
        assert cfg["permission"]["edit"] == "deny"
    implement = h._inline_config("implement")
    assert implement["permission"]["task"] == "deny"
    assert implement["permission"]["edit"] == "allow"
    assert implement["permission"]["bash"]["git push*"] == "deny"


def test_opencode_invocation_uses_current_cli_and_pure_mode(monkeypatch, tmp_path):
    monkeypatch.setattr("praector.harness.opencode.require_binary", lambda x: x)
    captured = {}

    class FakeProc:
        pid = 12345
        returncode = 0

        def communicate(self, timeout=None):
            return ('{"type":"text","text":"done"}\n', '')

    def fake_popen(args, **kwargs):
        captured["args"] = args
        captured["kwargs"] = kwargs
        return FakeProc()

    monkeypatch.setattr("praector.harness.opencode.subprocess.Popen", fake_popen)
    h = OpenCodeHarness("opencode")
    result = h.run_task(
        prompt="do work",
        workdir=tmp_path,
        profile="coordinator",
        model=None,
        env={"PRAECTOR_TASK_ID": "T-test"},
        log_path=tmp_path / "run.log",
        timeout_seconds=30,
    )

    assert result.exit_code == 0
    assert "--standalone" not in captured["args"]
    assert "--pure" in captured["args"]
    assert captured["args"][:2] == ["opencode", "run"]
    assert captured["args"][-1] == "do work"
    assert captured["kwargs"]["env"]["PWD"] == str(tmp_path)


def test_opencode_worker_env_sanitizes_parent_virtualenv_by_default(monkeypatch, tmp_path):
    monkeypatch.setattr("praector.harness.opencode.require_binary", lambda x: x)
    monkeypatch.setenv("VIRTUAL_ENV", "/tmp/praector-runtime-venv")
    monkeypatch.setenv("VIRTUAL_ENV_PROMPT", "(praector-runtime)")
    monkeypatch.setenv(
        "PATH",
        "/tmp/praector-runtime-venv/bin:/home/example/.local/bin:/usr/local/bin:/usr/bin",
    )
    monkeypatch.delenv("PRAECTOR_SANITIZE_PARENT_VENV", raising=False)
    captured = {}

    class FakeProc:
        pid = 12345
        returncode = 0

        def communicate(self, timeout=None):
            return ('{"type":"text","text":"done"}\n', '')

    def fake_popen(args, **kwargs):
        captured["env"] = kwargs["env"]
        return FakeProc()

    monkeypatch.setattr("praector.harness.opencode.subprocess.Popen", fake_popen)
    h = OpenCodeHarness("opencode")
    h.run_task(
        prompt="do work",
        workdir=tmp_path,
        profile="implement",
        model=None,
        env={"PRAECTOR_TASK_ID": "T-test"},
        log_path=tmp_path / "run.log",
        timeout_seconds=30,
    )

    worker_env = captured["env"]
    assert "VIRTUAL_ENV" not in worker_env
    assert "VIRTUAL_ENV_PROMPT" not in worker_env
    assert "/tmp/praector-runtime-venv/bin" not in worker_env["PATH"].split(":")
    assert "/home/example/.local/bin" in worker_env["PATH"].split(":")
    assert worker_env["PWD"] == str(tmp_path)


def test_opencode_worker_env_can_opt_out_of_parent_virtualenv_sanitization(monkeypatch, tmp_path):
    monkeypatch.setattr("praector.harness.opencode.require_binary", lambda x: x)
    monkeypatch.setenv("VIRTUAL_ENV", "/tmp/praector-runtime-venv")
    monkeypatch.setenv("VIRTUAL_ENV_PROMPT", "(praector-runtime)")
    monkeypatch.setenv("PRAECTOR_SANITIZE_PARENT_VENV", "0")
    monkeypatch.setenv(
        "PATH",
        "/tmp/praector-runtime-venv/bin:/home/example/.local/bin:/usr/local/bin:/usr/bin",
    )
    captured = {}

    class FakeProc:
        pid = 12345
        returncode = 0

        def communicate(self, timeout=None):
            return ('{"type":"text","text":"done"}\n', '')

    def fake_popen(args, **kwargs):
        captured["env"] = kwargs["env"]
        return FakeProc()

    monkeypatch.setattr("praector.harness.opencode.subprocess.Popen", fake_popen)
    h = OpenCodeHarness("opencode")
    h.run_task(
        prompt="do work",
        workdir=tmp_path,
        profile="implement",
        model=None,
        env={"PRAECTOR_TASK_ID": "T-test"},
        log_path=tmp_path / "run.log",
        timeout_seconds=30,
    )

    worker_env = captured["env"]
    assert worker_env["VIRTUAL_ENV"] == "/tmp/praector-runtime-venv"
    assert worker_env["VIRTUAL_ENV_PROMPT"] == "(praector-runtime)"
    assert worker_env["PATH"].startswith("/tmp/praector-runtime-venv/bin:")
    assert worker_env["PWD"] == str(tmp_path)


def test_explicit_task_virtualenv_can_override_cleaned_parent(monkeypatch, tmp_path):
    monkeypatch.setattr("praector.harness.opencode.require_binary", lambda x: x)
    monkeypatch.setenv("VIRTUAL_ENV", "/tmp/praector-runtime-venv")
    monkeypatch.setenv("PATH", "/tmp/praector-runtime-venv/bin:/usr/bin")
    monkeypatch.delenv("PRAECTOR_SANITIZE_PARENT_VENV", raising=False)
    captured = {}

    class FakeProc:
        pid = 12345
        returncode = 0

        def communicate(self, timeout=None):
            return ('{"type":"text","text":"done"}\n', '')

    def fake_popen(args, **kwargs):
        captured["env"] = kwargs["env"]
        return FakeProc()

    monkeypatch.setattr("praector.harness.opencode.subprocess.Popen", fake_popen)
    h = OpenCodeHarness("opencode")
    h.run_task(
        prompt="do work",
        workdir=tmp_path,
        profile="implement",
        model=None,
        env={
            "PRAECTOR_TASK_ID": "T-test",
            "VIRTUAL_ENV": "/workspace/.venv",
            "PATH": "/workspace/.venv/bin:/usr/bin",
        },
        log_path=tmp_path / "run.log",
        timeout_seconds=30,
    )

    worker_env = captured["env"]
    assert worker_env["VIRTUAL_ENV"] == "/workspace/.venv"
    assert worker_env["PATH"].startswith("/workspace/.venv/bin:")


def test_sanitized_worker_keeps_praector_control_plane_via_dedicated_shim(monkeypatch, tmp_path):
    monkeypatch.setattr("praector.harness.opencode.require_binary", lambda x: x)
    runtime = tmp_path / "praector-runtime"
    runtime_bin = runtime / "bin"
    runtime_bin.mkdir(parents=True)
    praector_cli = runtime_bin / "praector"
    praector_cli.write_text("#!/bin/sh\nexit 0\n", encoding="utf-8")
    praector_cli.chmod(0o755)
    monkeypatch.setenv("VIRTUAL_ENV", str(runtime))
    monkeypatch.setenv("PATH", f"{runtime_bin}:/usr/bin")
    monkeypatch.delenv("PRAECTOR_SANITIZE_PARENT_VENV", raising=False)
    captured = {}

    class FakeProc:
        pid = 12345
        returncode = 0

        def communicate(self, timeout=None):
            return ('{"type":"text","text":"done"}\n', '')

    def fake_popen(args, **kwargs):
        captured["env"] = kwargs["env"]
        return FakeProc()

    monkeypatch.setattr("praector.harness.opencode.subprocess.Popen", fake_popen)
    h = OpenCodeHarness("opencode")
    praector_home = tmp_path / "praector-home"
    h.run_task(
        prompt="do work",
        workdir=tmp_path,
        profile="critic",
        model=None,
        env={"PRAECTOR_TASK_ID": "T-test", "PRAECTOR_HOME": str(praector_home)},
        log_path=tmp_path / "run.log",
        timeout_seconds=30,
    )

    worker_env = captured["env"]
    path_parts = worker_env["PATH"].split(":")
    assert str(runtime_bin) not in path_parts
    assert path_parts[0] == str(praector_home / "control-bin")
    shim = Path(worker_env["PRAECTOR_CLI"])
    assert shim == praector_home / "control-bin" / "praector"
    assert shim.is_file()
    text = shim.read_text(encoding="utf-8")
    assert str(praector_cli.resolve()) in text
    assert "VIRTUAL_ENV" not in worker_env
