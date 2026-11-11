import pytest

from praector.harness.factory import create_harness
from praector.harness.opencode import OpenCodeHarness


def test_factory_only_exposes_opencode_in_v1(monkeypatch):
    monkeypatch.setattr("praector.harness.opencode.require_binary", lambda x: x)
    assert isinstance(create_harness("opencode"), OpenCodeHarness)
    with pytest.raises(ValueError):
        create_harness("claude")
