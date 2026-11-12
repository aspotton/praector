package praector

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestMCPServerConfigEnablePrivilegedMatrix proves that listener enablement and
// privileged-write exposure are completely independent opt-ins, across BOTH the
// config-file and environment-override paths. In particular, privileged=true must
// NEVER start the listener: Enabled is governed solely by [mcp] enabled /
// PRAECTOR_MCP.
func TestMCPServerConfigEnablePrivilegedMatrix(t *testing.T) {
	cases := []struct {
		name        string
		fileBody    string
		envEnabled  string
		envPriv     string
		wantEnabled bool
		wantPriv    bool
	}{
		{"env disabled+unprivileged", "", "0", "0", false, false},
		{"env disabled+privileged", "", "0", "1", false, true},
		{"env enabled+unprivileged", "", "1", "0", true, false},
		{"env enabled+privileged", "", "1", "1", true, true},
		{"file disabled+unprivileged", "[mcp]\nenabled = false\nprivileged = false\n", "", "", false, false},
		{"file disabled+privileged", "[mcp]\nenabled = false\nprivileged = true\n", "", "", false, true},
		{"file only privileged no enable", "[mcp]\nprivileged = true\n", "", "", false, true},
		{"file enabled+unprivileged", "[mcp]\nenabled = true\nprivileged = false\n", "", "", true, false},
		{"file enabled+privileged", "[mcp]\nenabled = true\nprivileged = true\n", "", "", true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("PRAECTOR_MCP", tc.envEnabled)
			t.Setenv("PRAECTOR_MCP_PRIVILEGED", tc.envPriv)
			if tc.fileBody != "" {
				if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(tc.fileBody), 0644); err != nil {
					t.Fatal(err)
				}
			}
			cfg := LoadMCPServerConfig(GlobalConfig{Home: home})
			if cfg.Enabled != tc.wantEnabled {
				t.Errorf("Enabled = %v, want %v", cfg.Enabled, tc.wantEnabled)
			}
			if cfg.Privileged != tc.wantPriv {
				t.Errorf("Privileged = %v, want %v", cfg.Privileged, tc.wantPriv)
			}
		})
	}
}

// TestMCPServerConfigCanonicalEnvOnly proves the canonical PRAECTOR_MCP_PRIVILEGED
// opt-in is honored and the misspelled PRACTOR_MCP_PRIVILEGED (single A) has no
// effect: Praector retains no backwards-compatibility alias for the misspelling.
func TestMCPServerConfigCanonicalEnvOnly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PRAECTOR_MCP", "")
	t.Setenv("PRAECTOR_MCP_PRIVILEGED", "")
	t.Setenv("PRACTOR_MCP_PRIVILEGED", "1")
	cfg := LoadMCPServerConfig(GlobalConfig{Home: home})
	if cfg.Privileged || cfg.Enabled {
		t.Fatalf("misspelled PRACTOR_MCP_PRIVILEGED must have no effect, got %+v", cfg)
	}
	// The canonical form still works.
	t.Setenv("PRACTOR_MCP_PRIVILEGED", "")
	t.Setenv("PRAECTOR_MCP", "1")
	cfg = LoadMCPServerConfig(GlobalConfig{Home: home})
	if !cfg.Enabled || cfg.Privileged {
		t.Fatalf("canonical env unexpectedly privileged, got %+v", cfg)
	}
}

func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "init", "-q")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init in %s: %v: %s", dir, err, out)
	}
}

// TestMCPInitIsTargetScoped proves an MCP praector_init call operates only on the
// explicitly requested root_path and never on the daemon process working directory.
func TestMCPInitIsTargetScoped(t *testing.T) {
	installFakeDolt(t)
	t.Setenv("PRAECTOR_HOME", t.TempDir())

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(cwd) }()

	// A clean "daemon" repo stands in for the daemon process working directory.
	daemonDir := t.TempDir()
	initGitRepo(t, daemonDir)
	if err := os.Chdir(daemonDir); err != nil {
		t.Fatal(err)
	}

	s, cfg := newFakeStore(t)
	svc := &Service{store: s, cfg: cfg}
	on := &mcpBridge{svc: svc, privileged: true}

	// A distinct target repo (different from the daemon cwd) to register.
	targetDir := t.TempDir()
	initGitRepo(t, targetDir)

	args, _ := json.Marshal(map[string]any{"root_path": targetDir})
	if _, err := on.CallTool("praector_init", args); err != nil {
		t.Fatalf("privileged MCP init of target failed: %v", err)
	}

	if _, err := os.Stat(filepath.Join(targetDir, ".praector", "project.toml")); err != nil {
		t.Fatalf("target repo was not initialized: %v", err)
	}
	if _, err := os.Stat(filepath.Join(daemonDir, ".praector", "project.toml")); err == nil {
		t.Fatal("MCP init must not write a marker into the daemon process cwd")
	}
}

// TestMCPInitRejectsNonGitTarget proves an explicit root_path outside a Git work
// tree fails clearly rather than silently operating elsewhere.
func TestMCPInitRejectsNonGitTarget(t *testing.T) {
	installFakeDolt(t)
	t.Setenv("PRAECTOR_HOME", t.TempDir())
	s, cfg := newFakeStore(t)
	on := &mcpBridge{svc: &Service{store: s, cfg: cfg}, privileged: true}

	notRepo := t.TempDir()
	args, _ := json.Marshal(map[string]any{"root_path": notRepo})
	_, err := on.CallTool("praector_init", args)
	if err == nil || !strings.Contains(err.Error(), "Git work tree") {
		t.Fatalf("expected a clear non-git-work-tree error, got %v", err)
	}
}

// TestMCPInitRefusedWhilePrivilegedDisabled proves praector_init is a privileged
// operation: it is refused while the privileged opt-in is off, without touching
// either the target or the daemon cwd.
func TestMCPInitRefusedWhilePrivilegedDisabled(t *testing.T) {
	installFakeDolt(t)
	t.Setenv("PRAECTOR_HOME", t.TempDir())
	s, cfg := newFakeStore(t)
	off := &mcpBridge{svc: &Service{store: s, cfg: cfg}, privileged: false}

	target := t.TempDir()
	initGitRepo(t, target)
	args, _ := json.Marshal(map[string]any{"root_path": target})
	if _, err := off.CallTool("praector_init", args); err == nil || !strings.Contains(err.Error(), "privileged") {
		t.Fatalf("disabled privileged MCP init must be refused: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, ".praector", "project.toml")); err == nil {
		t.Fatal("a refused privileged init must not write the target marker")
	}
}
