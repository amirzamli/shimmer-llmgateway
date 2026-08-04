package logging

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestLogRecordShape(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf)

	l.Info("startup", map[string]any{"listen": "127.0.0.1:8787", "count": 2})

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("unmarshal log line %q: %v", buf.String(), err)
	}
	for _, key := range []string{"timestamp", "level", "component", "event", "fields"} {
		if _, ok := rec[key]; !ok {
			t.Errorf("record missing key %q: %v", key, rec)
		}
	}
	if rec["component"] != "gateway" {
		t.Errorf("component = %v, want gateway", rec["component"])
	}
	if rec["level"] != "info" {
		t.Errorf("level = %v, want info", rec["level"])
	}
	if rec["event"] != "startup" {
		t.Errorf("event = %v, want startup", rec["event"])
	}
	if _, ok := rec["timestamp"].(string); !ok {
		t.Errorf("timestamp = %v (%T), want string", rec["timestamp"], rec["timestamp"])
	}
	fields, ok := rec["fields"].(map[string]any)
	if !ok {
		t.Fatalf("fields = %v (%T), want object", rec["fields"], rec["fields"])
	}
	if fields["listen"] != "127.0.0.1:8787" {
		t.Errorf("fields[listen] = %v, want 127.0.0.1:8787", fields["listen"])
	}
}

func TestLogFieldsAlwaysPresent(t *testing.T) {
	var buf bytes.Buffer
	New(&buf).Error("boom", nil)

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := rec["fields"]; !ok {
		t.Errorf("fields key missing for nil fields: %v", rec)
	}
}

func TestLogOneLinePerRecord(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf)
	l.Debug("a", nil)
	l.Warn("b", nil)

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2", len(lines))
	}
	for _, line := range lines {
		if strings.Count(line, "\n") != 0 {
			t.Errorf("line contains embedded newline: %q", line)
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Errorf("line is not valid JSON: %v", err)
		}
	}
}
