#!/usr/bin/env python3
"""Black-box smoke test for the compiled Praector executable.

The test intentionally runs the binary as a user would: in a fresh Git
repository with an isolated PRAECTOR_HOME. A small SQLite-backed Dolt shim exercises
the unchanged SQL/schema contract without requiring a Dolt installation in the
build environment. It does not replace the scheduler/store integration tests;
it verifies binary/config/filesystem/CLI wiring.
"""
from __future__ import annotations

import argparse
import os
from pathlib import Path
import re
import shutil
import stat
import subprocess
import sys
import tempfile

FAKE_DOLT = r'''#!/usr/bin/env python3
import json, os, re, sqlite3, sys
cwd=os.getcwd(); db=os.path.join(cwd,'.fake-dolt.sqlite3')
args=sys.argv[1:]
if not args: sys.exit(0)
cmd=args[0]
if cmd=='init': os.makedirs(os.path.join(cwd,'.dolt'),exist_ok=True); sys.exit(0)
if cmd in ('config','add','commit','status'): sys.exit(0)
if cmd!='sql': print('unsupported fake dolt command',args,file=sys.stderr); sys.exit(2)
q=None
if '-q' in args:
    i=args.index('-q'); q=args[i+1]
else:
    q=sys.stdin.read()
want_json='-r' in args and 'json' in args

def schema_xform(s):
    s=s.replace('LONGTEXT','TEXT').replace('DATETIME(6)','TEXT').replace('BOOLEAN','INTEGER')
    out=[]
    for line in s.splitlines():
        st=line.strip()
        if st.startswith('INDEX '): continue
        m=re.match(r'(\s*)UNIQUE KEY\s+\w+\s*\((.*)\)(,?)\s*$',line)
        if m: line=f"{m.group(1)}UNIQUE ({m.group(2)}){m.group(3)}"
        out.append(line)
    s='\n'.join(out)
    s=re.sub(r',\s*\n\s*\);', '\n);', s)
    return s

def xform(s):
    return s.replace('INSERT IGNORE','INSERT OR IGNORE')
con=sqlite3.connect(db)
con.row_factory=sqlite3.Row
try:
    sql=xform(q)
    if 'CREATE TABLE IF NOT EXISTS' in sql:
        con.executescript(schema_xform(sql)); con.commit(); rows=[]
    else:
        stripped=sql.lstrip().upper()
        if stripped.startswith('SELECT'):
            cur=con.execute(sql); rows=[dict(r) for r in cur.fetchall()]
        else:
            con.executescript(sql); con.commit(); rows=[]
    if want_json: print(json.dumps({'rows':rows},separators=(',',':')))
except Exception as e:
    print(f'fake dolt sql error: {e}\nSQL: {q}',file=sys.stderr); sys.exit(1)
finally:
    con.close()
'''


def run(cmd: list[str], *, cwd: Path, env: dict[str, str], check: bool = True) -> subprocess.CompletedProcess[str]:
    if env.get("PRAECTOR_SMOKE_VERBOSE"):
        print("+", " ".join(cmd), "[cwd=", cwd, "]", file=sys.stderr, flush=True)
    p = subprocess.run(cmd, cwd=cwd, env=env, text=True, capture_output=True)
    if check and p.returncode != 0:
        raise RuntimeError(
            f"command failed ({p.returncode}): {' '.join(cmd)}\nstdout:\n{p.stdout}\nstderr:\n{p.stderr}"
        )
    return p


def executable(path: Path, text: str) -> None:
    path.write_text(text, encoding="utf-8")
    path.chmod(path.stat().st_mode | stat.S_IXUSR | stat.S_IXGRP | stat.S_IXOTH)


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--praector", required=True, type=Path)
    ns = ap.parse_args()
    praector = ns.praector.resolve()
    if not praector.is_file():
        raise SystemExit(f"native binary not found: {praector}")
    git = shutil.which("git")
    if not git:
        raise SystemExit("git is required for native smoke")

    with tempfile.TemporaryDirectory(prefix="praector-native-smoke-") as td:
        root = Path(td)
        repo = root / "repo"
        home = root / "praector-home"
        bin_dir = root / "bin"
        repo.mkdir(); bin_dir.mkdir(); home.mkdir()
        executable(bin_dir / "dolt", FAKE_DOLT)
        executable(bin_dir / "opencode", "#!/bin/sh\nexit 0\n")

        env = os.environ.copy()
        env["PRAECTOR_HOME"] = str(home)
        env["PATH"] = str(bin_dir) + os.pathsep + env.get("PATH", "")

        run([git, "init", "-b", "main"], cwd=repo, env=env)
        run([git, "config", "user.email", "praector-smoke@example.invalid"], cwd=repo, env=env)
        run([git, "config", "user.name", "Praector Smoke"], cwd=repo, env=env)
        (repo / "README.md").write_text("# smoke\n", encoding="utf-8")
        run([git, "add", "README.md"], cwd=repo, env=env)
        run([git, "commit", "-m", "Initial"], cwd=repo, env=env)

        init = run([str(praector), "init", "--name", "native-smoke", "--opencode", str(bin_dir / "opencode")], cwd=repo, env=env)
        if "Initialized Praector project native-smoke" not in init.stdout:
            raise RuntimeError(f"unexpected init output: {init.stdout!r}")
        if not (repo / ".praector" / "project.toml").is_file():
            raise RuntimeError("praector init did not create .praector/project.toml")

        created = run(
            [str(praector), "run", "Analyze README.md wording and report findings", "--detach"],
            cwd=repo,
            env=env,
        )
        m = re.search(r"Created (COL-[0-9a-f]+)", created.stdout)
        if not m:
            raise RuntimeError(f"collection id missing from run output: {created.stdout!r}")
        cid = m.group(1)
        if "Queued. Run `praector daemon`" not in created.stdout:
            raise RuntimeError(f"detach contract missing: {created.stdout!r}")

        status = run([str(praector), "status", cid], cwd=repo, env=env)
        if cid not in status.stdout or "RUNNING" not in status.stdout:
            raise RuntimeError(f"unexpected status output: {status.stdout!r}")
        graph = run([str(praector), "graph", cid], cwd=repo, env=env)
        if cid not in graph.stdout or "coordinator" not in graph.stdout:
            raise RuntimeError(f"unexpected graph output: {graph.stdout!r}")
    print("Native executable black-box smoke passed")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
