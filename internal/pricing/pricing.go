// Package pricing provides per-model token rates (USD per 1M tokens) used to
// estimate the cost of captured traffic. The table is GENERATED from the
// models.dev catalog (https://models.dev/api.json) by cmd/genpricing and
// loaded from disk at startup (never embedded in the binary); regenerate with
// `just update-prices` (or go run ./cmd/genpricing) and restart the gateway.
//
// The table is provider-keyed: the same model id can carry different prices on
// different providers (e.g. DeepSeek's own API vs an aggregator reselling it),
// so lookup takes the serving provider (the gateway template name) and, for
// user-defined custom endpoints, the upstream base URL — a custom endpoint
// whose URL matches a catalog provider's API base uses that provider's prices.
// Providers absent from the catalog (e.g. commandcode) are emitted with
// inherited prices copied from the origin lab's own provider entry and carry
// an inherited_from marker in the JSON.
//
// Rates are best-effort list prices and can drift; treat all totals as
// estimates, not an invoice. The cost split is computed once at capture time
// and frozen in the store — later table updates never rewrite historical
// rows; requests.cost_schema records which snapshot priced each row.
package pricing

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"sync"
)

// DefaultPath is where the gateway looks for the generated pricing table,
// relative to the working directory: the same path cmd/genpricing (the
// `update-prices` justfile target) writes by default, so generating and
// running from the repo root needs no configuration.
const DefaultPath = "internal/pricing/models.json"

// TableSchema is the models.json format version parse accepts; the generator
// stamps it into the file it writes.
const TableSchema = 1

// Prices is one model's token rates in USD per 1M tokens. Zero values mean the
// rate is unknown (and not charged in the estimate).
type Prices struct {
	Input      float64
	Output     float64
	CacheRead  float64
	CacheWrite float64
	// Local marks local/sandbox models (ollama, vllm, custom endpoints) that
	// run free. It is distinct from "unknown": a local model is intentionally
	// unpriced rather than missing from the table.
	Local bool
	// Inherited is the source-catalog provider the prices were copied from
	// when the serving provider itself is absent from the catalog (e.g.
	// commandcode). Metadata only; estimates are unaffected.
	Inherited string
}

// FileModel is one model entry in the generated models.json. The price fields
// are pointers so "no price data" (absent) stays distinct from "free" (present
// and zero): an entry with neither input nor output is unpriced.
type FileModel struct {
	ID            string   `json:"id"`
	Name          string   `json:"name,omitempty"`
	Input         *float64 `json:"input,omitempty"`
	Output        *float64 `json:"output,omitempty"`
	CacheRead     *float64 `json:"cache_read,omitempty"`
	CacheWrite    *float64 `json:"cache_write,omitempty"`
	Local         bool     `json:"local,omitempty"`
	InheritedFrom string   `json:"inherited_from,omitempty"`
}

// FileProvider is one provider entry in the generated models.json.
type FileProvider struct {
	ID      string `json:"id"`
	Name    string `json:"name,omitempty"`
	BaseURL string `json:"base_url,omitempty"`
	// Inherited marks a provider absent from the source catalog whose model
	// prices were copied from origin-lab entries (see FileModel.InheritedFrom).
	Inherited bool        `json:"inherited,omitempty"`
	Models    []FileModel `json:"models"`
}

// FileTable is the generated models.json envelope.
type FileTable struct {
	Schema    int             `json:"schema"`
	Source    string          `json:"source"`
	FetchedAt string          `json:"fetched_at"`
	Providers []*FileProvider `json:"providers"`
}

// Table is an immutable pricing lookup over the loaded snapshot. It is safe
// for concurrent use.
type Table struct {
	mu          sync.RWMutex
	schema      string
	providers   []*FileProvider          // file order; bare-model lookups prefer earlier entries
	byPair      map[[2]string]*FileModel // (provider, model id) -> entry
	byCanonical map[string]*FileModel    // model id (last-slash canonical) -> first priced entry
	baseURLs    map[string]string        // normalized base URL -> provider id
}

// Load reads the generated pricing table from path. A missing table and a
// table that fails to parse are errors directing the operator to the
// generator (the `update-prices` justfile target); cost estimation stays off
// until a table is present.
func Load(path string) (*Table, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("pricing: no pricing table at %s (generate one with `just update-prices`, or go run ./cmd/genpricing)", path)
		}
		return nil, fmt.Errorf("pricing: read %s: %w", path, err)
	}
	t, err := parse(data)
	if err != nil {
		return nil, fmt.Errorf("pricing: %w (regenerate the table with `just update-prices`, or go run ./cmd/genpricing)", err)
	}
	return t, nil
}

// parse builds a Table from generated models.json bytes.
func parse(data []byte) (*Table, error) {
	var ft FileTable
	if err := json.Unmarshal(data, &ft); err != nil {
		return nil, fmt.Errorf("pricing: parse models.json: %w", err)
	}
	if ft.Schema != TableSchema {
		return nil, fmt.Errorf("pricing: models.json schema %d, want %d", ft.Schema, TableSchema)
	}
	t := &Table{
		schema:      "models.dev@" + dateOf(ft.FetchedAt),
		byPair:      make(map[[2]string]*FileModel),
		byCanonical: make(map[string]*FileModel),
		baseURLs:    make(map[string]string),
	}
	for _, p := range ft.Providers {
		if p == nil || p.ID == "" {
			continue
		}
		t.providers = append(t.providers, p)
		if p.BaseURL != "" {
			if norm := normalizeURL(p.BaseURL); norm != "" {
				t.baseURLs[norm] = p.ID
			}
		}
		for i := range p.Models {
			m := &p.Models[i]
			if m.ID == "" {
				continue
			}
			t.byPair[[2]string{p.ID, m.ID}] = m
			canon := canonicalModel(m.ID)
			if _, seen := t.byCanonical[canon]; !seen {
				t.byCanonical[canon] = m
			}
		}
	}
	return t, nil
}

// dateOf reduces an RFC3339 timestamp to its YYYY-MM-DD date part.
func dateOf(ts string) string {
	if len(ts) > 10 {
		return ts[:10]
	}
	return ts
}

// ModelIDs returns the model IDs listed for provider in catalog order.
func (t *Table) ModelIDs(provider string) []string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	for _, p := range t.providers {
		if p.ID != provider {
			continue
		}
		ids := make([]string, 0, len(p.Models))
		for _, m := range p.Models {
			if m.ID != "" {
				ids = append(ids, m.ID)
			}
		}
		return ids
	}
	return nil
}

// Schema returns a stable identifier of the loaded snapshot (the source
// catalog plus its fetch date), stamped onto priced request rows.
func (t *Table) Schema() string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.schema
}

// Lookup returns the prices for a model id as served by provider (the gateway
// template name) with the upstream base URL baseURL (empty when unknown).
// Resolution order: exact (provider, model) pair; a base-URL match to a
// catalog provider; the model id across providers in table order (preferring
// origin-lab prices over reseller markups); then the same ladder for the id
// after its last slash (catalog-forwarded ids like "deepseek/deepseek-v4-
// flash" resolve to the canonical entry). ok is false for models with no
// price data.
func (t *Table) Lookup(model, provider, baseURL string) (Prices, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if p, ok := t.resolve(model, provider, baseURL); ok {
		return p, true
	}
	if canon := canonicalModel(model); canon != model {
		return t.resolve(canon, provider, baseURL)
	}
	return Prices{}, false
}

// resolve runs one resolution ladder for a single id (no slash fallback).
func (t *Table) resolve(model, provider, baseURL string) (Prices, bool) {
	if provider != "" {
		if m, ok := t.byPair[[2]string{provider, model}]; ok {
			return pricesOf(m)
		}
	}
	if id := t.providerByBaseURL(baseURL); id != "" {
		if m, ok := t.byPair[[2]string{id, model}]; ok {
			return pricesOf(m)
		}
	}
	if m, ok := t.byCanonical[model]; ok {
		return pricesOf(m)
	}
	return Prices{}, false
}

// providerByBaseURL maps a normalized upstream base URL to its catalog
// provider id ("" when unknown), so user-defined custom endpoints inherit the
// prices of the catalog provider they proxy.
func (t *Table) providerByBaseURL(baseURL string) string {
	if baseURL == "" {
		return ""
	}
	return t.baseURLs[normalizeURL(baseURL)]
}

// canonicalModel returns the id after its last slash ("deepseek/deepseek-v4-
// flash" → "deepseek-v4-flash"), or the id unchanged when it has no slash.
func canonicalModel(model string) string {
	if i := strings.LastIndexByte(model, '/'); i >= 0 && i+1 < len(model) {
		return model[i+1:]
	}
	return model
}

// normalizeURL lowercases and trims the trailing slash so template and catalog
// base URLs compare equal despite formatting.
func normalizeURL(u string) string {
	return strings.ToLower(strings.TrimRight(strings.TrimSpace(u), "/"))
}

// pricesOf converts a file entry to the lookup result. An entry with no price
// fields at all is not priced; a present-but-zero price is a real $0.
func pricesOf(m *FileModel) (Prices, bool) {
	p := Prices{Local: m.Local, Inherited: m.InheritedFrom}
	if m.Input != nil {
		p.Input = *m.Input
	}
	if m.Output != nil {
		p.Output = *m.Output
	}
	if m.CacheRead != nil {
		p.CacheRead = *m.CacheRead
	}
	if m.CacheWrite != nil {
		p.CacheWrite = *m.CacheWrite
	}
	if m.Input == nil && m.Output == nil && !m.Local {
		return Prices{}, false
	}
	return p, true
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
