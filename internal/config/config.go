// Package config implements the gateway.toml configuration surface: provider
// templates, per-account instances, the §4.2 alias rules, routing resolution,
// and an atomically-swappable ConfigManager for Phase 5 hot-reload.
//
// The TOML shape follows the §4.2 example exactly:
//
//	listen = "127.0.0.1:8787"
//	store  = "gateway.db"
//	retention_days = 30
//
//	[settings]
//	default_alias = "openai"
//	request_plugins = []
//	response_plugins = []
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
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/pelletier/go-toml/v2"

	"shimmer-llmgateway/internal/plugins"
)

// AliasPattern is the §4.2 alias rule: a user alias must match [a-z0-9._-]+.
const AliasPattern = `[a-z0-9._-]+`

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
}

// Template is a provider definition (a [providers.<name>] entry, the §4.2
// dropdown entry). Name is the map key, not a TOML body field, so it is
// excluded from write-back serialization.
type Template struct {
	Name      string   `toml:"-"`
	BaseURL   string   `toml:"base_url"`
	APIKeyEnv string   `toml:"api_key_env"`
	Models    []string `toml:"models"`
	Docs      string   `toml:"docs,omitempty"`
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
	Models  []string `toml:"models,omitempty"`
	Plugins []string `toml:"plugins,omitempty"`
	// Disabled excludes the instance from routing (§6.2 PATCH disable). It
	// stays visible to the API/UI for re-enabling, but Resolve never routes
	// to it.
	Disabled bool `toml:"disabled,omitempty"`
}

// Config is a parsed and validated gateway.toml.
type Config struct {
	Listen        string
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
// races with readers of the live config. The plugins tables are shared
// read-only (the API never mutates them).
func (c *Config) Clone() *Config {
	out := &Config{
		Listen:        c.Listen,
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
		tc.Models = append([]string(nil), t.Models...)
		out.Templates[name] = &tc
	}
	for i, inst := range c.Instances {
		ic := *inst
		ic.Models = append([]string(nil), inst.Models...)
		ic.Plugins = append([]string(nil), inst.Plugins...)
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

// EffectivePlugins returns the instance's plugin chain (§4.5 scope): a
// non-nil per-instance plugins list overrides the global settings defaults for
// both sides (empty list = no plugins for that account); nil (unset) falls
// back to settings.request_plugins / settings.response_plugins.
func (inst *Instance) EffectivePlugins(c *Config) (request, response []string) {
	if inst.Plugins != nil {
		return inst.Plugins, inst.Plugins
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

// Resolve routes a client model string to an instance and the model name to
// forward. Disabled instances are not routable: a prefixed model naming a
// disabled alias returns an *UnknownAliasError (listing the enabled aliases),
// and disabled instances are skipped for unprefixed resolution.
//
// Prefixed form "alias/model": the alias must exist and be enabled, otherwise
// an *UnknownAliasError carrying the available alias list is returned (the
// Phase 3 caller turns that into a 400 INVALID_ARGUMENT). The model after the
// slash is returned verbatim; prefixed routing does not check membership.
//
// Unprefixed form: resolve via settings.default_alias if that instance lists
// the model, else the first instance (in config order) that lists the model,
// else an *UnknownModelError.
func (c *Config) Resolve(model string) (*Instance, string, error) {
	if i := strings.IndexByte(model, '/'); i >= 0 {
		alias, rest := model[:i], model[i+1:]
		inst, ok := c.instancesByAlias[alias]
		if !ok || inst.Disabled {
			return nil, "", &UnknownAliasError{Alias: alias, Aliases: c.AliasList()}
		}
		return inst, rest, nil
	}
	if c.Settings.DefaultAlias != "" {
		if inst, ok := c.instancesByAlias[c.Settings.DefaultAlias]; ok && !inst.Disabled && contains(inst.EffectiveModels(c), model) {
			return inst, model, nil
		}
	}
	for _, inst := range c.Instances {
		if inst.Disabled {
			continue
		}
		if contains(inst.EffectiveModels(c), model) {
			return inst, model, nil
		}
	}
	return nil, "", &UnknownModelError{Model: model}
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

// rawConfig mirrors gateway.toml for unmarshalling.
type rawConfig struct {
	Listen        string                    `toml:"listen"`
	Store         string                    `toml:"store"`
	RetentionDays int                       `toml:"retention_days"`
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
			Models:    []string{"deepseek-chat", "deepseek-reasoner"},
			Docs:      "https://api-docs.deepseek.com",
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
		// the gateway's OpenAI-compatible forward path.
		"zai": {
			BaseURL:   "https://api.z.ai/api/paas/v4",
			APIKeyEnv: "ZAI_API_KEY",
			Models:    []string{"glm-4.6", "glm-4.5"},
			Docs:      "https://docs.z.ai/api-reference/introduction",
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
			Models:    []string{"kimi-k2", "deepseek-chat"},
			Docs:      "https://opencode.ai/docs/go/",
		},
	}
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
		Listen:        raw.Listen,
		Store:         raw.Store,
		RetentionDays: raw.RetentionDays,
		Settings:      raw.Settings,
		Templates:     tmap,
		Instances:     make([]*Instance, len(raw.Instances)),
		Plugins:       raw.Plugins,
		userTemplates: make(map[string]bool, len(raw.Providers)),
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

// Validate enforces the §4.2 alias rules and config invariants:
//   - template names match [a-z0-9._-]+ (they seed auto-named aliases);
//   - every instance references a known template;
//   - aliases are unique and match [a-z0-9._-]+;
//   - per-instance api_key_env values are valid env var names;
//   - every plugin name (settings defaults or per-instance list) is known to
//     the built-in registry (§4.5).
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
	}

	for i, inst := range c.Instances {
		if _, ok := c.Templates[inst.Template]; !ok {
			problems = append(problems, fmt.Sprintf("instance %d: unknown template %q", i+1, inst.Template))
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
		if inst.APIKeyEnv != "" && !envNameRe.MatchString(inst.APIKeyEnv) {
			problems = append(problems, fmt.Sprintf("instance %q: invalid api_key_env %q", inst.Alias, inst.APIKeyEnv))
		}
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
			checkPlugins(inst.Plugins, fmt.Sprintf("instance %q plugins", inst.Alias))
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
// on load.
func (c *Config) toRaw() *rawConfig {
	raw := &rawConfig{
		Listen:        c.Listen,
		Store:         c.Store,
		RetentionDays: c.RetentionDays,
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
		raw.Instances[i] = *inst
	}
	return raw
}

// Marshal serializes the config back to TOML (the write-back side of the §4.2
// config file shape).
func Marshal(c *Config) ([]byte, error) {
	return toml.Marshal(c.toRaw())
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
