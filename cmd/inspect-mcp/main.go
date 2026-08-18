// Command inspect-mcp is the §7 inspection MCP server: a minimal hand-rolled
// MCP JSON-RPC server (initialize, ping, notifications/initialized,
// tools/list, tools/call) exposing the §7.1 tools (the seven generic tools
// plus validate_tool_call / classify_failure) over the store captured by the
// gateway. By default it serves the MCP stdio transport on stdin/stdout; pass
// -http <addr> to serve streamable-http instead.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/amirzamli/shimmer-llmgateway/internal/mcp"
	"github.com/amirzamli/shimmer-llmgateway/internal/store"
)

func main() {
	dbPath := flag.String("db", "", "path to the gateway SQLite store (required)")
	session := flag.String("session", "", "optional session id; when set, generic tools are constrained to that session")
	httpAddr := flag.String("http", "", "optional listen address for the streamable-http transport (e.g. 127.0.0.1:9876); default is the stdio transport")
	flag.Parse()

	if *dbPath == "" {
		fmt.Fprintln(os.Stderr, "inspect-mcp: -db <path> is required")
		flag.Usage()
		os.Exit(2)
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "inspect-mcp: open store %s: %v\n", *dbPath, err)
		os.Exit(1)
	}
	defer st.Close()

	srv := mcp.New(st, *session)

	if *httpAddr != "" {
		httpServer := &http.Server{Addr: *httpAddr, Handler: srv.Handler()}
		errCh := make(chan error, 1)
		go func() { errCh <- httpServer.ListenAndServe() }()
		fmt.Fprintf(os.Stderr, "inspect-mcp: streamable-http listening on %s (db %s)\n", *httpAddr, *dbPath)
		select {
		case err := <-errCh:
			fmt.Fprintf(os.Stderr, "inspect-mcp: http server: %v\n", err)
			os.Exit(1)
		case sig := <-signalCh():
			fmt.Fprintf(os.Stderr, "inspect-mcp: received %s, shutting down\n", sig)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := httpServer.Shutdown(ctx); err != nil {
				fmt.Fprintf(os.Stderr, "inspect-mcp: shutdown: %v\n", err)
			}
		}
		return
	}

	// stdio transport: the MCP client owns the process; exit when it closes
	// stdin (EOF).
	if err := srv.ServeStdio(context.Background(), os.Stdin, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "inspect-mcp: stdio: %v\n", err)
		os.Exit(1)
	}
}

func signalCh() <-chan os.Signal {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	return ch
}
