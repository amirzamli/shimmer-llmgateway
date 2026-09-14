package api

import (
	"encoding/json"
	"net/http"
	"sort"

	"github.com/amirzamli/shimmer-llmgateway/internal/config"
)

// templateView is the §6.2 GET /api/templates item.
type templateView struct {
	Name    string `json:"name"`
	BaseURL string `json:"base_url"`
	// Style is the upstream protocol: "openai" (default), "anthropic", or
	// "responses".
	Style     string   `json:"style"`
	APIKeyEnv string   `json:"api_key_env"`
	Models    []string `json:"models"`
	// OAuth marks the template as authenticated by the device sign-in flow
	// (the built-in chatgpt template) instead of an API key. The dashboard
	// uses it to offer the ChatGPT account surface and hide the key field;
	// ordinary API-key templates leave it unset.
	OAuth bool `json:"oauth,omitempty"`
	// ModelStyles carries per-model protocol overrides for templates such as
	// opencode_go, whose catalog mixes OpenAI chat, Responses, and Anthropic
	// Messages models on one endpoint.
	ModelStyles map[string]string `json:"model_styles,omitempty"`
	// ModelReasoningOptions advertises the valid reasoning effort levels per
	// model so the UI can render a per-model dropdown instead of one fixed
	// list. Omitted when the template carries no metadata.
	ModelReasoningOptions map[string][]string `json:"model_reasoning_options,omitempty"`
	Docs                  string              `json:"docs,omitempty"`
}

func templateViewOf(t *config.Template) templateView {
	return templateView{Name: t.Name, BaseURL: t.BaseURL, Style: styleOf(t.Style), APIKeyEnv: t.APIKeyEnv, Models: t.Models, OAuth: t.OAuth, ModelStyles: t.ModelStyles, ModelReasoningOptions: t.ModelReasoningOptions, Docs: t.Docs}
}

// styleOf normalizes an empty style to the "openai" default for the UI.
func styleOf(style string) string {
	if style == "" {
		return config.StyleOpenAI
	}
	return style
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
	Style     string   `json:"style"`
	APIKeyEnv string   `json:"api_key_env"`
	Models    []string `json:"models"`
	Docs      string   `json:"docs"`
}

// handleTemplatesCreate adds a custom template. The name must match the §4.2
// alias pattern (config.AliasPattern) and must not collide with an existing
// template (built-in or user-defined), so the dropdown never silently shadows
// a provider.
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
	if req.Style != "" && req.Style != config.StyleOpenAI && req.Style != config.StyleAnthropic && req.Style != config.StyleResponses {
		a.writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "style must be \"openai\", \"anthropic\", \"responses\", or empty")
		return
	}
	err := a.update(func(c *config.Config) error {
		if _, ok := c.Templates[req.Name]; ok {
			return badRequest("template %q already exists", req.Name)
		}
		c.Templates[req.Name] = &config.Template{
			Name:      req.Name,
			BaseURL:   req.BaseURL,
			Style:     req.Style,
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
