// Package secrets manages the UI-managed API keys file <store>.secrets.json
// (§6.2). Keys are stored alias→key encrypted at rest with AES-256-GCM using
// the SHIMMER_MASTER_KEY env var (base64, 32 bytes) or a key the gateway
// generates on first run; the file is chmod 0600 and keys never appear in
// gateway.toml or logs. Key precedence is env-var-then-file: the gateway uses
// an instance's api_key_env when set, falling back to this store.
//
// The same encrypted file also holds one OAuth credential record per
// instance (the ChatGPT Plus browser-flow credential: access token, refresh
// token, expiry, and account id). Records are stored as versioned,
// marker-prefixed JSON values under the instance alias, so the on-disk
// plaintext shape stays the legacy alias→string map and every existing
// secrets file (plaintext or encrypted envelope) keeps reading unchanged.
// OAuth records are never returned as API keys by ResolveKey, and token
// material never appears in any error or log emitted by this package.
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
	"strings"
	"sync"
	"time"
)

// envelopeVersion is the on-disk envelope format version.
const envelopeVersion = 1

// OAuth record format: a marker-prefixed JSON value stored under the
// instance alias in the same encrypted alias→string map as API keys. The
// marker keeps records distinguishable from legacy key strings (a real key
// starting with "oauth:v1:" plus a valid record JSON body is not a plausible
// collision), so existing files keep their exact on-disk plaintext shape.
const (
	// oauthRecordPrefix marks an OAuth credential record value.
	oauthRecordPrefix = "oauth:v1:"
	// oauthRecordVersion is the credential record schema version. Unknown
	// versions are rejected on read rather than silently reinterpreted.
	oauthRecordVersion = 1
)

// OAuthCredential is the persisted OAuth credential for one gateway instance
// (the ChatGPT Plus browser flow). It is stored encrypted inside the secrets
// envelope; the type intentionally does not implement fmt.Stringer so an
// accidental %v/%+v cannot render token material.
type OAuthCredential struct {
	// Version is the record schema version; SetOAuth stamps
	// oauthRecordVersion when zero and GetOAuth rejects other values.
	Version int `json:"v"`
	// AccessToken is the bearer credential for the upstream API.
	AccessToken string `json:"access_token"`
	// RefreshToken rotates the access token; may be empty only when the
	// provider never issued one.
	RefreshToken string `json:"refresh_token,omitempty"`
	// ExpiresAt is the access token expiry (zero = unknown).
	ExpiresAt time.Time `json:"expires_at,omitzero"`
	// AccountID is the verified ChatGPT account identifier; empty when the
	// token claims carried none.
	AccountID string `json:"account_id,omitempty"`
}

// Expired reports whether the access token has expired at now, applying skew
// seconds of leeway (positive skew expires the token earlier).
func (c OAuthCredential) Expired(now time.Time, skew time.Duration) bool {
	return !c.ExpiresAt.IsZero() && now.Add(skew).After(c.ExpiresAt)
}

// isOAuthRecord reports whether a stored value is an OAuth credential record.
func isOAuthRecord(value string) bool {
	return strings.HasPrefix(value, oauthRecordPrefix)
}

// encodeOAuthRecord renders cred as the marker-prefixed on-disk value.
func encodeOAuthRecord(cred OAuthCredential) (string, error) {
	if cred.Version == 0 {
		cred.Version = oauthRecordVersion
	}
	raw, err := json.Marshal(cred)
	if err != nil {
		return "", fmt.Errorf("secrets: encode oauth credential: %w", err)
	}
	return oauthRecordPrefix + string(raw), nil
}

// decodeOAuthRecord parses a marker-prefixed on-disk value. The error never
// includes the stored value (which contains token material).
func decodeOAuthRecord(value string) (OAuthCredential, error) {
	var cred OAuthCredential
	if err := json.Unmarshal([]byte(strings.TrimPrefix(value, oauthRecordPrefix)), &cred); err != nil {
		return OAuthCredential{}, fmt.Errorf("secrets: oauth credential record is corrupted")
	}
	if cred.Version != oauthRecordVersion {
		return OAuthCredential{}, fmt.Errorf("secrets: unsupported oauth credential version %d", cred.Version)
	}
	if cred.AccessToken == "" {
		return OAuthCredential{}, fmt.Errorf("secrets: oauth credential record has no access token")
	}
	return cred, nil
}

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
	// ErrMigrationPending reports a secrets mutation attempted before the
	// generated master key was acknowledged. Returning an error is safer than
	// claiming success for a value that exists only in memory.
	ErrMigrationPending = errors.New("secrets migration pending master-key acknowledgement")
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

// ResolveKey returns the provider key for an instance with §6.2 precedence:
// the api_key_env environment variable first, then this secrets file. envName
// is the instance's effective api_key_env ("" for keyless providers). ok is
// false when neither source has a key; callers decide whether that is a
// keyless provider or a misconfiguration. This is the single implementation
// shared by the gateway forward path, the /models fetch, and the quota fetcher.
//
// An OAuth credential record is not an API key: it is never returned here, so
// an OAuth instance can never leak its token material into a Bearer header
// through the API-key path (env vars still win and are unaffected).
func (s *Store) ResolveKey(envName, alias string) (key string, ok bool) {
	if envName != "" {
		if k := os.Getenv(envName); k != "" {
			return k, true
		}
	}
	key, ok = s.Get(alias)
	if ok && isOAuthRecord(key) {
		return "", false
	}
	return key, ok
}

// SetOAuth stores (or replaces) the OAuth credential for alias and persists
// atomically through the same encrypted envelope and 0600 atomic-rename path
// as API keys. An empty access token is rejected: callers clear a credential
// with Delete.
func (s *Store) SetOAuth(alias string, cred OAuthCredential) error {
	if cred.AccessToken == "" {
		return errors.New("secrets: oauth credential requires an access token")
	}
	value, err := encodeOAuthRecord(cred)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	previous, existed := s.keys[alias]
	s.keys[alias] = value
	if err := s.persistLocked(); err != nil {
		if existed {
			s.keys[alias] = previous
		} else {
			delete(s.keys, alias)
		}
		return err
	}
	return nil
}

// GetOAuth returns the OAuth credential stored for alias. ok is false with a
// nil error when the alias holds no credential (including the case where it
// holds a legacy API key). An error reports a record that is present but
// unreadable (corrupted or an unsupported version); no error ever contains
// token material.
func (s *Store) GetOAuth(alias string) (cred OAuthCredential, ok bool, err error) {
	value, ok := s.Get(alias)
	if !ok || !isOAuthRecord(value) {
		return OAuthCredential{}, false, nil
	}
	cred, err = decodeOAuthRecord(value)
	if err != nil {
		return OAuthCredential{}, false, err
	}
	return cred, true, nil
}

// Set stores (or replaces) the key for alias and persists atomically.
func (s *Store) Set(alias, key string) error {
	if key == "" {
		return s.Delete(alias)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	previous, existed := s.keys[alias]
	s.keys[alias] = key
	if err := s.persistLocked(); err != nil {
		if existed {
			s.keys[alias] = previous
		} else {
			delete(s.keys, alias)
		}
		return err
	}
	return nil
}

// Delete removes the key for alias (if present) and persists.
func (s *Store) Delete(alias string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	previous, existed := s.keys[alias]
	if !existed {
		return nil
	}
	delete(s.keys, alias)
	if err := s.persistLocked(); err != nil {
		s.keys[alias] = previous
		return err
	}
	return nil
}

// Move moves the value at oldAlias to newAlias and persists the result in one
// encrypted write. When replacement is non-nil, the source is removed and the
// destination is replaced with its value when non-empty; an empty replacement
// only removes the source. This supports an instance rename with an optional
// key replacement without a delete/set gap. The map is restored if persistence
// fails. A nil replacement moves the existing source value, if any.
func (s *Store) Move(oldAlias, newAlias string, replacement *string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if oldAlias == newAlias {
		return nil
	}
	oldValue, oldExists := s.keys[oldAlias]
	newValue, newExists := s.keys[newAlias]
	if replacement == nil && !oldExists {
		return nil
	}
	if replacement != nil && *replacement == "" && !oldExists {
		return nil
	}

	if replacement == nil {
		s.keys[newAlias] = oldValue
	} else if *replacement != "" {
		s.keys[newAlias] = *replacement
	}
	delete(s.keys, oldAlias)
	if err := s.persistLocked(); err != nil {
		if oldExists {
			s.keys[oldAlias] = oldValue
		} else {
			delete(s.keys, oldAlias)
		}
		if newExists {
			s.keys[newAlias] = newValue
		} else {
			delete(s.keys, newAlias)
		}
		return err
	}
	return nil
}

// persistLocked encrypts keys into an envelope and writes the secrets file
// with mode 0600 via an atomic temp-file + rename (the caller holds s.mu).
// Legacy plaintext migration is deliberately deferred until AckMasterKey, so
// a crash cannot replace the only recoverable copy with ciphertext encrypted
// by a key that exists only in memory. Mutations are rejected while migration
// is pending rather than being reported as durable when they are memory-only.
func (s *Store) persistLocked() error {
	if s.pendingMigration {
		return ErrMigrationPending
	}
	return s.persistNowLocked()
}

// persistNowLocked always writes the encrypted envelope. It is used by
// startup creation and AckMasterKey, which must bypass the migration barrier.
func (s *Store) persistNowLocked() error {
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
		if err := s.persistNowLocked(); err != nil {
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
