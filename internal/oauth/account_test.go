package oauth

import (
	"encoding/base64"
	"errors"
	"testing"
)

// base64url pads its output, producing the padded form some issuers send.
func base64url(s string) string {
	return base64.URLEncoding.EncodeToString([]byte(s))
}

func TestAccountIDTopLevelClaim(t *testing.T) {
	tok := makeJWT(map[string]any{"chatgpt_account_id": "acc-top"})
	id, err := AccountIDFromJWT(tok)
	if err != nil {
		t.Fatalf("AccountIDFromJWT: %v", err)
	}
	if id != "acc-top" {
		t.Errorf("id = %q, want acc-top", id)
	}
}

func TestAccountIDNamespacedClaim(t *testing.T) {
	tok := makeJWT(map[string]any{
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acc-ns"},
	})
	id, err := AccountIDFromJWT(tok)
	if err != nil {
		t.Fatalf("AccountIDFromJWT: %v", err)
	}
	if id != "acc-ns" {
		t.Errorf("id = %q, want acc-ns", id)
	}
}

func TestAccountIDOrganizationFallback(t *testing.T) {
	tok := makeJWT(map[string]any{
		"organizations": []map[string]any{{"id": "org-1"}, {"id": "org-2"}},
	})
	id, err := AccountIDFromJWT(tok)
	if err != nil {
		t.Fatalf("AccountIDFromJWT: %v", err)
	}
	if id != "org-1" {
		t.Errorf("id = %q, want org-1 (first organization)", id)
	}
}

func TestAccountIDClaimPrecedence(t *testing.T) {
	tok := makeJWT(map[string]any{
		"chatgpt_account_id":          "acc-top",
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acc-ns"},
		"organizations":               []map[string]any{{"id": "org-1"}},
	})
	id, err := AccountIDFromJWT(tok)
	if err != nil {
		t.Fatal(err)
	}
	if id != "acc-top" {
		t.Errorf("id = %q, want acc-top (top-level claim wins)", id)
	}

	tok2 := makeJWT(map[string]any{
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acc-ns"},
		"organizations":               []map[string]any{{"id": "org-1"}},
	})
	id2, err := AccountIDFromJWT(tok2)
	if err != nil {
		t.Fatal(err)
	}
	if id2 != "acc-ns" {
		t.Errorf("id = %q, want acc-ns (namespaced claim beats organizations)", id2)
	}
}

func TestAccountIDNotFound(t *testing.T) {
	tok := makeJWT(map[string]any{"sub": "user-1", "email": "u@example.com"})
	_, err := AccountIDFromJWT(tok)
	if !errors.Is(err, ErrAccountIDNotFound) {
		t.Errorf("err = %v, want ErrAccountIDNotFound", err)
	}
}

func TestAccountIDInvalidTokens(t *testing.T) {
	cases := []string{
		"",
		"not-a-jwt",
		"header.payload.missing-signature",
		"only-one-segment",
		"a.b", // second segment is not base64url
		"a.!!!.c",
	}
	for _, tc := range cases {
		if _, err := AccountIDFromJWT(tc); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("AccountIDFromJWT(%q) err = %v, want ErrInvalidToken", tc, err)
		}
	}
}

func TestAccountIDPaddedBase64(t *testing.T) {
	// A payload segment with padding must still decode (some issuers pad).
	header := base64url(`{"alg":"none"}`)
	payload := base64url(`{"chatgpt_account_id":"acc-pad"}`)
	id, err := AccountIDFromJWT(header + "." + payload + ".sig")
	if err != nil {
		t.Fatalf("AccountIDFromJWT padded: %v", err)
	}
	if id != "acc-pad" {
		t.Errorf("id = %q, want acc-pad", id)
	}
}

func TestExtractAccountIDPrecedence(t *testing.T) {
	idTok := makeJWT(map[string]any{"chatgpt_account_id": "acc-id"})
	atTok := makeJWT(map[string]any{"chatgpt_account_id": "acc-at"})
	raw := tokenResponse{IDToken: idTok, AccessToken: atTok}
	if id := extractAccountID(raw); id != "acc-id" {
		t.Errorf("id = %q, want acc-id (id_token wins)", id)
	}
	raw = tokenResponse{IDToken: makeJWT(map[string]any{"sub": "x"}), AccessToken: atTok}
	if id := extractAccountID(raw); id != "acc-at" {
		t.Errorf("id = %q, want acc-at (access token fallback)", id)
	}
	raw = tokenResponse{AccessToken: makeJWT(map[string]any{"sub": "x"})}
	if id := extractAccountID(raw); id != "" {
		t.Errorf("id = %q, want empty when no claims present", id)
	}
}

// TestComputeResidency pins the verified OpenCode v1.18.30 claim order for
// the x-openai-internal-codex-residency upstream header: the
// https://api.openai.com/auth namespace claim first, then the top-level
// chatgpt_compute_residency claim; absent and "no_constraint" values yield no
// header.
func TestComputeResidency(t *testing.T) {
	t.Run("namespaced claim wins", func(t *testing.T) {
		tok := makeJWT(map[string]any{
			"chatgpt_compute_residency": "us-east-1",
			"https://api.openai.com/auth": map[string]any{
				"chatgpt_compute_residency": "us-west-2",
			},
		})
		if got := ComputeResidency(tok); got != "us-west-2" {
			t.Errorf("ComputeResidency = %q, want us-west-2 (namespace first)", got)
		}
	})
	t.Run("top-level fallback", func(t *testing.T) {
		tok := makeJWT(map[string]any{"chatgpt_compute_residency": "eu-central-1"})
		if got := ComputeResidency(tok); got != "eu-central-1" {
			t.Errorf("ComputeResidency = %q, want eu-central-1", got)
		}
	})
	t.Run("no_constraint means no header", func(t *testing.T) {
		tok := makeJWT(map[string]any{"chatgpt_compute_residency": "no_constraint"})
		if got := ComputeResidency(tok); got != "" {
			t.Errorf("ComputeResidency(no_constraint) = %q, want empty", got)
		}
	})
	t.Run("absent claim means no header", func(t *testing.T) {
		tok := makeJWT(map[string]any{"chatgpt_account_id": "acc-1"})
		if got := ComputeResidency(tok); got != "" {
			t.Errorf("ComputeResidency = %q, want empty", got)
		}
	})
	t.Run("invalid token means no header", func(t *testing.T) {
		for _, tok := range []string{"", "not-a-jwt", "a.!!!.c"} {
			if got := ComputeResidency(tok); got != "" {
				t.Errorf("ComputeResidency(%q) = %q, want empty", tok, got)
			}
		}
	})
}
