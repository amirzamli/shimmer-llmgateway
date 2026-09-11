package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strconv"
	"testing"
	"time"

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
	if first.Period != "2026-08-01" || first.Group != "gpt-4o" || first.Provider != "openai" {
		t.Errorf("first by-model bucket = %+v", first)
	}
	if first.PromptTokens != 500 || !approx(first.CostTotal, 0.6) {
		t.Errorf("first by-model bucket tokens/cost = %+v", first)
	}
	mini := byModel[1]
	if mini.Group != "gpt-4o-mini" || mini.PromptTokens != 1000 || mini.CachedTokens != 100 {
		t.Errorf("gpt-4o-mini bucket = %+v", mini)
	}
	if byModel[2].Group != "deepseek-chat" || byModel[2].Provider != "deepseek" || byModel[2].Period != "2026-08-02" {
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
	st := openTestStore(t)
	ctx := context.Background()

	// unpriced-with-usage: the row the backfill must price (captured before
	// the model's price entered the table).
	unpriced := costTestRecord("r1", "s1", time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC),
		"commandcode", "commandcode", "deepseek/deepseek-v4-flash", 0, 0, 0, 0, 0, 0)
	unpriced.CostPriced = false
	unpriced.Usage = json.RawMessage(`{"prompt_tokens":1000,"completion_tokens":200,
		"prompt_tokens_details":{"cached_tokens":400}}`)
	// priced: an already-priced row that must stay frozen.
	priced := costTestRecord("r2", "s2", time.Date(2026, 8, 1, 11, 0, 0, 0, time.UTC),
		"openai", "openai", "gpt-4o-mini", 100, 20, 0, 0.1, 0.2, 0)
	priced.CostSchema = "models.dev@2026-01-01"
	// no-usage: stays untouched regardless.
	noUsage := costTestRecord("r3", "s3", time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC),
		"openai", "openai", "gpt-4o", 0, 0, 0, 0, 0, 0)
	noUsage.CostPriced = false
	for _, r := range []*CaptureRecord{unpriced, priced, noUsage} {
		if err := st.Capture(ctx, r); err != nil {
			t.Fatalf("Capture: %v", err)
		}
	}

	const schema = "models.dev@2026-08-31"
	backfill := func(model, provider string) BackfilledCost {
		if model == "deepseek/deepseek-v4-flash" {
			return BackfilledCost{PromptTokens: 1000, CompletionTokens: 200, CachedTokens: 400,
				Input: 0.0006, Output: 0.00022, CacheRead: 0.0000028, Total: 0.0008228,
				Priced: true, Schema: schema}
		}
		return BackfilledCost{}
	}
	n, err := st.BackfillCosts(ctx, func(model, provider string, usage json.RawMessage) BackfilledCost {
		return backfill(model, provider)
	})
	if err != nil {
		t.Fatalf("BackfillCosts: %v", err)
	}
	if n != 1 {
		t.Fatalf("backfilled = %d, want 1 (r1 only)", n)
	}

	// r1: priced with the schema stamp.
	req, err := st.GetRequest(ctx, "s1", 1)
	if err != nil {
		t.Fatalf("GetRequest r1: %v", err)
	}
	if !req.CostPriced || !approx(req.CostTotal, 0.0008228) || req.CostSchema != schema {
		t.Errorf("r1 = priced %v total %v schema %q", req.CostPriced, req.CostTotal, req.CostSchema)
	}
	if req.PromptTokens != 1000 || req.CachedTokens != 400 {
		t.Errorf("r1 tokens = %d/%d, want 1000/400", req.PromptTokens, req.CachedTokens)
	}

	// r2: already-priced rows keep their frozen split and original stamp.
	req, err = st.GetRequest(ctx, "s2", 1)
	if err != nil {
		t.Fatalf("GetRequest r2: %v", err)
	}
	if !approx(req.CostTotal, 0.3) || req.CostSchema != "models.dev@2026-01-01" {
		t.Errorf("r2 rewritten: total %v schema %q", req.CostTotal, req.CostSchema)
	}

	// r3: no usage — untouched, still unpriced.
	req, err = st.GetRequest(ctx, "s3", 1)
	if err != nil {
		t.Fatalf("GetRequest r3: %v", err)
	}
	if req.CostPriced || req.CostSchema != "" {
		t.Errorf("r3 = priced %v schema %q, want untouched", req.CostPriced, req.CostSchema)
	}

	// Idempotence: a second pass updates nothing (r1 is priced now, r3's model
	// still prices as unpriced).
	n2, err := st.BackfillCosts(ctx, func(model, provider string, usage json.RawMessage) BackfilledCost {
		return backfill(model, provider)
	})
	if err != nil {
		t.Fatalf("second BackfillCosts: %v", err)
	}
	if n2 != 0 {
		t.Errorf("second pass updated %d rows, want 0", n2)
	}
}
