package config

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
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

func TestInstanceCaptureOptionRoundTrip(t *testing.T) {
	cfg := mustParse(t, `
[providers.openai]
base_url = "https://api.openai.com/v1"
models = ["gpt-4o"]

[[instances]]
alias = "proxy"
template = "openai"
capture = false
`)
	if cfg.Instances[0].Capture == nil || *cfg.Instances[0].Capture {
		t.Fatalf("capture = %v, want explicit false", cfg.Instances[0].Capture)
	}
	out, err := Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(out), "capture = false") {
		t.Fatalf("marshaled config lost capture=false:\n%s", out)
	}
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

	got := cfg.Addrs()
	if len(got) != 1 || got[0] != "127.0.0.1:8787" {
		t.Errorf("listen_addrs = %v, want [127.0.0.1:8787]", got)
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

func TestParseListenAddrs(t *testing.T) {
	// Multiple addresses: localhost and a Tailscale CGNAT address.
	cfg := mustParse(t, `
listen_addrs = ["127.0.0.1:8787", "100.64.0.1:8787"]
store = "gateway.db"
`)
	got := cfg.Addrs()
	if len(got) != 2 || got[0] != "127.0.0.1:8787" || got[1] != "100.64.0.1:8787" {
		t.Errorf("listen_addrs = %v, want [127.0.0.1:8787 100.64.0.1:8787]", got)
	}

	// The legacy single listen key still parses into the list.
	legacy := mustParse(t, `
listen = "127.0.0.1:8787"
`)
	got = legacy.Addrs()
	if len(got) != 1 || got[0] != "127.0.0.1:8787" {
		t.Errorf("legacy listen = %v, want [127.0.0.1:8787]", got)
	}

	// Neither key defaults to the loopback address.
	none := mustParse(t, `
store = "gateway.db"
`)
	got = none.Addrs()
	if len(got) != 1 || got[0] != "127.0.0.1:8787" {
		t.Errorf("default listen = %v, want [127.0.0.1:8787]", got)
	}

	// Both keys at once is a config error.
	if _, err := Parse([]byte("listen = \"127.0.0.1:8787\"\nlisten_addrs = [\"127.0.0.1:8788\"]\n")); err == nil {
		t.Error("Parse with both listen and listen_addrs = nil, want error")
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
		"opencode_zen", "opencode_go", "commandcode", "chatgpt",
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
	// OpenCode Go enforces a per-conversation session header upstream; the
	// built-in template declares it so the forward path synthesises one.
	if got := cfg.Templates["opencode_go"].SessionHeader; got != "x-opencode-session" {
		t.Errorf("opencode_go session_header = %q, want x-opencode-session", got)
	}
	// The ChatGPT Plus template is OAuth-authenticated and pinned to the
	// verified OpenCode v1.18.30 upstream contract: the codex Responses
	// endpoint and the session-id conversation header. The codex endpoint has
	// no stable OpenAI /models contract, so the built-in list is used directly
	// instead of live discovery.
	chatgpt := cfg.Templates["chatgpt"]
	if !chatgpt.OAuth {
		t.Error("chatgpt oauth = false, want true")
	}
	if chatgpt.BaseURL != "https://chatgpt.com/backend-api/codex" {
		t.Errorf("chatgpt base_url = %q, want the verified codex base", chatgpt.BaseURL)
	}
	if chatgpt.Style != StyleResponses {
		t.Errorf("chatgpt style = %q, want %q (the codex endpoint speaks Responses)", chatgpt.Style, StyleResponses)
	}
	if chatgpt.APIKeyEnv != "" {
		t.Errorf("chatgpt api_key_env = %q, want empty (OAuth accounts have no API key)", chatgpt.APIKeyEnv)
	}
	if got := chatgpt.SessionHeader; got != "session-id" {
		t.Errorf("chatgpt session_header = %q, want session-id", got)
	}
	if !slices.Equal(chatgpt.Models, []string{"gpt-5.2-codex", "gpt-5.3-codex", "gpt-5.6", "gpt-5.6-luna", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-6-astra"}) {
		t.Errorf("chatgpt models = %v, want the built-in Codex model list", chatgpt.Models)
	}
	if got := chatgpt.ModelReasoningOptions["gpt-5.3-codex"]; !slices.Equal(got, []string{"low", "medium", "high", "xhigh"}) {
		t.Errorf("chatgpt gpt-5.3-codex reasoning options = %v, want [low medium high xhigh]", got)
	}
	if got := chatgpt.ModelReasoningOptions["gpt-5.6-sol"]; !slices.Equal(got, []string{"none", "low", "medium", "high", "xhigh", "max"}) {
		t.Errorf("chatgpt gpt-5.6-sol reasoning options = %v, want [none low medium high xhigh max]", got)
	}
	// Ordinary templates are not OAuth: the marker must be opt-in only.
	if cfg.Templates["openai"].OAuth || cfg.Templates["opencode_go"].OAuth {
		t.Error("an API-key template has oauth = true")
	}
	// OpenCode Go serves three protocols on one base URL: the template default
	// is the OpenAI-compatible chat style (/chat/completions), model_styles
	// routes Grok/GPT/Muse to /responses, and MiniMax/Qwen to /messages.
	if got := cfg.Templates["opencode_go"].Style; got != "" {
		t.Errorf("opencode_go style = %q, want the empty openai default", got)
	}
	ms := cfg.Templates["opencode_go"].ModelStyles
	if len(ms) != 13 {
		t.Errorf("opencode_go model_styles = %d entries, want 13", len(ms))
	}
	for model, want := range map[string]string{
		"grok-4.7":                   StyleResponses,
		"grok-4.6":                   StyleResponses,
		"gpt-5.6-luna":               StyleResponses,
		"muse-spark-1.3-contributor": StyleResponses,
		"minimax-m3":                 StyleAnthropic,
		"qwen3.6-plus":               StyleAnthropic,
	} {
		if got := ms[model]; got != want {
			t.Errorf("opencode_go model_styles[%q] = %q, want %q", model, got, want)
		}
	}
	if got := cfg.Templates["opencode_go"].ModelReasoningOptions["mimo-v2.6-pro"]; got == nil {
		t.Fatal("opencode_go MiMo metadata must explicitly record no documented effort values")
	}
	if len(cfg.Templates["opencode_go"].ModelReasoningOptions["mimo-v2.6-pro"]) != 0 {
		t.Fatal("opencode_go MiMo metadata unexpectedly advertises effort values")
	}
	// GLM, Kimi, DeepSeek, MiMo, Hy4, and Hy3 resolve to the template's
	// chat-completions default.
	if _, ok := ms["glm-5.3"]; ok {
		t.Errorf("opencode_go model_styles should not list glm-5.3 (chat default), got %q", ms["glm-5.3"])
	}
	// The built-in models list mirrors the current Go endpoint catalog exactly.
	models := cfg.Templates["opencode_go"].Models
	wantModels := []string{
		"grok-4.7", "grok-4.6", "gpt-5.6-luna",
		"glm-5.3-flash", "glm-5.3", "glm-5.2", "glm-5.1",
		"kimi-k3", "kimi-k2.7-code", "kimi-k2.6", "longcat-2.0",
		"deepseek-v4.1-flash", "deepseek-v4-pro", "deepseek-v4-flash",
		"deepseek-v4-flash-vision-exp",
		"mimo-v2.6-flash", "mimo-v2.6-pro", "mimo-v2.5", "mimo-v2.5-pro",
		"minimax-m3", "minimax-m2.7", "minimax-m2.5",
		"muse-spark-1.3-contributor", "muse-spark-1.2-contributor",
		"qwen3.8-max", "qwen3.8-flash", "qwen3.7-max", "qwen3.7-plus", "qwen3.6-plus",
		"hy4-preview", "hy3",
	}
	if !slices.Equal(models, wantModels) {
		t.Errorf("opencode_go models = %v, want %v", models, wantModels)
	}
	// OpenCode Go also expects coding-agent identity headers on every call;
	// the built-in template declares the same values the real opencode client
	// sends (client-supplied values win over these defaults).
	wantIdentity := map[string]string{"X-Opencode-Client": "cli", "X-Opencode-Project": "global"}
	for k, v := range wantIdentity {
		if got := cfg.Templates["opencode_go"].IdentityHeaders[k]; got != v {
			t.Errorf("opencode_go identity header %q = %q, want %q", k, got, v)
		}
	}
	if len(cfg.Templates["opencode_go"].IdentityHeaders) != len(wantIdentity) {
		t.Errorf("opencode_go identity_headers = %v, want exactly %v", cfg.Templates["opencode_go"].IdentityHeaders, wantIdentity)
	}
	if cfg.Templates["zai"].BaseURL != "https://api.z.ai/api/paas/v4" {
		t.Errorf("zai base_url = %q", cfg.Templates["zai"].BaseURL)
	}
	if cfg.Templates["openrouter"].APIKeyEnv == "" || cfg.Templates["openai"].APIKeyEnv == "" {
		t.Error("openrouter/openai api_key_env must be non-empty")
	}
	if cfg.Templates["commandcode"].BaseURL != "https://api.commandcode.ai/provider/v1" {
		t.Errorf("commandcode base_url = %q", cfg.Templates["commandcode"].BaseURL)
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
	for _, alias := range []string{"openai!", "open/ai", " openai", "openai ", "open  ai", "open\tai"} {
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

func TestValidationAliasUppercaseAndSpaces(t *testing.T) {
	// Upper case and interior single spaces are legal (resolved review:
	// the old lowercase-only rule was a conservatism, not a requirement);
	// matching itself stays exact and case-sensitive.
	cfg := mustParse(t, `
[[instances]]
alias = "My Provider"
template = "openai"

[instances.model_aliases]
"Small Key" = "gpt-4o-mini"
`)
	inst, model, err := cfg.Resolve("My Provider/Small Key")
	if err != nil {
		t.Fatalf("Resolve prefixed: %v", err)
	}
	if inst.Alias != "My Provider" || model != "gpt-4o-mini" {
		t.Fatalf("Resolve = %q/%q, want My Provider/gpt-4o-mini", inst.Alias, model)
	}
	// The alias round-trips through the write-back (quoted TOML key/value).
	out, err := Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	re, err := Parse(out)
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if re.Instances[0].Alias != "My Provider" {
		t.Errorf("round-tripped alias = %q, want My Provider", re.Instances[0].Alias)
	}
	// Case-sensitive: a different case is an unknown alias, and two aliases
	// differing only in case are distinct (no duplicate-alias error).
	if _, _, err := cfg.Resolve("my provider/small"); err == nil {
		t.Errorf("Resolve with wrong case succeeded, want unknown alias error")
	}
	cfg = mustParse(t, `
[[instances]]
alias = "OpenAI"
template = "openai"

[[instances]]
alias = "openai"
template = "ollama"
`)
	if cfg.Instances[0].Alias != "OpenAI" || cfg.Instances[1].Alias != "openai" {
		t.Errorf("case-distinguished aliases collapsed: %q, %q", cfg.Instances[0].Alias, cfg.Instances[1].Alias)
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

func TestValidationIdentityHeaders(t *testing.T) {
	// Identity headers become literal upstream headers, so empty names and
	// CR/LF in a name or value (header-smuggling vectors) fail at load.
	bad := []string{
		"[providers.opencode_go]\nbase_url = \"https://opencode.ai/zen/go/v1\"\nmodels = [\"m\"]\nidentity_headers = { \"\" = \"cli\" }\n",
		"[providers.opencode_go]\nbase_url = \"https://opencode.ai/zen/go/v1\"\nmodels = [\"m\"]\nidentity_headers = { \"X-Opencode-Client\" = \"cli\\nInjected: 1\" }\n",
		"[providers.opencode_go]\nbase_url = \"https://opencode.ai/zen/go/v1\"\nmodels = [\"m\"]\nidentity_headers = { \"X-Opencode-Client\\r\\nX-Evil: 1\" = \"cli\" }\n",
	}
	for _, raw := range bad {
		_, err := Parse([]byte(raw))
		if err == nil || !IsValidationError(err) {
			t.Errorf("identity_headers %q: err = %v, want ValidationError", raw, err)
		}
	}
	// A clean declaration still parses.
	if _, err := Parse([]byte("[providers.opencode_go]\nbase_url = \"https://opencode.ai/zen/go/v1\"\nmodels = [\"m\"]\nidentity_headers = { \"X-Opencode-Client\" = \"cli\" }\n")); err != nil {
		t.Errorf("clean identity_headers rejected: %v", err)
	}
}

func TestValidationModelStyles(t *testing.T) {
	// model_styles values reuse the style whitelist; an empty key, an empty
	// value, or an unknown value fails at load with a message naming the
	// template and the offending model.
	bad := []struct {
		name string
		toml string
	}{
		{"bad value", "[providers.x]\nbase_url = \"https://x\"\nmodels = [\"m\"]\nmodel_styles = { m = \"grpc\" }\n"},
		{"empty key", "[providers.x]\nbase_url = \"https://x\"\nmodels = [\"m\"]\nmodel_styles = { \"\" = \"openai\" }\n"},
		{"empty value", "[providers.x]\nbase_url = \"https://x\"\nmodels = [\"m\"]\nmodel_styles = { m = \"\" }\n"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.toml))
			if err == nil || !IsValidationError(err) {
				t.Fatalf("%s: err = %v, want ValidationError", tc.name, err)
			}
			if !strings.Contains(err.Error(), "model_styles") {
				t.Errorf("error %q does not mention model_styles", err)
			}
		})
	}
	// A clean map parses and survives the write-back round trip.
	cfg := mustParse(t, "[providers.x]\nbase_url = \"https://x\"\nmodels = [\"m\", \"n\"]\nmodel_styles = { m = \"responses\", n = \"anthropic\" }\n")
	if got := cfg.Templates["x"].ModelStyles["m"]; got != StyleResponses {
		t.Errorf("model_styles[m] = %q, want responses", got)
	}
	if got := cfg.Templates["x"].ModelStyles["n"]; got != StyleAnthropic {
		t.Errorf("model_styles[n] = %q, want anthropic", got)
	}
	out, err := Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	re, err := Parse(out)
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if got := re.Templates["x"].ModelStyles["m"]; got != StyleResponses {
		t.Errorf("round-tripped model_styles[m] = %q, want responses", got)
	}
}

func TestValidationUITheme(t *testing.T) {
	// Absent and both known values parse; anything else is a config error so
	// a typo fails at load instead of leaving the UI theme undefined.
	for _, theme := range []string{"", "dark", "light"} {
		cfg, err := Parse([]byte("[settings]\nui_theme = \"" + theme + "\"\n"))
		if err != nil {
			t.Fatalf("ui_theme %q: err = %v", theme, err)
		}
		if cfg.Settings.UITheme != theme {
			t.Errorf("ui_theme = %q, want %q", cfg.Settings.UITheme, theme)
		}
	}
	_, err := Parse([]byte("[settings]\nui_theme = \"blue\"\n"))
	if err == nil || !IsValidationError(err) {
		t.Fatalf("err = %v, want ValidationError", err)
	}
	if !strings.Contains(err.Error(), "ui_theme") {
		t.Errorf("error %q does not mention ui_theme", err)
	}
}

func TestUIThemeRoundTrip(t *testing.T) {
	cfg := mustParse(t, "[settings]\nui_theme = \"light\"\n")
	out, err := Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(out), `ui_theme = 'light'`) {
		t.Errorf("write-back %q does not carry ui_theme", out)
	}
	re, err := Parse(out)
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if re.Settings.UITheme != "light" {
		t.Errorf("round-tripped ui_theme = %q, want light", re.Settings.UITheme)
	}
	// An unset theme is omitted from the write-back (dark is the default look).
	cfg = mustParse(t, "")
	out, err = Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(out), "ui_theme") {
		t.Errorf("unset ui_theme leaked into write-back: %q", out)
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

func TestOAuthTemplateFieldRoundTrip(t *testing.T) {
	cfg := mustParse(t, `
[providers.oauth_custom]
base_url = "https://chatgpt.com/backend-api/codex"
style = "responses"
oauth = true
session_header = "session-id"
models = ["gpt-5.3-codex"]
`)
	tmpl := cfg.Templates["oauth_custom"]
	if !tmpl.OAuth {
		t.Fatal("oauth = false after parse, want true")
	}
	data, err := Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(data), "oauth = true") {
		t.Errorf("Marshal dropped the oauth marker: %s", data)
	}
	re, err := Parse(data)
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if !re.Templates["oauth_custom"].OAuth {
		t.Error("oauth marker lost in the marshal/parse round trip")
	}
	// The marker stays opt-in: an API-key template never serializes it.
	plain := mustParse(t, `
[providers.custom]
base_url = "https://example.com/v1"
models = ["m1"]
`)
	plainData, err := Marshal(plain)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plainData), "oauth") {
		t.Errorf("Marshal emitted oauth for an API-key template: %s", plainData)
	}
}

func TestOAuthTemplateEndpointIsFixed(t *testing.T) {
	for _, baseURL := range []string{"https://example.com/codex", "http://127.0.0.1:9000/codex"} {
		_, err := Parse([]byte("[providers.oauth_custom]\nbase_url = \"" + baseURL + "\"\nstyle = \"responses\"\noauth = true\n"))
		if err == nil {
			t.Errorf("OAuth template base_url %q was accepted, want fixed endpoint validation error", baseURL)
		}
	}
	if _, err := Parse([]byte("[providers.oauth_custom]\nbase_url = \"" + ChatGPTCodexBaseURL + "\"\noauth = true\n")); err == nil {
		t.Error("OAuth template without responses style was accepted, want fixed endpoint validation error")
	}
	if _, err := Parse([]byte("[providers.oauth_custom]\nbase_url = \"" + ChatGPTCodexBaseURL + "\"\nstyle = \"responses\"\noauth = true\n")); err == nil {
		t.Error("OAuth template without session_header was accepted, want fixed contract validation error")
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
	for _, key := range []string{"bad!", "bad/key", " leading", "trailing ", "double  space", ""} {
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

func TestResolveUnprefixedPriorityOverridesFileOrder(t *testing.T) {
	// The first instance in file order maps small but carries a higher
	// (worse) priority; the later instance with priority 1 must win.
	cfg := mustParse(t, `
[[instances]]
alias = "first"
template = "openai"
priority = 2
model_aliases = { small = "gpt-4o" }

[[instances]]
alias = "second"
template = "openai"
priority = 1
model_aliases = { small = "gpt-4o-mini" }
`)
	inst, model, err := cfg.Resolve("small")
	if err != nil {
		t.Fatalf("Resolve(small): %v", err)
	}
	if inst.Alias != "second" || model != "gpt-4o-mini" {
		t.Errorf("got (%q, %q), want (second, gpt-4o-mini) — priority 1 beats file order", inst.Alias, model)
	}
}

func TestResolvePriorityUnsetComesAfterPrioritized(t *testing.T) {
	// An instance with no explicit priority keeps file order among unset
	// instances, but any prioritized instance beats all of them.
	cfg := mustParse(t, `
[[instances]]
alias = "unset-a"
template = "openai"
model_aliases = { small = "gpt-4o" }

[[instances]]
alias = "prio"
template = "openai"
priority = 5
model_aliases = { small = "gpt-4o-mini" }

[[instances]]
alias = "unset-b"
template = "openai"
model_aliases = { small = "gpt-4o-turbo" }
`)
	inst, model, err := cfg.Resolve("small")
	if err != nil {
		t.Fatalf("Resolve(small): %v", err)
	}
	if inst.Alias != "prio" || model != "gpt-4o-mini" {
		t.Errorf("got (%q, %q), want (prio, gpt-4o-mini) — explicit priority beats unset instances", inst.Alias, model)
	}
}

func TestResolvePriorityUnsetKeepsFileOrderAmongItself(t *testing.T) {
	cfg := mustParse(t, `
[[instances]]
alias = "unset-a"
template = "openai"
model_aliases = { small = "gpt-4o" }

[[instances]]
alias = "unset-b"
template = "openai"
model_aliases = { small = "gpt-4o-mini" }
`)
	inst, model, err := cfg.Resolve("small")
	if err != nil {
		t.Fatalf("Resolve(small): %v", err)
	}
	if inst.Alias != "unset-a" || model != "gpt-4o" {
		t.Errorf("got (%q, %q), want (unset-a, gpt-4o) — file order among unset instances", inst.Alias, model)
	}
}

func TestAliasModelIDsPriorityOrder(t *testing.T) {
	cfg := mustParse(t, `
[[instances]]
alias = "low"
template = "openai"
priority = 2
model_aliases = { small = "gpt-4o" }

[[instances]]
alias = "high"
template = "openai"
priority = 1
model_aliases = { small = "gpt-4o-mini", tiny = "gpt-4o" }
`)
	ids := cfg.AliasModelIDs()
	want := []string{"small", "high/small", "tiny", "high/tiny", "low/small"}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Errorf("AliasModelIDs = %v, want %v (priority-ordered: high before low)", ids, want)
	}
}

func TestValidationNegativePriority(t *testing.T) {
	_, err := Parse([]byte(`
[[instances]]
alias = "x"
template = "openai"
priority = -1
`))
	if err == nil || !IsValidationError(err) {
		t.Fatalf("err = %v, want ValidationError", err)
	}
	if !strings.Contains(err.Error(), "priority") {
		t.Errorf("error %q does not mention priority", err)
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
model_reasoning = { small = "high" }
`)
	clone := cfg.Clone()
	clone.Instances[0].ModelAliases["small"] = "mutated"
	clone.Instances[0].ModelAliases["extra"] = "added"
	clone.Instances[0].ModelReasoning["small"] = "low"
	clone.Instances[0].ModelReasoning["extra"] = "none"
	if got := cfg.Instances[0].ModelAliases["small"]; got != "gpt-4o-mini" {
		t.Errorf("original model_aliases mutated by clone: %v", cfg.Instances[0].ModelAliases)
	}
	if _, ok := cfg.Instances[0].ModelAliases["extra"]; ok {
		t.Error("original model_aliases gained a key from the clone")
	}
	if got := cfg.Instances[0].ModelReasoning["small"]; got != "high" {
		t.Errorf("original model_reasoning mutated by clone: %v", cfg.Instances[0].ModelReasoning)
	}
	if _, ok := cfg.Instances[0].ModelReasoning["extra"]; ok {
		t.Error("original model_reasoning gained a key from the clone")
	}
}

func TestModelReasoningValidationAndEffective(t *testing.T) {
	valid := `
[[instances]]
alias = "openai"
template = "openai"
model_aliases = { small = "gpt-4o-mini", med = "gpt-4o" }
model_reasoning = { small = "high", med = "default" }
`
	cfg := mustParse(t, valid)
	inst := cfg.Instances[0]
	if got := inst.EffectiveReasoning("small"); got != "high" {
		t.Errorf("EffectiveReasoning(small) = %q, want high", got)
	}
	if got := inst.EffectiveReasoning("med"); got != "" {
		t.Errorf("EffectiveReasoning(med) = %q, want empty (default)", got)
	}
	if got := inst.EffectiveReasoning("unknown"); got != "" {
		t.Errorf("EffectiveReasoning(unknown) = %q, want empty", got)
	}

	invalidEffort := `
[[instances]]
alias = "openai"
template = "openai"
model_reasoning = { small = "extreme" }
`
	if _, err := Parse([]byte(invalidEffort)); err == nil {
		t.Fatal("Parse accepted invalid model reasoning level 'extreme'")
	}

	invalidKey := `
[[instances]]
alias = "openai"
template = "openai"
model_reasoning = { "invalid key!" = "high" }
`
	if _, err := Parse([]byte(invalidKey)); err == nil {
		t.Fatal("Parse accepted invalid model reasoning key")
	}
}

func TestModelReasoningGatedByAdvertisedOptions(t *testing.T) {
	// The deepseek template advertises [high max] for deepseek-v4-pro: max is
	// accepted, medium (valid on unadvertised models) is rejected with the
	// advertised list in the error.
	accepted := `
[[instances]]
alias = "ds"
template = "deepseek"
model_aliases = { big = "deepseek-v4-pro" }
model_reasoning = { big = "max" }
`
	cfg := mustParse(t, accepted)
	if got := cfg.Instances[0].EffectiveReasoning("big"); got != "max" {
		t.Errorf("EffectiveReasoning(big) = %q, want max", got)
	}

	rejected := `
[[instances]]
alias = "ds"
template = "deepseek"
model_aliases = { big = "deepseek-v4-pro" }
model_reasoning = { big = "medium" }
`
	_, err := Parse([]byte(rejected))
	if err == nil {
		t.Fatal("Parse accepted 'medium' for deepseek-v4-pro, which advertises only high/max")
	}
	if !strings.Contains(err.Error(), "high, max") {
		t.Errorf("error %v should list the advertised levels", err)
	}

	// An alias whose model has no advertised options falls back to the
	// generic vocabulary when metadata is genuinely unknown: medium passes,
	// extreme still fails.
	generic := `
[[instances]]
alias = "ds"
template = "deepseek"
model_aliases = { chat = "deepseek-chat" }
model_reasoning = { chat = "medium" }
`
	if _, err := Parse([]byte(generic)); err != nil {
		t.Errorf("Parse rejected generic level for unadvertised model: %v", err)
	}
	extreme := `
[[instances]]
alias = "ds"
template = "deepseek"
model_aliases = { chat = "deepseek-chat" }
model_reasoning = { chat = "extreme" }
`
	if _, err := Parse([]byte(extreme)); err == nil {
		t.Fatal("Parse accepted invalid model reasoning level 'extreme' via fallback")
	}

	// An explicit empty capability list is not the same as unknown metadata:
	// MiMo models must reject configured effort values rather than using the
	// generic fallback.
	mimoInvalid := `
[[instances]]
alias = "go"
template = "opencode_go"
model_aliases = { large = "mimo-v2.6-pro" }
model_reasoning = { large = "max" }
`
	if _, err := Parse([]byte(mimoInvalid)); err == nil {
		t.Fatal("Parse accepted reasoning effort for MiMo without documented effort values")
	}

	// OpenAI model metadata is also model-specific: gpt-6-astra does not
	// advertise none, while the GPT-5.6 family does.
	chatGPTNone := `
[[instances]]
alias = "chatgpt"
template = "chatgpt"
model_aliases = { astra = "gpt-6-astra" }
model_reasoning = { astra = "none" }
`
	if _, err := Parse([]byte(chatGPTNone)); err == nil {
		t.Fatal("Parse accepted 'none' for gpt-6-astra, which does not advertise it")
	}
	chatGPTDefault := `
[[instances]]
alias = "chatgpt"
template = "chatgpt"
model_aliases = { sol = "gpt-5.6-sol" }
model_reasoning = { sol = "none" }
`
	if _, err := Parse([]byte(chatGPTDefault)); err != nil {
		t.Errorf("Parse rejected advertised 'none' for gpt-5.6-sol: %v", err)
	}
}

func TestTemplateModelReasoningOptionsCloneAndUserGating(t *testing.T) {
	user := `
[providers.custom]
base_url = "https://example.com/v1"
models = ["m1"]
model_reasoning_options = { m1 = ["low", "max"] }

[[instances]]
alias = "c"
template = "custom"
model_aliases = { a = "m1" }
model_reasoning = { a = "low" }
`
	cfg := mustParse(t, user)

	// Clone deep-copies the options map: mutating the clone must not touch
	// the original.
	clone := cfg.Clone()
	clone.Templates["custom"].ModelReasoningOptions["m1"][0] = "mutated"
	if got := cfg.Templates["custom"].ModelReasoningOptions["m1"][0]; got != "low" {
		t.Errorf("original template options mutated by clone: %q", got)
	}

	// A user-defined template's advertised options gate instance values too.
	maxCfg := `
[providers.custom]
base_url = "https://example.com/v1"
models = ["m1"]
model_reasoning_options = { m1 = ["low", "max"] }

[[instances]]
alias = "c"
template = "custom"
model_aliases = { a = "m1" }
model_reasoning = { a = "max" }
`
	if _, err := Parse([]byte(maxCfg)); err != nil {
		t.Errorf("Parse rejected advertised value 'max': %v", err)
	}
	badCfg := `
[providers.custom]
base_url = "https://example.com/v1"
models = ["m1"]
model_reasoning_options = { m1 = ["low", "max"] }

[[instances]]
alias = "c"
template = "custom"
model_aliases = { a = "m1" }
model_reasoning = { a = "high" }
`
	if _, err := Parse([]byte(badCfg)); err == nil {
		t.Fatal("Parse accepted 'high' although user template advertises only low/max")
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

// ---- template-level default plugins ----

func TestNoBuiltinTemplateDefaultPlugins(t *testing.T) {
	// No built-in template seeds a default plugins list; per-provider plugins
	// are opt-in via the instance list or custom-template seeds.
	cfg := mustParse(t, "")
	for _, name := range []string{"opencode_go", "openai", "commandcode", "opencode_zen"} {
		tmpl := cfg.Templates[name]
		if tmpl == nil {
			t.Fatalf("built-in %s template missing", name)
		}
		if tmpl.DefaultPlugins != nil {
			t.Errorf("built-in %s.DefaultPlugins = %v, want nil", name, tmpl.DefaultPlugins)
		}
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
plugins = ["sanitize_tools"]

[[instances]]
alias = "d"
template = "opencode_go"
`)
	for _, tc := range []struct {
		alias    string
		wantReq  string
		wantResp string
	}{
		{"a", "redact", "redact"},                 // absent → settings defaults
		{"b", "", ""},                             // explicit empty → off (durable)
		{"c", "sanitize_tools", "sanitize_tools"}, // instance list → active
		{"d", "redact", "redact"},                 // opencode_go absent → settings only, no seed fallback
	} {
		inst := cfg.Instances[aliasIndex(t, cfg, tc.alias)]
		req, resp := inst.EffectivePlugins(cfg)
		if strings.Join(req, ",") != tc.wantReq || strings.Join(resp, ",") != tc.wantResp {
			t.Errorf("instance %s effective plugins = (%v, %v), want (%s, %s)", tc.alias, req, resp, tc.wantReq, tc.wantResp)
		}
	}
}

func TestEffectivePluginsAbsentOffExplicitOn(t *testing.T) {
	// An instance with no plugins line resolves from settings only; explicit
	// ["sanitize_tools"] / [] behave as on / off. No template seed applies at
	// runtime.
	cfg := mustParse(t, `
[settings]
request_plugins = []
response_plugins = []

[[instances]]
alias = "go"
template = "openai"

[[instances]]
alias = "go-on"
template = "openai"
plugins = ["sanitize_tools"]

[[instances]]
alias = "go-off"
template = "openai"
plugins = []
`)
	req, resp := cfg.Instances[aliasIndex(t, cfg, "go")].EffectivePlugins(cfg)
	if len(req) != 0 || len(resp) != 0 {
		t.Errorf("absent instance effective plugins = (%v, %v), want empty", req, resp)
	}
	req, resp = cfg.Instances[aliasIndex(t, cfg, "go-on")].EffectivePlugins(cfg)
	if strings.Join(req, ",") != "sanitize_tools" || strings.Join(resp, ",") != "sanitize_tools" {
		t.Errorf("explicit instance effective plugins = (%v, %v), want (sanitize_tools, sanitize_tools)", req, resp)
	}
	req, resp = cfg.Instances[aliasIndex(t, cfg, "go-off")].EffectivePlugins(cfg)
	if len(req) != 0 || len(resp) != 0 {
		t.Errorf("explicit-empty instance effective plugins = (%v, %v), want empty (durable off)", req, resp)
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

func TestValidationPluginNamesAcceptedUnknownRejected(t *testing.T) {
	// Known plugin names are accepted everywhere plugins lists are valid:
	// settings defaults, template seeds, and per-instance lists.
	cfg := mustParse(t, `
[settings]
request_plugins = ["sanitize_tools"]
response_plugins = ["sanitize_tools"]

[providers.custom]
base_url = "http://example.com/v1"
plugins = ["sanitize_tools"]

[[instances]]
alias = "x"
template = "custom"
plugins = ["sanitize_tools"]
`)
	if len(cfg.Instances) != 1 {
		t.Fatalf("valid config with sanitize_tools should parse: %v", cfg.Instances)
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
plugins = ["sanitize_tools", "redact"]
`)
	clone := cfg.Clone()
	clone.Templates["custom"].DefaultPlugins[0] = "mutated"
	if got := cfg.Templates["custom"].DefaultPlugins[0]; got != "sanitize_tools" {
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
plugins = ["sanitize_tools"]
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
	if c.Plugins == nil || strings.Join(*c.Plugins, ",") != "sanitize_tools" {
		t.Errorf("instance plugins = [sanitize_tools] after Clone = %#v, want non-nil [sanitize_tools]", c.Plugins)
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
plugins = ["sanitize_tools", "redact"]
`)
	out, err := Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(out), "sanitize_tools") {
		t.Errorf("Marshal did not write the template plugins list: %s", out)
	}
	re, err := Parse(out)
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if got := strings.Join(re.Templates["custom"].DefaultPlugins, ","); got != "sanitize_tools,redact" {
		t.Errorf("round-tripped DefaultPlugins = %v, want [sanitize_tools redact]", re.Templates["custom"].DefaultPlugins)
	}
}

func TestToRawMaterializesTemplateDefaultPlugins(t *testing.T) {
	// An instance of a custom template carrying a DefaultPlugins seed, with
	// no plugins line of its own, gets the seed materialized as its own
	// plugins entry on write-back, so the file records plugins explicitly.
	// The live config is left untouched — the seed applies only to the
	// persisted form.
	cfg := mustParse(t, `
[providers.custom]
base_url = "http://example.com/v1"
models = ["m1"]
plugins = ["sanitize_tools"]

[[instances]]
alias = "go"
template = "custom"
`)
	raw := cfg.toRaw()
	if raw.Instances[0].Plugins == nil {
		t.Fatal("toRaw did not materialize the seed onto the unset instance")
	}
	if strings.Join(*raw.Instances[0].Plugins, ",") != "sanitize_tools" {
		t.Errorf("materialized instance plugins = %v, want [sanitize_tools]", *raw.Instances[0].Plugins)
	}
	if cfg.Instances[0].Plugins != nil {
		t.Error("toRaw mutated the live config")
	}
	out, err := Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(out), "sanitize_tools") {
		t.Errorf("Marshal output does not record the materialized default: %s", out)
	}
	// A reload of the written file sees the default explicitly active.
	re, err := Parse(out)
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	inst := re.Instances[aliasIndex(t, re, "go")]
	if inst.Plugins == nil || strings.Join(*inst.Plugins, ",") != "sanitize_tools" {
		t.Errorf("reloaded instance plugins = %#v, want explicit [sanitize_tools]", inst.Plugins)
	}
}

func TestToRawDoesNotOverwriteExplicitEmptyPlugins(t *testing.T) {
	// An explicit plugins = [] is a durable off-switch: write-back keeps it
	// empty and never materializes the template seed onto it.
	cfg := mustParse(t, `
[providers.custom]
base_url = "http://example.com/v1"
models = ["m1"]
plugins = ["sanitize_tools"]

[[instances]]
alias = "go"
template = "custom"
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
plugins = ["sanitize_tools"]
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
	if c.Plugins == nil || strings.Join(*c.Plugins, ",") != "sanitize_tools" {
		t.Errorf("non-empty instance plugins after round trip = %#v, want [sanitize_tools]", c.Plugins)
	}
}

func TestTemplateStyleValidation(t *testing.T) {
	// A bad style is rejected; the anthropic and responses styles are accepted.
	if _, err := Parse([]byte("[providers.x]\nbase_url = \"https://x\"\nstyle = \"grpc\"\n")); err == nil {
		t.Errorf("Parse accepted style = grpc")
	}
	cfg, err := Parse([]byte("[providers.x]\nbase_url = \"https://x\"\nstyle = \"anthropic\"\n"))
	if err != nil {
		t.Fatalf("Parse anthropic style: %v", err)
	}
	if cfg.Templates["x"].Style != StyleAnthropic {
		t.Errorf("style = %q, want anthropic", cfg.Templates["x"].Style)
	}
	cfg, err = Parse([]byte("[providers.x]\nbase_url = \"https://x\"\nstyle = \"responses\"\n"))
	if err != nil {
		t.Fatalf("Parse responses style: %v", err)
	}
	if cfg.Templates["x"].Style != StyleResponses {
		t.Errorf("style = %q, want responses", cfg.Templates["x"].Style)
	}
}

func TestCustomPlaceholderTemplates(t *testing.T) {
	// The placeholders load without a base_url (exempt from validation).
	cfg, err := Parse([]byte(``))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	for _, name := range []string{CustomOpenAI, CustomAnthropic} {
		tmpl, ok := cfg.Templates[name]
		if !ok {
			t.Fatalf("missing placeholder template %q", name)
		}
		if tmpl.BaseURL != "" {
			t.Errorf("placeholder %q base_url = %q, want empty", name, tmpl.BaseURL)
		}
	}
	if cfg.Templates[CustomAnthropic].Style != StyleAnthropic {
		t.Errorf("custom_anthropic style = %q", cfg.Templates[CustomAnthropic].Style)
	}

	// An instance referencing a placeholder is rejected (it must be resolved
	// to a concrete per-endpoint template first).
	_, err = Parse([]byte("[[instances]]\nalias = \"x\"\ntemplate = \"custom_openai\"\n"))
	if err == nil {
		t.Errorf("Parse accepted an instance of the custom_openai placeholder")
	}
}
