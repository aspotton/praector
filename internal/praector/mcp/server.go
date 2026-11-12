package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// Tool describes one callable MCP tool advertised by tools/list.
type Tool struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	InputSchema any    `json:"inputSchema,omitempty"`
}

// ContentItem is one content block in a tool call result. Only text content is
// produced by the Praector bridge; the shape is retained for protocol fidelity.
type ContentItem struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// ToolResult is the tools/call result envelope.
type ToolResult struct {
	Content []ContentItem `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

// Resource describes one readable MCP resource advertised by resources/list.
type Resource struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	MIMEType    string `json:"mimeType,omitempty"`
}

// ResourceContents is one item returned by resources/read.
type ResourceContents struct {
	URI      string `json:"uri"`
	MIMEType string `json:"mimeType,omitempty"`
	Text     string `json:"text,omitempty"`
}

// ToolProvider supplies the tool surface. ListTools returns the current tool
// set; CallTool dispatches a named call with its raw JSON arguments. Either hook
// may be nil, in which case the corresponding method returns a JSON-RPC error.
type ToolProvider interface {
	ListTools() []Tool
	CallTool(name string, args json.RawMessage) (ToolResult, error)
}

// ResourceProvider supplies the resource surface.
type ResourceProvider interface {
	ListResources() []Resource
	ReadResource(uri string) ([]ResourceContents, error)
}

// Server is a minimal, transport-agnostic MCP core. It answers the
// initialize/initialized/ping lifecycle plus tools and resources introspection
// by delegating to injected providers. It never performs domain or process work
// itself.
type Server struct {
	ServerName    string
	ServerVersion string
	Tools         ToolProvider
	Resources     ResourceProvider
}

// handle processes one decoded request and returns the response to send back. A
// nil return means the request was a notification that needs no reply body.
func (s *Server) handle(req rpcRequest) *rpcResponse {
	switch req.Method {
	case methodInitialize:
		return s.initialize(req.ID, req.Params)
	case methodInitialized:
		return nil // notification: acknowledged, no body
	case methodPing:
		return s.result(req.ID, map[string]any{})
	case methodToolsList:
		if s.Tools == nil {
			return s.err(req.ID, codeMethodNotFound, "tools are not available", nil)
		}
		tools := s.Tools.ListTools()
		if tools == nil {
			tools = []Tool{}
		}
		return s.result(req.ID, map[string]any{"tools": tools})
	case methodToolsCall:
		if s.Tools == nil {
			return s.err(req.ID, codeMethodNotFound, "tools are not available", nil)
		}
		return s.toolsCall(req.ID, req.Params)
	case methodResourcesLst:
		if s.Resources == nil {
			return s.err(req.ID, codeMethodNotFound, "resources are not available", nil)
		}
		res := s.Resources.ListResources()
		if res == nil {
			res = []Resource{}
		}
		return s.result(req.ID, map[string]any{"resources": res})
	case methodResourcesRd:
		if s.Resources == nil {
			return s.err(req.ID, codeMethodNotFound, "resources are not available", nil)
		}
		return s.resourceRead(req.ID, req.Params)
	default:
		return s.err(req.ID, codeMethodNotFound, "method not found: "+req.Method, nil)
	}
}

// initialize answers the MCP initialize request with 2025-06-18 version
// negotiation (DEC-0edf905672, superseding the earlier refuse-on-mismatch
// stance). A well-formed request naming a non-empty protocolVersion — matching,
// older, newer, or an otherwise-unknown revision — is met with a SUCCESSFUL
// result advertising the one revision this server actually implements
// (2025-06-18), i.e. a counter-offer. It never echoes the client's requested
// revision back as if it were supported (never advertise a revision this server
// does not implement). The JSON-RPC Invalid-params error -32602 is returned for
// a missing or empty protocolVersion, for a present-but-non-string
// protocolVersion, or for a body that is not a JSON object — none of which name
// a revision this server could ever agree to speak. The missing/empty edge is an
// explicit fail-closed hardening beyond the 2025-06-18 counter-offer floor
// (DEC-12a3ff57db), not a spec mandate; the well-formed counter-offer path is
// preserved and must not be weakened to satisfy it.
func (s *Server) initialize(id json.RawMessage, params json.RawMessage) *rpcResponse {
	// A protocolVersion is required, must be a string, and must be non-empty.
	// Absent params, a non-object body, a missing field, an empty string, or a
	// non-string value are all Invalid-params; only a well-formed non-empty
	// revision proceeds to the counter-offer (or matching success) below.
	if len(bytes.TrimSpace(params)) == 0 {
		return s.err(id, codeInvalidParams, "Invalid params: initialize requires a protocolVersion", nil)
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(params, &fields) != nil {
		return s.err(id, codeInvalidParams, "Invalid params: initialize body must be a JSON object", nil)
	}
	raw, ok := fields["protocolVersion"]
	if !ok {
		return s.err(id, codeInvalidParams, "Invalid params: initialize requires a protocolVersion", nil)
	}
	var requested string
	if json.Unmarshal(raw, &requested) != nil {
		return s.err(id, codeInvalidParams, "Invalid params: protocolVersion must be a string", nil)
	}
	if strings.TrimSpace(requested) == "" {
		return s.err(id, codeInvalidParams, "Invalid params: protocolVersion must be a non-empty string", nil)
	}
	// A matching, older, newer, or otherwise-unknown well-formed revision is
	// answered by the counter-offer below, which always advertises only
	// ProtocolVersion and never echoes requested.
	return s.result(id, map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities": map[string]any{
			"tools":     map[string]any{"listChanged": false},
			"resources": map[string]any{"subscribe": false, "listChanged": false},
		},
		"serverInfo": map[string]any{"name": s.ServerName, "version": s.ServerVersion},
	})
}

func (s *Server) toolsCall(id json.RawMessage, params json.RawMessage) *rpcResponse {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(defaultObject(params), &p); err != nil || p.Name == "" {
		return s.err(id, codeInvalidParams, "tools/call requires a tool name", nil)
	}
	res, err := s.Tools.CallTool(p.Name, p.Arguments)
	if err != nil {
		// A domain-level failure is reported as a tool result with isError set,
		// not a transport error, so the client can read the message content.
		return s.result(id, ToolResult{Content: []ContentItem{{Type: "text", Text: err.Error()}}, IsError: true})
	}
	if res.Content == nil {
		res.Content = []ContentItem{}
	}
	return s.result(id, res)
}

func (s *Server) resourceRead(id json.RawMessage, params json.RawMessage) *rpcResponse {
	var p struct {
		URI string `json:"uri"`
	}
	if err := json.Unmarshal(defaultObject(params), &p); err != nil || p.URI == "" {
		return s.err(id, codeInvalidParams, "resources/read requires a uri", nil)
	}
	contents, err := s.Resources.ReadResource(p.URI)
	if err != nil {
		return s.err(id, codeInvalidParams, err.Error(), nil)
	}
	if contents == nil {
		contents = []ResourceContents{}
	}
	return s.result(id, map[string]any{"contents": contents})
}

func (s *Server) result(id json.RawMessage, v any) *rpcResponse {
	r := resultResponse(id, v)
	return &r
}

func (s *Server) err(id json.RawMessage, code int, msg string, data any) *rpcResponse {
	r := errorResponse(id, code, msg, data)
	return &r
}

// decodeRequest parses one JSON-RPC message body.
func decodeRequest(body []byte) (rpcRequest, error) {
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return req, fmt.Errorf("invalid JSON-RPC message: %w", err)
	}
	if req.JSONRPC != "" && req.JSONRPC != "2.0" {
		return req, fmt.Errorf("unsupported jsonrpc version %q", req.JSONRPC)
	}
	if req.Method == "" {
		return req, fmt.Errorf("missing jsonrpc method")
	}
	return req, nil
}

func defaultObject(params json.RawMessage) json.RawMessage {
	if len(params) == 0 {
		return []byte("{}")
	}
	return params
}
