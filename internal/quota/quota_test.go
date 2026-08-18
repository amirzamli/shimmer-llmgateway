package quota

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/amirzamli/shimmer-llmgateway/internal/config"
	"github.com/amirzamli/shimmer-llmgateway/internal/secrets"
)

// quotaTestKey is a fixed 32-byte AES-256 key mirroring the secrets/api test
// stores so the secrets file is written as an encrypted envelope.
var quotaTestKey = []byte("0123456789abcdef0123456789abcdef")

func mustRFC3339(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return tm
}

func TestParseDeepSeek(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"usd numeric", `{"balance_infos":[{"currency":"USD","total_balance":12.34,"granted_balance":10,"topped_up_balance":2.34}]}`, "$12.34"},
		{"usd numeric string", `{"balance_infos":[{"currency":"USD","total_balance":"45.60"}]}`, "$45.60"},
		{"cny fallback", `{"balance_infos":[{"currency":"CNY","total_balance":88.5}]}`, "¥88.50"},
		{"usd wins over cny", `{"balance_infos":[{"currency":"CNY","total_balance":1},{"currency":"USD","total_balance":2.5}]}`, "$2.50"},
		{"first cny of several", `{"balance_infos":[{"currency":"CNY","total_balance":1},{"currency":"CNY","total_balance":2}]}`, "¥1.00"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			windows, err := parseDeepSeek([]byte(tt.body))
			if err != nil {
				t.Fatalf("parseDeepSeek: %v", err)
			}
			w, ok := windows["credits_balance"]
			if !ok {
				t.Fatalf("windows = %v, want credits_balance key", windows)
			}
			if w.ValueLabel != tt.want {
				t.Errorf("ValueLabel = %q, want %q", w.ValueLabel, tt.want)
			}
		})
	}

	errorBodies := []struct {
		name string
		body string
	}{
		{"no entries", `{"balance_infos":[]}`},
		{"no eligible currency", `{"balance_infos":[{"currency":"EUR","total_balance":1}]}`},
		{"null balance", `{"balance_infos":[{"currency":"USD","total_balance":null}]}`},
		{"unparseable balance", `{"balance_infos":[{"currency":"USD","total_balance":"abc"}]}`},
		{"not json", `not json`},
	}
	for _, tt := range errorBodies {
		t.Run("error_"+tt.name, func(t *testing.T) {
			if _, err := parseDeepSeek([]byte(tt.body)); err == nil {
				t.Fatal("parseDeepSeek succeeded, want error")
			}
		})
	}

	// No-data outcomes carry the specific UI message; malformed JSON does not.
	_, err := parseDeepSeek([]byte(`{"balance_infos":[{"currency":"USD","total_balance":null}]}`))
	if !errors.Is(err, errNoQuotaData) {
		t.Errorf("no-data error = %v, want %q", err, errNoQuotaData)
	}
	if _, err := parseDeepSeek([]byte(`not json`)); errors.Is(err, errNoQuotaData) {
		t.Errorf("malformed JSON error = %v, want a raw parse error", err)
	}
}

func TestParseOpenCodeGo(t *testing.T) {
	t.Run("all windows with clamp", func(t *testing.T) {
		body := `{"usage":{"rolling":{"percent":50,"resetsAt":"2026-08-18T00:00:00Z"},"weekly":{"percent":110,"resetsAt":"2026-08-24T00:00:00Z"},"monthly":{"percent":-5,"resetsAt":"2026-09-01T00:00:00Z"}}}`
		windows, err := parseOpenCodeGo([]byte(body))
		if err != nil {
			t.Fatalf("parseOpenCodeGo: %v", err)
		}
		if len(windows) != 3 {
			t.Fatalf("windows = %v, want 3", windows)
		}
		if got := *windows["5h"].UsedPercent; got != 50 {
			t.Errorf("rolling percent = %v, want 50", got)
		}
		if got := *windows["weekly"].UsedPercent; got != 100 {
			t.Errorf("weekly percent = %v, want 100 (clamped)", got)
		}
		if got := *windows["monthly"].UsedPercent; got != 0 {
			t.Errorf("monthly percent = %v, want 0 (clamped)", got)
		}
		if want := mustRFC3339(t, "2026-08-18T00:00:00Z").UnixMilli(); *windows["5h"].ResetAt != want {
			t.Errorf("rolling reset = %d, want %d", *windows["5h"].ResetAt, want)
		}
	})

	t.Run("numeric string percent and epoch resetsAt", func(t *testing.T) {
		body := `{"usage":{"rolling":{"percent":"75.5","resetsAt":1780000000}}}`
		windows, err := parseOpenCodeGo([]byte(body))
		if err != nil {
			t.Fatalf("parseOpenCodeGo: %v", err)
		}
		w := windows["5h"]
		if got := *w.UsedPercent; got != 75.5 {
			t.Errorf("percent = %v, want 75.5", got)
		}
		if got := *w.ResetAt; got != 1780000000000 {
			t.Errorf("reset = %d, want 1780000000000 (epoch seconds → ms)", got)
		}
	})

	t.Run("skips invalid entries", func(t *testing.T) {
		body := `{"usage":{"rolling":{"percent":"abc","resetsAt":"2026-08-18T00:00:00Z"},"weekly":{"percent":50,"resetsAt":"nope"},"monthly":{"percent":50,"resetsAt":"2026-09-01T00:00:00Z"}}}`
		windows, err := parseOpenCodeGo([]byte(body))
		if err != nil {
			t.Fatalf("parseOpenCodeGo: %v", err)
		}
		if len(windows) != 1 {
			t.Fatalf("windows = %v, want only monthly", windows)
		}
		if _, ok := windows["monthly"]; !ok {
			t.Error("monthly window missing")
		}
	})

	t.Run("no usable windows", func(t *testing.T) {
		_, err := parseOpenCodeGo([]byte(`{"usage":{}}`))
		if !errors.Is(err, errOpenCodeUsage) {
			t.Errorf("error = %v, want %q", err, errOpenCodeUsage)
		}
	})

	t.Run("not json", func(t *testing.T) {
		_, err := parseOpenCodeGo([]byte(`not json`))
		if err == nil || errors.Is(err, errOpenCodeUsage) {
			t.Errorf("error = %v, want a raw parse error", err)
		}
	})
}

func TestParseOpenRouter(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"numbers", `{"data":{"total_credits":100,"total_usage":30}}`, "$70.00 left · $30.00 spent"},
		{"numeric strings", `{"data":{"total_credits":"100","total_usage":"25.5"}}`, "$74.50 left · $25.50 spent"},
		{"usage exceeds credits", `{"data":{"total_credits":10,"total_usage":20}}`, "$0.00 left · $20.00 spent"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			windows, err := parseOpenRouter([]byte(tt.body))
			if err != nil {
				t.Fatalf("parseOpenRouter: %v", err)
			}
			if got := windows["credits"].ValueLabel; got != tt.want {
				t.Errorf("ValueLabel = %q, want %q", got, tt.want)
			}
		})
	}

	t.Run("missing fields yield empty label", func(t *testing.T) {
		windows, err := parseOpenRouter([]byte(`{"data":{}}`))
		if err != nil {
			t.Fatalf("parseOpenRouter: %v", err)
		}
		if _, ok := windows["credits"]; !ok {
			t.Fatal("credits window missing")
		}
		if windows["credits"].ValueLabel != "" {
			t.Errorf("ValueLabel = %q, want empty", windows["credits"].ValueLabel)
		}
	})

	t.Run("not json", func(t *testing.T) {
		if _, err := parseOpenRouter([]byte(`not json`)); err == nil {
			t.Error("parseOpenRouter succeeded, want error")
		}
	})
}

func TestListClassification(t *testing.T) {
	// Fake DeepSeek: serves /user/balance and records the Authorization header
	// per request (List fetches serially, so order is stable).
	var authMu sync.Mutex
	var auths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authMu.Lock()
		auths = append(auths, r.Header.Get("Authorization"))
		authMu.Unlock()
		if r.URL.Path != "/user/balance" {
			t.Errorf("path = %q, want /user/balance", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"balance_infos":[{"currency":"USD","total_balance":12.34}]}`)
	}))
	t.Cleanup(srv.Close)

	sec, err := secrets.Open(filepath.Join(t.TempDir(), "test.secrets.json"), quotaTestKey)
	if err != nil {
		t.Fatalf("secrets.Open: %v", err)
	}
	if err := sec.Set("ds-secret", "sk-secret-key"); err != nil {
		t.Fatalf("secrets.Set: %v", err)
	}

	cfg := &config.Config{
		Templates: map[string]*config.Template{
			"deepseek": {Name: "deepseek", BaseURL: srv.URL, APIKeyEnv: "QUOTA_TEST_KEY"},
			"openai":   {Name: "openai", BaseURL: "https://api.openai.com/v1", APIKeyEnv: "OPENAI_API_KEY"},
		},
		Instances: []*config.Instance{
			{Alias: "ds-env", Template: "deepseek"},
			{Alias: "ds-nokey", Template: "deepseek", APIKeyEnv: "QUOTA_TEST_UNSET"},
			{Alias: "nope", Template: "openai"},
			{Alias: "ds-secret", Template: "deepseek", APIKeyEnv: "QUOTA_TEST_UNSET"},
		},
	}
	t.Setenv("QUOTA_TEST_KEY", "sk-env-key")

	f := New(func() *config.Config { return cfg }, sec)
	results := f.List(context.Background(), false)
	if len(results) != 4 {
		t.Fatalf("results = %d, want 4", len(results))
	}

	// Config order is preserved.
	if results[0].Alias != "ds-env" || results[3].Alias != "ds-secret" {
		t.Errorf("results = [%s %s %s %s], want config order", results[0].Alias, results[1].Alias, results[2].Alias, results[3].Alias)
	}

	// Supported + env key: fetched OK with a DeepSeek balance window.
	r := results[0]
	if !r.Supported || !r.Configured || !r.Ok {
		t.Fatalf("ds-env = %+v, want supported+configured+ok", r)
	}
	if r.Provider != "DeepSeek" {
		t.Errorf("provider = %q, want DeepSeek", r.Provider)
	}
	if r.Error != "" {
		t.Errorf("ds-env error = %q, want empty", r.Error)
	}
	if got := r.Windows["credits_balance"].ValueLabel; got != "$12.34" {
		t.Errorf("credits_balance label = %q, want $12.34", got)
	}

	// Supported but no key anywhere: not configured, no fetch attempted.
	r = results[1]
	if !r.Supported || r.Configured || r.Ok {
		t.Errorf("ds-nokey = %+v, want supported+not configured", r)
	}
	if r.Error != quotaErrNotConfigured {
		t.Errorf("ds-nokey error = %q, want %q", r.Error, quotaErrNotConfigured)
	}

	// Unsupported template: gap reported, no fetch attempted.
	r = results[2]
	if r.Supported || r.Configured || r.Ok {
		t.Errorf("nope = %+v, want unsupported", r)
	}
	if r.Error != quotaErrUnsupported {
		t.Errorf("nope error = %q, want %q", r.Error, quotaErrUnsupported)
	}

	// Supported with a stored secret key (env-var precedence falls through).
	r = results[3]
	if !r.Supported || !r.Configured || !r.Ok {
		t.Fatalf("ds-secret = %+v, want supported+configured+ok via secret", r)
	}

	// Only the two keyed deepseek instances hit the provider, each with its
	// own key (env first, then the stored secret).
	authMu.Lock()
	gotAuths := append([]string(nil), auths...)
	authMu.Unlock()
	wantAuths := "Bearer sk-env-key,Bearer sk-secret-key"
	if strings.Join(gotAuths, ",") != wantAuths {
		t.Errorf("authorizations = %v, want %s", gotAuths, wantAuths)
	}
}
