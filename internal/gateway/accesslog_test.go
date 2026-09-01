package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// accessLogLine is the subset of a §4.6 log record the access-log tests read.
type accessLogLine struct {
	Event  string         `json:"event"`
	Level  string         `json:"level"`
	Fields map[string]any `json:"fields"`
}

// accessLines returns the log records with event "request" emitted into buf.
func accessLines(t *testing.T, buf *bytes.Buffer) []accessLogLine {
	t.Helper()
	var out []accessLogLine
	for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		if line == "" {
			continue
		}
		var rec accessLogLine
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("unmarshal log line %q: %v", line, err)
		}
		if rec.Event == "request" {
			out = append(out, rec)
		}
	}
	return out
}

// newAccessLogServer builds the wrapped gateway handler with console logs
// captured into buf instead of discarded.
func newAccessLogServer(t *testing.T, buf *bytes.Buffer) http.Handler {
	t.Helper()
	provider := newFakeProvider(t, nil)
	srv, _ := newGatewayServer(t, provider, twoInstanceTOML, defaultEnv, buf)
	return srv.Handler()
}

// serveAccess serves one request against the wrapped handler and returns the
// response recorder. The request is addressed to a loopback Host, like a real
// local client (httptest.NewRequest would otherwise default to example.com,
// which the admin-surface host guard rightly refuses).
func serveAccess(t *testing.T, h http.Handler, method, target string, body io.Reader, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, body)
	req.Host = "127.0.0.1:8787"
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestAccessLogGETRoot(t *testing.T) {
	var buf bytes.Buffer
	h := newAccessLogServer(t, &buf)

	rec := serveAccess(t, h, http.MethodGet, "/", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", rec.Code)
	}

	lines := accessLines(t, &buf)
	if len(lines) != 1 {
		t.Fatalf("access lines = %d, want 1\n%s", len(lines), buf.String())
	}
	f := lines[0].Fields
	if f["method"] != http.MethodGet {
		t.Errorf("access method = %v, want GET", f["method"])
	}
	if f["path"] != "/" {
		t.Errorf("access path = %v, want /", f["path"])
	}
	if f["status"] != float64(http.StatusOK) {
		t.Errorf("access status = %v, want 200", f["status"])
	}
	if _, ok := f["remote_addr"]; !ok {
		t.Errorf("access record missing remote_addr: %v", f)
	}
}

func TestAccessLogDurationMSIsNumber(t *testing.T) {
	var buf bytes.Buffer
	h := newAccessLogServer(t, &buf)

	serveAccess(t, h, http.MethodGet, "/healthz", nil, nil)

	lines := accessLines(t, &buf)
	if len(lines) != 1 {
		t.Fatalf("access lines = %d, want 1\n%s", len(lines), buf.String())
	}
	dur, ok := lines[0].Fields["duration_ms"].(float64)
	if !ok {
		t.Fatalf("access duration_ms = %v (%T), want a number", lines[0].Fields["duration_ms"], lines[0].Fields["duration_ms"])
	}
	if dur < 0 {
		t.Errorf("access duration_ms = %v, want >= 0", dur)
	}
}

func TestAccessLogStatusCaptured(t *testing.T) {
	var buf bytes.Buffer
	h := newAccessLogServer(t, &buf)

	// Unknown /v1/* path → the catch-all route writes 404.
	rec := serveAccess(t, h, http.MethodGet, "/v1/nope", nil, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /v1/nope status = %d, want 404", rec.Code)
	}

	rec = serveAccess(t, h, http.MethodGet, "/healthz", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /healthz status = %d, want 200", rec.Code)
	}

	lines := accessLines(t, &buf)
	if len(lines) != 2 {
		t.Fatalf("access lines = %d, want 2\n%s", len(lines), buf.String())
	}
	if got := lines[0].Fields["status"]; got != float64(http.StatusNotFound) {
		t.Errorf("404 access status = %v, want 404", got)
	}
	if got := lines[1].Fields["status"]; got != float64(http.StatusOK) {
		t.Errorf("200 access status = %v, want 200", got)
	}
}

func TestAccessLogIncludesSessionID(t *testing.T) {
	var buf bytes.Buffer
	provider := newFakeProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	})
	srv, _ := newGatewayServer(t, provider, twoInstanceTOML, defaultEnv, &buf)
	h := srv.Handler()

	rec := serveAccess(t, h, http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","stream":false}`),
		map[string]string{"Content-Type": "application/json", "X-Session-Id": "sess-abc"})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /v1/chat/completions status = %d, body %s", rec.Code, rec.Body.String())
	}

	lines := accessLines(t, &buf)
	if len(lines) != 1 {
		t.Fatalf("access lines = %d, want 1\n%s", len(lines), buf.String())
	}
	if got := lines[0].Fields["session_id"]; got != "sess-abc" {
		t.Errorf("access session_id = %v, want sess-abc", got)
	}
}
