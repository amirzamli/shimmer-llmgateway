package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/amirzamli/shimmer-llmgateway/internal/oauth"
	"github.com/amirzamli/shimmer-llmgateway/internal/secrets"
)

// oauthExpirySkew is how far before its recorded expiry an access token is
// treated as expired, so a token never expires in flight between resolution
// and the upstream call.
const oauthExpirySkew = 30 * time.Second

// errOAuthNotConfigured reports an OAuth instance with no stored credential.
// The request handlers map it to a configuration error (the instance is not
// usable), mirroring the missing-API-key behavior.
var errOAuthNotConfigured = errors.New("oauth credential not set")

// OAuthResolver resolves the encrypted OAuth credential for one gateway
// instance and refreshes it through the canonical internal/oauth protocol
// when it is expired or about to expire. Refreshes are serialized per
// instance (concurrent callers share one refresh) and rotated tokens are
// persisted atomically through the secrets store. All errors are sanitized:
// the oauth package never renders tokens, verifiers, or provider response
// bodies, and persistence failures never include token material.
type OAuthResolver struct {
	sec    *secrets.Store
	client *http.Client
	cfg    oauth.Config
	life   *oauth.Lifecycle

	// mu guards inflight, the per-instance refresh singleflight map.
	mu       sync.Mutex
	inflight map[string]*oauthRefresh
}

// oauthRefresh is one in-flight refresh for an alias. done is closed after
// cred/err are set, so waiters always observe a complete result.
type oauthRefresh struct {
	done       chan struct{}
	generation uint64
	cred       secrets.OAuthCredential
	err        error
}

// NewOAuthResolver builds a resolver over the secrets store and the outbound
// client (nil uses http.DefaultClient). cfg pins the OAuth protocol
// endpoints; the zero value uses the verified OpenCode defaults.
func NewOAuthResolver(sec *secrets.Store, client *http.Client, cfg oauth.Config, life ...*oauth.Lifecycle) *OAuthResolver {
	if client == nil {
		client = http.DefaultClient
	}
	lifecycle := oauth.NewLifecycle()
	if len(life) > 0 && life[0] != nil {
		lifecycle = life[0]
	}
	return &OAuthResolver{
		sec:      sec,
		client:   client,
		cfg:      cfg,
		life:     lifecycle,
		inflight: map[string]*oauthRefresh{},
	}
}

// SetLifecycle shares lifecycle fencing with the API device path. It must be
// called before the resolver is used.
func (r *OAuthResolver) SetLifecycle(life *oauth.Lifecycle) {
	if life != nil {
		r.life = life
	}
}

// Credential returns a usable credential for alias: the stored record when it
// is not expired (within oauthExpirySkew), otherwise a refreshed one. It
// returns an error wrapping errOAuthNotConfigured when the instance has no
// stored credential.
func (r *OAuthResolver) Credential(ctx context.Context, alias string) (secrets.OAuthCredential, error) {
	generation := r.life.Generation(alias)
	cred, ok, err := r.sec.GetOAuth(alias)
	if err != nil {
		return secrets.OAuthCredential{}, err
	}
	if !ok {
		return secrets.OAuthCredential{}, fmt.Errorf("%w for instance %q", errOAuthNotConfigured, alias)
	}
	if !cred.Expired(time.Now(), oauthExpirySkew) {
		if !r.life.Current(alias, generation) {
			return secrets.OAuthCredential{}, oauth.ErrOperationStale
		}
		return cred, nil
	}
	return r.refresh(ctx, alias, generation)
}

// refresh serializes refreshes per instance: the first caller performs the
// refresh, later callers wait on the shared result instead of refreshing the
// same credential twice. The leader re-reads the stored credential inside the
// flight, so a caller that observed a stale (expired) record before a
// concurrent refresh persisted the rotated one gets the fresh record without
// a second token call.
func (r *OAuthResolver) refresh(ctx context.Context, alias string, generation uint64) (secrets.OAuthCredential, error) {
	for {
		r.mu.Lock()
		if f, ok := r.inflight[alias]; ok {
			r.mu.Unlock()
			select {
			case <-f.done:
				if !r.life.Current(alias, generation) {
					return secrets.OAuthCredential{}, oauth.ErrOperationStale
				}
				if f.generation == generation {
					return f.cred, f.err
				}
				// A newer generation was waiting behind an older refresh. Re-read
				// the credential and start a flight for the current operation.
				continue
			case <-ctx.Done():
				return secrets.OAuthCredential{}, ctx.Err()
			}
		}
		f := &oauthRefresh{done: make(chan struct{}), generation: generation}
		r.inflight[alias] = f
		r.mu.Unlock()

		f.cred, f.err = r.refreshNow(ctx, alias, generation)
		close(f.done)

		r.mu.Lock()
		delete(r.inflight, alias)
		r.mu.Unlock()
		return f.cred, f.err
	}
}

// refreshNow performs one refresh: it re-reads the stored credential (which a
// concurrent refresh may have already rotated), returns it unchanged when it
// is no longer expired, and otherwise runs the verified refresh request and
// persists the rotated record.
func (r *OAuthResolver) refreshNow(ctx context.Context, alias string, generation uint64) (secrets.OAuthCredential, error) {
	if !r.life.Current(alias, generation) {
		return secrets.OAuthCredential{}, oauth.ErrOperationStale
	}
	old, ok, err := r.sec.GetOAuth(alias)
	if err != nil {
		return secrets.OAuthCredential{}, err
	}
	if !ok {
		return secrets.OAuthCredential{}, fmt.Errorf("%w for instance %q", errOAuthNotConfigured, alias)
	}
	if !old.Expired(time.Now(), oauthExpirySkew) {
		if !r.life.Current(alias, generation) {
			return secrets.OAuthCredential{}, oauth.ErrOperationStale
		}
		return old, nil
	}

	tok, err := r.cfg.Refresh(ctx, r.client, old.RefreshToken)
	if err != nil {
		// oauth errors are already redacted; Redact also wraps any other
		// error defensively so no token material can escape.
		return secrets.OAuthCredential{}, oauth.Redact(err)
	}
	cred := secrets.OAuthCredential{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		ExpiresAt:    tok.ExpiresAt,
		AccountID:    tok.AccountID,
	}
	if cred.RefreshToken == "" {
		// The provider may omit refresh_token when it does not rotate it;
		// dropping the stored one would make the next refresh impossible.
		cred.RefreshToken = old.RefreshToken
	}
	if cred.AccountID == "" {
		// Refreshed tokens may carry no account claims; the verified account
		// identity from the original login stays authoritative.
		cred.AccountID = old.AccountID
	}
	current, err := r.life.IfCurrent(alias, generation, func() error {
		return r.sec.SetOAuth(alias, cred)
	})
	if err != nil {
		return secrets.OAuthCredential{}, fmt.Errorf("oauth: persist refreshed credential: %w", err)
	}
	if !current {
		return secrets.OAuthCredential{}, oauth.ErrOperationStale
	}
	return cred, nil
}
