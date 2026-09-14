package secrets

import (
	"bytes"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// testKey is a fixed 32-byte AES-256 key used by tests.
var testKey = []byte("0123456789abcdef0123456789abcdef")

// otherKey is a distinct 32-byte key for wrong-key tests.
var otherKey = []byte("fedcba9876543210fedcba9876543210")

func TestOpenCreatesFileWith0600(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.db.secrets.json")
	s, err := Open(path, testKey)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// §6.2: chmod 0600 — the gateway user only.
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("secrets file mode = %o, want 0600", perm)
	}
	if _, ok := s.Get("openai"); ok {
		t.Error("fresh store returned a key for openai")
	}
	// The created file is an encrypted envelope, not a plaintext map.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"ciphertext"`) {
		t.Errorf("created store is not an encrypted envelope: %s", data)
	}
}

func TestSetGetDeleteRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.db.secrets.json")
	s, err := Open(path, testKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Set("openai", "sk-secret-123"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if key, ok := s.Get("openai"); !ok || key != "sk-secret-123" {
		t.Errorf("Get(openai) = %q, %v; want sk-secret-123, true", key, ok)
	}

	// A reload from disk sees the persisted key.
	s2, err := Open(path, testKey)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if key, ok := s2.Get("openai"); !ok || key != "sk-secret-123" {
		t.Errorf("reloaded Get(openai) = %q, %v; want sk-secret-123, true", key, ok)
	}

	if err := s.Delete("openai"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := s.Get("openai"); ok {
		t.Error("key still present after Delete")
	}
	s3, err := Open(path, testKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s3.Get("openai"); ok {
		t.Error("reloaded store still has deleted key")
	}
}

func TestSetEmptyClears(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.db.secrets.json")
	s, _ := Open(path, testKey)
	s.Set("a", "key-123")
	s.Set("a", "") // empty means clear
	if _, ok := s.Get("a"); ok {
		t.Error("empty Set did not clear the key")
	}
}

func TestRoundtripEnvelopeOnDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.db.secrets.json")
	s, err := Open(path, testKey)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Set("openai", "sk-secret-123"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if key, ok := s.Get("openai"); !ok || key != "sk-secret-123" {
		t.Errorf("Get(openai) = %q, %v; want sk-secret-123, true", key, ok)
	}

	// A reload with the same key decrypts.
	s2, err := Open(path, testKey)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if key, ok := s2.Get("openai"); !ok || key != "sk-secret-123" {
		t.Errorf("reloaded Get(openai) = %q, %v; want sk-secret-123, true", key, ok)
	}

	// On-disk bytes are an envelope: envelope fields present, no plaintext.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"version"`, `"ciphertext"`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("on-disk bytes missing %s: %s", want, data)
		}
	}
	if strings.Contains(string(data), "sk-secret-123") {
		t.Errorf("on-disk bytes contain the plaintext key: %s", data)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("secrets file mode = %o, want 0600", perm)
	}
}

func TestNonceFreshness(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.db.secrets.json")
	s, err := Open(path, testKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Set("a", "k1"); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Set("a", "k2"); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first, second) {
		t.Error("two Set writes produced identical on-disk bytes (nonce not fresh)")
	}
}

func TestWrongKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.db.secrets.json")
	s, err := Open(path, testKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Set("a", "k1"); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, otherKey); !errors.Is(err, ErrWrongKey) {
		t.Errorf("Open with wrong key = %v, want ErrWrongKey", err)
	}
}

func TestCorruptedEnvelope(t *testing.T) {
	cases := []struct {
		name string
		data string
	}{
		{"bad version", `{"version":99,"nonce":"AA==","ciphertext":"AA=="}`},
		{"bad base64 nonce", `{"version":1,"nonce":"!!!","ciphertext":"AA=="}`},
		{"garbage", `garbage`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "gateway.db.secrets.json")
			if err := os.WriteFile(path, []byte(c.data), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(path, testKey); !errors.Is(err, ErrCorrupted) {
				t.Errorf("Open = %v, want ErrCorrupted", err)
			}
		})
	}
}

func TestValidateMasterKey(t *testing.T) {
	valid := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0xAB}, 32))
	if err := ValidateMasterKey([]byte(valid)); err != nil {
		t.Errorf("valid 32-byte key rejected: %v", err)
	}
	short := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0xAB}, 16))
	if err := ValidateMasterKey([]byte(short)); !errors.Is(err, ErrInvalidMasterKey) {
		t.Errorf("16-byte key = %v, want ErrInvalidMasterKey", err)
	}
	if err := ValidateMasterKey([]byte("not base64!!!")); !errors.Is(err, ErrInvalidMasterKey) {
		t.Errorf("bad base64 = %v, want ErrInvalidMasterKey", err)
	}
}

func TestLegacyMigrationWithProvidedKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.db.secrets.json")
	if err := os.WriteFile(path, []byte("{\n  \"openai\": \"sk-legacy-123\"\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path, testKey)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if key, ok := s.Get("openai"); !ok || key != "sk-legacy-123" {
		t.Fatalf("Get(openai) = %q, %v; want sk-legacy-123, true", key, ok)
	}
	if s.PendingMigration() {
		t.Error("PendingMigration = true, want false (provided key migrates immediately)")
	}
	// The file was migrated in place to an encrypted envelope.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"ciphertext"`) {
		t.Errorf("migrated file is not an envelope: %s", data)
	}
	if strings.Contains(string(data), "sk-legacy-123") {
		t.Errorf("migrated file still contains the plaintext key")
	}
	// A reload with the same key decrypts the migrated envelope.
	s2, err := Open(path, testKey)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if key, ok := s2.Get("openai"); !ok || key != "sk-legacy-123" {
		t.Errorf("reloaded Get(openai) = %q, %v", key, ok)
	}
}

func TestLegacyMigrationDeferredWithGeneratedKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.db.secrets.json")
	if err := os.WriteFile(path, []byte("{\n  \"openai\": \"sk-legacy-123\"\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if key, ok := s.Get("openai"); !ok || key != "sk-legacy-123" {
		t.Fatalf("Get(openai) = %q, %v; want sk-legacy-123, true", key, ok)
	}
	if !s.PendingMigration() {
		t.Fatal("PendingMigration = false, want true")
	}
	genKey, ok := s.GeneratedMasterKey()
	if !ok || genKey == "" {
		t.Fatalf("GeneratedMasterKey = %q, %v; want a key, true", genKey, ok)
	}
	// The file must STILL be plaintext on disk until ACK.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "sk-legacy-123") {
		t.Errorf("deferred migration rewrote the file early: %s", data)
	}

	if err := s.AckMasterKey(); err != nil {
		t.Fatalf("AckMasterKey: %v", err)
	}
	if s.PendingMigration() {
		t.Error("PendingMigration = true after ACK")
	}
	if _, ok := s.GeneratedMasterKey(); ok {
		t.Error("GeneratedMasterKey still exposed after ACK")
	}
	// The file is now an encrypted envelope.
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"ciphertext"`) {
		t.Errorf("acked file is not an envelope: %s", data)
	}
	if strings.Contains(string(data), "sk-legacy-123") {
		t.Errorf("acked file contains the plaintext key")
	}
	// Re-open with the original generated key decrypts.
	rawKey, err := base64.StdEncoding.DecodeString(genKey)
	if err != nil {
		t.Fatalf("decode generated key: %v", err)
	}
	s2, err := Open(path, rawKey)
	if err != nil {
		t.Fatalf("re-open with original key: %v", err)
	}
	if key, ok := s2.Get("openai"); !ok || key != "sk-legacy-123" {
		t.Errorf("reopened Get(openai) = %q, %v", key, ok)
	}
	// Re-open with nil on the now-encrypted file refuses.
	if _, err := Open(path, nil); !errors.Is(err, ErrEncryptedWithoutKey) {
		t.Errorf("re-open with nil = %v, want ErrEncryptedWithoutKey", err)
	}
}

func TestPendingMigrationRejectsMutationsUntilAck(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.db.secrets.json")
	legacy := []byte("{\n  \"openai\": \"sk-legacy-123\"\n}\n")
	if err := os.WriteFile(path, legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.SetOAuth("chatgpt", testCred()); !errors.Is(err, ErrMigrationPending) {
		t.Fatalf("SetOAuth before ACK = %v, want ErrMigrationPending", err)
	}
	if err := s.Set("new", "sk-new"); !errors.Is(err, ErrMigrationPending) {
		t.Fatalf("Set before ACK = %v, want ErrMigrationPending", err)
	}
	if err := s.Delete("openai"); !errors.Is(err, ErrMigrationPending) {
		t.Fatalf("Delete before ACK = %v, want ErrMigrationPending", err)
	}
	if err := s.Move("openai", "renamed", nil); !errors.Is(err, ErrMigrationPending) {
		t.Fatalf("Move before ACK = %v, want ErrMigrationPending", err)
	}
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(onDisk, legacy) {
		t.Errorf("rejected mutation before ACK rewrote the legacy file: %s", onDisk)
	}
	if key, ok := s.Get("openai"); !ok || key != "sk-legacy-123" {
		t.Errorf("legacy key after rejected Delete = (%q, %v), want original", key, ok)
	}
	if _, ok := s.Get("new"); ok {
		t.Error("rejected Set left a new key in memory")
	}
	if _, ok := s.Get("renamed"); ok {
		t.Error("rejected Move left a destination in memory")
	}
	if _, ok, err := s.GetOAuth("chatgpt"); err != nil || ok {
		t.Errorf("rejected OAuth mutation = (%v, %v), want absent", ok, err)
	}

	genKey, ok := s.GeneratedMasterKey()
	if !ok || genKey == "" {
		t.Fatal("generated key was cleared before ACK")
	}
	if err := s.AckMasterKey(); err != nil {
		t.Fatalf("AckMasterKey: %v", err)
	}
	onDisk, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(onDisk), `"ciphertext"`) || strings.Contains(string(onDisk), "sk-legacy-123") {
		t.Errorf("ACK did not encrypt the legacy map: %s", onDisk)
	}
	rawKey, err := base64.StdEncoding.DecodeString(genKey)
	if err != nil {
		t.Fatalf("decode generated key: %v", err)
	}
	s2, err := Open(path, rawKey)
	if err != nil {
		t.Fatalf("re-open with generated key: %v", err)
	}
	if key, ok := s2.Get("openai"); !ok || key != "sk-legacy-123" {
		t.Errorf("reopened legacy key = (%q, %v), want original", key, ok)
	}
}

func TestGeneratedKeyLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.db.secrets.json")
	s, err := Open(path, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	key, ok := s.GeneratedMasterKey()
	if !ok || key == "" {
		t.Fatalf("GeneratedMasterKey = %q, %v; want a key, true", key, ok)
	}
	if err := s.AckMasterKey(); err != nil {
		t.Fatalf("AckMasterKey: %v", err)
	}
	if _, ok := s.GeneratedMasterKey(); ok {
		t.Error("GeneratedMasterKey still exposed after ACK")
	}
	// The fresh empty store is encrypted on disk; reopening without a key
	// refuses rather than silently re-generating.
	if _, err := Open(path, nil); !errors.Is(err, ErrEncryptedWithoutKey) {
		t.Errorf("re-open with nil = %v, want ErrEncryptedWithoutKey", err)
	}
}

func TestWorkingKeySurvivesAck(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.db.secrets.json")
	s, err := Open(path, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	key, ok := s.GeneratedMasterKey()
	if !ok || key == "" {
		t.Fatalf("no generated key exposed")
	}
	if err := s.AckMasterKey(); err != nil {
		t.Fatalf("AckMasterKey: %v", err)
	}
	// ACK must not zero the working key: Set still persists.
	if err := s.Set("openai", "sk-post-ack"); err != nil {
		t.Fatalf("Set after ACK: %v", err)
	}
	rawKey, err := base64.StdEncoding.DecodeString(key)
	if err != nil {
		t.Fatalf("decode generated key: %v", err)
	}
	s2, err := Open(path, rawKey)
	if err != nil {
		t.Fatalf("re-open with the originally exposed key: %v", err)
	}
	if k, ok := s2.Get("openai"); !ok || k != "sk-post-ack" {
		t.Errorf("Get(openai) after ACK = %q, %v; want sk-post-ack, true", k, ok)
	}
}

func TestEmptyFileTreatedAsMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.db.secrets.json")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path, testKey)
	if err != nil {
		t.Fatalf("Open empty file: %v", err)
	}
	if _, ok := s.Get("a"); ok {
		t.Error("empty-file store returned a key")
	}
	// Persisting proves the empty file was treated as a fresh store and not
	// parsed as a map.
	if err := s.Set("a", "k1"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"ciphertext"`) {
		t.Errorf("persisted store is not an envelope: %s", data)
	}
}

func TestMaskKey(t *testing.T) {
	cases := []struct{ in, want string }{
		{"sk-abcdefghijklmnop1234", "sk-…1234"},
		{"short", "••••••"}, // ≤8 chars never revealed
	}
	for _, c := range cases {
		got := MaskKey(c.in)
		if got != c.want {
			t.Errorf("MaskKey(%q) = %q, want %q", c.in, got, c.want)
		}
		if strings.Contains(got, c.in) {
			t.Errorf("MaskKey(%q) leaked the full key in %q", c.in, got)
		}
	}
}

func TestOpenInvalidJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.db.secrets.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, testKey); !errors.Is(err, ErrCorrupted) {
		t.Fatalf("Open of malformed secrets file = %v, want ErrCorrupted", err)
	}
}

// TestResolveKey verifies the single env-var-then-secrets-file key resolution
// shared by the gateway forward path, the /models fetch, and the quota fetcher.
func TestResolveKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.db.secrets.json")
	s, err := Open(path, testKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Set("openai", "sk-stored"); err != nil {
		t.Fatal(err)
	}

	// 1. Env var wins over the secrets file.
	t.Setenv("TEST_KEY_1", "sk-env")
	if key, ok := s.ResolveKey("TEST_KEY_1", "openai"); !ok || key != "sk-env" {
		t.Errorf("ResolveKey with env set = (%q, %v), want (sk-env, true)", key, ok)
	}

	// 2. Env var unset (or empty) falls back to the secrets file.
	t.Setenv("TEST_KEY_1", "")
	if key, ok := s.ResolveKey("TEST_KEY_1", "openai"); !ok || key != "sk-stored" {
		t.Errorf("ResolveKey with empty env = (%q, %v), want (sk-stored, true)", key, ok)
	}

	// 3. No env configured (keyless provider) and no stored key → not found.
	if key, ok := s.ResolveKey("", "ollama"); ok || key != "" {
		t.Errorf("ResolveKey keyless = (%q, %v), want ('', false)", key, ok)
	}

	// 4. Env configured-but-unset and no stored key → not found (callers turn
	//    this into a misconfiguration error).
	t.Setenv("NEVER_SET_ENV", "")
	if key, ok := s.ResolveKey("NEVER_SET_ENV", "openai-2"); ok || key != "" {
		t.Errorf("ResolveKey missing = (%q, %v), want ('', false)", key, ok)
	}
}

// ---- OAuth credential records ----

// testCred is a fixed OAuth credential used by the record tests.
func testCred() OAuthCredential {
	return OAuthCredential{
		AccessToken:  "at-secret-access-token",
		RefreshToken: "rt-secret-refresh-token",
		ExpiresAt:    time.Now().Add(time.Hour),
		AccountID:    "acc_123",
	}
}

func TestOAuthRoundTripAndReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.db.secrets.json")
	s, err := Open(path, testKey)
	if err != nil {
		t.Fatal(err)
	}
	cred := testCred()
	if err := s.SetOAuth("chatgpt", cred); err != nil {
		t.Fatalf("SetOAuth: %v", err)
	}
	got, ok, err := s.GetOAuth("chatgpt")
	if err != nil || !ok {
		t.Fatalf("GetOAuth = (%+v, %v, %v); want record, true, nil", got, ok, err)
	}
	if got.AccessToken != cred.AccessToken || got.RefreshToken != cred.RefreshToken || got.AccountID != cred.AccountID {
		t.Errorf("GetOAuth = %+v, want %+v", got, cred)
	}
	if !got.ExpiresAt.Equal(cred.ExpiresAt) {
		t.Errorf("ExpiresAt = %v, want %v", got.ExpiresAt, cred.ExpiresAt)
	}
	if got.Version != oauthRecordVersion {
		t.Errorf("Version = %d, want %d", got.Version, oauthRecordVersion)
	}

	// A reload from disk sees the persisted record.
	s2, err := Open(path, testKey)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	got2, ok, err := s2.GetOAuth("chatgpt")
	if err != nil || !ok {
		t.Fatalf("reloaded GetOAuth = (%+v, %v, %v); want record, true, nil", got2, ok, err)
	}
	if got2.AccessToken != cred.AccessToken || got2.RefreshToken != cred.RefreshToken {
		t.Errorf("reloaded record = %+v, want %+v", got2, cred)
	}
}

func TestOAuthRecordNoPlaintextOnDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.db.secrets.json")
	s, err := Open(path, testKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetOAuth("chatgpt", testCred()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"at-secret-access-token", "rt-secret-refresh-token", "acc_123"} {
		if strings.Contains(string(data), secret) {
			t.Errorf("on-disk bytes contain plaintext %q", secret)
		}
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("secrets file mode = %o, want 0600", perm)
	}
}

func TestOAuthAndLegacyKeyCoexist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.db.secrets.json")
	s, err := Open(path, testKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Set("openai", "sk-api-key-123"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetOAuth("chatgpt", testCred()); err != nil {
		t.Fatal(err)
	}
	// The API-key alias still resolves as a key...
	if key, ok := s.ResolveKey("", "openai"); !ok || key != "sk-api-key-123" {
		t.Errorf("ResolveKey(openai) = (%q, %v), want (sk-api-key-123, true)", key, ok)
	}
	// ...and GetOAuth reports it is not a record (no error).
	if _, ok, err := s.GetOAuth("openai"); err != nil || ok {
		t.Errorf("GetOAuth(openai) = (%v, %v); want false, nil", ok, err)
	}
	// The OAuth alias resolves as a record...
	if _, ok, err := s.GetOAuth("chatgpt"); err != nil || !ok {
		t.Errorf("GetOAuth(chatgpt) = (%v, %v); want true, nil", ok, err)
	}
	// ...and never as an API key.
	if key, ok := s.ResolveKey("", "chatgpt"); ok || key != "" {
		t.Errorf("ResolveKey(chatgpt) = (%q, %v), want ('', false) — an OAuth record is not a key", key, ok)
	}
	// A reload sees both values (the plaintext shape stayed the legacy map).
	s2, err := Open(path, testKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s2.GetOAuth("chatgpt"); !ok {
		t.Error("reloaded store lost the OAuth record")
	}
	if _, ok := s2.Get("openai"); !ok {
		t.Error("reloaded store lost the legacy key")
	}
}

func TestResolveKeyEnvStillWinsOverOAuthRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.db.secrets.json")
	s, err := Open(path, testKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetOAuth("chatgpt", testCred()); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_OAUTH_ENV", "sk-env-key")
	// The env var is a real key and always wins — even for an alias whose
	// stored value is an OAuth record (defensive: no path should send the
	// record as a Bearer token).
	if key, ok := s.ResolveKey("TEST_OAUTH_ENV", "chatgpt"); !ok || key != "sk-env-key" {
		t.Errorf("ResolveKey with env = (%q, %v), want (sk-env-key, true)", key, ok)
	}
}

func TestLegacyPlaintextFileWithOAuthRecordMigrates(t *testing.T) {
	// A legacy plaintext file whose value happens to carry the marker prefix
	// (written by an older tool) is read as a record after migration.
	cred := testCred()
	encoded, err := encodeOAuthRecord(cred)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "gateway.db.secrets.json")
	legacy := "{\n  \"openai\": \"sk-legacy-123\",\n  \"chatgpt\": " + strconv.Quote(encoded) + "\n}\n"
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path, testKey)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if key, ok := s.Get("openai"); !ok || key != "sk-legacy-123" {
		t.Errorf("Get(openai) = (%q, %v), want (sk-legacy-123, true)", key, ok)
	}
	got, ok, err := s.GetOAuth("chatgpt")
	if err != nil || !ok {
		t.Fatalf("GetOAuth(chatgpt) = (%v, %v); want true, nil", ok, err)
	}
	if got.AccessToken != cred.AccessToken || got.AccountID != cred.AccountID {
		t.Errorf("migrated record = %+v, want %+v", got, cred)
	}
	// The file was migrated in place to an encrypted envelope.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"ciphertext"`) {
		t.Errorf("migrated file is not an envelope: %s", data)
	}
	if strings.Contains(string(data), "at-secret-access-token") {
		t.Errorf("migrated file still contains plaintext token material")
	}
}

func TestOAuthDeleteAndReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.db.secrets.json")
	s, err := Open(path, testKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetOAuth("chatgpt", testCred()); err != nil {
		t.Fatal(err)
	}
	// Delete clears the record.
	if err := s.Delete("chatgpt"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok, err := s.GetOAuth("chatgpt"); err != nil || ok {
		t.Errorf("GetOAuth after Delete = (%v, %v), want false, nil", ok, err)
	}
	// Set (API key) replaces a record; SetOAuth replaces a key.
	if err := s.Set("chatgpt", "sk-now-a-key"); err != nil {
		t.Fatal(err)
	}
	if key, ok := s.ResolveKey("", "chatgpt"); !ok || key != "sk-now-a-key" {
		t.Errorf("ResolveKey after Set = (%q, %v), want (sk-now-a-key, true)", key, ok)
	}
	if err := s.SetOAuth("chatgpt", testCred()); err != nil {
		t.Fatal(err)
	}
	if key, ok := s.ResolveKey("", "chatgpt"); ok || key != "" {
		t.Errorf("ResolveKey after SetOAuth = (%q, %v), want ('', false)", key, ok)
	}
	if _, ok, err := s.GetOAuth("chatgpt"); err != nil || !ok {
		t.Errorf("GetOAuth after SetOAuth = (%v, %v), want true, nil", ok, err)
	}
}

func TestMoveCredentialAndReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.db.secrets.json")
	s, err := Open(path, testKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetOAuth("old", testCred()); err != nil {
		t.Fatal(err)
	}
	if err := s.Move("old", "new", nil); err != nil {
		t.Fatalf("Move OAuth credential: %v", err)
	}
	if _, ok, err := s.GetOAuth("old"); err != nil || ok {
		t.Errorf("old credential after Move = (%v, %v), want absent", ok, err)
	}
	if _, ok, err := s.GetOAuth("new"); err != nil || !ok {
		t.Errorf("new credential after Move = (%v, %v), want present", ok, err)
	}
	replacement := "sk-replacement"
	if err := s.Move("new", "final", &replacement); err != nil {
		t.Fatalf("Move with replacement: %v", err)
	}
	if _, ok, err := s.GetOAuth("new"); err != nil || ok {
		t.Errorf("source after replacement = (%v, %v), want absent", ok, err)
	}
	if key, ok := s.ResolveKey("", "final"); !ok || key != replacement {
		t.Errorf("replacement destination = (%q, %v), want %q", key, ok, replacement)
	}
}

func TestMoveRollsBackOnPersistenceFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.db.secrets.json")
	s, err := Open(path, testKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Set("old", "sk-old"); err != nil {
		t.Fatal(err)
	}
	originalPath := s.path
	s.path = filepath.Join(t.TempDir(), "missing", "gateway.db.secrets.json")
	replacement := "sk-new"
	if err := s.Move("old", "new", &replacement); err == nil {
		t.Fatal("Move with an unwritable path succeeded, want error")
	}
	if key, ok := s.Get("old"); !ok || key != "sk-old" {
		t.Errorf("source after failed Move = (%q, %v), want sk-old", key, ok)
	}
	if _, ok := s.Get("new"); ok {
		t.Error("destination after failed Move still contains a value")
	}
	s.path = originalPath
	s2, err := Open(originalPath, testKey)
	if err != nil {
		t.Fatalf("re-open after failed Move: %v", err)
	}
	if key, ok := s2.Get("old"); !ok || key != "sk-old" {
		t.Errorf("on-disk source after failed Move = (%q, %v), want sk-old", key, ok)
	}
}

func TestSetOAuthRollsBackOnPersistenceFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.db.secrets.json")
	s, err := Open(path, testKey)
	if err != nil {
		t.Fatal(err)
	}
	old := testCred()
	if err := s.SetOAuth("chatgpt", old); err != nil {
		t.Fatal(err)
	}
	// Force the atomic temp-file creation to fail without touching the original
	// persisted file.
	s.path = filepath.Join(t.TempDir(), "missing", "gateway.db.secrets.json")
	replacement := testCred()
	replacement.AccessToken = "replacement-access-token"
	if err := s.SetOAuth("chatgpt", replacement); err == nil {
		t.Fatal("SetOAuth with an unwritable path succeeded, want error")
	}
	got, ok, err := s.GetOAuth("chatgpt")
	if err != nil || !ok {
		t.Fatalf("GetOAuth after failed SetOAuth = (%+v, %v, %v), want old record", got, ok, err)
	}
	if got.AccessToken != old.AccessToken || got.RefreshToken != old.RefreshToken {
		t.Errorf("failed SetOAuth changed in-memory record = %+v, want old record", got)
	}
}

func TestSetOAuthRequiresAccessToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.db.secrets.json")
	s, err := Open(path, testKey)
	if err != nil {
		t.Fatal(err)
	}
	cred := testCred()
	cred.AccessToken = ""
	if err := s.SetOAuth("chatgpt", cred); err == nil {
		t.Error("SetOAuth with empty access token succeeded, want error")
	}
}

func TestGetOAuthCorruptedRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.db.secrets.json")
	s, err := Open(path, testKey)
	if err != nil {
		t.Fatal(err)
	}
	// A marker-prefixed value that is not a valid record body.
	if err := s.Set("chatgpt", oauthRecordPrefix+"{not json"); err != nil {
		t.Fatal(err)
	}
	_, ok, err := s.GetOAuth("chatgpt")
	if err == nil {
		t.Fatal("GetOAuth of corrupted record = nil error, want error")
	}
	if ok {
		t.Error("GetOAuth of corrupted record returned ok=true")
	}
	if strings.Contains(err.Error(), "{not json") {
		t.Errorf("corruption error leaked the stored value: %q", err)
	}
	// Unsupported versions are rejected without leaking the value.
	raw, err := encodeOAuthRecord(testCred())
	if err != nil {
		t.Fatal(err)
	}
	raw = strings.Replace(raw, `"v":1`, `"v":99`, 1)
	if err := s.Set("chatgpt", raw); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.GetOAuth("chatgpt"); err == nil {
		t.Error("GetOAuth of version-99 record = nil error, want error")
	}
}

func TestOAuthCredentialExpiredSkew(t *testing.T) {
	now := time.Now()
	if testCred().Expired(now, 0) {
		t.Error("fresh credential reported expired")
	}
	cred := testCred()
	cred.ExpiresAt = now.Add(20 * time.Second)
	if !cred.Expired(now, 30*time.Second) {
		t.Error("credential inside the skew window reported fresh")
	}
	cred.ExpiresAt = time.Time{}
	if cred.Expired(now, 0) {
		t.Error("zero-expiry credential reported expired")
	}
}
