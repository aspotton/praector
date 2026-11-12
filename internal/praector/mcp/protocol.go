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
// does not implement. A client requesting any other revision is refused (see
// Server.initializeResult) rather than echoed, per the authoritative research
// (DEC-92e6ae36d4: advertise ONLY 2025-06-18; never echo unsupported versions).
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
	methodInitialize   = "initialize"
	methodInitialized  = "notifications/initialized"
	methodPing         = "ping"
	methodToolsList    = "tools/list"
	methodToolsCall    = "tools/call"
	methodResourcesLst = "resources/list"
	methodResourcesRd  = "resources/read"
)

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
