package api

import (
	"encoding/json"
	"fmt"
	"net/http"

	"shimmer-llmgateway/internal/config"
	"shimmer-llmgateway/internal/secrets"
)

// instanceView is the §6.2 instance item. KeyMasked is the UI-managed stored
// key masked for display ("sk-…abcd"), present only when a key is stored in
// the secrets file; keys from api_key_env are shown as the env var name.
type instanceView struct {
	Alias        string            `json:"alias"`
	Template     string            `json:"template"`
	APIKeyEnv    string            `json:"api_key_env"`
	Models       []string          `json:"models,omitempty"`
	Plugins      []string          `json:"plugins,omitempty"`
	ModelAliases map[string]string `json:"model_aliases,omitempty"`
	Disabled     bool              `json:"disabled"`
	KeyMasked    string            `json:"key_masked,omitempty"`
}

func (a *API) instanceViewOf(cfg *config.Config, inst *config.Instance) instanceView {
	v := instanceView{
		Alias:        inst.Alias,
		Template:     inst.Template,
		APIKeyEnv:    inst.EffectiveAPIKeyEnv(cfg),
		Models:       inst.Models,
		Plugins:      inst.Plugins,
		ModelAliases: inst.ModelAliases,
		Disabled:     inst.Disabled,
	}
	if key, ok := a.sec.Get(inst.Alias); ok {
		v.KeyMasked = secrets.MaskKey(key)
	}
	return v
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
// is stored in the secrets file (never in gateway.toml).
type instanceCreateReq struct {
	Alias     string   `json:"alias"`
	Template  string   `json:"template"`
	APIKeyEnv string   `json:"api_key_env"`
	Models    []string `json:"models"`
	Plugins   []string `json:"plugins"`
	Key       string   `json:"key"`
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

	err := a.update(func(c *config.Config) error {
		if _, ok := c.Templates[req.Template]; !ok {
			return badRequest("unknown template %q", req.Template)
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
	// ModelAliases replaces the whole model_aliases map when provided (an empty
	// map clears it); absent → unchanged. Validation runs via config.Validate
	// in ConfigManager.Update (bad key → 400 INVALID_ARGUMENT).
	ModelAliases *map[string]string `json:"model_aliases"`
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
		if req.ModelAliases != nil {
			inst.ModelAliases = *req.ModelAliases
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
