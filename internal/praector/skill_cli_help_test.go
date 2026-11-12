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
}
