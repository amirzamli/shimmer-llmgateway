package gateway

import (
	"encoding/json"
	"testing"

	"github.com/amirzamli/shimmer-llmgateway/internal/pricing"
	"github.com/amirzamli/shimmer-llmgateway/internal/store"
)

func TestUsageTokensOpenAI(t *testing.T) {
	u := json.RawMessage(`{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,
		"prompt_tokens_details":{"cached_tokens":40}}`)
	p, c, cached := usageTokens(u)
	if p != 100 || c != 20 || cached != 40 {
		t.Errorf("tokens = %d/%d cached %d, want 100/20/40", p, c, cached)
	}
}

func TestUsageTokensDeepSeek(t *testing.T) {
	u := json.RawMessage(`{"prompt_tokens":100,"completion_tokens":5,"total_tokens":105,
		"prompt_cache_hit_tokens":70,"prompt_cache_miss_tokens":30}`)
	p, c, cached := usageTokens(u)
	if p != 100 || c != 5 || cached != 70 {
		t.Errorf("tokens = %d/%d cached %d, want 100/5/70", p, c, cached)
	}
}

func TestUsageTokensEmptyAndGarbage(t *testing.T) {
	if p, c, cached := usageTokens(nil); p != 0 || c != 0 || cached != 0 {
		t.Errorf("nil usage = %d/%d/%d, want 0/0/0", p, c, cached)
	}
	if p, c, cached := usageTokens(json.RawMessage(`not json`)); p != 0 || c != 0 || cached != 0 {
		t.Errorf("garbage usage = %d/%d/%d, want 0/0/0", p, c, cached)
	}
}

func TestEstimateCostPricedModel(t *testing.T) {
	tab, err := pricing.Load()
	if err != nil {
		t.Fatalf("pricing.Load: %v", err)
	}
	s := &Server{pricing: tab}
	rec := &store.CaptureRecord{
		Model: "gpt-4o-mini",
		Usage: json.RawMessage(`{"prompt_tokens":1000000,"completion_tokens":1000000,
			"prompt_tokens_details":{"cached_tokens":500000}}`),
	}
	s.estimateCost(rec)
	if !rec.CostPriced {
		t.Error("gpt-4o-mini should be priced")
	}
	// 500k fresh input at $0.15 + 500k cache read at $0.075 + 1M output at $0.6.
	if rec.CostInput != 0.075 || rec.CostCacheRead != 0.0375 || rec.CostOutput != 0.6 {
		t.Errorf("split = in %v cache %v out %v, want 0.075/0.0375/0.6",
			rec.CostInput, rec.CostCacheRead, rec.CostOutput)
	}
	if rec.PromptTokens != 1000000 || rec.CompletionTokens != 1000000 || rec.CachedTokens != 500000 {
		t.Errorf("tokens = %d/%d/%d, want 1M/1M/500k", rec.PromptTokens, rec.CompletionTokens, rec.CachedTokens)
	}
	if rec.CostTotal != rec.CostInput+rec.CostCacheRead+rec.CostOutput {
		t.Errorf("total %v != sum of split", rec.CostTotal)
	}
}

func TestEstimateCostUnknownAndLocalModels(t *testing.T) {
	tab, err := pricing.Load()
	if err != nil {
		t.Fatalf("pricing.Load: %v", err)
	}
	s := &Server{pricing: tab}
	for _, model := range []string{"some-custom-model", "llama3.1"} {
		rec := &store.CaptureRecord{
			Model: model,
			Usage: json.RawMessage(`{"prompt_tokens":100,"completion_tokens":50}`),
		}
		s.estimateCost(rec)
		if rec.CostPriced {
			t.Errorf("%s should be unpriced", model)
		}
		if rec.CostTotal != 0 {
			t.Errorf("%s cost = %v, want 0", model, rec.CostTotal)
		}
		if rec.PromptTokens != 100 {
			t.Errorf("%s: token counts should still be recorded, got %d", model, rec.PromptTokens)
		}
	}
}

func TestEstimateCostNoPricingTable(t *testing.T) {
	s := &Server{} // pricing nil — estimation is skipped entirely
	rec := &store.CaptureRecord{
		Model: "gpt-4o-mini",
		Usage: json.RawMessage(`{"prompt_tokens":100,"completion_tokens":50}`),
	}
	s.estimateCost(rec)
	if rec.CostPriced || rec.CostTotal != 0 || rec.PromptTokens != 0 {
		t.Errorf("nil pricing table must leave the record untouched, got %+v", rec)
	}
}
