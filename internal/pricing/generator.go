package pricing

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/amirzamli/shimmer-llmgateway/internal/config"
)

const SourceURL = "https://models.dev/api.json"

// templateProvider maps a gateway template to the models.dev provider id that
// carries its models, for providers whose catalog entry lacks an api base URL
// (so a base-URL match is impossible) or whose ids diverge (gemini → google).
var templateProvider = map[string]string{
	"openai":       "openai",
	"anthropic":    "anthropic",
	"gemini":       "google",
	"deepseek":     "deepseek",
	"kimi":         "moonshotai",
	"mistral":      "mistral",
	"groq":         "groq",
	"zai":          "zai",
	"opencode_zen": "opencode",
	"opencode_go":  "opencode-go",
	"openrouter":   "openrouter",
}

// localTemplates are self-hosted runtimes: no catalog entry exists and their
// models run free, so they are emitted as local (unpriced) providers.
var localTemplates = map[string]bool{"ollama": true, "vllm": true}

// inheritTemplates are resellers absent from the catalog: their models get
// prices inherited from the origin lab's own provider entry.
var inheritTemplates = map[string]bool{"commandcode": true}

// labProviders maps the "lab/" prefix of a forwarded model id to the catalog
// provider id of that lab, and names the providers whose own prices win over
// aggregator re-listings during the inheritance scan.
var labProviders = []string{
	"openai", "anthropic", "google", "deepseek", "moonshotai", "mistral",
	"groq", "zai", "zhipuai", "meta", "xai", "alibaba", "minimax",
	"microsoft", "nvidia",
}

// labByPrefix maps a model id prefix to its lab's catalog provider id.
var labByPrefix = map[string]string{
	"deepseek": "deepseek", "openai": "openai", "anthropic": "anthropic",
	"google": "google", "moonshot": "moonshotai", "moonshotai": "moonshotai",
	"z-ai": "zai", "zai": "zai", "zhipu": "zhipuai", "zhipuai": "zhipuai",
	"mistral": "mistral", "groq": "groq", "meta": "meta", "xai": "xai",
	"alibaba": "alibaba", "qwen": "alibaba", "minimax": "minimax",
	"microsoft": "microsoft", "nvidia": "nvidia",
}

// templateOrder fixes the provider emission order: origin labs first so
// provider-unknown bare-model lookups prefer list prices over aggregator
// markups, then resellers, then local runtimes. Unlisted templates follow in
// sorted order.
var templateOrder = []string{
	"openai", "anthropic", "gemini", "deepseek", "kimi", "mistral", "groq", "zai",
	"opencode_zen", "opencode_go", "openrouter", "commandcode", "ollama", "vllm", "lite_llm",
}

// devCost is the catalog's per-model price object (USD per 1M tokens). Tiered
// context pricing (tiers / context_over_200k) is deliberately dropped: the
// gateway records no request context size, so estimates use the flat base
// rates.
type devCost struct {
	Input      *float64 `json:"input"`
	Output     *float64 `json:"output"`
	CacheRead  *float64 `json:"cache_read"`
	CacheWrite *float64 `json:"cache_write"`
}

// devModel is one catalog model entry; cost is nil when the catalog has no
// price data (the model is emitted unpriced).
type devModel struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Cost        *devCost `json:"cost"`
	ReleaseDate string   `json:"release_date"`
}

// devProvider is one catalog provider entry.
type devProvider struct {
	ID     string              `json:"id"`
	Name   string              `json:"name"`
	API    string              `json:"api"`
	Models map[string]devModel `json:"models"`
}

// fetchSource downloads and parses the catalog.
func fetchSource(url string, timeout time.Duration) (map[string]devProvider, error) {
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: unexpected status %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	var out map[string]devProvider
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("parse %s: %w", url, err)
	}
	return out, nil
}

// orderedTemplates returns the template names in emission order.
func orderedTemplates(templates map[string]config.Template) []string {
	seen := map[string]bool{}
	var out []string
	add := func(name string) {
		if _, ok := templates[name]; ok && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	for _, name := range templateOrder {
		add(name)
	}
	var rest []string
	for name := range templates {
		if !seen[name] {
			rest = append(rest, name)
		}
	}
	sort.Strings(rest)
	return append(out, rest...)
}

// devModelToEntry converts a catalog model to a file entry. A model with no
// cost object keeps only its metadata: the runtime treats it as unpriced.
func devModelToEntry(m devModel) FileModel {
	e := FileModel{ID: m.ID, Name: m.Name}
	if m.Cost != nil {
		e.Input, e.Output = m.Cost.Input, m.Cost.Output
		e.CacheRead, e.CacheWrite = m.Cost.CacheRead, m.Cost.CacheWrite
	}
	return e
}

// catalogProviderEntry builds the file entry for a template backed by a
// catalog provider: every catalog model of that provider, sorted by id.
func catalogProviderEntry(tmpl config.Template, dev devProvider) FileProvider {
	p := FileProvider{ID: dev.ID, Name: dev.Name, BaseURL: dev.API}
	if p.BaseURL == "" {
		p.BaseURL = tmpl.BaseURL
	}
	ids := make([]string, 0, len(dev.Models))
	for id := range dev.Models {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		p.Models = append(p.Models, devModelToEntry(dev.Models[id]))
	}
	return p
}

// localProviderEntry builds the file entry for a self-hosted runtime template:
// its configured models, all local (unpriced by design).
func localProviderEntry(tmpl config.Template) FileProvider {
	p := FileProvider{ID: tmpl.Name, Name: tmpl.Name, BaseURL: tmpl.BaseURL}
	for _, id := range tmpl.Models {
		p.Models = append(p.Models, FileModel{ID: id, Local: true})
	}
	return p
}

// inheritProviderEntry builds the file entry for a reseller template absent
// from the catalog: its configured models priced from the origin lab's own
// catalog entry (matched via the "lab/" prefix, then a scan preferring lab
// providers), flagged inherited_from. Models with no catalog source are
// emitted unpriced. The entry keeps the template's verbatim model ids, so the
// runtime's exact (provider, model) lookup hits without slash fallback.
func inheritProviderEntry(tmpl config.Template, dev map[string]devProvider) FileProvider {
	p := FileProvider{ID: tmpl.Name, Name: tmpl.Name, BaseURL: tmpl.BaseURL, Inherited: true}
	for _, id := range tmpl.Models {
		e := FileModel{ID: id}
		canon := id
		if i := strings.LastIndexByte(id, '/'); i >= 0 && i+1 < len(id) {
			canon = id[i+1:]
		}
		if src, m, ok := findInherited(id, canon, dev); ok {
			cost := devModelToEntry(m)
			e.Name = cost.Name
			e.Input, e.Output = cost.Input, cost.Output
			e.CacheRead, e.CacheWrite = cost.CacheRead, cost.CacheWrite
			e.InheritedFrom = src
		}
		p.Models = append(p.Models, e)
	}
	return p
}

// findInherited locates the canonical model id in the catalog, preferring the
// lab hinted by the original id's prefix, then the known lab providers, then
// every remaining provider in sorted order. Only entries with real price data
// (input or output present) count as a source.
func findInherited(id, canon string, dev map[string]devProvider) (string, devModel, bool) {
	prefix := id
	if i := strings.LastIndexByte(id, '/'); i >= 0 {
		prefix = id[:i]
	}
	candidates := []string{}
	if lab, ok := labByPrefix[prefix]; ok {
		candidates = append(candidates, lab)
	}
	candidates = append(candidates, labProviders...)
	var rest []string
	for id := range dev {
		if !contains(candidates, id) {
			rest = append(rest, id)
		}
	}
	sort.Strings(rest)
	candidates = append(candidates, rest...)
	for _, pid := range candidates {
		p, ok := dev[pid]
		if !ok {
			continue
		}
		m, ok := p.Models[canon]
		if !ok || !hasPrice(m) {
			continue
		}
		return pid, m, true
	}
	return "", devModel{}, false
}

// hasPrice reports whether a catalog model carries usable price data.
func hasPrice(m devModel) bool {
	return m.Cost != nil && (m.Cost.Input != nil || m.Cost.Output != nil)
}

// providerByBaseURL maps a template base URL to a catalog provider id by
// normalized URL equality, for templates absent from templateProvider.
func providerByBaseURL(baseURL string, dev map[string]devProvider) string {
	norm := normalizeURL(baseURL)
	if norm == "" {
		return ""
	}
	ids := make([]string, 0, len(dev))
	for id := range dev {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if normalizeURL(dev[id].API) == norm {
			return id
		}
	}
	return ""
}

// buildTable assembles the generated provider entries for the gateway's
// built-in templates.
func buildTable(dev map[string]devProvider, templates map[string]config.Template) []*FileProvider {
	var out []*FileProvider
	for _, name := range orderedTemplates(templates) {
		tmpl := templates[name]
		switch {
		case localTemplates[name]:
			p := localProviderEntry(tmpl)
			out = append(out, &p)
		case inheritTemplates[name]:
			p := inheritProviderEntry(tmpl, dev)
			out = append(out, &p)
		default:
			id, ok := templateProvider[name]
			if !ok {
				id = providerByBaseURL(tmpl.BaseURL, dev)
			}
			if id == "" {
				continue // no catalog source; its models stay unpriced at runtime
			}
			dp, ok := dev[id]
			if !ok {
				continue
			}
			p := catalogProviderEntry(tmpl, dp)
			out = append(out, &p)
		}
	}
	return out
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

// FetchAndBuild fetches models.dev and builds a gateway snapshot.
func FetchAndBuild(source string, timeout time.Duration) (FileTable, error) {
	dev, err := fetchSource(source, timeout)
	if err != nil {
		return FileTable{}, err
	}
	return FileTable{Schema: TableSchema, Source: source, FetchedAt: time.Now().UTC().Format(time.RFC3339), Providers: buildTable(dev, config.BuiltinTemplates())}, nil
}

// WriteSnapshot atomically replaces path with a generated pricing snapshot.
func WriteSnapshot(path string, table FileTable) error {
	data, err := json.MarshalIndent(table, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".models-*.json")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err = tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Chmod(0644); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
