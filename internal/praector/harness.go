package praector

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

type HarnessResult struct {
	ExitCode          int
	Stdout            string
	Stderr            string
	FinalText         string
	ExternalSessionID string
	TimedOut          bool
}

type Harness interface {
	RunTask(prompt, workdir, profile, model string, env map[string]string, logPath string, timeout time.Duration, onStart func(int)) (HarnessResult, error)
}

type OpenCodeHarness struct{ Command string }

func NewOpenCodeHarness(command string) (*OpenCodeHarness, error) {
	p, err := RequireBinary(command)
	if err != nil {
		return nil, err
	}
	return &OpenCodeHarness{Command: p}, nil
}

func CreateHarness(name, openCodeCommand string) (Harness, error) {
	if strings.EqualFold(strings.TrimSpace(name), "opencode") {
		return NewOpenCodeHarness(openCodeCommand)
	}
	return nil, fmt.Errorf("Unsupported harness %q. Praector currently supports only: opencode", name)
}

func recursiveSessionID(v any) string {
	switch x := v.(type) {
	case map[string]any:
		for _, key := range []string{"sessionID", "session_id", "sessionId"} {
			if s, ok := x[key].(string); ok && s != "" {
				return s
			}
		}
		for _, child := range x {
			if s := recursiveSessionID(child); s != "" {
				return s
			}
		}
	case []any:
		for _, child := range x {
			if s := recursiveSessionID(child); s != "" {
				return s
			}
		}
	}
	return ""
}

func extractText(v any, out *[]string) {
	switch x := v.(type) {
	case map[string]any:
		if s, ok := x["text"].(string); ok {
			*out = append(*out, s)
		}
		for key, child := range x {
			if key != "text" {
				extractText(child, out)
			}
		}
	case []any:
		for _, child := range x {
			extractText(child, out)
		}
	}
}

func envMap(source []string) map[string]string {
	out := map[string]string{}
	for _, item := range source {
		if k, v, ok := strings.Cut(item, "="); ok {
			out[k] = v
		}
	}
	return out
}

func envList(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	return out
}

func cleanInheritedWorkerEnv(source map[string]string) map[string]string {
	cleaned := map[string]string{}
	for k, v := range source {
		cleaned[k] = v
	}
	active := cleaned["VIRTUAL_ENV"]
	delete(cleaned, "VIRTUAL_ENV")
	delete(cleaned, "VIRTUAL_ENV_PROMPT")
	if active != "" {
		bin := "bin"
		if runtime.GOOS == "windows" {
			bin = "Scripts"
		}
		cleaned["PATH"] = PathListWithout(cleaned["PATH"], filepath.Join(active, bin))
	}
	return cleaned
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	_, cpErr := io.Copy(out, in)
	closeErr := out.Close()
	if cpErr != nil {
		_ = os.Remove(tmp)
		return cpErr
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return closeErr
	}
	if err := os.Chmod(tmp, mode); err != nil && runtime.GOOS != "windows" {
		_ = os.Remove(tmp)
		return err
	}
	_ = os.Remove(dst)
	return os.Rename(tmp, dst)
}

func installPraectorControlShim(processEnv map[string]string, taskEnv map[string]string) string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	exe, _ = filepath.Abs(exe)
	home := taskEnv["PRAECTOR_HOME"]
	if home == "" {
		home = processEnv["PRAECTOR_HOME"]
	}
	if home == "" {
		return exe
	}
	dir := filepath.Join(home, "control-bin")
	_ = os.MkdirAll(dir, 0755)
	name := "praector"
	if runtime.GOOS == "windows" {
		name = "praector.exe"
	}
	shim := filepath.Join(dir, name)
	if runtime.GOOS != "windows" {
		_ = os.Remove(shim)
		if err := os.Symlink(exe, shim); err != nil {
			_ = copyFile(exe, shim, 0755)
		}
	} else {
		needCopy := true
		if a, e1 := os.Stat(exe); e1 == nil {
			if b, e2 := os.Stat(shim); e2 == nil && a.Size() == b.Size() && !a.ModTime().After(b.ModTime()) {
				needCopy = false
			}
		}
		if needCopy {
			_ = copyFile(exe, shim, 0755)
		}
	}
	parts := filepath.SplitList(processEnv["PATH"])
	filtered := []string{dir}
	for _, p := range parts {
		if !strings.EqualFold(filepath.Clean(p), filepath.Clean(dir)) && p != "" {
			filtered = append(filtered, p)
		}
	}
	processEnv["PATH"] = strings.Join(filtered, string(os.PathListSeparator))
	processEnv["PRAECTOR_CLI"] = shim
	return shim
}

func inlineOpenCodeConfig(profile string) Row {
	permission := Row{
		"task":               "deny",
		"question":           "deny",
		"external_directory": "deny",
	}
	if ReadOnlyProfiles[profile] {
		permission["edit"] = "deny"
	} else {
		permission["edit"] = "allow"
	}
	permission["bash"] = Row{"*": "allow", "git push*": "deny"}
	return Row{
		"$schema":    "https://opencode.ai/config.json",
		"permission": permission,
		"agent":      Row{"build": Row{"permission": permission}},
	}
}

func deepMergeAny(base, over any) any {
	bm, bok := base.(map[string]any)
	if !bok {
		if br, ok := base.(Row); ok {
			bm = map[string]any(br)
			bok = true
		}
	}
	om, ook := over.(map[string]any)
	if !ook {
		if or, ok := over.(Row); ok {
			om = map[string]any(or)
			ook = true
		}
	}
	if !bok || !ook {
		return over
	}
	out := map[string]any{}
	for k, v := range bm {
		out[k] = v
	}
	for k, v := range om {
		if prior, ok := out[k]; ok {
			out[k] = deepMergeAny(prior, v)
		} else {
			out[k] = v
		}
	}
	return out
}

func (h *OpenCodeHarness) RunTask(prompt, workdir, profile, model string, taskEnv map[string]string, logPath string, timeout time.Duration, onStart func(int)) (HarnessResult, error) {
	sanitize := BoolEnv("PRAECTOR_SANITIZE_PARENT_VENV", true)
	parent := envMap(os.Environ())
	processEnv := parent
	if sanitize {
		processEnv = cleanInheritedWorkerEnv(parent)
	} else {
		processEnv = map[string]string{}
		for k, v := range parent {
			processEnv[k] = v
		}
	}
	for k, v := range taskEnv {
		processEnv[k] = v
	}
	control := installPraectorControlShim(processEnv, taskEnv)
	processEnv["PWD"] = workdir

	var injected any = inlineOpenCodeConfig(profile)
	if raw := processEnv["OPENCODE_CONFIG_CONTENT"]; strings.TrimSpace(raw) != "" {
		var existing any
		if json.Unmarshal([]byte(raw), &existing) == nil {
			injected = deepMergeAny(existing, injected)
		}
	}
	b, _ := json.Marshal(injected)
	processEnv["OPENCODE_CONFIG_CONTENT"] = string(b)

	args := []string{"run", "--pure", "--format", "json", "--auto", "--agent", "build", "--title", "praector " + firstNonEmpty(taskEnv["PRAECTOR_TASK_ID"], "task"), "--dir", workdir}
	if strings.TrimSpace(model) != "" {
		args = append(args, "--model", model)
	}
	args = append(args, prompt)

	ctx := context.Background()
	cmd := exec.CommandContext(ctx, h.Command, args...)
	cmd.Dir = workdir
	cmd.Env = envList(processEnv)
	configureChildProcess(cmd)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := os.MkdirAll(filepath.Dir(logPath), 0755); err != nil {
		return HarnessResult{}, err
	}
	if err := cmd.Start(); err != nil {
		return HarnessResult{}, err
	}
	tree, attachErr := attachProcessTree(cmd)
	if attachErr != nil {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		return HarnessResult{}, attachErr
	}
	defer closeProcessTree(tree)
	if onStart != nil {
		onStart(cmd.Process.Pid)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	timedOut := false
	var waitErr error
	if timeout <= 0 {
		waitErr = <-done
	} else {
		select {
		case waitErr = <-done:
		case <-time.After(timeout):
			timedOut = true
			terminateProcessTree(cmd, tree)
			select {
			case waitErr = <-done:
			case <-time.After(10 * time.Second):
				waitErr = fmt.Errorf("process did not exit after timeout termination")
			}
		}
	}

	exitCode := 0
	if timedOut {
		exitCode = 124
	} else if waitErr != nil {
		if ee, ok := waitErr.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else {
			exitCode = 1
		}
	}

	hash := sha256.Sum256([]byte(prompt))
	header := fmt.Sprintf("# HARNESS\ncommand=%s\nprofile=%s\nworkdir=%s\nmodel=%s\nprompt_bytes=%d\nprompt_sha256=%s\nsanitize_parent_venv=%t\nvirtual_env=%s\npraector_control=%s\n", h.Command, profile, workdir, model, len([]byte(prompt)), hex.EncodeToString(hash[:]), sanitize, processEnv["VIRTUAL_ENV"], control)
	_ = os.WriteFile(logPath, []byte(header+"# STDOUT\n"+stdout.String()+"\n# STDERR\n"+stderr.String()), 0644)

	sessionID := ""
	fragments := []string{}
	for _, line := range strings.Split(stdout.String(), "\n") {
		var event any
		if json.Unmarshal([]byte(line), &event) != nil {
			continue
		}
		if sessionID == "" {
			sessionID = recursiveSessionID(event)
		}
		extractText(event, &fragments)
	}
	finalParts := []string{}
	for _, text := range fragments {
		if len(finalParts) == 0 || finalParts[len(finalParts)-1] != text {
			finalParts = append(finalParts, text)
		}
	}
	finalText := strings.TrimSpace(stdout.String())
	if len(finalParts) > 0 {
		finalText = finalParts[len(finalParts)-1]
	}
	return HarnessResult{ExitCode: exitCode, Stdout: stdout.String(), Stderr: stderr.String(), FinalText: finalText, ExternalSessionID: sessionID, TimedOut: timedOut}, nil
}
