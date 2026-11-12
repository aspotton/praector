package praector

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestOpenCodeHarnessStreamsLargePromptViaStdin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fake OpenCode script")
	}
	dir := t.TempDir()
	fake := filepath.Join(dir, "opencode")
	script := `#!/bin/sh
n=$(wc -c | tr -d ' ')
printf '{"type":"text","text":"bytes=%s"}\n' "$n"
`
	if err := os.WriteFile(fake, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	h, err := NewOpenCodeHarness(fake)
	if err != nil {
		t.Fatal(err)
	}
	prompt := "evidence:" + strings.Repeat("x", 512_000)
	logPath := filepath.Join(dir, "run.log")
	got, err := h.RunTask(prompt, dir, "critic", "", map[string]string{"PRAECTOR_TASK_ID": "T-large"}, logPath, 30*time.Second, nil)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if got.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", got.ExitCode, got.Stderr)
	}
	want := "bytes=" + strconv.Itoa(len([]byte(prompt)))
	if got.FinalText != want {
		t.Fatalf("FinalText=%q want %q", got.FinalText, want)
	}
	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	log := string(logBytes)
	if !strings.Contains(log, "prompt_transport=stdin") {
		t.Fatalf("log missing stdin transport marker: %s", log)
	}
	if !strings.Contains(log, "prompt_bytes="+strconv.Itoa(len([]byte(prompt)))) {
		t.Fatalf("log missing prompt size")
	}
}
