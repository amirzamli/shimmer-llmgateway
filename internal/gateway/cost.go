package gateway

import (
	"context"
	"encoding/json"

	"github.com/amirzamli/shimmer-llmgateway/internal/logging"
	"github.com/amirzamli/shimmer-llmgateway/internal/pricing"
	"github.com/amirzamli/shimmer-llmgateway/internal/store"
)

// usageTokens extracts prompt/completion/cache-read token counts from an
// OpenAI-shaped usage object. Cache reads are read from either
// prompt_tokens_details.cached_tokens (OpenAI shape) or
// prompt_cache_hit_tokens (DeepSeek shape). The OpenAI usage object carries no
// cache-write count (Anthropic's cache_creation_input_tokens is dropped by the
// OpenAI translation), so cache writes are always estimated at 0.
func usageTokens(usage json.RawMessage) (prompt, completion, cached int64) {
	if len(usage) == 0 {
		return 0, 0, 0
	}
	var u struct {
		PromptTokens       int64 `json:"prompt_tokens"`
		CompletionTokens   int64 `json:"completion_tokens"`
		PromptTokensDetail struct {
			CachedTokens int64 `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		PromptCacheHitTokens int64 `json:"prompt_cache_hit_tokens"`
	}
	if err := json.Unmarshal(usage, &u); err != nil {
		return 0, 0, 0
	}
	cached = u.PromptTokensDetail.CachedTokens
	if cached == 0 {
		cached = u.PromptCacheHitTokens
	}
	return u.PromptTokens, u.CompletionTokens, cached
}

// estimateCost fills a capture record's token counts and estimated cost split
// from its usage object using the pricing table loaded at startup. The lookup is
// provider-aware: the serving provider (the gateway template name) and, when
// set, the resolved upstream base URL select the price row — a reseller's
// prices differ from the origin lab's. The table snapshot id is stamped on the
// record so a later table update never silently reinterprets the stored row. A
// model not in the table (or a local model) yields $0 with CostPriced false,
// so capture never fails on an unknown model — the aggregate surface uses the
// flag to report how much of a total is estimated, and the startup backfill
// prices such rows once the table learns them.
func (s *Server) estimateCost(rec *store.CaptureRecord) {
	if s.pricing == nil || len(rec.Usage) == 0 {
		return
	}
	prompt, completion, cached := usageTokens(rec.Usage)
	rec.PromptTokens, rec.CompletionTokens, rec.CachedTokens = prompt, completion, cached
	p, ok := s.pricing.Lookup(rec.Model, rec.Provider, rec.ProviderBaseURL)
	if !ok || p.Local {
		return
	}
	split := p.Estimate(prompt, completion, cached)
	rec.CostInput = split.Input
	rec.CostOutput = split.Output
	rec.CostCacheRead = split.CacheRead
	rec.CostCacheWrite = split.CacheWrite
	rec.CostTotal = split.Total()
	rec.CostPriced = true
	rec.CostSchema = s.pricing.Schema()
}

// BackfillCosts prices requests captured while their model was unpriced (rows
// with usage and cost_priced = 0 — e.g. captured before their price entered
// the table) using the pricing table from disk. Already-priced rows are never
// touched: the cost split is computed once and frozen, so a table update never
// rewrites historical rows (requests.cost_schema records which snapshot priced
// each row). Idempotent and safe to run on every startup; runs before the
// server accepts traffic so the UI never shows a half-priced ledger.
func BackfillCosts(ctx context.Context, st *store.Store, logger *logging.Logger) {
	pt, err := pricing.Load(pricing.DefaultPath)
	if err != nil {
		logger.Warn("cost_backfill_skipped", map[string]any{"error": err.Error()})
		return
	}
	n, err := st.BackfillCosts(ctx, func(model, provider string, usage json.RawMessage) store.BackfilledCost {
		prompt, completion, cached := usageTokens(usage)
		bc := store.BackfilledCost{PromptTokens: prompt, CompletionTokens: completion, CachedTokens: cached}
		p, ok := pt.Lookup(model, provider, "")
		if !ok || p.Local {
			return bc
		}
		split := p.Estimate(prompt, completion, cached)
		bc.Input = split.Input
		bc.Output = split.Output
		bc.CacheRead = split.CacheRead
		bc.CacheWrite = split.CacheWrite
		bc.Total = split.Total()
		bc.Priced = true
		bc.Schema = pt.Schema()
		return bc
	})
	if err != nil {
		logger.Warn("cost_backfill_failed", map[string]any{"error": err.Error()})
		return
	}
	if n > 0 {
		logger.Info("cost_backfilled", map[string]any{"requests": n, "schema": pt.Schema()})
	}
}
