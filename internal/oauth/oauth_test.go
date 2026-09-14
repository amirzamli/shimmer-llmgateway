package oauth

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestVerifiedConstants pins the OpenCode v1.18.30 contract recorded from
// packages/core/src/plugin/provider/openai.ts. Any drift here is a contract
// change that must be re-verified against the installed OpenCode build.
func TestVerifiedConstants(t *testing.T) {
	const (
		wantIssuer     = "https://auth.openai.com"
		wantClientID   = "app_EMoamEEZ73f0CkXaXp7hrann"
		wantRedirect   = "http://localhost:1455/auth/callback"
		wantScope      = "openid profile email offline_access"
		wantAuthorize  = "/oauth/authorize"
		wantToken      = "/oauth/token"
		wantOriginator = "opencode"
		wantExpiresIn  = 3600
		wantChallengeM = "S256"
	)
	if Issuer != wantIssuer {
		t.Errorf("Issuer = %q, want %q", Issuer, wantIssuer)
	}
	if ClientID != wantClientID {
		t.Errorf("ClientID = %q, want %q", ClientID, wantClientID)
	}
	if RedirectURI != wantRedirect {
		t.Errorf("RedirectURI = %q, want %q", RedirectURI, wantRedirect)
	}
	if Scope != wantScope {
		t.Errorf("Scope = %q, want %q", Scope, wantScope)
	}
	if AuthorizePath != wantAuthorize {
		t.Errorf("AuthorizePath = %q, want %q", AuthorizePath, wantAuthorize)
	}
	if TokenPath != wantToken {
		t.Errorf("TokenPath = %q, want %q", TokenPath, wantToken)
	}
	if DefaultExpiresIn != wantExpiresIn {
		t.Errorf("DefaultExpiresIn = %d, want %d", DefaultExpiresIn, wantExpiresIn)
	}
	if CodeChallengeMethod != wantChallengeM {
		t.Errorf("CodeChallengeMethod = %q, want %q", CodeChallengeMethod, wantChallengeM)
	}
	if Originator != wantOriginator {
		t.Errorf("Originator = %q, want %q", Originator, wantOriginator)
	}
}

func TestConfigWithDefaults(t *testing.T) {
	c := Config{}.WithDefaults()
	if c.Issuer != Issuer || c.ClientID != ClientID || c.RedirectURI != RedirectURI || c.Scope != Scope {
		t.Errorf("WithDefaults did not fill all fields: %+v", c)
	}
	if c.UserAgent == "" {
		t.Error("WithDefaults left UserAgent empty")
	}
	// Explicit fields must survive WithDefaults.
	c2 := Config{Issuer: "https://example.test", UserAgent: "custom"}.WithDefaults()
	if c2.Issuer != "https://example.test" || c2.UserAgent != "custom" {
		t.Errorf("WithDefaults overwrote explicit fields: %+v", c2)
	}
	if c2.ClientID != ClientID {
		t.Errorf("WithDefaults did not fill ClientID: %+v", c2)
	}
}

func TestGenerateVerifierRFC7636(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		v, err := GenerateVerifier()
		if err != nil {
			t.Fatalf("GenerateVerifier: %v", err)
		}
		if len(v) != verifierLength {
			t.Fatalf("verifier length = %d, want %d", len(v), verifierLength)
		}
		for _, r := range v {
			if !strings.ContainsRune(verifierChars, r) {
				t.Fatalf("verifier contains %q outside RFC 7636 alphabet", r)
			}
		}
		if seen[v] {
			t.Fatal("verifier repeated across generations")
		}
		seen[v] = true
	}
}

func TestValidateVerifier(t *testing.T) {
	valid, err := GenerateVerifier()
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateVerifier(valid); err != nil {
		t.Errorf("ValidateVerifier(%q) = %v, want nil", valid, err)
	}
	cases := []string{
		"",
		"too-short-verifier",
		strings.Repeat("a", 42),  // below RFC 7636 minimum
		strings.Repeat("a", 129), // above RFC 7636 maximum
		valid + "!",              // outside alphabet
		valid + "+",              // base64url char not in RFC 7636 alphabet
	}
	for _, tc := range cases {
		if err := ValidateVerifier(tc); err == nil {
			t.Errorf("ValidateVerifier(%q) = nil, want error", tc)
		}
	}
}

func TestS256ChallengeAndMatch(t *testing.T) {
	verifier, err := GenerateVerifier()
	if err != nil {
		t.Fatal(err)
	}
	challenge, err := S256Challenge(verifier)
	if err != nil {
		t.Fatalf("S256Challenge: %v", err)
	}
	if challenge == "" || strings.Contains(challenge, "=") {
		t.Fatalf("challenge %q must be non-empty and unpadded base64url", challenge)
	}
	if !PKCEMatches(verifier, challenge) {
		t.Error("PKCEMatches(verifier, own challenge) = false, want true")
	}
	other, _ := GenerateVerifier()
	if PKCEMatches(other, challenge) {
		t.Error("PKCEMatches(other verifier, challenge) = true, want false")
	}
	if PKCEMatches("bad", challenge) {
		t.Error("PKCEMatches(invalid verifier, challenge) = true, want false")
	}
	if PKCEMatches(verifier, "") {
		t.Error("PKCEMatches(verifier, empty challenge) = true, want false")
	}
}

func TestNewState(t *testing.T) {
	before := time.Now()
	st, err := NewState("inst-1", 10*time.Minute)
	if err != nil {
		t.Fatalf("NewState: %v", err)
	}
	after := time.Now()
	if st.Value == "" || st.Verifier == "" || st.Challenge == "" {
		t.Fatalf("NewState left fields empty: %+v", st)
	}
	if st.InstanceID != "inst-1" {
		t.Errorf("InstanceID = %q, want inst-1", st.InstanceID)
	}
	if !PKCEMatches(st.Verifier, st.Challenge) {
		t.Error("state challenge does not match its verifier")
	}
	if st.Used {
		t.Error("fresh state marked Used")
	}
	if st.CreatedAt.Before(before) || st.CreatedAt.After(after) {
		t.Errorf("CreatedAt %v outside [%v, %v]", st.CreatedAt, before, after)
	}
	if !st.ExpiresAt.Equal(st.CreatedAt.Add(10 * time.Minute)) {
		t.Errorf("ExpiresAt = %v, want CreatedAt+10m", st.ExpiresAt)
	}
	if st.Expired(after) {
		t.Error("fresh state reports expired")
	}
	if !st.Expired(after.Add(11 * time.Minute)) {
		t.Error("state does not report expiry after ttl")
	}
	if _, err := NewState("inst", 0); err == nil {
		t.Error("NewState with zero ttl: want error")
	}
	if _, err := NewState("inst", -time.Second); err == nil {
		t.Error("NewState with negative ttl: want error")
	}
}

func TestNewStateUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		st, err := NewState("inst", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if seen[st.Value] {
			t.Fatal("state value repeated across generations")
		}
		seen[st.Value] = true
	}
}

// TestAuthorizeURL matches the verified OpenCode v1.18.30 authorize request
// parameter for parameter (including id_token_add_organizations,
// codex_cli_simplified_flow, and originator).
func TestAuthorizeURL(t *testing.T) {
	st, err := NewState("inst", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := Config{}.AuthorizeURL(st)
	if err != nil {
		t.Fatalf("AuthorizeURL: %v", err)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("AuthorizeURL returned unparseable URL %q: %v", raw, err)
	}
	if u.Scheme != "https" || u.Host != "auth.openai.com" || u.Path != AuthorizePath {
		t.Errorf("URL = %s, want https://auth.openai.com%s", u, AuthorizePath)
	}
	q := u.Query()
	want := map[string]string{
		"response_type":              "code",
		"client_id":                  ClientID,
		"redirect_uri":               RedirectURI,
		"scope":                      Scope,
		"code_challenge":             st.Challenge,
		"code_challenge_method":      "S256",
		"id_token_add_organizations": "true",
		"codex_cli_simplified_flow":  "true",
		"state":                      st.Value,
		"originator":                 "opencode",
	}
	for k, v := range want {
		if got := q.Get(k); got != v {
			t.Errorf("authorize param %s = %q, want %q", k, got, v)
		}
	}
	if len(q) != len(want) {
		t.Errorf("authorize URL has %d params, want %d: %v", len(q), len(want), q)
	}
	if !strings.Contains(raw, "redirect_uri=http%3A%2F%2Flocalhost%3A1455%2Fauth%2Fcallback") {
		t.Errorf("redirect_uri not percent-encoded in %q", raw)
	}
}

func TestAuthorizeURLInvalidState(t *testing.T) {
	if _, err := (Config{}).AuthorizeURL(nil); err == nil {
		t.Error("AuthorizeURL(nil) = nil error, want error")
	}
	if _, err := (Config{}).AuthorizeURL(&State{Value: "x"}); err == nil {
		t.Error("AuthorizeURL(state without challenge) = nil error, want error")
	}
	if _, err := (Config{}).AuthorizeURL(&State{Challenge: "x"}); err == nil {
		t.Error("AuthorizeURL(state without value) = nil error, want error")
	}
}

func TestAuthorizeURLCustomConfig(t *testing.T) {
	st, err := NewState("inst", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := Config{Issuer: "https://auth.example.test"}.AuthorizeURL(st)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(raw, "https://auth.example.test/oauth/authorize?") {
		t.Errorf("custom issuer not honored: %q", raw)
	}
}
