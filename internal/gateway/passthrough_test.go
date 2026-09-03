package gateway

import (
	"bufio"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// ---- empty-completion passthrough (no retry: what the client sends is what
// the provider answers; the gateway never re-issues a 2xx) ----

func TestNonStreamEmptyPassthrough(t *testing.T) {
	// An empty result passes through and is captured with a single provider
	// request, exactly as received.
	provider := newFakeProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","choices":[]}`))
	})
	cfg := `
[[instances]]
alias = "openai"
template = "openai"
api_key_env = "TEST_KEY_1"
`
	gs, st := newGatewayTest(t, provider, cfg, defaultEnv)

	resp := postChat(t, gs, `{"model":"gpt-4o"}`, map[string]string{"X-Session-Id": "sess-empty-ns"})
	drainClose(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if len(provider.requests()) != 1 {
		t.Errorf("provider saw %d requests, want 1 (no retry)", len(provider.requests()))
	}
	req := waitForRequest(t, st, "sess-empty-ns", 5*time.Second)
	if len(req.ResponseJSON) == 0 {
		t.Error("empty result was not captured")
	}
}

func TestStreamKeepsLiveForwardByteForByte(t *testing.T) {
	// The stream path forwards the provider's SSE bytes live, line-by-line —
	// keepalives, reasoning deltas, content, tool_calls — and never re-issues
	// or re-shapes them into a synthetic final block. This is the wire
	// contract clients parse; the non-regression guard for it.
	var provider *fakeProvider
	proceed := make(chan struct{})
	provider = newFakeProvider(t, func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		write := func(s string) {
			w.Write([]byte(s))
			fl.Flush()
			provider.addWrite(s)
		}
		// Reasoning-only delta is forwarded verbatim (nothing is held or
		// reassembled away on this path).
		write(": keepalive\n\n")
		write("data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"reasoning_content\":\"thinking...\"},\"finish_reason\":null}]}\n\n")
		<-proceed
		write("data: [DONE]\n\n")
	})
	cfg := `
[[instances]]
alias = "openai"
template = "openai"
api_key_env = "TEST_KEY_1"
`
	gs, _ := newGatewayTest(t, provider, cfg, defaultEnv)

	resp := postChat(t, gs, `{"model":"gpt-4o","stream":true}`, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	// Live proof: the first reasoning delta arrives while the provider is
	// still blocked on proceed — line-by-line forward, never buffered.
	br := bufio.NewReader(resp.Body)
	before := readUntilContent(t, br, 3*time.Second, "thinking...")
	if !strings.Contains(before, "keepalive") {
		t.Errorf("expected the keepalive before the first delta, got %q", before)
	}
	close(proceed)
	rest, err := io.ReadAll(br)
	if err != nil {
		t.Fatalf("read rest of stream: %v", err)
	}
	got := before + string(rest)
	if want := provider.totalWrites(); got != want {
		t.Errorf("client bytes != provider bytes\nclient: %q\nprovider: %q", got, want)
	}
	if len(provider.requests()) != 1 {
		t.Errorf("provider saw %d requests, want 1 (no retry)", len(provider.requests()))
	}
}
