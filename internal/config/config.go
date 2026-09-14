// Package config implements the gateway.toml configuration surface: provider
// templates, per-account instances, the §4.2 alias rules, routing resolution,
// and an atomically-swappable ConfigManager for Phase 5 hot-reload.
//
// The TOML shape follows the §4.2 example exactly:
//
//	listen = "127.0.0.1:8787"
//	store  = "gateway.db"
//	retention_days = 7
//
//	[settings]
//	default_alias = "openai"
//	request_plugins = []
//	response_plugins = []
//	ui_theme = "dark"
//
//	[providers.openai]
//	base_url = "https://api.openai.com/v1"
//	api_key_env = "OPENAI_API_KEY"
//	models = ["gpt-4o", "gpt-4o-mini"]
//
//	[[instances]]
//	alias = "openai"
//	template = "openai"
//	api_key_env = "OPENAI_API_KEY"
//
// Per-plugin config lives under [plugins.<name>] (e.g. [plugins.redact] with
// patterns/field names); plugin names themselves are validated against the
// internal/plugins registry.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/pelletier/go-toml/v2"

	"github.com/amirzamli/shimmer-llmgateway/internal/plugins"
)

// AliasPattern is the §4.2 alias rule: upper/lowercase letters, digits, ".",
// "_" and "-", plus interior single spaces between words (e.g. "openai",
// "OpenAI", "My Provider"). "/" is excluded because it separates the
// "alias/model" routing prefix; leading, trailing and double spaces are
// rejected. Matching is exact and case-sensitive throughout — "OpenAI" and
// "openai" are two distinct aliases.
const AliasPattern = `[A-Za-z0-9_.-]+(?: [A-Za-z0-9_.-]+)*`

// DefaultRetentionDays is the payload retention window applied when
// gateway.toml does not set retention_days: after this many days a session's
// conversation payloads are removed and only the usage statistics remain. An
// explicit non-positive retention_days still disables purging entirely.
const DefaultRetentionDays = 7

// UserAgent is the User-Agent header the gateway sends on every outbound
// provider request: the chat/stream/responses forward path forwards the
// inbound client's User-Agent verbatim when present and falls back to this
// constant when absent, and the /models model-fetch and quota-probe requests
// (which have no inbound client) always use it. A constant own product name
// keeps upstream accounts from being fingerprinted as generic Go
// ("Go-http-client/1.1") HTTP traffic.
const UserAgent = "shimmer-gateway/1.0"

// genericReasoningLevels is the fallback effort vocabulary for models whose
// template advertises no ModelReasoningOptions. Sentinels ("", "default",
// "none") are handled separately in Validate.
var genericReasoningLevels = []string{"minimal", "low", "medium", "high", "xhigh", "max"}

var (
	aliasRe   = regexp.MustCompile(`^` + AliasPattern + `$`)
	envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// Settings is the [settings] table.
type Settings struct {
	// DefaultAlias is used when the client sends a model name with no alias
	// prefix and the alias's instance lists the model.
	DefaultAlias string `toml:"default_alias"`
	// RequestPlugins are global defaults, overridable per instance.
	RequestPlugins []string `toml:"request_plugins"`
	// ResponsePlugins are global defaults, overridable per instance.
	ResponsePlugins []string `toml:"response_plugins"`
	// LogPayloads opts into per-request console logging of the redacted
	// request/response payloads at the capture chokepoint.
	LogPayloads bool `toml:"log_payloads"`
	// UITheme is the dashboard color scheme persisted by the header toggle:
	// "dark" (the default look) or "light". Empty means dark.
	UITheme string `toml:"ui_theme,omitempty"`
}

// Template is a provider definition (a [providers.<name>] entry, the §4.2
// dropdown entry). Name is the map key, not a TOML body field, so it is
// excluded from write-back serialization.
type Template struct {
	Name      string `toml:"-"`
	BaseURL   string `toml:"base_url"`
	APIKeyEnv string `toml:"api_key_env"`
	// Style selects the upstream protocol the gateway speaks to this
	// provider: "" or "openai" (default) forwards to <base_url>/chat/completions
	// with an OpenAI-shaped body, "anthropic" forwards to <base_url>/messages
	// and translates between the OpenAI chat format and the Anthropic Messages
	// API (request, non-stream response, and SSE stream), and "responses"
	// forwards to <base_url>/responses and translates between the OpenAI chat
	// format and the OpenAI Responses API (§4.2).
	Style string `toml:"style,omitempty"`
	// OAuth marks the template as authenticated by a stored OAuth credential
	// (the ChatGPT Plus browser flow) instead of an API key. For such
	// templates the forward path resolves the instance's encrypted OAuth
	// record (refreshing it when expired) and injects the credential identity
	// upstream; api_key_env and the API-key secrets path are not consulted.
	// The built-in chatgpt template sets this; ordinary templates leave it
	// false and keep the unchanged API-key/keyless routing.
	OAuth  bool     `toml:"oauth,omitempty"`
	Models []string `toml:"models"`
	// DefaultPlugins is a materialization seed only: when an instance of this
	// template has no plugins of its own (nil), toRaw writes a copy of this
	// list into the instance's plugins line so the config file records the
	// default explicitly. It is never consulted at runtime — an instance with
	// an absent plugins list resolves from settings alone (empty by default =
	// no plugins). No built-in template seeds a default; custom templates may.
	DefaultPlugins []string `toml:"plugins,omitempty"`
	// ModelReasoningOptions advertises the valid reasoning effort levels per
	// model of this template (e.g. "deepseek-v4-pro": ["high", "max"]). It is
	// data only: validation consults it to gate instance model_reasoning
	// values, and the UI renders the advertised list per model.
	ModelReasoningOptions map[string][]string `toml:"model_reasoning_options,omitempty"`
	// SessionHeader names a request header the upstream requires to carry a
	// stable per-conversation session id on every chat call, or "" when none.
	// OpenCode Go is the built-in case: it rejects requests without an
	// x-opencode-session header (HTTP 400 MissingSessionID) so it can route
	// traffic and cache prompts. When set, the gateway sends the request's
	// effective session id (the client's X-Session-Id when present, else a
	// per-request UUID) in this header — the same id it echoes back as
	// X-Gateway-Session-Id, so turns of one conversation share the upstream
	// session. The header is synthesised by the forward path only; the
	// provider model fetch (/models) and quota probes are not affected.
	SessionHeader string `toml:"session_header,omitempty"`
	// IdentityHeaders are request headers the upstream expects every call to
	// carry so it can identify the calling client (the §4.2 opencode_go
	// template declares X-Opencode-Client: cli and X-Opencode-Project: global,
	// which the real opencode client sends). For each declared header the
	// gateway sends the declared value upstream on every request to this
	// template; when the inbound client request carries the same header name
	// (case-insensitive), the client's value wins so a faithful client
	// identity is forwarded as-is.
	IdentityHeaders map[string]string `toml:"identity_headers,omitempty"`
	// ModelStyles overrides the template-level Style per concrete model id
	// (§4.2): each key is a concrete model id and its value is one of the
	// valid style values ("openai", "anthropic", "responses"). A model
	// listed here routes to the override protocol instead of the template
	// default, so one provider (e.g. opencode_go) can serve several protocols
	// on one base URL. The template-level Style remains the default for models
	// not listed.
	ModelStyles map[string]string `toml:"model_styles,omitempty"`
	Docs        string            `toml:"docs,omitempty"`
}

// Instance is a concrete account of a template: alias, template, api_key_env,
// and an optional model subset.
type Instance struct {
	// Alias is the routing key. Empty means "auto-name per §4.2" (first
	// instance of a template takes the template name, each further instance
	// takes the next free name-N).
	Alias    string `toml:"alias"`
	Template string `toml:"template"`
	// APIKeyEnv defaults to the template's value when empty, so an empty env
	// name is omitted from write-back.
	APIKeyEnv string `toml:"api_key_env,omitempty"`
	// Models optionally narrows the template's model list for this instance.
	Models []string `toml:"models,omitempty"`
	// Plugins is the instance's own plugin list. The pointer keeps "absent"
	// (nil — no plugins line in the file, resolved from settings only, so
	// nothing is active by default) distinct from "explicit empty" (non-nil
	// pointer to an empty slice — `plugins = []` in the file, a durable
	// off-switch). A non-nil list applies to both request and response sides.
	Plugins *[]string `toml:"plugins,omitempty"`
	// ModelAliases maps a user-facing alias name to a concrete model string
	// (e.g. "small" → "gpt-4o-mini"). Keys must match AliasPattern; values are
	// non-empty concrete model strings (slashes allowed) that are forwarded
	// verbatim — expansion is single-level, never re-expanded.
	ModelAliases map[string]string `toml:"model_aliases,omitempty"`
	// ModelReasoning maps a model alias name to a reasoning effort level
	// (e.g. "small" → "high", "medium", "low", "none", "default", "").
	ModelReasoning map[string]string `toml:"model_reasoning,omitempty"`
	// Disabled excludes the instance from routing (§6.2 PATCH disable). It
	// stays visible to the API/UI for re-enabling, but Resolve never routes
	// to it.
	Disabled bool `toml:"disabled,omitempty"`
	// Priority orders unprefixed (alias-key) model resolution: a lower value
	// wins over a higher one, and any instance with an explicit priority wins
	// over instances without one (0 = unset), which keep config file order
	// among themselves. Prefixed "alias/model" routing is never affected.
	Priority int `toml:"priority,omitempty"`
}

// Config is a parsed and validated gateway.toml.
type Config struct {
	// ListenAddrs are the host:port addresses the gateway serves. One listener
	// is opened per address, so the gateway can bind localhost and a Tailscale
	// address at once. ListenAddrs is empty when the file used the legacy
	// single `listen` key.
	ListenAddrs   []string
	Store         string
	RetentionDays int
	Settings      Settings
	Templates     map[string]*Template
	Instances     []*Instance
	// Plugins holds the optional [plugins.<name>] TOML tables (e.g.
	// [plugins.redact] with patterns/field names), raw-decoded. The registry
	// itself lives in internal/plugins; this is only per-plugin configuration.
	Plugins map[string]map[string]any

	// userTemplates records which template names were defined in gateway.toml
	// (as opposed to the built-ins). Marshal writes back only these, keeping
	// the file minimal and preserving the built-in merge on reload.
	userTemplates map[string]bool

	instancesByAlias map[string]*Instance
}

// MarkUserTemplate records name as a user-defined template so Marshal writes
// it back to gateway.toml (built-ins are implicit and re-merged on load).
func (c *Config) MarkUserTemplate(name string) {
	if c.userTemplates == nil {
		c.userTemplates = map[string]bool{}
	}
	c.userTemplates[name] = true
}

// Clone returns a copy of the config safe to mutate before a Swap: templates,
// instances, and the user-template marker set are copied so mutation never
// races with readers of the live config. Slice fields preserve nil-vs-empty so
// an explicit `plugins = []` toggle-off survives the copy. The plugins tables
// are shared read-only (the API never mutates them).
func (c *Config) Clone() *Config {
	out := &Config{
		ListenAddrs:   append([]string{}, c.ListenAddrs...),
		Store:         c.Store,
		RetentionDays: c.RetentionDays,
		Settings:      c.Settings,
		Templates:     make(map[string]*Template, len(c.Templates)),
		Instances:     make([]*Instance, len(c.Instances)),
		Plugins:       c.Plugins,
		userTemplates: make(map[string]bool, len(c.userTemplates)),
	}
	for name, t := range c.Templates {
		tc := *t
		if t.Models != nil {
			tc.Models = append([]string{}, t.Models...)
		}
		if t.DefaultPlugins != nil {
			tc.DefaultPlugins = append([]string{}, t.DefaultPlugins...)
		}
		if t.ModelReasoningOptions != nil {
			tc.ModelReasoningOptions = make(map[string][]string, len(t.ModelReasoningOptions))
			for k, v := range t.ModelReasoningOptions {
				tc.ModelReasoningOptions[k] = append([]string{}, v...)
			}
		}
		if t.IdentityHeaders != nil {
			tc.IdentityHeaders = make(map[string]string, len(t.IdentityHeaders))
			for k, v := range t.IdentityHeaders {
				tc.IdentityHeaders[k] = v
			}
		}
		if t.ModelStyles != nil {
			tc.ModelStyles = make(map[string]string, len(t.ModelStyles))
			for k, v := range t.ModelStyles {
				tc.ModelStyles[k] = v
			}
		}
		out.Templates[name] = &tc
	}
	for i, inst := range c.Instances {
		ic := *inst
		if inst.Models != nil {
			ic.Models = append([]string{}, inst.Models...)
		}
		if inst.Plugins != nil {
			cp := append([]string{}, (*inst.Plugins)...)
			ic.Plugins = &cp
		}
		if inst.ModelAliases != nil {
			ic.ModelAliases = make(map[string]string, len(inst.ModelAliases))
			for k, v := range inst.ModelAliases {
				ic.ModelAliases[k] = v
			}
		}
		if inst.ModelReasoning != nil {
			ic.ModelReasoning = make(map[string]string, len(inst.ModelReasoning))
			for k, v := range inst.ModelReasoning {
				ic.ModelReasoning[k] = v
			}
		}
		out.Instances[i] = &ic
	}
	// The alias index is rebuilt over the cloned instances (the original
	// pointers are not shared, so mutations never touch the live config).
	if len(out.Instances) > 0 {
		out.instancesByAlias = make(map[string]*Instance, len(out.Instances))
		for _, inst := range out.Instances {
			out.instancesByAlias[inst.Alias] = inst
		}
	}
	for name, ok := range c.userTemplates {
		out.userTemplates[name] = ok
	}
	return out
}

// EffectiveModels returns the models this instance can route: its own subset
// when set, otherwise the template's list.
func (inst *Instance) EffectiveModels(c *Config) []string {
	if len(inst.Models) > 0 {
		return inst.Models
	}
	if t, ok := c.Templates[inst.Template]; ok {
		return t.Models
	}
	return nil
}

// EffectiveAPIKeyEnv returns the instance's api_key_env, defaulting to the
// template's when the instance does not set one.
func (inst *Instance) EffectiveAPIKeyEnv(c *Config) string {
	if inst.APIKeyEnv != "" {
		return inst.APIKeyEnv
	}
	if t, ok := c.Templates[inst.Template]; ok {
		return t.APIKeyEnv
	}
	return ""
}

// EffectivePlugins returns the instance's plugin chain (§4.5 scope), resolved
// in two tiers: a non-nil per-instance plugins list wins (empty list = no
// plugins for that account, a durable off-switch); otherwise the global
// settings.request_plugins / settings.response_plugins apply. An absent
// instance plugins list never auto-activates from a template default — the
// opencode_go default is a write-back materialization seed only (see toRaw),
// so "no plugins line" means no plugins at runtime (settings are empty by
// default). An instance list applies to both sides.
func (inst *Instance) EffectivePlugins(c *Config) (request, response []string) {
	if inst.Plugins != nil {
		return *inst.Plugins, *inst.Plugins
	}
	return c.Settings.RequestPlugins, c.Settings.ResponsePlugins
}

// PluginConfig returns the raw [plugins.<name>] TOML table, or nil when absent.
func (c *Config) PluginConfig(name string) map[string]any {
	if c.Plugins == nil {
		return nil
	}
	return c.Plugins[name]
}

// AliasList returns the routable (enabled) instance aliases in config order.
// Disabled instances are excluded so an unknown-alias error never lists an
// alias the gateway will not route to.
func (c *Config) AliasList() []string {
	out := make([]string, 0, len(c.Instances))
	for _, inst := range c.Instances {
		if inst.Disabled {
			continue
		}
		out = append(out, inst.Alias)
	}
	return out
}

// Instance returns the instance registered under alias.
func (c *Config) Instance(alias string) (*Instance, bool) {
	inst, ok := c.instancesByAlias[alias]
	return inst, ok
}

// EffectiveReasoning returns the configured reasoning level for the given model key
// (e.g. "small" -> "high"). Returns "" if not set or default.
func (inst *Instance) EffectiveReasoning(modelKey string) string {
	if inst.ModelReasoning == nil {
		return ""
	}
	r := inst.ModelReasoning[modelKey]
	if r == "default" {
		return ""
	}
	return r
}

// routingOrder returns the instances ordered for unprefixed model resolution:
// instances with an explicit Priority come first (ascending priority), then
// instances without one in config file order. The sort is stable, so file
// order is preserved within each group. Disabled instances are not filtered
// here — callers skip them.
func (c *Config) routingOrder() []*Instance {
	out := make([]*Instance, len(c.Instances))
	copy(out, c.Instances)
	sort.SliceStable(out, func(i, j int) bool {
		pi, pj := out[i].Priority, out[j].Priority
		switch {
		case pi > 0 && pj > 0:
			return pi < pj
		case pi > 0:
			return true
		case pj > 0:
			return false
		default:
			return false
		}
	})
	return out
}

// Resolve routes a client model string to an instance and the model name to
// forward. Disabled instances are not routable: a prefixed model naming a
// disabled alias returns an *UnknownAliasError (listing the enabled aliases),
// and disabled instances are skipped for unprefixed resolution.
//
// Prefixed form "alias/model": the alias must exist and be enabled, otherwise
// an *UnknownAliasError carrying the available alias list is returned (the
// Phase 3 caller turns that into a 400 INVALID_ARGUMENT). When the model is a
// key of the instance's model_aliases map it is expanded to the mapped value;
// otherwise the model after the slash is returned verbatim (the current
// no-membership-check behavior).
//
// Unprefixed form: resolve via settings.default_alias if that instance maps or
// lists the model, else the first enabled instance — in routingOrder (explicit
// priority ascending, then file order) — that maps or lists the model, else an
// *UnknownModelError. An alias mapping shadows a literal model membership, and
// the mapped value is forwarded verbatim (single-level expansion).
func (c *Config) Resolve(model string) (*Instance, string, error) {
	if i := strings.IndexByte(model, '/'); i >= 0 {
		alias, rest := model[:i], model[i+1:]
		inst, ok := c.instancesByAlias[alias]
		if !ok || inst.Disabled {
			return nil, "", &UnknownAliasError{Alias: alias, Aliases: c.AliasList()}
		}
		if mapped, ok := inst.ModelAliases[rest]; ok {
			return inst, mapped, nil
		}
		return inst, rest, nil
	}
	// Alias maps shadow literal membership, so all map passes run before the
	// literal fallback (default_alias map → first enabled instance map → literal).
	if c.Settings.DefaultAlias != "" {
		if inst, ok := c.instancesByAlias[c.Settings.DefaultAlias]; ok && !inst.Disabled {
			if mapped, ok := inst.ModelAliases[model]; ok {
				return inst, mapped, nil
			}
		}
	}
	for _, inst := range c.routingOrder() {
		if inst.Disabled {
			continue
		}
		if mapped, ok := inst.ModelAliases[model]; ok {
			return inst, mapped, nil
		}
	}
	if c.Settings.DefaultAlias != "" {
		if inst, ok := c.instancesByAlias[c.Settings.DefaultAlias]; ok && !inst.Disabled && contains(inst.EffectiveModels(c), model) {
			return inst, model, nil
		}
	}
	for _, inst := range c.routingOrder() {
		if inst.Disabled {
			continue
		}
		if contains(inst.EffectiveModels(c), model) {
			return inst, model, nil
		}
	}
	return nil, "", &UnknownModelError{Model: model}
}

// AliasModelIDs returns the routable model-alias ids to advertise in
// /v1/models: for each enabled instance in routing order, the deduped
// unprefixed key plus the "alias/key" prefixed form, with keys sorted within
// an instance for a deterministic order. Disabled instances contribute
// nothing.
func (c *Config) AliasModelIDs() []string {
	var out []string
	seenUnprefixed := map[string]bool{}
	seenAliased := map[string]bool{}
	for _, inst := range c.routingOrder() {
		if inst.Disabled {
			continue
		}
		keys := make([]string, 0, len(inst.ModelAliases))
		for key := range inst.ModelAliases {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if !seenUnprefixed[key] {
				seenUnprefixed[key] = true
				out = append(out, key)
			}
			id := inst.Alias + "/" + key
			if !seenAliased[id] {
				seenAliased[id] = true
				out = append(out, id)
			}
		}
	}
	return out
}

// UnknownAliasError reports a prefixed model naming an alias that does not
// exist. Aliases lists the available aliases (mapped to a 400 INVALID_ARGUMENT
// in Phase 3).
type UnknownAliasError struct {
	Alias   string
	Aliases []string
}

func (e *UnknownAliasError) Error() string {
	return fmt.Sprintf("unknown alias %q; available aliases: %s", e.Alias, strings.Join(e.Aliases, ", "))
}

// UnknownModelError reports that no configured instance lists the model.
type UnknownModelError struct {
	Model string
}

func (e *UnknownModelError) Error() string {
	return fmt.Sprintf("unknown model %q", e.Model)
}

// rawConfig mirrors gateway.toml for unmarshalling. RetentionDays is a
// pointer so an absent key (nil → DefaultRetentionDays) is distinguishable
// from an explicit `retention_days = 0` (purging disabled).
type rawConfig struct {
	ListenAddrs   []string                  `toml:"listen_addrs"`
	Listen        string                    `toml:"listen"`
	Store         string                    `toml:"store"`
	RetentionDays *int                      `toml:"retention_days"`
	Settings      Settings                  `toml:"settings"`
	Providers     map[string]Template       `toml:"providers"`
	Instances     []Instance                `toml:"instances"`
	Plugins       map[string]map[string]any `toml:"plugins"`
}

// Parse parses and validates gateway.toml bytes. Instances with an empty
// alias are auto-named per §4.2 before validation.
func Parse(data []byte) (*Config, error) {
	var raw rawConfig
	if err := toml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse gateway.toml: %w", err)
	}
	return fromRaw(&raw)
}

// Load reads and parses the gateway.toml at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// builtinTemplates are the provider templates that ship with the gateway: the
// six §4.2 templates (openai/ollama match the §4.2 example exactly; the rest
// get reasonable OpenAI-compatible defaults) plus catalog-derived templates
// seeded from the verified provider catalog in internal/config/providers.json.
// Model lists are representative defaults users can override per instance.
func builtinTemplates() map[string]Template {
	return map[string]Template{
		"openai": {
			BaseURL:   "https://api.openai.com/v1",
			APIKeyEnv: "OPENAI_API_KEY",
			Models:    []string{"gpt-4o", "gpt-4o-mini"},
		},
		// chatgpt: a ChatGPT Plus/Pro account authenticated by the browser
		// OAuth flow (internal/oauth) rather than an API key. The upstream is
		// the verified OpenCode v1.18.30 ChatGPT codex endpoint, which speaks
		// the Responses API, so the template defaults to the responses style
		// and the forward path translates requests to that wire format. The
		// gateway sends its own identity (never impersonating OpenCode): the
		// stored credential's bearer token and account id, the per-conversation
		// session-id header, and the gateway User-Agent. Models are configured
		// here because the codex endpoint does not expose the OpenAI /models
		// contract. Leave the list empty rather than shipping a stale model
		// allowlist; callers select a currently supported codex model explicitly.
		"chatgpt": {
			BaseURL:       ChatGPTCodexBaseURL,
			Style:         StyleResponses,
			OAuth:         true,
			SessionHeader: ChatGPTCodexSessionHeader,
		},
		"anthropic": {
			BaseURL:   "https://api.anthropic.com/v1",
			APIKeyEnv: "ANTHROPIC_API_KEY",
			Models:    []string{"claude-3-5-sonnet-latest", "claude-3-5-haiku-latest"},
		},
		"ollama": {
			BaseURL:   "http://localhost:11434/v1",
			APIKeyEnv: "",
			Models:    []string{"llama3.1"},
		},
		"groq": {
			BaseURL:   "https://api.groq.com/openai/v1",
			APIKeyEnv: "GROQ_API_KEY",
			Models:    []string{"llama-3.3-70b-versatile"},
		},
		"vllm": {
			BaseURL:   "http://localhost:8000/v1",
			APIKeyEnv: "VLLM_API_KEY",
			Models:    []string{"meta-llama/Meta-Llama-3-8B-Instruct"},
		},
		"lite_llm": {
			BaseURL:   "http://localhost:4000/v1",
			APIKeyEnv: "LITELLM_API_KEY",
			Models:    []string{"gpt-4o"},
		},
		// Templates below are derived from the verified provider catalog at
		// internal/config/providers.json; base URLs and docs URLs come from the
		// catalog entries that expose an OpenAI-compatible chat-completions
		// endpoint (template.base_url + /chat/completions).
		"openrouter": {
			BaseURL:   "https://openrouter.ai/api/v1",
			APIKeyEnv: "OPENROUTER_API_KEY",
			Models:    []string{"openrouter/auto"},
			Docs:      "https://openrouter.ai/docs/api/reference/overview",
		},
		"deepseek": {
			BaseURL:   "https://api.deepseek.com",
			APIKeyEnv: "DEEPSEEK_API_KEY",
			Models:    []string{"deepseek-chat", "deepseek-reasoner", "deepseek-v4-pro", "deepseek-v4-flash"},
			ModelReasoningOptions: map[string][]string{
				"deepseek-v4-flash": {"low", "high", "max"},
				"deepseek-v4-pro":   {"high", "max"},
			},
			Docs: "https://api-docs.deepseek.com",
		},
		"gemini": {
			// The catalog's OpenAI-compatible base, not the native
			// /v1beta RPC base.
			BaseURL:   "https://generativelanguage.googleapis.com/v1beta/openai",
			APIKeyEnv: "GEMINI_API_KEY",
			Models:    []string{"gemini-2.5-pro", "gemini-2.5-flash"},
			Docs:      "https://ai.google.dev/gemini-api/docs",
		},
		"mistral": {
			BaseURL:   "https://api.mistral.ai/v1",
			APIKeyEnv: "MISTRAL_API_KEY",
			Models:    []string{"mistral-large-latest", "mistral-small-latest"},
			Docs:      "https://docs.mistral.ai",
		},
		"kimi": {
			BaseURL:   "https://api.moonshot.ai/v1",
			APIKeyEnv: "MOONSHOT_API_KEY",
			Models:    []string{"kimi-latest", "kimi-k2"},
			Docs:      "https://platform.kimi.ai/docs/api/overview",
		},
		// zai: the catalog marks OpenAI-compat status "unknown" but documents
		// /chat/completions + Bearer auth + /models on this base, so it fits
		// the gateway's OpenAI-compatible forward path. Official docs
		// recommend reasoning_effort max for GLM-5.3(-Flash) and state
		// GLM-5.3-flash text parameters match GLM-5.3.
		"zai": {
			BaseURL:   "https://api.z.ai/api/paas/v4",
			APIKeyEnv: "ZAI_API_KEY",
			Models:    []string{"glm-4.6", "glm-4.5", "glm-4.7", "glm-5.3", "glm-5.3-flash"},
			ModelReasoningOptions: map[string][]string{
				"glm-5.3":       {"low", "high", "max"},
				"glm-5.3-flash": {"low", "high", "max"},
			},
			Docs: "https://docs.z.ai/api-reference/introduction",
		},
		"opencode_zen": {
			BaseURL:   "https://opencode.ai/zen/v1",
			APIKeyEnv: "OPENCODE_API_KEY",
			Models:    []string{"claude-sonnet-4", "gpt-4o"},
			Docs:      "https://opencode.ai/docs/zen/",
		},
		"opencode_go": {
			BaseURL:   "https://opencode.ai/zen/go/v1",
			APIKeyEnv: "OPENCODE_API_KEY",
			Models: []string{
				"grok-4.6", "gpt-5.6-luna",
				"glm-5.3-flash", "glm-5.3", "glm-5.2", "glm-5.1",
				"kimi-k3", "kimi-k2.7-code", "kimi-k2.6", "longcat-2.0",
				"deepseek-flash", "deepseek-v4-pro", "deepseek-v4-flash",
				"deepseek-v4-flash-vision-exp",
				"mimo-v2.5", "mimo-v2.5-pro",
				"minimax-m3", "minimax-m2.7", "minimax-m2.5",
				"muse-spark-1.3-contributor", "muse-spark-1.2-contributor",
				"qwen3.8-max", "qwen3.8-flash", "qwen3.7-max", "qwen3.7-plus", "qwen3.6-plus",
				"hy4-preview", "hy3",
			},
			SessionHeader: "x-opencode-session",
			// The upstream expects coding-agent identity headers on every
			// call; these mirror what the real opencode client sends.
			IdentityHeaders: map[string]string{
				"X-Opencode-Client":  "cli",
				"X-Opencode-Project": "global",
			},
			// The opencode.go endpoint serves three protocols on one base URL:
			// /chat/completions (the openai default) for most models,
			// /responses for the muse contributor models, and /messages for
			// the anthropic-style models. The template default is openai; the
			// per-model overrides below route each model to its protocol.
			ModelStyles: map[string]string{
				"grok-4.6":                   "responses",
				"gpt-5.6-luna":               "responses",
				"muse-spark-1.3-contributor": "responses",
				"muse-spark-1.2-contributor": "responses",
				"minimax-m3":                 "anthropic",
				"minimax-m2.7":               "anthropic",
				"minimax-m2.5":               "anthropic",
				"qwen3.8-max":                "anthropic",
				"qwen3.8-flash":              "anthropic",
				"qwen3.7-max":                "anthropic",
				"qwen3.7-plus":               "anthropic",
				"qwen3.6-plus":               "anthropic",
			},
			Docs: "https://opencode.ai/docs/go/",
		},
		// commandcode: hybrid gateway (OpenAI-compatible /chat/completions and
		// Anthropic-style /messages on one base); the catalog entry is
		// analogous to opencode_go/opencode_zen.
		"commandcode": {
			BaseURL:   "https://api.commandcode.ai/provider/v1",
			APIKeyEnv: "COMMANDCODE_API_KEY",
			Models:    []string{"claude-sonnet-5", "gpt-5.6-sol", "deepseek/deepseek-v4-pro", "deepseek/deepseek-v4-flash"},
			ModelReasoningOptions: map[string][]string{
				"deepseek/deepseek-v4-flash": {"low", "high", "max"},
				"deepseek/deepseek-v4-pro":   {"high", "max"},
			},
			Docs: "https://commandcode.ai/docs/provider",
		},
		// custom_openai / custom_anthropic are placeholders for user-supplied
		// endpoints (any provider, including local servers such as llama.cpp or
		// a local Anthropic-compatible proxy). They carry no base_url — the
		// endpoint is required when creating an instance — and the API resolves
		// the placeholder to a concrete template before the instance is
		// created: an existing template already serving that endpoint is
		// reused, otherwise a custom template is minted named after the
		// instance alias (falling back to the endpoint when the alias is
		// blank).
		"custom_openai": {
			Models: []string{"my-model"},
		},
		"custom_anthropic": {
			Style:  "anthropic",
			Models: []string{"claude-sonnet-4"},
		},
	}
}

// BuiltinTemplates returns the built-in provider templates keyed by name, with
// each template's Name set to its key. Runtime code uses the parsed config
// instead; this export exists for tooling that mirrors the template set (the
// pricing generator maps these names to source-catalog providers).
func BuiltinTemplates() map[string]Template {
	out := builtinTemplates()
	for name, t := range out {
		t.Name = name
		out[name] = t
	}
	return out
}

// CustomOpenAI and CustomAnthropic are the built-in placeholder templates that
// the add-instance flow resolves into a concrete template (reusing an existing
// one serving the endpoint, or minting one named after the instance alias).
// They are exempt from the base_url requirement at config load (their endpoint
// is user-supplied at instance creation).
const (
	CustomOpenAI    = "custom_openai"
	CustomAnthropic = "custom_anthropic"
	StyleOpenAI     = "openai"
	StyleAnthropic  = "anthropic"
	StyleResponses  = "responses"
	// ChatGPTCodexBaseURL is the only upstream base URL permitted for an
	// OAuth-marked template. OAuth credentials are bearer credentials, so the
	// endpoint cannot be user-configurable.
	ChatGPTCodexBaseURL = "https://chatgpt.com/backend-api/codex"
	// ChatGPTCodexSessionHeader is required by the fixed Codex upstream
	// contract for conversation routing.
	ChatGPTCodexSessionHeader = "session-id"
)

// IsCustomTemplatePlaceholder reports whether name is one of the endpoint-less
// placeholder templates that get resolved into a concrete custom template.
func IsCustomTemplatePlaceholder(name string) bool {
	return name == CustomOpenAI || name == CustomAnthropic
}

// fromRaw merges built-in templates with the file's providers (a file entry
// overrides the built-in of the same name), then validates and indexes.
func fromRaw(raw *rawConfig) (*Config, error) {
	templates := builtinTemplates()
	for name, t := range raw.Providers {
		templates[name] = t
	}
	tmap := make(map[string]*Template, len(templates))
	for name, t := range templates {
		t.Name = name
		tmap[name] = &t
	}
	cfg := &Config{
		ListenAddrs: append([]string{}, raw.ListenAddrs...),
		Store:       raw.Store,
		// Absent retention_days defaults to the 7-day payload window; an
		// explicit non-positive value disables purging (the store treats
		// <= 0 as off).
		RetentionDays: DefaultRetentionDays,
		Settings:      raw.Settings,
		Templates:     tmap,
		Instances:     make([]*Instance, len(raw.Instances)),
		Plugins:       raw.Plugins,
		userTemplates: make(map[string]bool, len(raw.Providers)),
	}
	if raw.RetentionDays != nil {
		cfg.RetentionDays = *raw.RetentionDays
	}
	// The legacy single listen address keeps working when listen_addrs is
	// absent; both keys may not be set at once. With neither key, the
	// loopback default applies (the old behavior of binding a wildcard on a
	// typo'd key was worse).
	if len(raw.ListenAddrs) == 0 && raw.Listen != "" {
		cfg.ListenAddrs = []string{raw.Listen}
	}
	if len(raw.ListenAddrs) > 0 && raw.Listen != "" {
		return nil, fmt.Errorf("set listen_addrs or listen, not both")
	}
	if len(cfg.ListenAddrs) == 0 {
		cfg.ListenAddrs = []string{"127.0.0.1:8787"}
	}
	for name := range raw.Providers {
		cfg.userTemplates[name] = true
	}
	for i := range raw.Instances {
		cfg.Instances[i] = &raw.Instances[i]
	}
	if err := Validate(cfg); err != nil {
		return nil, err
	}
	cfg.index()
	return cfg, nil
}

func (c *Config) index() {
	c.instancesByAlias = make(map[string]*Instance, len(c.Instances))
	for _, inst := range c.Instances {
		c.instancesByAlias[inst.Alias] = inst
	}
}

// ValidationError reports config problems found by Validate.
type ValidationError struct {
	Problems []string
}

func (e *ValidationError) Error() string {
	return "invalid config: " + strings.Join(e.Problems, "; ")
}

// ValidateBaseURL reports whether baseURL is an absolute http(s) URL usable
// as a provider base. The scheme plus non-empty-host checks reject file://,
// gopher://, empty, and relative URLs without blocking the legitimately
// loopback/private providers (ollama, vllm).
func ValidateBaseURL(baseURL string) error {
	if baseURL == "" {
		return errors.New("base_url is required")
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return fmt.Errorf("invalid base_url: %v", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("base_url %q must use scheme http or https", baseURL)
	}
	if u.Host == "" {
		return fmt.Errorf("base_url %q must include a host", baseURL)
	}
	return nil
}

// Validate enforces the §4.2 alias rules and config invariants:
//   - template names match AliasPattern (they seed auto-named aliases) and
//     template base_url values are absolute http(s) URLs;
//   - every instance references a known template;
//   - aliases are unique and match AliasPattern;
//   - per-instance api_key_env values are valid env var names;
//   - per-instance model_aliases keys match AliasPattern and values are
//     non-empty;
//   - settings.ui_theme, when set, is "dark" or "light";
//   - every plugin name (settings defaults, template defaults, or per-instance
//     list) is known to the built-in registry or the control-plugin set
//     (§4.5).
//
// Instances with an empty alias are auto-named in place: the first instance
// of a template takes the template name, and each further instance takes the
// next free name-N, skipping already-taken aliases.
func Validate(c *Config) error {
	var problems []string

	for name, t := range c.Templates {
		if t.Name == "" {
			t.Name = name
		}
		if !aliasRe.MatchString(name) {
			problems = append(problems, fmt.Sprintf("template name %q must match %s", name, AliasPattern))
		}
		if t.Style != "" && t.Style != StyleOpenAI && t.Style != StyleAnthropic && t.Style != StyleResponses {
			problems = append(problems, fmt.Sprintf("template %q: style must be %q, %q, %q, or empty", name, StyleOpenAI, StyleAnthropic, StyleResponses))
		}
		// model_styles keys are concrete model ids and values are the same
		// style vocabulary as Style; an empty key or an empty/invalid value
		// would be a silent routing misconfiguration, so both fail at load.
		for model, style := range t.ModelStyles {
			if model == "" {
				problems = append(problems, fmt.Sprintf("template %q: model_styles contains an empty model key", name))
				continue
			}
			if style == "" || (style != StyleOpenAI && style != StyleAnthropic && style != StyleResponses) {
				problems = append(problems, fmt.Sprintf("template %q: model_styles for model %q must be %q, %q, or %q", name, model, StyleOpenAI, StyleAnthropic, StyleResponses))
			}
		}
		// The custom placeholders are endpoint-less by design; their endpoint
		// is supplied (and validated) when an instance is created.
		if !IsCustomTemplatePlaceholder(name) {
			if err := ValidateBaseURL(t.BaseURL); err != nil {
				problems = append(problems, fmt.Sprintf("template %q: %v", name, err))
			}
		}
		if t.OAuth {
			if t.BaseURL != ChatGPTCodexBaseURL {
				problems = append(problems, fmt.Sprintf("template %q: oauth templates must use the fixed ChatGPT Codex base_url %q", name, ChatGPTCodexBaseURL))
			}
			if t.Style != StyleResponses {
				problems = append(problems, fmt.Sprintf("template %q: oauth templates must use style %q", name, StyleResponses))
			}
			if t.SessionHeader != ChatGPTCodexSessionHeader {
				problems = append(problems, fmt.Sprintf("template %q: oauth templates must use session_header %q", name, ChatGPTCodexSessionHeader))
			}
			if t.APIKeyEnv != "" {
				problems = append(problems, fmt.Sprintf("template %q: oauth templates cannot set api_key_env", name))
			}
			for model, style := range t.ModelStyles {
				if style != StyleResponses {
					problems = append(problems, fmt.Sprintf("template %q: oauth model_styles for %q must use style %q", name, model, StyleResponses))
				}
			}
		}
		// Identity headers become literal upstream headers, so a bad name or
		// a value carrying CR/LF would be a header-smuggling vector; reject
		// both at load.
		for hname, hvalue := range t.IdentityHeaders {
			if hname == "" {
				problems = append(problems, fmt.Sprintf("template %q: identity_headers contains an empty header name", name))
			}
			if strings.ContainsAny(hname, "\r\n") || strings.ContainsAny(hvalue, "\r\n") {
				problems = append(problems, fmt.Sprintf("template %q: identity header %q must not contain CR/LF", name, hname))
			}
		}
	}

	for i, inst := range c.Instances {
		if _, ok := c.Templates[inst.Template]; !ok {
			problems = append(problems, fmt.Sprintf("instance %d: unknown template %q", i+1, inst.Template))
		}
		if IsCustomTemplatePlaceholder(inst.Template) {
			problems = append(problems, fmt.Sprintf("instance %d: template %q is a placeholder — an endpoint URL must be provided so it resolves to a concrete custom template", i+1, inst.Template))
		}
	}

	// Auto-name empty aliases, skipping aliases already taken (explicit or
	// auto-assigned).
	used := make(map[string]bool, len(c.Instances))
	for _, inst := range c.Instances {
		if inst.Alias != "" {
			if used[inst.Alias] {
				problems = append(problems, fmt.Sprintf("duplicate alias %q", inst.Alias))
			}
			used[inst.Alias] = true
		}
	}
	for _, inst := range c.Instances {
		if inst.Alias != "" {
			continue
		}
		base := inst.Template
		if t, ok := c.Templates[inst.Template]; ok {
			base = t.Name
		}
		name := base
		for i := 2; used[name]; i++ {
			name = fmt.Sprintf("%s-%d", base, i)
		}
		inst.Alias = name
		used[name] = true
	}

	for _, inst := range c.Instances {
		if !aliasRe.MatchString(inst.Alias) {
			problems = append(problems, fmt.Sprintf("alias %q must match %s", inst.Alias, AliasPattern))
		}
		if inst.Priority < 0 {
			problems = append(problems, fmt.Sprintf("instance %q: priority must be >= 0", inst.Alias))
		}
		if inst.APIKeyEnv != "" && !envNameRe.MatchString(inst.APIKeyEnv) {
			problems = append(problems, fmt.Sprintf("instance %q: invalid api_key_env %q", inst.Alias, inst.APIKeyEnv))
		}
		for key, value := range inst.ModelAliases {
			if !aliasRe.MatchString(key) {
				problems = append(problems, fmt.Sprintf("instance %q: model alias key %q must match %s", inst.Alias, key, AliasPattern))
			}
			if value == "" {
				problems = append(problems, fmt.Sprintf("instance %q: model alias %q has an empty model value", inst.Alias, key))
			}
		}
		for key, value := range inst.ModelReasoning {
			if !aliasRe.MatchString(key) {
				problems = append(problems, fmt.Sprintf("instance %q: model reasoning key %q must match %s", inst.Alias, key, AliasPattern))
			}
			// Two-tier reasoning validation: sentinels are always accepted;
			// anything else must be either an extended generic level or a
			// value the instance's template advertises for the resolved model.
			switch value {
			case "", "default", "none":
			default:
				valid := false
				var advertised []string
				if t, ok := c.Templates[inst.Template]; ok {
					model := key
					if mapped, ok := inst.ModelAliases[key]; ok {
						model = mapped
					}
					if opts, ok := t.ModelReasoningOptions[model]; ok {
						advertised = opts
						valid = slices.Contains(opts, value)
					}
				}
				// No advertised options for this model: fall back to the
				// extended generic effort vocabulary.
				if !valid && advertised == nil {
					valid = slices.Contains(genericReasoningLevels, value)
				}
				if !valid {
					if advertised != nil {
						problems = append(problems, fmt.Sprintf("instance %q: model reasoning for %q has invalid value %q (must be one of: %s)", inst.Alias, key, value, strings.Join(advertised, ", ")))
					} else {
						problems = append(problems, fmt.Sprintf("instance %q: model reasoning for %q has invalid value %q (must be one of: default, none, minimal, low, medium, high, xhigh, max, or a value advertised by the model)", inst.Alias, key, value))
					}
				}
			}
		}
	}

	// The dashboard theme is a closed vocabulary; anything else is a config
	// error so a typo fails at load (or at PATCH time) instead of leaving the
	// UI stuck on an unstyled body class.
	switch c.Settings.UITheme {
	case "", "dark", "light":
	default:
		problems = append(problems, fmt.Sprintf("settings.ui_theme %q must be \"dark\" or \"light\"", c.Settings.UITheme))
	}

	// Plugins are selected by name from the built-in registry (§4.5); an
	// unknown name in the global defaults or a per-instance list is a config
	// error so a typo fails at load, not at request time.
	known := plugins.Known()
	checkPlugins := func(list []string, where string) {
		for _, name := range list {
			if !slices.Contains(known, name) {
				problems = append(problems, fmt.Sprintf("%s: unknown plugin %q; available plugins: %s", where, name, strings.Join(known, ", ")))
			}
		}
	}
	checkPlugins(c.Settings.RequestPlugins, "settings.request_plugins")
	checkPlugins(c.Settings.ResponsePlugins, "settings.response_plugins")
	for _, inst := range c.Instances {
		if inst.Plugins != nil {
			checkPlugins(*inst.Plugins, fmt.Sprintf("instance %q plugins", inst.Alias))
		}
		if t, ok := c.Templates[inst.Template]; ok && t.OAuth && inst.APIKeyEnv != "" {
			problems = append(problems, fmt.Sprintf("instance %q: oauth instances cannot set api_key_env", inst.Alias))
		}
	}
	for name, t := range c.Templates {
		if t.DefaultPlugins != nil {
			checkPlugins(t.DefaultPlugins, fmt.Sprintf("template %q plugins", name))
		}
	}

	if len(problems) > 0 {
		return &ValidationError{Problems: problems}
	}
	return nil
}

// ConfigManager holds the live config and supports atomic swap plus reload
// hooks (used for Phase 5 hot-reload).
type ConfigManager struct {
	mu     sync.RWMutex
	config *Config
	hooks  []func(*Config)
}

// New returns a manager holding the initial config.
func New(c *Config) *ConfigManager {
	return &ConfigManager{config: c}
}

// Get returns the current config.
func (m *ConfigManager) Get() *Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.config
}

// OnReload registers a hook invoked with the new config after each successful
// Swap.
func (m *ConfigManager) OnReload(hook func(*Config)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.hooks = append(m.hooks, hook)
}

// Swap atomically replaces the live config and runs reload hooks. Invalid
// configs are rejected with a *ValidationError and the current config is kept.
func (m *ConfigManager) Swap(c *Config) error {
	if err := Validate(c); err != nil {
		return err
	}
	c.index()

	m.mu.Lock()
	m.config = c
	hooks := append([]func(*Config){}, m.hooks...)
	m.mu.Unlock()

	for _, h := range hooks {
		h(c)
	}
	return nil
}

// Load parses the gateway.toml at path and atomically swaps it in.
func (m *ConfigManager) Load(path string) error {
	c, err := Load(path)
	if err != nil {
		return err
	}
	return m.Swap(c)
}

// Update persists the config to path via an atomic temp-file + rename
// (resolved review decision #5) and atomically swaps it in so the next request
// observes the new config with no restart. The on-disk file is rewritten from
// the in-memory config (value-level round-trip; comments/formatting of a
// hand-edited gateway.toml are not preserved).
func (m *ConfigManager) Update(path string, c *Config) error {
	if err := Validate(c); err != nil {
		return err
	}
	if err := WriteFile(path, c); err != nil {
		return err
	}
	return m.Swap(c)
}

// toRaw converts a Config back to its TOML shape (the mirror of Parse's
// rawConfig). Every instance carries its resolved alias explicitly, so the
// persisted file reflects the live routing state after auto-naming. Only
// user-defined templates are written back; built-ins are implicit and re-merged
// on load. An instance whose plugins list is unset (nil) whose template
// carries DefaultPlugins gets a copy of that seed materialized as its plugins
// line, so template defaults are recorded explicitly in the file. An
// explicitly-set instance plugins list — including an explicit empty
// `plugins = []` off-switch — is never overwritten.
func (c *Config) toRaw() *rawConfig {
	retention := c.RetentionDays
	raw := &rawConfig{
		ListenAddrs:   append([]string{}, c.ListenAddrs...),
		Store:         c.Store,
		RetentionDays: &retention,
		Settings:      c.Settings,
		Instances:     make([]Instance, len(c.Instances)),
		Plugins:       c.Plugins,
	}
	for name, t := range c.Templates {
		if c.userTemplates[name] {
			if raw.Providers == nil {
				raw.Providers = map[string]Template{}
			}
			raw.Providers[name] = *t
		}
	}
	for i, inst := range c.Instances {
		ic := *inst
		if ic.Plugins == nil {
			if t, ok := c.Templates[inst.Template]; ok && t.DefaultPlugins != nil {
				seed := append([]string{}, t.DefaultPlugins...)
				ic.Plugins = &seed
			}
		}
		raw.Instances[i] = ic
	}
	return raw
}

// Marshal serializes the config back to TOML (the write-back side of the §4.2
// config file shape).
func Marshal(c *Config) ([]byte, error) {
	return toml.Marshal(c.toRaw())
}

// Addrs returns the listen addresses to bind. fromRaw normalizes the legacy
// single `listen` key into ListenAddrs, so a parsed config always carries the
// list; Addrs only substitutes the loopback default for hand-built configs
// (tests) that set no addresses at all.
func (c *Config) Addrs() []string {
	if len(c.ListenAddrs) > 0 {
		return c.ListenAddrs
	}
	return []string{"127.0.0.1:8787"}
}

// WriteFile persists the config to path atomically: it writes a temp file in
// the same directory (mode 0600), syncs it, then renames it over the target.
// A reader either sees the old complete file or the new complete file, never a
// partial write.
func WriteFile(path string, c *Config) error {
	data, err := Marshal(c)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".gateway.toml-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		if tmpName != "" {
			os.Remove(tmpName) //nolint:errcheck // best-effort cleanup on failure
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	tmpName = ""
	return nil
}

// IsValidationError reports whether err is a config validation failure.
func IsValidationError(err error) bool {
	var ve *ValidationError
	return errors.As(err, &ve)
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
