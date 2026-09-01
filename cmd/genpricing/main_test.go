package main

import (
	"testing"

	"github.com/amirzamli/shimmer-llmgateway/internal/config"
	"github.com/amirzamli/shimmer-llmgateway/internal/pricing"
)

// devFixture is a small fake catalog exercising the mapping and inheritance
// rules: an aggregator re-listing a lab model at a different price, a lab with
// no price data, and a provider absent entirely.
var devFixture = map[string]devProvider{
	"openai": {ID: "openai", Name: "OpenAI", Models: map[string]devModel{
		"gpt-5.6-luna": {ID: "gpt-5.6-luna", Name: "GPT-5.6 Luna", Cost: &devCost{
			Input: fp(0.2), Output: fp(1.2), CacheRead: fp(0.02)}},
	}},
	"deepseek": {ID: "deepseek", Name: "DeepSeek", API: "https://api.deepseek.com", Models: map[string]devModel{
		"deepseek-v4-flash": {ID: "deepseek-v4-flash", Name: "DeepSeek V4 Flash", Cost: &devCost{
			Input: fp(0.14), Output: fp(0.28), CacheRead: fp(0.0028)}},
		"deepseek-secret": {ID: "deepseek-secret", Name: "Unpriced model"},
	}},
	"zai": {ID: "zai", Name: "Z.AI", API: "https://api.z.ai/api/paas/v4", Models: map[string]devModel{
		"glm-5.3-flash": {ID: "glm-5.3-flash", Name: "GLM-5.3 Flash", Cost: &devCost{
			Input: fp(0.075), Output: fp(0.25), CacheRead: fp(0.015)}},
	}},
	"opencode-go": {ID: "opencode-go", Name: "OpenCode Go", Models: map[string]devModel{
		// Aggregator re-listing at a different price than the lab's.
		"glm-5.3-flash": {ID: "glm-5.3-flash", Name: "GLM-5.3 Flash", Cost: &devCost{
			Input: fp(0.6), Output: fp(2.2)}},
	}},
}

func fp(v float64) *float64 { return &v }

func tmpl(name, base string, models ...string) config.Template {
	return config.Template{Name: name, BaseURL: base, Models: models}
}

func TestBuildTableOrderAndMapping(t *testing.T) {
	templates := map[string]config.Template{
		"openai":       tmpl("openai", "https://api.openai.com/v1", "gpt-4o"),
		"deepseek":     tmpl("deepseek", "https://api.deepseek.com", "deepseek-v4-flash"),
		"zai":          tmpl("zai", "https://api.z.ai/api/paas/v4", "glm-4.6"),
		"opencode_go":  tmpl("opencode_go", "https://opencode.ai/zen/go/v1", "kimi-k2"),
		"commandcode":  tmpl("commandcode", "https://api.commandcode.ai/provider/v1", "deepseek/deepseek-v4-flash"),
		"ollama":       tmpl("ollama", "http://localhost:11434/v1", "llama3.1"),
		"unknown_tmpl": tmpl("unknown_tmpl", "https://no-such.example/v1", "mystery"),
	}
	got := buildTable(devFixture, templates)
	var ids []string
	for _, p := range got {
		ids = append(ids, p.ID)
	}
	// Labs first (bare-model tie-breaks prefer list prices), then resellers,
	// then locals; the unmatched template is skipped.
	want := []string{"openai", "deepseek", "zai", "opencode-go", "commandcode", "ollama"}
	if len(ids) != len(want) {
		t.Fatalf("providers = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("providers = %v, want %v", ids, want)
		}
	}

	// Catalog-backed entries carry the catalog's API base and full model list.
	for _, p := range got {
		switch p.ID {
		case "deepseek":
			if p.BaseURL != "https://api.deepseek.com" || len(p.Models) != 2 {
				t.Errorf("deepseek = base %q models %d", p.BaseURL, len(p.Models))
			}
		case "ollama":
			if !p.Models[0].Local {
				t.Error("ollama models must be local")
			}
		case "opencode-go":
			if p.BaseURL == "" {
				t.Error("opencode-go must carry its catalog base URL for custom-endpoint matching")
			}
		}
	}
}

func TestInheritProviderEntry(t *testing.T) {
	cc := tmpl("commandcode", "https://api.commandcode.ai/provider/v1",
		"deepseek/deepseek-v4-flash", "gpt-5.6-luna", "z-ai/glm-5.3-flash", "mystery-model")
	p := inheritProviderEntry(cc, devFixture)
	if !p.Inherited || p.ID != "commandcode" {
		t.Fatalf("provider = %+v", p)
	}
	byID := map[string]pricing.FileModel{}
	for _, m := range p.Models {
		byID[m.ID] = m
	}
	if m := byID["deepseek/deepseek-v4-flash"]; m.InheritedFrom != "deepseek" ||
		m.Input == nil || *m.Input != 0.14 {
		t.Errorf("deepseek inherit = %+v", m)
	}
	// Entries keep the template's verbatim model id, so the runtime's exact
	// (provider, model) lookup hits without slash fallback.
	if _, ok := byID["deepseek-v4-flash"]; ok {
		t.Error("inherited entries must keep the verbatim template id")
	}
	if m := byID["gpt-5.6-luna"]; m.InheritedFrom != "openai" || m.Input == nil || *m.Input != 0.2 {
		t.Errorf("openai scan inherit = %+v", m)
	}
	// The lab prefix hint beats an aggregator re-listing at a higher price.
	if m := byID["z-ai/glm-5.3-flash"]; m.InheritedFrom != "zai" || m.Input == nil || *m.Input != 0.075 {
		t.Errorf("z-ai hint inherit = %+v", m)
	}
	// No catalog source anywhere: emitted unpriced, no inheritance marker.
	if m := byID["mystery-model"]; m.InheritedFrom != "" || m.Input != nil {
		t.Errorf("unresolvable model = %+v, want unpriced", m)
	}
}
