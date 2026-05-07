// Package auth owns pairing exchange + token storage. OS keyring preferred
// (99designs/keyring), file fallback when unavailable.
package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/99designs/keyring"
)

// SessionBundle is the persisted token pair.
type SessionBundle struct {
	DeviceID         string    `json:"device_id"`
	OrganizationID   string    `json:"organization_id"`
	UserID           string    `json:"user_id"`
	SessionJWT       string    `json:"session_jwt"`
	SessionExpiresAt time.Time `json:"session_expires_at"`
	RefreshToken     string    `json:"refresh_token"`
	RefreshExpiresAt time.Time `json:"refresh_expires_at"`
	HwFingerprint    string    `json:"hw_fingerprint"`
}

// Keystore persists the SessionBundle across daemon restarts.
type Keystore interface {
	Save(b SessionBundle) error
	Load() (SessionBundle, error)
	Clear() error
	Backend() string // "keyring" | "file"
}

const keyringServiceName = "astery-engine-tools"
const keystoreItemKey = "session_bundle"

// ErrNoSession is returned by Load when no bundle is stored.
var ErrNoSession = errors.New("no stored session — pairing required")

// NewKeystore tries the OS keyring first; falls back to a file under
// secretsDir when keyring unavailable (Linux headless without DBus).
func NewKeystore(secretsDir string) Keystore {
	ring, err := keyring.Open(keyring.Config{
		ServiceName: keyringServiceName,
		// Linux backends in preference order — secret-service preferred.
		AllowedBackends: []keyring.BackendType{
			keyring.SecretServiceBackend,
			keyring.KWalletBackend,
			keyring.KeychainBackend,    // macOS
			keyring.WinCredBackend,     // Windows
			keyring.PassBackend,        // pass(1) fallback
		},
	})
	if err == nil {
		return &keyringKeystore{ring: ring}
	}
	// File fallback — strict 0600 permissions; not encrypted in MVP, but
	// stored under <data-dir>/secrets/ with restrictive perms.
	if mkErr := os.MkdirAll(secretsDir, 0o700); mkErr != nil {
		return &nullKeystore{err: fmt.Errorf("init keystore secretsDir: %w", mkErr)}
	}
	return &fileKeystore{path: filepath.Join(secretsDir, "session.json")}
}

// ─── keyring backend ─────────────────────────────────────────────────────

type keyringKeystore struct {
	ring keyring.Keyring
	mu   sync.Mutex
}

func (k *keyringKeystore) Save(b SessionBundle) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	raw, err := json.Marshal(b)
	if err != nil {
		return fmt.Errorf("marshal session: %w", err)
	}
	if err := k.ring.Set(keyring.Item{Key: keystoreItemKey, Data: raw}); err != nil {
		return fmt.Errorf("keyring set: %w", err)
	}
	return nil
}

func (k *keyringKeystore) Load() (SessionBundle, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	item, err := k.ring.Get(keystoreItemKey)
	if err != nil {
		if errors.Is(err, keyring.ErrKeyNotFound) {
			return SessionBundle{}, ErrNoSession
		}
		return SessionBundle{}, fmt.Errorf("keyring get: %w", err)
	}
	var b SessionBundle
	if err := json.Unmarshal(item.Data, &b); err != nil {
		return SessionBundle{}, fmt.Errorf("unmarshal session: %w", err)
	}
	return b, nil
}

func (k *keyringKeystore) Clear() error {
	k.mu.Lock()
	defer k.mu.Unlock()
	err := k.ring.Remove(keystoreItemKey)
	if err != nil && !errors.Is(err, keyring.ErrKeyNotFound) {
		return fmt.Errorf("keyring remove: %w", err)
	}
	return nil
}

func (k *keyringKeystore) Backend() string { return "keyring" }

// ─── file backend ────────────────────────────────────────────────────────

type fileKeystore struct {
	path string
	mu   sync.Mutex
}

func (k *fileKeystore) Save(b SessionBundle) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	raw, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal session: %w", err)
	}
	return os.WriteFile(k.path, raw, 0o600)
}

func (k *fileKeystore) Load() (SessionBundle, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	raw, err := os.ReadFile(k.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return SessionBundle{}, ErrNoSession
		}
		return SessionBundle{}, fmt.Errorf("read session: %w", err)
	}
	var b SessionBundle
	if err := json.Unmarshal(raw, &b); err != nil {
		return SessionBundle{}, fmt.Errorf("unmarshal session: %w", err)
	}
	return b, nil
}

func (k *fileKeystore) Clear() error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := os.Remove(k.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove session: %w", err)
	}
	return nil
}

func (k *fileKeystore) Backend() string { return "file" }

// ─── null backend (init failure) ────────────────────────────────────────

type nullKeystore struct{ err error }

func (n *nullKeystore) Save(SessionBundle) error      { return n.err }
func (n *nullKeystore) Load() (SessionBundle, error)  { return SessionBundle{}, n.err }
func (n *nullKeystore) Clear() error                  { return n.err }
func (n *nullKeystore) Backend() string               { return "disabled" }
