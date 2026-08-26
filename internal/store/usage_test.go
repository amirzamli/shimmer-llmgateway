package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/amirzamli/shimmer-llmgateway/internal/pricing"
	_ "modernc.org/sqlite"
)

// costTestRecord builds one captured request with token counts and an
// estimated cost split as the gateway would persist them.
func costTestRecord(id, session string, at time.Time, alias, provider, model string, prompt, completion, cached int64, costIn, costOut, costCache float64) *CaptureRecord {
	return &CaptureRecord{
		ID:           id,
		SessionID:    session,
		CreatedAt:    at,
		Alias:        alias,
		Provider:     provider,
		Model:        model,
		Endpoint:     "/v1/chat/completions",
		DurationMS:   5,
		StatusCode:   200,
		FinishReason: "stop",
		Usage: json.RawMessage(`{"prompt_tokens":` + itoa(prompt) + `,"completion_tokens":` + itoa(completion) +
			`,"total_tokens":` + itoa(prompt+completion) + `,"prompt_tokens_details":{"cached_tokens":` + itoa(cached) + `}}`),
		PromptTokens:     prompt,
		CompletionTokens: completion,
		CachedTokens:     cached,
		CostInput:        costIn,
		CostOutput:       costOut,
		CostCacheRead:    costCache,
		CostTotal:        costIn + costOut + costCache,
		CostPriced:       true,
	}
}

func itoa(v int64) string {
	return strconv.FormatInt(v, 10)
}

// approx compares floats with a tolerance for the SUM float drift.
func approx(a, b float64) bool {
	diff := a - b
	if diff < 0 {
		diff = -diff
	}
	return diff < 1e-9
}

func TestAggregateUsageDaily(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	recs := []*CaptureRecord{
		costTestRecord("r1", "s1", time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC), "openai", "openai", "gpt-4o-mini", 1000, 200, 100, 0.000135, 0.00012, 0.0000075),
		costTestRecord("r2", "s2", time.Date(2026, 8, 1, 22, 0, 0, 0, time.UTC), "openai", "openai", "gpt-4o-mini", 500, 100, 0, 0.000075, 0.00006, 0),
		costTestRecord("r3", "s3", time.Date(2026, 8, 2, 9, 0, 0, 0, time.UTC), "deepseek", "deepseek", "deepseek-chat", 100, 10, 50, 0.0000135, 0.000011, 0.0000035),
	}
	for _, r := range recs {
		if err := st.Capture(ctx, r); err != nil {
			t.Fatalf("Capture: %v", err)
		}
	}
	buckets, err := st.AggregateUsage(ctx, UsageFilter{Granularity: GranularityDay})
	if err != nil {
		t.Fatalf("AggregateUsage: %v", err)
	}
	if len(buckets) != 2 {
		t.Fatalf("got %d buckets, want 2 (days 2026-08-01, 2026-08-02)", len(buckets))
	}
	b0 := buckets[0]
	if b0.Period != "2026-08-01" {
		t.Errorf("period 0 = %q, want 2026-08-01", b0.Period)
	}
	if b0.RequestCount != 2 || b0.PromptTokens != 1500 || b0.CompletionTokens != 300 || b0.CachedTokens != 100 {
		t.Errorf("bucket0 = %+v", b0)
	}
	if !approx(b0.CostTotal, 0.000135+0.00012+0.0000075+0.000075+0.00006) {
		t.Errorf("bucket0 total = %v", b0.CostTotal)
	}
	if b0.UnpricedRequests != 0 {
		t.Errorf("bucket0 unpriced = %d, want 0", b0.UnpricedRequests)
	}
	if b1 := buckets[1]; b1.Period != "2026-08-02" || b1.RequestCount != 1 {
		t.Errorf("bucket1 = %+v", b1)
	}
}

func TestAggregateUsageGranularityAndFilters(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	recs := []*CaptureRecord{
		costTestRecord("r1", "s1", time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC), "openai", "openai", "gpt-4o-mini", 1000, 200, 100, 1, 2, 0.5),
		costTestRecord("r2", "s2", time.Date(2026, 8, 5, 10, 0, 0, 0, time.UTC), "openai", "openai", "gpt-4o", 500, 100, 0, 2, 4, 0),
		costTestRecord("r3", "s3", time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC), "deepseek", "deepseek", "deepseek-chat", 100, 10, 50, 3, 6, 1),
	}
	for _, r := range recs {
		if err := st.Capture(ctx, r); err != nil {
			t.Fatalf("Capture: %v", err)
		}
	}

	// Monthly: August and September group into two buckets.
	months, err := st.AggregateUsage(ctx, UsageFilter{Granularity: GranularityMonth})
	if err != nil {
		t.Fatalf("AggregateUsage month: %v", err)
	}
	if len(months) != 2 || months[0].Period != "2026-08" || months[1].Period != "2026-09" {
		t.Errorf("monthly buckets = %+v", months)
	}
	if months[0].RequestCount != 2 || !approx(months[0].CostTotal, 1+2+0.5+2+4) {
		t.Errorf("monthly aug bucket = %+v", months[0])
	}

	// Weekly: Aug 3 (Mon) and Aug 5 (Wed) share a week bucket.
	weeks, err := st.AggregateUsage(ctx, UsageFilter{Granularity: GranularityWeek})
	if err != nil {
		t.Fatalf("AggregateUsage week: %v", err)
	}
	if len(weeks) != 2 {
		t.Fatalf("weekly buckets = %d, want 2", len(weeks))
	}
	if weeks[0].RequestCount != 2 {
		t.Errorf("week 0 request count = %d, want 2 (same ISO week)", weeks[0].RequestCount)
	}

	// Provider filter: only deepseek rows.
	prov, err := st.AggregateUsage(ctx, UsageFilter{Granularity: GranularityDay, Provider: "deepseek"})
	if err != nil {
		t.Fatalf("AggregateUsage provider: %v", err)
	}
	if len(prov) != 1 || prov[0].RequestCount != 1 || !approx(prov[0].CostTotal, 10) {
		t.Errorf("provider-filtered buckets = %+v", prov)
	}

	// Alias filter.
	alias, err := st.AggregateUsage(ctx, UsageFilter{Granularity: GranularityDay, Alias: "openai"})
	if err != nil {
		t.Fatalf("AggregateUsage alias: %v", err)
	}
	if len(alias) != 2 {
		t.Errorf("alias-filtered buckets = %d, want 2 days", len(alias))
	}

	// Model filter.
	model, err := st.AggregateUsage(ctx, UsageFilter{Granularity: GranularityDay, Model: "gpt-4o"})
	if err != nil {
		t.Fatalf("AggregateUsage model: %v", err)
	}
	if len(model) != 1 || !approx(model[0].CostTotal, 6) {
		t.Errorf("model-filtered buckets = %+v", model)
	}

	// Since/until window (ISO strings, inclusive).
	win, err := st.AggregateUsage(ctx, UsageFilter{Granularity: GranularityDay,
		Since: "2026-08-04T00:00:00.000Z", Until: "2026-08-05T23:59:59.999Z"})
	if err != nil {
		t.Fatalf("AggregateUsage window: %v", err)
	}
	if len(win) != 1 || win[0].RequestCount != 1 || win[0].Period != "2026-08-05" {
		t.Errorf("window buckets = %+v", win)
	}
}

func TestAggregateUsageUnpricedAndEmpty(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	priced := costTestRecord("r1", "s1", time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC), "openai", "openai", "gpt-4o-mini", 100, 20, 0, 0.1, 0.2, 0)
	priced.CostPriced = true
	unpriced := costTestRecord("r2", "s2", time.Date(2026, 8, 1, 11, 0, 0, 0, time.UTC), "openai", "openai", "mystery-model", 100, 20, 0, 0, 0, 0)
	unpriced.CostPriced = false
	for _, r := range []*CaptureRecord{priced, unpriced} {
		if err := st.Capture(ctx, r); err != nil {
			t.Fatalf("Capture: %v", err)
		}
	}
	buckets, err := st.AggregateUsage(ctx, UsageFilter{Granularity: GranularityDay})
	if err != nil {
		t.Fatalf("AggregateUsage: %v", err)
	}
	if len(buckets) != 1 {
		t.Fatalf("buckets = %d, want 1", len(buckets))
	}
	if buckets[0].UnpricedRequests != 1 {
		t.Errorf("unpriced = %d, want 1", buckets[0].UnpricedRequests)
	}
	if buckets[0].RequestCount != 2 || !approx(buckets[0].CostTotal, 0.3) {
		t.Errorf("bucket = %+v", buckets[0])
	}

	empty, err := st.AggregateUsage(ctx, UsageFilter{Granularity: GranularityDay, Provider: "nope"})
	if err != nil {
		t.Fatalf("AggregateUsage empty: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("empty buckets = %+v, want none", empty)
	}
}

func TestAggregateUsageInvalidGranularity(t *testing.T) {
	st := openTestStore(t)
	if _, err := st.AggregateUsage(context.Background(), UsageFilter{Granularity: "hour"}); err == nil {
		t.Error("invalid granularity should error")
	}
	if _, err := st.AggregateUsage(context.Background(), UsageFilter{Group: "alias"}); err == nil {
		t.Error("invalid group should error")
	}
}

func TestAggregateUsageGroupBy(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	// Two actual models on one day and one on another; the alias differs from
	// the resolved model on purpose (facade alias "small" → gpt-4o-mini).
	recs := []*CaptureRecord{
		costTestRecord("r1", "s1", time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC), "openai", "openai", "gpt-4o-mini", 1000, 200, 100, 0.1, 0.2, 0.05),
		costTestRecord("r2", "s2", time.Date(2026, 8, 1, 11, 0, 0, 0, time.UTC), "openai", "openai", "gpt-4o", 500, 100, 0, 0.2, 0.4, 0),
		costTestRecord("r3", "s3", time.Date(2026, 8, 2, 9, 0, 0, 0, time.UTC), "deepseek", "deepseek", "deepseek-chat", 300, 60, 150, 0.3, 0.6, 0.15),
	}
	for _, r := range recs {
		if err := st.Capture(ctx, r); err != nil {
			t.Fatalf("Capture: %v", err)
		}
	}

	// Group by model: buckets keyed by the resolved upstream model.
	byModel, err := st.AggregateUsage(ctx, UsageFilter{Granularity: GranularityDay, Group: "model"})
	if err != nil {
		t.Fatalf("AggregateUsage by model: %v", err)
	}
	// 3 buckets: gpt-4o-mini / gpt-4o on Aug 1, deepseek-chat on Aug 2.
	if len(byModel) != 3 {
		t.Fatalf("by-model buckets = %d, want 3: %+v", len(byModel), byModel)
	}
	first := byModel[0]
	if first.Period != "2026-08-01" || first.Group != "gpt-4o" {
		t.Errorf("first by-model bucket = %+v", first)
	}
	if first.PromptTokens != 500 || !approx(first.CostTotal, 0.6) {
		t.Errorf("first by-model bucket tokens/cost = %+v", first)
	}
	mini := byModel[1]
	if mini.Group != "gpt-4o-mini" || mini.PromptTokens != 1000 || mini.CachedTokens != 100 {
		t.Errorf("gpt-4o-mini bucket = %+v", mini)
	}
	if byModel[2].Group != "deepseek-chat" || byModel[2].Period != "2026-08-02" {
		t.Errorf("deepseek bucket = %+v", byModel[2])
	}

	// Group by provider: two providers, one day each.
	byProvider, err := st.AggregateUsage(ctx, UsageFilter{Granularity: GranularityDay, Group: "provider"})
	if err != nil {
		t.Fatalf("AggregateUsage by provider: %v", err)
	}
	if len(byProvider) != 2 {
		t.Fatalf("by-provider buckets = %d, want 2: %+v", len(byProvider), byProvider)
	}
	if byProvider[0].Group != "openai" || byProvider[0].RequestCount != 2 || byProvider[0].PromptTokens != 1500 {
		t.Errorf("provider openai bucket = %+v", byProvider[0])
	}
	if byProvider[1].Group != "deepseek" || byProvider[1].PromptTokens != 300 {
		t.Errorf("provider deepseek bucket = %+v", byProvider[1])
	}

	// Grouping combines with the provider filter.
	filtered, err := st.AggregateUsage(ctx, UsageFilter{Granularity: GranularityDay, Group: "model", Provider: "openai"})
	if err != nil {
		t.Fatalf("AggregateUsage filtered by model: %v", err)
	}
	if len(filtered) != 2 {
		t.Errorf("filtered by-model buckets = %d, want 2: %+v", len(filtered), filtered)
	}

	// Ungrouped buckets carry no group key.
	plain, err := st.AggregateUsage(ctx, UsageFilter{Granularity: GranularityDay})
	if err != nil {
		t.Fatalf("AggregateUsage plain: %v", err)
	}
	for _, b := range plain {
		if b.Group != "" {
			t.Errorf("plain bucket has group %q", b.Group)
		}
	}
}

// oldSchemaRequests is the pre-cost §5 requests table, used to prove migrate
// brings an existing database up to the current column set.
const oldSchemaRequests = `CREATE TABLE IF NOT EXISTS requests (
  id TEXT PRIMARY KEY,
  session_id TEXT NOT NULL REFERENCES sessions(id),
  seq INTEGER,
  created_at TEXT,
  alias TEXT, provider TEXT, model TEXT, endpoint TEXT,
  duration_ms INTEGER,
  status_code INTEGER,
  finish_reason TEXT,
  usage_json TEXT,
  request_json TEXT,
  request_filtered_json TEXT,
  response_json TEXT,
  response_filtered_json TEXT,
  plugins_applied TEXT,
  error_json TEXT,
  truncated INTEGER DEFAULT 0
)`

func TestMigrateAddsCostColumns(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "old.db")

	// Build a database with the original schema (no cost columns) and one row.
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS sessions (
		id TEXT PRIMARY KEY,
		created_at TEXT,
		first_alias TEXT, first_model TEXT,
		request_count INTEGER, tool_call_count INTEGER, failure_count INTEGER)`); err != nil {
		t.Fatalf("create sessions: %v", err)
	}
	if _, err := db.Exec(oldSchemaRequests); err != nil {
		t.Fatalf("create old schema: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO sessions (id, created_at, first_alias, first_model, request_count, tool_call_count, failure_count)
		VALUES ('s1', '2026-08-01T10:00:00.000Z', 'openai', 'gpt-4o-mini', 1, 0, 0)`); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO requests (id, session_id, seq, created_at, alias, provider, model, endpoint,
		duration_ms, status_code, finish_reason, usage_json, truncated)
		VALUES ('r1', 's1', 1, '2026-08-01T10:00:00.000Z', 'openai', 'openai', 'gpt-4o-mini', '/v1/chat/completions',
		5, 200, 'stop', '{"prompt_tokens":100}', 0)`); err != nil {
		t.Fatalf("insert request: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open (migrate): %v", err)
	}
	defer st.Close()

	rows, err := st.db.Query(`PRAGMA table_info(requests)`)
	if err != nil {
		t.Fatalf("table_info: %v", err)
	}
	cols := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		cols[name] = true
	}
	rows.Close()
	for _, want := range []string{"prompt_tokens", "completion_tokens", "cached_tokens",
		"cost_input", "cost_output", "cost_cache_read", "cost_cache_write", "cost_total", "cost_priced"} {
		if !cols[want] {
			t.Errorf("column %q missing after migration", want)
		}
	}

	// The legacy row aggregates with zero cost and its token counts still read
	// through the usage JSON (usage_json preserved).
	buckets, err := st.AggregateUsage(context.Background(), UsageFilter{Granularity: GranularityDay})
	if err != nil {
		t.Fatalf("AggregateUsage after migration: %v", err)
	}
	if len(buckets) != 1 || buckets[0].RequestCount != 1 || buckets[0].CostTotal != 0 || buckets[0].UnpricedRequests != 1 {
		t.Errorf("legacy bucket = %+v", buckets)
	}
	req, err := st.GetRequest(context.Background(), "s1", 1)
	if err != nil {
		t.Fatalf("GetRequest: %v", err)
	}
	if len(req.Usage) == 0 || req.PromptTokens != 0 {
		t.Errorf("legacy request usage = %s, prompt tokens = %d", req.Usage, req.PromptTokens)
	}
}

func TestCaptureCostRoundTrip(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	rec := costTestRecord("r1", "s1", time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC),
		"openai", "openai", "gpt-4o-mini", 1000, 200, 300, 0.1, 0.2, 0.05)
	if err := st.Capture(ctx, rec); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	req, err := st.GetRequest(ctx, "s1", 1)
	if err != nil {
		t.Fatalf("GetRequest: %v", err)
	}
	if req.PromptTokens != 1000 || req.CompletionTokens != 200 || req.CachedTokens != 300 {
		t.Errorf("tokens = %d/%d/%d", req.PromptTokens, req.CompletionTokens, req.CachedTokens)
	}
	if req.CostInput != 0.1 || req.CostOutput != 0.2 || req.CostCacheRead != 0.05 {
		t.Errorf("cost = in %v out %v cache %v", req.CostInput, req.CostOutput, req.CostCacheRead)
	}
	if !req.CostPriced || !approx(req.CostTotal, 0.35) {
		t.Errorf("priced = %v total = %v", req.CostPriced, req.CostTotal)
	}
}

func TestBackfillCosts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "old.db")

	// Seed a database with the pre-cost schema and rows exactly as the gateway
	// would have captured them before the feature (usage present, no cost
	// columns at all).
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS sessions (
		id TEXT PRIMARY KEY,
		created_at TEXT,
		first_alias TEXT, first_model TEXT,
		request_count INTEGER, tool_call_count INTEGER, failure_count INTEGER)`); err != nil {
		t.Fatalf("create sessions: %v", err)
	}
	if _, err := db.Exec(oldSchemaRequests); err != nil {
		t.Fatalf("create old schema: %v", err)
	}
	insert := func(id, model, usage string) {
		t.Helper()
		if _, err := db.Exec(`INSERT INTO sessions (id, created_at, first_alias, first_model, request_count, tool_call_count, failure_count)
			VALUES (?, '2026-08-01T10:00:00.000Z', 'a', 'm', 1, 0, 0)`, id); err != nil {
			t.Fatalf("insert session: %v", err)
		}
		if _, err := db.Exec(`INSERT INTO requests (id, session_id, seq, created_at, alias, provider, model, endpoint,
			duration_ms, status_code, finish_reason, usage_json, truncated)
			VALUES (?, ?, 1, '2026-08-01T10:00:00.000Z', 'a', 'p', ?, '/v1/chat/completions', 5, 200, 'stop', ?, 0)`,
			id, id, model, usage); err != nil {
			t.Fatalf("insert request: %v", err)
		}
	}
	insert("r1", "gpt-4o-mini", `{"prompt_tokens":1000000,"completion_tokens":1000000,"prompt_tokens_details":{"cached_tokens":500000}}`)
	insert("r2", "stealth/ox-alpha", `{"prompt_tokens":100,"completion_tokens":50}`)
	insert("r3", "deepseek/deepseek-v4-flash", `{"prompt_tokens":1000,"completion_tokens":200,"prompt_cache_hit_tokens":400}`)
	insert("r4", "llama3.1", `{"prompt_tokens":100,"completion_tokens":50}`)
	insert("r5", "some-model", "") // no usage → must be left alone
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	st, err := Open(path) // migration adds the cost columns
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	// The backfill callback mirrors the gateway's pricing logic: slash-prefixed
	// ids resolve via the pricing table, local/unknown models stay unpriced.
	tab, err := pricing.Load()
	if err != nil {
		t.Fatalf("pricing.Load: %v", err)
	}
	n, err := st.BackfillCosts(context.Background(), func(model string, usage json.RawMessage) BackfilledCost {
		var u struct {
			PromptTokens        int64 `json:"prompt_tokens"`
			CompletionTokens    int64 `json:"completion_tokens"`
			PromptTokensDetails struct {
				CachedTokens int64 `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
			PromptCacheHitTokens int64 `json:"prompt_cache_hit_tokens"`
		}
		_ = json.Unmarshal(usage, &u)
		cached := u.PromptTokensDetails.CachedTokens
		if cached == 0 {
			cached = u.PromptCacheHitTokens
		}
		bc := BackfilledCost{PromptTokens: u.PromptTokens, CompletionTokens: u.CompletionTokens, CachedTokens: cached}
		p, ok := tab.Lookup(model)
		if !ok || p.Local {
			return bc
		}
		s := p.Estimate(u.PromptTokens, u.CompletionTokens, cached)
		bc.Input, bc.Output, bc.CacheRead, bc.CacheWrite, bc.Total = s.Input, s.Output, s.CacheRead, s.CacheWrite, s.Total()
		bc.Priced = true
		return bc
	})
	if err != nil {
		t.Fatalf("BackfillCosts: %v", err)
	}
	if n != 4 {
		t.Errorf("backfilled = %d, want 4 (r5 has no usage)", n)
	}

	// r1: gpt-4o-mini priced with cache split.
	req, err := st.GetRequest(ctx, "r1", 1)
	if err != nil {
		t.Fatalf("GetRequest r1: %v", err)
	}
	if !req.CostPriced || !approx(req.CostInput, 0.075) || !approx(req.CostCacheRead, 0.0375) || !approx(req.CostOutput, 0.6) {
		t.Errorf("r1 backfilled cost = %+v", req)
	}

	// r3: slash-prefixed id still prices via the fallback.
	req, err = st.GetRequest(ctx, "r3", 1)
	if err != nil {
		t.Fatalf("GetRequest r3: %v", err)
	}
	if !req.CostPriced || !approx(req.CostTotal, 0.27*600/1e6+0.07*400/1e6+1.1*200/1e6) {
		t.Errorf("r3 backfilled cost = %+v", req)
	}

	// r2 (unknown) and r4 (local) keep token counts but stay unpriced.
	for _, id := range []string{"r2", "r4"} {
		req, err = st.GetRequest(ctx, id, 1)
		if err != nil {
			t.Fatalf("GetRequest %s: %v", id, err)
		}
		if req.CostPriced || req.CostTotal != 0 || req.PromptTokens == 0 {
			t.Errorf("%s = priced %v total %v prompt %d", id, req.CostPriced, req.CostTotal, req.PromptTokens)
		}
	}

	// r5: no usage → untouched (still marker-eligible but harmless).
	req, err = st.GetRequest(ctx, "r5", 1)
	if err != nil {
		t.Fatalf("GetRequest r5: %v", err)
	}
	if req.PromptTokens != 0 {
		t.Errorf("r5 prompt tokens = %d, want 0 (no usage)", req.PromptTokens)
	}

	// Idempotence: a second pass updates nothing.
	n2, err := st.BackfillCosts(context.Background(), func(string, json.RawMessage) BackfilledCost {
		return BackfilledCost{Priced: true}
	})
	if err != nil {
		t.Fatalf("second BackfillCosts: %v", err)
	}
	if n2 != 0 {
		t.Errorf("second backfill updated %d rows, want 0", n2)
	}

	// Aggregation now shows priced totals.
	buckets, err := st.AggregateUsage(context.Background(), UsageFilter{Granularity: GranularityDay})
	if err != nil {
		t.Fatalf("AggregateUsage: %v", err)
	}
	if len(buckets) != 1 || buckets[0].RequestCount != 5 || buckets[0].UnpricedRequests != 3 {
		t.Errorf("post-backfill bucket = %+v", buckets)
	}
	if buckets[0].CostTotal <= 0 || buckets[0].CachedTokens != 500400 {
		t.Errorf("post-backfill totals = %+v", buckets[0])
	}
}
