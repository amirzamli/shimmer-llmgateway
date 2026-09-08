package pricing

import (
	"encoding/json"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// loadFixture writes the fixture table to a temp file and loads it through
// Load (the runtime entry point) — the package no longer embeds a table, so
// tests supply their own file.
func loadFixture(t *testing.T) *Table {
	t.Helper()
	path := filepath.Join(t.TempDir(), "models.json")
	if err := os.WriteFile(path, []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	tab, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return tab
}

func TestLoadAndLookup(t *testing.T) {
	tab := loadFixture(t)
	p, ok := tab.Lookup("gpt-4o-mini", "openai", "")
	if !ok {
		t.Fatal("gpt-4o-mini should be priced")
	}
	if p.Input != 0.15 || p.Output != 0.6 {
		t.Errorf("gpt-4o-mini prices = %+v, want input 0.15 output 0.6", p)
	}
	if _, ok := tab.Lookup("no-such-model-xyz", "", ""); ok {
		t.Error("unknown model should not be found")
	}
}

func TestLoadMissingTableDirectsToGenerator(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	_, err := Load(path) // never written: the fresh-checkout case
	if err == nil {
		t.Fatal("missing table should be an error")
	}
	if !strings.Contains(err.Error(), "just update-prices") || !strings.Contains(err.Error(), "cmd/genpricing") {
		t.Errorf("error = %v, want the just update-prices / cmd/genpricing hint", err)
	}
}

func TestLoadStaleSchemaHintsRegenerate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	stale := strings.Replace(fixture, `"schema": 1`, `"schema": 99`, 1)
	if err := os.WriteFile(path, []byte(stale), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("stale schema should be an error")
	}
	if !strings.Contains(err.Error(), "just update-prices") {
		t.Errorf("error = %v, want the regenerate hint", err)
	}
}

// TestLoadGeneratedCatalog runs the real generator (which fetches the same
// models.dev endpoint as cmd/genpricing) and loads the table it wrote — an
// end-to-end check of the Load(path) contract against live catalog data. It
// skips when the network or the go toolchain is unavailable, so air-gapped
// runs still pass.
func TestLoadGeneratedCatalog(t *testing.T) {
	if testing.Short() {
		t.Skip("network test skipped in -short mode")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not in PATH")
	}
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Skip("cannot locate the repo root")
	}
	root := filepath.Dir(filepath.Dir(filepath.Dir(thisFile))) // internal/pricing → repo root
	out := filepath.Join(t.TempDir(), "models.json")
	cmd := exec.Command(goBin, "run", "./cmd/genpricing", "-out", out, "-timeout", "30s")
	cmd.Dir = root
	if err := cmd.Run(); err != nil {
		t.Skipf("generator unavailable (offline?): %v", err)
	}
	tab, err := Load(out)
	if err != nil {
		t.Fatalf("Load generated table: %v", err)
	}
	if !strings.HasPrefix(tab.Schema(), "models.dev@") {
		t.Errorf("Schema() = %q, want a models.dev@<date> stamp", tab.Schema())
	}
	// The live catalog must yield at least one priced model, resolvable end
	// to end through the table.
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var ft FileTable
	if err := json.Unmarshal(data, &ft); err != nil {
		t.Fatal(err)
	}
	for _, p := range ft.Providers {
		for _, m := range p.Models {
			if m.Input == nil && m.Output == nil {
				continue
			}
			if _, ok := tab.Lookup(m.ID, p.ID, ""); !ok {
				t.Errorf("Lookup(%q, %q) from the generated table should be priced", m.ID, p.ID)
			}
			return // one end-to-end priced lookup is enough
		}
	}
	t.Error("generated catalog contains no priced model")
}

func TestLookupLocalMarked(t *testing.T) {
	tab := loadFixture(t)
	p, ok := tab.Lookup("llama3.1", "ollama", "")
	if !ok {
		t.Fatal("llama3.1 should be present")
	}
	if !p.Local {
		t.Error("llama3.1 should be marked local (free)")
	}
}

func TestLookupSlashFallback(t *testing.T) {
	tab := loadFixture(t)
	// Forwarded model ids often carry a provider prefix; the part after the
	// last slash must still resolve.
	for _, id := range []string{"deepseek/deepseek-v4-flash", "deepseek/deepseek-v4-pro", "openai/gpt-4o-mini"} {
		if _, ok := tab.Lookup(id, "", ""); !ok {
			t.Errorf("Lookup(%q) should fall back past the slash", id)
		}
	}
	// No slash and unknown → miss; a slash-prefixed unknown → miss.
	for _, id := range []string{"no-such-model-xyz", "stealth/ox-alpha", "stealth/stealth-unknown"} {
		if _, ok := tab.Lookup(id, "", ""); ok {
			t.Errorf("Lookup(%q) should miss", id)
		}
	}
	// Bare prefix "/gpt-4o" would be ambiguous; the fallback still applies to
	// the empty prefix side only when a real suffix exists.
	if _, ok := tab.Lookup("gpt-4o/", "", ""); ok {
		t.Error("Lookup with empty suffix should miss")
	}
}

func TestEstimateSplit(t *testing.T) {
	p := Prices{Input: 1, Output: 2, CacheRead: 0.5}
	// 1M tokens input, half cached: 500k at $1 + 500k at $0.5 = $0.75 input+cache.
	s := p.Estimate(1_000_000, 250_000, 500_000)
	if math.Abs(s.Input-0.5) > 1e-9 {
		t.Errorf("input = %v, want 0.5", s.Input)
	}
	if math.Abs(s.CacheRead-0.25) > 1e-9 {
		t.Errorf("cache read = %v, want 0.25", s.CacheRead)
	}
	if math.Abs(s.Output-0.5) > 1e-9 {
		t.Errorf("output = %v, want 0.5", s.Output)
	}
	if s.CacheWrite != 0 {
		t.Errorf("cache write = %v, want 0 (not derivable from usage)", s.CacheWrite)
	}
	if math.Abs(s.Total()-1.25) > 1e-9 {
		t.Errorf("total = %v, want 1.25", s.Total())
	}
	// Cached count above the prompt count is clamped: no negative input cost.
	s = p.Estimate(100, 10, 500)
	if s.Input != 0 {
		t.Errorf("clamped input = %v, want 0", s.Input)
	}
	if math.Abs(s.CacheRead-0.00025) > 1e-9 {
		t.Errorf("cache read = %v, want 0.00025", s.CacheRead)
	}
}

// fixture is a controlled generated-table shape exercising every lookup path.
const fixture = `{
  "schema": 1,
  "source": "https://models.dev/api.json",
  "fetched_at": "2026-08-31T12:00:00Z",
  "providers": [
    {"id": "openai", "name": "OpenAI", "base_url": "https://api.openai.com/v1", "models": [
      {"id": "gpt-4o", "input": 2.5, "output": 10, "cache_read": 1.25},
      {"id": "gpt-4o-mini", "input": 0.15, "output": 0.6, "cache_read": 0.075},
      {"id": "gpt-free", "input": 0, "output": 0},
      {"id": "no-price-model"}
    ]},
    {"id": "ollama", "name": "ollama", "base_url": "http://localhost:11434/v1", "models": [
      {"id": "llama3.1", "local": true}
    ]},
    {"id": "deepseek", "name": "DeepSeek", "base_url": "https://api.deepseek.com", "models": [
      {"id": "deepseek-v4-flash", "input": 0.14, "output": 0.28, "cache_read": 0.0028},
      {"id": "deepseek-v4-pro", "input": 0.28, "output": 1.14}
    ]},
    {"id": "zai", "name": "Z.AI", "base_url": "https://api.z.ai/api/paas/v4", "models": [
      {"id": "glm-5.3-flash", "input": 0.075, "output": 0.25, "cache_read": 0.015}
    ]},
    {"id": "commandcode", "name": "commandcode", "base_url": "https://api.commandcode.ai/provider/v1",
     "inherited": true, "models": [
      {"id": "deepseek/deepseek-v4-flash", "input": 0.14, "output": 0.28, "cache_read": 0.0028, "inherited_from": "deepseek"},
      {"id": "no-price-model"}
    ]}
  ]
}`

func parseFixture(t *testing.T) *Table {
	t.Helper()
	tab, err := parse([]byte(fixture))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return tab
}

func TestParseSchema(t *testing.T) {
	if got := parseFixture(t).Schema(); got != "models.dev@2026-08-31" {
		t.Errorf("Schema() = %q", got)
	}
	if _, err := parse([]byte(`{"schema": 99, "fetched_at": "2026-08-31T12:00:00Z", "providers": []}`)); err == nil {
		t.Error("unknown schema version should be rejected")
	}
}

func TestLookupResolutionLadder(t *testing.T) {
	tab := parseFixture(t)

	// Exact (provider, model) pair wins, carrying the inheritance marker.
	p, ok := tab.Lookup("deepseek/deepseek-v4-flash", "commandcode", "")
	if !ok || p.Input != 0.14 || p.Inherited != "deepseek" {
		t.Errorf("commandcode exact = %+v ok %v", p, ok)
	}

	// The origin lab's own price is distinct from the reseller entry.
	p, ok = tab.Lookup("deepseek/deepseek-v4-flash", "deepseek", "")
	if !ok || p.Input != 0.14 || p.Inherited != "" {
		t.Errorf("deepseek exact = %+v ok %v", p, ok)
	}

	// A user-defined custom endpoint resolves by base URL to its catalog
	// provider, without the provider name matching anything.
	p, ok = tab.Lookup("glm-5.3-flash", "zai-custom", "https://api.z.ai/api/paas/v4/")
	if !ok || p.Input != 0.075 {
		t.Errorf("baseURL resolve = %+v ok %v", p, ok)
	}
	// An unmatchable base URL falls through the ladder to the bare model id
	// (first provider in table order) — the pre-existing bare-lookup behavior.
	p, ok = tab.Lookup("glm-5.3-flash", "zai-custom", "https://other.example/v1")
	if !ok || p.Input != 0.075 {
		t.Errorf("unmatched baseURL falls through to bare id = %+v ok %v", p, ok)
	}

	// Bare model id resolves across providers in table order.
	p, ok = tab.Lookup("gpt-4o", "", "")
	if !ok || p.Input != 2.5 {
		t.Errorf("bare resolve = %+v ok %v", p, ok)
	}

	// Canonical fallback: an unknown prefixed id resolves to the canonical
	// entry (first provider in file order wins).
	p, ok = tab.Lookup("unknown-lab/deepseek-v4-flash", "", "")
	if !ok || p.Input != 0.14 {
		t.Errorf("canonical fallback = %+v ok %v", p, ok)
	}

	// "Free" (present-but-zero) is priced; "no price data" is not.
	if p, ok := tab.Lookup("gpt-free", "", ""); !ok || p.Input != 0 || p.Output != 0 {
		t.Errorf("free model = %+v ok %v, want priced at $0", p, ok)
	}
	if _, ok := tab.Lookup("no-price-model", "commandcode", ""); ok {
		t.Error("model without price data must be unpriced")
	}
	if _, ok := tab.Lookup("no-such-model", "", ""); ok {
		t.Error("unknown model must be unpriced")
	}
}
