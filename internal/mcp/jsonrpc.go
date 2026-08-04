package mcp

import (
	"encoding/json"
)

// Protocol constants for the MCP surface.
const (
	// protocolVersion is the MCP protocol version this server negotiates
	// (Streamable HTTP transport, 2025-06-18).
	protocolVersion = "2025-06-18"
	serverName      = "inspect-mcp"
	serverVersion   = "0.2.0"
)

// JSON-RPC 2.0 error codes (the reserved -32768..-32000 range).
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
)

// knownProtocolVersions are the MCP protocol versions this server
// understands. A client that negotiates one of these gets it echoed back in
// the initialize result and the MCP-Protocol-Version header.
var knownProtocolVersions = []string{"2024-11-05", "2025-03-26", "2025-06-18"}

// rpcRequest is a decoded JSON-RPC message. Notifications carry no id.
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// IsNotification reports whether the message is a JSON-RPC notification
// (absent or explicit-null id).
func (r rpcRequest) IsNotification() bool {
	return len(r.ID) == 0 || string(r.ID) == "null"
}

// rpcResponse is a JSON-RPC response.
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// rpcError is the JSON-RPC error object.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// resultResponse builds a success response, marshaling result.
func resultResponse(id json.RawMessage, result any) rpcResponse {
	b, err := json.Marshal(result)
	if err != nil {
		return errorResponse(id, codeInternalError, "internal error: result marshal failed")
	}
	return rpcResponse{JSONRPC: "2.0", ID: id, Result: b}
}

// errorResponse builds a JSON-RPC error response.
func errorResponse(id json.RawMessage, code int, message string) rpcResponse {
	return rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: message}}
}

// knownProtocol reports whether v is a protocol version this server
// understands.
func knownProtocol(v string) bool {
	for _, p := range knownProtocolVersions {
		if v == p {
			return true
		}
	}
	return false
}
