package secrets

import (
	"bytes"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
