package gateway

import (
	"encoding/json"
	"strconv"
	"strings"
	"sync"

	"shimmer-llmgateway/internal/store"
)

// completionAssembler reassembles chat.completion.chunk SSE events into a
// single chat.completion object, running concurrently with the forward loop:
//
//   - per-choice message content deltas are concatenated;
//   - per-index tool_calls[].function.arguments deltas are concatenated, keyed
//     by the tool_call index field so interleaved chunks of tool_call[0] and
//     tool_call[1] stay index-stable;
//   - the first event's id/created/model and the last event's usage are kept;
//   - a data line carrying {"error": ...} is recorded (and forwarded
//     verbatim) without terminating the stream.
type completionAssembler struct {
	mu          sync.Mutex
	choices     map[int]*choiceState
	choiceOrder []int
	id          string
	created     int64
	model       string
	usage       json.RawMessage
	chunkCount  int
	streamErr   *store.ErrorInfo
}

type choiceState struct {
	index        int
	role         string
	content      strings.Builder
	toolCalls    map[int]*toolCallState
	toolOrder    []int
	finishReason string
}

type toolCallState struct {
	index     int
	id        string
	typ       string
	name      string
	arguments strings.Builder
}

func newAssembler() *completionAssembler {
	return &completionAssembler{choices: map[int]*choiceState{}}
}

// add folds one SSE data payload into the reassembly. Non-chunk payloads
// ([DONE], non-JSON) are ignored; the forward loop passes them through
// verbatim.
func (a *completionAssembler) add(payload []byte) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if string(payload) == "[DONE]" {
		return
	}
	var ev struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		Model   string `json:"model"`
		Choices []struct {
			Index int `json:"index"`
			Delta struct {
				Role      string  `json:"role"`
				Content   *string `json:"content"`
				ToolCalls []struct {
					Index    *int   `json:"index"`
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		Usage json.RawMessage `json:"usage"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(payload, &ev); err != nil {
		return
	}
	if len(ev.Error) > 0 {
		a.streamErr = streamError(ev.Error)
		return
	}
	if a.id == "" {
		a.id = ev.ID
	}
	if a.created == 0 {
		a.created = ev.Created
	}
	if a.model == "" {
		a.model = ev.Model
	}
	if len(ev.Usage) > 0 {
		a.usage = append(json.RawMessage(nil), ev.Usage...)
	}
	a.chunkCount++
	for _, c := range ev.Choices {
		ch := a.choice(c.Index)
		if c.Delta.Role != "" {
			ch.role = c.Delta.Role
		}
		if c.Delta.Content != nil {
			ch.content.WriteString(*c.Delta.Content)
		}
		for _, tc := range c.Delta.ToolCalls {
			idx := len(ch.toolOrder) // fallback: delta array position
			if tc.Index != nil {
				idx = *tc.Index
			}
			tcs := ch.tool(idx)
			if tc.ID != "" {
				tcs.id = tc.ID
			}
			if tc.Type != "" {
				tcs.typ = tc.Type
			}
			if tc.Function.Name != "" {
				tcs.name = tc.Function.Name
			}
			tcs.arguments.WriteString(tc.Function.Arguments)
		}
		if c.FinishReason != nil && *c.FinishReason != "" {
			ch.finishReason = *c.FinishReason
		}
	}
}

func (a *completionAssembler) choice(index int) *choiceState {
	ch, ok := a.choices[index]
	if !ok {
		ch = &choiceState{index: index, toolCalls: map[int]*toolCallState{}}
		a.choices[index] = ch
		a.choiceOrder = append(a.choiceOrder, index)
	}
	return ch
}

func (ch *choiceState) tool(index int) *toolCallState {
	tc, ok := ch.toolCalls[index]
	if !ok {
		tc = &toolCallState{index: index}
		ch.toolCalls[index] = tc
		ch.toolOrder = append(ch.toolOrder, index)
	}
	return tc
}

// streamError returns the error recorded from an SSE error event, if any.
func (a *completionAssembler) streamError() *store.ErrorInfo {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.streamErr
}

// chunks returns the number of data chunks folded into the reassembly.
func (a *completionAssembler) chunks() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.chunkCount
}

// choiceJSON mirrors a non-stream chat.completion choice.
type choiceJSON struct {
	Index        int            `json:"index"`
	Message      map[string]any `json:"message"`
	FinishReason string         `json:"finish_reason"`
}

// result returns the reassembled completion JSON, the first choice's
// finish_reason, and the captured usage.
func (a *completionAssembler) result() (json.RawMessage, string, json.RawMessage) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var resp struct {
		ID      string          `json:"id"`
		Object  string          `json:"object"`
		Created int64           `json:"created"`
		Model   string          `json:"model"`
		Choices []*choiceJSON   `json:"choices"`
		Usage   json.RawMessage `json:"usage,omitempty"`
	}
	resp.ID = a.id
	resp.Object = "chat.completion"
	resp.Created = a.created
	resp.Model = a.model
	for _, idx := range a.choiceOrder {
		ch := a.choices[idx]
		role := ch.role
		if role == "" {
			role = "assistant"
		}
		msg := map[string]any{"role": role}
		if len(ch.toolOrder) > 0 && ch.content.Len() == 0 {
			msg["content"] = nil
		} else {
			msg["content"] = ch.content.String()
		}
		if len(ch.toolOrder) > 0 {
			calls := make([]map[string]any, 0, len(ch.toolOrder))
			for _, ti := range ch.toolOrder {
				tc := ch.toolCalls[ti]
				tcType := tc.typ
				if tcType == "" {
					tcType = "function"
				}
				calls = append(calls, map[string]any{
					"id":       tc.id,
					"type":     tcType,
					"function": map[string]any{"name": tc.name, "arguments": tc.arguments.String()},
				})
			}
			msg["tool_calls"] = calls
		}
		resp.Choices = append(resp.Choices, &choiceJSON{
			Index:        ch.index,
			Message:      msg,
			FinishReason: ch.finishReason,
		})
	}
	if len(a.usage) > 0 {
		resp.Usage = a.usage
	}
	b, err := json.Marshal(resp)
	if err != nil {
		return nil, "", nil
	}
	finish := ""
	if len(resp.Choices) > 0 {
		finish = resp.Choices[0].FinishReason
	}
	return b, finish, a.usage
}

// streamError extracts {code, message} from an OpenAI-shaped SSE error event.
func streamError(raw json.RawMessage) *store.ErrorInfo {
	var e struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    any    `json:"code"`
	}
	if err := json.Unmarshal(raw, &e); err != nil {
		return &store.ErrorInfo{Code: "STREAM_ERROR", Message: string(raw)}
	}
	code := stringOf(e.Code)
	if code == "" {
		code = e.Type
	}
	if code == "" {
		code = "STREAM_ERROR"
	}
	if e.Message == "" {
		e.Message = string(raw)
	}
	return &store.ErrorInfo{Code: code, Message: e.Message}
}

// stringOf normalizes a JSON error code (string or number) to string.
func stringOf(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	}
	return ""
}
