package praector

import (
	"github.com/datascape/praector/internal/assets"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// browserCapConfig builds a ProjectConfig with the browser capability resolved to the
// given on/off state and cdp_url, so every browser-decision assertion in this file is
// driven by the SAME BrowserGranted/BrowserCDPURL predicate the code uses.
func browserCapConfig(on bool, cdpURL string) ProjectConfig {
	models := map[string]string{}
	for _, n := range ProfileNames {
		models[n] = ""
	}
	return ProjectConfig{
		ProjectID: "PRJ-browser", Name: "browser", Root: t_temp(), BaseBranch: "main",
		Harness: "opencode", OpenCodeCommand: "opencode", Models: models,
		Limits:       DefaultLimits(),
		Capabilities: ProjectCapabilities{CurrentExternalResearch: true, Browser: on, explicit: true},
		CDPURL:       cdpURL,
	}
}

func t_temp() string { return os.TempDir() }

// The single named grant predicate must be exactly {research, implement}.
func TestBrowserGrantSetIsExactlyResearchImplement(t *testing.T) {
	if len(BrowserProfiles) != 2 || !BrowserProfiles["research"] || !BrowserProfiles["implement"] {
		t.Fatalf("BrowserProfiles must be exactly {research,implement}, got %v", BrowserProfiles)
	}
	cfg := browserCapConfig(true, DefaultCDPURL)
	for _, p := range ProfileNames {
		got := cfg.BrowserGranted(p)
		if got != BrowserProfiles[p] {
			t.Fatalf("profile %s: BrowserGranted=%v but BrowserProfiles=%v", p, got, BrowserProfiles[p])
		}
	}
	if cfg.BrowserGranted("review") {
		t.Fatal("review must never be granted browser access")
	}
	// A capability-off project grants nothing, for any profile.
	off := browserCapConfig(false, DefaultCDPURL)
	for _, p := range ProfileNames {
		if off.BrowserGranted(p) {
			t.Fatalf("capability-off project granted browser to %s", p)
		}
	}
}

// Capability config round-trips through both the project.toml marker and the durable
// config_json row, and a legacy marker with neither section defaults to off + the
// blessed default cdp_url.
func TestBrowserConfigRoundTripAndDefault(t *testing.T) {
	root := t.TempDir()
	models := map[string]string{}
	for _, n := range ProfileNames {
		models[n] = ""
	}
	cfg := ProjectConfig{ProjectID: "PRJ-b", Name: "b", Root: root, BaseBranch: "main", Harness: "opencode", OpenCodeCommand: "opencode", Models: models, Limits: DefaultLimits(), Capabilities: DefaultProjectCapabilities(), CDPURL: "http://127.0.0.1:9333"}
	cfg.Capabilities.Browser = true
	marker, err := WriteProjectMarker(cfg)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ReadProjectMarker(marker)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Capabilities.Browser {
		t.Fatal("browser=true must round-trip through the marker")
	}
	if got.BrowserCDPURL() != "http://127.0.0.1:9333" {
		t.Fatalf("cdp_url marker round-trip = %q", got.BrowserCDPURL())
	}
	// config_json passthrough (project show / MCP parity).
	row := ProjectConfigFromRow(Row{"id": "PRJ-b", "name": "b", "root_path": root, "base_branch": "main", "config": ProjectConfigJSON(got)})
	if !row.Capabilities.Browser {
		t.Fatal("browser=true must round-trip through config_json")
	}
	if row.BrowserCDPURL() != "http://127.0.0.1:9333" {
		t.Fatalf("cdp_url config_json round-trip = %q", row.BrowserCDPURL())
	}
	cj := ProjectConfigJSON(cfg)
	if B(M(cj, "capabilities"), "browser") != true {
		t.Fatalf("capabilities.browser missing from config_json projection: %v", cj)
	}
	if S(M(cj, "browser"), "cdp_url") != "http://127.0.0.1:9333" {
		t.Fatalf("browser.cdp_url must be a top-level config_json key, got %v", cj)
	}
	// Legacy marker with neither section defaults to off + the blessed default.
	legacy := ProjectConfigFromRow(Row{"id": "PRJ-old", "name": "old", "root_path": root, "base_branch": "main", "config_json": "{}"})
	if legacy.Capabilities.Browser {
		t.Fatal("legacy rows must default browser=false")
	}
	if legacy.BrowserCDPURL() != DefaultCDPURL {
		t.Fatalf("legacy default cdp_url = %q want %q", legacy.BrowserCDPURL(), DefaultCDPURL)
	}
}

// The default cdp_url constant is used identically in the off-default path and the
// on-but-[browser]-absent path.
func TestBrowserCDPURLDefaultsToBlessedConstant(t *testing.T) {
	off := browserCapConfig(false, "")
	if off.BrowserCDPURL() != DefaultCDPURL {
		t.Fatalf("off-default cdp_url = %q", off.BrowserCDPURL())
	}
	on := browserCapConfig(true, "")
	if on.BrowserCDPURL() != DefaultCDPURL {
		t.Fatalf("on-but-absent cdp_url = %q", on.BrowserCDPURL())
	}
}

// Capability-OFF inline config is byte-identical to today's baseline for every profile:
// no mcp entry is ever added when not granted.
func TestInlineConfigNoMcpWhenNotGranted(t *testing.T) {
	for _, p := range ProfileNames {
		cfg := inlineOpenCodeConfig(p, false, DefaultCDPURL)
		if _, ok := cfg["mcp"]; ok {
			t.Fatalf("profile %s: capability-off inline config must not add an mcp entry", p)
		}
	}
}

// The artifact directory lives strictly outside any worktree, so a granted read-only
// attempt's screenshots/probes cannot trip the worktree-scoped mutation check.
func TestArtifactsDirOutsideWorktree(t *testing.T) {
	g := GlobalConfig{Home: filepath.Join(os.TempDir(), "praector-browser-home")}
	ad := g.ArtifactsDir("T-abc", 3)
	if !strings.HasPrefix(ad, filepath.Join(g.Home, "artifacts")) {
		t.Fatalf("artifact dir not under home/artifacts: %s", ad)
	}
	if strings.HasPrefix(ad, g.WorktreesDir()) {
		t.Fatalf("artifact dir must not be under the worktrees dir: %s vs %s", ad, g.WorktreesDir())
	}
	if !strings.HasSuffix(ad, filepath.Join("T-abc", "attempt-3")) {
		t.Fatalf("artifact dir path shape wrong: %s", ad)
	}
}

// Both new env names are part of the documented worker contract so the config-candidate
// gate does not surface them as introduced-but-never-read knobs.
func TestBrowserEnvVarsWhitelisted(t *testing.T) {
	if !commonEnvVars["PRAECTOR_CDP_URL"] || !commonEnvVars["PRAECTOR_ARTIFACT_DIR"] {
		t.Fatal("PRAECTOR_CDP_URL and PRAECTOR_ARTIFACT_DIR must be in commonEnvVars")
	}
}

// The HARNESS header carries browser_cdp on every attempt, derived from the grant
// argument (not the environment): off when not granted, the url when granted, including
// for non-granted profiles which still emit browser_cdp=off.
func TestHarnessHeaderBrowserCDPAudit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fake OpenCode script")
	}
	dir := t.TempDir()
	fake := filepath.Join(dir, "opencode")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nprintf '{\"type\":\"text\",\"text\":\"ok\"}'\n"), 0755); err != nil {
		t.Fatal(err)
	}
	h, err := NewOpenCodeHarness(fake)
	if err != nil {
		t.Fatal(err)
	}
	run := func(granted bool, cdp string) string {
		logPath := filepath.Join(dir, "run.log")
		if _, err := h.RunTask("prompt", dir, "research", "", map[string]string{"PRAECTOR_TASK_ID": "T-a"}, granted, cdp, logPath, 20*time.Second, nil); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(logPath)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	off := run(false, DefaultCDPURL)
	if !strings.Contains(off, "\nbrowser_cdp=off\n") {
		t.Fatalf("non-granted header must carry browser_cdp=off; got:\n%s", off)
	}
	// Existing header fields stay present (byte-identity floor is existing fields + suffix).
	for _, want := range []string{"prompt_transport=stdin", "praector_control="} {
		if !strings.Contains(off, want) {
			t.Fatalf("header lost existing field %q:\n%s", want, off)
		}
	}
	on := run(true, "http://127.0.0.1:9444")
	if !strings.Contains(on, "\nbrowser_cdp=http://127.0.0.1:9444\n") {
		t.Fatalf("granted header must carry browser_cdp=<url>; got:\n%s", on)
	}
}

// A stray PRAECTOR_CDP_URL inherited from the parent environment can never forge an "on"
// environment for a non-granted attempt: the harness strips it.
func TestStrayCDPURLStrippedForNonGranted(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fake OpenCode script")
	}
	dir := t.TempDir()
	fake := filepath.Join(dir, "opencode")
	// The fake echoes whether PRAECTOR_CDP_URL survived into the child environment.
	script := "#!/bin/sh\nif [ -n \"$PRAECTOR_CDP_URL\" ]; then printf '{\"type\":\"text\",\"text\":\"leak\"}'; else printf '{\"type\":\"text\",\"text\":\"clean\"}'; fi\n"
	if err := os.WriteFile(fake, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PRAECTOR_SANITIZE_PARENT_VENV", "false")
	t.Setenv("PRAECTOR_CDP_URL", "http://127.0.0.1:6666")
	h, err := NewOpenCodeHarness(fake)
	if err != nil {
		t.Fatal(err)
	}
	res, err := h.RunTask("p", dir, "review", "", map[string]string{"PRAECTOR_TASK_ID": "T-x"}, false, DefaultCDPURL, filepath.Join(dir, "r.log"), 20*time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.FinalText != "clean" {
		t.Fatalf("a stray parent PRAECTOR_CDP_URL leaked into a non-granted attempt: %q", res.FinalText)
	}
}

// The embedded browser-evidence prompt contracts must require MOTION evidence (a
// tick-stepped frame sequence plus a per-tick state trace, not a single frame) in
// implement.md, and must flag a single-frame motion claim as a finding in review.md.
// Both sentences are pinned against the EMBEDDED assets (assets.FS), not the on-disk
// files, so a build/embedding regression fails here rather than shipping a regressed prompt.
func TestBrowserEvidencePromptContract(t *testing.T) {
	impl, err := assets.FS.ReadFile("prompts/implement.md")
	if err != nil {
		t.Fatal(err)
	}
	implText := string(impl)
	for _, want := range []string{
		"A single frame proves placement, never motion.",
		"tick-stepped frame sequence",
		"per-tick state trace",
	} {
		if !strings.Contains(implText, want) {
			t.Fatalf("embedded implement.md browser-evidence motion clause missing %q", want)
		}
	}
	rev, err := assets.FS.ReadFile("prompts/review.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rev), "is a finding while its implementation made that motion/animation claim and had browser access") {
		t.Fatal("embedded review.md must carry the browser-free single-frame-is-a-findings motion clause")
	}
}
