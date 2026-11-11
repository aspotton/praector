package praector

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestInterspersedOptions(t *testing.T) {
	p, e := parseArgs([]string{"COL-1", "--dry-run", "--base", "main", "--depends-on=T-1", "--depends-on", "T-2"}, map[string]bool{"dry-run": false, "base": true, "depends-on": true})
	if e != nil {
		t.Fatal(e)
	}
	if len(p.Pos) != 1 || p.Pos[0] != "COL-1" || !p.bool("dry-run") || p.str("base") != "main" || len(p.strs("depends-on")) != 2 {
		t.Fatalf("bad parse: %#v", p)
	}
}
func TestProfileShowEffectiveOrder(t *testing.T) {
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	code := cmdProfile([]string{"show", "implement"})
	_ = w.Close()
	os.Stdout = old
	var b bytes.Buffer
	_, _ = b.ReadFrom(r)
	out := b.String()
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	a := strings.Index(out, "# Global engineering rules")
	z := strings.Index(out, "# Implementation worker operating contract")
	if a < 0 || z < 0 || a >= z {
		t.Fatalf("unexpected output order")
	}
}

func captureStdout(t *testing.T, fn func() int) (int, string) {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	code := fn()
	_ = w.Close()
	os.Stdout = old
	var b bytes.Buffer
	_, _ = b.ReadFrom(r)
	return code, b.String()
}

func TestNestedHelpDoesNotInitializeRuntime(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no dolt/git/opencode available
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"collection", "--help"}, "collection {show|revise|diff|pr|accept|cleanup}"},
		{[]string{"collection", "pr", "--help"}, "collection pr COL-ID"},
		{[]string{"task", "request", "--help"}, "task request --profile PROFILE"},
		{[]string{"result", "--help"}, "result {show|submit}"},
		{[]string{"memory", "add", "--help"}, "memory add --text TEXT"},
		{[]string{"daemon", "--help"}, "daemon [--once]"},
	}
	for _, tc := range cases {
		code, out := captureStdout(t, func() int { return Main(tc.args) })
		if code != 0 {
			t.Fatalf("%v exit=%d", tc.args, code)
		}
		if !strings.Contains(out, tc.want) {
			t.Fatalf("%v output %q does not contain %q", tc.args, out, tc.want)
		}
	}
}
