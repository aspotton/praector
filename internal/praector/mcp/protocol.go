// Package mcp implements a dependency-light Model Context Protocol (MCP) core:
// JSON-RPC 2.0 framing plus the MCP initialize/ping/tools/resources methods and
// a streamable-HTTP transport. It deliberately depends only on the Go standard
// library so the Praector binary remains statically buildable (decision
// DEC-9997d2394f: stdlib MCP-core behind internal/praector/mcp; defer the
// official modelcontextprotocol/go-sdk).
//
// The package owns protocol framing and transport only. It knows nothing about
// Praector's domain, store, scheduler, or process lifecycle: the caller injects
// tool/resource providers that map onto the shared service layer. Enqueue-only,
// read-only, and no-process-launch guarantees are enforced by that injected
// wiring (see internal/praector/mcp_server.go), not by this package.
package mcp

import "encoding/json"

// Supported protocol revisions. The server advertises ONLY ProtocolVersion: it
// implements exactly one MCP revision and never claims support for a revision it
// does not implement (never echoes an unsupported version back as if supported).
// Per the 2025-06-18 Lifecycle Version Negotiation requirement (DEC-0edf905672),
// a well-formed initialize naming an older, newer, or otherwise-unknown revision
// receives a successful counter-offer advertising only 2025-06-18. The JSON-RPC
// Invalid-params error -32602 is reserved for a missing/empty, non-string, or
// malformed protocolVersion — the missing/empty edge being an explicit fail-closed
// hardening beyond the spec floor (DEC-12a3ff57db), not a spec mandate.
const (
	// ProtocolVersion is the single MCP revision this server implements and
	// advertises.
	ProtocolVersion = "2025-06-18"
)

// ProtocolVersionHeader is the HTTP header a Streamable-HTTP client sets on every
// post-initialize request to name the protocol revision in force. Praector
// requires it to equal ProtocolVersion on any non-initialize request (see
// serveStreamable); it does not assume the pre-2025-06-18 revision for clients
// that omit it, because it does not implement that revision.
const ProtocolVersionHeader = "MCP-Protocol-Version"

// SupportedVersions lists every protocol revision this server actually
// implements. It is exposed (via initialize errors and the transport) so clients
// can discover the honest supported set rather than a version the server only
// echoes.
func SupportedVersions() []string { return []string{ProtocolVersion} }

// JSON-RPC method names handled by the core.
const (
	methodInitialize      = "initialize"
	methodInitialized     = "notifications/initialized"
	methodPing            = "ping"
	methodToolsList       = "tools/list"
	methodToolsCall       = "tools/call"
	methodResourcesLst    = "resources/list"
	methodResourcesRd     = "resources/read"
	methodLoggingSetLevel = "logging/setLevel"
)

// LoggingMethod is the JSON-RPC method a client calls to change the verbosity of
// the server-initiated notifications/message stream. It is exported so the
// wiring layer can name the same method the core dispatches.
const LoggingMethod = methodLoggingSetLevel

// NotificationMessage is the JSON-RPC notification method the server emits on an
// open SSE stream to deliver a logging message (2025-06-18 logging capability).
const NotificationMessage = "notifications/message"

// LoggingLevels are the exactly-eight verbosity levels the MCP logging capability
// defines, in ascending order. logging/setLevel accepts only these names; any
// other value is Invalid Params (-32602). The ordering gives the wiring layer a
// rank so a per-connection level filter can decide whether a notification of a
// given level is delivered to a given stream.
var LoggingLevels = []string{
	"debug", "info", "notice", "warning", "error", "critical", "alert", "emergency",
}

// loggingLevelRank maps a valid logging level to its ascending rank. An unknown
// level maps to -1 so callers can distinguish "below debug" from "invalid".
func loggingLevelRank(level string) int {
	for i, l := range LoggingLevels {
		if l == level {
			return i
		}
	}
	return -1
}

// ValidLoggingLevel reports whether level is one of the exactly-eight MCP logging
// levels. It is exported so the wiring layer can share one source of truth for
// the accepted set (used when binding a stream's connect-time level).
func ValidLoggingLevel(level string) bool { return loggingLevelRank(level) >= 0 }

// LevelRank returns the ascending rank of a valid logging level, or -1 for an
// unknown level. It is exported so the wiring layer can apply the per-connection
// level filter without duplicating the ordering.
func LevelRank(level string) int { return loggingLevelRank(level) }

// NotificationMessageBody builds a server-initiated JSON-RPC notification for the
// logging capability. It intentionally carries NO id and NO result/error field: a
// server MUST NOT send a JSON-RPC response on an SSE stream (2025-06-18). Only
// notifications are ever written to the observation stream.
func NotificationMessageBody(level, logger string, data any) map[string]any {
	return map[string]any{
		"jsonrpc": "2.0",
		"method":  NotificationMessage,
		"params": map[string]any{
			"level":  level,
			"logger": logger,
			"data":   data,
		},
	}
}

// JSON-RPC 2.0 standard error codes.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
)

// rpcRequest is a single inbound JSON-RPC 2.0 request or notification. A
// notification has no ID.
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// rpcResponse is a single outbound JSON-RPC 2.0 result or error.
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func resultResponse(id json.RawMessage, result any) rpcResponse {
	return rpcResponse{JSONRPC: "2.0", ID: id, Result: result}
}

func errorResponse(id json.RawMessage, code int, msg string, data any) rpcResponse {
	return rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg, Data: data}}
}

// isNotification reports whether the request carries no id (i.e. it is a
// JSON-RPC notification that must not be answered with a result body).
func (r rpcRequest) isNotification() bool {
	return len(r.ID) == 0 || string(r.ID) == "null"
}
