package mcp

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// EndpointPath is the single streamable-HTTP endpoint the MCP server serves.
// Clients POST JSON-RPC messages here; the server replies with a JSON-RPC
// response body (or 202 Accepted for notifications).
const EndpointPath = "/mcp"

// Handler returns the MCP streamable-HTTP handler. It implements the "Streamable
// HTTP" transport: each POST carries one JSON-RPC message answered with one
// JSON-RPC response (or 202 Accepted for notifications), and a GET that advertises
// text/event-stream opens a server-initiated SSE observation stream (when a
// StreamHandler is wired).
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
	// DNS-rebinding / local-listener hardening runs BEFORE any method-specific
	// dispatch or stream open, so the GET and POST paths cannot diverge (DEC).
	// Before reading, decoding, dispatching, or writing a single byte, the
	// request's Host/local-authority and Origin must resolve to the loopback
	// authority this server binds. A foreign or opaque (literal-null) Origin/Host
	// is rejected with 403 before a stream opens or any handler runs. This is
	// defense in depth on top of the authoritative socket bind gate
	// (NewLoopbackServer/IsLoopbackAddr); it does not replace them.
	if !requestAuthorized(r) {
		http.Error(w, "forbidden: request authority is not loopback", http.StatusForbidden)
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.serveSSE(w, r)
	case http.MethodPost:
		s.servePOST(w, r)
	default:
		w.Header().Set("Allow", http.MethodPost+", OPTIONS")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// serveSSE handles a GET on the endpoint. A client that does not advertise
// text/event-stream receives the historical 405 (so the POST-only surface is
// unchanged for non-streaming clients); one that does must satisfy the same
// MCP-Protocol-Version gate POST enforces before a single byte is written, and is
// then handed to the injected StreamHandler. When no StreamHandler is wired the
// endpoint keeps returning 405 for every GET.
func (s *Server) serveSSE(w http.ResponseWriter, r *http.Request) {
	if !acceptsEventStream(r) {
		w.Header().Set("Allow", http.MethodPost+", OPTIONS")
		http.Error(w, "streamable GET not supported; POST JSON-RPC to "+EndpointPath, http.StatusMethodNotAllowed)
		return
	}
	// Unlike POST (where initialize may omit the header because a body names the
	// version), a GET carries no JSON-RPC body, so the version header is required
	// unconditionally before any stream is opened. Nothing is written on refusal.
	if v := r.Header.Get(ProtocolVersionHeader); v != ProtocolVersion {
		http.Error(w, fmt.Sprintf("invalid or missing %s header (want %s)", ProtocolVersionHeader, ProtocolVersion), http.StatusBadRequest)
		return
	}
	if s.Streams == nil {
		w.Header().Set("Allow", http.MethodPost+", OPTIONS")
		http.Error(w, "streamable GET not supported; POST JSON-RPC to "+EndpointPath, http.StatusMethodNotAllowed)
		return
	}
	s.Streams.ServeSSE(w, r)
}

// acceptsEventStream reports whether the request's Accept header includes the SSE
// media type (text/event-stream), honoring the */* and text/* wildcards.
func acceptsEventStream(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept"), ",") {
		tok := strings.TrimSpace(part)
		if i := strings.IndexByte(tok, ';'); i >= 0 {
			tok = strings.TrimSpace(tok[:i])
		}
		if tok == "text/event-stream" || tok == "*/*" || tok == "text/*" {
			return true
		}
	}
	return false
}

// servePOST handles the request/response half of the transport: one JSON-RPC
// message answered by one JSON-RPC response (or 202 Accepted for a notification).
func (s *Server) servePOST(w http.ResponseWriter, r *http.Request) {
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
	// Per the 2025-06-18 Streamable-HTTP contract, every request after
	// initialize must carry MCP-Protocol-Version naming a revision this server
	// implements. Praector does not implement any pre-2025-06-18 revision, so a
	// missing or unsupported header is a plain 400 (it does not silently assume
	// 2025-03-26). initialize itself may omit the header.
	if req.Method != methodInitialize {
		if v := r.Header.Get(ProtocolVersionHeader); v != ProtocolVersion {
			http.Error(w, fmt.Sprintf("invalid or missing %s header (want %s)", ProtocolVersionHeader, ProtocolVersion), http.StatusBadRequest)
			return
		}
	}
	resp := s.handle(req)
	if resp == nil {
		// Notification: acknowledged with no body.
		w.WriteHeader(http.StatusAccepted)
		return
	}
	writeJSON(w, *resp)
}

// requestAuthorized reports whether an inbound Streamable-HTTP request targets
// the loopback authority this server binds, guarding against DNS rebinding and
// other cross-origin attacks against a local, unauthenticated listener. The
// Host/local-authority must be loopback, and when a browser Origin header is
// present it must be an http/https origin whose host is loopback and whose port
// matches the request authority (fail closed). A request with no Origin (a
// non-browser client) is accepted on the Host check alone. A literal "null" or
// otherwise-opaque/unparsable Origin names no loopback authority to validate and
// is therefore refused, exactly like a foreign Origin.
func requestAuthorized(r *http.Request) bool {
	if !loopbackAuthority(r.Host) {
		return false
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	// An opaque "null" origin (file:// pages, sandboxed/unique-origin contexts,
	// or a client that sends the literal string "null") carries no host to bind
	// against the loopback authority; per the 2025-06-18 Transports security
	// guidance it is rejected before dispatch rather than treated as absent.
	if origin == "null" {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	if !loopbackAuthority(u.Host) {
		return false
	}
	return authorityPort(u.Host) == authorityPort(r.Host)
}

// loopbackAuthority reports whether a "host[:port]" authority names a loopback
// interface: the hostname "localhost" or a 127.0.0.0/8 or [::1] address. A bare
// or non-loopback host is refused.
func loopbackAuthority(authority string) bool {
	host, _, err := net.SplitHostPort(authority)
	if err != nil {
		host = authority
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// authorityPort returns the port component of a "host[:port]" authority, or the
// empty string when no explicit port is present.
func authorityPort(authority string) string {
	if _, port, err := net.SplitHostPort(authority); err == nil {
		return port
	}
	return ""
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// IsLoopbackAddr reports whether an ip:port listen address resolves to a
// loopback interface only. An explicit loopback IP (127.0.0.0/8 or ::1) or the
// hostname "localhost" is accepted; any other host is rejected. A bare ":port"
// (empty host) is NOT loopback: Go binds an empty-host address to the wildcard
// 0.0.0.0/[::] (every interface), which would silently expose the unauthenticated
// listener beyond loopback, so an empty host is refused.
func IsLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "" {
		// Empty host means "all interfaces" to Go's net package, not loopback.
		return false
	}
	if strings.EqualFold(host, "localhost") {
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
