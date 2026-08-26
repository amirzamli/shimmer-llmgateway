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
// provider/model key when UsageFilter.Group was set, else "".
type UsageBucket struct {
	Period           string  `json:"period"`
	Group            string  `json:"group,omitempty"`
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

	// grp is NULL when ungrouped so one Scan serves both forms.
	grp := `NULL AS grp`
	groupBy := `GROUP BY period`
	orderBy := `ORDER BY period ASC`
	if grouped {
		grp = f.Group + ` AS grp`
		groupBy = `GROUP BY period, grp`
		orderBy = `ORDER BY period ASC, grp ASC`
	}

	q := fmt.Sprintf(`SELECT %[1]s AS period, %[3]s,
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
		%[4]s
		%[5]s`, period, where, grp, groupBy, orderBy)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*UsageBucket
	for rows.Next() {
		var b UsageBucket
		var grpVal sql.NullString
		var unpriced int
		if err := rows.Scan(&b.Period, &grpVal, &b.RequestCount,
			&b.PromptTokens, &b.CompletionTokens, &b.CachedTokens,
			&b.CostInput, &b.CostOutput, &b.CostCacheRead, &b.CostCacheWrite, &b.CostTotal,
			&unpriced); err != nil {
			return nil, err
		}
		b.Group = grpVal.String
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
	Priced           bool
}

// BackfillCosts computes and persists token counts and estimated costs for
// requests captured before the cost columns existed. The marker is a row that
// carries usage_json but has prompt_tokens == 0 — captures always record token
// counts from usage, so this condition converges after one pass and makes the
// backfill idempotent: already-processed rows and rows without usage are never
// touched. fn derives the values from each row's model and usage object (the
// caller owns the pricing table). Returns the number of rows updated.
func (s *Store) BackfillCosts(ctx context.Context, fn func(model string, usage json.RawMessage) BackfilledCost) (int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, model, usage_json FROM requests
		WHERE usage_json IS NOT NULL AND usage_json != '' AND prompt_tokens = 0`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	updated := 0
	for rows.Next() {
		var id, model string
		var usage []byte
		if err := rows.Scan(&id, &model, &usage); err != nil {
			return updated, err
		}
		bc := fn(model, usage)
		res, err := s.db.ExecContext(ctx, `UPDATE requests SET
			prompt_tokens = ?, completion_tokens = ?, cached_tokens = ?,
			cost_input = ?, cost_output = ?, cost_cache_read = ?, cost_cache_write = ?, cost_total = ?,
			cost_priced = ?
			WHERE id = ?`,
			bc.PromptTokens, bc.CompletionTokens, bc.CachedTokens,
			bc.Input, bc.Output, bc.CacheRead, bc.CacheWrite, bc.Total,
			boolInt(bc.Priced), id)
		if err != nil {
			return updated, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			updated++
		}
	}
	return updated, rows.Err()
}
