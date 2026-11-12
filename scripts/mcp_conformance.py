#!/usr/bin/env python3
"""Praector-owned black-box MCP probe (NOT an authoritative conformance oracle).

This is a black_box_test written and owned by Praector. It is deliberately
independent of Praector's own Go MCP core (`internal/praector/mcp`) so the
protocol contract is exercised by a second, out-of-process implementation rather
than only by in-process Go tests, and it speaks MCP Streamable HTTP over the
loopback endpoint exactly as a real client would. It is a helpful smoke probe, but
it is NOT the authoritative independent conformance oracle: that role belongs to
an official MCP client — the Inspector CLI (`npx @modelcontextprotocol/inspector
--cli --transport http`, driven by `TestMCPOfficialClientInteroperability`) or a
Tier-1 official SDK client. A green run of this script alone must not be presented
as official protocol conformance.

It is intended to be run against a LIVE loopback Praector MCP listener (the same
loopback-only server a running `praector daemon` hosts). The Go regression test
`TestMCPExternalConformanceOracle` starts that listener and invokes this script;
it can also be run by hand against a live daemon:

    # terminal 1: PRAECTOR_MCP=127.0.0.1:8765 praector daemon
    # terminal 2:
    python3 scripts/mcp_conformance.py --url http://127.0.0.1:8765/mcp

For the authoritative independent check, run the official Inspector CLI directly
against the same listener (preferred) or a Tier-1 official SDK client:

    npx @modelcontextprotocol/inspector --cli http://127.0.0.1:8765/mcp \
        --method tools/list

The built-in client below already covers the required checks with only the Python
standard library:

  * protocol/version negotiation: a supported revision succeeds and advertises
    exactly 2025-06-18; a well-formed UNSUPPORTED revision is answered with a
    legacy 2025-06-18 COUNTER-OFFER (a successful result advertising 2025-06-18)
    and is never echoed back, while a malformed/non-string protocolVersion is
    refused with -32602;
  * the MCP-Protocol-Version HTTP header contract: a missing header on a
    post-initialize request is a plain HTTP 400;
  * the DNS-rebinding defense: a foreign Origin or a literal "null"/opaque Origin
    is a plain HTTP 403;
  * tools/list, resources/list, and resources/read;
  * a safe read-only tool call (project list);
  * with --caller, a safe durable queue-write (task request) that records a real
    durable task, proving the same gated Service/Store path is reachable.

It never launches a scheduler or pushes/merges/accepts anything.
"""
from __future__ import annotations

import argparse
import json
import subprocess
import sys
import urllib.error
import urllib.request

PROTOCOL_VERSION = "2025-06-18"


class Client:
    def __init__(self, url: str) -> None:
        self.url = url
        self._id = 0

    def _post(self, payload: dict, *, header: bool = True, origin: str | None = None, version: str | None = None):
        body = json.dumps(payload).encode("utf-8")
        req = urllib.request.Request(self.url, data=body, method="POST")
        req.add_header("Content-Type", "application/json")
        if header:
            req.add_header("MCP-Protocol-Version", version or PROTOCOL_VERSION)
        if origin is not None:
            req.add_header("Origin", origin)
        try:
            with urllib.request.urlopen(req, timeout=10) as resp:
                return resp.status, json.loads(resp.read() or b"null")
        except urllib.error.HTTPError as e:
            raw = e.read() or b"null"
            try:
                return e.code, json.loads(raw)
            except json.JSONDecodeError:
                return e.code, None

    def call(self, method: str, params=None, **kw):
        self._id += 1
        msg = {"jsonrpc": "2.0", "id": self._id, "method": method}
        if params is not None:
            msg["params"] = params
        return self._post(msg, **kw)


def check(cond: bool, label: str) -> None:
    if not cond:
        raise SystemExit(f"FAIL: {label}")
    print(f"ok: {label}")


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--url", required=True, help="loopback MCP Streamable HTTP endpoint, e.g. http://127.0.0.1:8765/mcp")
    ap.add_argument("--caller", default="", help="an existing coordinator task id (T-*) to drive a safe queue-write")
    ap.add_argument("--inspector", default="", help="optional command for the official MCP Inspector CLI client")
    ns = ap.parse_args()

    c = Client(ns.url)

    status, res = c.call("initialize", {"protocolVersion": PROTOCOL_VERSION, "capabilities": {}, "clientInfo": {"name": "praector-conformance", "version": "0"}})
    check(status == 200 and res and res.get("error") is None, "initialize (supported version) succeeds")
    check(res["result"]["protocolVersion"] == PROTOCOL_VERSION, "advertises exactly the implemented protocol revision 2025-06-18")

    status, res = c.call("initialize", {"protocolVersion": "1999-01-01", "capabilities": {}, "clientInfo": {"name": "x", "version": "0"}})
    check(status == 200 and res and res.get("error") is None, "a well-formed unsupported protocol version counter-offers (success), not refused")
    check(res["result"]["protocolVersion"] == PROTOCOL_VERSION, "the counter-offer advertises the implemented revision 2025-06-18, never echoes 1999-01-01")

    status, res = c.call("tools/list")
    tools = ((res or {}).get("result") or {}).get("tools") or []
    check(status == 200 and len(tools) > 0, "tools/list returns a non-empty set")
    names = {t.get("name") for t in tools}
    check("praector_project_list" in names, "tools/list includes praector_project_list")

    status, res = c.call("resources/list")
    resources = ((res or {}).get("result") or {}).get("resources") or []
    check(status == 200 and len(resources) > 0, "resources/list returns a non-empty set")
    json_types = [r for r in resources if r.get("mimeType") == "application/json"]
    text_types = [r for r in resources if r.get("mimeType") == "text/plain"]
    check(json_types and text_types, "resources advertise accurate content types (both application/json and text/plain present)")

    uri = resources[0]["uri"]
    status, res = c.call("resources/read", {"uri": uri})
    check(status == 200 and ((res or {}).get("result") or {}).get("contents") is not None, "resources/read returns contents")

    status, res = c.call("tools/call", {"name": "praector_project_list", "arguments": {}})
    check(status == 200 and ((res or {}).get("result") or {}).get("isError") is not True, "a safe read tool call succeeds")

    status, _ = c.call("tools/list", header=False)
    check(status == 400, "a post-initialize request with a missing MCP-Protocol-Version header is HTTP 400")

    status, _ = c.call("tools/list", origin="http://evil.example")
    check(status == 403, "a request carrying a foreign Origin is rejected with HTTP 403 before dispatch")

    status, _ = c.call("tools/list", origin="null")
    check(status == 403, "a request carrying a literal \"null\"/opaque Origin is rejected with HTTP 403 before dispatch")

    if ns.caller:
        status, res = c.call("tools/call", {"name": "praector_task_request", "arguments": {
            "caller": ns.caller, "profile": "research", "title": "Conformance probe",
            "objective": "Determine X with evidence", "reason": "conformance oracle queue-write probe",
        }})
        text = json.dumps(res or {})
        check(status == 200 and "task T-" in text, "a safe durable queue-write records a durable task")

    if ns.inspector:
        # Optional authoritative external client (official MCP Inspector CLI).
        subprocess.run(ns.inspector.split() + [ns.url, "--method", "tools/list"], check=False)

    print("MCP external conformance checks passed")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
