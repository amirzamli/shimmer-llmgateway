// Package quota fetches per-provider account quota/balance for the gateway UI
// (§6.2): DeepSeek balance, OpenCode Go usage limits, OpenRouter credits, and
// ChatGPT OAuth usage limits.
// Each provider has a small strategy (suffix + payload parser) registered by
// template name; unsupported templates are reported as a gap so the UI can
// show them. Credentials are used only for the outbound probe and no error
// string ever includes the base URL or credential material — the /api/ surface
// is unauthenticated, so failures surface a fixed message while the underlying
// detail is dropped.
package quota

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/amirzamli/shimmer-llmgateway/internal/config"
	"github.com/amirzamli/shimmer-llmgateway/internal/secrets"
)

const (
	// quotaCacheTTL mirrors the model-fetch cache TTL: a fetched result (or a
	// failed fetch) is served from memory for this long before refetching.
	quotaCacheTTL = 5 * time.Minute
	// quotaFetchTimeout bounds each provider request so the UI never hangs on
	// a dead upstream.
	quotaFetchTimeout = 15 * time.Second
	// quotaResponseLimit caps the response body at ~1 MiB so an abusive
	// provider cannot exhaust memory.
	quotaResponseLimit = 1 << 20
)

// Result classification error strings. These are the only errors surfaced to
// the UI; none of them can contain a URL, base_url, or key.
const (
	quotaErrUnsupported     = "Quota not supported for this provider"
	quotaErrNotConfigured   = "Not configured"
	quotaErrInvalidResponse = "invalid response from provider"
	quotaErrAuthFailed      = "authentication failed"
	quotaErrTimedOut        = "request timed out"
	quotaErrRequestFailed   = "request failed"
)

// Provider-parser errors: no-data outcomes keep their specific message when
// surfaced (the UI shows why), while malformed JSON maps to
// quotaErrInvalidResponse.
var (
	errNoQuotaData   = errors.New("No quota data in response")
	errOpenCodeUsage = errors.New("OpenCode Go usage data could not be parsed")
)

// Window is one quota/balance value for an instance. Which fields are set
// depends on the provider: DeepSeek and OpenRouter produce a ValueLabel,
// OpenCode Go produces a clamped percent plus a reset time.
type Window struct {
	// UsedPercent is the clamped (0-100) utilization for OpenCode Go windows.
	UsedPercent *float64 `json:"used_percent,omitempty"`
	// WindowSeconds is reserved for future windowed-credit providers.
	WindowSeconds *int64 `json:"window_seconds,omitempty"`
	// ResetAt is the window reset as unix milliseconds.
	ResetAt *int64 `json:"reset_at,omitempty"`
	// ValueLabel is the human-readable balance/remaining summary, e.g.
	// "$12.34" or "$70.00 left · $30.00 spent".
	ValueLabel string `json:"value_label,omitempty"`
}

// Result is the per-instance quota status rendered by the UI.
type Result struct {
	Alias      string            `json:"alias"`
	Template   string            `json:"template"`
	Provider   string            `json:"provider"` // display name
	Supported  bool              `json:"supported"`
	Configured bool              `json:"configured"`
	Ok         bool              `json:"ok"`
	Error      string            `json:"error,omitempty"`
	UsageURL   string            `json:"usage_url,omitempty"`
	Windows    map[string]Window `json:"windows,omitempty"`
	FetchedAt  time.Time         `json:"fetched_at"`
}

// providerSpec is the quota strategy for one template name: where to GET on the
// template's base_url and how to parse the payload.
type providerSpec struct {
	providerName string
	suffix       string
	oauth        bool
	parse        func([]byte) (map[string]Window, error)
}

// OAuthCredentialResolver supplies a current ChatGPT OAuth credential. The
// gateway implementation refreshes expired access tokens before returning;
// quota keeps the interface here to avoid importing the gateway package.
type OAuthCredentialResolver interface {
	Credential(context.Context, string) (secrets.OAuthCredential, error)
}

// specs registers the quota strategies for providers with
// account balance/quota/usage endpoints. Unsupported templates are reported as a
// gap rather than fetched.
var specs = map[string]providerSpec{
	"deepseek":    {providerName: "DeepSeek", suffix: "/user/balance", parse: parseDeepSeek},
	"opencode_go": {providerName: "OpenCode Go", suffix: "/usage", parse: parseOpenCodeGo},
	"openrouter":  {providerName: "OpenRouter", suffix: "/credits", parse: parseOpenRouter},
	"chatgpt":     {providerName: "ChatGPT", suffix: "/wham/usage", oauth: true, parse: parseChatGPT},
}

// usageURLs maps template names to web-based usage / quota dashboard URLs
// when API-based quota fetching is unsupported.
var usageURLs = map[string]string{
	"commandcode": "https://commandcode.ai/studio",
}

// Fetcher resolves per-instance quota/balance with a TTL cache keyed by
// instance alias. It takes a getter (not the manager) so tests can drive it
// with a plain config and no server.
type Fetcher struct {
	getCfg func() *config.Config
	sec    *secrets.Store
	client *http.Client

	// mu guards cache, the in-memory per-alias fetch results (TTL
	// quotaCacheTTL; invalidated on instance PATCH/DELETE).
	mu            sync.Mutex
	cache         map[string]Result
	oauthResolver OAuthCredentialResolver
}

// New returns a Fetcher resolving API keys with the gateway's §6.2 precedence
// (api_key_env first, then the secrets store). resolver supplies refresh-aware
// OAuth credentials for ChatGPT instances; nil keeps the direct secrets
// fallback used by isolated tests.
func New(getCfg func() *config.Config, sec *secrets.Store, resolver OAuthCredentialResolver) *Fetcher {
	return &Fetcher{
		getCfg: getCfg,
		sec:    sec,
		// Redirect-following is disabled so a redirecting or malicious
		// provider base_url cannot bounce the quota fetch to an internal
		// endpoint; the 3xx response is returned as-is.
		client: &http.Client{
			Timeout: quotaFetchTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		cache:         map[string]Result{},
		oauthResolver: resolver,
	}
}

// List returns one Result per configured instance in config order, including
// unsupported and unconfigured ones so the UI can show gaps. With refresh=true
// the TTL cache is bypassed for every instance.
func (f *Fetcher) List(ctx context.Context, refresh bool) []Result {
	cfg := f.getCfg()
	results := make([]Result, 0, len(cfg.Instances))
	for _, inst := range cfg.Instances {
		results = append(results, f.resultFor(ctx, cfg, inst, refresh))
	}
	return results
}

// InvalidateQuota drops the cached result for an alias (called after a
// successful instance PATCH or DELETE so a recycled, renamed, or reconfigured
// alias never serves a stale balance).
func (f *Fetcher) InvalidateQuota(alias string) {
	f.mu.Lock()
	delete(f.cache, alias)
	f.mu.Unlock()
}

func (f *Fetcher) resultFor(ctx context.Context, cfg *config.Config, inst *config.Instance, refresh bool) Result {
	spec, ok := specs[inst.Template]
	if !ok {
		return Result{
			Alias:      inst.Alias,
			Template:   inst.Template,
			Supported:  false,
			Configured: false,
			Ok:         false,
			Error:      quotaErrUnsupported,
			UsageURL:   usageURLs[inst.Template],
			FetchedAt:  time.Now(),
		}
	}
	if !refresh {
		f.mu.Lock()
		if r, ok := f.cache[inst.Alias]; ok && time.Since(r.FetchedAt) < quotaCacheTTL {
			f.mu.Unlock()
			return r
		}
		f.mu.Unlock()
	}

	res := Result{
		Alias:      inst.Alias,
		Template:   inst.Template,
		Provider:   spec.providerName,
		Supported:  true,
		Configured: true,
		FetchedAt:  time.Now(),
	}
	credential, configured, credentialErr := f.fetchCredential(ctx, cfg, inst, spec)
	if !configured {
		res.Configured = false
		res.Error = credentialErr
	} else if credentialErr != "" {
		res.Ok = false
		res.Error = credentialErr
	} else {
		windows, errMsg := f.fetchWindows(ctx, cfg, inst, spec, credential)
		if errMsg != "" {
			res.Ok = false
			res.Error = errMsg
		} else {
			res.Ok = true
			res.Windows = windows
		}
	}
	f.store(inst.Alias, res)
	return res
}

func (f *Fetcher) store(alias string, res Result) {
	f.mu.Lock()
	f.cache[alias] = res
	f.mu.Unlock()
}

// fetchKey returns the provider key for an alias with §6.2 precedence: the
// api_key_env environment variable first, then the UI-managed secrets file.
// An empty result means no key is available anywhere.
func (f *Fetcher) fetchKey(cfg *config.Config, inst *config.Instance) string {
	key, _ := f.sec.ResolveKey(inst.EffectiveAPIKeyEnv(cfg), inst.Alias)
	return key
}

type quotaCredential struct {
	token     string
	accountID string
}

// fetchCredential resolves either the normal API key or the ChatGPT OAuth
// credential. OAuth instances are read through the refresh-aware resolver when
// one is installed; the direct secrets fallback keeps the quota package usable
// in isolated tests and reports the same configured state.
func (f *Fetcher) fetchCredential(ctx context.Context, cfg *config.Config, inst *config.Instance, spec providerSpec) (quotaCredential, bool, string) {
	if !spec.oauth {
		if key := f.fetchKey(cfg, inst); key != "" {
			return quotaCredential{token: key}, true, ""
		}
		return quotaCredential{}, false, quotaErrNotConfigured
	}

	f.mu.Lock()
	resolver := f.oauthResolver
	f.mu.Unlock()
	if resolver != nil {
		cred, err := resolver.Credential(ctx, inst.Alias)
		if err == nil && cred.AccessToken != "" {
			return quotaCredential{token: cred.AccessToken, accountID: cred.AccountID}, true, ""
		}
		stored, ok, storedErr := f.sec.GetOAuth(inst.Alias)
		if storedErr != nil {
			return quotaCredential{}, true, quotaErrInvalidResponse
		}
		if !ok || stored.AccessToken == "" {
			return quotaCredential{}, false, quotaErrNotConfigured
		}
		return quotaCredential{}, true, quotaErrAuthFailed
	}

	cred, ok, err := f.sec.GetOAuth(inst.Alias)
	if err != nil {
		return quotaCredential{}, true, quotaErrInvalidResponse
	}
	if !ok || cred.AccessToken == "" {
		return quotaCredential{}, false, quotaErrNotConfigured
	}
	return quotaCredential{token: cred.AccessToken, accountID: cred.AccountID}, true, ""
}

// fetchWindows GETs the provider's quota endpoint (base_url + strategy
// suffix) with Bearer auth, maps the status/body to the fixed set of
// user-facing errors (never the URL or credential), and parses the windows.
func (f *Fetcher) fetchWindows(ctx context.Context, cfg *config.Config, inst *config.Instance, spec providerSpec, credential quotaCredential) (map[string]Window, string) {
	tmpl, ok := cfg.Templates[inst.Template]
	if !ok {
		return nil, quotaErrInvalidResponse
	}
	baseURL := strings.TrimRight(tmpl.BaseURL, "/")
	if spec.oauth {
		// The ChatGPT request endpoint includes /codex, while the account usage
		// endpoint is rooted at /backend-api/wham/usage.
		baseURL = strings.TrimSuffix(baseURL, "/codex")
	}
	url := baseURL + spec.suffix

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, quotaErrRequestFailed
	}
	req.Header.Set("Authorization", "Bearer "+credential.token)
	if credential.accountID != "" {
		req.Header.Set("ChatGPT-Account-Id", credential.accountID)
	}
	// No inbound client exists on this path, so the probe always identifies
	// itself with the gateway's constant UA instead of Go's "Go-http-client/1.1".
	req.Header.Set("User-Agent", config.UserAgent)

	resp, err := f.client.Do(req)
	if err != nil {
		if isTimeoutErr(err) {
			return nil, quotaErrTimedOut
		}
		return nil, quotaErrRequestFailed
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, quotaErrAuthFailed
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Sprintf("API error: %d", resp.StatusCode)
	}

	// Cap the response body at ~1 MiB, reading one extra byte to detect
	// truncation before decoding.
	body, err := io.ReadAll(io.LimitReader(resp.Body, quotaResponseLimit+1))
	if err != nil {
		if isTimeoutErr(err) {
			return nil, quotaErrTimedOut
		}
		return nil, quotaErrInvalidResponse
	}
	if len(body) > quotaResponseLimit {
		return nil, quotaErrInvalidResponse
	}

	windows, pErr := spec.parse(body)
	if pErr != nil {
		// No-data outcomes keep their specific message; malformed payloads
		// collapse to the generic invalid-response string.
		if errors.Is(pErr, errNoQuotaData) || errors.Is(pErr, errOpenCodeUsage) {
			return nil, pErr.Error()
		}
		return nil, quotaErrInvalidResponse
	}
	return windows, ""
}

// isTimeoutErr reports whether err is a client or context deadline (the
// http.Client.Timeout path and an in-flight body read both surface as one of
// these wrapped errors).
func isTimeoutErr(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded)
}

// asFloat parses a JSON number or a numeric string ("12.34") into a float64.
// JSON null, empty, and unparseable values are rejected; non-finite numbers
// are rejected too.
func asFloat(raw json.RawMessage) (float64, bool) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return 0, false
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		return f, true
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if err == nil && !math.IsNaN(v) && !math.IsInf(v, 0) {
			return v, true
		}
	}
	return 0, false
}

// parseResetAt converts a resetsAt value to unix milliseconds: RFC3339
// strings are parsed as time, and numeric values are treated as an epoch —
// seconds when small enough (< 1e11), milliseconds otherwise.
func parseResetAt(raw json.RawMessage) (int64, bool) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return 0, false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		s = strings.TrimSpace(s)
		if s == "" {
			return 0, false
		}
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t.UnixMilli(), true
		}
		return 0, false
	}
	v, ok := asFloat(raw)
	if !ok {
		return 0, false
	}
	if v >= 0 && v < 1e11 { // epoch seconds
		return int64(v * 1000), true
	}
	return int64(v), true
}

// parseDeepSeek parses GET {base}/user/balance: balance_infos carrying
// currency and total_balance (a JSON number or numeric string). The first USD
// entry wins, then the first CNY; an entry with no usable balance or no
// eligible currency is "no quota data".
func parseDeepSeek(body []byte) (map[string]Window, error) {
	var payload struct {
		BalanceInfos []struct {
			Currency        string          `json:"currency"`
			TotalBalance    json.RawMessage `json:"total_balance"`
			GrantedBalance  json.RawMessage `json:"granted_balance"`
			ToppedUpBalance json.RawMessage `json:"topped_up_balance"`
		} `json:"balance_infos"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	firstUSD, firstCNY := -1, -1
	for i := range payload.BalanceInfos {
		switch payload.BalanceInfos[i].Currency {
		case "USD":
			if firstUSD == -1 {
				firstUSD = i
			}
		case "CNY":
			if firstCNY == -1 {
				firstCNY = i
			}
		}
	}
	idx := firstUSD
	if idx == -1 {
		idx = firstCNY
	}
	if idx == -1 {
		return nil, errNoQuotaData
	}
	total, ok := asFloat(payload.BalanceInfos[idx].TotalBalance)
	if !ok {
		return nil, errNoQuotaData
	}
	symbol := "$"
	if payload.BalanceInfos[idx].Currency == "CNY" {
		symbol = "¥"
	}
	return map[string]Window{
		"credits_balance": {ValueLabel: fmt.Sprintf("%s%.2f", symbol, total)},
	}, nil
}

// openCodeUsageWindow is one usage sub-object on GET {base}/usage; fields stay
// raw so percent and resetsAt can be parsed flexibly.
type openCodeUsageWindow struct {
	Percent  json.RawMessage `json:"percent"`
	ResetsAt json.RawMessage `json:"resetsAt"`
}

// parseOpenCodeGo parses GET {base}/usage: rolling/weekly/monthly limits with
// percent utilization and reset time. A window without a usable percent or
// reset is skipped; zero usable windows is an error.
func parseOpenCodeGo(body []byte) (map[string]Window, error) {
	var payload struct {
		Usage struct {
			Rolling openCodeUsageWindow `json:"rolling"`
			Weekly  openCodeUsageWindow `json:"weekly"`
			Monthly openCodeUsageWindow `json:"monthly"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	windows := map[string]Window{}
	for _, e := range []struct {
		key string
		w   openCodeUsageWindow
	}{
		{"5h", payload.Usage.Rolling},
		{"weekly", payload.Usage.Weekly},
		{"monthly", payload.Usage.Monthly},
	} {
		p, ok := asFloat(e.w.Percent)
		if !ok {
			continue
		}
		reset, okReset := parseResetAt(e.w.ResetsAt)
		if !okReset {
			continue
		}
		used := math.Max(0, math.Min(100, p))
		windows[e.key] = Window{UsedPercent: &used, ResetAt: &reset}
	}
	if len(windows) == 0 {
		return nil, errOpenCodeUsage
	}
	return windows, nil
}

// chatGPTUsageWindow is one rate-limit window from GET /wham/usage. The
// endpoint returns numeric values today, but raw fields keep the parser
// tolerant of the numeric-string form used by some provider APIs.
type chatGPTUsageWindow struct {
	UsedPercent       json.RawMessage `json:"used_percent"`
	LimitWindowSecond json.RawMessage `json:"limit_window_seconds"`
	ResetAt           json.RawMessage `json:"reset_at"`
}

// parseChatGPT parses the ChatGPT Codex usage response. Its primary and
// secondary windows correspond to the rolling and weekly limits shown by the
// OpenCode Go quota surface; a tertiary window and credit balance are retained
// when the account supplies them.
func parseChatGPT(body []byte) (map[string]Window, error) {
	var payload struct {
		RateLimit struct {
			Primary   chatGPTUsageWindow `json:"primary_window"`
			Secondary chatGPTUsageWindow `json:"secondary_window"`
			Tertiary  chatGPTUsageWindow `json:"tertiary_window"`
		} `json:"rate_limit"`
		Credits struct {
			Balance json.RawMessage `json:"balance"`
		} `json:"credits"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}

	windows := map[string]Window{}
	addWindow := func(key string, raw chatGPTUsageWindow) {
		percent, ok := asFloat(raw.UsedPercent)
		if !ok {
			return
		}
		used := math.Max(0, math.Min(100, percent))
		window := Window{UsedPercent: &used}
		if seconds, ok := asFloat(raw.LimitWindowSecond); ok && seconds >= 0 {
			windowSeconds := int64(seconds)
			window.WindowSeconds = &windowSeconds
		}
		if reset, ok := parseResetAt(raw.ResetAt); ok {
			window.ResetAt = &reset
		}
		windows[key] = window
	}
	addWindow("5h", payload.RateLimit.Primary)
	addWindow("weekly", payload.RateLimit.Secondary)
	addWindow("monthly", payload.RateLimit.Tertiary)
	if balance, ok := quotaValueLabel(payload.Credits.Balance); ok {
		windows["credits_balance"] = Window{ValueLabel: balance}
	}
	if len(windows) == 0 {
		return nil, errNoQuotaData
	}
	return windows, nil
}

// quotaValueLabel renders a provider balance without accepting arbitrary JSON
// into the UI. String balances stay verbatim; numeric balances use a stable
// non-exponential representation.
func quotaValueLabel(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		s = strings.TrimSpace(s)
		return s, s != ""
	}
	v, ok := asFloat(raw)
	if !ok {
		return "", false
	}
	return strconv.FormatFloat(v, 'f', -1, 64), true
}

// parseOpenRouter parses GET {base}/credits: total_credits / total_usage
// (numbers or numeric strings). The remaining balance is shown alongside what
// was spent; missing fields yield an empty label.
func parseOpenRouter(body []byte) (map[string]Window, error) {
	var payload struct {
		Data struct {
			TotalCredits json.RawMessage `json:"total_credits"`
			TotalUsage   json.RawMessage `json:"total_usage"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	w := Window{}
	if credits, ok := asFloat(payload.Data.TotalCredits); ok {
		if usage, okUsage := asFloat(payload.Data.TotalUsage); okUsage {
			remaining := math.Max(0, credits-usage)
			w.ValueLabel = fmt.Sprintf("$%.2f left · $%.2f spent", remaining, usage)
		}
	}
	return map[string]Window{"credits": w}, nil
}

// parseCommandCode parses GET {base}/usage/summary: totalCost / totalCount / totalTokens
// or similar usage stats.
func parseCommandCode(body []byte) (map[string]Window, error) {
	var payload struct {
		TotalCost    json.RawMessage `json:"totalCost"`
		TotalCredits json.RawMessage `json:"totalCredits"`
		TotalTokens  json.RawMessage `json:"totalTokens"`
		TotalCount   json.RawMessage `json:"totalCount"`
		SuccessRate  json.RawMessage `json:"successRate"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	cost, okCost := asFloat(payload.TotalCost)
	count, okCount := asFloat(payload.TotalCount)
	if !okCost && !okCount {
		return nil, errNoQuotaData
	}
	var label string
	if okCost && okCount {
		label = fmt.Sprintf("$%.2f spent · %d requests", cost, int64(count))
	} else if okCost {
		label = fmt.Sprintf("$%.2f spent", cost)
	} else {
		label = fmt.Sprintf("%d requests", int64(count))
	}
	return map[string]Window{
		"usage": {ValueLabel: label},
	}, nil
}
