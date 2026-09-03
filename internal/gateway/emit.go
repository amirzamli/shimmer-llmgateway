package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/amirzamli/shimmer-llmgateway/internal/store"
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

// streamOutcome is the result of one SSE read: the reassembled completion plus
// the metadata a capture record needs.
type streamOutcome struct {
	reassembled json.RawMessage
	finish      string
	usage       json.RawMessage
	streamErr   *store.ErrorInfo
	chunks      int
	truncated   bool
	cleanEnd    bool
	statusCode  int
}

// readStream reads one SSE response to completion (or context end), forwarding
// each raw line to forward while a concurrent assembler goroutine folds data
// payloads into asm. cleanEnd marks a fully delivered stream: the terminating
// [DONE] event was seen or the upstream reached io.EOF. Truncation is decided
// on loop exit, NOT in the disconnect branches, so a client that closes right
// after receiving [DONE] does not produce a false truncated:true outcome (the
// request context can be canceled before the loop observes EOF). Context
// cancellation (client disconnect) aborts the upstream read via the transport.
func readStream(ctx context.Context, resp *http.Response, asm *completionAssembler, forward func([]byte) error) streamOutcome {
	lines := make(chan []byte, 64)
	asmDone := make(chan struct{})
	go func() {
		defer close(asmDone)
		for payload := range lines {
			asm.add(payload)
		}
	}()

	cleanEnd := false
	reader := bufio.NewReader(resp.Body)
loop:
	for {
		select {
		case <-ctx.Done():
			// Client disconnected; the transport aborts the upstream read.
			// The clean-end flag still decides truncation.
		default:
		}
		line, err := reader.ReadString('\n')
		if line != "" {
			if werr := forward([]byte(line)); werr != nil {
				// Client is gone mid-write. Whether the stream was truncated
				// is decided by cleanEnd on exit.
				break loop
			}
			if payload := ssePayload(line); payload != nil {
				lines <- payload
				if string(payload) == "[DONE]" {
					cleanEnd = true
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				cleanEnd = true
			}
			break loop
		}
	}
	close(lines)
	<-asmDone

	reassembled, finish, usage := asm.result()
	return streamOutcome{
		reassembled: reassembled,
		finish:      finish,
		usage:       usage,
		streamErr:   asm.streamError(),
		chunks:      asm.chunks(),
		truncated:   !cleanEnd,
		cleanEnd:    cleanEnd,
		statusCode:  resp.StatusCode,
	}
}
