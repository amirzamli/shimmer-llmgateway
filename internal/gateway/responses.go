package gateway

// The OpenAI Responses API shim (POST /v1/responses): a stateless-only
// translation layer that lets Responses clients (e.g. Codex CLI) use the
// shared chat pipeline. Requests are validated against the stateless
// contract (unknown tool types are skipped, not rejected — Codex sends newer
// tool groupings alongside function tools), translated to a Chat
// Completions body, and
// dispatched into the existing branches (routing, plugins, anthropic
// composition, capture); 2xx chat bodies are converted to Responses objects
// for the CLIENT only — capture keeps the as-received Responses request bytes
// and the chat-normalized response. Upstream errors reuse the existing
// {"error":{...}} passthrough envelope. Streaming event translation lives in
// Phase 2.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/amirzamli/shimmer-llmgateway/internal/store"
)

// responsesEndpoint is the capture surface endpoint recorded on every
// /v1/responses row.
const responsesEndpoint = "/v1/responses"

// ---------- request shape ----------

// responsesRequest is the OpenAI Responses API request subset the shim
// understands. store/include/metadata and reasoning.summary are parsed (so
// unknown-field strictness never rejects them) and accepted-and-ignored.
type responsesRequest struct {
	Model              string           `json:"model"`
	Input              json.RawMessage  `json:"input"`
	Instructions       string           `json:"instructions"`
	Stream             *bool            `json:"stream"`
	PreviousResponseID string           `json:"previous_response_id"`
	Tools              []responsesTool  `json:"tools"`
	ToolChoice         json.RawMessage  `json:"tool_choice"`
	ParallelToolCalls  *bool            `json:"parallel_tool_calls"`
	Temperature        *float64         `json:"temperature"`
	TopP               *float64         `json:"top_p"`
	MaxOutputTokens    *int             `json:"max_output_tokens"`
	Text               *responsesText   `json:"text"`
	Reasoning          *responsesIntake `json:"reasoning"`
	Store              *bool            `json:"store"`
	Include            json.RawMessage  `json:"include"`
	Metadata           json.RawMessage  `json:"metadata"`
	// PromptCacheKey is Codex CLI's stable per-conversation string: with no
	// X-Session-Id header it groups a multi-turn conversation into one
	// capture session (see handleResponses). It is accepted-and-ignored
	// upstream — the translated chat body never carries it.
	PromptCacheKey string `json:"prompt_cache_key"`
}

// responsesTool is one Responses tool definition (the flat form: name at the
// tool level, unlike the chat surface's nested "function" object).
type responsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      *bool           `json:"strict"`
}

// responsesText carries the text.format response shape.
type responsesText struct {
	Format json.RawMessage `json:"format"`
}

// responsesIntake is the reasoning request field: effort maps to
// reasoning_effort, summary is accepted and ignored.
type responsesIntake struct {
	Effort  string          `json:"effort"`
	Summary json.RawMessage `json:"summary"`
}

// responsesItem is one Responses input item (input may also be a plain
// string, which becomes a single user message).
type responsesItem struct {
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	CallID    string          `json:"call_id"`
	Name      string          `json:"name"`
	Arguments string          `json:"arguments"`
	Output    json.RawMessage `json:"output"`
}

// parseResponsesRequest decodes the as-received Responses body.
func parseResponsesRequest(body []byte) (*responsesRequest, error) {
	var req responsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, err
	}
	return &req, nil
}

// validateResponsesRequest enforces the stateless contract before any
// upstream work. It returns the offending request field (the error envelope's
// param) and a client-facing message; both empty when the request is
// acceptable. Non-function tool types are deliberately NOT rejected here
// (Codex CLI 0.153 sends newer tool groupings alongside function tools);
// translateResponsesToChat skips them.
func validateResponsesRequest(req *responsesRequest) (string, string) {
	if req.PreviousResponseID != "" {
		return "previous_response_id", "previous_response_id is not supported: this gateway is stateless and does not store Responses API state"
	}
	return "", ""
}

// ---------- request translation ----------

// translateResponsesToChat converts a validated Responses request into a Chat
// Completions body. The model field is forwarded as received — buildUpstream
// rewrites it to the resolved model exactly as on the chat surface.
func translateResponsesToChat(req *responsesRequest) ([]byte, error) {
	messages := make([]map[string]any, 0, 8)
	if req.Instructions != "" {
		messages = append(messages, map[string]any{"role": "system", "content": req.Instructions})
	}
	if len(req.Input) > 0 && string(req.Input) != "null" {
		var inputText string
		if err := json.Unmarshal(req.Input, &inputText); err == nil {
			messages = append(messages, map[string]any{"role": "user", "content": inputText})
		} else {
			var items []responsesItem
			if err := json.Unmarshal(req.Input, &items); err != nil {
				return nil, fmt.Errorf("invalid input: %v", err)
			}
			callSeq := 0
			for _, item := range items {
				switch item.Type {
				case "", "message":
					m, err := responsesMessageToChat(item)
					if err != nil {
						return nil, err
					}
					messages = append(messages, m)
				case "function_call":
					callSeq++
					id := item.CallID
					if id == "" {
						// Synthesized when upstream history omits one.
						id = fmt.Sprintf("call_%d", callSeq)
					}
					args := item.Arguments
					if args == "" {
						args = "{}"
					}
					messages = append(messages, map[string]any{
						"role":    "assistant",
						"content": nil,
						"tool_calls": []any{map[string]any{
							"id":   id,
							"type": "function",
							"function": map[string]any{
								"name":      item.Name,
								"arguments": args,
							},
						}},
					})
				case "function_call_output":
					output, err := responsesContentText(item.Output)
					if err != nil {
						return nil, fmt.Errorf("function_call_output: %v", err)
					}
					messages = append(messages, map[string]any{
						"role":         "tool",
						"tool_call_id": item.CallID,
						"content":      output,
					})
				case "reasoning":
					// Dropped by prior decision: reasoning input items carry
					// nothing chat upstreams accept.
				default:
					return nil, fmt.Errorf("unsupported input item type %q", item.Type)
				}
			}
		}
	}

	out := map[string]any{
		"model":    req.Model,
		"messages": messages,
	}
	// The streamed surface dispatches into handleStream, which forwards SSE
	// frames — so the upstream must actually be asked to stream. Absent
	// otherwise (a non-streaming upstream answer would leave the client with
	// lifecycle events and empty output).
	if req.Stream != nil && *req.Stream {
		out["stream"] = true
	}
	if len(req.Tools) > 0 {
		tools := make([]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			if t.Type != "function" {
				// Skipped, not rejected (the guard no longer filters): the
				// model works with the function tools it receives. handleResponses
				// logs what was dropped.
				continue
			}
			fn := map[string]any{"name": t.Name}
			if t.Description != "" {
				fn["description"] = t.Description
			}
			if len(t.Parameters) > 0 && string(t.Parameters) != "null" {
				fn["parameters"] = t.Parameters
			}
			if t.Strict != nil {
				fn["strict"] = *t.Strict
			}
			tools = append(tools, map[string]any{"type": "function", "function": fn})
		}
		if len(tools) > 0 {
			out["tools"] = tools
		}
	}
	tc, err := translateResponsesToolChoice(req.ToolChoice)
	if err != nil {
		return nil, err
	}
	if tc != nil {
		out["tool_choice"] = tc
	}
	if req.ParallelToolCalls != nil {
		out["parallel_tool_calls"] = *req.ParallelToolCalls
	}
	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}
	if req.MaxOutputTokens != nil {
		out["max_tokens"] = *req.MaxOutputTokens
	}
	if req.Text != nil && len(req.Text.Format) > 0 && string(req.Text.Format) != "null" {
		out["response_format"] = req.Text.Format
	}
	if req.Reasoning != nil && req.Reasoning.Effort != "" {
		out["reasoning_effort"] = req.Reasoning.Effort
	}
	return json.Marshal(out)
}

// responsesMessageToChat converts one Responses message item into a chat
// message. developer is accepted as an input role but maps to chat system:
// Codex CLI 0.153 sends instructions as developer-role message items, and
// chat upstreams reject role:"developer" (z.ai/GLM: error 1214 "Incorrect
// role information" — only system/user/assistant/tool are accepted).
func responsesMessageToChat(item responsesItem) (map[string]any, error) {
	role := item.Role
	switch role {
	case "user", "assistant", "system", "developer":
	default:
		return nil, fmt.Errorf("unsupported message role %q", item.Role)
	}
	if role == "developer" {
		role = "system"
	}
	text, err := responsesContentText(item.Content)
	if err != nil {
		return nil, fmt.Errorf("message role %q: %v", item.Role, err)
	}
	return map[string]any{"role": role, "content": text}, nil
}

// responsesContentText extracts plain text from a Responses content field: a
// string stays as-is, an array joins the text of input_text/output_text
// parts, null/absent becomes "". Any other part type is an error — the shim
// is text-only.
func responsesContentText(content json.RawMessage) (string, error) {
	if len(content) == 0 || string(content) == "null" {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(content, &s); err == nil {
		return s, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(content, &parts); err != nil {
		return "", fmt.Errorf("invalid content: %s", snippet(string(content), 64))
	}
	var b strings.Builder
	for _, p := range parts {
		switch p.Type {
		case "input_text", "output_text":
			b.WriteString(p.Text)
		default:
			return "", fmt.Errorf("unsupported content part type %q", p.Type)
		}
	}
	return b.String(), nil
}

// translateResponsesToolChoice maps Responses tool_choice to the chat form:
// string forms pass through; a function object {"type":"function","name":N}
// becomes the chat nested form. Any other shape is rejected.
func translateResponsesToolChoice(raw json.RawMessage) (any, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch s {
		case "none", "auto", "required":
			return s, nil
		}
		return nil, fmt.Errorf("unsupported tool_choice %q", s)
	}
	var obj struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("invalid tool_choice: %s", snippet(string(raw), 64))
	}
	if obj.Type == "function" && obj.Name != "" {
		return map[string]any{"type": "function", "function": map[string]any{"name": obj.Name}}, nil
	}
	return nil, fmt.Errorf("unsupported tool_choice: only {\"type\":\"function\",\"name\":...} is accepted")
}

// ---------- response translation ----------

// chatCompletionToResponse converts a non-stream chat.completion body into an
// OpenAI Responses response object for the client only (capture keeps the
// chat shape). respID is the request's pre-generated resp_ id. An
// unparseable body is returned unchanged rather than fabricating a response
// object (mirrors translateUpstreamBody's passthrough).
func chatCompletionToResponse(respID string, chatBody []byte) []byte {
	var c struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content          json.RawMessage `json:"content"`
				ReasoningContent string          `json:"reasoning_content"`
				ToolCalls        []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens            int64 `json:"prompt_tokens"`
			CompletionTokens        int64 `json:"completion_tokens"`
			TotalTokens             int64 `json:"total_tokens"`
			CompletionTokensDetails *struct {
				ReasoningTokens int64 `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(chatBody, &c); err != nil {
		return chatBody
	}

	finish := ""
	output := make([]any, 0, 2)
	if len(c.Choices) > 0 {
		ch := c.Choices[0]
		finish = ch.FinishReason
		if ch.Message.ReasoningContent != "" {
			output = append(output, map[string]any{
				"type": "reasoning",
				"id":   "rs_" + store.NewID(),
				"summary": []any{map[string]any{
					"type": "summary_text",
					"text": ch.Message.ReasoningContent,
				}},
			})
		}
		output = append(output, map[string]any{
			"type":   "message",
			"id":     "msg_" + store.NewID(),
			"status": "completed",
			"role":   "assistant",
			"content": []any{map[string]any{
				"type":        "output_text",
				"text":        contentText(ch.Message.Content),
				"annotations": []any{},
			}},
		})
		for _, call := range ch.Message.ToolCalls {
			// call_id and item id reuse the chat call id, so follow-up
			// function_call_output turns round-trip unchanged.
			output = append(output, map[string]any{
				"type":      "function_call",
				"id":        call.ID,
				"call_id":   call.ID,
				"name":      call.Function.Name,
				"arguments": call.Function.Arguments,
				"status":    "completed",
			})
		}
	}

	resp := map[string]any{
		"id":         respID,
		"object":     "response",
		"created_at": time.Now().Unix(),
		"model":      c.Model,
		"output":     output,
		"status":     "completed",
	}
	if finish == "length" {
		resp["status"] = "incomplete"
		resp["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}
	if c.Usage != nil {
		usage := map[string]any{
			"input_tokens":  c.Usage.PromptTokens,
			"output_tokens": c.Usage.CompletionTokens,
			"total_tokens":  c.Usage.TotalTokens,
		}
		if c.Usage.CompletionTokensDetails != nil {
			usage["output_tokens_details"] = map[string]any{
				"reasoning_tokens": c.Usage.CompletionTokensDetails.ReasoningTokens,
			}
		}
		resp["usage"] = usage
	}
	b, err := json.Marshal(resp)
	if err != nil {
		return chatBody
	}
	return b
}

// ---------- error writer ----------

// writeResponsesError writes the OpenAI error envelope Responses clients
// expect: {"error":{message,type,param,code}}. The chat surface keeps its own
// §4.2 {code,message} shape via writeError.
func (s *Server) writeResponsesError(w http.ResponseWriter, status int, typ, param, message string) {
	errObj := map[string]any{"message": message, "type": typ, "param": nil, "code": nil}
	if param != "" {
		errObj["param"] = param
	}
	writeJSON(w, status, map[string]any{"error": errObj})
	s.logger.Warn("request_rejected", map[string]any{"status": status, "code": typ, "message": message})
}
