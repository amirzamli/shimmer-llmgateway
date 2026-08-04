package api

import (
	"encoding/json"
	"net/http"

	"shimmer-llmgateway/internal/config"
)

// settingsView is the §6.2 settings item. Listen and store path are read-only
// after start; the rest are patchable.
type settingsView struct {
	Listen          string   `json:"listen"`
	Store           string   `json:"store"`
	RetentionDays   int      `json:"retention_days"`
	DefaultAlias    string   `json:"default_alias"`
	RequestPlugins  []string `json:"request_plugins"`
	ResponsePlugins []string `json:"response_plugins"`
}

// settingsPatchReq is the PATCH /api/settings body; every field is optional.
// listen and store are intentionally absent (read-only after start).
type settingsPatchReq struct {
	RetentionDays   *int      `json:"retention_days"`
	DefaultAlias    *string   `json:"default_alias"`
	RequestPlugins  *[]string `json:"request_plugins"`
	ResponsePlugins *[]string `json:"response_plugins"`
}

func (a *API) handleSettingsGet(w http.ResponseWriter, r *http.Request) {
	cfg := a.mgr.Get()
	writeJSON(w, http.StatusOK, settingsView{
		Listen:          cfg.Listen,
		Store:           cfg.Store,
		RetentionDays:   cfg.RetentionDays,
		DefaultAlias:    cfg.Settings.DefaultAlias,
		RequestPlugins:  cfg.Settings.RequestPlugins,
		ResponsePlugins: cfg.Settings.ResponsePlugins,
	})
}

func (a *API) handleSettingsPatch(w http.ResponseWriter, r *http.Request) {
	var req settingsPatchReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		a.writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "invalid body: "+err.Error())
		return
	}

	var retention *int
	err := a.update(func(c *config.Config) error {
		if req.RetentionDays != nil {
			if *req.RetentionDays < 0 {
				return &apiError{status: http.StatusBadRequest, code: "INVALID_ARGUMENT", message: "retention_days cannot be negative"}
			}
			c.RetentionDays = *req.RetentionDays
		}
		if req.DefaultAlias != nil {
			if *req.DefaultAlias != "" {
				if _, ok := c.Instance(*req.DefaultAlias); !ok {
					return &apiError{status: http.StatusBadRequest, code: "INVALID_ARGUMENT", message: "default_alias must name an existing instance"}
				}
			}
			c.Settings.DefaultAlias = *req.DefaultAlias
		}
		if req.RequestPlugins != nil {
			c.Settings.RequestPlugins = *req.RequestPlugins
		}
		if req.ResponsePlugins != nil {
			c.Settings.ResponsePlugins = *req.ResponsePlugins
		}
		retention = req.RetentionDays
		return nil
	})
	if err != nil {
		a.writeUpdateError(w, err)
		return
	}

	if retention != nil {
		// The live store's purge window follows the config.
		a.store.SetRetention(*retention)
	}
	cfg := a.mgr.Get()
	writeJSON(w, http.StatusOK, settingsView{
		Listen:          cfg.Listen,
		Store:           cfg.Store,
		RetentionDays:   cfg.RetentionDays,
		DefaultAlias:    cfg.Settings.DefaultAlias,
		RequestPlugins:  cfg.Settings.RequestPlugins,
		ResponsePlugins: cfg.Settings.ResponsePlugins,
	})
}
