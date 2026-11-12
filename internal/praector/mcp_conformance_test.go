package praector

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestMCPExternalConformanceOracle is the independent black-box interoperability
// oracle required for the 0.2.1 hardening revision. Praector's in-process protocol
// tests are necessary but not sufficient for an external protocol, so this drives
// the SAME loopback MCP listener a running daemon hosts (built by startMCPServer,
// the daemon's sole construction gate, with no scheduler and no process launch)
// from an EXTERNAL client — a stdlib-only Python MCP client script that is
// independent of Praector's own Go MCP core.
//
// Against the live loopback listener the external client verifies protocol
// negotiation, tools/list, resources/list/read (with accurate content types), a
// safe read-only tool call, and a safe durable queue-write that records a real
// task through the shared Service/Store path.
//
// If python3 is not installed the oracle skips with an explicit reason rather than
// silently passing; the script remains documented-runnable by hand against a live
// daemon (see scripts/mcp_conformance.py).
func TestMCPExternalConformanceOracle(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available; skipping the external MCP conformance oracle (run scripts/mcp_conformance.py against a live daemon by hand)")
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "scripts", "mcp_conformance.py"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("conformance oracle script missing: %v", err)
	}

	s, cfg := newFakeStore(t)
	svc := &Service{store: s, cfg: cfg}
	p, err := s.RegisterProject(testProjectConfig(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	_, coord, err := s.CreateCollectionWithCoordinator(S(p, "id"), "Conformance", "Probe the MCP surface", "Plan")
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("PRAECTOR_MCP", "127.0.0.1:0")
	srv, err := startMCPServer(svc)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	defer srv.Close()
	url := "http://" + ln.Addr().String() + "/mcp"

	cmd := exec.Command(python, script, "--url", url, "--caller", S(coord, "id"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("external conformance oracle failed: %v\n%s", err, out)
	} else {
		t.Log(string(out))
	}
}
