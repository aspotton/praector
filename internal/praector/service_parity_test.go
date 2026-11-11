package praector

import (
	"strings"
	"testing"
)

// TestCommandHelpServiceParity is the CLI/MCP parity contract from DEC-28fcd38435:
// every entry in the canonical CLI inventory (commandHelp) must resolve to a
// shared service-layer operation, and there must be no service operation without
// a corresponding CLI command. The future MCP adapter is driven off this same
// registry, so any drift between the CLI help surface and the service layer
// fails here rather than silently diverging at runtime.
func TestCommandHelpServiceParity(t *testing.T) {
	for key := range commandHelp {
		op, ok := serviceOps[key]
		if !ok {
			t.Errorf("commandHelp entry %q has no service op registration", key)
			continue
		}
		if op.name == "" {
			t.Errorf("service op for %q has an empty name", key)
		}
		switch op.kind {
		case opDomain:
			if op.call == nil {
				t.Errorf("domain command %q does not resolve to a service method", key)
			}
		case opGroup, opLaunchOnly:
			if op.call != nil {
				t.Errorf("non-domain command %q (kind %d) must not carry a domain op method", key, op.kind)
			}
		default:
			t.Errorf("command %q has an invalid service op kind %d", key, op.kind)
		}
	}
	for key := range serviceOps {
		if _, ok := commandHelp[key]; !ok {
			t.Errorf("service op registration %q has no commandHelp entry", key)
		}
	}
}

// TestCommandHelpGroupsResolveToChildren ensures each dispatch-only (group)
// commandHelp entry actually owns at least one leaf command that does resolve to
// a domain op, so the 1:1 mapping is not satisfied by a group entry alone.
func TestCommandHelpGroupsResolveToChildren(t *testing.T) {
	for key, op := range serviceOps {
		if op.kind != opGroup {
			continue
		}
		found := false
		for childKey, child := range serviceOps {
			if strings.HasPrefix(childKey, key+" ") && child.kind == opDomain {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("group command %q has no leaf commandHelp entry resolving to a domain op", key)
		}
	}
}

// TestServiceOpsAreWired asserts the registry's domain-op method expressions are
// non-nil. Because serviceOps stores `(*Service).Op` method expressions, the
// package will not even compile if a listed domain op does not exist, so this is
// a defense-in-depth runtime check rather than the primary guarantee.
func TestServiceOpsAreWired(t *testing.T) {
	for key, op := range serviceOps {
		if op.kind == opDomain && op.call == nil {
			t.Errorf("domain command %q missing method reference", key)
		}
	}
}
