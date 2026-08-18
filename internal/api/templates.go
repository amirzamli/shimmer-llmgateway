package api

import (
	"encoding/json"
	"net/http"
	"sort"

	"github.com/amirzamli/shimmer-llmgateway/internal/config"
)

// templateView is the §6.2 GET /api/templates item.
type templateView struct {
	Name      string   `json:"name"`
	BaseURL   string   `json:"base_url"`
	APIKeyEnv string   `json:"api_key_env"`
	Models    []string `json:"models"`
	Docs      string   `json:"docs,omitempty"`
}

func templateViewOf(t *config.Template) templateView {
	return templateView{Name: t.Name, BaseURL: t.BaseURL, APIKeyEnv: t.APIKeyEnv, Models: t.Models, Docs: t.Docs}
}

// handleTemplatesList returns every template (built-in + user-defined) sorted
// by name, the §6.1 dropdown entries.
func (a *API) handleTemplatesList(w http.ResponseWriter, r *http.Request) {
	cfg := a.mgr.Get()
	names := make([]string, 0, len(cfg.Templates))
	for name := range cfg.Templates {
		names = append(names, name)
	}
	sort.Strings(names)
	views := make([]templateView, 0, len(names))
	for _, name := range names {
		views = append(views, templateViewOf(cfg.Templates[name]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"templates": views})
}

// templateCreateReq is the POST /api/templates body.
type templateCreateReq struct {
	Name      string   `json:"name"`
	BaseURL   string   `json:"base_url"`
	APIKeyEnv string   `json:"api_key_env"`
	Models    []string `json:"models"`
	Docs      string   `json:"docs"`
}

// handleTemplatesCreate adds a custom template. The name must match the §4.2
// pattern ([a-z0-9._-]+) and must not collide with an existing template
// (built-in or user-defined), so the dropdown never silently shadows a
// provider.
func (a *API) handleTemplatesCreate(w http.ResponseWriter, r *http.Request) {
	var req templateCreateReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		a.writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "invalid body: "+err.Error())
		return
	}
	if req.Name == "" || req.BaseURL == "" {
		a.writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "name and base_url are required")
		return
	}
	if err := config.ValidateBaseURL(req.BaseURL); err != nil {
		a.writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error())
		return
	}
	err := a.update(func(c *config.Config) error {
		if _, ok := c.Templates[req.Name]; ok {
			return badRequest("template %q already exists", req.Name)
		}
		c.Templates[req.Name] = &config.Template{
			Name:      req.Name,
			BaseURL:   req.BaseURL,
			APIKeyEnv: req.APIKeyEnv,
			Models:    req.Models,
			Docs:      req.Docs,
		}
		c.MarkUserTemplate(req.Name)
		return nil
	})
	if err != nil {
		a.writeUpdateError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, templateViewOf(a.mgr.Get().Templates[req.Name]))
}
