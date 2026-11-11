import argparse
from types import SimpleNamespace

import praector.cli as cli
from praector.pull_request import (
    build_pull_request_body,
    default_pull_request_title,
    implementation_change_bullets,
)


COLLECTION = {
    "id": "COL-1",
    "project_id": "PRJ-1",
    "title": "Add Twilio transfer support",
    "objective": "Add Twilio transfer support for comma-separated extensions.",
    "status": "COMPLETE",
    "integration_branch": "praector/collections/col-1",
}
PROJECT = {
    "id": "PRJ-1",
    "root_path": "/repo",
    "base_branch": "main",
}
TASKS = [
    {
        "id": "T-I",
        "profile": "implement",
        "status": "COMPLETE",
        "title": "Twilio comma-transfer via Number sendDigits",
        "parent_task_id": None,
        "result": {
            "summary": "Implemented comma transfer with Twilio Number sendDigits.",
            "pr_title": "Add Twilio transfer support for extensions via sendDigits",
            "pr_summary": "Adds support for Twilio transfer destinations that include an extension, using Number sendDigits after answer.",
            "changes": [
                "Allow comma-separated transfer destinations in `CallTransferFastTool`.",
                "Keep the raw transfer destination unchanged outside the Twilio gateway.",
                "Translate comma-separated Twilio destinations into `<Dial><Number sendDigits=\"...\">`.",
                "Preserve existing behavior for normal phone-number transfers.",
                "Keep ECR, browser, and fallback-number behavior unchanged.",
                "Add unit and handler-level regression tests.",
                "Update `AGENTS.md` and fast-tools documentation.",
            ],
            "unresolved": ["Run one live Twilio/PBX smoke call."],
        },
    },
    {
        "id": "T-R",
        "profile": "review",
        "status": "COMPLETE",
        "title": "Review Twilio comma-transfer",
        "parent_task_id": "T-I",
        "result": {
            "verdict": "pass",
            "evidence": ["pytest tests/ -q -> 1268 passed, 1 skipped"],
            "unresolved": [],
        },
    },
]
EDGES = [{"from_task_id": "T-I", "to_task_id": "T-R", "relation": "verifies"}]


class FakeStore:
    def __init__(self, *, existing_pr=None, acceptance=None):
        self.existing_pr = existing_pr
        self.acceptance = acceptance
        self.recorded = []

    def update_collection_terminal_state(self, collection_id):
        return "COMPLETE"

    def get_collection(self, collection_id):
        return dict(COLLECTION)

    def get_project(self, project_id):
        return dict(PROJECT)

    def critic_revalidation_state(self, collection_id):
        return {
            "latest_critic_task_id": "T-C",
            "latest_critic_status": "COMPLETE",
            "current_material_plan_fingerprint": "fp",
            "sealed_fingerprint": "fp",
            "baseline_source": "initial_plan_seal",
            "stale": False,
        }

    def list_tasks(self, collection_id):
        return [dict(t) for t in TASKS]

    def list_edges(self, collection_id):
        return list(EDGES)

    def get_collection_pull_request(self, collection_id, provider="github"):
        return dict(self.existing_pr) if self.existing_pr else None

    def get_collection_acceptance(self, collection_id, target_branch=None):
        return self.acceptance

    def record_collection_pull_request(self, **kwargs):
        record = {
            "id": "PR-1",
            "provider": kwargs["provider"],
            "pr_number": kwargs["pr_number"],
            "url": kwargs["url"],
            "state": kwargs["state"],
            "is_draft": kwargs["is_draft"],
            "base_branch": kwargs["base_branch"],
            "head_branch": kwargs["head_branch"],
            "source_commit": kwargs["source_commit"],
            "title": kwargs["title"],
            "remote_name": kwargs["remote_name"],
        }
        self.recorded.append(record)
        self.existing_pr = record
        return record


class FakeWorkspace:
    def __init__(self, store, cfg):
        pass

    def acceptance_plan(self, project, collection, *, target_branch=None):
        return {
            "source_branch": collection["integration_branch"],
            "source_commit": "abc1234567890",
            "target_branch": target_branch or "main",
            "target_commit": "def9876543210",
            "already_contained": False,
            "fast_forward": True,
            "diff_stat": " 6 files changed, 100 insertions(+)",
        }


class FakeProvider:
    name = "github"

    def __init__(self, *, found=None):
        self.found = found
        self.created = []
        self.opened = []
        self.views = []
        self.updated_bodies = []

    def repository(self):
        return {"name": "acme/agents", "url": "https://github.com/acme/agents"}

    def find_for_head(self, head_branch):
        return dict(self.found) if self.found else None

    def view(self, number):
        self.views.append(number)
        if self.found:
            return dict(self.found)
        return {
            "number": number,
            "url": f"https://github.com/acme/agents/pull/{number}",
            "state": "OPEN",
            "title": "Twilio comma-transfer via Number sendDigits",
            "base_branch": "main",
            "head_branch": "praector/collections/col-1",
            "is_draft": False,
        }

    def create(self, **kwargs):
        self.created.append(kwargs)
        self.found = {
            "number": 42,
            "url": "https://github.com/acme/agents/pull/42",
            "state": "OPEN",
            "title": kwargs["title"],
            "base_branch": kwargs["base_branch"],
            "head_branch": kwargs["head_branch"],
            "is_draft": kwargs["draft"],
        }
        return dict(self.found)

    def update_body(self, number, body):
        self.updated_bodies.append((number, body))
        current = self.view(number)
        return current

    def open(self, number):
        self.opened.append(number)


def _args(**overrides):
    values = {
        "collection_id": "COL-1",
        "draft": False,
        "title": None,
        "base": None,
        "remote": None,
        "dry_run": False,
        "refresh_copy": False,
        "open": False,
    }
    values.update(overrides)
    return argparse.Namespace(**values)


def _wire(monkeypatch, store, provider, pushes):
    monkeypatch.setattr(cli, "_store", lambda: store)
    monkeypatch.setattr(cli, "GitWorkspace", FakeWorkspace)
    monkeypatch.setattr(cli, "global_config", lambda: object())
    monkeypatch.setattr(cli, "_resolve_pr_remote", lambda repo, base, explicit=None: explicit or "origin")
    monkeypatch.setattr(cli, "_pull_request_provider", lambda repo: provider)
    monkeypatch.setattr(cli, "_accept_actor", lambda project: "Adam")

    def fake_run(argv, **kwargs):
        if "push" in argv:
            pushes.append(argv)
        return SimpleNamespace(stdout="", stderr="", returncode=0)

    monkeypatch.setattr(cli, "run", fake_run)


def test_pr_parser_defaults():
    args = cli.build_parser().parse_args(["collection", "pr", "COL-1"])
    assert args.collection_id == "COL-1"
    assert args.draft is False
    assert args.base is None
    assert args.remote is None
    assert args.dry_run is False
    assert args.refresh_copy is False
    assert args.open is False


def test_generated_pr_copy_uses_durable_results():
    title = default_pull_request_title(COLLECTION, TASKS)
    body = build_pull_request_body(COLLECTION, TASKS, source_commit="abc123456789")
    assert title == "Add Twilio transfer support for extensions via sendDigits"
    assert "Adds support for Twilio transfer destinations that include an extension" in body
    assert "Implemented comma transfer with Twilio Number sendDigits." not in body
    assert "- Allow comma-separated transfer destinations in `CallTransferFastTool`." in body
    assert "- Keep the raw transfer destination unchanged outside the Twilio gateway." in body
    assert "- Add unit and handler-level regression tests." in body
    assert "- Independent Praector review: **PASS**" in body
    assert "- Full test suite: 1268 passed, 1 skipped." in body
    assert "Run one live Twilio/PBX smoke test to confirm extension digits are accepted after answer." in body
    assert "`COL-1`" in body
    assert "`abc123456789`" in body


def test_legacy_implementation_result_recovers_richer_change_bullets():
    task = {
        "profile": "implement",
        "status": "COMPLETE",
        "title": "Twilio comma-transfer via Number sendDigits",
        "result": {
            "summary": "Twilio-only comma transfer is implemented with tests and docs.",
            "findings": [
                "tools/call_transfer.py:113 validation now accepts the comma form via a strict whitelist. The raw AUDIO_SINK_DTMF payload is unchanged so ECR/browser listeners remain byte-identical.",
                "gateways/twilio.py gains transfer_dial_twiml(), called only from twilio_dtmf, and comma-free input remains byte-identical.",
                "The tenant fallback divert path is unchanged and still rejects comma targets.",
            ],
            "evidence": [
                "python -m pytest tests/ -q -> 1268 passed, 1 skipped",
                "git diff --name-only -> AGENTS.md, docs/features/fast-tools.md, gateways/twilio.py, tests/unit/test_gateways_twilio.py, tests/unit/tools/test_call_transfer.py, tools/call_transfer.py",
            ],
            "artifacts": [],
        },
    }

    bullets = implementation_change_bullets(task)

    assert len(bullets) >= 6
    assert "Allow comma-separated transfer destinations while keeping input validation strict." in bullets
    assert "Keep the raw transfer destination unchanged outside the Twilio gateway." in bullets
    assert "Translate comma-separated Twilio destinations into `<Dial><Number sendDigits=\"...\">`." in bullets
    assert "Preserve existing behavior for normal phone-number transfers." in bullets
    assert "Keep ECR and browser transfer behavior unchanged." in bullets
    assert "Keep tenant fallback-number behavior unchanged." in bullets
    assert "Add automated regression coverage for the implemented behavior." in bullets
    assert "Update durable documentation in `AGENTS.md`, `docs/features/fast-tools.md`." in bullets
    assert not any("_COMMA_SEPARATED_DIGITS_RE" in bullet for bullet in bullets)
    assert not any("tools/call_transfer.py" in bullet for bullet in bullets)


def test_legacy_twilio_pr_body_is_human_facing_not_implementation_log_prose():
    legacy_tasks = [
        {
            "id": "T-I",
            "profile": "implement",
            "status": "COMPLETE",
            "title": "Twilio comma-transfer: dial extension via Number sendDigits",
            "result": {
                "summary": "Twilio-only comma transfer is implemented: tools/call_transfer.py now accepts a comma-separated target ('+12125552211,6655') through a strict whitelist while still emitting the raw string, and gateways/twilio.py builds the in-call dial TwiML as <Response><Dial><Number sendDigits=\"W6655\">+12125552211</Number></Dial></Response>, leaving comma-free transfers byte-identical. Full suite passes (1268 passed, 1 skipped).",
                "findings": [
                    "tools/call_transfer.py:113 validation now ORs the legacy plain rule with _COMMA_SEPARATED_DIGITS_RE via a new _is_transfer_target() helper. USER: UUID branch is untouched, and the AUDIO_SINK_DTMF emit still sends the raw formatted string, so ECR/browser listeners see byte-identical payloads.",
                    "gateways/twilio.py gains one module-level helper, transfer_dial_twiml(), called only from twilio_dtmf. Comma-free input returns the exact legacy f-string.",
                    "The tenant fallback divert path (_E164_RE, templates/twilio_fallback.xml) is unchanged.",
                ],
                "evidence": [
                    "python -m pytest tests/ -q -> 1268 passed, 1 skipped",
                    "git diff --name-only -> AGENTS.md, docs/features/fast-tools.md, gateways/twilio.py, tests/unit/test_gateways_twilio.py, tests/unit/tools/test_call_transfer.py, tools/call_transfer.py",
                ],
                "unresolved": ["Live Twilio sandbox smoke call for the comma transfer."],
            },
        },
        {
            "id": "T-R",
            "profile": "review",
            "status": "COMPLETE",
            "title": "Review Twilio comma-transfer",
            "result": {
                "verdict": "pass",
                "evidence": [
                    "python -m pytest tests/unit/tools/test_call_transfer.py tests/unit/test_gateways_twilio.py -q -> 49 passed in 1.19s",
                    "python -m pytest tests/ -q -> 1268 passed, 1 skipped in 27.40s",
                    "git diff HEAD~1 HEAD --name-only -> exactly AGENTS.md, docs/features/fast-tools.md, gateways/twilio.py, tests/unit/test_gateways_twilio.py, tests/unit/tools/test_call_transfer.py, tools/call_transfer.py; diff line count for gateways/ecr.py + browser/orchestration.py + templates/ = 0",
                ],
                "unresolved": [],
            },
        },
    ]

    title = default_pull_request_title(COLLECTION, legacy_tasks)
    body = build_pull_request_body(COLLECTION, legacy_tasks, source_commit="6b5f854d3503")

    assert title == "Add Twilio transfer support for extensions via sendDigits"
    assert "Adds support for Twilio transfer destinations that include an extension" in body
    assert "The phone number is dialed normally" in body
    assert "- Allow comma-separated transfer destinations while keeping input validation strict." in body
    assert "- Translate comma-separated Twilio destinations into `<Dial><Number sendDigits=\"...\">`." in body
    assert "- Independent Praector review: **PASS**" in body
    assert "- Targeted tests: 49 passed." in body
    assert "- Full test suite: 1268 passed, 1 skipped." in body
    assert "- Scope check: the reviewed diff stayed within the intended files and left out-of-scope paths unchanged." in body
    assert "_COMMA_SEPARATED_DIGITS_RE" not in body
    assert "git diff HEAD~1" not in body
    assert "python -m pytest" not in body
    assert "Run one live Twilio/PBX smoke test to confirm extension digits are accepted after answer." in body


def test_collection_pr_creates_pushes_and_records(monkeypatch, capsys):
    store = FakeStore()
    provider = FakeProvider()
    pushes = []
    _wire(monkeypatch, store, provider, pushes)

    assert cli.cmd_collection_pr(_args()) == 0

    assert len(pushes) == 1
    assert pushes[0][-2:] == ["origin", "praector/collections/col-1:praector/collections/col-1"]
    assert len(provider.created) == 1
    assert provider.created[0]["base_branch"] == "main"
    assert provider.created[0]["head_branch"] == "praector/collections/col-1"
    assert len(store.recorded) == 1
    assert store.recorded[0]["pr_number"] == 42
    assert "PR #42 created" in capsys.readouterr().out


def test_collection_pr_adopts_existing_remote_pr_without_duplicate(monkeypatch):
    existing = {
        "number": 41,
        "url": "https://github.com/acme/agents/pull/41",
        "state": "OPEN",
        "title": "Existing title",
        "base_branch": "main",
        "head_branch": "praector/collections/col-1",
        "is_draft": False,
    }
    store = FakeStore()
    provider = FakeProvider(found=existing)
    pushes = []
    _wire(monkeypatch, store, provider, pushes)

    assert cli.cmd_collection_pr(_args()) == 0

    assert len(pushes) == 1
    assert provider.created == []
    assert store.recorded[0]["pr_number"] == 41


def test_collection_pr_rerun_updates_same_open_pr_after_revision(monkeypatch):
    record = {
        "id": "PR-1",
        "provider": "github",
        "remote_name": "origin",
        "repository": "acme/agents",
        "pr_number": 41,
        "url": "https://github.com/acme/agents/pull/41",
        "state": "OPEN",
        "is_draft": False,
        "base_branch": "main",
        "head_branch": "praector/collections/col-1",
        "source_commit": "oldcommit",
        "title": "Existing title",
    }
    remote = {
        "number": 41,
        "url": record["url"],
        "state": "OPEN",
        "title": "Existing title",
        "base_branch": "main",
        "head_branch": "praector/collections/col-1",
        "is_draft": False,
    }
    store = FakeStore(existing_pr=record)
    provider = FakeProvider(found=remote)
    pushes = []
    _wire(monkeypatch, store, provider, pushes)

    assert cli.cmd_collection_pr(_args()) == 0

    assert len(pushes) == 1
    assert provider.created == []
    assert store.recorded[-1]["pr_number"] == 41
    assert store.recorded[-1]["source_commit"] == "abc1234567890"


def test_collection_pr_refresh_copy_updates_existing_open_pr_body(monkeypatch):
    record = {
        "id": "PR-1",
        "provider": "github",
        "remote_name": "origin",
        "repository": "acme/agents",
        "pr_number": 41,
        "url": "https://github.com/acme/agents/pull/41",
        "state": "OPEN",
        "is_draft": False,
        "base_branch": "main",
        "head_branch": "praector/collections/col-1",
        "source_commit": "abc1234567890",
        "title": "Existing title",
    }
    remote = {
        "number": 41,
        "url": record["url"],
        "state": "OPEN",
        "title": "Existing title",
        "base_branch": "main",
        "head_branch": "praector/collections/col-1",
        "is_draft": False,
    }
    store = FakeStore(existing_pr=record)
    provider = FakeProvider(found=remote)
    pushes = []
    _wire(monkeypatch, store, provider, pushes)

    assert cli.cmd_collection_pr(_args(refresh_copy=True)) == 0

    assert len(provider.updated_bodies) == 1
    number, body = provider.updated_bodies[0]
    assert number == 41
    assert "## Changes" in body
    assert "Allow comma-separated transfer destinations" in body
    assert provider.created == []


def test_collection_pr_dry_run_has_no_external_or_durable_mutation(monkeypatch, capsys):
    store = FakeStore()
    provider = FakeProvider()
    pushes = []
    _wire(monkeypatch, store, provider, pushes)

    assert cli.cmd_collection_pr(_args(dry_run=True, draft=True)) == 0

    assert pushes == []
    assert provider.created == []
    assert store.recorded == []
    output = capsys.readouterr().out
    assert "Draft:      yes" in output
    assert "Dry run: no branch was pushed" in output


def test_collection_pr_refuses_redundant_pr_after_direct_acceptance(monkeypatch):
    store = FakeStore(acceptance={"id": "ACC-1"})
    provider = FakeProvider()
    pushes = []
    _wire(monkeypatch, store, provider, pushes)

    try:
        cli.cmd_collection_pr(_args())
    except RuntimeError as exc:
        assert "already been accepted directly" in str(exc)
    else:
        raise AssertionError("expected direct-acceptance guard")
    assert pushes == []
    assert provider.created == []
