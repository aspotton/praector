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
con=sqlite3.connect(db)
con.row_factory=sqlite3.Row
try:
    sql=xform(q)
    if 'CREATE TABLE IF NOT EXISTS' in sql:
        con.executescript(schema_xform(sql)); con.commit(); rows=[]
    else:
        # Store batches mutations with semicolons. SELECT calls are single statements.
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
