package pricing

import (
	"math"
	"testing"
)

func TestLoadAndLookup(t *testing.T) {
	tab, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	p, ok := tab.Lookup("gpt-4o-mini")
	if !ok {
		t.Fatal("gpt-4o-mini should be priced")
	}
	if p.Input != 0.15 || p.Output != 0.6 {
		t.Errorf("gpt-4o-mini prices = %+v, want input 0.15 output 0.6", p)
	}
	if _, ok := tab.Lookup("no-such-model-xyz"); ok {
		t.Error("unknown model should not be found")
	}
}

func TestLookupLocalMarked(t *testing.T) {
	tab, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	p, ok := tab.Lookup("llama3.1")
	if !ok {
		t.Fatal("llama3.1 should be present")
	}
	if !p.Local {
		t.Error("llama3.1 should be marked local (free)")
	}
}

func TestLookupSlashFallback(t *testing.T) {
	tab, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Forwarded model ids often carry a provider prefix; the part after the
	// last slash must still resolve.
	for _, id := range []string{"deepseek/deepseek-v4-flash", "deepseek/deepseek-v4-pro", "openai/gpt-4o-mini"} {
		if _, ok := tab.Lookup(id); !ok {
			t.Errorf("Lookup(%q) should fall back past the slash", id)
		}
	}
	// No slash and unknown → miss; a slash-prefixed unknown → miss.
	for _, id := range []string{"no-such-model-xyz", "stealth/ox-alpha", "z-ai/glm-5.2:free"} {
		if _, ok := tab.Lookup(id); ok {
			t.Errorf("Lookup(%q) should miss", id)
		}
	}
	// Bare prefix "/gpt-4o" would be ambiguous; the fallback still applies to
	// the empty prefix side only when a real suffix exists.
	if _, ok := tab.Lookup("gpt-4o/"); ok {
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
