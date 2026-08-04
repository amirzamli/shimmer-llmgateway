package mcp

import (
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"strings"
	"time"
)

// Handler returns the streamable-http handler for the MCP surface: one POST
// endpoint (any path) accepting application/json. Per the plan, HTTP
// responses are JSON; SSE is used only for notifications when the client
// requests text/event-stream (an open server-to-client channel this server
// keeps alive with comments — it never sends notifications in v1).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleHTTP)
	return mux
}

func (s *Server) handleHTTP(w http.ResponseWriter, r *http.Request) {
	// CORS preflight so browser-based MCP clients can call the endpoint.
	if r.Method == http.MethodOptions {
		w.Header().Set("Allow", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, MCP-Protocol-Version, Accept")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ct := r.Header.Get("Content-Type")
	if mediaType, _, _ := mime.ParseMediaType(ct); mediaType != "application/json" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(errorResponse(json.RawMessage("null"), codeInvalidRequest,
			"Content-Type must be application/json"))
		return
	}

	w.Header().Set("MCP-Protocol-Version", negotiatedVersion(r.Header.Get("MCP-Protocol-Version")))
	w.Header().Set("Access-Control-Allow-Origin", "*")

	var req rpcRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(errorResponse(json.RawMessage("null"), codeParseError,
			"invalid JSON body: "+err.Error()))
		return
	}

	if req.IsNotification() {
		s.handleNotification(req)
		if acceptsSSE(r) {
			s.serveNotificationSSE(w, r)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.HandleMessage(r.Context(), req))
}

// negotiatedVersion echoes the client's MCP-Protocol-Version header when it is
// one this server understands, else the server's default.
func negotiatedVersion(client string) string {
	if knownProtocol(client) {
		return client
	}
	return protocolVersion
}

// acceptsSSE reports whether the client requested a server-to-client SSE
// stream (Accept: text/event-stream).
func acceptsSSE(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "text/event-stream")
}

// serveNotificationSSE opens the server-to-client SSE channel for a
// notification the client wants to receive follow-up events on. This server
// never sends notifications in v1, so the stream is established with an
// initial comment and kept alive with comments until the client disconnects.
func (s *Server) serveNotificationSSE(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusAccepted)
	fl, ok := w.(http.Flusher)
	if !ok {
		return
	}
	fmt.Fprint(w, ": connected\n\n")
	fl.Flush()

	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			fmt.Fprint(w, ": keepalive\n\n")
			fl.Flush()
		}
	}
}
