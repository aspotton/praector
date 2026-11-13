package praector

import (
	"github.com/datascape/praector/internal/assets"
	"strings"
	"testing"
)

func TestEmbeddedRuntimeAssets(t *testing.T) {
	schema, e := assets.FS.ReadFile("schema.sql")
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(string(schema), "CREATE TABLE IF NOT EXISTS collections") || !strings.Contains(string(schema), "collection_pull_requests") {
		t.Fatal("schema incomplete")
	}
	for _, p := range ProfileNames {
		b, e := assets.FS.ReadFile("prompts/" + p + ".md")
		if e != nil || len(b) < 300 {
			t.Fatalf("prompt %s missing/short", p)
		}
	}
	global, _ := assets.FS.ReadFile("prompts/global.md")
	if len(global) < 1200 {
		t.Fatal("global prompt too short")
	}
	envelope, err := assets.FS.ReadFile("envelope.md")
	if err != nil {
		t.Fatal(err)
	}
	envelopeText := string(envelope)
	if strings.Contains(envelopeText, "T-PLAN-REVIEW") {
		t.Fatal("task envelope must not teach fake dependency task IDs")
	}
	if !strings.Contains(envelopeText, "never invent or pass symbolic `T-*` placeholders") {
		t.Fatal("task envelope must require exact durable dependency IDs")
	}
	if !strings.Contains(envelopeText, "{{CAPABILITY_PROMPT}}") {
		t.Fatal("task envelope must expose capability-specific discipline")
	}
}

func TestCapabilityPromptSeparatesReadOnlyAndWriteProfiles(t *testing.T) {
	for profile := range ReadOnlyProfiles {
		prompt := capabilityPrompt(profile)
		for _, want := range []string{"no net workspace mutation", "git status --porcelain=v1 --untracked-files=all", "PYTHONDONTWRITEBYTECODE=1", "starting baseline", "assigned detached HEAD", "Never discard pre-existing changes"} {
			if !strings.Contains(prompt, want) {
				t.Fatalf("read-only capability prompt for %s missing %q", profile, want)
			}
		}
	}
	if prompt := capabilityPrompt("implement"); strings.Contains(prompt, "no net workspace mutation") || !strings.Contains(prompt, "write capability") {
		t.Fatalf("implement capability prompt is wrong: %q", prompt)
	}
}
