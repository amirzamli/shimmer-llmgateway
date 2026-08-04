package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// Emitter is the pluggable SSE emit path (resolved review decision #1).
//
// Live mode (no response plugins) forwards lines exactly as received and
// flushes after each line so latency is never sacrificed (§4.4 "never buffer
// the whole stream"). Buffer mode (response plugins configured) reassembles the
// stream, runs the response plugins post-reassembly, then emits the filtered
// completion as one SSE block flushed per chunk — the §4.5 "v1 default, simple
// and safe" exception to the never-buffer rule.
type Emitter interface {
	// Write forwards one SSE line (including its trailing newline). LiveEmitter
	// writes it verbatim and flushes; BufferedEmitter holds it until Done.
	Write(line []byte) error
	// Done signals the end of the stream. LiveEmitter is a no-op; the buffered
	// emitter emits the filtered post-plugin block here.
	Done() error
}

// EmitterSource returns the reassembled completion the buffered emitter emits
// at Done(). It is safe to call after the assembler goroutine has finished.
type EmitterSource func() json.RawMessage

// EmitterFilter transforms the reassembled completion for the client (the
// response plugin chain). nil means no response plugins → live mode.
type EmitterFilter func(json.RawMessage) (json.RawMessage, error)

// newStreamEmitter is the default emitter factory (the Phase 4 swap point):
// response plugins configured (non-nil filter) → buffered mode, else the
// §4.4 live line-by-line mode.
func newStreamEmitter(w http.ResponseWriter, source EmitterSource, filter EmitterFilter) Emitter {
	if filter != nil {
		return newBufferedEmitter(w, source, filter)
	}
	return newLiveEmitter(w, source, filter)
}

// liveEmitter forwards SSE lines verbatim with a flush after each line. It is
// the §4.4 live mode.
type liveEmitter struct {
	w     io.Writer
	flush http.Flusher
}

// newLiveEmitter builds the §4.4 live emitter for a response writer. The
// caller guarantees w implements http.Flusher (checked by handleStream).
func newLiveEmitter(w http.ResponseWriter, _ EmitterSource, _ EmitterFilter) Emitter {
	return &liveEmitter{w: w, flush: w.(http.Flusher)}
}

func (e *liveEmitter) Write(line []byte) error {
	if _, err := e.w.Write(line); err != nil {
		return err
	}
	e.flush.Flush()
	return nil
}

func (e *liveEmitter) Done() error { return nil }

// bufferedEmitter buffers the SSE stream and, at Done(), emits the filtered
// reassembled completion as one data block plus a terminating [DONE], flushing
// after each chunk. emitted records exactly what the client received so the
// capture record's response_filtered_json matches byte-for-byte.
type bufferedEmitter struct {
	w      io.Writer
	flush  http.Flusher
	source EmitterSource
	filter EmitterFilter

	emitted json.RawMessage
}

func newBufferedEmitter(w http.ResponseWriter, source EmitterSource, filter EmitterFilter) Emitter {
	return &bufferedEmitter{w: w, flush: w.(http.Flusher), source: source, filter: filter}
}

func (e *bufferedEmitter) Write(line []byte) error {
	// Nothing is forwarded until the stream completes and the response plugins
	// have run post-reassembly (the documented buffer-mode latency tradeoff).
	return nil
}

func (e *bufferedEmitter) Done() error {
	body := e.source()
	if e.filter != nil {
		filtered, err := e.filter(body)
		if err == nil {
			body = filtered
		}
		// On a plugin error the unfiltered reassembly is still delivered (the
		// gateway never drops the provider's data); the caller logs the failure.
	}
	e.emitted = body
	if _, err := fmt.Fprintf(e.w, "data: %s\n\n", body); err != nil {
		return err
	}
	e.flush.Flush()
	if _, err := io.WriteString(e.w, "data: [DONE]\n\n"); err != nil {
		return err
	}
	e.flush.Flush()
	return nil
}

// emittedBody returns the exact bytes written to the client at Done().
func (e *bufferedEmitter) emittedBody() json.RawMessage { return e.emitted }
