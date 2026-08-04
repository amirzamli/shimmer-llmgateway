package gateway

import (
	"os"
	"sync"

	"shimmer-llmgateway/internal/store"
)

// appendLog writes the §8 canonical JSONL events to <store>.jsonl during
// capture. Each new session opens with a session_start line, then every
// captured request appends its request / error / tool_call / tool_result lines
// — the same line stream ExportSession produces, shared by export, the UI
// export button, and the gateway's append log.
type appendLog struct {
	mu   sync.Mutex
	file *os.File
}

// openAppendLog opens (creating if needed) <store>.jsonl in append mode.
func openAppendLog(path string) (*appendLog, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return &appendLog{file: f}, nil
}

func (l *appendLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.file.Close()
}

// write appends the §8 lines for one captured request. The session_start line
// is written once, on the session's first request (seq == 1).
func (l *appendLog) write(rec *store.CaptureRecord) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if rec.Seq == 1 {
		if err := store.EncodeSessionStart(l.file, rec.SessionSummary()); err != nil {
			return err
		}
	}
	return store.EncodeRequest(l.file, rec.Request())
}
