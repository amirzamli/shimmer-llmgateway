package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const specExample = `
listen = "127.0.0.1:8787"
store  = "gateway.db"
retention_days = 30

[settings]
default_alias = "openai"
request_plugins = []
response_plugins = []

[providers.openai]
base_url = "https://api.openai.com/v1"
api_key_env = "OPENAI_API_KEY"
models = ["gpt-4o", "gpt-4o-mini"]

[providers.ollama]
base_url = "http://localhost:11434/v1"
api_key_env = ""
models = ["llama3.1"]

[[instances]]
alias = "openai"
template = "openai"
api_key_env = "OPENAI_API_KEY"

[[instances]]
alias = "openai-2"
template = "openai"
api_key_env = "OPENAI_API_KEY_2"
plugins = ["redact"]
`

func mustParse(t *testing.T, data string) *Config {
	t.Helper()
	cfg, err := Parse([]byte(data))
	if err != nil {
		t.Fatalf("Parse(%q) failed: %v", data, err)
	}
	return cfg
}

func assertAliases(t *testing.T, cfg *Config, want ...string) {
	t.Helper()
	if len(cfg.Instances) != len(want) {
		t.Fatalf("got %d instances, want %d", len(cfg.Instances), len(want))
	}
	for i, w := range want {
		if got := cfg.Instances[i].Alias; got != w {
			t.Errorf("instance %d alias = %q, want %q", i, got, w)
		}
	}
}

func TestParseSpecExample(t *testing.T) {
	cfg := mustParse(t, specExample)

	if cfg.Listen != "127.0.0.1:8787" {
		t.Errorf("listen = %q, want 127.0.0.1:8787", cfg.Listen)
	}
	if cfg.Store != "gateway.db" {
		t.Errorf("store = %q, want gateway.db", cfg.Store)
	}
	if cfg.RetentionDays != 30 {
		t.Errorf("retention_days = %d, want 30", cfg.RetentionDays)
	}
	if cfg.Settings.DefaultAlias != "openai" {
		t.Errorf("default_alias = %q, want openai", cfg.Settings.DefaultAlias)
	}

	assertAliases(t, cfg, "openai", "openai-2")

	inst2 := cfg.Instances[1]
	if got := inst2.EffectiveAPIKeyEnv(cfg); got != "OPENAI_API_KEY_2" {
		t.Errorf("instance 2 api_key_env = %q, want OPENAI_API_KEY_2", got)
	}
	if len(inst2.Plugins) != 1 || inst2.Plugins[0] != "redact" {
		t.Errorf("instance 2 plugins = %v, want [redact]", inst2.Plugins)
	}
	// Template list is the default for instances without a subset.
	if got := inst2.EffectiveModels(cfg); !contains(got, "gpt-4o") || !contains(got, "gpt-4o-mini") {
		t.Errorf("instance 2 effective models = %v, want template defaults", got)
	}
}

func TestAutoNaming(t *testing.T) {
	cfg := mustParse(t, `
[[instances]]
template = "openai"

[[instances]]
template = "openai"
`)
	assertAliases(t, cfg, "openai", "openai-2")
}

func TestAutoNamingThree(t *testing.T) {
	cfg := mustParse(t, `
[[instances]]
template = "openai"

[[instances]]
template = "openai"

[[instances]]
template = "openai"
`)
	assertAliases(t, cfg, "openai", "openai-2", "openai-3")
}

func TestAutoNamingSkipsTakenAliases(t *testing.T) {
	// openai-2 is taken explicitly, so the third instance skips to openai-3.
	cfg := mustParse(t, `
[[instances]]
template = "openai"

[[instances]]
template = "openai"
alias = "openai-2"

[[instances]]
template = "openai"
`)
	assertAliases(t, cfg, "openai", "openai-2", "openai-3")
}

func TestAutoNamingMixedTemplates(t *testing.T) {
	cfg := mustParse(t, `
[[instances]]
template = "openai"

[[instances]]
template = "openai"

[[instances]]
template = "ollama"

[[instances]]
template = "ollama"
`)
	assertAliases(t, cfg, "openai", "openai-2", "ollama", "ollama-2")
}

func TestAutoNamingSkipsOtherTemplatesAlias(t *testing.T) {
	// An explicit alias from another template reserves the name.
	cfg := mustParse(t, `
[[instances]]
template = "openai"

[[instances]]
alias = "openai-2"
template = "ollama"

[[instances]]
template = "openai"
`)
	assertAliases(t, cfg, "openai", "openai-2", "openai-3")
}

func TestRoutingPrefixed(t *testing.T) {
	cfg := mustParse(t, specExample)

	inst, model, err := cfg.Resolve("openai-2/gpt-4o")
	if err != nil {
		t.Fatalf("Resolve(openai-2/gpt-4o): %v", err)
	}
	if inst.Alias != "openai-2" {
		t.Errorf("instance alias = %q, want openai-2", inst.Alias)
	}
	if model != "gpt-4o" {
		t.Errorf("model = %q, want gpt-4o", model)
	}
}

func TestRoutingPrefixedPassthroughModel(t *testing.T) {
	// Prefixed routing returns the model verbatim; membership is not checked.
	cfg := mustParse(t, specExample)
	inst, model, err := cfg.Resolve("openai-2/anything-else")
	if err != nil {
		t.Fatalf("Resolve(openai-2/anything-else): %v", err)
	}
	if inst.Alias != "openai-2" || model != "anything-else" {
		t.Errorf("got (%q, %q), want (openai-2, anything-else)", inst.Alias, model)
	}
}

func TestRoutingUnknownAliasCarriesAliasList(t *testing.T) {
	cfg := mustParse(t, specExample)

	_, _, err := cfg.Resolve("nope/gpt-4o")
	if err == nil {
		t.Fatal("Resolve(nope/gpt-4o) returned no error")
	}
	var iae *UnknownAliasError
	if !errors.As(err, &iae) {
		t.Fatalf("error type = %T, want *UnknownAliasError", err)
	}
	if iae.Alias != "nope" {
		t.Errorf("error alias = %q, want nope", iae.Alias)
	}
	want := []string{"openai", "openai-2"}
	if strings.Join(iae.Aliases, ",") != strings.Join(want, ",") {
		t.Errorf("available aliases = %v, want %v", iae.Aliases, want)
	}
}

func TestRoutingUnprefixedDefaultAliasPrecedence(t *testing.T) {
	// Both instances list gpt-4o via the template; default_alias wins.
	cfg := mustParse(t, specExample)

	inst, model, err := cfg.Resolve("gpt-4o")
	if err != nil {
		t.Fatalf("Resolve(gpt-4o): %v", err)
	}
	if inst.Alias != "openai" { // default_alias = "openai"
		t.Errorf("instance alias = %q, want openai (default_alias)", inst.Alias)
	}
	if model != "gpt-4o" {
		t.Errorf("model = %q, want gpt-4o", model)
	}
}

func TestRoutingUnprefixedDefaultAliasFallsBackWhenModelMissing(t *testing.T) {
	// default_alias instance has a subset that excludes gpt-4o, so the first
	// instance listing the model wins.
	cfg := mustParse(t, `
[settings]
default_alias = "openai-2"

[[instances]]
alias = "openai"
template = "openai"

[[instances]]
alias = "openai-2"
template = "openai"
models = ["gpt-4o-mini"]
`)
	inst, _, err := cfg.Resolve("gpt-4o")
	if err != nil {
		t.Fatalf("Resolve(gpt-4o): %v", err)
	}
	if inst.Alias != "openai" {
		t.Errorf("instance alias = %q, want openai (first listing the model)", inst.Alias)
	}
}

func TestRoutingUnprefixedUnknownModel(t *testing.T) {
	cfg := mustParse(t, specExample)

	_, _, err := cfg.Resolve("no-such-model")
	if err == nil {
		t.Fatal("Resolve(no-such-model) returned no error")
	}
	var ume *UnknownModelError
	if !errors.As(err, &ume) {
		t.Fatalf("error type = %T, want *UnknownModelError", err)
	}
	if ume.Model != "no-such-model" {
		t.Errorf("error model = %q, want no-such-model", ume.Model)
	}
}

func TestInstanceModelSubsetRouting(t *testing.T) {
	cfg := mustParse(t, `
[[instances]]
alias = "mini"
template = "openai"
models = ["gpt-4o-mini"]
`)
	// Unprefixed: only gpt-4o-mini is routable via this instance.
	if _, _, err := cfg.Resolve("gpt-4o"); !errors.As(err, new(*UnknownModelError)) {
		t.Errorf("Resolve(gpt-4o) err = %v, want UnknownModelError", err)
	}
	inst, model, err := cfg.Resolve("gpt-4o-mini")
	if err != nil {
		t.Fatalf("Resolve(gpt-4o-mini): %v", err)
	}
	if inst.Alias != "mini" || model != "gpt-4o-mini" {
		t.Errorf("got (%q, %q), want (mini, gpt-4o-mini)", inst.Alias, model)
	}
}

func TestBuiltinTemplatesPresent(t *testing.T) {
	cfg := mustParse(t, "")
	want := []string{
		"openai", "anthropic", "ollama", "groq", "vllm", "lite_llm",
		"openrouter", "deepseek", "gemini", "mistral", "kimi", "zai",
		"opencode_zen", "opencode_go",
	}
	for _, name := range want {
		if _, ok := cfg.Templates[name]; !ok {
			t.Errorf("builtin template %q missing", name)
		}
	}
	// Spec example defaults.
	if cfg.Templates["openai"].BaseURL != "https://api.openai.com/v1" {
		t.Errorf("openai base_url = %q", cfg.Templates["openai"].BaseURL)
	}
	if cfg.Templates["ollama"].BaseURL != "http://localhost:11434/v1" {
		t.Errorf("ollama base_url = %q", cfg.Templates["ollama"].BaseURL)
	}
	if cfg.Templates["ollama"].APIKeyEnv != "" {
		t.Errorf("ollama api_key_env = %q, want empty", cfg.Templates["ollama"].APIKeyEnv)
	}
	// Catalog-derived template spot checks.
	if cfg.Templates["gemini"].BaseURL != "https://generativelanguage.googleapis.com/v1beta/openai" {
		t.Errorf("gemini base_url = %q", cfg.Templates["gemini"].BaseURL)
	}
	if cfg.Templates["deepseek"].BaseURL != "https://api.deepseek.com" {
		t.Errorf("deepseek base_url = %q", cfg.Templates["deepseek"].BaseURL)
	}
	if cfg.Templates["zai"].BaseURL != "https://api.z.ai/api/paas/v4" {
		t.Errorf("zai base_url = %q", cfg.Templates["zai"].BaseURL)
	}
	if cfg.Templates["openrouter"].APIKeyEnv == "" || cfg.Templates["openai"].APIKeyEnv == "" {
		t.Error("openrouter/openai api_key_env must be non-empty")
	}
}

func TestUserTemplateOverridesBuiltin(t *testing.T) {
	cfg := mustParse(t, `
[providers.openai]
base_url = "http://example.com/v1"
models = ["custom-model"]
`)
	openai := cfg.Templates["openai"]
	if openai.BaseURL != "http://example.com/v1" {
		t.Errorf("openai base_url = %q, want override", openai.BaseURL)
	}
	if !contains(openai.Models, "custom-model") {
		t.Errorf("openai models = %v, want override to include custom-model", openai.Models)
	}
}

func TestValidationDuplicateAlias(t *testing.T) {
	_, err := Parse([]byte(`
[[instances]]
alias = "foo"
template = "openai"

[[instances]]
alias = "foo"
template = "ollama"
`))
	if err == nil || !IsValidationError(err) {
		t.Fatalf("err = %v, want ValidationError", err)
	}
	if !strings.Contains(err.Error(), "duplicate alias") {
		t.Errorf("error %q does not mention duplicate alias", err)
	}
}

func TestValidationInvalidAlias(t *testing.T) {
	for _, alias := range []string{"Bad Alias", "OpenAI", "alias!"} {
		t.Run(alias, func(t *testing.T) {
			_, err := Parse([]byte(`
[[instances]]
alias = "` + alias + `"
template = "openai"
`))
			if err == nil || !IsValidationError(err) {
				t.Fatalf("alias %q: err = %v, want ValidationError", alias, err)
			}
			if !strings.Contains(err.Error(), "must match") {
				t.Errorf("error %q does not mention alias pattern", err)
			}
		})
	}
}

func TestValidationUnknownTemplate(t *testing.T) {
	_, err := Parse([]byte(`
[[instances]]
alias = "x"
template = "no-such-provider"
`))
	if err == nil || !IsValidationError(err) {
		t.Fatalf("err = %v, want ValidationError", err)
	}
	if !strings.Contains(err.Error(), "unknown template") {
		t.Errorf("error %q does not mention unknown template", err)
	}
}

func TestValidationInvalidAPIKeyEnv(t *testing.T) {
	_, err := Parse([]byte(`
[[instances]]
alias = "x"
template = "openai"
api_key_env = "1BAD"
`))
	if err == nil || !IsValidationError(err) {
		t.Fatalf("err = %v, want ValidationError", err)
	}
	if !strings.Contains(err.Error(), "invalid api_key_env") {
		t.Errorf("error %q does not mention invalid api_key_env", err)
	}
}

func TestValidationMalformedTOML(t *testing.T) {
	_, err := Parse([]byte("listen ="))
	if err == nil {
		t.Fatal("malformed TOML returned no error")
	}
}

func TestConfigManagerSwapAndHooks(t *testing.T) {
	cfg1 := mustParse(t, specExample)
	mgr := New(cfg1)

	if mgr.Get() != cfg1 {
		t.Fatal("Get() did not return initial config")
	}

	var swapped []*Config
	mgr.OnReload(func(c *Config) { swapped = append(swapped, c) })

	cfg2 := mustParse(t, `
[[instances]]
template = "ollama"
`)
	if err := mgr.Swap(cfg2); err != nil {
		t.Fatalf("Swap: %v", err)
	}
	if mgr.Get() != cfg2 {
		t.Fatal("Get() did not return swapped config")
	}
	if len(swapped) != 1 || swapped[0] != cfg2 {
		t.Fatalf("reload hooks = %d runs, want 1 with new config", len(swapped))
	}
}

func TestConfigManagerRejectsInvalidSwap(t *testing.T) {
	cfg1 := mustParse(t, specExample)
	mgr := New(cfg1)

	// Build an invalid config directly; Parse would reject it before returning.
	invalid := &Config{
		Templates: map[string]*Template{"openai": {Name: "openai"}},
		Instances: []*Instance{{Alias: "x", Template: "no-such-provider"}},
	}
	if err := mgr.Swap(invalid); err == nil || !IsValidationError(err) {
		t.Fatalf("Swap(invalid) err = %v, want ValidationError", err)
	}
	if mgr.Get() != cfg1 {
		t.Fatal("invalid Swap replaced the live config")
	}
}

func TestConfigManagerLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gateway.toml")
	if err := os.WriteFile(path, []byte(specExample), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg1 := mustParse(t, "")
	mgr := New(cfg1)
	if err := mgr.Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := mgr.Get().Instances[0].Alias; got != "openai" {
		t.Errorf("loaded alias = %q, want openai", got)
	}
}

func TestConfigManagerLoadMissingFile(t *testing.T) {
	mgr := New(mustParse(t, ""))
	if err := mgr.Load(filepath.Join(t.TempDir(), "nope.toml")); err == nil {
		t.Fatal("Load of missing file returned no error")
	}
}

func TestEffectivePluginsGlobalDefaults(t *testing.T) {
	cfg := mustParse(t, `
[settings]
request_plugins = ["redact"]
response_plugins = ["redact"]

[[instances]]
alias = "a"
template = "openai"
`)
	req, resp := cfg.Instances[0].EffectivePlugins(cfg)
	if strings.Join(req, ",") != "redact" || strings.Join(resp, ",") != "redact" {
		t.Errorf("effective plugins = (%v, %v), want settings defaults (redact, redact)", req, resp)
	}
}

func TestEffectivePluginsPerInstanceOverride(t *testing.T) {
	// §4.5 scope: a per-instance plugins list overrides the global defaults.
	cfg := mustParse(t, `
[settings]
request_plugins = ["redact"]
response_plugins = ["redact"]

[[instances]]
alias = "a"
template = "openai"

[[instances]]
alias = "b"
template = "ollama"
plugins = []
`)
	// Instance a: unset → settings defaults.
	req, resp := cfg.Instances[0].EffectivePlugins(cfg)
	if strings.Join(req, ",") != "redact" || strings.Join(resp, ",") != "redact" {
		t.Errorf("instance a effective plugins = (%v, %v), want (redact, redact)", req, resp)
	}
	// Instance b: empty list overrides the defaults → no plugins for that
	// account.
	req, resp = cfg.Instances[1].EffectivePlugins(cfg)
	if len(req) != 0 || len(resp) != 0 {
		t.Errorf("instance b effective plugins = (%v, %v), want empty (override)", req, resp)
	}
}

func TestEffectivePluginsInstanceListAppliesToBothSides(t *testing.T) {
	cfg := mustParse(t, `
[settings]
request_plugins = []
response_plugins = []

[[instances]]
alias = "a"
template = "openai"
plugins = ["redact"]
`)
	req, resp := cfg.Instances[0].EffectivePlugins(cfg)
	if strings.Join(req, ",") != "redact" || strings.Join(resp, ",") != "redact" {
		t.Errorf("instance plugins = (%v, %v), want (redact, redact)", req, resp)
	}
}

func TestValidationUnknownPluginGlobal(t *testing.T) {
	_, err := Parse([]byte(`
[settings]
request_plugins = ["nope"]
[[instances]]
template = "openai"
`))
	if err == nil || !IsValidationError(err) {
		t.Fatalf("err = %v, want ValidationError", err)
	}
	if !strings.Contains(err.Error(), "unknown plugin") || !strings.Contains(err.Error(), "redact") {
		t.Errorf("error %q should mention the unknown plugin and available ones", err)
	}
}

func TestValidationUnknownPluginResponseDefaults(t *testing.T) {
	_, err := Parse([]byte(`
[settings]
response_plugins = ["nope"]
[[instances]]
template = "openai"
`))
	if err == nil || !IsValidationError(err) {
		t.Fatalf("err = %v, want ValidationError", err)
	}
}

func TestValidationUnknownPluginPerInstance(t *testing.T) {
	_, err := Parse([]byte(`
[[instances]]
alias = "x"
template = "openai"
plugins = ["wat"]
`))
	if err == nil || !IsValidationError(err) {
		t.Fatalf("err = %v, want ValidationError", err)
	}
	if !strings.Contains(err.Error(), `instance "x" plugins`) {
		t.Errorf("error %q should identify the instance list", err)
	}
}

func TestParsePluginsTable(t *testing.T) {
	cfg := mustParse(t, `
[[instances]]
template = "openai"

[plugins.redact]
patterns = ["TOKEN-[0-9]+"]
field_names = ["password"]
`)
	raw := cfg.PluginConfig("redact")
	if raw == nil {
		t.Fatal("PluginConfig(redact) = nil, want the [plugins.redact] table")
	}
	patterns, ok := raw["patterns"].([]any)
	if !ok || len(patterns) != 1 || patterns[0] != "TOKEN-[0-9]+" {
		t.Errorf("patterns = %#v, want [TOKEN-[0-9]+]", raw["patterns"])
	}
	fields, ok := raw["field_names"].([]any)
	if !ok || len(fields) != 1 || fields[0] != "password" {
		t.Errorf("field_names = %#v, want [password]", raw["field_names"])
	}
	// Unconfigured plugin name → nil table.
	if cfg.PluginConfig("other") != nil {
		t.Error("PluginConfig(other) should be nil")
	}
}

func TestValidationValidRedactPasses(t *testing.T) {
	cfg := mustParse(t, `
[settings]
request_plugins = ["redact"]
response_plugins = ["redact"]

[[instances]]
template = "openai"

[plugins.redact]
patterns = ["sk-[A-Za-z0-9]+"]
`)
	if len(cfg.Instances) != 1 {
		t.Fatalf("valid config should parse: %v", cfg.Instances)
	}
}

func TestResolveSkipsDisabledInstances(t *testing.T) {
	cfg := mustParse(t, `
[settings]
default_alias = "a"

[[instances]]
alias = "a"
template = "openai"

[[instances]]
alias = "b"
template = "openai"
disabled = true
`)
	// Prefixed routing to a disabled alias → unknown alias, listing only the
	// enabled alias.
	_, _, err := cfg.Resolve("b/gpt-4o")
	var uae *UnknownAliasError
	if !errors.As(err, &uae) {
		t.Fatalf("Resolve(b/gpt-4o) err = %T, want *UnknownAliasError", err)
	}
	if strings.Join(uae.Aliases, ",") != "a" {
		t.Errorf("available aliases = %v, want [a]", uae.Aliases)
	}
	// Unprefixed resolution never picks the disabled instance.
	inst, _, err := cfg.Resolve("gpt-4o")
	if err != nil {
		t.Fatalf("Resolve(gpt-4o): %v", err)
	}
	if inst.Alias != "a" {
		t.Errorf("unprefixed resolved alias = %q, want a", inst.Alias)
	}
}

func TestConfigManagerUpdatePersistsAndSwaps(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gateway.toml")
	if err := os.WriteFile(path, []byte(specExample), 0o600); err != nil {
		t.Fatal(err)
	}
	mgr := New(mustParse(t, ""))
	if err := mgr.Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Mutate a clone and persist + swap.
	c := mgr.Get().Clone()
	c.Settings.DefaultAlias = "openai-2"
	c.Instances[0].Disabled = true
	if err := mgr.Update(path, c); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got := mgr.Get().Settings.DefaultAlias; got != "openai-2" {
		t.Errorf("live default_alias = %q, want openai-2", got)
	}
	if !mgr.Get().Instances[0].Disabled {
		t.Error("live instance not disabled after Update")
	}

	// The persisted file round-trips to the same live state.
	reloaded, err := Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Settings.DefaultAlias != "openai-2" || !reloaded.Instances[0].Disabled {
		t.Errorf("reloaded config = %+v / %+v", reloaded.Settings, reloaded.Instances[0])
	}
	if len(reloaded.Instances) != 2 {
		t.Errorf("reloaded instances = %d, want 2", len(reloaded.Instances))
	}
}

func TestMarshalKeepsFileMinimalForBuiltinTemplates(t *testing.T) {
	// A config whose templates are all built-in (nothing in the file) writes
	// back with no [providers] tables: the file only carries what the API
	// changed.
	cfg := mustParse(t, `
[[instances]]
template = "openai"
`)
	data, err := Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(data), "[providers.openai]") {
		t.Errorf("Marshal wrote built-in templates back: %s", data)
	}
	if !strings.Contains(string(data), "openai") {
		t.Errorf("Marshal should still write the instance alias: %s", data)
	}
}

func TestMarshalWritesUserTemplate(t *testing.T) {
	cfg := mustParse(t, `
[providers.custom]
base_url = "https://example.com/v1"
models = ["m1"]
`)
	data, err := Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(data), "[providers.custom]") {
		t.Errorf("Marshal did not write the user template: %s", data)
	}
	// Round-trips through Parse.
	re, err := Parse(data)
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if re.Templates["custom"] == nil || re.Templates["custom"].BaseURL != "https://example.com/v1" {
		t.Errorf("re-parsed custom template = %+v", re.Templates["custom"])
	}
}

func TestMarshalDoesNotLeakNameField(t *testing.T) {
	// Template.Name is the map key, not a TOML body field: write-back must not
	// emit a `Name =` line inside [providers.<name>].
	cfg := mustParse(t, `
[providers.custom]
base_url = "https://example.com/v1"
models = ["m1"]
`)
	data, err := Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(data), "Name =") {
		t.Errorf("Marshal leaked the Name field: %s", data)
	}
}
