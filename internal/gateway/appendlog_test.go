package gateway

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/amirzamli/shimmer-llmgateway/internal/store"
)

func testRec(id, session string, seq int, at time.Time) *store.CaptureRecord {
	return &store.CaptureRecord{
		ID:          id,
		SessionID:   session,
		Seq:         seq,
		CreatedAt:   at,
		Alias:       "openai",
		Provider:    "openai",
		Model:       "gpt-4o",
		Endpoint:    "/v1/chat/completions",
		DurationMS:  1,
		StatusCode:  200,
		RequestJSON: []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`),
	}
}

func TestOpenAppendLogCreatesWith0600(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.db.jsonl")
	l, err := openAppendLog(path)
	if err != nil {
		t.Fatalf("openAppendLog: %v", err)
	}
	defer l.Close()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// W1: the §8 log holds the same payloads as the store; must not be
	// world-readable.
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("append log mode = %o, want 0600", perm)
	}
}

func TestKeeplineKeepsUndateableLines(t *testing.T) {
	if keepLine([]byte(`{"type":"request","ts":"2026-01-01T00:00:00.000Z"}`), "2026-06-01T00:00:00.000Z") {
		t.Error("line with ts before the cutoff was kept")
	}
	if !keepLine([]byte(`{"type":"request","ts":"2026-06-02T00:00:00.000Z"}`), "2026-06-01T00:00:00.000Z") {
		t.Error("line with ts at/after the cutoff was dropped")
	}
	if !keepLine([]byte(`not json`), "2026-06-01T00:00:00.000Z") {
		t.Error("unparseable line was dropped (never drop undateable data)")
	}
	if !keepLine([]byte(`{"type":"request"}`), "2026-06-01T00:00:00.000Z") {
		t.Error("line without a ts field was dropped")
	}
}

func TestAppendLogTrimRetention(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.db.jsonl")
	l, err := openAppendLog(path)
	if err != nil {
		t.Fatalf("openAppendLog: %v", err)
	}
	defer l.Close()

	old := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	recent := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	if err := l.write(testRec("req-old", "sess-old", 1, old)); err != nil {
		t.Fatalf("write old: %v", err)
	}
	if err := l.write(testRec("req-new", "sess-old", 2, recent)); err != nil {
		t.Fatalf("write recent: %v", err)
	}

	// Cutoff between the two timestamps (store format: ISO-8601 UTC ms).
	if err := l.trim("2026-03-01T00:00:00.000Z"); err != nil {
		t.Fatalf("trim: %v", err)
	}

	lines := readAppendLog(t, path)
	if len(lines) != 1 {
		t.Fatalf("append log after trim = %d lines, want 1: %v", len(lines), lines)
	}
	if strings.Contains(lines[0], "2026-01-01") {
		t.Errorf("trimmed log still contains the old record: %s", lines[0])
	}

	// Appends after trim land in the rewritten file (the handle was reopened
	// over the new inode).
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	if err := l.write(testRec("req-after", "sess-old", 3, now)); err != nil {
		t.Fatalf("write after trim: %v", err)
	}
	lines = readAppendLog(t, path)
	if len(lines) != 2 {
		t.Fatalf("append log after post-trim write = %d lines, want 2: %v", len(lines), lines)
	}
	if !strings.Contains(lines[1], "req-after") {
		t.Errorf("post-trim append missing the new record: %v", lines)
	}
}
