package gateway

// Phase 2 tests: the Responses SSE stream translation (chunk-sequence →
// ordered typed events, [DONE] swallowed, error payload → response.failed),
// the live/buffered emitter wiring end-to-end (event order, usage in
// response.completed, chat-shaped capture), and the inherited non-2xx and
// disconnect behaviors.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// typesOf extracts the ordered event type names from translator events.
func typesOf(events []responseEvent) []string {
	types := make([]string, len(events))
	for i, ev := range events {
		types[i] = ev.typ
	}
	return types
}

// assertSeqNumbers asserts the translator events carry sequence_number
// 0..n-1 in order.
func assertSeqNumbers(t *testing.T, events []responseEvent) {
	t.Helper()
	for i, ev := range events {
		seq, ok := ev.fields["sequence_number"].(int)
		if !ok {
			t.Fatalf("event %d (%s) has no sequence_number: %v", i, ev.typ, ev.fields)
		}
		if seq != i {
			t.Fatalf("event %d (%s) sequence_number = %d", i, ev.typ, seq)
		}
	}
}

// ---- translator unit tests ----

func TestResponsesStreamTranslateOrdering(t *testing.T) {
	st := newResponsesStreamState("resp_fixed", "gpt-4o")
	var events []responseEvent
	for _, chunk := range []string{
		`{"id":"c1","object":"chat.completion.chunk","created":1722600000,"model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":"Hel"},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"get_weather","arguments":""}}]},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":\"Par"}}]},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"is\"}"}}]},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	} {
		events = append(events, st.translate([]byte(chunk))...)
	}
	events = append(events, st.finish("tool_calls", json.RawMessage(`{"prompt_tokens":5,"completion_tokens":7,"total_tokens":12}`))...)

	want := []string{
		"response.created",
		"response.in_progress",
		"response.output_item.added", // message
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_text.delta",
		"response.output_item.added", // function_call
		"response.function_call_arguments.delta",
		"response.function_call_arguments.delta",
		"response.output_text.done",
		"response.content_part.done",
		"response.output_item.done", // message
		"response.function_call_arguments.done",
		"response.output_item.done", // function_call
		"response.completed",
	}
	if got := typesOf(events); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("event sequence:\n got %v\nwant %v", got, want)
	}
	assertSeqNumbers(t, events)

	// created carries the in-progress skeleton bound to the request's resp_ id.
	created := events[0].fields["response"].(map[string]any)
	if created["id"] != "resp_fixed" || created["status"] != "in_progress" || created["object"] != "response" {
		t.Errorf("created response = %v", created)
	}

	// The message item is output 0 (msg_ id), the function_call item output 1
	// (fc_ id, upstream call id round-tripped).
	if got := events[4].fields["item_id"].(string); !strings.HasPrefix(got, "msg_") {
		t.Errorf("text delta item_id = %q, want msg_ prefix", got)
	}
	if events[4].fields["output_index"] != 0 || events[4].fields["content_index"] != 0 {
		t.Errorf("text delta indices = %v", events[4].fields)
	}
	if got := events[7].fields["item_id"].(string); !strings.HasPrefix(got, "fc_") {
		t.Errorf("args delta item_id = %q, want fc_ prefix", got)
	}
	if events[7].fields["output_index"] != 1 {
		t.Errorf("args delta output_index = %v", events[7].fields["output_index"])
	}
	fcAdded := events[6].fields["item"].(map[string]any)
	if fcAdded["call_id"] != "call_a" || fcAdded["name"] != "get_weather" {
		t.Errorf("function_call added item = %v", fcAdded)
	}

	// completed embeds the full response: both items, mapped usage, status.
	completed := events[len(events)-1].fields["response"].(map[string]any)
	if completed["id"] != "resp_fixed" || completed["status"] != "completed" {
		t.Errorf("completed response id/status = %v/%v", completed["id"], completed["status"])
	}
	output := completed["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("completed output = %v", output)
	}
	msg := output[0].(map[string]any)
	text := msg["content"].([]any)[0].(map[string]any)["text"]
	if text != "Hello" {
		t.Errorf("message text = %v, want Hello", text)
	}
	fc := output[1].(map[string]any)
	if fc["call_id"] != "call_a" || fc["arguments"] != `{"city":"Paris"}` || fc["status"] != "completed" {
		t.Errorf("function_call item = %v", fc)
	}
	usage := completed["usage"].(map[string]any)
	if usage["input_tokens"] != int64(5) || usage["output_tokens"] != int64(7) || usage["total_tokens"] != int64(12) {
		t.Errorf("usage = %v", usage)
	}
}

func TestResponsesStreamReasoningItemEvents(t *testing.T) {
	st := newResponsesStreamState("resp_1", "m")
	var events []responseEvent
	// Both upstream field names surface: reasoning_content first, then the
	// `reasoning` fallback.
	events = append(events, st.translate([]byte(`{"choices":[{"index":0,"delta":{"reasoning_content":"thin"}}]}`))...)
	events = append(events, st.translate([]byte(`{"choices":[{"index":0,"delta":{"reasoning":"king"}}]}`))...)
	events = append(events, st.translate([]byte(`{"choices":[{"index":0,"delta":{"content":"answer"}}]}`))...)
	events = append(events, st.finish("stop", nil)...)

	want := []string{
		"response.created",
		"response.in_progress",
		"response.output_item.added", // reasoning
		"response.reasoning_summary_text.delta",
		"response.reasoning_summary_text.delta",
		"response.output_item.added", // message
		"response.content_part.added",
		"response.output_text.delta",
		"response.reasoning_summary_text.done",
		"response.output_item.done", // reasoning
		"response.output_text.done",
		"response.content_part.done",
		"response.output_item.done", // message
		"response.completed",
	}
	if got := typesOf(events); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("event sequence:\n got %v\nwant %v", got, want)
	}

	// The reasoning item precedes the message: output indices 0 and 1.
	rsAdded := events[2].fields["item"].(map[string]any)
	if rsAdded["type"] != "reasoning" || !strings.HasPrefix(rsAdded["id"].(string), "rs_") {
		t.Errorf("reasoning added item = %v", rsAdded)
	}
	if events[3].fields["summary_index"] != 0 || events[3].fields["delta"] != "thin" {
		t.Errorf("reasoning delta = %v", events[3].fields)
	}
	if events[5].fields["output_index"] != 1 {
		t.Errorf("message output_index = %v, want 1", events[5].fields["output_index"])
	}
	completed := events[len(events)-1].fields["response"].(map[string]any)
	rsItem := completed["output"].([]any)[0].(map[string]any)
	summary := rsItem["summary"].([]any)[0].(map[string]any)
	if summary["text"] != "thinking" {
		t.Errorf("reasoning summary text = %v, want thinking", summary["text"])
	}
}

func TestResponsesStreamDoneSwallowedAndErrorPayload(t *testing.T) {
	st := newResponsesStreamState("resp_1", "m")
	if got := st.translate([]byte("[DONE]")); len(got) != 0 {
		t.Fatalf("[DONE] emitted %d events, want 0", len(got))
	}

	events := st.translate([]byte(`{"error":{"message":"boom","type":"server_error","code":null}}`))
	if len(events) != 2 || events[0].typ != "response.failed" || events[1].typ != "error" {
		t.Fatalf("error payload events = %v", typesOf(events))
	}
	failed := events[0].fields["response"].(map[string]any)
	if failed["status"] != "failed" {
		t.Errorf("failed response status = %v", failed["status"])
	}
	errObj := failed["error"].(map[string]any)
	if errObj["message"] != "boom" || errObj["code"] != "server_error" {
		t.Errorf("failed response error = %v", errObj)
	}
	if events[1].fields["message"] != "boom" {
		t.Errorf("error event = %v", events[1].fields)
	}

	// Subsequent chunks and finalization no-op after the failure.
	if got := st.translate([]byte(`{"choices":[{"index":0,"delta":{"content":"x"}}]}`)); len(got) != 0 {
		t.Errorf("post-error chunk emitted %v", typesOf(got))
	}
	if got := st.finish("stop", nil); len(got) != 0 {
		t.Errorf("post-error finish emitted %v", typesOf(got))
	}
}

func TestResponsesStreamLengthIncomplete(t *testing.T) {
	st := newResponsesStreamState("resp_1", "m")
	st.translate([]byte(`{"choices":[{"index":0,"delta":{"content":"partial"}}]}`))
	events := st.finish("length", nil)
	completed := events[len(events)-1]
	if completed.typ != "response.completed" {
		t.Fatalf("last event = %s, want response.completed", completed.typ)
	}
	resp := completed.fields["response"].(map[string]any)
	if resp["status"] != "incomplete" {
		t.Errorf("status = %v, want incomplete", resp["status"])
	}
	details, ok := resp["incomplete_details"].(map[string]any)
	if !ok || details["reason"] != "max_output_tokens" {
		t.Errorf("incomplete_details = %v", resp["incomplete_details"])
	}
}

// TestResponsesStreamBurstMatchesLiveSequence proves buffered-mode burst
// equivalence: for the same item set the burst emits exactly the live event
// type sequence, with monotonic sequence numbers from 0 and the same final
// response payload.
func TestResponsesStreamBurstMatchesLiveSequence(t *testing.T) {
	usage := json.RawMessage(`{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}`)

	// Live: reasoning, then message content, then a tool call — one delta
	// chunk per payload so delta counts match the burst's single deltas.
	live := newResponsesStreamState("resp_fixed", "m")
	var events []responseEvent
	events = append(events, live.translate([]byte(`{"choices":[{"index":0,"delta":{"reasoning_content":"thinking"}}]}`))...)
	events = append(events, live.translate([]byte(`{"choices":[{"index":0,"delta":{"content":"hi"}}]}`))...)
	events = append(events, live.translate([]byte(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_9","type":"function","function":{"name":"f","arguments":"{}"}}]}}]}`))...)
	events = append(events, live.finish("tool_calls", usage)...)

	// Buffered: the reassembled chat body converted once, then burst.
	chat := []byte(`{"id":"chatcmpl-1","object":"chat.completion","created":1722600000,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hi","reasoning_content":"thinking","tool_calls":[{"id":"call_9","type":"function","function":{"name":"f","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`)
	burstSt := newResponsesStreamState("resp_fixed", "m")
	burstEvents := burstSt.burst(chatCompletionToResponse("resp_fixed", chat))

	liveTypes, burstTypes := typesOf(events), typesOf(burstEvents)
	if fmt.Sprint(liveTypes) != fmt.Sprint(burstTypes) {
		t.Fatalf("burst sequence differs:\nlive  %v\nburst %v", liveTypes, burstTypes)
	}
	assertSeqNumbers(t, burstEvents)

	// Both completed events carry the same final payload (modulo generated
	// item ids): status, usage mapping, and output item shapes.
	final := func(data []byte) string {
		var ev struct {
			Response struct {
				Status string `json:"status"`
				Output []struct {
					Type      string           `json:"type"`
					CallID    string           `json:"call_id"`
					Arguments string           `json:"arguments"`
					Content   []map[string]any `json:"content"`
					Summary   []map[string]any `json:"summary"`
				} `json:"output"`
				Usage map[string]any `json:"usage"`
			} `json:"response"`
		}
		if err := json.Unmarshal(data, &ev); err != nil {
			t.Fatalf("completed data: %v", err)
		}
		b, _ := json.Marshal(ev.Response)
		return string(b)
	}
	liveData, err := events[len(events)-1].data()
	if err != nil {
		t.Fatal(err)
	}
	burstData, err := burstEvents[len(burstEvents)-1].data()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := final(burstData), final(liveData); got != want {
		t.Fatalf("final response differs:\nburst %s\nlive %s", got, want)
	}
}

// ---- end-to-end ----

// responsesSSEEvent is one parsed Responses SSE event from a streamed body.
type responsesSSEEvent struct {
	Event string
	Data  map[string]any
}

// parseResponsesSSE parses an `event:`/`data:` SSE body into ordered events,
// asserting each event line matches its data payload's type.
func parseResponsesSSE(t *testing.T, body []byte) []responsesSSEEvent {
	t.Helper()
	var events []responsesSSEEvent
	typ := ""
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.HasPrefix(line, "event:"):
			typ = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			var data map[string]any
			if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &data); err != nil {
				t.Fatalf("event %q data not JSON: %v", typ, err)
			}
			if got := data["type"]; got != typ {
				t.Errorf("event line %q does not match data type %v", typ, got)
			}
			events = append(events, responsesSSEEvent{Event: typ, Data: data})
			typ = ""
		}
	}
	return events
}

// sseEventTypes extracts the ordered event names from parsed SSE events.
func sseEventTypes(events []responsesSSEEvent) []string {
	types := make([]string, len(events))
	for i, ev := range events {
		types[i] = ev.Event
	}
	return types
}

func TestResponsesStreamEndToEnd(t *testing.T) {
	provider := newFakeProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for _, line := range []string{
			`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1722600000,"model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"thinking"},"finish_reason":null}]}`,
			`data: {"choices":[{"index":0,"delta":{"content":"Hello "},"finish_reason":null}]}`,
			`data: {"choices":[{"index":0,"delta":{"content":"world"},"finish_reason":null}]}`,
			`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_9","type":"function","function":{"name":"lookup","arguments":""}}]},"finish_reason":null}]}`,
			`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"q\":\"x\"}"}}]},"finish_reason":null}]}`,
			`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":5,"completion_tokens":8,"total_tokens":13}}`,
			`data: [DONE]`,
		} {
			fmt.Fprintf(w, "%s\n\n", line)
			fl.Flush()
		}
	})
	gs, st := newGatewayTest(t, provider, twoInstanceTOML, defaultEnv)

	raw := `{"model":"gpt-4o","input":"hi","stream":true,"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}`
	resp := postResponses(t, gs, raw, map[string]string{"X-Session-Id": "sess-rstream"})
	body := drainClose(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("content-type = %q, want text/event-stream", got)
	}
	if strings.Contains(string(body), "[DONE]") {
		t.Errorf("chat [DONE] leaked to the Responses surface: %s", body)
	}

	events := parseResponsesSSE(t, body)
	want := []string{
		"response.created",
		"response.in_progress",
		"response.output_item.added", // reasoning
		"response.reasoning_summary_text.delta",
		"response.output_item.added", // message
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_text.delta",
		"response.output_item.added", // function_call
		"response.function_call_arguments.delta",
		"response.reasoning_summary_text.done",
		"response.output_item.done", // reasoning
		"response.output_text.done",
		"response.content_part.done",
		"response.output_item.done", // message
		"response.function_call_arguments.done",
		"response.output_item.done", // function_call
		"response.completed",
	}
	if got := sseEventTypes(events); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("event sequence:\n got %v\nwant %v", got, want)
	}
	// Monotonic sequence_number from 0 across the whole stream.
	for i, ev := range events {
		if got := ev.Data["sequence_number"].(float64); int(got) != i {
			t.Fatalf("event %d (%s) sequence_number = %v\ngot %v", i, ev.Event, got, sseEventTypes(events))
		}
	}

	completed := events[len(events)-1].Data["response"].(map[string]any)
	if id, ok := completed["id"].(string); !ok || !strings.HasPrefix(id, "resp_") {
		t.Errorf("completed id = %v, want resp_ prefix", completed["id"])
	}
	if completed["status"] != "completed" {
		t.Errorf("completed status = %v", completed["status"])
	}
	usage := completed["usage"].(map[string]any)
	if usage["total_tokens"] != float64(13) || usage["input_tokens"] != float64(5) || usage["output_tokens"] != float64(8) {
		t.Errorf("completed usage = %v", usage)
	}
	output := completed["output"].([]any)
	if len(output) != 3 {
		t.Fatalf("completed output = %v", output)
	}
	if output[0].(map[string]any)["type"] != "reasoning" {
		t.Errorf("output[0] = %v", output[0])
	}
	fc := output[2].(map[string]any)
	if fc["call_id"] != "call_9" || fc["arguments"] != `{"q":"x"}` {
		t.Errorf("function_call item = %v", fc)
	}

	// Capture invariants hold on the streaming path too: endpoint,
	// as-received request bytes, chat-shaped response_json.
	req := waitForRequest(t, st, "sess-rstream", 5*time.Second)
	if req.Endpoint != "/v1/responses" {
		t.Errorf("endpoint = %q, want /v1/responses", req.Endpoint)
	}
	if string(req.RequestJSON) != raw {
		t.Errorf("request_json not the as-received Responses bytes: %q", req.RequestJSON)
	}
	if !strings.Contains(string(req.ResponseJSON), `"chat.completion"`) {
		t.Errorf("response_json should stay chat-shaped: %q", req.ResponseJSON)
	}
	if req.FinishReason != "tool_calls" || req.Error != nil {
		t.Errorf("finish/error = %q/%+v, want tool_calls/nil", req.FinishReason, req.Error)
	}
}

func TestResponsesStreamBufferedPluginBurst(t *testing.T) {
	provider := newFakeProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for _, line := range []string{
			`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"role":"assistant","content":"Hello"},"finish_reason":null}]}`,
			`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`,
			`data: [DONE]`,
		} {
			fmt.Fprintf(w, "%s\n\n", line)
			fl.Flush()
		}
	})
	// A response plugin (redact) switches the surface to buffered mode.
	toml := `
[settings]
default_alias = "openai"

[[instances]]
alias = "openai"
template = "openai"
api_key_env = "TEST_KEY_1"
plugins = ["redact"]
`
	gs, st := newGatewayTest(t, provider, toml, defaultEnv)

	resp := postResponses(t, gs, `{"model":"gpt-4o","input":"hi","stream":true}`, map[string]string{"X-Session-Id": "sess-rbuf"})
	body := drainClose(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", resp.StatusCode, body)
	}

	// The whole translated sequence arrives as one burst, in the live order.
	events := parseResponsesSSE(t, body)
	want := []string{
		"response.created",
		"response.in_progress",
		"response.output_item.added",
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_text.done",
		"response.content_part.done",
		"response.output_item.done",
		"response.completed",
	}
	if got := sseEventTypes(events); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("burst sequence:\n got %v\nwant %v", got, want)
	}
	completed := events[len(events)-1].Data["response"].(map[string]any)
	if completed["status"] != "completed" {
		t.Errorf("completed status = %v", completed["status"])
	}
	if usage, ok := completed["usage"].(map[string]any); !ok || usage["total_tokens"] != float64(3) {
		t.Errorf("completed usage = %v", completed["usage"])
	}

	// Capture: response_filtered_json stays the CHAT-shaped filtered body.
	req := waitForRequest(t, st, "sess-rbuf", 5*time.Second)
	if req.Endpoint != "/v1/responses" {
		t.Errorf("endpoint = %q, want /v1/responses", req.Endpoint)
	}
	if !strings.Contains(string(req.ResponseFilteredJSON), `"chat.completion"`) {
		t.Errorf("response_filtered_json should stay chat-shaped: %q", req.ResponseFilteredJSON)
	}
	if !strings.Contains(string(req.ResponseJSON), `"chat.completion"`) {
		t.Errorf("response_json should stay chat-shaped: %q", req.ResponseJSON)
	}
}

func TestResponsesStreamUpstreamErrorPassthrough(t *testing.T) {
	provider := newFakeProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte(`{"error":{"message":"upstream exploded","type":"server_error","code":null}}`))
	})
	gs, st := newGatewayTest(t, provider, twoInstanceTOML, defaultEnv)

	raw := `{"model":"gpt-4o","input":"hi","stream":true}`
	resp := postResponses(t, gs, raw, map[string]string{"X-Session-Id": "sess-rerr"})
	body := drainClose(t, resp)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (%s)", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "upstream exploded") {
		t.Errorf("error body not passed through: %s", body)
	}

	req := waitForRequest(t, st, "sess-rerr", 5*time.Second)
	if req.Endpoint != "/v1/responses" {
		t.Errorf("endpoint = %q, want /v1/responses", req.Endpoint)
	}
	if string(req.RequestJSON) != raw {
		t.Errorf("request_json = %q, want the as-received bytes", req.RequestJSON)
	}
	if req.StatusCode != http.StatusBadGateway || req.Error == nil {
		t.Errorf("status/error = %d/%+v, want 502/non-nil", req.StatusCode, req.Error)
	}
}

func TestResponsesStreamDisconnectTruncated(t *testing.T) {
	provider := newFakeProvider(t, func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		i := 0
		for {
			select {
			case <-r.Context().Done():
				return
			default:
			}
			fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"chunk-%d \"},\"finish_reason\":null}]}\n\n", i)
			fl.Flush()
			i++
			time.Sleep(10 * time.Millisecond)
		}
	})
	gs, st := newGatewayTest(t, provider, twoInstanceTOML, defaultEnv)

	resp := postResponses(t, gs, `{"model":"gpt-4o","input":"hi","stream":true}`, map[string]string{"X-Session-Id": "sess-rtrunc"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	br := bufio.NewReader(resp.Body)
	readUntilContent(t, br, 3*time.Second, "output_text.delta")
	resp.Body.Close() // abort mid-stream, before any finalization

	req := waitForRequest(t, st, "sess-rtrunc", 5*time.Second)
	if !req.Truncated {
		t.Error("truncated = false, want true")
	}
	if len(req.ResponseJSON) == 0 {
		t.Fatal("no partial response captured")
	}
	if !strings.Contains(string(req.ResponseJSON), `"chat.completion"`) {
		t.Errorf("partial response_json should stay chat-shaped: %q", req.ResponseJSON)
	}
}
