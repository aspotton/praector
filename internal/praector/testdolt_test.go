package praector

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func installFakeDolt(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake Dolt test harness is POSIX-only; Windows is compile-checked")
	}
	dir := t.TempDir()
	script := `#!/usr/bin/python3
import json, os, re, sqlite3, sys
cwd=os.getcwd(); db=os.path.join(cwd,'.fake-dolt.sqlite3')
args=sys.argv[1:]
if not args: sys.exit(0)
cmd=args[0]
if cmd=='init': os.makedirs(os.path.join(cwd,'.dolt'),exist_ok=True); sys.exit(0)
if cmd in ('config','add','commit','status'): sys.exit(0)
if cmd!='sql': print('unsupported fake dolt command',args,file=sys.stderr); sys.exit(2)
# Invocation-count seam: when PRAECTOR_FAKE_DOLT_LOG is set, append one "sql ..."
# line per dolt sql invocation so tests can assert a bounded number of SQL
# processes during bootstrap, separated from add/commit (docs/PERFORMANCE.md
# PATH-shim grep -c '^sql' methodology).
count_path=os.environ.get('PRAECTOR_FAKE_DOLT_LOG')
if count_path:
    try:
        with open(count_path,'a') as cf: cf.write('sql '+' '.join(args[1:])+'\n')
    except Exception: pass
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
    s=s.replace('INSERT IGNORE','INSERT OR IGNORE')
    return s

def split_statements(s):
    # Literal-aware split: a ';' inside a single- or double-quoted string does not
    # terminate a statement, so a semicolon-joined batch of reads (which only embed
    # internal ids via SQLLiteral plus fixed literals) splits correctly, and real
    # Dolt's per-statement JSONL ordering is matched.
    parts=[]; buf=[]; quote=None; prev=''
    for ch in s:
        if quote:
            buf.append(ch)
            if ch==quote and prev!='\\': quote=None
        elif ch in ("'",'"'):
            quote=ch; buf.append(ch)
        elif ch==';':
            parts.append(''.join(buf)); buf=[]
        else:
            buf.append(ch)
        prev=ch
    tail=''.join(buf)
    if tail.strip(): parts.append(tail)
    return [p for p in parts if p.strip()]

con=sqlite3.connect(db)
con.row_factory=sqlite3.Row
try:
    sql=xform(q)
    if 'CREATE TABLE IF NOT EXISTS' in sql:
        con.executescript(schema_xform(sql)); con.commit()
    elif want_json:
        # Multi-statement JSONL read path: real Dolt 2.7.2 emits one JSON document
        # per statement in order, using {} for a zero-row statement. Store batches
        # reads through dolt sql -r json -q with semicolon-joined SELECTs.
        for st in split_statements(sql):
            if st.lstrip().upper().startswith('SELECT'):
                cur=con.execute(st); rows=[dict(r) for r in cur.fetchall()]
            else:
                con.execute(st); con.commit(); rows=[]
            print(json.dumps({'rows':rows},separators=(',',':')) if rows else '{}')
    else:
        # Mutation/DDL path (dolt sql without -r json): batched writes are executed
        # as a script; nothing is printed.
        con.executescript(sql); con.commit()
except Exception as e:
    print(f'fake dolt sql error: {e}\nSQL: {q}',file=sys.stderr); sys.exit(1)
finally:
    con.close()
`
	p := filepath.Join(dir, "dolt")
	if err := os.WriteFile(p, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	old := os.Getenv("PATH")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+old)
	return p
}

func newFakeStore(t *testing.T) (*Store, GlobalConfig) {
	t.Helper()
	installFakeDolt(t)
	cfg := GlobalConfig{Home: t.TempDir()}
	s, err := NewStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	return s, cfg
}
