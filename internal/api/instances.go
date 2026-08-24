package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
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
	BaseURL        string            `json:"base_url"`
	APIKeyEnv      string            `json:"api_key_env"`
	Models         []string          `json:"models,omitempty"`
	Plugins        *[]string         `json:"plugins,omitempty"`
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
	}
	if key, ok := a.sec.Get(inst.Alias); ok {
		v.KeyMasked = secrets.MaskKey(key)
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
	// BaseURL supplies the provider endpoint (required for the custom_openai
	// / custom_anthropic placeholders, optional as an override for any other
	// template). It resolves to a concrete per-endpoint custom template before
	// the instance is created; see resolveCustomTemplate.
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
		a.writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "alias must match [a-z0-9._-]+")
		return
	}
	if req.BaseURL != "" {
		if err := config.ValidateBaseURL(req.BaseURL); err != nil {
			a.writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error())
			return
		}
	}

	err := a.update(func(c *config.Config) error {
		src, ok := c.Templates[req.Template]
		if !ok {
			return badRequest("unknown template %q", req.Template)
		}
		if req.BaseURL != "" {
			// A custom endpoint resolves to a concrete per-endpoint template
			// (named after the endpoint, styled after the source template) so
			// the config file records a real provider the gateway can route
			// to. The same endpoint+protocol reuses the template; a different
			// endpoint under the same generated name is refused loudly.
			name := customTemplateName(src.Style, req.BaseURL)
			if existing, ok := c.Templates[name]; ok {
				if existing.BaseURL != req.BaseURL {
					return badRequest("endpoint template %q already exists with a different base url (%s)", name, existing.BaseURL)
				}
				if existing.Style != src.Style {
					return badRequest("endpoint template %q already exists with a different protocol", name)
				}
			} else {
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
	})
	if err != nil {
		a.writeUpdateError(w, err)
		return
	}

	// The new instance is appended; its alias was auto-named by Validate if
	// the request left it empty.
	cfg := a.mgr.Get()
	inst := cfg.Instances[len(cfg.Instances)-1]
	if req.Key != "" {
		if err := a.sec.Set(inst.Alias, req.Key); err != nil {
			a.logger.Error("secrets_write_failed", map[string]any{"alias": inst.Alias, "error": err.Error()})
		}
	}
	writeJSON(w, http.StatusCreated, a.instanceViewOf(cfg, inst))
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

	var renamed bool
	var newAlias string
	err := a.update(func(c *config.Config) error {
		inst, ok := c.Instance(oldAlias)
		if !ok {
			return &apiError{status: http.StatusNotFound, code: "NOT_FOUND", message: fmt.Sprintf("instance %q not found", oldAlias)}
		}
		if req.Alias != nil && *req.Alias != oldAlias {
			alias := *req.Alias
			if alias == "" {
				return badRequest("alias cannot be empty; provide a name matching [a-z0-9._-]+")
			}
			if !aliasPatternOK(alias) {
				return badRequest("alias must match [a-z0-9._-]+")
			}
			for _, other := range c.Instances {
				if other.Alias == alias {
					return badRequest("alias %q already exists", alias)
				}
			}
			inst.Alias = alias
			renamed = true
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
		return nil
	})
	if err != nil {
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

	// Secrets follow the instance: a rename moves the stored key, a key
	// replacement overwrites it, and a key clear removes it.
	keyChange := req.Key != nil
	if renamed {
		if keyChange {
			a.sec.Delete(oldAlias) //nolint:errcheck // best effort
		} else if stored, ok := a.sec.Get(oldAlias); ok {
			a.sec.Delete(oldAlias)      //nolint:errcheck // best effort
			a.sec.Set(newAlias, stored) //nolint:errcheck // best effort
		}
	}
	if keyChange {
		alias := ifRenamed(newAlias, oldAlias)
		if *req.Key == "" {
			a.sec.Delete(alias) //nolint:errcheck // best effort
		} else {
			a.sec.Set(alias, *req.Key) //nolint:errcheck // best effort
		}
	}

	cfg := a.mgr.Get()
	inst, ok := cfg.Instance(ifRenamed(newAlias, oldAlias))
	if !ok {
		a.writeError(w, http.StatusInternalServerError, "INTERNAL", "updated instance not found")
		return
	}
	writeJSON(w, http.StatusOK, a.instanceViewOf(cfg, inst))
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
	err := a.update(func(c *config.Config) error {
		for i, inst := range c.Instances {
			if inst.Alias == alias {
				c.Instances = append(c.Instances[:i], c.Instances[i+1:]...)
				return nil
			}
		}
		return &apiError{status: http.StatusNotFound, code: "NOT_FOUND", message: fmt.Sprintf("instance %q not found", alias)}
	})
	if err != nil {
		a.writeUpdateError(w, err)
		return
	}
	a.sec.Delete(alias) //nolint:errcheck // best effort
	// Invalidate the cached provider list (and quota) so a recycled alias
	// never serves stale model or balance data after re-create.
	a.invalidateModels(alias)
	a.quota.InvalidateQuota(alias)
	w.WriteHeader(http.StatusNoContent)
}

// aliasPatternOK is a cheap client-side mirror of the §4.2 alias rule used for
// early 400s; the authoritative check runs in config.Validate.
func aliasPatternOK(alias string) bool {
	if alias == "" {
		return false
	}
	for _, c := range alias {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

// customTemplateName derives the concrete template name for a user-supplied
// endpoint: "custom-" + protocol style + "-" + host (and port when non-default)
// + first path segments, lowercased and sanitized to [a-z0-9._-]+. The same
// endpoint+protocol always maps to the same name, so repeated adds reuse the
// template instead of duplicating it.
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
