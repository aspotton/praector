package praector

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestSkillCLICommandHelpCoversCommandHelp is the drift guard for the repo-local
// $praector skill's CLI usage reference. It follows the mcp_parity_test.go
// contract of iterating commandHelp directly: for every commandHelp entry, the
// literal first line of its help text ("Usage: praector ...") must appear
// verbatim in .agents/skills/praector/references/cli.md. Adding a CLI command
// to the registry without documenting it in the skill therefore fails here, so
// the skill cannot silently rot and license a `--help` discovery loop again.
//
// The guard is bidirectional, mirroring mcp_parity_test.go's reverse intent (no
// MCP surface entry may exist without a commandHelp entry): a second pass also
// requires every "Usage: praector ..." rendering in references/cli.md to match a
// commandHelp entry verbatim, so deleting a command from the registry cannot
// leave a stale row documenting a command that no longer exists. The two
// directions together fail both when a commandHelp entry is missing from the
// table and when the table names a command absent from commandHelp.
//
// The file is read from disk via the repo-root-relative pattern used by
// assets_test.go rather than go:embed, so the skill stays outside the binary
// (no registry/embedded-asset change).
func TestSkillCLICommandHelpCoversCommandHelp(t *testing.T) {
	path := filepath.Join("..", "..", ".agents", "skills", "praector", "references", "cli.md")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("cannot read skill CLI reference %s: %v", path, err)
	}
	text := string(raw)

	var missing []string
	for key := range commandHelp {
		usage := strings.SplitN(commandHelp[key], "\n", 2)[0]
		if usage == "" {
			t.Errorf("commandHelp entry %q has an empty usage line", key)
			continue
		}
		if !strings.Contains(text, usage) {
			missing = append(missing, key+" ("+usage+")")
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("references/cli.md is missing the Usage line for %d commandHelp command(s); document every command so agents never need `--help` to discover it: %s",
			len(missing), strings.Join(missing, ", "))
	}

	// Reverse direction: every "Usage: praector ..." rendering in the skill must
	// correspond to a real commandHelp entry, so a command deleted from the
	// registry cannot leave an orphan row that documents a nonexistent command.
	// Each candidate is delimited by a closing backtick (table code spans) or the
	// end of the line (the fenced namespace block), then trimmed of surrounding
	// backticks/whitespace so the renderings compare exactly against commandHelp's
	// first lines rather than by a loose prefix match.
	valid := map[string]bool{}
	for key := range commandHelp {
		valid[strings.SplitN(commandHelp[key], "\n", 2)[0]] = true
	}
	var orphans []string
	for _, line := range strings.Split(text, "\n") {
		idx := strings.Index(line, "Usage: praector ")
		if idx < 0 {
			continue
		}
		candidate := line[idx:]
		if end := strings.IndexByte(candidate, '`'); end >= 0 {
			candidate = candidate[:end]
		}
		candidate = strings.Trim(candidate, "` \t")
		if !valid[candidate] {
			orphans = append(orphans, candidate)
		}
	}
	if len(orphans) > 0 {
		sort.Strings(orphans)
		t.Errorf("references/cli.md documents %d Usage line(s) with no commandHelp entry; remove rows for commands that no longer exist: %s",
			len(orphans), strings.Join(orphans, ", "))
	}
}
