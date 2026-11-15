package praector

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMCPServerConfigAllowExternalMatrix proves the [mcp] allow_external opt-in is
// default-OFF and follows the same two-knob precedence as privileged: the file key
// sets it, a truthy env override forces it on, a falsey env forces it off, and any
// other env value leaves the file-derived value untouched.
func TestMCPServerConfigAllowExternalMatrix(t *testing.T) {
	cases := []struct {
		name     string
		fileBody string
		envValue string
		want     bool
	}{
		{"default off", "", "", false},
		{"file key sets it", "[mcp]\nallow_external = true\n", "", true},
		{"file key false stays off", "[mcp]\nallow_external = false\n", "", false},
		{"env truthy forces on", "", "1", true},
		{"env override wins over falsey file", "[mcp]\nallow_external = false\n", "true", true},
		{"env falsey forces off over true file", "[mcp]\nallow_external = true\n", "0", false},
		{"env other value leaves file value true", "[mcp]\nallow_external = true\n", "maybe", true},
		{"env other value leaves file value false", "[mcp]\nallow_external = false\n", "maybe", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("PRAECTOR_MCP", "")
			t.Setenv("PRAECTOR_MCP_ALLOW_EXTERNAL", tc.envValue)
			if tc.fileBody != "" {
				if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(tc.fileBody), 0644); err != nil {
					t.Fatal(err)
				}
			}
			cfg := LoadMCPServerConfig(GlobalConfig{Home: home})
			if cfg.AllowExternal != tc.want {
				t.Errorf("AllowExternal = %v, want %v", cfg.AllowExternal, tc.want)
			}
		})
	}
}

// TestMCPServerConfigForeignRefusal pins the fail-closed startup rule: a foreign
// PRAECTOR_MCP bind address with allow_external unset must still refuse to start,
// and the refusal error must name the opt-in so the operator knows how to enable it.
func TestMCPServerConfigForeignRefusal(t *testing.T) {
	s, cfg := newFakeStore(t)
	svc := &Service{store: s, cfg: cfg}
	t.Setenv("PRAECTOR_MCP", "0.0.0.0:9999")
	t.Setenv("PRAECTOR_MCP_ALLOW_EXTERNAL", "")
	_, err := startMCPServer(svc)
	if err == nil {
		t.Fatal("startMCPServer must refuse a foreign addr with allow_external unset")
	}
	if !strings.Contains(err.Error(), "allow_external") || !strings.Contains(err.Error(), "PRAECTOR_MCP_ALLOW_EXTERNAL") {
		t.Fatalf("refusal error must name the opt-in, got: %v", err)
	}
}

// TestMCPServerConfigStartForeignWithFlag proves the explicit opt-in lets the
// listener construct over a non-loopback addr (no process/serve is started here;
// NewLoopbackServer only refuses at construction).
func TestMCPServerConfigStartForeignWithFlag(t *testing.T) {
	s, cfg := newFakeStore(t)
	svc := &Service{store: s, cfg: cfg}
	t.Setenv("PRAECTOR_MCP", "0.0.0.0:0")
	t.Setenv("PRAECTOR_MCP_ALLOW_EXTERNAL", "1")
	srv, err := startMCPServer(svc)
	t.Cleanup(func() { svc.store.CloseEventSink() })
	if err != nil {
		t.Fatalf("allow_external=1 must permit a non-loopback bind: %v", err)
	}
	if srv.Addr != "0.0.0.0:0" {
		t.Fatalf("unexpected addr %q", srv.Addr)
	}
	srv.Close()
}

// TestMCPServerConfigBannerLoopback proves a loopback bind prints the historical
// "loopback only" banner line byte-identically.
func TestMCPServerConfigBannerLoopback(t *testing.T) {
	lines := mcpBannerLines("127.0.0.1:8765")
	want := []string{"MCP listener enabled on 127.0.0.1:8765 (loopback only)"}
	if len(lines) != len(want) || lines[0] != want[0] {
		t.Fatalf("loopback banner must be byte-identical; got %q", lines)
	}
}

// TestMCPServerConfigBannerNonLoopback proves a non-loopback bind prints the
// enabled line plus a loud WARNING with the reachability, privileged-off,
// bridge-IP-over-0.0.0.0, and public-tunnel guidance, framed as CSRF/DNS-rebinding
// defense (make smoke never starts the listener, so this Go test is the only cover).
func TestMCPServerConfigBannerNonLoopback(t *testing.T) {
	lines := mcpBannerLines("0.0.0.0:8765")
	if len(lines) < 2 {
		t.Fatalf("non-loopback banner must add a WARNING line, got %q", lines)
	}
	if lines[0] != "MCP listener enabled on 0.0.0.0:8765" {
		t.Fatalf("unexpected enabled line: %q", lines[0])
	}
	warning := strings.Join(lines, "\n")
	for _, want := range []string{"WARNING", "UNAUTHENTICATED", "privileged", "bridge", "0.0.0.0", "tunnel", "CSRF"} {
		if !strings.Contains(warning, want) {
			t.Errorf("WARNING banner must contain %q; got:\n%s", want, warning)
		}
	}
}
