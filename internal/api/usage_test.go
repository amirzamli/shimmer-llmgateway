package api

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/amirzamli/shimmer-llmgateway/internal/store"
)

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestUsageEndpoint(t *testing.T) {
	gs, _, st, _ := newAPITest(t, apiTestTOML, testMasterKey)
	ctx := context.Background()

	rec := &store.CaptureRecord{
		ID:               "u1",
		SessionID:        "sess-usage",
		CreatedAt:        time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC),
		Alias:            "openai",
		Provider:         "openai",
		Model:            "gpt-4o-mini",
		Endpoint:         "/v1/chat/completions",
		DurationMS:       5,
		StatusCode:       200,
		FinishReason:     "stop",
		PromptTokens:     1000,
		CompletionTokens: 200,
		CachedTokens:     100,
		CostInput:        0.00135,
		CostOutput:       0.00012,
		CostCacheRead:    0.0000075,
		CostTotal:        0.0014775,
		CostPriced:       true,
	}
	if err := st.Capture(ctx, rec); err != nil {
		t.Fatalf("Capture: %v", err)
	}

	status, out := doJSON(t, gs, "GET", "/api/usage?granularity=day", "")
	if status != 200 {
		t.Fatalf("status = %d, body %v", status, out)
	}
	totals, ok := out["totals"].(map[string]any)
	if !ok {
		t.Fatalf("missing totals: %v", out)
	}
	if totals["request_count"].(float64) != 1 || !approx(totals["cost_total"].(float64), 0.0014775) {
		t.Errorf("totals = %v", totals)
	}
	buckets := out["buckets"].([]any)
	if len(buckets) != 1 {
		t.Fatalf("buckets = %v", buckets)
	}
	b := buckets[0].(map[string]any)
	if b["period"] != "2026-08-01" || b["cost_input"].(float64) != 0.00135 {
		t.Errorf("bucket = %v", b)
	}

	// Provider filter narrows to nothing when it does not match.
	status, out = doJSON(t, gs, "GET", "/api/usage?granularity=day&provider=deepseek", "")
	if status != 200 {
		t.Fatalf("status = %d, body %v", status, out)
	}
	if len(out["buckets"].([]any)) != 0 {
		t.Errorf("filtered buckets = %v, want none", out["buckets"])
	}

	// Group by model: the bucket carries the resolved upstream model.
	status, out = doJSON(t, gs, "GET", "/api/usage?granularity=day&group=model", "")
	if status != 200 {
		t.Fatalf("status = %d, body %v", status, out)
	}
	buckets = out["buckets"].([]any)
	if len(buckets) != 1 {
		t.Fatalf("grouped buckets = %v", buckets)
	}
	b = buckets[0].(map[string]any)
	if b["group"] != "gpt-4o-mini" || b["provider"] != "openai" || b["period"] != "2026-08-01" {
		t.Errorf("grouped bucket = %v", b)
	}
	if out["group"] != "model" {
		t.Errorf("response group = %v, want model", out["group"])
	}

	// Invalid granularity is a 400.
	status, out = doJSON(t, gs, "GET", "/api/usage?granularity=hour", "")
	if status != 400 {
		t.Fatalf("invalid granularity status = %d, body %v", status, out)
	}

	// Invalid group is a 400.
	status, out = doJSON(t, gs, "GET", "/api/usage?group=alias", "")
	if status != 400 {
		t.Fatalf("invalid group status = %d, body %v", status, out)
	}
}
