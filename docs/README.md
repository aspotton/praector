# Praector documentation

This directory is the canonical documentation set for Praector.

Back to the human entry point: [`../README.md`](../README.md)  
Coding-agent entry point: [`../AGENTS.md`](../AGENTS.md)  
Release history: [`../CHANGELOG.md`](../CHANGELOG.md)  
Roadmap: [`../ROADMAP.md`](../ROADMAP.md)

## Choose a document

| Document | Purpose |
| --- | --- |
| [`ARCHITECTURE.md`](ARCHITECTURE.md) | System boundaries, components, invariants, lifecycle, extension points |
| [`PROFILES.md`](PROFILES.md) | Runtime role map, prompt locations, and OMO-inspired design mapping |
| [`AGENT_PROTOCOL.md`](AGENT_PROTOCOL.md) | Task Envelope, delegation, structured result contract |
| [`CLI.md`](CLI.md) | Current command/flag reference and examples |
| [`WORKSPACES.md`](WORKSPACES.md) | Git branch/worktree model, checkpointing, integration, review, acceptance |
| [`STATE_AND_RECOVERY.md`](STATE_AND_RECOVERY.md) | Dolt schema, task/attempt/session states, events, memory, crash recovery |
| [`OPERATIONS.md`](OPERATIONS.md) | Installation, project setup, running, observing, accepting, cleanup, troubleshooting |
| [`DEVELOPMENT.md`](DEVELOPMENT.md) | Source layout, extension rules, testing and documentation maintenance |

## Reading paths

### I want to use Praector

1. [`../README.md`](../README.md)
2. [`OPERATIONS.md`](OPERATIONS.md)
3. [`CLI.md`](CLI.md)
4. [`WORKSPACES.md`](WORKSPACES.md) before accepting production work

### I want to understand how Praector works

1. [`ARCHITECTURE.md`](ARCHITECTURE.md)
2. [`PROFILES.md`](PROFILES.md)
3. [`AGENT_PROTOCOL.md`](AGENT_PROTOCOL.md)
4. [`WORKSPACES.md`](WORKSPACES.md)
5. [`STATE_AND_RECOVERY.md`](STATE_AND_RECOVERY.md)

### I am an AI/coding agent modifying Praector

1. [`../AGENTS.md`](../AGENTS.md)
2. [`ARCHITECTURE.md`](ARCHITECTURE.md)
3. [`DEVELOPMENT.md`](DEVELOPMENT.md)
4. the focused subsystem document

## Canonical-source guidance

Documentation describes intended/current behavior, but exact executable behavior is enforced by code and tests. When they disagree, treat it as a bug: verify the source/tests and update the documentation in the same change.

Useful source anchors:

- CLI: `internal/praector/cli.go`, `cmd/praector/main.go`
- state/schema: `internal/praector/store.go`, `internal/assets/schema.sql`
- scheduler/recovery/review: `internal/praector/scheduler.go`
- deterministic planning policy: `internal/praector/policy.go`
- runtime prompts: `internal/assets/prompts/*.md`
- agent protocol/context compiler: `internal/praector/envelope.go`
- Git workspaces/acceptance: `internal/praector/workspace.go`
- OpenCode harness: `internal/praector/harness.go`
