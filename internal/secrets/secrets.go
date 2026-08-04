// Package secrets manages the UI-managed API keys file <store>.secrets.json
// (§6.2). Keys are stored alias→key encrypted at rest with AES-256-GCM using
// the SHIMMER_MASTER_KEY env var (base64, 32 bytes) or a key the gateway
// generates on first run; the file is chmod 0600 and keys never appear in
// gateway.toml or logs. Key precedence is env-var-then-file: the gateway uses
// an instance's api_key_env when set, falling back to this store.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// envelopeVersion is the on-disk envelope format version.
const envelopeVersion = 1

// Error sentinels: distinct startup failures per the encryption behavior
// matrix. None of the errors ever include key material.
var (
	// ErrEncryptedWithoutKey reports an encrypted secrets file opened with no
	// master key (SHIMMER_MASTER_KEY unset).
	ErrEncryptedWithoutKey = errors.New("secrets file is encrypted — set SHIMMER_MASTER_KEY")
	// ErrWrongKey reports an AES-GCM authentication failure: the provided key
	// does not decrypt the envelope (or the file is corrupted).
	ErrWrongKey = errors.New("wrong master key (or file corrupted)")
	// ErrCorrupted reports a secrets file that is neither a valid envelope nor
	// a legacy plaintext map.
	ErrCorrupted = errors.New("secrets file is corrupted or unparseable")
	// ErrInvalidMasterKey reports a malformed SHIMMER_MASTER_KEY value (bad
	// base64 or decoded length ≠ 32 bytes). The env value itself is never
	// echoed.
	ErrInvalidMasterKey = errors.New("invalid or malformed SHIMMER_MASTER_KEY")
)

// envelope is the on-disk encrypted format: the plaintext alias→key JSON map
// wrapped in AES-256-GCM.
type envelope struct {
	Version    int    `json:"version"`
	Nonce      string `json:"nonce"`
	Ciphertext string `json:"ciphertext"`
}

// Store holds the alias→key mapping. It is safe for concurrent use.
type Store struct {
	mu   sync.RWMutex
	path string
	keys map[string]string

	// masterKey is the working AES-256 key retained for the process lifetime;
	// used by every persist. Never zeroed.
	masterKey []byte
	// generatedKey is the one-time exposure copy (base64 std encoding) set
	// only when the gateway generated the key (nil passed to Open); cleared by
	// AckMasterKey.
	generatedKey string
	// pendingMigration is set when a legacy plaintext file was loaded with a
	// generated key: the file stays plaintext on disk until AckMasterKey
	// persists the encrypted form.
	pendingMigration bool
}

// Open loads (or creates, with mode 0600) the secrets file at path. A nil
// masterKey generates a fresh 32-byte key, exposed once via
// GeneratedMasterKey; a non-nil key must be the decoded 32-byte AES-256 key
// (validate the raw env value with ValidateMasterKey). Existing contents are
// read when present; a missing or empty file is created encrypted-empty.
func Open(path string, masterKey []byte) (*Store, error) {
	s := &Store{path: path, keys: map[string]string{}}
	if masterKey == nil {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, fmt.Errorf("secrets: generate master key: %w", err)
		}
		s.masterKey = key
		s.generatedKey = base64.StdEncoding.EncodeToString(key)
	} else {
		s.masterKey = masterKey
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			if err := s.persistLocked(); err != nil {
				return nil, err
			}
			return s, nil
		}
		return nil, fmt.Errorf("secrets: read %s: %w", path, err)
	}
	if len(data) == 0 {
		// Empty file ≡ missing file: fresh empty store, encrypted on persist.
		if err := s.persistLocked(); err != nil {
			return nil, err
		}
		return s, nil
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("secrets: parse %s: %w", path, ErrCorrupted)
	}
	if isEnvelope(raw) {
		if masterKey == nil {
			return nil, fmt.Errorf("secrets: %s: %w", path, ErrEncryptedWithoutKey)
		}
		keys, err := decryptEnvelope(data, s.masterKey)
		if err != nil {
			return nil, fmt.Errorf("secrets: decrypt %s: %w", path, err)
		}
		s.keys = keys
		return s, nil
	}

	// Legacy plaintext alias→key map.
	if err := json.Unmarshal(data, &s.keys); err != nil {
		return nil, fmt.Errorf("secrets: parse %s: %w", path, ErrCorrupted)
	}
	if masterKey == nil {
		// Generated key: defer migration until AckMasterKey so a crash before
		// ACK never destroys the only copy of real secrets (the file stays
		// plaintext on disk until then).
		s.pendingMigration = true
		return s, nil
	}
	// Provided key: migrate in place immediately.
	if err := s.persistLocked(); err != nil {
		return nil, err
	}
	return s, nil
}

// ValidateMasterKey checks a raw SHIMMER_MASTER_KEY env value: it must be
// valid base64 and decode to exactly 32 bytes (AES-256). The value is never
// echoed in any returned error.
func ValidateMasterKey(raw []byte) error {
	decoded, err := base64.StdEncoding.DecodeString(string(raw))
	if err != nil {
		return ErrInvalidMasterKey
	}
	if len(decoded) != 32 {
		return ErrInvalidMasterKey
	}
	return nil
}

// isEnvelope reports whether raw has the envelope shape: version (number),
// nonce (string), and ciphertext (string). A legacy map whose aliases happen
// to be named exactly like those keys is treated as an envelope (pathological,
// acceptable).
func isEnvelope(raw map[string]json.RawMessage) bool {
	verRaw, ok := raw["version"]
	if !ok {
		return false
	}
	var ver json.Number
	if err := json.Unmarshal(verRaw, &ver); err != nil {
		return false
	}
	for _, k := range []string{"nonce", "ciphertext"} {
		v, ok := raw[k]
		if !ok {
			return false
		}
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return false
		}
	}
	return true
}

// encryptEnvelope encrypts keys under masterKey with AES-256-GCM and returns
// the envelope JSON without a trailing newline (persistLocked adds it).
func encryptEnvelope(keys map[string]string, masterKey []byte) ([]byte, error) {
	plaintext, err := json.MarshalIndent(keys, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("secrets: marshal: %w", err)
	}
	plaintext = append(plaintext, '\n')
	block, err := aes.NewCipher(masterKey)
	if err != nil {
		return nil, fmt.Errorf("secrets: cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secrets: gcm: %w", err)
	}
	// A fresh 12-byte random nonce per encryption; never fixed or reused.
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("secrets: nonce: %w", err)
	}
	env := envelope{
		Version:    envelopeVersion,
		Nonce:      base64.StdEncoding.EncodeToString(nonce),
		Ciphertext: base64.StdEncoding.EncodeToString(gcm.Seal(nil, nonce, plaintext, nil)),
	}
	return json.MarshalIndent(env, "", "  ")
}

// decryptEnvelope decrypts an envelope under masterKey. GCM authentication
// failure maps to ErrWrongKey; structural failures (base64, version, plaintext
// shape) map to ErrCorrupted.
func decryptEnvelope(data, masterKey []byte) (map[string]string, error) {
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, ErrCorrupted
	}
	if env.Version != envelopeVersion {
		return nil, ErrCorrupted
	}
	nonce, err := base64.StdEncoding.DecodeString(env.Nonce)
	if err != nil {
		return nil, ErrCorrupted
	}
	ciphertext, err := base64.StdEncoding.DecodeString(env.Ciphertext)
	if err != nil {
		return nil, ErrCorrupted
	}
	block, err := aes.NewCipher(masterKey)
	if err != nil {
		return nil, fmt.Errorf("secrets: cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secrets: gcm: %w", err)
	}
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, ErrWrongKey
	}
	keys := map[string]string{}
	if err := json.Unmarshal(plaintext, &keys); err != nil {
		return nil, ErrCorrupted
	}
	return keys, nil
}

// Path returns the secrets file path.
func (s *Store) Path() string { return s.path }

// Get returns the stored key for alias, if any.
func (s *Store) Get(alias string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	key, ok := s.keys[alias]
	return key, ok
}

// Set stores (or replaces) the key for alias and persists atomically.
func (s *Store) Set(alias, key string) error {
	if key == "" {
		return s.Delete(alias)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys[alias] = key
	return s.persistLocked()
}

// Delete removes the key for alias (if present) and persists.
func (s *Store) Delete(alias string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.keys[alias]; !ok {
		return nil
	}
	delete(s.keys, alias)
	return s.persistLocked()
}

// persistLocked encrypts keys into an envelope and writes the secrets file
// with mode 0600 via an atomic temp-file + rename (the caller holds s.mu).
func (s *Store) persistLocked() error {
	data, err := encryptEnvelope(s.keys, s.masterKey)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".secrets-*.tmp")
	if err != nil {
		return fmt.Errorf("secrets: write %s: %w", s.path, err)
	}
	tmpName := tmp.Name()
	defer func() {
		if tmpName != "" {
			os.Remove(tmpName) //nolint:errcheck // best-effort cleanup on failure
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	// Crash safety parity with config.WriteFile: fsync before rename so a
	// power loss never leaves an empty/partial secrets file at the target.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// 0600: only the gateway user may read the keys (§6.2).
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return err
	}
	tmpName = ""
	return nil
}

// GeneratedMasterKey returns the one-time exposure copy of a gateway-generated
// master key (base64 std encoding), or false when no key was generated or it
// has already been acknowledged.
func (s *Store) GeneratedMasterKey() (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.generatedKey == "" {
		return "", false
	}
	return s.generatedKey, true
}

// PendingMigration reports whether a legacy plaintext file is still on disk
// awaiting the AckMasterKey migration.
func (s *Store) PendingMigration() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.pendingMigration
}

// AckMasterKey clears the one-time exposure copy. When a legacy migration is
// pending it first encrypts the in-memory map and atomic-persists it with the
// working key; on persist failure the exposure copy is left intact so the
// caller can retry.
func (s *Store) AckMasterKey() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pendingMigration {
		if err := s.persistLocked(); err != nil {
			return err
		}
		s.pendingMigration = false
	}
	s.generatedKey = ""
	return nil
}

// MaskKey returns a masked form of key for display ("sk-…abcd"), keeping the
// first three and last four characters. Very short keys are masked entirely so
// the display never reveals the value.
func MaskKey(key string) string {
	if len(key) <= 8 {
		return "••••••"
	}
	return key[:3] + "…" + key[len(key)-4:]
}
