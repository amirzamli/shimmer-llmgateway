package oauth

import (
	"strings"
	"testing"
)

// TestVerifiedConstants pins the OpenCode v1.18.30 contract recorded from
// packages/core/src/plugin/provider/openai.ts. Any drift here is a contract
// change that must be re-verified against the installed OpenCode build.
func TestVerifiedConstants(t *testing.T) {
	const (
		wantIssuer    = "https://auth.openai.com"
		wantClientID  = "app_EMoamEEZ73f0CkXaXp7hrann"
		wantToken     = "/oauth/token"
		wantExpiresIn = 3600
	)
	if Issuer != wantIssuer {
		t.Errorf("Issuer = %q, want %q", Issuer, wantIssuer)
	}
	if ClientID != wantClientID {
		t.Errorf("ClientID = %q, want %q", ClientID, wantClientID)
	}
	if TokenPath != wantToken {
		t.Errorf("TokenPath = %q, want %q", TokenPath, wantToken)
	}
	if DefaultExpiresIn != wantExpiresIn {
		t.Errorf("DefaultExpiresIn = %d, want %d", DefaultExpiresIn, wantExpiresIn)
	}
}

func TestConfigWithDefaults(t *testing.T) {
	c := Config{}.WithDefaults()
	if c.Issuer != Issuer || c.ClientID != ClientID {
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
