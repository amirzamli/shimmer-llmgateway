// Command gateway is the capture-only LLM gateway: it loads gateway.toml,
// opens the SQLite capture store, and serves the §4 HTTP surface on the
// configured listen address.
package main

import (
	"context"
	"encoding/base64"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"

	"github.com/amirzamli/shimmer-llmgateway/internal/config"
	"github.com/amirzamli/shimmer-llmgateway/internal/gateway"
	"github.com/amirzamli/shimmer-llmgateway/internal/logging"
	"github.com/amirzamli/shimmer-llmgateway/internal/netutil"
	"github.com/amirzamli/shimmer-llmgateway/internal/secrets"
	"github.com/amirzamli/shimmer-llmgateway/internal/store"
)

func main() {
	configPath := flag.String("config", "gateway.toml", "path to gateway.toml")
	allowRemote := flag.Bool("allow-remote", false, "allow binding to a non-loopback listen address")
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

	if !*allowRemote {
		if err := checkLoopbackListen(cfg.Listen); err != nil {
			logger.Error("listen_rejected", map[string]any{"listen": cfg.Listen, "error": err.Error()})
			os.Exit(1)
		}
	}

	mgr := config.New(cfg)

	st, err := store.Open(cfg.Store)
	if err != nil {
		logger.Error("store_open_failed", map[string]any{"store": cfg.Store, "error": err.Error()})
		os.Exit(1)
	}
	defer st.Close()
	st.SetRetention(cfg.RetentionDays)

	ctx, stop := context.WithCancel(context.Background())
	defer stop()

	srv, err := gateway.New(mgr, st, logger, *configPath, masterKey)
	if err != nil {
		logger.Error("gateway_new_failed", map[string]any{"error": err.Error()})
		os.Exit(1)
	}

	// Startup retention purge; the daily loop handles subsequent purges. The
	// gateway is built first so its §8 append-log trimmer is registered and
	// the JSONL is purged in lockstep with the SQLite rows.
	if n, err := st.Purge(context.Background()); err != nil {
		logger.Warn("retention_purge_failed", map[string]any{"error": err.Error()})
	} else if n > 0 {
		logger.Info("retention_purged", map[string]any{"sessions": n})
	}
	st.StartRetentionLoop(ctx, 0)

	logger.Info("startup", map[string]any{
		"listen":         cfg.Listen,
		"store":          cfg.Store,
		"retention_days": cfg.RetentionDays,
		"instances":      len(cfg.Instances),
		"templates":      len(cfg.Templates),
	})

	httpServer := &http.Server{Addr: cfg.Listen, Handler: srv.Handler()}
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("listen_failed", map[string]any{"error": err.Error()})
		os.Exit(1)
	}
}

// checkLoopbackListen verifies that a cfg.Listen host:port binds to a
// loopback address only (localhost, 127.0.0.0/8, ::1). An empty host (":8787")
// is a wildcard bind over all interfaces, which would silently defeat the
// loopback-only default, so it is refused here too; -allow-remote overrides
// both cases.
func checkLoopbackListen(listen string) error {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("invalid listen address %q: %v", listen, err)
	}
	if host == "" {
		return fmt.Errorf("refusing to bind wildcard address %q; use -allow-remote to override", listen)
	}
	if !netutil.IsLoopbackHost(host) {
		return fmt.Errorf("refusing to bind non-loopback address %q; use -allow-remote to override", listen)
	}
	return nil
}
