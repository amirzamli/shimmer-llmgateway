package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amirzamli/shimmer-llmgateway/internal/config"
	"github.com/amirzamli/shimmer-llmgateway/internal/logging"
	"github.com/amirzamli/shimmer-llmgateway/internal/plugins"
	"github.com/amirzamli/shimmer-llmgateway/internal/secrets"
	"github.com/amirzamli/shimmer-llmgateway/internal/store"
)

// apiTestTOML seeds a config with one template and one instance, matching the
// §4.2 shape.
const apiTestTOML = `
listen = "127.0.0.1:8787"
store = "gateway.db"
retention_days = 30

[settings]
default_alias = "openai"

[providers.openai]
base_url = "https://api.openai.com/v1"
api_key_env = "TEST_KEY_1"
models = ["gpt-4o", "gpt-4o-mini"]

[[instances]]
alias = "openai"
template = "openai"
api_key_env = "TEST_KEY_1"
`

// testMasterKey is a fixed 32-byte AES-256 key used by the API tests so the
// secrets file is written as an encrypted envelope deterministically.
var testMasterKey = []byte("0123456789abcdef0123456789abcdef")

// newAPITest starts the §6.2 API handler over a temp store and gateway.toml.
// masterKey is the decoded secrets master key passed to secrets.Open: a fixed
// key for the env-set path, or nil to exercise the generated-key path.
func newAPITest(t *testing.T, toml string, masterKey []byte) (*httptest.Server, *config.ConfigManager, *store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "gateway.toml")
	if err := os.WriteFile(cfgPath, []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	mgr := config.New(cfg)
	st, err := store.Open(filepath.Join(dir, "gateway.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	sec, err := secrets.Open(st.Path()+".secrets.json", masterKey)
	if err != nil {
		t.Fatalf("secrets.Open: %v", err)
	}
	apiSrv := New(mgr, cfgPath, st, sec, logging.New(io.Discard))
	gs := httptest.NewServer(apiSrv.Handler())
	t.Cleanup(gs.Close)
	return gs, mgr, st, cfgPath
}

// doJSON performs an API request and returns the status and decoded JSON body.
func doJSON(t *testing.T, gs *httptest.Server, method, path, body string) (int, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, gs.URL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// doRaw performs an API request and returns the raw response.
func doRaw(t *testing.T, gs *httptest.Server, method, path, body string) *http.Response {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, gs.URL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestTemplatesListAndCreate(t *testing.T) {
	gs, _, _, _ := newAPITest(t, apiTestTOML, testMasterKey)

	status, out := doJSON(t, gs, "GET", "/api/templates", "")
	if status != http.StatusOK {
		t.Fatalf("GET /api/templates status = %d, want 200", status)
	}
	got := map[string]bool{}
	for _, x := range out["templates"].([]any) {
		got[x.(map[string]any)["name"].(string)] = true
	}
	for _, want := range []string{"openai", "anthropic", "ollama", "groq", "vllm", "lite_llm"} {
		if !got[want] {
			t.Errorf("templates list missing builtin %q", want)
		}
	}
	// Seeded built-ins expose per-model advertised reasoning options so the
	// UI can render per-model dropdowns.
	for _, x := range out["templates"].([]any) {
		entry := x.(map[string]any)
		if entry["name"] != "deepseek" {
			continue
		}
		opts, ok := entry["model_reasoning_options"].(map[string]any)
		if !ok {
			t.Fatalf("deepseek template missing model_reasoning_options: %v", entry)
		}
		flash, ok := opts["deepseek-v4-flash"].([]any)
		if !ok || len(flash) != 3 || flash[0] != "low" || flash[2] != "max" {
			t.Errorf("deepseek-v4-flash advertised options = %v, want [low high max]", flash)
		}
	}

	status, created := doJSON(t, gs, "POST", "/api/templates", `{"name":"myprov","base_url":"https://example.com/v1","api_key_env":"","models":["m1"]}`)
	if status != http.StatusCreated {
		t.Fatalf("POST /api/templates status = %d, body %v; want 201", status, created)
	}
	if created["name"] != "myprov" {
		t.Errorf("created template name = %v, want myprov", created["name"])
	}

	status, out = doJSON(t, gs, "GET", "/api/templates", "")
	// The newly created template appears in the list.
	found := false
	for _, x := range out["templates"].([]any) {
		if x.(map[string]any)["name"] == "myprov" {
			found = true
		}
	}
	if !found {
		t.Errorf("templates list missing newly created myprov: %v", out["templates"])
	}

	// Duplicate name (built-in or user-defined) is rejected.
	status, _ = doJSON(t, gs, "POST", "/api/templates", `{"name":"openai","base_url":"https://x"}`)
	if status != http.StatusBadRequest {
		t.Errorf("duplicate template status = %d, want 400", status)
	}
	status, _ = doJSON(t, gs, "POST", "/api/templates", `{"name":"myprov","base_url":"https://y"}`)
	if status != http.StatusBadRequest {
		t.Errorf("duplicate user template status = %d, want 400", status)
	}
}

// TestTemplatesCreateRejectsBadBaseURL verifies POST /api/templates returns a
// 400 for non-http(s) base_url values instead of persisting a template whose
// base_url is unusable or unsafe (file://, gopher://, relative).
func TestTemplatesCreateRejectsBadBaseURL(t *testing.T) {
	gs, _, _, _ := newAPITest(t, apiTestTOML, testMasterKey)

	for name, body := range map[string]string{
		"file":     `{"name":"bad-file","base_url":"file:///etc/passwd"}`,
		"gopher":   `{"name":"bad-gopher","base_url":"gopher://example.com"}`,
		"relative": `{"name":"bad-relative","base_url":"example.com/v1"}`,
		"no-host":  `{"name":"bad-nohost","base_url":"https:///v1"}`,
	} {
		status, out := doJSON(t, gs, "POST", "/api/templates", body)
		if status != http.StatusBadRequest {
			t.Errorf("%s base_url: POST status = %d, want 400 (body %v)", name, status, out)
		}
		if out["code"] != "INVALID_ARGUMENT" {
			t.Errorf("%s base_url: code = %v, want INVALID_ARGUMENT", name, out["code"])
		}
	}

	// The rejected templates never reached the config.
	status, list := doJSON(t, gs, "GET", "/api/templates", "")
	if status != http.StatusOK {
		t.Fatalf("GET /api/templates status = %d", status)
	}
	for _, x := range list["templates"].([]any) {
		if n := x.(map[string]any)["name"]; n == "bad-file" || n == "bad-gopher" || n == "bad-relative" || n == "bad-nohost" {
			t.Errorf("rejected template %v persisted anyway", n)
		}
	}
}

func TestInstanceCreateAutoNamingAndKeyMasking(t *testing.T) {
	gs, _, _, cfgPath := newAPITest(t, `
[settings]
default_alias = "openai"
`, testMasterKey)

	// First instance of the openai template → alias "openai".
	status, inst := doJSON(t, gs, "POST", "/api/instances", `{"template":"openai"}`)
	if status != http.StatusCreated {
		t.Fatalf("POST instance 1 status = %d, body %v", status, inst)
	}
	if inst["alias"] != "openai" {
		t.Errorf("instance 1 alias = %v, want openai (§4.2 auto-naming)", inst["alias"])
	}

	// Second instance with a stored key → alias "openai-2", masked key shown.
	status, inst = doJSON(t, gs, "POST", "/api/instances", `{"template":"openai","key":"sk-secret-1234567890"}`)
	if status != http.StatusCreated {
		t.Fatalf("POST instance 2 status = %d, body %v", status, inst)
	}
	if inst["alias"] != "openai-2" {
		t.Errorf("instance 2 alias = %v, want openai-2", inst["alias"])
	}
	if inst["key_masked"] != "sk-…7890" {
		t.Errorf("instance 2 key_masked = %v, want sk-…7890", inst["key_masked"])
	}

	// GET /api/instances lists both, the second masked.
	status, list := doJSON(t, gs, "GET", "/api/instances", "")
	if status != http.StatusOK {
		t.Fatalf("GET /api/instances status = %d", status)
	}
	items := list["instances"].([]any)
	if len(items) != 2 {
		t.Fatalf("instances = %d, want 2", len(items))
	}

	// The stored key lives in <store>.secrets.json with mode 0600, never in
	// gateway.toml. The file is an AES-256-GCM envelope: it carries the
	// ciphertext, never the plaintext key.
	dir := filepath.Dir(cfgPath)
	secretsPath := filepath.Join(dir, "gateway.db.secrets.json")
	data, err := os.ReadFile(secretsPath)
	if err != nil {
		t.Fatalf("read secrets file: %v", err)
	}
	fi, err := os.Stat(secretsPath)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("secrets file mode = %o, want 0600", fi.Mode().Perm())
	}
	if !strings.Contains(string(data), `"ciphertext"`) {
		t.Error("secrets file is not an encrypted envelope")
	}
	if strings.Contains(string(data), "sk-secret-1234567890") {
		t.Error("secrets file contains the plaintext key")
	}
	tomlData, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(tomlData), "sk-secret-1234567890") {
		t.Error("gateway.toml contains the API key")
	}
}

func TestInstancePatchDisableAndRename(t *testing.T) {
	gs, mgr, _, _ := newAPITest(t, `
[[instances]]
alias = "a"
template = "openai"

[[instances]]
alias = "b"
template = "openai"
`, testMasterKey)

	// Rename b → c.
	status, inst := doJSON(t, gs, "PATCH", "/api/instances/b", `{"alias":"c"}`)
	if status != http.StatusOK {
		t.Fatalf("PATCH rename status = %d, body %v", status, inst)
	}
	if inst["alias"] != "c" {
		t.Errorf("renamed alias = %v, want c", inst["alias"])
	}

	// Rename onto an existing alias → 400.
	status, _ = doJSON(t, gs, "PATCH", "/api/instances/c", `{"alias":"a"}`)
	if status != http.StatusBadRequest {
		t.Errorf("collision rename status = %d, want 400", status)
	}

	// Unknown alias → 404.
	status, _ = doJSON(t, gs, "PATCH", "/api/instances/nope", `{"disabled":true}`)
	if status != http.StatusNotFound {
		t.Errorf("PATCH unknown alias status = %d, want 404", status)
	}

	// Disable a.
	status, inst = doJSON(t, gs, "PATCH", "/api/instances/a", `{"disabled":true}`)
	if status != http.StatusOK {
		t.Fatalf("PATCH disable status = %d, body %v", status, inst)
	}
	if inst["disabled"] != true {
		t.Errorf("disabled = %v, want true", inst["disabled"])
	}

	// Routing aliases exclude the disabled instance.
	cfg := mgr.Get()
	if got := strings.Join(cfg.AliasList(), ","); got != "c" {
		t.Errorf("AliasList = %q, want c (disabled a excluded)", got)
	}
	// The disabled instance is still visible to the API for re-enabling.
	status, list := doJSON(t, gs, "GET", "/api/instances", "")
	if status != http.StatusOK {
		t.Fatal(status)
	}
	if len(list["instances"].([]any)) != 2 {
		t.Errorf("instances list should still show the disabled instance")
	}
}

func TestInstancePatchKeyReplaceAndClear(t *testing.T) {
	gs, _, _, cfgPath := newAPITest(t, apiTestTOML, testMasterKey)

	// Store a key on the existing instance.
	status, inst := doJSON(t, gs, "PATCH", "/api/instances/openai", `{"key":"sk-old-1234567890"}`)
	if status != http.StatusOK {
		t.Fatalf("PATCH key status = %d, body %v", status, inst)
	}
	if inst["key_masked"] != "sk-…7890" {
		t.Errorf("key_masked = %v, want sk-…7890", inst["key_masked"])
	}

	// Replace it.
	status, inst = doJSON(t, gs, "PATCH", "/api/instances/openai", `{"key":"sk-new-abcdefghijkl"}`)
	if status != http.StatusOK {
		t.Fatalf("PATCH key replace status = %d", status)
	}
	if inst["key_masked"] != "sk-…ijkl" {
		t.Errorf("key_masked after replace = %v, want sk-…ijkl", inst["key_masked"])
	}

	// Rename moves the stored key to the new alias.
	status, inst = doJSON(t, gs, "PATCH", "/api/instances/openai", `{"alias":"main"}`)
	if status != http.StatusOK {
		t.Fatalf("PATCH rename status = %d", status)
	}
	if inst["key_masked"] != "sk-…ijkl" {
		t.Errorf("key_masked after rename = %v, want sk-…ijkl", inst["key_masked"])
	}

	// Clear the key.
	status, inst = doJSON(t, gs, "PATCH", "/api/instances/main", `{"key":""}`)
	if status != http.StatusOK {
		t.Fatalf("PATCH clear status = %d", status)
	}
	if _, ok := inst["key_masked"]; ok {
		t.Errorf("key_masked should be absent after clear, got %v", inst["key_masked"])
	}

	tomlData, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(tomlData), "sk-") {
		t.Error("gateway.toml contains an API key")
	}
}

func TestInstanceDelete(t *testing.T) {
	gs, _, _, _ := newAPITest(t, apiTestTOML, testMasterKey)

	resp := doRaw(t, gs, "DELETE", "/api/instances/openai", "")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE status = %d, want 204", resp.StatusCode)
	}
	resp.Body.Close()

	status, list := doJSON(t, gs, "GET", "/api/instances", "")
	if len(list["instances"].([]any)) != 0 {
		t.Errorf("instances after delete = %v, want empty", list["instances"])
	}
	_ = status

	status, _ = doJSON(t, gs, "DELETE", "/api/instances/openai", "")
	if status != http.StatusNotFound {
		t.Errorf("DELETE unknown alias status = %d, want 404", status)
	}
}

func TestSettingsGetAndPatch(t *testing.T) {
	gs, mgr, st, cfgPath := newAPITest(t, apiTestTOML, testMasterKey)

	status, out := doJSON(t, gs, "GET", "/api/settings", "")
	if status != http.StatusOK {
		t.Fatalf("GET /api/settings status = %d", status)
	}
	if addrs, ok := out["listen_addrs"].([]any); !ok || len(addrs) != 1 || addrs[0] != "127.0.0.1:8787" {
		t.Errorf("settings listen_addrs = %v, want [127.0.0.1:8787]", out["listen_addrs"])
	}
	if out["store"] != "gateway.db" || out["retention_days"] != float64(30) {
		t.Errorf("settings view = %v", out)
	}
	if out["default_alias"] != "openai" {
		t.Errorf("default_alias = %v, want openai", out["default_alias"])
	}

	// default_alias must name an existing instance.
	status, _ = doJSON(t, gs, "PATCH", "/api/settings", `{"default_alias":"nope"}`)
	if status != http.StatusBadRequest {
		t.Errorf("invalid default_alias status = %d, want 400", status)
	}

	status, _ = doJSON(t, gs, "PATCH", "/api/settings", `{"retention_days":7,"request_plugins":["redact"],"response_plugins":[]}`)
	if status != http.StatusOK {
		t.Fatalf("PATCH /api/settings status = %d", status)
	}

	// The live config and the persisted file agree.
	if got := mgr.Get().RetentionDays; got != 7 {
		t.Errorf("live retention_days = %d, want 7", got)
	}
	if got := strings.Join(mgr.Get().Settings.RequestPlugins, ","); got != "redact" {
		t.Errorf("live request_plugins = %v, want [redact]", got)
	}
	reloaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("reload persisted config: %v", err)
	}
	if reloaded.RetentionDays != 7 || strings.Join(reloaded.Settings.RequestPlugins, ",") != "redact" {
		t.Errorf("persisted settings = %+v", reloaded)
	}

	// The store purge window follows the config.
	stAt, err := st.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stAt.RetentionDays != 7 {
		t.Errorf("store retention = %d, want 7", stAt.RetentionDays)
	}
}

// TestPluginsListAndPatch drives GET /api/plugins and PATCH
// /api/plugins/{name}: metadata listing, global enable/disable, config
// write-back with build-time validation, and the control-plugin guard.
func TestPluginsListAndPatch(t *testing.T) {
	gs, mgr, _, cfgPath := newAPITest(t, apiTestTOML, testMasterKey)

	status, out := doJSON(t, gs, "GET", "/api/plugins", "")
	if status != http.StatusOK {
		t.Fatalf("GET /api/plugins status = %d", status)
	}
	list, ok := out["plugins"].([]any)
	if !ok || len(list) == 0 {
		t.Fatalf("plugins list = %v", out["plugins"])
	}
	// Metadata must cover every known plugin name with a description.
	for _, name := range plugins.Known() {
		found := false
		for _, raw := range list {
			p := raw.(map[string]any)
			if p["name"] == name {
				found = true
				if p["description"] == "" || p["source"] != "built-in" {
					t.Errorf("plugin %q info incomplete: %+v", name, p)
				}
			}
		}
		if !found {
			t.Errorf("plugin %q missing from GET /api/plugins", name)
		}
	}
	// redact is configurable and documents its config fields.
	var redactInfo map[string]any
	for _, raw := range list {
		p := raw.(map[string]any)
		if p["name"] == "redact" {
			redactInfo = p
		}
	}
	if redactInfo == nil || redactInfo["configurable"] != true {
		t.Fatalf("redact info = %v, want configurable", redactInfo)
	}
	if fields, ok := redactInfo["config_fields"].([]any); !ok || len(fields) != 2 {
		t.Errorf("redact config_fields = %v, want 2 fields", redactInfo["config_fields"])
	}

	// Enable: the name lands on both sides of the global chain.
	status, out = doJSON(t, gs, "PATCH", "/api/plugins/redact", `{"enabled":true}`)
	if status != http.StatusOK || out["enabled"] != true {
		t.Fatalf("enable redact: status=%d out=%v", status, out)
	}
	reqP := mgr.Get().Settings.RequestPlugins
	if len(reqP) != 1 || reqP[0] != "redact" {
		t.Errorf("request_plugins = %v, want [redact]", reqP)
	}
	if respP := mgr.Get().Settings.ResponsePlugins; len(respP) != 1 || respP[0] != "redact" {
		t.Errorf("response_plugins = %v, want [redact]", respP)
	}

	// Config write-back persists [plugins.redact] and hot-swaps.
	status, _ = doJSON(t, gs, "PATCH", "/api/plugins/redact", `{"config":{"field_names":["password","api_key"]}}`)
	if status != http.StatusOK {
		t.Fatalf("patch redact config status = %d", status)
	}
	if cfg := mgr.Get().PluginConfig("redact"); cfg == nil || len(cfg) != 1 {
		t.Errorf("live redact config = %v", mgr.Get().PluginConfig("redact"))
	}
	reloaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("reload persisted config: %v", err)
	}
	if cfg := reloaded.PluginConfig("redact"); cfg == nil || cfg["field_names"] == nil {
		t.Errorf("persisted redact config = %v", reloaded.PluginConfig("redact"))
	}

	// An invalid pattern is rejected at build time and nothing is persisted.
	status, _ = doJSON(t, gs, "PATCH", "/api/plugins/redact", `{"config":{"patterns":["([bad"]}}`)
	if status != http.StatusBadRequest {
		t.Errorf("invalid redact pattern status = %d, want 400", status)
	}

	// Control plugins take no settings; unknown names 404.
	status, _ = doJSON(t, gs, "PATCH", "/api/plugins/nope", `{"config":{"x":1}}`)
	if status != http.StatusNotFound {
		t.Errorf("unknown plugin config status = %d, want 404", status)
	}
	status, _ = doJSON(t, gs, "PATCH", "/api/plugins/nope", `{"enabled":true}`)
	if status != http.StatusNotFound {
		t.Errorf("unknown plugin status = %d, want 404", status)
	}

	// Disable: both sides return to empty.
	status, _ = doJSON(t, gs, "PATCH", "/api/plugins/redact", `{"enabled":false}`)
	if status != http.StatusOK {
		t.Fatalf("disable redact status = %d", status)
	}
	if len(mgr.Get().Settings.RequestPlugins) != 0 || len(mgr.Get().Settings.ResponsePlugins) != 0 {
		t.Errorf("after disable: req=%v resp=%v", mgr.Get().Settings.RequestPlugins, mgr.Get().Settings.ResponsePlugins)
	}
}

// TestSettingsLogPayloads covers the log_payloads opt-in on the settings
// surface: GET exposes it and PATCH persists it.
func TestSettingsLogPayloads(t *testing.T) {
	gs, mgr, _, cfgPath := newAPITest(t, apiTestTOML, testMasterKey)

	status, out := doJSON(t, gs, "PATCH", "/api/settings", `{"log_payloads":true}`)
	if status != http.StatusOK {
		t.Fatalf("PATCH /api/settings status = %d", status)
	}
	if out["log_payloads"] != true || !mgr.Get().Settings.LogPayloads {
		t.Errorf("log_payloads = %v / live = %v, want true", out["log_payloads"], mgr.Get().Settings.LogPayloads)
	}
	reloaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("reload persisted config: %v", err)
	}
	if !reloaded.Settings.LogPayloads {
		t.Errorf("persisted log_payloads = false, want true")
	}
}

func TestSessionsReadOnly(t *testing.T) {
	gs, _, st, _ := newAPITest(t, apiTestTOML, testMasterKey)

	// Seed two requests in one session directly in the store (the UI is
	// read-only for traces; the gateway captures them).
	sessID := "sess-api-test"
	for i := 0; i < 2; i++ {
		rec := &store.CaptureRecord{
			SessionID:    sessID,
			CreatedAt:    time.Now().Add(-time.Duration(i) * time.Second),
			Alias:        "openai",
			Provider:     "openai",
			Model:        "gpt-4o",
			Endpoint:     "/v1/chat/completions",
			DurationMS:   12,
			StatusCode:   200,
			FinishReason: "stop",
			Usage:        json.RawMessage(`{"total_tokens":5}`),
			RequestJSON:  json.RawMessage(`{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}`),
			ResponseJSON: json.RawMessage(`{"choices":[{"message":{"role":"assistant","content":"hi"}}]}`),
		}
		if err := st.Capture(context.Background(), rec); err != nil {
			t.Fatalf("capture %d: %v", i, err)
		}
	}

	// List with text search.
	status, out := doJSON(t, gs, "GET", "/api/sessions?q=hello", "")
	if status != http.StatusOK {
		t.Fatalf("GET /api/sessions status = %d", status)
	}
	sessions := out["sessions"].([]any)
	if len(sessions) != 1 {
		t.Fatalf("sessions = %d, want 1", len(sessions))
	}
	sum := sessions[0].(map[string]any)
	if sum["id"] != sessID || sum["request_count"] != float64(2) {
		t.Errorf("session summary = %v", sum)
	}

	// Detail.
	status, detail := doJSON(t, gs, "GET", "/api/sessions/"+sessID, "")
	if status != http.StatusOK {
		t.Fatalf("GET /api/sessions/{id} status = %d", status)
	}
	if len(detail["requests"].([]any)) != 2 {
		t.Errorf("detail requests = %v", detail["requests"])
	}
	if len(detail["tool_calls"].([]any)) != 0 {
		t.Errorf("detail tool_calls = %v, want empty", detail["tool_calls"])
	}

	// Export: §8 JSONL lines.
	resp := doRaw(t, gs, "GET", "/api/sessions/"+sessID+"/export", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("export status = %d", resp.StatusCode)
	}
	lines := strings.Split(strings.TrimSpace(readBody(t, resp)), "\n")
	if len(lines) != 3 { // session_start + request + request
		t.Errorf("export lines = %d, want 3: %v", len(lines), lines)
	}
	var first struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil || first.Type != "session_start" {
		t.Errorf("export first line = %q, want session_start", lines[0])
	}

	// Unknown session → 404 JSON.
	status, _ = doJSON(t, gs, "GET", "/api/sessions/nope", "")
	if status != http.StatusNotFound {
		t.Errorf("GET unknown session status = %d, want 404", status)
	}
}

func TestStatus(t *testing.T) {
	gs, _, _, _ := newAPITest(t, apiTestTOML, testMasterKey)

	status, out := doJSON(t, gs, "GET", "/api/status", "")
	if status != http.StatusOK {
		t.Fatalf("GET /api/status status = %d", status)
	}
	if out["store_path"] == nil || out["session_count"] == nil {
		t.Errorf("status missing store fields: %v", out)
	}
	if out["uptime_seconds"].(float64) < 0 {
		t.Errorf("uptime_seconds = %v, want >= 0", out["uptime_seconds"])
	}
	plugins := out["plugins"].([]any)
	found := false
	for _, p := range plugins {
		if p == "redact" {
			found = true
		}
	}
	if !found {
		t.Errorf("status plugins = %v, want redact listed", plugins)
	}
	if !strings.Contains(out["aliases"].([]any)[0].(string), "openai") {
		t.Errorf("status aliases = %v, want openai", out["aliases"])
	}
}

func TestUnknownAPIPathIsJSON404(t *testing.T) {
	gs, _, _, _ := newAPITest(t, apiTestTOML, testMasterKey)
	resp := doRaw(t, gs, "GET", "/api/not-a-real-endpoint", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown api path status = %d, want 404", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("unknown api path content-type = %q, want JSON", ct)
	}
}

func TestConcurrentMutationsAllApplied(t *testing.T) {
	// Config mutations are serialized (read-clone-modify-swap under a mutex);
	// firing concurrent POSTs must not silently drop any of them.
	gs, _, _, _ := newAPITest(t, `
[[instances]]
alias = "openai"
template = "openai"
`, testMasterKey)
	const n = 8
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, err := http.Post(gs.URL+"/api/instances", "application/json",
				strings.NewReader(`{"template":"ollama","alias":"inst-`+strconv.Itoa(i)+`"}`))
			if err != nil {
				errs <- err
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusCreated {
				errs <- fmt.Errorf("POST instance %d status = %d", i, resp.StatusCode)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	// The initial instance plus all 8 concurrent additions are present.
	status, list := doJSON(t, gs, "GET", "/api/instances", "")
	if status != http.StatusOK {
		t.Fatalf("GET /api/instances status = %d", status)
	}
	if got := len(list["instances"].([]any)); got != n+1 {
		t.Errorf("instances = %d, want %d (lost updates)", got, n+1)
	}
}

// TestMasterKeyGeneratedFlow covers the generated-key lifecycle: the endpoint
// exposes a valid base64 key exactly once, ACK clears it (404 after), and ACK
// stays idempotent (204 on repeated POSTs).
func TestMasterKeyGeneratedFlow(t *testing.T) {
	gs, _, _, _ := newAPITest(t, apiTestTOML, nil)

	resp := doRaw(t, gs, "GET", "/api/secrets/master-key", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET master-key status = %d, want 200", resp.StatusCode)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	if p := resp.Header.Get("Pragma"); p != "no-cache" {
		t.Errorf("Pragma = %q, want no-cache", p)
	}
	var body struct {
		Generated bool   `json:"generated"`
		Key       string `json:"key"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode master-key body: %v", err)
	}
	resp.Body.Close()
	if !body.Generated {
		t.Error("generated = false, want true")
	}
	raw, err := base64.StdEncoding.DecodeString(body.Key)
	if err != nil || len(raw) != 32 {
		t.Errorf("key = %q is not base64 of 32 bytes", body.Key)
	}

	// ACK → 204; the key is gone (404); repeated ACK is still 204.
	status, _ := doJSON(t, gs, "POST", "/api/secrets/master-key/ack", "")
	if status != http.StatusNoContent {
		t.Fatalf("POST ack status = %d, want 204", status)
	}
	status, out := doJSON(t, gs, "GET", "/api/secrets/master-key", "")
	if status != http.StatusNotFound {
		t.Errorf("GET master-key after ack status = %d, want 404", status)
	}
	if out["code"] != "NOT_FOUND" {
		t.Errorf("GET master-key after ack code = %v, want NOT_FOUND", out["code"])
	}
	if out["key"] != nil {
		t.Errorf("GET master-key after ack must never return a key, got %v", out["key"])
	}
	status, _ = doJSON(t, gs, "POST", "/api/secrets/master-key/ack", "")
	if status != http.StatusNoContent {
		t.Errorf("repeated ack status = %d, want 204 (idempotent)", status)
	}
}

// TestMasterKeyProvidedKeyNotExposed verifies the env-set path: a user-provided
// key is never exposed, so GET returns 404.
func TestMasterKeyProvidedKeyNotExposed(t *testing.T) {
	gs, _, _, _ := newAPITest(t, apiTestTOML, testMasterKey)

	status, out := doJSON(t, gs, "GET", "/api/secrets/master-key", "")
	if status != http.StatusNotFound {
		t.Fatalf("GET master-key with provided key status = %d, want 404", status)
	}
	if out["code"] != "NOT_FOUND" {
		t.Errorf("GET master-key code = %v, want NOT_FOUND", out["code"])
	}
	if out["key"] != nil {
		t.Errorf("GET master-key must never return a user-provided key, got %v", out["key"])
	}
}

// TestMasterKeyLoopbackGuard verifies the master-key surface rejects requests
// from non-loopback remote addresses with 403 (defense in depth for
// non-loopback listen_addrs deployments) while loopback sources are
// unaffected. The handlers are invoked directly with a crafted RemoteAddr, so
// no sockets are bound.
func TestMasterKeyLoopbackGuard(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "gateway.toml")
	if err := os.WriteFile(cfgPath, []byte(apiTestTOML), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	mgr := config.New(cfg)
	st, err := store.Open(filepath.Join(dir, "gateway.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	// nil master key → generated-key path, so a loopback GET would be 200.
	sec, err := secrets.Open(st.Path()+".secrets.json", nil)
	if err != nil {
		t.Fatalf("secrets.Open: %v", err)
	}
	apiSrv := New(mgr, cfgPath, st, sec, logging.New(io.Discard))

	for _, addr := range []string{"127.0.0.1:4321", "[::1]:4321", "localhost:4321", "127.8.8.8:99"} {
		req := httptest.NewRequest("GET", "/api/secrets/master-key", nil)
		req.RemoteAddr = addr
		rec := httptest.NewRecorder()
		apiSrv.handleMasterKeyGet(rec, req)
		if rec.Code == http.StatusForbidden {
			t.Errorf("GET master-key from %q denied; want allowed", addr)
		}
	}

	for _, addr := range []string{"203.0.113.5:4321", "10.0.0.9:4321", "192.168.1.7:4321", "8.8.8.8", ""} {
		req := httptest.NewRequest("GET", "/api/secrets/master-key", nil)
		req.RemoteAddr = addr
		rec := httptest.NewRecorder()
		apiSrv.handleMasterKeyGet(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("GET master-key from %q status = %d, want 403", addr, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "FORBIDDEN") {
			t.Errorf("GET master-key 403 body = %q, want code FORBIDDEN", rec.Body.String())
		}

		req = httptest.NewRequest("POST", "/api/secrets/master-key/ack", nil)
		req.RemoteAddr = addr
		rec = httptest.NewRecorder()
		apiSrv.handleMasterKeyAck(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("POST master-key/ack from %q status = %d, want 403", addr, rec.Code)
		}
	}
}

// TestMasterKeyLegacyMigrationOnAck covers the generated-key + legacy path: the
// file stays plaintext on disk while the generated key is exposed, ACK encrypts
// it in place, and the key is no longer exposed afterwards.
func TestMasterKeyLegacyMigrationOnAck(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "gateway.toml")
	if err := os.WriteFile(cfgPath, []byte(apiTestTOML), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	mgr := config.New(cfg)
	st, err := store.Open(filepath.Join(dir, "gateway.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	// A legacy plaintext secrets file is pre-written before the store opens.
	secretsPath := st.Path() + ".secrets.json"
	legacy := "{\n  \"openai\": \"sk-legacy-1234567890\"\n}\n"
	if err := os.WriteFile(secretsPath, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	sec, err := secrets.Open(secretsPath, nil)
	if err != nil {
		t.Fatalf("secrets.Open(nil): %v", err)
	}
	if !sec.PendingMigration() {
		t.Error("PendingMigration = false, want true for legacy file with generated key")
	}
	apiSrv := New(mgr, cfgPath, st, sec, logging.New(io.Discard))
	gs := httptest.NewServer(apiSrv.Handler())
	t.Cleanup(gs.Close)

	// The generated key is exposed while the file is still plaintext on disk.
	status, out := doJSON(t, gs, "GET", "/api/secrets/master-key", "")
	if status != http.StatusOK {
		t.Fatalf("GET master-key status = %d, want 200", status)
	}
	if out["generated"] != true {
		t.Errorf("generated = %v, want true", out["generated"])
	}
	if _, err := base64.StdEncoding.DecodeString(out["key"].(string)); err != nil {
		t.Errorf("key is not valid base64: %v", err)
	}
	onDisk, err := os.ReadFile(secretsPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(onDisk), `"ciphertext"`) {
		t.Error("file was migrated before ACK; expected plaintext on disk")
	}

	// ACK persists the encrypted envelope; the key is no longer exposed.
	status, _ = doJSON(t, gs, "POST", "/api/secrets/master-key/ack", "")
	if status != http.StatusNoContent {
		t.Fatalf("POST ack status = %d, want 204", status)
	}
	onDisk, err = os.ReadFile(secretsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(onDisk), `"ciphertext"`) {
		t.Error("file is not an encrypted envelope after ACK")
	}
	if strings.Contains(string(onDisk), "sk-legacy-1234567890") {
		t.Error("secrets file still contains the plaintext legacy key")
	}
	status, _ = doJSON(t, gs, "GET", "/api/secrets/master-key", "")
	if status != http.StatusNotFound {
		t.Errorf("GET master-key after ack status = %d, want 404", status)
	}
}

// ---- provider model fetch (GET /api/instances/{alias}/models) ----

// newModelsServer starts an httptest provider serving the OpenAI /models shape
// and counts how many times it was hit (the fake provider records fetch
// attempts so cache behavior is observable).
func newModelsServer(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// modelsTestTOML seeds one openai template/instance whose base_url points at
// the fake provider. keyEnv is the api_key_env used for both the template and
// the instance ("" for a keyless instance).
func modelsTestTOML(baseURL, keyEnv string) string {
	return fmt.Sprintf(`
listen = "127.0.0.1:8787"
store = "gateway.db"
retention_days = 30

[settings]
default_alias = "openai"

[providers.openai]
base_url = %q
api_key_env = %q
models = ["gpt-4o", "gpt-4o-mini"]

[[instances]]
alias = "openai"
template = "openai"
api_key_env = %q
`, baseURL, keyEnv, keyEnv)
}

func TestInstanceModelsFetchSuccess(t *testing.T) {
	var gotAuth string
	srv, _ := newModelsServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"id":"gpt-4o"},{"id":"gpt-4o-mini"},{"id":"  spaced-id  "}]}`)
	})
	t.Setenv("TEST_KEY_1", "sk-test-key")
	gs, _, _, _ := newAPITest(t, modelsTestTOML(srv.URL, "TEST_KEY_1"), testMasterKey)

	status, out := doJSON(t, gs, "GET", "/api/instances/openai/models", "")
	if status != http.StatusOK {
		t.Fatalf("GET models status = %d, body %v", status, out)
	}
	if gotAuth != "Bearer sk-test-key" {
		t.Errorf("Authorization = %q, want Bearer sk-test-key", gotAuth)
	}
	if out["alias"] != "openai" {
		t.Errorf("alias = %v, want openai", out["alias"])
	}
	if out["source"] != "provider" {
		t.Errorf("source = %v, want provider", out["source"])
	}
	if out["error"] != nil {
		t.Errorf("error = %v, want absent on success", out["error"])
	}
	if _, ok := out["fetched_at"]; !ok {
		t.Error("fetched_at missing")
	}
	models, ok := out["models"].([]any)
	if !ok || len(models) != 3 {
		t.Fatalf("models = %v, want 3 entries", out["models"])
	}
	if models[0] != "gpt-4o" || models[1] != "gpt-4o-mini" || models[2] != "spaced-id" {
		t.Errorf("models = %v, want [gpt-4o gpt-4o-mini spaced-id] (ids trimmed)", models)
	}
}

func TestInstanceModelsKeylessNoAuth(t *testing.T) {
	var sawAuth bool
	srv, _ := newModelsServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			sawAuth = true
		}
		fmt.Fprint(w, `{"data":[{"id":"llama3.1"}]}`)
	})
	gs, _, _, _ := newAPITest(t, modelsTestTOML(srv.URL, ""), testMasterKey)

	status, out := doJSON(t, gs, "GET", "/api/instances/openai/models", "")
	if status != http.StatusOK {
		t.Fatalf("GET models status = %d, body %v", status, out)
	}
	if sawAuth {
		t.Error("keyless fetch sent an Authorization header")
	}
	if out["source"] != "provider" {
		t.Errorf("source = %v, want provider (keyless fetch succeeded)", out["source"])
	}
	if out["error"] != nil {
		t.Errorf("error = %v, want absent (keyless-only skip is not an error)", out["error"])
	}
	if models := out["models"].([]any); len(models) != 1 || models[0] != "llama3.1" {
		t.Errorf("models = %v, want [llama3.1]", out["models"])
	}
}

func TestInstanceModelsFallbackOnProviderError(t *testing.T) {
	srv, _ := newModelsServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":"boom"}`)
	})
	t.Setenv("TEST_KEY_1", "sk-test-key")
	gs, _, _, _ := newAPITest(t, modelsTestTOML(srv.URL, "TEST_KEY_1"), testMasterKey)

	status, out := doJSON(t, gs, "GET", "/api/instances/openai/models", "")
	if status != http.StatusOK {
		t.Fatalf("GET models status = %d, body %v", status, out)
	}
	if out["source"] != "config" {
		t.Errorf("source = %v, want config", out["source"])
	}
	if out["error"] != "provider models unavailable" {
		t.Errorf("error = %v, want %q", out["error"], "provider models unavailable")
	}
	models := out["models"].([]any)
	if len(models) != 2 || models[0] != "gpt-4o" || models[1] != "gpt-4o-mini" {
		t.Errorf("models = %v, want configured [gpt-4o gpt-4o-mini]", out["models"])
	}
}

func TestInstanceModelsFallbackOnUnparseable(t *testing.T) {
	srv, _ := newModelsServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `this is not json`)
	})
	t.Setenv("TEST_KEY_1", "sk-test-key")
	gs, _, _, _ := newAPITest(t, modelsTestTOML(srv.URL, "TEST_KEY_1"), testMasterKey)

	status, out := doJSON(t, gs, "GET", "/api/instances/openai/models", "")
	if status != http.StatusOK {
		t.Fatalf("GET models status = %d, body %v", status, out)
	}
	if out["source"] != "config" {
		t.Errorf("source = %v, want config", out["source"])
	}
	if out["error"] != "provider models unavailable" {
		t.Errorf("error = %v, want %q", out["error"], "provider models unavailable")
	}
	if models := out["models"].([]any); len(models) != 2 || models[0] != "gpt-4o" {
		t.Errorf("models = %v, want configured [gpt-4o gpt-4o-mini]", out["models"])
	}
}

func TestInstanceModelsUnknownAlias404(t *testing.T) {
	gs, _, _, _ := newAPITest(t, apiTestTOML, testMasterKey)

	status, out := doJSON(t, gs, "GET", "/api/instances/nope/models", "")
	if status != http.StatusNotFound {
		t.Errorf("GET unknown alias models status = %d, want 404", status)
	}
	if out["code"] != "NOT_FOUND" {
		t.Errorf("code = %v, want NOT_FOUND", out["code"])
	}
}

func TestInstanceModelsCacheAndRefresh(t *testing.T) {
	srv, hits := newModelsServer(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"m1"}]}`)
	})
	t.Setenv("TEST_KEY_1", "sk-test-key")
	gs, _, _, _ := newAPITest(t, modelsTestTOML(srv.URL, "TEST_KEY_1"), testMasterKey)

	status, out := doJSON(t, gs, "GET", "/api/instances/openai/models", "")
	if status != http.StatusOK {
		t.Fatalf("GET models status = %d", status)
	}
	if hits.Load() != 1 {
		t.Fatalf("hits after first GET = %d, want 1", hits.Load())
	}

	// A second GET within the TTL is served from cache: the provider is not hit.
	status, out = doJSON(t, gs, "GET", "/api/instances/openai/models", "")
	if status != http.StatusOK {
		t.Fatalf("GET models (cached) status = %d", status)
	}
	if hits.Load() != 1 {
		t.Errorf("hits after second GET = %d, want 1 (cached)", hits.Load())
	}
	if out["source"] != "provider" {
		t.Errorf("cached source = %v, want provider", out["source"])
	}

	// ?refresh=1 bypasses the cache.
	status, _ = doJSON(t, gs, "GET", "/api/instances/openai/models?refresh=1", "")
	if status != http.StatusOK {
		t.Fatalf("GET models (refresh) status = %d", status)
	}
	if hits.Load() != 2 {
		t.Errorf("hits after refresh = %d, want 2", hits.Load())
	}
}

func TestInstanceModelsPatchInvalidatesCache(t *testing.T) {
	srv, hits := newModelsServer(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"m1"}]}`)
	})
	t.Setenv("TEST_KEY_1", "sk-test-key")
	gs, _, _, _ := newAPITest(t, modelsTestTOML(srv.URL, "TEST_KEY_1"), testMasterKey)

	if status, _ := doJSON(t, gs, "GET", "/api/instances/openai/models", ""); status != http.StatusOK {
		t.Fatalf("GET models status = %d", status)
	}
	if hits.Load() != 1 {
		t.Fatalf("hits after first GET = %d, want 1", hits.Load())
	}

	// A successful PATCH invalidates the cache; a disabled instance is still
	// served by the models endpoint, and it refetches.
	status, _ := doJSON(t, gs, "PATCH", "/api/instances/openai", `{"disabled":true}`)
	if status != http.StatusOK {
		t.Fatalf("PATCH status = %d, want 200", status)
	}
	if status, _ := doJSON(t, gs, "GET", "/api/instances/openai/models", ""); status != http.StatusOK {
		t.Fatalf("GET models after PATCH status = %d", status)
	}
	if hits.Load() != 2 {
		t.Errorf("hits after PATCH = %d, want 2 (cache invalidated)", hits.Load())
	}

	// A rename invalidates both the old and the new alias: fetching the new
	// alias refetches rather than serving stale cache.
	status, _ = doJSON(t, gs, "PATCH", "/api/instances/openai", `{"alias":"main"}`)
	if status != http.StatusOK {
		t.Fatalf("PATCH rename status = %d, want 200", status)
	}
	if status, _ := doJSON(t, gs, "GET", "/api/instances/main/models", ""); status != http.StatusOK {
		t.Fatalf("GET renamed models status = %d", status)
	}
	if hits.Load() != 3 {
		t.Errorf("hits after rename = %d, want 3 (rename invalidated cache)", hits.Load())
	}
}

func TestInstanceModelsDeleteInvalidatesCache(t *testing.T) {
	srv, hits := newModelsServer(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"m1"}]}`)
	})
	t.Setenv("TEST_KEY_1", "sk-test-key")
	gs, _, _, _ := newAPITest(t, modelsTestTOML(srv.URL, "TEST_KEY_1"), testMasterKey)

	if status, _ := doJSON(t, gs, "GET", "/api/instances/openai/models", ""); status != http.StatusOK {
		t.Fatalf("GET models status = %d", status)
	}
	if hits.Load() != 1 {
		t.Fatalf("hits after first GET = %d, want 1", hits.Load())
	}

	resp := doRaw(t, gs, "DELETE", "/api/instances/openai", "")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE status = %d, want 204", resp.StatusCode)
	}
	resp.Body.Close()

	// Re-creating the alias must refetch, never serve the stale cached list.
	status, inst := doJSON(t, gs, "POST", "/api/instances", `{"template":"openai"}`)
	if status != http.StatusCreated {
		t.Fatalf("POST re-create status = %d, body %v", status, inst)
	}
	if status, out := doJSON(t, gs, "GET", "/api/instances/openai/models", ""); status != http.StatusOK {
		t.Fatalf("GET re-created models status = %d, body %v", status, out)
	}
	if hits.Load() != 2 {
		t.Errorf("hits after re-create = %d, want 2 (delete invalidated cache)", hits.Load())
	}
}

func TestInstancePatchModelAliasesPersists(t *testing.T) {
	gs, _, _, cfgPath := newAPITest(t, apiTestTOML, testMasterKey)

	status, inst := doJSON(t, gs, "PATCH", "/api/instances/openai", `{"model_aliases":{"small":"gpt-4o-mini"}}`)
	if status != http.StatusOK {
		t.Fatalf("PATCH model_aliases status = %d, body %v", status, inst)
	}
	aliases, ok := inst["model_aliases"].(map[string]any)
	if !ok || aliases["small"] != "gpt-4o-mini" {
		t.Errorf("response model_aliases = %v, want {small: gpt-4o-mini}", inst["model_aliases"])
	}

	// The map is visible on the instance list and persisted to gateway.toml.
	status, list := doJSON(t, gs, "GET", "/api/instances", "")
	if status != http.StatusOK {
		t.Fatalf("GET instances status = %d", status)
	}
	item := list["instances"].([]any)[0].(map[string]any)
	if got := item["model_aliases"].(map[string]any); got["small"] != "gpt-4o-mini" {
		t.Errorf("list model_aliases = %v, want {small: gpt-4o-mini}", item["model_aliases"])
	}
	reloaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("reload persisted config: %v", err)
	}
	if got := reloaded.Instances[0].ModelAliases["small"]; got != "gpt-4o-mini" {
		t.Errorf("persisted model_aliases = %v, want small→gpt-4o-mini", reloaded.Instances[0].ModelAliases)
	}

	// Whole-map replacement: a new map replaces the old entirely.
	status, inst = doJSON(t, gs, "PATCH", "/api/instances/openai", `{"model_aliases":{"large":"gpt-4o"}}`)
	if status != http.StatusOK {
		t.Fatalf("PATCH replace status = %d, body %v", status, inst)
	}
	aliases, _ = inst["model_aliases"].(map[string]any)
	if len(aliases) != 1 || aliases["large"] != "gpt-4o" {
		t.Errorf("replace model_aliases = %v, want only {large: gpt-4o}", inst["model_aliases"])
	}
	if _, present := aliases["small"]; present {
		t.Errorf("replace kept stale key small: %v", inst["model_aliases"])
	}

	// An empty map clears the field (omitempty drops it from the view).
	status, inst = doJSON(t, gs, "PATCH", "/api/instances/openai", `{"model_aliases":{}}`)
	if status != http.StatusOK {
		t.Fatalf("PATCH clear status = %d, body %v", status, inst)
	}
	if _, ok := inst["model_aliases"]; ok {
		t.Errorf("model_aliases should be absent after clearing, got %v", inst["model_aliases"])
	}
}

func TestInstancePatchPluginsPersists(t *testing.T) {
	gs, _, _, cfgPath := newAPITest(t, apiTestTOML, testMasterKey)

	// Enable sanitize_tools on the instance: the response reflects it and the
	// config file records the plugins list.
	status, inst := doJSON(t, gs, "PATCH", "/api/instances/openai", `{"plugins":["sanitize_tools"]}`)
	if status != http.StatusOK {
		t.Fatalf("PATCH plugins status = %d, body %v", status, inst)
	}
	plugins, ok := inst["plugins"].([]any)
	if !ok || len(plugins) != 1 || plugins[0] != "sanitize_tools" {
		t.Errorf("response plugins = %v, want [sanitize_tools]", inst["plugins"])
	}
	reloaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("reload persisted config: %v", err)
	}
	if reloaded.Instances[0].Plugins == nil || len(*reloaded.Instances[0].Plugins) != 1 || (*reloaded.Instances[0].Plugins)[0] != "sanitize_tools" {
		t.Errorf("persisted plugins = %v, want [sanitize_tools]", reloaded.Instances[0].Plugins)
	}

	// Replace with an empty list: the durable off-switch survives reload.
	status, inst = doJSON(t, gs, "PATCH", "/api/instances/openai", `{"plugins":[]}`)
	if status != http.StatusOK {
		t.Fatalf("PATCH empty plugins status = %d, body %v", status, inst)
	}
	if got, ok := inst["plugins"].([]any); !ok || len(got) != 0 {
		t.Errorf("response plugins = %v, want []", inst["plugins"])
	}
	reloaded, err = config.Load(cfgPath)
	if err != nil {
		t.Fatalf("reload persisted config: %v", err)
	}
	if reloaded.Instances[0].Plugins == nil || len(*reloaded.Instances[0].Plugins) != 0 {
		t.Errorf("persisted plugins = %v, want empty non-nil list", reloaded.Instances[0].Plugins)
	}

	// Absent plugins in a later PATCH leaves the list untouched.
	status, _ = doJSON(t, gs, "PATCH", "/api/instances/openai", `{"priority":9}`)
	if status != http.StatusOK {
		t.Fatalf("PATCH priority status = %d", status)
	}
	reloaded, err = config.Load(cfgPath)
	if err != nil {
		t.Fatalf("reload persisted config: %v", err)
	}
	if reloaded.Instances[0].Plugins == nil || len(*reloaded.Instances[0].Plugins) != 0 {
		t.Errorf("plugins changed by an unrelated PATCH: %v", reloaded.Instances[0].Plugins)
	}
}

func TestInstancePatchPluginsInherit(t *testing.T) {
	gs, _, _, cfgPath := newAPITest(t, apiTestTOML, testMasterKey)

	// An explicit per-instance override can be reset back to inheriting the
	// global settings defaults: plugins_inherit clears the override (nil).
	status, inst := doJSON(t, gs, "PATCH", "/api/instances/openai", `{"plugins":["sanitize_tools"]}`)
	if status != http.StatusOK {
		t.Fatalf("PATCH plugins status = %d, body %v", status, inst)
	}
	if _, ok := inst["plugins"].([]any); !ok {
		t.Errorf("response plugins = %v, want a non-null list", inst["plugins"])
	}
	status, inst = doJSON(t, gs, "PATCH", "/api/instances/openai", `{"plugins_inherit":true}`)
	if status != http.StatusOK {
		t.Fatalf("PATCH plugins_inherit status = %d, body %v", status, inst)
	}
	if got, exists := inst["plugins"]; got != nil || !exists {
		t.Errorf("response plugins after inherit = %v (exists=%v), want null (inherit)", got, exists)
	}
	reloaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("reload persisted config: %v", err)
	}
	if reloaded.Instances[0].Plugins != nil {
		t.Errorf("persisted plugins after inherit = %v, want nil (inherit global)", reloaded.Instances[0].Plugins)
	}

	// plugins and plugins_inherit in one request is rejected.
	status, _ = doJSON(t, gs, "PATCH", "/api/instances/openai", `{"plugins":["redact"],"plugins_inherit":true}`)
	if status != http.StatusBadRequest {
		t.Errorf("PATCH plugins+plugins_inherit status = %d, want 400", status)
	}
	// plugins_inherit: false is a no-op (an absent list stays absent).
	status, _ = doJSON(t, gs, "PATCH", "/api/instances/openai", `{"plugins_inherit":false}`)
	if status != http.StatusOK {
		t.Errorf("PATCH plugins_inherit=false status = %d, want 200", status)
	}
	reloaded, err = config.Load(cfgPath)
	if err != nil {
		t.Fatalf("reload persisted config: %v", err)
	}
	if reloaded.Instances[0].Plugins != nil {
		t.Errorf("plugins_inherit=false changed the list: %v", reloaded.Instances[0].Plugins)
	}
}

func TestInstancePatchPluginsValidation(t *testing.T) {
	gs, _, _, _ := newAPITest(t, apiTestTOML, testMasterKey)

	status, out := doJSON(t, gs, "PATCH", "/api/instances/openai", `{"plugins":["no-such-plugin"]}`)
	if status != http.StatusBadRequest {
		t.Errorf("PATCH unknown plugin status = %d, want 400 (body %v)", status, out)
	}
	if out["code"] != "INVALID_ARGUMENT" {
		t.Errorf("code = %v, want INVALID_ARGUMENT", out["code"])
	}

	// Rejected update left the instance unchanged (plugins absent or null =
	// inherit, never a list).
	_, list := doJSON(t, gs, "GET", "/api/instances", "")
	if got, _ := list["instances"].([]any)[0].(map[string]any)["plugins"].([]any); len(got) != 0 {
		t.Errorf("instance gained plugins from a rejected PATCH: %v", got)
	}
}

func TestInstancePatchModelAliasesValidation(t *testing.T) {
	gs, _, _, _ := newAPITest(t, apiTestTOML, testMasterKey)

	for _, body := range []string{
		`{"model_aliases":{"UPPER":"gpt-4o"}}`,
		`{"model_aliases":{"bad key":"gpt-4o"}}`,
		`{"model_aliases":{"small":""}}`,
	} {
		status, out := doJSON(t, gs, "PATCH", "/api/instances/openai", body)
		if status != http.StatusBadRequest {
			t.Errorf("PATCH %s status = %d, want 400 (body %v)", body, status, out)
		}
		if out["code"] != "INVALID_ARGUMENT" {
			t.Errorf("PATCH %s code = %v, want INVALID_ARGUMENT", body, out["code"])
		}
	}

	// Rejected updates left the instance unchanged and the handler usable.
	status, list := doJSON(t, gs, "GET", "/api/instances", "")
	if status != http.StatusOK {
		t.Fatalf("GET instances status = %d", status)
	}
	if _, ok := list["instances"].([]any)[0].(map[string]any)["model_aliases"]; ok {
		t.Error("instance gained model_aliases from a rejected PATCH")
	}
	status, _ = doJSON(t, gs, "PATCH", "/api/instances/openai", `{"model_aliases":{"small":"gpt-4o-mini"}}`)
	if status != http.StatusOK {
		t.Errorf("valid PATCH after rejected ones status = %d, want 200", status)
	}
}

func TestInstancePatchCombinedFields(t *testing.T) {
	gs, _, _, _ := newAPITest(t, apiTestTOML, testMasterKey)

	// Rename + disable + model_aliases + model_reasoning together: all fields apply.
	status, inst := doJSON(t, gs, "PATCH", "/api/instances/openai", `{"alias":"main","disabled":true,"model_aliases":{"small":"gpt-4o-mini"},"model_reasoning":{"small":"high"}}`)
	if status != http.StatusOK {
		t.Fatalf("PATCH combined status = %d, body %v", status, inst)
	}
	if inst["alias"] != "main" || inst["disabled"] != true {
		t.Errorf("combined view = %v, want alias=main disabled=true", inst)
	}
	if got := inst["model_aliases"].(map[string]any)["small"]; got != "gpt-4o-mini" {
		t.Errorf("combined model_aliases = %v, want {small: gpt-4o-mini}", inst["model_aliases"])
	}
	if got := inst["model_reasoning"].(map[string]any)["small"]; got != "high" {
		t.Errorf("combined model_reasoning = %v, want {small: high}", inst["model_reasoning"])
	}
}

func TestInstanceOrderSetsPriorities(t *testing.T) {
	// Fixture with two instances: openai then openai-2.
	toml := apiTestTOML + `
[[instances]]
alias = "openai-2"
template = "openai"
`
	gs, _, _, cfgPath := newAPITest(t, toml, testMasterKey)

	// Reverse the order: openai-2 first (priority 1), openai second (2).
	resp := doRaw(t, gs, "PUT", "/api/instances/order", `{"aliases":["openai-2","openai"]}`)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT order status = %d, want 204", resp.StatusCode)
	}
	reloaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("reload persisted config: %v", err)
	}
	byAlias := map[string]int{}
	for _, inst := range reloaded.Instances {
		byAlias[inst.Alias] = inst.Priority
	}
	if byAlias["openai-2"] != 1 || byAlias["openai"] != 2 {
		t.Errorf("priorities = %v, want openai-2:1 openai:2", byAlias)
	}

	// Invalid bodies are rejected: missing aliases, duplicates, empty.
	for _, body := range []string{
		`{"aliases":["openai"]}`,
		`{"aliases":["openai","openai"]}`,
		`{"aliases":["openai",""]}`,
		`{"aliases":["openai","openai-2","extra"]}`,
	} {
		status, out := doJSON(t, gs, "PUT", "/api/instances/order", body)
		if status != http.StatusBadRequest {
			t.Errorf("PUT order %s status = %d, want 400 (%v)", body, status, out)
		}
	}
}

func TestInstancePatchPriority(t *testing.T) {
	gs, _, _, cfgPath := newAPITest(t, apiTestTOML, testMasterKey)

	status, inst := doJSON(t, gs, "PATCH", "/api/instances/openai", `{"priority":7}`)
	if status != http.StatusOK {
		t.Fatalf("PATCH priority status = %d, body %v", status, inst)
	}
	if got := inst["priority"]; got != float64(7) {
		t.Errorf("priority after PATCH = %v, want 7", got)
	}

	// The patched priority is persisted to gateway.toml.
	reloaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("reload persisted config: %v", err)
	}
	if got := reloaded.Instances[0].Priority; got != 7 {
		t.Errorf("persisted priority = %d, want 7", got)
	}

	// Explicit 0 clears the priority back to unset (file order).
	status, inst = doJSON(t, gs, "PATCH", "/api/instances/openai", `{"priority":0}`)
	if status != http.StatusOK {
		t.Fatalf("PATCH priority=0 status = %d, body %v", status, inst)
	}
	if _, present := inst["priority"]; present {
		t.Errorf("priority should be omitted after clearing, got %v", inst["priority"])
	}
	reloaded, err = config.Load(cfgPath)
	if err != nil {
		t.Fatalf("reload persisted config after clear: %v", err)
	}
	if got := reloaded.Instances[0].Priority; got != 0 {
		t.Errorf("persisted priority after clear = %d, want 0", got)
	}
}

func TestInstanceCreateCustomEndpoint(t *testing.T) {
	gs, _, _, cfgPath := newAPITest(t, apiTestTOML, testMasterKey)

	// A custom_openai placeholder without an endpoint URL is refused.
	status, out := doJSON(t, gs, "POST", "/api/instances", `{"template":"custom_openai","alias":"myai"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("placeholder without endpoint status = %d, want 400 (%v)", status, out)
	}

	// With an endpoint URL the placeholder resolves into a concrete custom
	// template and the instance is created against it.
	status, inst := doJSON(t, gs, "POST", "/api/instances", `{"template":"custom_openai","alias":"myai","base_url":"http://localhost:8080/v1","models":["my-model"]}`)
	if status != http.StatusCreated {
		t.Fatalf("custom create status = %d, body %v", status, inst)
	}
	if got := inst["template"]; got != "custom-openai-localhost-8080-v1" {
		t.Errorf("instance template = %v, want custom-openai-localhost-8080-v1", got)
	}
	if got := inst["style"]; got != "openai" {
		t.Errorf("instance style = %v, want openai", got)
	}
	if got := inst["base_url"]; got != "http://localhost:8080/v1" {
		t.Errorf("instance base_url = %v", got)
	}

	// The custom template is persisted to the config file and reusable: a
	// second instance with the same endpoint reuses it (no duplicate).
	status, inst2 := doJSON(t, gs, "POST", "/api/instances", `{"template":"custom_openai","alias":"myai-2","base_url":"http://localhost:8080/v1"}`)
	if status != http.StatusCreated {
		t.Fatalf("second custom create status = %d, body %v", status, inst2)
	}
	if got := inst2["template"]; got != "custom-openai-localhost-8080-v1" {
		t.Errorf("second instance template = %v", got)
	}
	status, tmplList := doJSON(t, gs, "GET", "/api/templates", "")
	count := 0
	for _, x := range tmplList["templates"].([]any) {
		if x.(map[string]any)["name"] == "custom-openai-localhost-8080-v1" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("custom template count = %d, want 1 (reused, not duplicated)", count)
	}

	// Anthropic style: custom_anthropic resolves to the anthropic-styled name.
	status, inst3 := doJSON(t, gs, "POST", "/api/instances", `{"template":"custom_anthropic","alias":"claude","base_url":"http://localhost:9000","models":["claude-sonnet-4"]}`)
	if status != http.StatusCreated {
		t.Fatalf("custom anthropic create status = %d, body %v", status, inst3)
	}
	if got := inst3["template"]; got != "custom-anthropic-localhost-9000" {
		t.Errorf("anthropic instance template = %v, want custom-anthropic-localhost-9000", got)
	}
	if got := inst3["style"]; got != "anthropic" {
		t.Errorf("anthropic instance style = %v", got)
	}

	// A user-defined anthropic template + endpoint override keeps the style.
	status, _ = doJSON(t, gs, "POST", "/api/templates", `{"name":"my-anth","base_url":"https://api.example.com/v1","style":"anthropic","models":["m"]}`)
	if status != http.StatusCreated {
		t.Fatalf("template create status = %d", status)
	}
	status, inst4 := doJSON(t, gs, "POST", "/api/instances", `{"template":"my-anth","alias":"claude-2","base_url":"http://localhost:9001"}`)
	if status != http.StatusCreated {
		t.Fatalf("override create status = %d, body %v", status, inst4)
	}
	if got := inst4["template"]; got != "custom-anthropic-localhost-9001" {
		t.Errorf("override instance template = %v, want custom-anthropic-localhost-9001", got)
	}

	// An invalid endpoint URL is rejected before any write.
	status, out = doJSON(t, gs, "POST", "/api/instances", `{"template":"custom_openai","base_url":"file:///etc/passwd"}`)
	if status != http.StatusBadRequest {
		t.Errorf("bad base_url status = %d, want 400 (%v)", status, out)
	}

	// The custom templates survive a full config reload.
	reloaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("reload persisted config: %v", err)
	}
	for _, want := range []string{"custom-openai-localhost-8080-v1", "custom-anthropic-localhost-9000", "custom-anthropic-localhost-9001"} {
		if _, ok := reloaded.Templates[want]; !ok {
			t.Errorf("reloaded config missing template %q", want)
		}
	}
}

func TestTemplateCreateStyleValidation(t *testing.T) {
	gs, _, _, _ := newAPITest(t, apiTestTOML, testMasterKey)
	status, out := doJSON(t, gs, "POST", "/api/templates", `{"name":"bad-style","base_url":"https://x","style":"grpc"}`)
	if status != http.StatusBadRequest {
		t.Errorf("bad style status = %d, want 400 (%v)", status, out)
	}
	status, out = doJSON(t, gs, "POST", "/api/templates", `{"name":"anth","base_url":"https://x","style":"anthropic"}`)
	if status != http.StatusCreated {
		t.Errorf("anthropic style template status = %d, want 201 (%v)", status, out)
	}
}
