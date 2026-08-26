// Package pricing provides per-model token rates (USD per 1M tokens) used to
// estimate the cost of captured traffic. The data is a curated embedded
// snapshot (models.json) covering the models the built-in templates reference
// plus common public models. Models not in the table are unpriced: they
// contribute $0 and the UI flags estimated totals with an unpriced-request
// count, so a cost total is never silently understated as exact.
//
// Rates are best-effort list prices and can drift; treat all totals as
// estimates, not an invoice.
package pricing

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	_ "embed"
)

//go:embed models.json
var modelsJSON []byte

// Prices is one model's token rates in USD per 1M tokens. Zero values mean the
// rate is unknown (and not charged in the estimate).
type Prices struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read,omitempty"`
	CacheWrite float64 `json:"cache_write,omitempty"`
	// Local marks local/sandbox models (ollama, vllm, custom endpoints) that
	// run free. It is distinct from "unknown": a local model is intentionally
	// unpriced rather than missing from the table.
	Local bool `json:"local,omitempty"`
}

// Table is an immutable pricing lookup keyed by exact model id.
type Table struct {
	mu     sync.RWMutex
	models map[string]Prices
}

// Load returns the pricing table from the embedded snapshot.
func Load() (*Table, error) {
	var m map[string]Prices
	if err := json.Unmarshal(modelsJSON, &m); err != nil {
		return nil, fmt.Errorf("pricing: parse embedded models.json: %w", err)
	}
	return &Table{models: m}, nil
}

// Lookup returns the prices for a model id. The exact id is tried first; ids
// that carry a provider prefix in the forwarded model name (e.g.
// "deepseek/deepseek-v4-flash") fall back to the part after the last slash, so
// gateway-translated and prefixed model names still match their table entry.
// ok is false for models not in the table.
func (t *Table) Lookup(model string) (Prices, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if p, ok := t.models[model]; ok {
		return p, true
	}
	if i := strings.LastIndexByte(model, '/'); i >= 0 && i+1 < len(model) {
		if p, ok := t.models[model[i+1:]]; ok {
			return p, true
		}
	}
	return Prices{}, false
}

// Estimate converts token counts to a cost split using p. Input tokens are the
// prompt tokens charged at the input rate after subtracting cache reads;
// cache-write tokens are not derivable from the OpenAI usage shape the gateway
// records, so the split is an estimate when cache_write > 0.
func (p Prices) Estimate(prompt, completion, cached int64) Split {
	in := prompt - cached
	if in < 0 {
		in = 0
	}
	return Split{
		Input:      float64(in) / 1e6 * p.Input,
		Output:     float64(completion) / 1e6 * p.Output,
		CacheRead:  float64(cached) / 1e6 * p.CacheRead,
		CacheWrite: 0,
	}
}

// Split is one request's estimated cost by token category, in USD.
type Split struct {
	Input      float64
	Output     float64
	CacheRead  float64
	CacheWrite float64
}

// Total returns the sum of the split's components.
func (s Split) Total() float64 {
	return s.Input + s.Output + s.CacheRead + s.CacheWrite
}
