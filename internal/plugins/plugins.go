// Package plugins implements the §4.5 plugin seam: the Plugin interface
// exactly per spec, the Request/Response domain types (JSON payloads), a chain
// runner that applies request plugins in config order before forward and
// response plugins in config order after reassembly, and the built-in plugin
// registry (external loading is explicitly out of scope per §2).
package plugins

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Plugin is the §4.5 seam. Plugins run in config order: FilterRequest on the
// input side before forward, FilterResponse on the output side post-reassembly
// (buffer mode for streaming). Plugins mutate the payload in place via the
// domain type's Body field.
type Plugin interface {
	Name() string
	FilterRequest(ctx context.Context, req *Request) error    // input side
	FilterResponse(ctx context.Context, resp *Response) error // output side, post-reassembly
}

// Request is the gateway request payload as the plugin chain sees it: the
// client JSON body. A plugin mutates req.Body to filter the request.
type Request struct {
	Body json.RawMessage
}

// Response is the provider payload as the plugin chain sees it: for streaming,
// the fully reassembled chat.completion; for non-stream, the provider body. A
// plugin mutates resp.Body to filter the response.
type Response struct {
	Body json.RawMessage
}

// Chain applies a plugin chain in config order. A Chain with no request (or
// response) plugins is a no-op for that side.
type Chain struct {
	requestPlugins  []Plugin
	responsePlugins []Plugin
}

// NewChain returns an empty plugin chain.
func NewChain() *Chain { return &Chain{} }

// AddRequest appends a request-side plugin in config order.
func (c *Chain) AddRequest(p Plugin) { c.requestPlugins = append(c.requestPlugins, p) }

// AddResponse appends a response-side plugin in config order.
func (c *Chain) AddResponse(p Plugin) { c.responsePlugins = append(c.responsePlugins, p) }

// HasRequest reports whether the chain has request plugins (the request body
// was filtered, so request_filtered_json is populated).
func (c *Chain) HasRequest() bool { return len(c.requestPlugins) > 0 }

// HasResponse reports whether the chain has response plugins (streaming runs
// in buffer mode and response_filtered_json is populated).
func (c *Chain) HasResponse() bool { return len(c.responsePlugins) > 0 }

// FilterRequest runs the request plugins in config order.
func (c *Chain) FilterRequest(ctx context.Context, req *Request) error {
	for _, p := range c.requestPlugins {
		if err := p.FilterRequest(ctx, req); err != nil {
			return fmt.Errorf("plugin %q: %w", p.Name(), err)
		}
	}
	return nil
}

// FilterResponse runs the response plugins in config order on the reassembled
// response.
func (c *Chain) FilterResponse(ctx context.Context, resp *Response) error {
	for _, p := range c.responsePlugins {
		if err := p.FilterResponse(ctx, resp); err != nil {
			return fmt.Errorf("plugin %q: %w", p.Name(), err)
		}
	}
	return nil
}

// Applied returns the plugin names that ran, in chain order with duplicates
// removed, or nil when no plugins are configured (→ NULL plugins_applied).
func (c *Chain) Applied() []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range c.requestPlugins {
		if !seen[p.Name()] {
			seen[p.Name()] = true
			out = append(out, p.Name())
		}
	}
	for _, p := range c.responsePlugins {
		if !seen[p.Name()] {
			seen[p.Name()] = true
			out = append(out, p.Name())
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Options carries per-plugin configuration to a Factory: Config is the raw
// decoded [plugins.<name>] TOML table (patterns/field names for redact), or
// nil when the section is absent.
type Options struct {
	Config map[string]any
}

// Factory builds a plugin of one registered name. Plugins are selected by name
// from the built-in registry (external loading is v2, §2).
type Factory func(Options) (Plugin, error)

var registry = map[string]Factory{}

// Register adds a plugin factory under name. Registry entries are process-wide;
// the built-in redact plugin registers itself in an init function.
func Register(name string, f Factory) {
	if f == nil {
		panic("plugins: Register(nil factory)")
	}
	registry[name] = f
}

// Build constructs a plugin by name, applying the optional config.
func Build(name string, opts Options) (Plugin, error) {
	f, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("plugins: unknown plugin %q; available plugins: %s", name, strings.Join(Known(), ", "))
	}
	return f(opts)
}

// Known returns every valid plugin name — the registered transforms — sorted.
func Known() []string {
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// ConfigField documents one key of a plugin's [plugins.<name>] TOML table for
// the settings UI. Kind mirrors the value shape ("string_list" = list of
// strings); Defaults lists what applies when the key is unset (nil = the key
// has no effect when unset, e.g. no field-list masking).
type ConfigField struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Kind        string   `json:"kind"`
	Defaults    []string `json:"defaults,omitempty"`
}

// Info describes a known plugin for the API and the settings UI: what it does,
// where it comes from, and which config keys its [plugins.<name>] table
// accepts.
type Info struct {
	Name         string        `json:"name"`
	Description  string        `json:"description"`
	Kind         string        `json:"kind"` // "transform"
	Source       string        `json:"source"`
	Configurable bool          `json:"configurable"`
	ConfigFields []ConfigField `json:"config_fields,omitempty"`
}

// pluginInfo carries the registered metadata. Entries are added by the same
// init() functions that register the factories, so a plugin's docs can never
// drift from its registration.
var pluginInfo = map[string]Info{}

// registerInfo records one plugin's metadata. It panics on an unregistered
// name so docs and registry cannot fall out of sync.
func registerInfo(i Info) {
	if _, ok := registry[i.Name]; !ok {
		panic(fmt.Sprintf("plugins: registerInfo for unknown plugin %q", i.Name))
	}
	pluginInfo[i.Name] = i
}

// Infos returns metadata for every known plugin, sorted by name. Unregistered
// names still appear with a generic entry so the list is always complete.
func Infos() []Info {
	out := make([]Info, 0, len(registry))
	for _, n := range Known() {
		if i, ok := pluginInfo[n]; ok {
			out = append(out, i)
			continue
		}
		out = append(out, Info{Name: n, Kind: "transform", Source: "built-in"})
	}
	return out
}
