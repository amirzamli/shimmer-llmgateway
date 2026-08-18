// Package mcp implements the §7 inspection MCP server: a minimal hand-rolled
// MCP JSON-RPC surface (initialize, ping, notifications/initialized,
// tools/list, tools/call) over the stdio and streamable-http transports, with
// the §7.1 tools mapped onto the store's query surface (the seven generic
// tools plus the validate_tool_call / classify_failure analysis tools).
//
// Conventions (§7): tools have structured_output=false and return JSON text
// strings; tools never throw — execution failures are
// CallToolResult(isError=true) carrying a body of {errorCode, message}. Only
// protocol-level problems (unknown method, unknown tool, malformed request)
// become JSON-RPC errors.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/amirzamli/shimmer-llmgateway/internal/store"
)

// Server is the §7 inspection MCP server. It is safe for concurrent use
// across transports.
type Server struct {
	store        *store.Store
	sessionScope string // --session flag; "" = no scoping

	mu        sync.RWMutex
	toolOrder []string
	tools     map[string]tool
}

// New builds an inspection server over st. sessionScope, when non-empty,
// constrains the generic tools to that session (the --session flag): the
// scoped session overrides any caller-supplied session_id.
func New(st *store.Store, sessionScope string) *Server {
	s := &Server{store: st, sessionScope: sessionScope}
	s.registerTools()
	return s
}

// Store returns the underlying capture store.
func (s *Server) Store() *store.Store { return s.store }

// SessionScope returns the --session scope, or "" when unscoped.
func (s *Server) SessionScope() string { return s.sessionScope }

// toolNames returns the registered tool names in registration order.
func (s *Server) toolNames() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]string(nil), s.toolOrder...)
}

// HandleMessage dispatches one JSON-RPC request (never a notification) and
// returns the response to encode. Notifications are handled separately by the
// transports.
func (s *Server) HandleMessage(ctx context.Context, req rpcRequest) rpcResponse {
	if req.JSONRPC != "2.0" {
		return errorResponse(req.ID, codeInvalidRequest, "invalid jsonrpc version: "+req.JSONRPC)
	}
	switch req.Method {
	case "initialize":
		return s.handleInitialize(req)
	case "ping":
		return s.handlePing(req)
	case "tools/list":
		return s.handleToolsList(req)
	case "tools/call":
		return s.handleToolsCall(ctx, req)
	default:
		return errorResponse(req.ID, codeMethodNotFound, "method not found: "+req.Method)
	}
}

// handleNotification processes a JSON-RPC notification. The minimal server has
// no notification side effects: notifications/initialized (and any unknown
// notification) are acknowledged by being ignored, per JSON-RPC.
func (s *Server) handleNotification(req rpcRequest) {}

// handleInitialize implements the MCP initialize handshake. The result echoes
// the client's protocol version when it is one this server understands, else
// the server's latest supported version.
func (s *Server) handleInitialize(req rpcRequest) rpcResponse {
	var params struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(req.Params, &params)
	ver := params.ProtocolVersion
	if !knownProtocol(ver) {
		ver = protocolVersion
	}
	return resultResponse(req.ID, map[string]any{
		"protocolVersion": ver,
		"capabilities": map[string]any{
			"tools": map[string]any{"listChanged": false},
		},
		"serverInfo": map[string]any{
			"name":    serverName,
			"version": serverVersion,
		},
	})
}

// handlePing implements MCP ping: an empty result.
func (s *Server) handlePing(req rpcRequest) rpcResponse {
	return resultResponse(req.ID, map[string]any{})
}

// handleToolsList returns the §7.1 tools with their input schemas. The tool
// registry is table-driven, so tools slot in by registration only.
func (s *Server) handleToolsList(req rpcRequest) rpcResponse {
	tools := make([]map[string]any, 0, len(s.tools))
	for _, name := range s.toolNames() {
		t := s.tools[name]
		tools = append(tools, map[string]any{
			"name":        t.name,
			"description": t.description,
			"inputSchema": t.inputSchema,
		})
	}
	return resultResponse(req.ID, map[string]any{"tools": tools})
}

// handleToolsCall invokes a tool. An unknown tool is a JSON-RPC invalid-params
// error (a protocol-level problem). A tool execution failure is returned as
// CallToolResult(isError=true) whose text content is {"errorCode": ...,
// "message": ...} — the §7 convention: never throw.
func (s *Server) handleToolsCall(ctx context.Context, req rpcRequest) rpcResponse {
	var params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return errorResponse(req.ID, codeInvalidParams, "invalid params: "+err.Error())
	}
	t, ok := s.tools[params.Name]
	if !ok {
		return errorResponse(req.ID, codeInvalidParams, "unknown tool: "+params.Name)
	}
	result, terr := t.handler(s, ctx, params.Arguments)
	if terr != nil {
		body, _ := json.Marshal(map[string]string{"errorCode": terr.code, "message": terr.message})
		return resultResponse(req.ID, map[string]any{
			"content": []any{map[string]any{"type": "text", "text": string(body)}},
			"isError": true,
		})
	}
	return resultResponse(req.ID, map[string]any{
		"content": []any{map[string]any{"type": "text", "text": resultJSONText(result)}},
		"isError": false,
	})
}

// ServeStdio runs the MCP stdio transport over r/w: newline-delimited JSON
// requests in, one JSON response per request out (notifications receive no
// response). It returns when r hits EOF (the client closed the pipe) or ctx is
// done.
func (s *Server) ServeStdio(ctx context.Context, r io.Reader, w io.Writer) error {
	dec := json.NewDecoder(r)
	bw := bufio.NewWriter(w)
	enc := json.NewEncoder(bw)
	enc.SetEscapeHTML(false)
	for {
		var req rpcRequest
		if err := dec.Decode(&req); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("mcp stdio: decode request: %w", err)
		}
		if req.IsNotification() {
			s.handleNotification(req)
			continue
		}
		resp := s.HandleMessage(ctx, req)
		if err := enc.Encode(resp); err != nil {
			return fmt.Errorf("mcp stdio: encode response: %w", err)
		}
		if err := bw.Flush(); err != nil {
			return fmt.Errorf("mcp stdio: flush: %w", err)
		}
	}
}
