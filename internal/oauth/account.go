package oauth

import (
	"encoding/base64"
	"encoding/json"
	"strings"
)

// Claims mirrors the verified OpenCode v1.18.30 claim shape for ChatGPT
// OAuth tokens (packages/core/src/plugin/provider/openai.ts): the account
// identifier appears as chatgpt_account_id, either top-level or inside the
// https://api.openai.com/auth namespace, with organizations[0].id as the
// final fallback; the compute residency appears as chatgpt_compute_residency
// in the same two places (namespace first, then top-level).
type Claims struct {
	ChatgptAccountID        string `json:"chatgpt_account_id"`
	ChatgptComputeResidency string `json:"chatgpt_compute_residency"`
	Organizations           []struct {
		ID string `json:"id"`
	} `json:"organizations"`
	Auth *struct {
		ChatgptAccountID        string `json:"chatgpt_account_id"`
		ChatgptComputeResidency string `json:"chatgpt_compute_residency"`
	} `json:"https://api.openai.com/auth"`
}

// ParseClaims decodes the payload segment of a JWT (id_token or access
// token). The token must have the standard header.payload[.signature] shape.
func ParseClaims(token string) (*Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return nil, errf("claims", ErrInvalidToken, "token is not a JWT")
	}
	payload, err := decodeBase64URL(parts[1])
	if err != nil {
		return nil, errf("claims", ErrInvalidToken, "token payload is not valid base64url")
	}
	var claims Claims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, errf("claims", ErrInvalidToken, "token payload is not valid JSON")
	}
	return &claims, nil
}

// AccountIDFromJWT extracts the ChatGPT account identifier from a JWT
// (id_token or access token) using the verified OpenCode v1.18.30 claim
// order: top-level chatgpt_account_id, then
// https://api.openai.com/auth.chatgpt_account_id, then organizations[0].id.
// It returns ErrInvalidToken when the token is not a decodable JWT and
// ErrAccountIDNotFound when no claim is present.
func AccountIDFromJWT(token string) (string, error) {
	claims, err := ParseClaims(token)
	if err != nil {
		return "", err
	}
	if claims.ChatgptAccountID != "" {
		return claims.ChatgptAccountID, nil
	}
	if claims.Auth != nil && claims.Auth.ChatgptAccountID != "" {
		return claims.Auth.ChatgptAccountID, nil
	}
	if len(claims.Organizations) > 0 && claims.Organizations[0].ID != "" {
		return claims.Organizations[0].ID, nil
	}
	return "", errf("claims", ErrAccountIDNotFound, "token claims carry no account id")
}

// ComputeResidency extracts the verified compute-residency value from a JWT
// (the access token) using the OpenCode v1.18.30 order: the
// https://api.openai.com/auth.chatgpt_compute_residency namespace claim
// first, then the top-level chatgpt_compute_residency claim. It returns ""
// when the token is not decodable or the claim is absent or "no_constraint";
// the empty result means the upstream request carries no residency header.
func ComputeResidency(token string) string {
	claims, err := ParseClaims(token)
	if err != nil {
		return ""
	}
	residency := ""
	if claims.Auth != nil {
		residency = claims.Auth.ChatgptComputeResidency
	}
	if residency == "" {
		residency = claims.ChatgptComputeResidency
	}
	if residency == "" || residency == "no_constraint" {
		return ""
	}
	return residency
}

// decodeBase64URL decodes an unpadded or padded base64url segment.
func decodeBase64URL(s string) ([]byte, error) {
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.URLEncoding.DecodeString(s)
}
