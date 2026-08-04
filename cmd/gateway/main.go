// Command gateway is the capture-only LLM gateway: it loads gateway.toml,
// opens the SQLite capture store, and serves the §4 HTTP surface on the
// configured listen address.
package main

import (
	"context"
	"encoding/base64"
	"flag"
	"net/http"
	"os"

	"shimmer-llmgateway/internal/config"
	"shimmer-llmgateway/internal/gateway"
	"shimmer-llmgateway/internal/logging"
	"shimmer-llmgateway/internal/secrets"
	"shimmer-llmgateway/internal/store"
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

	mgr := config.New(cfg)

	st, err := store.Open(cfg.Store)
	if err != nil {
		logger.Error("store_open_failed", map[string]any{"store": cfg.Store, "error": err.Error()})
		os.Exit(1)
	}
	defer st.Close()
	st.SetRetention(cfg.RetentionDays)

	// Startup retention purge; the daily loop handles subsequent purges.
	if n, err := st.Purge(context.Background()); err != nil {
		logger.Warn("retention_purge_failed", map[string]any{"error": err.Error()})
	} else if n > 0 {
		logger.Info("retention_purged", map[string]any{"sessions": n})
	}
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	st.StartRetentionLoop(ctx, 0)

	srv, err := gateway.New(mgr, st, logger, *configPath, masterKey)
	if err != nil {
		logger.Error("gateway_new_failed", map[string]any{"error": err.Error()})
		os.Exit(1)
	}

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
