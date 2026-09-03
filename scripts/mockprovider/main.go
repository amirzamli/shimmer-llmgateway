// Command mockprovider is a tiny OpenAI-compatible fake upstream for the
// run-test environment (scripts/run-test.sh). It serves the two endpoints the
// gateway calls — GET /v1/models and POST /v1/chat/completions — with canned
// responses so the gateway can capture realistic dummy traffic without any
// real provider credentials:
//
//   - model "test-fail"            → HTTP 500 (exercises the failure surfaces)
//   - last message role "tool"     → text completion acknowledging the result
//   - request carries tools        → tool_call completion (finish_reason
//     "tool_calls"), so a follow-up request with a role:"tool" message links
//   - anything else                → text completion, content varies by model
//
// Both streaming (SSE chat.completion.chunk) and non-stream shapes are
// supported, with a plausible usage object on every response.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

type message struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCallID string          `json:"tool_call_id"`
	ToolCalls  []struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
}

type chatRequest struct {
	Model    string            `json:"model"`
	Stream   bool              `json:"stream"`
	Messages []message         `json:"messages"`
	Tools    []json.RawMessage `json:"tools"`
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8899", "listen address")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", handleModels)
	mux.HandleFunc("POST /v1/chat/completions", handleChat)

	log.Printf("mockprovider listening on %s", *addr)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Fatal(err)
	}
}

func handleModels(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data": []map[string]string{
			{"id": "test-small", "object": "model"},
			{"id": "test-large", "object": "model"},
			{"id": "test-fail", "object": "model"},
		},
	})
}

func handleChat(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	body := readAll(r)
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]string{"message": "mockprovider: bad request body", "type": "mock_error"},
		})
		return
	}
	log.Printf("%s %s model=%s stream=%v messages=%d tools=%d",
		r.Method, r.URL.Path, req.Model, req.Stream, len(req.Messages), len(req.Tools))

	if strings.Contains(req.Model, "fail") {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": map[string]string{
				"message": "mockprovider: intentional failure (model contains \"fail\")",
				"type":    "mock_error",
				"code":    "mock_500",
			},
		})
		return
	}

	promptTokens := len(body) / 4
	var (
		content string
		call    *toolCall
		finish  string
	)
	switch {
	case len(req.Messages) > 0 && req.Messages[len(req.Messages)-1].Role == "tool":
		content = "Tool result received and understood: " + truncate(text(req.Messages[len(req.Messages)-1].Content), 120)
		finish = "stop"
	case len(req.Tools) > 0:
		call = &toolCall{ID: "call_mock_1", Name: firstToolName(req.Tools), Arguments: `{"location":"Berlin","unit":"celsius"}`}
		finish = "tool_calls"
	default:
		content = cannedContent(req.Model)
		finish = "stop"
	}
	completionTokens := len(content) / 4
	usage := map[string]any{
		"prompt_tokens":     promptTokens,
		"completion_tokens": completionTokens,
		"total_tokens":      promptTokens + completionTokens,
	}

	if req.Stream {
		stream(w, req.Model, content, call, finish, usage)
		return
	}

	msg := map[string]any{"role": "assistant"}
	if call != nil {
		msg["tool_calls"] = []map[string]any{{
			"id":   call.ID,
			"type": "function",
			"function": map[string]string{
				"name":      call.Name,
				"arguments": call.Arguments,
			},
		}}
	} else {
		msg["content"] = content
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":      fmt.Sprintf("chatcmpl-mock-%d", time.Now().UnixNano()),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   req.Model,
		"choices": []map[string]any{
			{"index": 0, "message": msg, "finish_reason": finish},
		},
		"usage": usage,
	})
}

type toolCall struct {
	ID        string
	Name      string
	Arguments string
}

// cannedContent returns deterministic dummy text varying by model so the
// traces view has visibly different conversations.
func cannedContent(model string) string {
	if strings.Contains(model, "large") {
		return "This is a longer dummy reply from the mock provider. " +
			"The gateway reassembled it from several streamed deltas, " +
			"recorded the token usage, and estimated the cost from the pricing table. " +
			"Everything you see in this trace was captured, never forwarded to a real provider."
	}
	return "Dummy reply from the mock provider (" + model + ")."
}

// stream writes the SSE form: role chunk, content or tool_call argument
// deltas, a finish_reason chunk, a usage-only chunk, then [DONE].
func stream(w http.ResponseWriter, model, content string, tc *toolCall, finish string, usage map[string]any) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": map[string]string{"message": "streaming unsupported"}})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")

	id := fmt.Sprintf("chatcmpl-mock-%d", time.Now().UnixNano())
	emit := func(delta map[string]any, fr any) {
		if delta == nil {
			delta = map[string]any{}
		}
		writeSSE(w, map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model,
			"choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": fr}},
		})
		flusher.Flush()
		time.Sleep(30 * time.Millisecond)
	}

	emit(map[string]any{"role": "assistant"}, nil)
	if tc != nil {
		emit(map[string]any{"tool_calls": []map[string]any{{
			"index": 0, "id": tc.ID, "type": "function",
			"function": map[string]string{"name": tc.Name, "arguments": ""},
		}}}, nil)
		for _, part := range splitArgs(tc.Arguments, 8) {
			emit(map[string]any{"tool_calls": []map[string]any{{
				"index": 0, "function": map[string]string{"arguments": part},
			}}}, nil)
		}
	} else {
		for _, part := range splitArgs(content, 16) {
			emit(map[string]any{"content": part}, nil)
		}
	}
	emit(map[string]any{}, finish)
	// OpenAI sends usage on a final choices-less chunk; the assembler keeps it.
	writeSSE(w, map[string]any{
		"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model,
		"choices": []any{}, "usage": usage,
	})
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

// splitArgs chunks s into pieces of at most n runes for delta emission.
func splitArgs(s string, n int) []string {
	var out []string
	r := []rune(s)
	for i := 0; i < len(r); i += n {
		end := i + n
		if end > len(r) {
			end = len(r)
		}
		out = append(out, string(r[i:end]))
	}
	if len(out) == 0 {
		out = []string{""}
	}
	return out
}

// firstToolName extracts the first tool's function name, defaulting to "echo".
func firstToolName(tools []json.RawMessage) string {
	var t struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if len(tools) > 0 && json.Unmarshal(tools[0], &t) == nil && t.Function.Name != "" {
		return t.Function.Name
	}
	return "echo"
}

// text flattens a message content value (string or parts array) to text.
func text(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var sb strings.Builder
		for _, p := range parts {
			sb.WriteString(p.Text)
		}
		return sb.String()
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	b, _ := json.Marshal(v)
	_, _ = w.Write(b)
}

// writeSSE writes one `data: <json>\n\n` event line.
func writeSSE(w http.ResponseWriter, v any) {
	b, _ := json.Marshal(v)
	fmt.Fprintf(w, "data: %s\n\n", b)
}

func readAll(r *http.Request) []byte {
	b, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	if err != nil {
		return nil
	}
	return b
}
