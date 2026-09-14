package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/amirzamli/shimmer-llmgateway/internal/config"
	"github.com/amirzamli/shimmer-llmgateway/internal/secrets"
)

// instanceView is the §6.2 instance item. KeyMasked is the UI-managed stored
// key masked for display ("sk-…abcd"), present only when a key is stored in
// the secrets file; keys from api_key_env are shown as the env var name.
type instanceView struct {
	Alias    string `json:"alias"`
	Template string `json:"template"`
	// Style is the template's upstream protocol ("openai"/"anthropic").
	Style string `json:"style"`
	// BaseURL is the template's endpoint (the resolved custom endpoint for
	// custom providers), for display.
	BaseURL   string   `json:"base_url"`
	APIKeyEnv string   `json:"api_key_env"`
	Models    []string `json:"models,omitempty"`
	// Plugins is the per-instance override: nil marshals as explicit null
	// (inherit the global defaults), non-nil as the explicit list.
	Plugins        *[]string         `json:"plugins"`
	ModelAliases   map[string]string `json:"model_aliases,omitempty"`
	ModelReasoning map[string]string `json:"model_reasoning,omitempty"`
	Disabled       bool              `json:"disabled"`
	Priority       int               `json:"priority,omitempty"`
	KeyMasked      string            `json:"key_masked,omitempty"`
}

func (a *API) instanceViewOf(cfg *config.Config, inst *config.Instance) instanceView {
	v := instanceView{
		Alias:          inst.Alias,
		Template:       inst.Template,
		APIKeyEnv:      inst.EffectiveAPIKeyEnv(cfg),
		Models:         inst.Models,
		Plugins:        inst.Plugins,
		ModelAliases:   inst.ModelAliases,
		ModelReasoning: inst.ModelReasoning,
		Disabled:       inst.Disabled,
		Priority:       inst.Priority,
	}
	if t, ok := cfg.Templates[inst.Template]; ok {
		v.Style = styleOf(t.Style)
		v.BaseURL = t.BaseURL
		if t.OAuth {
			v.APIKeyEnv = ""
		}
	}
	if key, ok := a.sec.Get(inst.Alias); ok {
		if _, isOAuth, err := a.sec.GetOAuth(inst.Alias); err != nil {
			a.logger.Error("oauth_record_corrupted", map[string]any{"alias": inst.Alias, "error": err.Error()})
		} else if !isOAuth {
			v.KeyMasked = secrets.MaskKey(key)
		}
		// An OAuth credential record is not an API key: it is never masked for
		// display here (the connection status surface lands with the OAuth API
		// handlers in the next phase).
	}
	return v
}

// instanceOrderReq is the PUT /api/instances/order body: the full desired
// ordering of every instance alias (enabled and disabled), first = highest
// priority. The server maps list position to a 1-based instance priority, so
// a UI drag-reorder becomes the routing order for unprefixed model names
// without reordering the file or sending N patches.
type instanceOrderReq struct {
	Aliases []string `json:"aliases"`
}

func (a *API) handleInstancesOrder(w http.ResponseWriter, r *http.Request) {
	var req instanceOrderReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		a.writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "invalid body: "+err.Error())
		return
	}
	err := a.update(func(c *config.Config) error {
		if len(req.Aliases) != len(c.Instances) {
			return badRequest("order must list every instance alias exactly once (%d aliases, have %d)", len(c.Instances), len(req.Aliases))
		}
		pos := make(map[string]int, len(req.Aliases))
		for i, alias := range req.Aliases {
			if alias == "" {
				return badRequest("alias at position %d is empty", i+1)
			}
			if _, dup := pos[alias]; dup {
				return badRequest("duplicate alias %q in order", alias)
			}
			pos[alias] = i
		}
		for _, inst := range c.Instances {
			i, ok := pos[inst.Alias]
			if !ok {
				return badRequest("order is missing instance %q", inst.Alias)
			}
			inst.Priority = i + 1
		}
		return nil
	})
	if err != nil {
		a.writeUpdateError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleInstancesList returns every instance in config order, including
// disabled ones (the Providers view shows them for re-enabling).
func (a *API) handleInstancesList(w http.ResponseWriter, r *http.Request) {
	cfg := a.mgr.Get()
	views := make([]instanceView, 0, len(cfg.Instances))
	for _, inst := range cfg.Instances {
		views = append(views, a.instanceViewOf(cfg, inst))
	}
	writeJSON(w, http.StatusOK, map[string]any{"instances": views})
}

// instanceCreateReq is the POST /api/instances body. An empty alias triggers
// server-side auto-naming per §4.2 (first instance of a template takes the
// template name, each further instance the next free name-N). An optional Key
// is stored in the secrets file (never in gateway.toml). Plugins mirrors the
// config semantics: absent/null → unset (the template's materialization seed,
// if any, is written into the file on write-back); explicit [] → durable off.
type instanceCreateReq struct {
	Alias     string    `json:"alias"`
	Template  string    `json:"template"`
	APIKeyEnv string    `json:"api_key_env"`
	Models    []string  `json:"models"`
	Plugins   *[]string `json:"plugins"`
	Key       string    `json:"key"`
	Priority  int       `json:"priority"`
	// BaseURL supplies the provider endpoint (required for the custom_openai /
	// custom_anthropic placeholders, optional as an override for any other
	// template). Before the instance is created the endpoint resolves to the
	// concrete template that serves it: an existing template with the same
	// endpoint+protocol is reused (so a custom add pointed at a built-in base
	// adopts that template instead of duplicating it); otherwise a custom
	// template is minted, named after the instance alias when one was typed
	// and after the endpoint when the alias is left blank.
	BaseURL string `json:"base_url"`
}

func (a *API) handleInstancesCreate(w http.ResponseWriter, r *http.Request) {
	var req instanceCreateReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		a.writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "invalid body: "+err.Error())
		return
	}
	if req.Template == "" {
		a.writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "template is required")
		return
	}
	if req.Alias != "" && !aliasPatternOK(req.Alias) {
		a.writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "alias must match [A-Za-z0-9_.-]+ with interior single spaces (e.g. \"My Provider\")")
		return
	}
	if req.BaseURL != "" {
		if err := config.ValidateBaseURL(req.BaseURL); err != nil {
			a.writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error())
			return
		}
	}

	a.updateMu.Lock()
	before := a.mgr.Get().Clone()
	candidate := before.Clone()
	err := func(c *config.Config) error {
		src, ok := c.Templates[req.Template]
		if !ok {
			return badRequest("unknown template %q", req.Template)
		}
		if src.OAuth && req.BaseURL != "" && req.BaseURL != config.ChatGPTCodexBaseURL {
			return badRequest("OAuth instances must use the fixed ChatGPT Codex base_url %q", config.ChatGPTCodexBaseURL)
		}
		if req.BaseURL != "" {
			// A custom endpoint resolves to the concrete template that serves
			// it, styled after the source template, so the config file records
			// a real provider the gateway can route to. Resolution order:
			//   1. an existing template already serving this exact endpoint+
			//      protocol is reused — a custom add pointed at a known base
			//      URL (e.g. a built-in provider) adopts that template and no
			//      custom-* entry is minted;
			//   2. otherwise the new template is named after the instance
			//      alias the user typed, so the Providers list shows their
			//      label instead of a mangled form of the URL;
			//   3. a blank alias (auto-named instance) falls back to the
			//      endpoint-derived custom-<style>-<host>-… name.
			name, ok := templateForEndpoint(c, src.Style, req.BaseURL)
			if !ok {
				name = req.Alias
				if name == "" {
					name = customTemplateName(src.Style, req.BaseURL)
				}
				if existing, ok := c.Templates[name]; ok {
					// The requested name already belongs to a template serving
					// a different endpoint; never shadow it — either add the
					// instance against that template or pick another alias.
					return badRequest("template %q already exists (base_url %s) — pick a different alias or add the instance against that template", name, existing.BaseURL)
				}
				c.Templates[name] = &config.Template{
					Name:    name,
					BaseURL: req.BaseURL,
					Style:   src.Style,
					Models:  append([]string{}, src.Models...),
				}
				c.MarkUserTemplate(name)
			}
			req.Template = name
		}
		if config.IsCustomTemplatePlaceholder(req.Template) {
			return badRequest("template %q requires an endpoint URL", req.Template)
		}
		if resolved := c.Templates[req.Template]; resolved != nil && resolved.OAuth && (req.APIKeyEnv != "" || req.Key != "") {
			return badRequest("OAuth instances cannot use API keys or api_key_env")
		}
		for _, inst := range c.Instances {
			if req.Alias != "" && inst.Alias == req.Alias {
				return badRequest("alias %q already exists", req.Alias)
			}
		}
		c.Instances = append(c.Instances, &config.Instance{
			Alias:     req.Alias, // empty → Validate auto-names
			Template:  req.Template,
			APIKeyEnv: req.APIKeyEnv,
			Models:    req.Models,
			Plugins:   req.Plugins,
			Priority:  req.Priority,
		})
		return nil
	}(candidate)
	if err == nil {
		err = config.Validate(candidate)
	}
	if err != nil {
		a.updateMu.Unlock()
		a.writeUpdateError(w, err)
		return
	}

	// The new instance is appended; its alias was auto-named by Validate if
	// the request left it empty.
	createdAlias := candidate.Instances[len(candidate.Instances)-1].Alias
	err = a.commitInstanceTransition([]string{createdAlias}, before, candidate, func() error {
		if req.Key == "" {
			return a.sec.Delete(createdAlias)
		}
		return a.sec.Set(createdAlias, req.Key)
	}, "failed to store the instance credential", func() {
		a.oauthDevices.PurgeInstance(createdAlias)
	})
	if err != nil {
		a.updateMu.Unlock()
		a.writeUpdateError(w, err)
		return
	}
	cfg := a.mgr.Get()
	inst, ok := cfg.Instance(createdAlias)
	if !ok {
		a.updateMu.Unlock()
		a.writeError(w, http.StatusInternalServerError, "INTERNAL", "created instance not found")
		return
	}
	view := a.instanceViewOf(cfg, inst)
	a.updateMu.Unlock()
	writeJSON(w, http.StatusCreated, view)
}

// commitInstanceTransition persists candidate config and then applies its
// credential mutation while updateMu and the affected lifecycle lock are held.
// A cleanup failure rolls config back when possible; if rollback fails, the
// committed config is fenced and the caller still receives a 500.
func (a *API) commitInstanceTransition(ids []string, before, candidate *config.Config, cleanup func() error, cleanupMessage string, purge func()) error {
	return a.oauthLife.WithTransition(ids, func() (bool, error) {
		if err := a.mgr.Update(a.path, candidate); err != nil {
			return false, err
		}
		if cleanup == nil {
			if purge != nil {
				purge()
			}
			return true, nil
		}
		if err := cleanup(); err != nil {
			if rollbackErr := a.mgr.Update(a.path, before); rollbackErr == nil {
				a.logger.Error("secrets_write_failed", map[string]any{"error": err.Error()})
				return false, &apiError{status: http.StatusInternalServerError, code: "INTERNAL", message: cleanupMessage}
			} else {
				a.logger.Error("config_rollback_failed", map[string]any{"error": rollbackErr.Error()})
			}
			for _, alias := range ids {
				a.invalidateModels(alias)
				a.quota.InvalidateQuota(alias)
			}
			if purge != nil {
				purge()
			}
			return true, &apiError{status: http.StatusInternalServerError, code: "INTERNAL", message: cleanupMessage}
		}
		if purge != nil {
			purge()
		}
		return true, nil
	})
}

// instancePatchReq is the PATCH /api/instances/{alias} body. All fields are
// optional; only the provided ones are applied.
type instancePatchReq struct {
	Alias    *string `json:"alias"`
	Disabled *bool   `json:"disabled"`
	// Priority replaces the routing priority when provided (0 clears it back
	// to file order). Absent → unchanged.
	Priority *int `json:"priority"`
	// ModelAliases replaces the whole model_aliases map when provided (an empty
	// map clears it); absent → unchanged. Validation runs via config.Validate
	// in ConfigManager.Update (bad key → 400 INVALID_ARGUMENT).
	ModelAliases *map[string]string `json:"model_aliases"`
	// ModelReasoning replaces the whole model_reasoning map when provided (an empty
	// map clears it); absent → unchanged. Validation runs via config.Validate
	// in ConfigManager.Update (bad key or invalid effort level → 400 INVALID_ARGUMENT).
	ModelReasoning *map[string]string `json:"model_reasoning"`
	// Plugins replaces the whole per-instance plugins list when provided
	// (an empty list is the durable off-switch); absent → unchanged. Names are
	// validated by config.Validate in ConfigManager.Update (unknown name →
	// 400 INVALID_ARGUMENT).
	Plugins *[]string `json:"plugins"`
	// PluginsInherit resets the instance back to inheriting the global
	// settings plugin defaults (clears the per-instance override); absent or
	// false → unchanged. Mutually exclusive with plugins in one request.
	PluginsInherit *bool `json:"plugins_inherit"`
	// Key replaces the stored key when non-empty and clears it when present
	// and empty (the UI sends "" to forget a stored key). Absent → unchanged.
	Key *string `json:"key"`
}

func (a *API) handleInstancePatch(w http.ResponseWriter, r *http.Request) {
	oldAlias := r.PathValue("alias")
	var req instancePatchReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		a.writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "invalid body: "+err.Error())
		return
	}
	keyChange := req.Key != nil
	a.updateMu.Lock()
	before := a.mgr.Get().Clone()
	candidate := before.Clone()
	renamed, newAlias, err := applyInstancePatch(candidate, oldAlias, req)
	if err == nil && keyChange {
		if inst, ok := candidate.Instance(oldAlias); ok {
			if tpl := candidate.Templates[inst.Template]; tpl != nil && tpl.OAuth {
				err = badRequest("OAuth instances cannot use API keys")
			}
		}
	}
	if err == nil && renamed {
		// Validate the complete candidate before touching lifecycle state. A
		// rejected alias/config update must not cancel a flow for the target.
		err = config.Validate(candidate)
	}
	if err != nil {
		a.updateMu.Unlock()
		a.writeUpdateError(w, err)
		return
	}

	var cleanup func() error
	ids := []string{oldAlias}
	cleanupMessage := "failed to update the stored credential"
	if renamed {
		ids = append(ids, newAlias)
		cleanupMessage = "failed to move the stored credential"
		cleanup = func() error { return a.sec.Move(oldAlias, newAlias, req.Key) }
	} else if keyChange {
		cleanup = func() error {
			if *req.Key == "" {
				return a.sec.Delete(oldAlias)
			}
			return a.sec.Set(oldAlias, *req.Key)
		}
	}
	purge := func() {
		if renamed {
			a.oauthDevices.PurgeInstance(oldAlias)
			a.oauthDevices.PurgeInstance(newAlias)
		}
	}
	if cleanup != nil {
		err = a.commitInstanceTransition(ids, before, candidate, cleanup, cleanupMessage, purge)
	} else {
		err = a.mgr.Update(a.path, candidate)
	}
	if err != nil {
		a.updateMu.Unlock()
		a.writeUpdateError(w, err)
		return
	}

	// A successful PATCH invalidates the cached provider model list (and the
	// quota cache) for the old and (if renamed) new alias so the next fetch is
	// fresh.
	a.invalidateModels(oldAlias)
	a.quota.InvalidateQuota(oldAlias)
	if renamed {
		a.invalidateModels(newAlias)
		a.quota.InvalidateQuota(newAlias)
	}

	cfg := a.mgr.Get()
	inst, ok := cfg.Instance(ifRenamed(newAlias, oldAlias))
	if !ok {
		a.updateMu.Unlock()
		a.writeError(w, http.StatusInternalServerError, "INTERNAL", "updated instance not found")
		return
	}
	view := a.instanceViewOf(cfg, inst)
	a.updateMu.Unlock()
	writeJSON(w, http.StatusOK, view)
}

// applyInstancePatch applies a validated request to a config candidate and
// reports whether it renames the instance. It performs all request-level and
// config-independent checks without touching lifecycle state.
func applyInstancePatch(c *config.Config, oldAlias string, req instancePatchReq) (bool, string, error) {
	inst, ok := c.Instance(oldAlias)
	if !ok {
		return false, "", &apiError{status: http.StatusNotFound, code: "NOT_FOUND", message: fmt.Sprintf("instance %q not found", oldAlias)}
	}
	if req.Plugins != nil && req.PluginsInherit != nil && *req.PluginsInherit {
		return false, "", badRequest("plugins and plugins_inherit are mutually exclusive")
	}
	renamed := req.Alias != nil && *req.Alias != oldAlias
	newAlias := ""
	if renamed {
		alias := *req.Alias
		if alias == "" {
			return false, "", badRequest("alias cannot be empty; provide a name matching [A-Za-z0-9_.-]+ with interior single spaces (e.g. \"My Provider\")")
		}
		if !aliasPatternOK(alias) {
			return false, "", badRequest("alias must match [A-Za-z0-9_.-]+ with interior single spaces (e.g. \"My Provider\")")
		}
		for _, other := range c.Instances {
			if other.Alias == alias {
				return false, "", badRequest("alias %q already exists", alias)
			}
		}
		inst.Alias = alias
		newAlias = alias
	}
	if req.Disabled != nil {
		inst.Disabled = *req.Disabled
	}
	if req.Priority != nil {
		inst.Priority = *req.Priority
	}
	if req.ModelAliases != nil {
		inst.ModelAliases = *req.ModelAliases
	}
	if req.ModelReasoning != nil {
		inst.ModelReasoning = *req.ModelReasoning
	}
	if req.Plugins != nil {
		ps := make([]string, len(*req.Plugins))
		copy(ps, *req.Plugins)
		inst.Plugins = &ps
	}
	if req.PluginsInherit != nil && *req.PluginsInherit {
		inst.Plugins = nil
	}
	return renamed, newAlias, nil
}

// ifRenamed returns the effective alias after a rename.
func ifRenamed(newAlias, oldAlias string) string {
	if newAlias != "" {
		return newAlias
	}
	return oldAlias
}

func (a *API) handleInstanceDelete(w http.ResponseWriter, r *http.Request) {
	alias := r.PathValue("alias")
	a.updateMu.Lock()
	before := a.mgr.Get().Clone()
	candidate := before.Clone()
	if _, ok := candidate.Instance(alias); !ok {
		a.updateMu.Unlock()
		a.writeError(w, http.StatusNotFound, "NOT_FOUND", fmt.Sprintf("instance %q not found", alias))
		return
	}
	for i, inst := range candidate.Instances {
		if inst.Alias == alias {
			candidate.Instances = append(candidate.Instances[:i], candidate.Instances[i+1:]...)
			break
		}
	}
	err := a.commitInstanceTransition([]string{alias}, before, candidate, func() error {
		return a.sec.Delete(alias)
	}, "failed to remove the stored credential", func() {
		a.oauthDevices.PurgeInstance(alias)
	})
	if err != nil {
		a.updateMu.Unlock()
		a.writeUpdateError(w, err)
		return
	}
	a.invalidateModels(alias)
	a.quota.InvalidateQuota(alias)
	a.updateMu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// aliasPatternOK is a cheap client-side mirror of the §4.2 alias rule used for
// early 400s; the authoritative check runs in config.Validate. Words of
// [A-Za-z0-9_.-] separated by single spaces; no leading/trailing space, and
// "/" stays reserved for the "alias/model" routing prefix.
func aliasPatternOK(alias string) bool {
	if alias == "" {
		return false
	}
	for _, word := range strings.Split(alias, " ") {
		if word == "" {
			return false
		}
		for _, c := range word {
			switch {
			case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
			default:
				return false
			}
		}
	}
	return true
}

// templateForEndpoint returns the name of an existing template that already
// serves baseURL with the given protocol style (an empty style meaning the
// "openai" default). Exact endpoint+protocol matches are reused so adding a
// custom endpoint never duplicates a template — in particular, a custom add
// pointed at a built-in provider's base URL resolves to that built-in. When
// several templates match, the alphabetically-first name wins for determinism.
func templateForEndpoint(c *config.Config, style, baseURL string) (string, bool) {
	if style == "" {
		style = config.StyleOpenAI
	}
	names := make([]string, 0, len(c.Templates))
	for name := range c.Templates {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		t := c.Templates[name]
		if t.BaseURL != baseURL {
			continue
		}
		s := t.Style
		if s == "" {
			s = config.StyleOpenAI
		}
		if s == style {
			return name, true
		}
	}
	return "", false
}

// customTemplateName derives a template name for a user-supplied endpoint:
// "custom-" + protocol style + "-" + host (and port when non-default) + first
// path segments, lowercased and sanitized to [a-z0-9._-]+. It is the fallback
// for the blank-alias add flow — when the user typed an alias the new custom
// template takes that alias verbatim instead (see handleInstancesCreate). The
// same endpoint+protocol always maps to the same name, so repeated blank-alias
// adds reuse the template instead of duplicating it.
func customTemplateName(style, baseURL string) string {
	if style == "" {
		style = config.StyleOpenAI
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		// Unreachable: the caller validated the URL first.
		return "custom-" + style + "-" + slugify(baseURL)
	}
	name := "custom-" + style + "-" + slugify(u.Hostname())
	if port := u.Port(); port != "" && port != "80" && port != "443" {
		name += "-" + port
	}
	segs := make([]string, 0, 2)
	for _, s := range strings.Split(strings.Trim(u.Path, "/"), "/") {
		if s == "" {
			continue
		}
		segs = append(segs, slugify(s))
		if len(segs) >= 2 {
			break
		}
	}
	if len(segs) > 0 {
		name += "-" + strings.Join(segs, "-")
	}
	return name
}

// slugify maps s to lowercase [a-z0-9._-]+ (every other rune becomes '_').
func slugify(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		}
		return '_'
	}, s)
}
