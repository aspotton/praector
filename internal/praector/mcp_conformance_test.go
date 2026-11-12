package praector

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestMCPExternalConformanceOracle is an ADDITIONAL non-authoritative black-box
// interoperability probe. It is NOT the official conformance oracle: the
// authoritative independent client path is TestMCPOfficialClientInteroperability
// (an official Inspector CLI / Tier-1 SDK client). This probe drives the SAME
// loopback MCP listener a running daemon hosts (built by startMCPServer, the
// daemon's sole construction gate, with no scheduler and no process launch) from
// an EXTERNAL stdlib-only Python client that is independent of Praector's own Go
// MCP core, exercising protocol negotiation, tools/list, resources/list/read (with
// accurate content types), a safe read-only tool call, and a safe durable
// queue-write that records a real task through the shared Service/Store path.
//
// Because the client is implementation-owned, it is a black_box_test and is
// insufficient by itself as authoritative external conformance; it is retained for
// its fast, dependency-free coverage of the live transport. If python3 is not
// installed the probe skips with an explicit reason; the script remains
// documented-runnable by hand against a live daemon (scripts/mcp_conformance.py).
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

// TestMCPOfficialClientInteroperability is the AUTHORITATIVE independent
// conformance path: it drives the SAME loopback MCP listener a running daemon
// hosts (startMCPServer, no scheduler/no process launch) from an official MCP
// client — the Inspector CLI (`npx @modelcontextprotocol/inspector --cli
// --transport http`) or a Tier-1 official SDK client — rather than only by
// Praector's own in-process Go tests or the Praector-owned black-box script. It
// ACTUALLY exercises every required surface as a separate Inspector `--method`
// invocation against the live loopback listener, each preceded by an implicit
// initialize/version-negotiation handshake:
//
//	initialize/version negotiation (implicit on every request),
//	tools/list, resources/list, resources/read,
//	one safe read tool call (praector_project_list), and
//	one safe durable queue-write (praector_task_request) recording a real task
//	through the shared Service/Store path.
//
// It is deliberately OFF the default `go test ./...` path so it never requires a
// network fetch or raises the go toolchain floor: set PRAECTOR_MCP_INSPECTOR to a
// runnable official-client command to execute it, e.g.
//
//	PRAECTOR_MCP_INSPECTOR="npx -y @modelcontextprotocol/inspector --cli --transport http" \
//	  go test ./internal/praector -run TestMCPOfficialClientInteroperability
//
// Each surface is run as `<command> <url> <surface-args>`; every invocation must
// exit successfully. When PRAECTOR_MCP_INSPECTOR is unset the test SKIPS with an
// explicit reason rather than silently passing or pretending the in-process tests
// are authoritative. The official client is never a runtime module dependency;
// scripts/mcp_conformance.py remains an additional non-authoritative black-box
// probe (see TestMCPExternalConformanceOracle), not this oracle.
func TestMCPOfficialClientInteroperability(t *testing.T) {
	inspector := strings.TrimSpace(os.Getenv("PRAECTOR_MCP_INSPECTOR"))
	if inspector == "" {
		t.Skip("no official MCP client configured; set PRAECTOR_MCP_INSPECTOR to the Inspector CLI or a Tier-1 SDK command to run the authoritative interoperability oracle (documented-runnable by hand against a live daemon)")
	}

	s, cfg := newFakeStore(t)
	svc := &Service{store: s, cfg: cfg}
	p, err := s.RegisterProject(testProjectConfig(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	_, coord, err := s.CreateCollectionWithCoordinator(S(p, "id"), "Interop", "Official client probe", "Plan")
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

	fields := strings.Fields(inspector)
	run := func(name string, extra ...string) {
		t.Helper()
		args := append(append([]string{}, fields[1:]...), url)
		args = append(args, extra...)
		cmd := exec.Command(fields[0], args...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("official MCP client failed on %s (%v): %s\n%s", name, err, strings.Join(args, " "), out)
		} else {
			t.Logf("official client %s output:\n%s", name, out)
		}
	}

	// Surface 1+2: tools/list (implicit initialize/version negotiation + tool set).
	run("tools/list", "--method", "tools/list")
	// Surface 3: resources/list.
	run("resources/list", "--method", "resources/list")
	// Surface 4: resources/read against a registered-projects resource.
	run("resources/read", "--method", "resources/read", "--uri", "praector://projects")
	// Surface 5: one safe read tool call.
	run("read-tool", "--method", "tools/call", "--tool-name", "praector_project_list")
	// Surface 6: one safe durable queue-write that records a real task.
	run("queue-write", "--method", "tools/call", "--tool-name", "praector_task_request",
		"--tool-arg", "caller="+S(coord, "id"),
		"--tool-arg", "profile=research",
		"--tool-arg", "title=Official client probe",
		"--tool-arg", "objective=Determine X with evidence",
		"--tool-arg", "reason=authoritative official-client queue-write conformance probe",
	)
}
