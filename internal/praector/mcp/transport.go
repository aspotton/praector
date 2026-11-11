package mcp

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
)

// EndpointPath is the single streamable-HTTP endpoint the MCP server serves.
// Clients POST JSON-RPC messages here; the server replies with a JSON-RPC
// response body (or 202 Accepted for notifications).
const EndpointPath = "/mcp"

// Handler returns the MCP streamable-HTTP handler. It implements the minimal
// "Streamable HTTP" transport: each POST carries one JSON-RPC message and is
// answered with one JSON-RPC response. Server-sent-event streaming is not
// required for the request/response round-trips Praector exposes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(EndpointPath, s.serveStreamable)
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	return mux
}

func (s *Server) serveStreamable(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		// A GET on the endpoint would open an SSE stream, which this minimal
		// transport does not implement. Signal that plainly rather than 404.
		w.Header().Set("Allow", http.MethodPost+", OPTIONS")
		http.Error(w, "streamable GET not supported; POST JSON-RPC to "+EndpointPath, http.StatusMethodNotAllowed)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost+", OPTIONS")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		writeJSON(w, errorResponse(nil, codeParseError, "failed to read request body", nil))
		return
	}
	req, derr := decodeRequest(body)
	if derr != nil {
		writeJSON(w, errorResponse(nil, codeParseError, derr.Error(), nil))
		return
	}
	resp := s.handle(req)
	if resp == nil {
		// Notification: acknowledged with no body.
		w.WriteHeader(http.StatusAccepted)
		return
	}
	writeJSON(w, *resp)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// IsLoopbackAddr reports whether an ip:port listen address resolves to a
// loopback interface only. Bare "localhost" and ":port" (empty host) are treated
// as loopback; any explicit routable host is rejected.
func IsLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "" || strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// A non-IP hostname that is not "localhost" could resolve anywhere.
		return false
	}
	return ip.IsLoopback()
}

// NewLoopbackServer builds an *http.Server bound to addr after asserting that
// addr is loopback-only. Non-loopback binding is refused to preserve the local,
// unauthenticated boundary (decision DEC-aacb7bfc23: auth/identity deferred).
func NewLoopbackServer(addr string, srv *Server) (*http.Server, error) {
	if !IsLoopbackAddr(addr) {
		return nil, fmt.Errorf("MCP listener must bind loopback only; refusing %q", addr)
	}
	return &http.Server{Addr: addr, Handler: srv.Handler()}, nil
}
