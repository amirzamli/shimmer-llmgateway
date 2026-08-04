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
	"testing"
	"time"

	"shimmer-llmgateway/internal/config"
	"shimmer-llmgateway/internal/logging"
	"shimmer-llmgateway/internal/secrets"
	"shimmer-llmgateway/internal/store"
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
	if out["listen"] != "127.0.0.1:8787" || out["store"] != "gateway.db" || out["retention_days"] != float64(30) {
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
