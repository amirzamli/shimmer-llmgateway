package api

import (
	"encoding/json"
	"net/http"
	"slices"

	"github.com/amirzamli/shimmer-llmgateway/internal/config"
	"github.com/amirzamli/shimmer-llmgateway/internal/plugins"
)

// pluginsInstance is the per-instance slice of the GET /api/plugins response:
// the plugin names the instance actually runs (config order, both sides
// deduped), whether that list is the instance's own override or inherited from
// the global settings defaults, and whether the instance is routable.
type pluginsInstance struct {
	Alias    string   `json:"alias"`
	Disabled bool     `json:"disabled"`
	Override bool     `json:"override"`
	Plugins  []string `json:"plugins"`
}

// handlePluginsList returns the plugin registry with metadata (description,
// kind, source, config fields), the global enablement, each plugin's current
// [plugins.<name>] config table, and the per-instance effective lists so the
// UI can show exactly where a plugin is active.
func (a *API) handlePluginsList(w http.ResponseWriter, r *http.Request) {
	cfg := a.mgr.Get()
	insts := make([]pluginsInstance, 0, len(cfg.Instances))
	for _, inst := range cfg.Instances {
		reqNames, respNames := inst.EffectivePlugins(cfg)
		seen := map[string]bool{}
		eff := []string{} // non-nil so an empty chain marshals as [], not null
		for _, name := range append(slices.Clone(reqNames), respNames...) {
			if !seen[name] {
				seen[name] = true
				eff = append(eff, name)
			}
		}
		insts = append(insts, pluginsInstance{
			Alias:    inst.Alias,
			Disabled: inst.Disabled,
			Override: inst.Plugins != nil,
			Plugins:  eff,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"plugins":          plugins.Infos(),
		"request_plugins":  nonNil(cfg.Settings.RequestPlugins),
		"response_plugins": nonNil(cfg.Settings.ResponsePlugins),
		"plugin_config":    cfg.Plugins,
		"instances":        insts,
	})
}

// pluginPatchReq is the PATCH /api/plugins/{name} body; every field is
// optional. Enabled flips the plugin in the global settings defaults (both
// request and response sides, mirroring how a per-instance list applies to
// both). Config replaces the plugin's [plugins.<name>] table ({} or absent
// keys clear it back to the plugin's built-in defaults).
type pluginPatchReq struct {
	Enabled *bool           `json:"enabled"`
	Config  *map[string]any `json:"config"`
}

func (a *API) handlePluginPatch(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !slices.Contains(plugins.Known(), name) {
		a.writeError(w, http.StatusNotFound, "NOT_FOUND", "unknown plugin: "+name)
		return
	}
	var req pluginPatchReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		a.writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "invalid body: "+err.Error())
		return
	}
	if req.Config != nil && plugins.IsControl(name) {
		a.writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "plugin "+name+" is a control plugin and takes no settings")
		return
	}
	if req.Config != nil {
		// Validate the config by building the plugin before anything is
		// persisted (e.g. redact compiles its patterns at build time).
		if _, err := plugins.Build(name, plugins.Options{Config: *req.Config}); err != nil {
			a.writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error())
			return
		}
	}

	err := a.update(func(c *config.Config) error {
		if req.Enabled != nil {
			c.Settings.RequestPlugins = withoutPlugin(c.Settings.RequestPlugins, name)
			c.Settings.ResponsePlugins = withoutPlugin(c.Settings.ResponsePlugins, name)
			if *req.Enabled {
				c.Settings.RequestPlugins = append(c.Settings.RequestPlugins, name)
				c.Settings.ResponsePlugins = append(c.Settings.ResponsePlugins, name)
			}
		}
		if req.Config != nil {
			if c.Plugins == nil {
				c.Plugins = map[string]map[string]any{}
			}
			if len(*req.Config) == 0 {
				delete(c.Plugins, name)
			} else {
				c.Plugins[name] = *req.Config
			}
		}
		return nil
	})
	if err != nil {
		a.writeUpdateError(w, err)
		return
	}
	a.writePluginState(w, name)
}

// writePluginState answers a PATCH with the plugin's post-update state.
func (a *API) writePluginState(w http.ResponseWriter, name string) {
	cfg := a.mgr.Get()
	enabled := slices.Contains(cfg.Settings.RequestPlugins, name) || slices.Contains(cfg.Settings.ResponsePlugins, name)
	writeJSON(w, http.StatusOK, map[string]any{
		"name":             name,
		"enabled":          enabled,
		"request_plugins":  nonNil(cfg.Settings.RequestPlugins),
		"response_plugins": nonNil(cfg.Settings.ResponsePlugins),
		"config":           cfg.PluginConfig(name),
	})
}

// withoutPlugin removes name from a plugin list (nil-safe), preserving order.
func withoutPlugin(list []string, name string) []string {
	out := make([]string, 0, len(list))
	for _, n := range list {
		if n != name {
			out = append(out, n)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// nonNil maps a nil string slice to an empty one so JSON answers carry []
// instead of null (the UI tolerates both; null is just ambiguous to read).
func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
