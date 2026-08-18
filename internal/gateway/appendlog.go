package gateway

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
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
	path string
	file *os.File
}

// openAppendLog opens (creating if needed) <store>.jsonl in append mode with
// mode 0600 (the §8 log contains the same request payloads as the store, so it
// must not be world-readable).
func openAppendLog(path string) (*appendLog, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	return &appendLog{path: path, file: f}, nil
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

// trim rewrites the append log keeping only lines whose ts is at or after the
// cutoff, so the log never retains data past retention_days. It is called by
// the store's retention purge with the same cutoff string the SQL deletes use.
// The rewrite is atomic (temp file + rename) and runs under l.mu, so it never
// races with concurrent appends. Lines whose ts cannot be parsed are kept —
// never drop a record we cannot date.
func (l *appendLog) trim(cutoff string) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	in, err := os.Open(l.path)
	if err != nil {
		return err
	}
	defer in.Close()

	dir := filepath.Dir(l.path)
	tmp, err := os.CreateTemp(dir, ".jsonl-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		if tmpName != "" {
			os.Remove(tmpName) //nolint:errcheck // best-effort cleanup on failure
		}
	}()
	// Match the log's own mode: the temp file must not be world-readable
	// before (or after) the rename.
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}

	br := bufio.NewReader(in)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 && keepLine(line, cutoff) {
			if _, werr := tmp.Write(line); werr != nil {
				tmp.Close()
				return werr
			}
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			tmp.Close()
			return err
		}
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, l.path); err != nil {
		return err
	}
	tmpName = ""
	// The append handle still points at the pre-rename inode; reopen it on the
	// rewritten file so subsequent appends land in the trimmed log.
	nf, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	old := l.file
	l.file = nf
	return old.Close()
}

// keepLine reports whether a §8 JSONL line survives at the cutoff. The ts
// field uses the store's fixed ISO-8601 UTC ms format, so plain string
// comparison is chronological. Lines without a parseable ts are kept.
func keepLine(line []byte, cutoff string) bool {
	var env struct {
		TS string `json:"ts"`
	}
	if err := json.Unmarshal(line, &env); err != nil {
		return true
	}
	return env.TS == "" || env.TS >= cutoff
}
