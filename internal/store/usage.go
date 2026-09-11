package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// UsageGranularity selects the bucket size for AggregateUsage.
type UsageGranularity string

const (
	GranularityDay   UsageGranularity = "day"
	GranularityWeek  UsageGranularity = "week"
	GranularityMonth UsageGranularity = "month"
)

// UsageFilter narrows AggregateUsage. Zero fields are not applied.
type UsageFilter struct {
	Granularity UsageGranularity // "" defaults to day
	Provider    string
	Alias       string
	Model       string
	// Group breaks the buckets down by the requests.provider or requests.model
	// column (the resolved upstream model, never the client-facing alias). ""
	// returns plain per-period buckets.
	Group string
	Since string // ISO-8601 UTC (formatTS), or ""
	Until string // ISO-8601 UTC (formatTS), or ""
}

// UsageBucket is one aggregation period (day/week/month) of estimated cost and
// token usage, plus how many of its requests were unpriced. Group is the
// provider/model key when UsageFilter.Group was set, else "". Provider is set
// for model-grouped buckets so consumers can identify the upstream provider
// for a resolved model.
type UsageBucket struct {
	Period           string  `json:"period"`
	Group            string  `json:"group,omitempty"`
	Provider         string  `json:"provider,omitempty"`
	RequestCount     int     `json:"request_count"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	CachedTokens     int64   `json:"cached_tokens"`
	CostInput        float64 `json:"cost_input"`
	CostOutput       float64 `json:"cost_output"`
	CostCacheRead    float64 `json:"cost_cache_read"`
	CostCacheWrite   float64 `json:"cost_cache_write"`
	CostTotal        float64 `json:"cost_total"`
	// UnpricedRequests is the count of requests in the period whose model was
	// not in the pricing table (or was a local model), so a total is never
	// mistaken for a complete ledger.
	UnpricedRequests int `json:"unpriced_requests"`
}

// AggregateUsage sums cost and token columns across requests matching the
// filter, bucketed by period. created_at is the store's ISO-8601 UTC ms text,
// so strftime produces daily/weekly/monthly keys directly. With a Group the
// buckets are additionally split by provider or model.
func (s *Store) AggregateUsage(ctx context.Context, f UsageFilter) ([]*UsageBucket, error) {
	gran := f.Granularity
	if gran == "" {
		gran = GranularityDay
	}
	var period string
	switch gran {
	case GranularityDay:
		period = `strftime('%Y-%m-%d', created_at)`
	case GranularityWeek:
		period = `strftime('%Y-W%W', created_at)`
	case GranularityMonth:
		period = `strftime('%Y-%m', created_at)`
	default:
		return nil, fmt.Errorf("store: AggregateUsage: invalid granularity %q", f.Granularity)
	}
	grouped := false
	switch f.Group {
	case "":
	case "provider", "model":
		grouped = true
	default:
		return nil, fmt.Errorf("store: AggregateUsage: invalid group %q", f.Group)
	}

	var conds []string
	var args []any
	if f.Provider != "" {
		conds = append(conds, `provider = ?`)
		args = append(args, f.Provider)
	}
	if f.Alias != "" {
		conds = append(conds, `alias = ?`)
		args = append(args, f.Alias)
	}
	if f.Model != "" {
		conds = append(conds, `model = ?`)
		args = append(args, f.Model)
	}
	if f.Since != "" {
		conds = append(conds, `created_at >= ?`)
		args = append(args, f.Since)
	}
	if f.Until != "" {
		conds = append(conds, `created_at <= ?`)
		args = append(args, f.Until)
	}
	where := ""
	if len(conds) > 0 {
		where = ` WHERE ` + strings.Join(conds, " AND ")
	}

	// grp and providerName are NULL when not needed so one Scan serves all forms.
	grp := `NULL AS grp`
	providerName := `NULL AS provider_name`
	groupBy := `GROUP BY period`
	orderBy := `ORDER BY period ASC`
	if grouped {
		grp = f.Group + ` AS grp`
		groupBy = `GROUP BY period, grp`
		orderBy = `ORDER BY period ASC, grp ASC`
		if f.Group == "model" {
			// A model can be served by more than one configured provider. Keep
			// those rows distinct and expose the provider for the model rail.
			providerName = `provider AS provider_name`
			groupBy = `GROUP BY period, grp, provider_name`
			orderBy = `ORDER BY period ASC, grp ASC, provider_name ASC`
		}
	}

	q := fmt.Sprintf(`SELECT %[1]s AS period, %[3]s, %[4]s,
			COUNT(*),
			COALESCE(SUM(prompt_tokens), 0),
			COALESCE(SUM(completion_tokens), 0),
			COALESCE(SUM(cached_tokens), 0),
			COALESCE(SUM(cost_input), 0),
			COALESCE(SUM(cost_output), 0),
			COALESCE(SUM(cost_cache_read), 0),
			COALESCE(SUM(cost_cache_write), 0),
			COALESCE(SUM(cost_total), 0),
			SUM(CASE WHEN cost_priced = 0 THEN 1 ELSE 0 END)
		FROM requests%[2]s
		%[5]s
		%[6]s`, period, where, grp, providerName, groupBy, orderBy)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*UsageBucket
	for rows.Next() {
		var b UsageBucket
		var grpVal sql.NullString
		var providerVal sql.NullString
		var unpriced int
		if err := rows.Scan(&b.Period, &grpVal, &providerVal, &b.RequestCount,
			&b.PromptTokens, &b.CompletionTokens, &b.CachedTokens,
			&b.CostInput, &b.CostOutput, &b.CostCacheRead, &b.CostCacheWrite, &b.CostTotal,
			&unpriced); err != nil {
			return nil, err
		}
		b.Group = grpVal.String
		b.Provider = providerVal.String
		b.UnpricedRequests = unpriced
		out = append(out, &b)
	}
	return out, rows.Err()
}

// BackfilledCost is the computed token/cost result for one backfilled request.
type BackfilledCost struct {
	PromptTokens     int64
	CompletionTokens int64
	CachedTokens     int64
	Input            float64
	Output           float64
	CacheRead        float64
	CacheWrite       float64
	Total            float64
	// Priced gates the update: rows whose model has no price in the caller's
	// table (Priced false) are left untouched so they are retried — and
	// naturally picked up — after the table learns their price.
	Priced bool
	// Schema is the pricing snapshot id stamped into requests.cost_schema
	// alongside the cost split (e.g. "models.dev@2026-08-31").
	Schema string
}

// BackfillCosts prices requests captured while their model was unpriced
// (cost_priced = 0) using the caller's current pricing table. Already-priced
// rows are never touched — the cost split is computed once and frozen, so a
// table update never rewrites historical rows; only rows the table could not
// price at capture time (unknown model, since-learned price) are filled in.
// Rows lacking usage stay untouched and remain flagged unpriced. fn derives
// the values from each row's model, provider, and usage object. Returns the
// number of rows updated. Idempotent: a second pass updates nothing.
func (s *Store) BackfillCosts(ctx context.Context, fn func(model, provider string, usage json.RawMessage) BackfilledCost) (int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, model, provider, usage_json FROM requests
		WHERE usage_json IS NOT NULL AND usage_json != '' AND cost_priced = 0`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	updated := 0
	for rows.Next() {
		var id, model, provider string
		var usage []byte
		if err := rows.Scan(&id, &model, &provider, &usage); err != nil {
			return updated, err
		}
		bc := fn(model, provider, usage)
		if !bc.Priced {
			continue
		}
		res, err := s.db.ExecContext(ctx, `UPDATE requests SET
			prompt_tokens = ?, completion_tokens = ?, cached_tokens = ?,
			cost_input = ?, cost_output = ?, cost_cache_read = ?, cost_cache_write = ?, cost_total = ?,
			cost_priced = 1, cost_schema = ?
			WHERE id = ? AND cost_priced = 0`,
			bc.PromptTokens, bc.CompletionTokens, bc.CachedTokens,
			bc.Input, bc.Output, bc.CacheRead, bc.CacheWrite, bc.Total,
			bc.Schema, id)
		if err != nil {
			return updated, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			updated++
		}
	}
	return updated, rows.Err()
}
