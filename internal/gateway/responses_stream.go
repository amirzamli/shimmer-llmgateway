package gateway

// Responses SSE streaming (Phase 2): a stateful translator turns the
// chat.completion.chunk payloads the shared read loop forwards into the typed
// Responses event sequence (response.created → per-item added/delta/done →
// response.completed), and responsesEmitter implements the Emitter seam for
// the /v1/responses surface. Live mode translates per chunk and flushes per
// event; buffered mode (response plugins configured) holds, filters the
// reassembled CHAT body (stored as emitted, so ResponseFilteredJSON stays
// chat-shaped), converts it once via chatCompletionToResponse, and emits the
// whole event sequence as one burst. Upstream mid-stream {"error":...}
// payloads become response.failed + error and the translator no-ops
// afterwards; the chat [DONE] sentinel is swallowed (this surface has no
// trailing [DONE]). Item state — msg_/fc_/rs_ ids, output indices, accumulated
// text/arguments — lives here; the completionAssembler is untouched and keeps
// not folding reasoning_content.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/amirzamli/shimmer-llmgateway/internal/store"
)

// responseEvent is one typed Responses SSE event: the event: line carries
// typ, the data: line the full JSON object (type included) with the event's
// monotonic sequence_number.
type responseEvent struct {
	typ    string
	fields map[string]any
}

// data marshals the event's data payload (type included).
func (ev responseEvent) data() ([]byte, error) {
	m := make(map[string]any, len(ev.fields)+1)
	for k, v := range ev.fields {
		m[k] = v
	}
	m["type"] = ev.typ
	return json.Marshal(m)
}

// responsesItemState is one open output item, kept in first-appearance order.
type responsesItemState struct {
	kind      string          // "reasoning" | "message" | "function_call"
	id        string          // msg_/rs_/fc_ item id
	callID    string          // function_call: the upstream chat call id (round-trips)
	name      string          // function_call
	text      strings.Builder // message / reasoning accumulated text
	args      strings.Builder // function_call accumulated arguments
	outputIdx int
}

// finalItemObject is the item shape carried by response.output_item.done and
// embedded in the response.completed output array.
func finalItemObject(item *responsesItemState) map[string]any {
	switch item.kind {
	case "reasoning":
		return map[string]any{
			"type": "reasoning",
			"id":   item.id,
			"summary": []any{map[string]any{
				"type": "summary_text",
				"text": item.text.String(),
			}},
		}
	case "function_call":
		return map[string]any{
			"type":      "function_call",
			"id":        item.id,
			"call_id":   item.callID,
			"name":      item.name,
			"arguments": item.args.String(),
			"status":    "completed",
		}
	}
	return map[string]any{
		"type":   "message",
		"id":     item.id,
		"status": "completed",
		"role":   "assistant",
		"content": []any{map[string]any{
			"type":        "output_text",
			"text":        item.text.String(),
			"annotations": []any{},
		}},
	}
}

// responsesStreamState is the stateful chunk→events translator (the
// anthropicStreamState precedent): it consumes chat chunk payloads and emits
// zero or more typed events with a monotonic sequence_number starting at 0.
// Only the first choice is translated, mirroring chatCompletionToResponse.
type responsesStreamState struct {
	respID  string
	model   string
	created int64
	seq     int
	started bool // response.created/response.in_progress emitted
	failed  bool // upstream error seen: no-op every subsequent chunk
	items   []*responsesItemState
	msgItem *responsesItemState
	rsItem  *responsesItemState
	// toolIndex maps a chat tool_call delta index to its item so interleaved
	// deltas stay index-stable (the assembler's own rule).
	toolIndex map[int]*responsesItemState
}

func newResponsesStreamState(respID, model string) *responsesStreamState {
	return &responsesStreamState{
		respID:    respID,
		model:     model,
		created:   time.Now().Unix(),
		toolIndex: map[int]*responsesItemState{},
	}
}

func (st *responsesStreamState) nextSeq() int {
	n := st.seq
	st.seq++
	return n
}

// translate consumes one chat chunk payload and emits zero or more typed
// Responses events. [DONE] and unparseable payloads emit nothing.
func (st *responsesStreamState) translate(payload []byte) []responseEvent {
	if st.failed || string(payload) == "[DONE]" {
		return nil
	}
	var ev struct {
		Model   string `json:"model"`
		Choices []struct {
			Delta struct {
				Content          *string `json:"content"`
				Reasoning        *string `json:"reasoning"`
				ReasoningContent *string `json:"reasoning_content"`
				ToolCalls        []struct {
					Index    *int   `json:"index"`
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
		} `json:"choices"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(payload, &ev); err != nil {
		return nil
	}
	if len(ev.Error) > 0 {
		return st.fail(ev.Error)
	}
	if ev.Model != "" {
		st.model = ev.Model
	}

	var events []responseEvent
	st.begin(&events)
	if len(ev.Choices) == 0 {
		return events
	}
	d := ev.Choices[0].Delta
	if text := reasoningText(d.Reasoning, d.ReasoningContent); text != "" {
		item, fresh := st.reasoningItem()
		if fresh {
			st.itemAdded(&events, item)
		}
		item.text.WriteString(text)
		events = append(events, responseEvent{"response.reasoning_summary_text.delta", map[string]any{
			"sequence_number": st.nextSeq(),
			"item_id":         item.id,
			"output_index":    item.outputIdx,
			"summary_index":   0,
			"delta":           text,
		}})
	}
	if d.Content != nil && *d.Content != "" {
		item, fresh := st.messageItem()
		if fresh {
			st.itemAdded(&events, item)
		}
		item.text.WriteString(*d.Content)
		events = append(events, responseEvent{"response.output_text.delta", map[string]any{
			"sequence_number": st.nextSeq(),
			"item_id":         item.id,
			"output_index":    item.outputIdx,
			"content_index":   0,
			"delta":           *d.Content,
		}})
	}
	for i, tc := range d.ToolCalls {
		idx := i // fallback: delta array position
		if tc.Index != nil {
			idx = *tc.Index
		}
		item, fresh := st.toolItem(idx)
		if tc.ID != "" {
			item.callID = tc.ID
		}
		if tc.Function.Name != "" {
			item.name = tc.Function.Name
		}
		if fresh {
			st.itemAdded(&events, item)
		}
		if tc.Function.Arguments != "" {
			item.args.WriteString(tc.Function.Arguments)
			events = append(events, responseEvent{"response.function_call_arguments.delta", map[string]any{
				"sequence_number": st.nextSeq(),
				"item_id":         item.id,
				"output_index":    item.outputIdx,
				"delta":           tc.Function.Arguments,
			}})
		}
	}
	return events
}

// begin emits response.created + response.in_progress once, on the first
// translatable chunk.
func (st *responsesStreamState) begin(events *[]responseEvent) {
	if st.started {
		return
	}
	st.started = true
	skeleton := st.inProgressSkeleton()
	*events = append(*events,
		responseEvent{"response.created", map[string]any{
			"sequence_number": st.nextSeq(), "response": skeleton,
		}},
		responseEvent{"response.in_progress", map[string]any{
			"sequence_number": st.nextSeq(), "response": skeleton,
		}},
	)
}

// inProgressSkeleton is the response object carried by created/in_progress:
// status in_progress, empty output.
func (st *responsesStreamState) inProgressSkeleton() map[string]any {
	return map[string]any{
		"id":         st.respID,
		"object":     "response",
		"created_at": st.created,
		"status":     "in_progress",
		"model":      st.model,
		"output":     []any{},
	}
}

// fail turns an upstream mid-stream {"error": ...} payload into
// response.failed + error; subsequent chunks no-op and finish emits nothing
// (capture keeps the existing streamErr path).
func (st *responsesStreamState) fail(raw json.RawMessage) []responseEvent {
	info := streamError(raw)
	st.failed = true
	resp := st.inProgressSkeleton()
	resp["status"] = "failed"
	resp["error"] = map[string]any{"code": info.Code, "message": info.Message}
	return []responseEvent{
		{"response.failed", map[string]any{
			"sequence_number": st.nextSeq(), "response": resp,
		}},
		{"error", map[string]any{
			"sequence_number": st.nextSeq(), "code": info.Code, "message": info.Message, "param": nil,
		}},
	}
}

// finish emits the closing sequence at stream end: the item .done events for
// everything still open, then response.completed embedding the full response
// object. finishReason/usage come from the reassembled chat body (asm.result()
// via the emitter's source) at Done() time. A failed stream emits nothing.
func (st *responsesStreamState) finish(finishReason string, usage json.RawMessage) []responseEvent {
	if st.failed {
		return nil
	}
	var events []responseEvent
	st.begin(&events)
	for _, item := range st.items {
		events = append(events, st.itemDoneEvents(item)...)
	}
	events = append(events, responseEvent{"response.completed", map[string]any{
		"sequence_number": st.nextSeq(),
		"response":        st.finalResponse(finishReason, usage),
	}})
	return events
}

// itemDoneEvents closes one item: the payload .done event(s), then
// response.output_item.done with the final item object. Sequence numbers are
// allocated in emission order.
func (st *responsesStreamState) itemDoneEvents(item *responsesItemState) []responseEvent {
	done := func(typ string, fields map[string]any) responseEvent {
		fields["sequence_number"] = st.nextSeq()
		return responseEvent{typ, fields}
	}
	outputDone := func() responseEvent {
		return responseEvent{"response.output_item.done", map[string]any{
			"sequence_number": st.nextSeq(),
			"output_index":    item.outputIdx,
			"item":            finalItemObject(item),
		}}
	}
	switch item.kind {
	case "reasoning":
		return []responseEvent{
			done("response.reasoning_summary_text.done", map[string]any{
				"item_id": item.id, "output_index": item.outputIdx, "summary_index": 0, "text": item.text.String(),
			}),
			outputDone(),
		}
	case "function_call":
		return []responseEvent{
			done("response.function_call_arguments.done", map[string]any{
				"item_id": item.id, "output_index": item.outputIdx, "arguments": item.args.String(),
			}),
			outputDone(),
		}
	}
	text := item.text.String()
	return []responseEvent{
		done("response.output_text.done", map[string]any{
			"item_id": item.id, "output_index": item.outputIdx, "content_index": 0, "text": text,
		}),
		done("response.content_part.done", map[string]any{
			"item_id": item.id, "output_index": item.outputIdx, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": text, "annotations": []any{}},
		}),
		outputDone(),
	}
}

// finalResponse builds the response.completed payload from the accumulated
// item state (ids match the events already emitted) plus the mapped usage.
func (st *responsesStreamState) finalResponse(finishReason string, usage json.RawMessage) map[string]any {
	output := make([]any, 0, len(st.items))
	for _, item := range st.items {
		output = append(output, finalItemObject(item))
	}
	resp := map[string]any{
		"id":         st.respID,
		"object":     "response",
		"created_at": st.created,
		"model":      st.model,
		"output":     output,
		"status":     "completed",
	}
	if finishReason == "length" {
		resp["status"] = "incomplete"
		resp["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}
	if u := responsesUsageFromChat(usage); u != nil {
		resp["usage"] = u
	}
	return resp
}

// burst emits the full Responses event sequence for a final response object
// (buffered mode: the response plugins have already run on the reassembled
// chat body and chatCompletionToResponse produced the client object). The
// ordering mirrors the live sequence exactly — created/in_progress, then per
// item added + payload deltas, then per item payload .done + output_item.done,
// finally response.completed — with each item's payload riding a single delta.
// An unparseable object emits nothing (the capture keeps the chat body).
func (st *responsesStreamState) burst(respObj []byte) []responseEvent {
	var ro struct {
		ID        string            `json:"id"`
		Model     string            `json:"model"`
		CreatedAt int64             `json:"created_at"`
		Output    []json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal(respObj, &ro); err != nil {
		return nil
	}
	seq := 0
	next := func() int { seq++; return seq - 1 }
	skeleton := func() map[string]any {
		return map[string]any{
			"id": ro.ID, "object": "response", "created_at": ro.CreatedAt,
			"status": "in_progress", "model": ro.Model, "output": []any{},
		}
	}
	events := []responseEvent{
		{"response.created", map[string]any{"sequence_number": next(), "response": skeleton()}},
		{"response.in_progress", map[string]any{"sequence_number": next(), "response": skeleton()}},
	}
	type burstItem struct {
		raw    json.RawMessage
		kind   string
		id     string
		text   string
		args   string
		callID string
		name   string
	}
	items := make([]burstItem, 0, len(ro.Output))
	for i, raw := range ro.Output {
		var item struct {
			Type      string `json:"type"`
			ID        string `json:"id"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
			Content   []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			Summary []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"summary"`
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			continue
		}
		bi := burstItem{raw: raw, kind: item.Type, id: item.ID, args: item.Arguments, callID: item.CallID, name: item.Name}
		added := map[string]any{"type": item.Type, "id": item.ID, "status": "in_progress"}
		switch item.Type {
		case "reasoning":
			delete(added, "status")
			added["summary"] = []any{}
			if len(item.Summary) > 0 {
				bi.text = item.Summary[0].Text
			}
		case "function_call":
			added["call_id"] = item.CallID
			added["name"] = item.Name
			added["arguments"] = ""
		default:
			added["role"] = "assistant"
			added["content"] = []any{}
			if len(item.Content) > 0 {
				bi.text = item.Content[0].Text
			}
		}
		events = append(events, responseEvent{"response.output_item.added", map[string]any{
			"sequence_number": next(), "output_index": i, "item": added,
		}})
		switch item.Type {
		case "reasoning":
			events = append(events, responseEvent{"response.reasoning_summary_text.delta", map[string]any{
				"sequence_number": next(), "item_id": item.ID, "output_index": i, "summary_index": 0, "delta": bi.text,
			}})
		case "function_call":
			events = append(events, responseEvent{"response.function_call_arguments.delta", map[string]any{
				"sequence_number": next(), "item_id": item.ID, "output_index": i, "delta": bi.args,
			}})
		default:
			events = append(events,
				responseEvent{"response.content_part.added", map[string]any{
					"sequence_number": next(), "item_id": item.ID, "output_index": i, "content_index": 0,
					"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
				}},
				responseEvent{"response.output_text.delta", map[string]any{
					"sequence_number": next(), "item_id": item.ID, "output_index": i, "content_index": 0, "delta": bi.text,
				}},
			)
		}
		items = append(items, bi)
	}
	// Finalization mirrors finish(): payload .done events, then
	// output_item.done per item, then response.completed.
	for i := range items {
		bi := &items[i]
		switch bi.kind {
		case "reasoning":
			events = append(events, responseEvent{"response.reasoning_summary_text.done", map[string]any{
				"sequence_number": next(), "item_id": bi.id, "output_index": i, "summary_index": 0, "text": bi.text,
			}})
		case "function_call":
			events = append(events, responseEvent{"response.function_call_arguments.done", map[string]any{
				"sequence_number": next(), "item_id": bi.id, "output_index": i, "arguments": bi.args,
			}})
		default:
			events = append(events,
				responseEvent{"response.output_text.done", map[string]any{
					"sequence_number": next(), "item_id": bi.id, "output_index": i, "content_index": 0, "text": bi.text,
				}},
				responseEvent{"response.content_part.done", map[string]any{
					"sequence_number": next(), "item_id": bi.id, "output_index": i, "content_index": 0,
					"part": map[string]any{"type": "output_text", "text": bi.text, "annotations": []any{}},
				}},
			)
		}
		events = append(events, responseEvent{"response.output_item.done", map[string]any{
			"sequence_number": next(), "output_index": i, "item": bi.raw,
		}})
	}
	events = append(events, responseEvent{"response.completed", map[string]any{
		"sequence_number": next(), "response": json.RawMessage(respObj),
	}})
	return events
}

// addItem appends a first-appearance item and returns it.
func (st *responsesStreamState) addItem(kind string) *responsesItemState {
	prefix := "msg_"
	switch kind {
	case "reasoning":
		prefix = "rs_"
	case "function_call":
		prefix = "fc_"
	}
	item := &responsesItemState{
		kind:      kind,
		id:        prefix + store.NewID(),
		outputIdx: len(st.items),
	}
	st.items = append(st.items, item)
	return item
}

func (st *responsesStreamState) reasoningItem() (*responsesItemState, bool) {
	if st.rsItem == nil {
		st.rsItem = st.addItem("reasoning")
		return st.rsItem, true
	}
	return st.rsItem, false
}

func (st *responsesStreamState) messageItem() (*responsesItemState, bool) {
	if st.msgItem == nil {
		st.msgItem = st.addItem("message")
		return st.msgItem, true
	}
	return st.msgItem, false
}

func (st *responsesStreamState) toolItem(chatIndex int) (*responsesItemState, bool) {
	item, ok := st.toolIndex[chatIndex]
	if !ok {
		item = st.addItem("function_call")
		st.toolIndex[chatIndex] = item
		return item, true
	}
	return item, false
}

// itemAdded emits response.output_item.added (plus content_part.added for
// messages) when an item first appears.
func (st *responsesStreamState) itemAdded(events *[]responseEvent, item *responsesItemState) {
	added := map[string]any{"type": item.kind, "id": item.id, "status": "in_progress"}
	switch item.kind {
	case "reasoning":
		delete(added, "status")
		added["summary"] = []any{}
	case "function_call":
		added["call_id"] = item.callID
		added["name"] = item.name
		added["arguments"] = ""
	default:
		added["role"] = "assistant"
		added["content"] = []any{}
	}
	*events = append(*events, responseEvent{"response.output_item.added", map[string]any{
		"sequence_number": st.nextSeq(), "output_index": item.outputIdx, "item": added,
	}})
	if item.kind == "message" {
		*events = append(*events, responseEvent{"response.content_part.added", map[string]any{
			"sequence_number": st.nextSeq(), "item_id": item.id, "output_index": item.outputIdx, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
		}})
	}
}

// reasoningText picks the reasoning delta text from the two field names
// upstreams use (never both).
func reasoningText(reasoning, reasoningContent *string) string {
	if reasoningContent != nil && *reasoningContent != "" {
		return *reasoningContent
	}
	if reasoning != nil {
		return *reasoning
	}
	return ""
}

// responsesUsageFromChat maps a chat usage object to the Responses shape
// (prompt/completion/total → input/output/total, reasoning tokens nested
// under output_tokens_details). Absent/nil raw → nil.
func responsesUsageFromChat(raw json.RawMessage) map[string]any {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var u struct {
		PromptTokens            int64 `json:"prompt_tokens"`
		CompletionTokens        int64 `json:"completion_tokens"`
		TotalTokens             int64 `json:"total_tokens"`
		CompletionTokensDetails *struct {
			ReasoningTokens int64 `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	}
	if err := json.Unmarshal(raw, &u); err != nil {
		return nil
	}
	usage := map[string]any{
		"input_tokens":  u.PromptTokens,
		"output_tokens": u.CompletionTokens,
		"total_tokens":  u.TotalTokens,
	}
	if u.CompletionTokensDetails != nil {
		usage["output_tokens_details"] = map[string]any{
			"reasoning_tokens": u.CompletionTokensDetails.ReasoningTokens,
		}
	}
	return usage
}

// chatFinishUsage extracts the first choice's finish_reason and the usage the
// assembler folded into a reassembled chat.completion body.
func chatFinishUsage(body []byte) (string, json.RawMessage) {
	var c struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(body, &c); err != nil {
		return "", nil
	}
	var finish string
	if len(c.Choices) > 0 {
		finish = c.Choices[0].FinishReason
	}
	return finish, c.Usage
}

// responsesEmitter is the /v1/responses Emitter: live mode translates each
// forwarded chat SSE line into typed Responses events with a flush per event;
// buffered mode (response plugins configured) holds every line, filters the
// reassembled CHAT body at Done (stored as emitted so the capture record's
// response_filtered_json stays chat-shaped), converts it once via
// chatCompletionToResponse, and emits the full event sequence as one burst.
type responsesEmitter struct {
	w      io.Writer
	flush  http.Flusher
	st     *responsesStreamState
	source EmitterSource
	filter EmitterFilter

	buffered bool
	emitted  json.RawMessage
}

// newResponsesStreamEmitter builds the surface's emitter factory, binding a
// fresh translator to the request's pre-generated resp_ id and resolved model.
func newResponsesStreamEmitter(respID, model string) func(http.ResponseWriter, EmitterSource, EmitterFilter) Emitter {
	return func(w http.ResponseWriter, source EmitterSource, filter EmitterFilter) Emitter {
		return &responsesEmitter{
			w:        w,
			flush:    w.(http.Flusher),
			st:       newResponsesStreamState(respID, model),
			source:   source,
			filter:   filter,
			buffered: filter != nil,
		}
	}
}

func (e *responsesEmitter) Write(line []byte) error {
	if e.buffered {
		// Nothing is forwarded until the stream completes and the response
		// plugins have run post-reassembly (bufferedEmitter's tradeoff).
		return nil
	}
	payload := ssePayload(string(line))
	if payload == nil {
		// Keepalive comments and other non-data lines carry nothing to
		// translate; this surface never forwards them verbatim.
		return nil
	}
	return e.writeEvents(e.st.translate(payload))
}

// Done finalizes the sequence. Live mode closes the open items and emits
// response.completed from the reassembled chat body (finish_reason/usage are
// safe to consult — readStream has returned). Buffered mode runs the response
// plugins on the reassembled CHAT body, keeps those bytes for capture, and
// emits the translated burst from the converted response object.
func (e *responsesEmitter) Done() error {
	if !e.buffered {
		finish, usage := chatFinishUsage(e.source())
		return e.writeEvents(e.st.finish(finish, usage))
	}
	body := e.source()
	if e.filter != nil {
		if filtered, err := e.filter(body); err == nil {
			body = filtered
		}
		// On a plugin error the unfiltered reassembly is still delivered (the
		// gateway never drops the provider's data; the caller logs it).
	}
	// Capture keeps the filtered CHAT bytes; the client sees the burst.
	e.emitted = body
	return e.writeEvents(e.st.burst(chatCompletionToResponse(e.st.respID, body)))
}

// emittedBody returns the exact chat-shaped body the buffered burst was built
// from (nil in live mode); the capture record stores it as
// response_filtered_json.
func (e *responsesEmitter) emittedBody() json.RawMessage { return e.emitted }

// writeEvents writes each event as `event: <type>\ndata: {...}\n\n` with a
// flush after every event. A write error means the client is gone.
func (e *responsesEmitter) writeEvents(events []responseEvent) error {
	for _, ev := range events {
		data, err := ev.data()
		if err != nil {
			// Unreachable for translator-constructed events; never emit a
			// half-formed event instead.
			continue
		}
		if _, err := fmt.Fprintf(e.w, "event: %s\ndata: %s\n\n", ev.typ, data); err != nil {
			return err
		}
		e.flush.Flush()
	}
	return nil
}
