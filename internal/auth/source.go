package auth

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// TokenSource implements sync.TokenSource — yields the current bearer JWT,
// refreshing through the keystore + cloud pairing client when within the
// expiry window.
type TokenSource struct {
	store       Keystore
	pairing     *PairingClient
	mu          sync.Mutex
	cached      SessionBundle
	cachedKnown bool
}

// NewTokenSource constructs a TokenSource.
func NewTokenSource(store Keystore, pairing *PairingClient) *TokenSource {
	return &TokenSource{store: store, pairing: pairing}
}

// refreshGracePeriod — refresh proactively when this much TTL remains.
const refreshGracePeriod = 5 * time.Minute

// BearerToken returns the current session JWT, refreshing if needed.
func (t *TokenSource) BearerToken(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.cachedKnown {
		b, err := t.store.Load()
		if err != nil {
			return "", fmt.Errorf("token source load: %w", err)
		}
		t.cached = b
		t.cachedKnown = true
	}
	if time.Until(t.cached.SessionExpiresAt) > refreshGracePeriod {
		return t.cached.SessionJWT, nil
	}
	if t.pairing == nil {
		return "", errors.New("token source: refresh not configured")
	}
	resp, err := t.pairing.Refresh(ctx, RefreshRequest{
		RefreshToken:  t.cached.RefreshToken,
		HwFingerprint: t.cached.HwFingerprint,
	})
	if err != nil {
		return "", fmt.Errorf("refresh session: %w", err)
	}
	t.cached.SessionJWT = resp.Session.Token
	t.cached.SessionExpiresAt = resp.Session.ExpiresAt
	t.cached.RefreshToken = resp.Refresh.Token
	t.cached.RefreshExpiresAt = resp.Refresh.ExpiresAt
	if err := t.store.Save(t.cached); err != nil {
		return "", fmt.Errorf("token source save rotated: %w", err)
	}
	return t.cached.SessionJWT, nil
}

// Bundle returns the current bundle (may be empty when no session is loaded).
func (t *TokenSource) Bundle() SessionBundle {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.cached
}

// Reset wipes the in-memory cache and the persistent store.
func (t *TokenSource) Reset() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.cached = SessionBundle{}
	t.cachedKnown = false
	return t.store.Clear()
}

// Set replaces the cached bundle (called after a fresh pairing exchange).
func (t *TokenSource) Set(b SessionBundle) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.cached = b
	t.cachedKnown = true
	return t.store.Save(b)
}
