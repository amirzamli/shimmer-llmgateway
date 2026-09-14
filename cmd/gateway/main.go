// Command gateway is the capture-only LLM gateway: it loads gateway.toml,
// opens the SQLite capture store, and serves the §4 HTTP surface on each of
// the configured listen addresses.
package main

import (
	"context"
	"encoding/base64"
	"flag"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/amirzamli/shimmer-llmgateway/internal/config"
	"github.com/amirzamli/shimmer-llmgateway/internal/gateway"
	"github.com/amirzamli/shimmer-llmgateway/internal/logging"
	"github.com/amirzamli/shimmer-llmgateway/internal/netutil"
	"github.com/amirzamli/shimmer-llmgateway/internal/secrets"
	"github.com/amirzamli/shimmer-llmgateway/internal/store"
)

func main() {
	configPath := flag.String("config", "gateway.toml", "path to gateway.toml")
	flag.Parse()

	logger := logging.New(os.Stderr)

	// SHIMMER_MASTER_KEY: optional base64-encoded 32-byte AES-256 master key
	// for the secrets file. A malformed value is fatal before any file is
	// touched; the env value itself is never logged.
	var masterKey []byte
	if env := os.Getenv("SHIMMER_MASTER_KEY"); env != "" {
		if err := secrets.ValidateMasterKey([]byte(env)); err != nil {
			logger.Error("master_key_invalid", map[string]any{"error": err.Error()})
			os.Exit(1)
		}
		// ValidateMasterKey guaranteed the value decodes to 32 bytes.
		masterKey, _ = base64.StdEncoding.DecodeString(env)
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Error("config_load_failed", map[string]any{"path": *configPath, "error": err.Error()})
		os.Exit(1)
	}

	// Each listen address is bound to its own socket (see the serving loop at
	// the bottom), so the gateway can serve localhost and a Tailscale address
	// side by side. The addresses are validated before any state is opened.
	var addrs []string
	for _, a := range cfg.Addrs() {
		host, _, splitErr := net.SplitHostPort(a)
		if splitErr != nil {
			logger.Error("listen_rejected", map[string]any{"listen": a, "error": splitErr.Error()})
			os.Exit(1)
		}
		if !netutil.IsBindableHost(host) {
			logger.Error("listen_rejected", map[string]any{
				"listen": a,
				"error":  "refusing to bind " + host + ": only loopback, CGNAT (Tailscale), and ULA addresses are allowed",
			})
			os.Exit(1)
		}
		addrs = append(addrs, a)
	}

	mgr := config.New(cfg)

	st, err := store.Open(cfg.Store)
	if err != nil {
		logger.Error("store_open_failed", map[string]any{"store": cfg.Store, "error": err.Error()})
		os.Exit(1)
	}
	defer st.Close()
	st.SetRetention(cfg.RetentionDays)

	// SIGINT/SIGTERM cancel ctx so the shutdown path at the bottom runs:
	// listeners drain, the append log closes, and the SQLite close checkpoints
	// and truncates the WAL instead of leaving it for the next startup to
	// recover.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv, err := gateway.New(mgr, st, logger, *configPath, masterKey, addrs)
	if err != nil {
		logger.Error("gateway_new_failed", map[string]any{"error": err.Error()})
		os.Exit(1)
	}

	// TRUNCATE the WAL before serving: no captures are in flight yet — this is
	// the one moment the WAL is quiescent. The size limit set at store open
	// keeps it bounded after that; this pass reclaims whatever an unclean kill
	// left behind.
	if err := st.Checkpoint(context.Background()); err != nil {
		logger.Warn("startup_checkpoint_failed", map[string]any{"error": err.Error()})
	}
	// Price requests captured while their model was unpriced (idempotent; see
	// gateway.BackfillCosts). Already-priced rows are never touched, so a
	// table update never rewrites historical costs. Runs before serving so the
	// Usage view never shows a half-priced ledger.
	gateway.BackfillCosts(context.Background(), st, logger)
	// Retention runs entirely in the background: an immediate first pass
	// expires whatever aged out while the gateway was down, then the loop
	// purges daily. Serving never waits for a purge, and shutdown performs
	// none — the store close checkpoints the WAL, purge state lives in the
	// store, and the next start's first pass catches up.
	st.StartRetentionLoop(ctx, 0, func(n int, err error) {
		if err != nil {
			logger.Warn("retention_purge_failed", map[string]any{"error": err.Error()})
		} else if n > 0 {
			logger.Info("retention_purged", map[string]any{"sessions": n})
		}
	})

	logger.Info("startup", map[string]any{
		"listen":         addrs,
		"store":          cfg.Store,
		"retention_days": cfg.RetentionDays,
		"instances":      len(cfg.Instances),
		"templates":      len(cfg.Templates),
	})

	// One http.Server per listen address, all sharing the same handler. A
	// bind failure on one address (e.g. the address is not currently assigned
	// to an interface) is fatal, matching the previous single-listener
	// behavior.
	handler := srv.Handler()
	var servers []*http.Server
	serving := 0
	for _, addr := range addrs {
		httpServer := &http.Server{Addr: addr, Handler: handler}
		httpServerCopy := httpServer
		go func() {
			logger.Info("serving", map[string]any{"listen": addr})
			if err := httpServerCopy.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				logger.Error("listen_failed", map[string]any{"listen": addr, "error": err.Error()})
				os.Exit(1)
			}
		}()
		servers = append(servers, httpServer)
		serving++
	}
	if serving == 0 {
		logger.Error("listen_failed", map[string]any{"error": "no listen addresses configured"})
		os.Exit(1)
	}

	// Shutdown on SIGINT/SIGTERM: drain in-flight requests (bounded), close
	// the append log, then close the store. The store close is what
	// checkpoints and truncates the WAL, so skipping it (os.Exit, a kill
	// signal without this path) is what made restarts slow.
	<-ctx.Done()
	logger.Info("shutdown_started", nil)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, httpServer := range servers {
		_ = httpServer.Shutdown(shutdownCtx)
	}
	if err := srv.Close(); err != nil {
		logger.Warn("append_log_close_failed", map[string]any{"error": err.Error()})
	}
	if err := st.Close(); err != nil {
		logger.Warn("store_close_failed", map[string]any{"error": err.Error()})
	}
	logger.Info("shutdown_complete", nil)
}

// serveListeners runs one http.Server per address over handler, returning the
// first error (including http.ErrServerClosed) from any of them. This is the
// sequential serving path used by tests; main runs the same servers
// concurrently so the process survives an individual listen failure and keeps
// serving the surviving addresses.
func serveListeners(addrs []string, handler http.Handler) error {
	servers := make([]*http.Server, 0, len(addrs))
	for _, addr := range addrs {
		srv := &http.Server{Addr: addr, Handler: handler}
		servers = append(servers, srv)
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return err
		}
		go srv.Serve(ln) //nolint:errcheck // the returned error is not observable; Listen succeeded
	}
	for _, srv := range servers {
		waitServed(srv)
	}
	return nil
}

// waitServed blocks until srv has served at least one connection; used by
// tests to know the listener is accepting before issuing requests.
func waitServed(srv *http.Server) <-chan struct{} {
	done := make(chan struct{})
	srv.RegisterOnShutdown(func() { close(done) })
	go func() {
		<-done
	}()
	return done
}
