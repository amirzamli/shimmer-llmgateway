// Package logging implements the §4.6 JSON logging convention: one-line JSON
// records to a writer (stderr for the gateway) shaped
// {timestamp, level, component, event, fields} with component "gateway".
package logging

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"
)

// Level is the log severity.
type Level string

const (
	LevelDebug Level = "debug"
	LevelInfo  Level = "info"
	LevelWarn  Level = "warn"
	LevelError Level = "error"
)

// timestampFormat is ISO-8601 UTC with millisecond precision.
const timestampFormat = "2006-01-02T15:04:05.000Z"

// Logger writes one-line JSON records. It is safe for concurrent use.
type Logger struct {
	mu        sync.Mutex
	w         io.Writer
	component string
}

// New returns a Logger writing one-line JSON records to w. The component is
// fixed to "gateway" per §4.6; pass os.Stderr for the gateway's log stream.
func New(w io.Writer) *Logger {
	return &Logger{w: w, component: "gateway"}
}

// Debug logs a record at debug level.
func (l *Logger) Debug(event string, fields map[string]any) { l.log(LevelDebug, event, fields) }

// Info logs a record at info level.
func (l *Logger) Info(event string, fields map[string]any) { l.log(LevelInfo, event, fields) }

// Warn logs a record at warn level.
func (l *Logger) Warn(event string, fields map[string]any) { l.log(LevelWarn, event, fields) }

// Error logs a record at error level.
func (l *Logger) Error(event string, fields map[string]any) { l.log(LevelError, event, fields) }

func (l *Logger) log(level Level, event string, fields map[string]any) {
	if fields == nil {
		fields = map[string]any{}
	}
	rec := map[string]any{
		"timestamp": time.Now().UTC().Format(timestampFormat),
		"level":     string(level),
		"component": l.component,
		"event":     event,
		"fields":    fields,
	}
	b, err := json.Marshal(rec)
	if err != nil {
		b = []byte(`{"timestamp":"","level":"error","component":"gateway","event":"log_marshal_failed","fields":{}}`)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintln(l.w, string(b))
}
