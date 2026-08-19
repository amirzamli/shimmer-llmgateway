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
	if inst2.Plugins == nil || len(*inst2.Plugins) != 1 || (*inst2.Plugins)[0] != "redact" {
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

func TestModelAliasesRoundTrip(t *testing.T) {
	cases := map[string]string{
		"subtable": `
[[instances]]
alias = "openai"
template = "openai"

[instances.model_aliases]
small = "gpt-4o-mini"
large = "gpt-4o"
`,
		"inline": `
[[instances]]
alias = "openai"
template = "openai"
model_aliases = { small = "gpt-4o-mini", large = "gpt-4o" }
`,
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := mustParse(t, data)
			got := cfg.Instances[0].ModelAliases
			if got["small"] != "gpt-4o-mini" || got["large"] != "gpt-4o" {
				t.Fatalf("model_aliases = %v, want {small,gpt-4o-mini large,gpt-4o}", got)
			}
			// Marshal then re-parse: the map survives the write-back.
			out, err := Marshal(cfg)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			re, err := Parse(out)
			if err != nil {
				t.Fatalf("re-parse of %s: %v", out, err)
			}
			got = re.Instances[0].ModelAliases
			if got["small"] != "gpt-4o-mini" || got["large"] != "gpt-4o" {
				t.Errorf("round-tripped model_aliases = %v, want {small,gpt-4o-mini large,gpt-4o}", got)
			}
		})
	}
}

func TestModelAliasesOmittedWhenAbsent(t *testing.T) {
	cfg := mustParse(t, `
[[instances]]
alias = "openai"
template = "openai"
`)
	out, err := Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(out), "model_aliases") {
		t.Errorf("Marshal emitted model_aliases when unset: %s", out)
	}
}

func TestValidationModelAliasBadKeys(t *testing.T) {
	for _, key := range []string{"UPPER", "with space", "bad!", ""} {
		t.Run(key, func(t *testing.T) {
			_, err := Parse([]byte(`
[[instances]]
alias = "openai"
template = "openai"
model_aliases = { "` + key + `" = "gpt-4o" }
`))
			if err == nil || !IsValidationError(err) {
				t.Fatalf("alias key %q: err = %v, want ValidationError", key, err)
			}
			if !strings.Contains(err.Error(), "must match") {
				t.Errorf("error %q does not mention the alias pattern", err)
			}
		})
	}
}

func TestValidationModelAliasEmptyValue(t *testing.T) {
	_, err := Parse([]byte(`
[[instances]]
alias = "openai"
template = "openai"
model_aliases = { small = "" }
`))
	if err == nil || !IsValidationError(err) {
		t.Fatalf("err = %v, want ValidationError", err)
	}
	if !strings.Contains(err.Error(), "empty model value") {
		t.Errorf("error %q does not mention the empty value", err)
	}
}

func TestValidationModelAliasValueMayContainSlash(t *testing.T) {
	cfg := mustParse(t, `
[[instances]]
alias = "openai"
template = "openai"
model_aliases = { local = "meta-llama/Meta-Llama-3-8B-Instruct" }
`)
	if got := cfg.Instances[0].ModelAliases["local"]; got != "meta-llama/Meta-Llama-3-8B-Instruct" {
		t.Errorf("model_aliases[local] = %q, want the slashed value", got)
	}
}

func TestResolvePrefixedAliasExpands(t *testing.T) {
	cfg := mustParse(t, `
[[instances]]
alias = "openai"
template = "openai"
model_aliases = { small = "gpt-4o-mini" }
`)
	inst, model, err := cfg.Resolve("openai/small")
	if err != nil {
		t.Fatalf("Resolve(openai/small): %v", err)
	}
	if inst.Alias != "openai" || model != "gpt-4o-mini" {
		t.Errorf("got (%q, %q), want (openai, gpt-4o-mini)", inst.Alias, model)
	}
}

func TestResolvePrefixedNoAliasMapPassthrough(t *testing.T) {
	// A prefixed model not in the instance's map passes through verbatim,
	// preserving the no-membership-check behavior.
	cfg := mustParse(t, `
[[instances]]
alias = "openai"
template = "openai"
model_aliases = { small = "gpt-4o-mini" }
`)
	inst, model, err := cfg.Resolve("openai/anything-else")
	if err != nil {
		t.Fatalf("Resolve(openai/anything-else): %v", err)
	}
	if inst.Alias != "openai" || model != "anything-else" {
		t.Errorf("got (%q, %q), want (openai, anything-else)", inst.Alias, model)
	}
}

func TestResolvePrefixedSlashyValueVerbatim(t *testing.T) {
	// The value "meta-llama/Meta-Llama-3-8B-Instruct" contains a slash; the
	// prefixed routing key names it after the first slash and expands verbatim.
	cfg := mustParse(t, `
[[instances]]
alias = "openai"
template = "openai"
model_aliases = { local = "meta-llama/Meta-Llama-3-8B-Instruct" }
`)
	inst, model, err := cfg.Resolve("openai/local")
	if err != nil {
		t.Fatalf("Resolve(openai/local): %v", err)
	}
	if inst.Alias != "openai" || model != "meta-llama/Meta-Llama-3-8B-Instruct" {
		t.Errorf("got (%q, %q), want (openai, meta-llama/...)", inst.Alias, model)
	}
}

func TestResolveUnprefixedDefaultAliasMapWins(t *testing.T) {
	cfg := mustParse(t, `
[settings]
default_alias = "openai"

[[instances]]
alias = "openai"
template = "openai"
model_aliases = { small = "gpt-4o-mini" }

[[instances]]
alias = "openai-2"
template = "openai"
model_aliases = { small = "gpt-4o" }
`)
	inst, model, err := cfg.Resolve("small")
	if err != nil {
		t.Fatalf("Resolve(small): %v", err)
	}
	if inst.Alias != "openai" || model != "gpt-4o-mini" {
		t.Errorf("got (%q, %q), want (openai, gpt-4o-mini) from default_alias map", inst.Alias, model)
	}
}

func TestResolveUnprefixedFirstEnabledInstanceMap(t *testing.T) {
	// No default_alias: the first enabled instance in config order whose map
	// contains the name wins.
	cfg := mustParse(t, `
[[instances]]
alias = "openai"
template = "openai"

[[instances]]
alias = "openai-2"
template = "openai"
model_aliases = { small = "gpt-4o-mini" }
`)
	inst, model, err := cfg.Resolve("small")
	if err != nil {
		t.Fatalf("Resolve(small): %v", err)
	}
	if inst.Alias != "openai-2" || model != "gpt-4o-mini" {
		t.Errorf("got (%q, %q), want (openai-2, gpt-4o-mini)", inst.Alias, model)
	}
}

func TestResolveUnprefixedDisabledInstanceMapSkipped(t *testing.T) {
	cfg := mustParse(t, `
[settings]
default_alias = "openai"

[[instances]]
alias = "openai"
template = "openai"

[[instances]]
alias = "openai-2"
template = "openai"
disabled = true
model_aliases = { small = "gpt-4o" }
`)
	_, _, err := cfg.Resolve("small")
	var ume *UnknownModelError
	if !errors.As(err, &ume) {
		t.Fatalf("Resolve(small) err = %T, want *UnknownModelError", err)
	}
}

func TestResolveAliasBeatsLiteralShadowing(t *testing.T) {
	// "small" is a literal model of the default_alias instance but an alias key
	// of a later instance; the alias mapping shadows the literal membership.
	cfg := mustParse(t, `
[settings]
default_alias = "openai"

[[instances]]
alias = "openai"
template = "openai"
models = ["small"]

[[instances]]
alias = "openai-2"
template = "openai"
model_aliases = { small = "gpt-4o" }
`)
	inst, model, err := cfg.Resolve("small")
	if err != nil {
		t.Fatalf("Resolve(small): %v", err)
	}
	if inst.Alias != "openai-2" || model != "gpt-4o" {
		t.Errorf("got (%q, %q), want (openai-2, gpt-4o) — alias shadows literal", inst.Alias, model)
	}
}

func TestResolveSingleLevelExpansion(t *testing.T) {
	// small → "medium" is forwarded verbatim and never re-expanded, even
	// though the same instance maps medium too.
	cfg := mustParse(t, `
[[instances]]
alias = "openai"
template = "openai"
model_aliases = { small = "medium", medium = "gpt-4o" }
`)
	for _, model := range []string{"openai/small", "small"} {
		inst, resolved, err := cfg.Resolve(model)
		if err != nil {
			t.Fatalf("Resolve(%s): %v", model, err)
		}
		if inst.Alias != "openai" || resolved != "medium" {
			t.Errorf("Resolve(%s) = (%q, %q), want (openai, medium) — single level", model, inst.Alias, resolved)
		}
	}
}

func TestResolveNoAliasMapsUnchanged(t *testing.T) {
	cfg := mustParse(t, specExample)
	inst, model, err := cfg.Resolve("gpt-4o")
	if err != nil {
		t.Fatalf("Resolve(gpt-4o): %v", err)
	}
	if inst.Alias != "openai" || model != "gpt-4o" {
		t.Errorf("got (%q, %q), want (openai, gpt-4o)", inst.Alias, model)
	}
	if _, _, err := cfg.Resolve("no-such-model"); !errors.As(err, new(*UnknownModelError)) {
		t.Errorf("Resolve(no-such-model) err = %v, want UnknownModelError", err)
	}
}

func TestAliasModelIDsOrderAndDedupe(t *testing.T) {
	cfg := mustParse(t, `
[[instances]]
alias = "openai"
template = "openai"
model_aliases = { b = "gpt-4o", a = "gpt-4o-mini" }

[[instances]]
alias = "openai-2"
template = "openai"
model_aliases = { a = "claude", small = "llama" }

[[instances]]
alias = "disabled"
template = "ollama"
disabled = true
model_aliases = { x = "y" }
`)
	got := cfg.AliasModelIDs()
	want := []string{"a", "openai/a", "b", "openai/b", "openai-2/a", "small", "openai-2/small"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("AliasModelIDs = %v, want %v", got, want)
	}
}

func TestCloneDeepCopiesModelAliases(t *testing.T) {
	cfg := mustParse(t, `
[[instances]]
alias = "openai"
template = "openai"
model_aliases = { small = "gpt-4o-mini" }
`)
	clone := cfg.Clone()
	clone.Instances[0].ModelAliases["small"] = "mutated"
	clone.Instances[0].ModelAliases["extra"] = "added"
	if got := cfg.Instances[0].ModelAliases["small"]; got != "gpt-4o-mini" {
		t.Errorf("original model_aliases mutated by clone: %v", cfg.Instances[0].ModelAliases)
	}
	if _, ok := cfg.Instances[0].ModelAliases["extra"]; ok {
		t.Error("original model_aliases gained a key from the clone")
	}
}

func TestValidateBaseURL(t *testing.T) {
	valid := []string{
		"https://api.openai.com/v1",
		"http://localhost:11434/v1", // ollama is legitimately loopback
		"http://127.0.0.1:8000/v1",  // vllm is legitimately loopback
	}
	for _, u := range valid {
		if err := ValidateBaseURL(u); err != nil {
			t.Errorf("ValidateBaseURL(%q) = %v, want nil", u, err)
		}
	}

	invalid := []string{
		"", // empty
		"file:///etc/passwd",
		"gopher://example.com",
		"ftp://example.com/v1",
		"//example.com/v1",  // scheme-less
		"example.com/v1",    // relative
		"/chat/completions", // relative
		"https:///no-host",  // no host
	}
	for _, u := range invalid {
		if err := ValidateBaseURL(u); err == nil {
			t.Errorf("ValidateBaseURL(%q) = nil, want error", u)
		}
	}
}

// TestParseRejectsNonHTTPBaseURL verifies a bad base_url fails at config load
// with a ValidationError, not at request time.
func TestParseRejectsNonHTTPBaseURL(t *testing.T) {
	for _, data := range []string{
		"[providers.bad]\nbase_url = \"file:///etc/passwd\"\n",
		"[providers.bad]\nbase_url = \"gopher://example.com\"\n",
		"[providers.bad]\nbase_url = \"example.com/v1\"\n",
		"[providers.empty]\n",
	} {
		_, err := Parse([]byte(data))
		if err == nil {
			t.Fatalf("Parse accepted config with non-http(s) base_url:\n%s", data)
		}
		var ve *ValidationError
		if !errors.As(err, &ve) {
			t.Errorf("Parse err = %T, want *ValidationError:\n%s", err, data)
		}
	}
}

// ---- retry_empty: template-level default plugins ----

func TestOpenCodeGoTemplateDefaultsRetryEmpty(t *testing.T) {
	cfg := mustParse(t, "")
	tmpl := cfg.Templates["opencode_go"]
	if tmpl == nil {
		t.Fatal("built-in opencode_go template missing")
	}
	if strings.Join(tmpl.DefaultPlugins, ",") != "retry_empty" {
		t.Errorf("opencode_go.DefaultPlugins = %v, want [retry_empty]", tmpl.DefaultPlugins)
	}
	// The other built-ins carry no template default plugins.
	if cfg.Templates["openai"].DefaultPlugins != nil {
		t.Errorf("openai.DefaultPlugins = %v, want nil", cfg.Templates["openai"].DefaultPlugins)
	}
}

func TestEffectivePluginsTwoTierResolution(t *testing.T) {
	// Two tiers: a non-nil instance plugins list (including explicit empty)
	// wins; otherwise the settings request/response plugins apply. There is no
	// template-default runtime fallback: an absent instance plugins list never
	// auto-activates from a template's DefaultPlugins seed.
	cfg := mustParse(t, `
[settings]
request_plugins = ["redact"]
response_plugins = ["redact"]

[[instances]]
alias = "a"
template = "openai"

[[instances]]
alias = "b"
template = "openai"
plugins = []

[[instances]]
alias = "c"
template = "openai"
plugins = ["retry_empty"]

[[instances]]
alias = "d"
template = "opencode_go"
`)
	for _, tc := range []struct {
		alias    string
		wantReq  string
		wantResp string
	}{
		{"a", "redact", "redact"},           // absent → settings defaults
		{"b", "", ""},                       // explicit empty → off (durable)
		{"c", "retry_empty", "retry_empty"}, // instance list → active
		{"d", "redact", "redact"},           // opencode_go absent → settings only, no seed fallback
	} {
		inst := cfg.Instances[aliasIndex(t, cfg, tc.alias)]
		req, resp := inst.EffectivePlugins(cfg)
		if strings.Join(req, ",") != tc.wantReq || strings.Join(resp, ",") != tc.wantResp {
			t.Errorf("instance %s effective plugins = (%v, %v), want (%s, %s)", tc.alias, req, resp, tc.wantReq, tc.wantResp)
		}
	}
}

func TestEffectivePluginsOpenCodeGoAbsentOffExplicitOn(t *testing.T) {
	// The built-in opencode_go template carries the ["retry_empty"] seed, but
	// the seed is a write-back materialization seed only: an instance with no
	// plugins line resolves from settings (empty by default → no retry at
	// runtime), while explicit ["retry_empty"] / [] behave as on / off.
	cfg := mustParse(t, `
[settings]
request_plugins = []
response_plugins = []

[[instances]]
alias = "go"
template = "opencode_go"

[[instances]]
alias = "go-on"
template = "opencode_go"
plugins = ["retry_empty"]

[[instances]]
alias = "go-off"
template = "opencode_go"
plugins = []
`)
	req, resp := cfg.Instances[aliasIndex(t, cfg, "go")].EffectivePlugins(cfg)
	if len(req) != 0 || len(resp) != 0 {
		t.Errorf("absent opencode_go instance effective plugins = (%v, %v), want empty (no runtime fallback)", req, resp)
	}
	req, resp = cfg.Instances[aliasIndex(t, cfg, "go-on")].EffectivePlugins(cfg)
	if strings.Join(req, ",") != "retry_empty" || strings.Join(resp, ",") != "retry_empty" {
		t.Errorf("explicit opencode_go instance effective plugins = (%v, %v), want (retry_empty, retry_empty)", req, resp)
	}
	req, resp = cfg.Instances[aliasIndex(t, cfg, "go-off")].EffectivePlugins(cfg)
	if len(req) != 0 || len(resp) != 0 {
		t.Errorf("explicit-empty opencode_go instance effective plugins = (%v, %v), want empty (durable off)", req, resp)
	}
}

func aliasIndex(t *testing.T, cfg *Config, alias string) int {
	t.Helper()
	for i, inst := range cfg.Instances {
		if inst.Alias == alias {
			return i
		}
	}
	t.Fatalf("instance %q not found", alias)
	return -1
}

func TestValidationRetryEmptyAcceptedUnknownRejected(t *testing.T) {
	// retry_empty is accepted everywhere control plugins are valid names:
	// settings defaults, template seeds, and per-instance lists.
	cfg := mustParse(t, `
[settings]
request_plugins = ["retry_empty"]
response_plugins = ["retry_empty"]

[providers.custom]
base_url = "http://example.com/v1"
plugins = ["retry_empty"]

[[instances]]
alias = "x"
template = "custom"
plugins = ["retry_empty"]
`)
	if len(cfg.Instances) != 1 {
		t.Fatalf("valid config with retry_empty should parse: %v", cfg.Instances)
	}
	// Unknown names are still rejected in a template's default list.
	_, err := Parse([]byte(`
[providers.custom]
base_url = "http://example.com/v1"
plugins = ["nope"]
`))
	if err == nil || !IsValidationError(err) {
		t.Fatalf("err = %v, want ValidationError", err)
	}
	if !strings.Contains(err.Error(), `template "custom" plugins`) {
		t.Errorf("error %q should identify the template list", err)
	}
}

func TestCloneDeepCopiesTemplateDefaultPlugins(t *testing.T) {
	cfg := mustParse(t, `
[providers.custom]
base_url = "http://example.com/v1"
plugins = ["retry_empty", "redact"]
`)
	clone := cfg.Clone()
	clone.Templates["custom"].DefaultPlugins[0] = "mutated"
	if got := cfg.Templates["custom"].DefaultPlugins[0]; got != "retry_empty" {
		t.Errorf("original DefaultPlugins mutated by clone: %v", cfg.Templates["custom"].DefaultPlugins)
	}
}

func TestClonePreservesNilVsEmptyPlugins(t *testing.T) {
	// The `plugins = []` toggle-off must survive an in-process config swap:
	// Clone keeps an explicitly-empty (non-nil) list non-nil and keeps an
	// unset (nil) list nil, for both template defaults and instances.
	cfg := mustParse(t, `
[providers.empty]
base_url = "http://example.com/v1"
models = ["m1"]
plugins = []

[providers.unset]
base_url = "http://example.com/v2"
models = ["m2"]

[[instances]]
alias = "a"
template = "empty"
plugins = []

[[instances]]
alias = "b"
template = "unset"

[[instances]]
alias = "c"
template = "unset"
plugins = ["retry_empty"]
`)
	cl := cfg.Clone()
	if cl.Templates["empty"].DefaultPlugins == nil || len(cl.Templates["empty"].DefaultPlugins) != 0 {
		t.Errorf("template plugins = [] after Clone = %#v, want non-nil empty", cl.Templates["empty"].DefaultPlugins)
	}
	if cl.Templates["unset"].DefaultPlugins != nil {
		t.Errorf("template with no plugins after Clone = %#v, want nil", cl.Templates["unset"].DefaultPlugins)
	}
	a := cl.Instances[aliasIndex(t, cl, "a")]
	if a.Plugins == nil || len(*a.Plugins) != 0 {
		t.Errorf("instance plugins = [] after Clone = %#v, want non-nil empty", a.Plugins)
	}
	b := cl.Instances[aliasIndex(t, cl, "b")]
	if b.Plugins != nil {
		t.Errorf("instance with no plugins after Clone = %#v, want nil", b.Plugins)
	}
	c := cl.Instances[aliasIndex(t, cl, "c")]
	if c.Plugins == nil || strings.Join(*c.Plugins, ",") != "retry_empty" {
		t.Errorf("instance plugins = [retry_empty] after Clone = %#v, want non-nil [retry_empty]", c.Plugins)
	}
	// The clone's pointers are deep copies: mutating one never touches the
	// other (or the original).
	cl.Instances[aliasIndex(t, cl, "c")].Plugins = nil
	if cfg.Instances[aliasIndex(t, cfg, "c")].Plugins == nil {
		t.Error("original instance plugins mutated by clone")
	}
	// The toggle-off semantics survive the clone: the cloned empty list still
	// resolves to an explicit (non-nil) empty chain.
	req, resp := a.EffectivePlugins(cl)
	if req == nil || len(req) != 0 || resp == nil || len(resp) != 0 {
		t.Errorf("effective plugins after Clone of plugins = [] = (%v, %v), want (non-nil empty, non-nil empty)", req, resp)
	}
}

func TestMarshalRoundTripsTemplatePlugins(t *testing.T) {
	cfg := mustParse(t, `
[providers.custom]
base_url = "http://example.com/v1"
models = ["m1"]
plugins = ["retry_empty", "redact"]
`)
	out, err := Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(out), "retry_empty") {
		t.Errorf("Marshal did not write the template plugins list: %s", out)
	}
	re, err := Parse(out)
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if got := strings.Join(re.Templates["custom"].DefaultPlugins, ","); got != "retry_empty,redact" {
		t.Errorf("round-tripped DefaultPlugins = %v, want [retry_empty redact]", re.Templates["custom"].DefaultPlugins)
	}
}

func TestToRawMaterializesTemplateDefaultPlugins(t *testing.T) {
	// An opencode_go instance with no plugins line gets the built-in seed
	// materialized as its own plugins entry on write-back, so the file records
	// plugins = ["retry_empty"] explicitly (default-on is visible in the
	// file). The live config is left untouched — the seed applies only to the
	// persisted form.
	cfg := mustParse(t, `
[[instances]]
alias = "go"
template = "opencode_go"
`)
	raw := cfg.toRaw()
	if raw.Instances[0].Plugins == nil {
		t.Fatal("toRaw did not materialize the seed onto the unset opencode_go instance")
	}
	if strings.Join(*raw.Instances[0].Plugins, ",") != "retry_empty" {
		t.Errorf("materialized instance plugins = %v, want [retry_empty]", *raw.Instances[0].Plugins)
	}
	if cfg.Instances[0].Plugins != nil {
		t.Error("toRaw mutated the live config")
	}
	out, err := Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(out), "retry_empty") {
		t.Errorf("Marshal output does not record the materialized default: %s", out)
	}
	// A reload of the written file sees the default explicitly active.
	re, err := Parse(out)
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	inst := re.Instances[aliasIndex(t, re, "go")]
	if inst.Plugins == nil || strings.Join(*inst.Plugins, ",") != "retry_empty" {
		t.Errorf("reloaded instance plugins = %#v, want explicit [retry_empty]", inst.Plugins)
	}
}

func TestToRawDoesNotOverwriteExplicitEmptyPlugins(t *testing.T) {
	// An explicit plugins = [] is a durable off-switch: write-back keeps it
	// empty and never materializes the template seed onto it.
	cfg := mustParse(t, `
[[instances]]
alias = "go"
template = "opencode_go"
plugins = []
`)
	raw := cfg.toRaw()
	if raw.Instances[0].Plugins == nil || len(*raw.Instances[0].Plugins) != 0 {
		t.Errorf("explicit empty plugins after toRaw = %#v, want non-nil empty", raw.Instances[0].Plugins)
	}
	out, err := Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(out), "plugins = []") {
		t.Errorf("Marshal did not preserve the explicit empty off-switch: %s", out)
	}
	re, err := Parse(out)
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	inst := re.Instances[aliasIndex(t, re, "go")]
	if inst.Plugins == nil || len(*inst.Plugins) != 0 {
		t.Errorf("round-tripped explicit empty plugins = %#v, want non-nil empty", inst.Plugins)
	}
}

func TestMarshalParseRoundTripPreservesNilVsEmptyInstancePlugins(t *testing.T) {
	// The pointer field distinguishes "absent" (nil) from "explicit empty"
	// (non-nil pointer to []) through a full Marshal → Parse round trip. The
	// openai template has no DefaultPlugins seed, so toRaw leaves every
	// instance's pointer state exactly as parsed.
	cfg := mustParse(t, `
[[instances]]
alias = "a"
template = "openai"

[[instances]]
alias = "b"
template = "openai"
plugins = []

[[instances]]
alias = "c"
template = "openai"
plugins = ["retry_empty"]
`)
	out, err := Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	re, err := Parse(out)
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	a := re.Instances[aliasIndex(t, re, "a")]
	if a.Plugins != nil {
		t.Errorf("absent instance plugins after round trip = %#v, want nil", a.Plugins)
	}
	b := re.Instances[aliasIndex(t, re, "b")]
	if b.Plugins == nil || len(*b.Plugins) != 0 {
		t.Errorf("explicit empty instance plugins after round trip = %#v, want non-nil empty", b.Plugins)
	}
	c := re.Instances[aliasIndex(t, re, "c")]
	if c.Plugins == nil || strings.Join(*c.Plugins, ",") != "retry_empty" {
		t.Errorf("non-empty instance plugins after round trip = %#v, want [retry_empty]", c.Plugins)
	}
}
