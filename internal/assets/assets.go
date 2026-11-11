package assets

import "embed"

// FS contains the durable Dolt schema and built-in prompt contracts.
// Keeping these resources compiled into the Go binary makes praector relocatable
// without a Python package or sidecar data directory.
//
//go:embed schema.sql envelope.md prompts/*.md
var FS embed.FS
