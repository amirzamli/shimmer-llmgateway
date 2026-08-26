package api

import (
	"net/http"

	"github.com/amirzamli/shimmer-llmgateway/internal/store"
)

// handleUsage serves the aggregated cost/usage surface:
// GET /api/usage?granularity=day|week|month&provider=&alias=&model=&group=&since=&until=
// The response carries per-period buckets (newest-last) plus summed totals, so
// the UI can render daily/weekly/monthly costs with the input/output/cache
// split and a per-provider filter. group=model|provider additionally splits
// each period's bucket by the resolved upstream model or provider template,
// for the token/cost time-series graphs.
func (a *API) handleUsage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.UsageFilter{
		Granularity: store.UsageGranularity(q.Get("granularity")),
		Provider:    q.Get("provider"),
		Alias:       q.Get("alias"),
		Model:       q.Get("model"),
		Group:       q.Get("group"),
		Since:       q.Get("since"),
		Until:       q.Get("until"),
	}
	if f.Granularity != "" {
		switch f.Granularity {
		case store.GranularityDay, store.GranularityWeek, store.GranularityMonth:
		default:
			a.writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "granularity must be day, week, or month")
			return
		}
	}
	if f.Group != "" {
		switch f.Group {
		case "provider", "model":
		default:
			a.writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "group must be provider or model")
			return
		}
	}
	buckets, err := a.store.AggregateUsage(r.Context(), f)
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "INTERNAL", "aggregate usage: "+err.Error())
		return
	}
	if buckets == nil {
		buckets = []*store.UsageBucket{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"granularity": f.Granularity,
		"group":       f.Group,
		"buckets":     buckets,
		"totals":      sumUsageBuckets(buckets),
	})
}

// usageTotals is the summed view across all buckets.
type usageTotals struct {
	RequestCount     int     `json:"request_count"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	CachedTokens     int64   `json:"cached_tokens"`
	CostInput        float64 `json:"cost_input"`
	CostOutput       float64 `json:"cost_output"`
	CostCacheRead    float64 `json:"cost_cache_read"`
	CostCacheWrite   float64 `json:"cost_cache_write"`
	CostTotal        float64 `json:"cost_total"`
	UnpricedRequests int     `json:"unpriced_requests"`
}

func sumUsageBuckets(buckets []*store.UsageBucket) usageTotals {
	var t usageTotals
	for _, b := range buckets {
		t.RequestCount += b.RequestCount
		t.PromptTokens += b.PromptTokens
		t.CompletionTokens += b.CompletionTokens
		t.CachedTokens += b.CachedTokens
		t.CostInput += b.CostInput
		t.CostOutput += b.CostOutput
		t.CostCacheRead += b.CostCacheRead
		t.CostCacheWrite += b.CostCacheWrite
		t.CostTotal += b.CostTotal
		t.UnpricedRequests += b.UnpricedRequests
	}
	return t
}
