package praector

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProjectMarkerRoundTrip(t *testing.T) {
	root := t.TempDir()
	models := map[string]string{}
	for _, n := range ProfileNames {
		models[n] = ""
	}
	c := ProjectConfig{ProjectID: "PRJ-1", Name: "demo", Root: root, BaseBranch: "main", Harness: "opencode", OpenCodeCommand: "opencode", Models: models, Limits: DefaultLimits()}
	p, e := WriteProjectMarker(c)
	if e != nil {
		t.Fatal(e)
	}
	got, e := ReadProjectMarker(p)
	if e != nil {
		t.Fatal(e)
	}
	if got.ProjectID != c.ProjectID || got.Root != root || got.BaseBranch != "main" {
		t.Fatalf("round trip mismatch: %#v", got)
	}
	if _, e = os.Stat(filepath.Join(root, ".praector", "project.toml")); e != nil {
		t.Fatal(e)
	}
}
func TestProfileTaxonomy(t *testing.T) {
	want := []string{"coordinator", "critic", "plan_review", "research", "implement", "review", "advise"}
	if len(ProfileNames) != len(want) {
		t.Fatal(ProfileNames)
	}
	for i := range want {
		if ProfileNames[i] != want[i] {
			t.Fatalf("%v", ProfileNames)
		}
	}
	if ProfileTitles["coordinator"] != "Coordinator / Planner" || ProfileTitles["review"] != "Implementation Reviewer" {
		t.Fatalf("profile titles drifted: %v", ProfileTitles)
	}
}

func TestGlobalUsesCanonicalHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PRAECTOR_HOME", "")
	cfg, err := Global()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".praector")
	if cfg.Home != want {
		t.Fatalf("expected canonical home %s, got %s", want, cfg.Home)
	}
}

func TestGlobalUsesPraectorHomeOverride(t *testing.T) {
	home := t.TempDir()
	custom := filepath.Join(home, "custom-praector")
	t.Setenv("PRAECTOR_HOME", custom)
	cfg, err := Global()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Home != custom {
		t.Fatalf("expected PRAECTOR_HOME %s, got %s", custom, cfg.Home)
	}
}

func TestFindProjectMarkerUsesPraectorNamespace(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(nested, 0755); err != nil {
		t.Fatal(err)
	}
	if got, ok := FindProjectMarker(nested); ok || got != "" {
		t.Fatalf("expected no project marker, got %s ok=%v", got, ok)
	}
	canonicalDir := filepath.Join(root, ".praector")
	if err := os.MkdirAll(canonicalDir, 0755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(canonicalDir, "project.toml")
	if err := os.WriteFile(marker, []byte("version = 1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	got, ok := FindProjectMarker(nested)
	if !ok || got != marker {
		t.Fatalf("expected canonical marker %s, got %s ok=%v", marker, got, ok)
	}
}
