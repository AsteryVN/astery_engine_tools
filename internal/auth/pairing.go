package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ErrUnauthorized is returned by Exchange / Refresh when the cloud rejects
// the request with HTTP 401 (e.g. invalid pairing display code, expired
// refresh token). Callers should compare with errors.Is — never string-match
// the wrapped error message, which is unstable.
var ErrUnauthorized = errors.New("auth: cloud rejected credentials (401)")

// ErrCloudUnreachable is returned by Unpair (and any other call that wants
// to distinguish "I asked the cloud and it errored" from "I asked the cloud
// and got an authoritative no") when the cloud is offline / 5xx / network
// error. The unpair handler uses this to gate the "force clear local"
// escape hatch — we ONLY clear local state without cloud confirmation when
// the cloud is genuinely unreachable, never on a 4xx.
var ErrCloudUnreachable = errors.New("auth: cloud unreachable")

// PairingClient performs the pairing handshake against the cloud control
// plane. Returns a SessionBundle the daemon then hands to the keystore.
type PairingClient struct {
	baseURL string
	http    *http.Client
}

// NewPairingClient constructs a PairingClient.
func NewPairingClient(baseURL string) *PairingClient {
	return &PairingClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// ExchangeRequest is the desktop's exchange-payload.
type ExchangeRequest struct {
	DisplayCode  string  `json:"display_code"`
	Device       Device  `json:"device"`
	HwFingerprint string `json:"hw_fingerprint"`
	DevicePubkey string  `json:"device_pubkey,omitempty"`
}

// Device describes the engine node at pairing time.
type Device struct {
	InstallationID string `json:"installation_id"`
	DisplayName    string `json:"display_name"`
	Hostname       string `json:"hostname,omitempty"`
	OS             string `json:"os"`
	Arch           string `json:"arch"`
	AppVersion     string `json:"app_version,omitempty"`
	RuntimeVersion string `json:"runtime_version,omitempty"`
}

// ExchangeResponse mirrors the cloud body (under data envelope).
type ExchangeResponse struct {
	Device  ResponseDevice  `json:"device"`
	Session ResponseToken   `json:"session"`
	Refresh ResponseToken   `json:"refresh"`
}

// ResponseDevice is the device row the cloud returned.
type ResponseDevice struct {
	ID             string `json:"id"`
	OrganizationID string `json:"organization_id"`
}

// ResponseToken is a token + expiry pair.
type ResponseToken struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Exchange performs POST /v1/desktop/exchange.
func (c *PairingClient) Exchange(ctx context.Context, req ExchangeRequest) (*ExchangeResponse, error) {
	raw, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal exchange: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/desktop/exchange", bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("new exchange request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Idempotency-Key", uuid.NewString())
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("exchange http: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("exchange: %w (body: %s)", ErrUnauthorized, string(body))
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("exchange: status %d: %s", resp.StatusCode, string(body))
	}
	var env struct {
		Data ExchangeResponse `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("decode exchange: %w", err)
	}
	return &env.Data, nil
}

// Unpair calls DELETE /v1/desktop/devices/self on the cloud, identifying
// the daemon via its current session JWT. The cloud route lives under
// RegisterDeviceRoutes so the device-session middleware authenticates the
// request — no user JWT required.
//
// Outcomes:
//   - 200/204 → nil (cloud successfully soft-deleted the device row + revoked sessions)
//   - 401     → returns nil (session already expired/revoked; cloud row will be
//               GC'd by the reconciler — no point making the user re-pair just to
//               unpair). The caller MUST still clear local state.
//   - 5xx / transport error → ErrCloudUnreachable
//   - other 4xx → wrapped error (caller surfaces verbatim to UI)
func (c *PairingClient) Unpair(ctx context.Context, sessionJWT, hwFingerprint string) error {
	if sessionJWT == "" {
		return errors.New("unpair: empty session jwt")
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.baseURL+"/v1/desktop/devices/self", nil)
	if err != nil {
		return fmt.Errorf("new unpair request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+sessionJWT)
	if hwFingerprint != "" {
		httpReq.Header.Set("X-Device-Fingerprint", hwFingerprint)
	}
	httpReq.Header.Set("Idempotency-Key", uuid.NewString())

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrCloudUnreachable, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	switch {
	case resp.StatusCode == http.StatusOK,
		resp.StatusCode == http.StatusNoContent:
		return nil
	case resp.StatusCode == http.StatusUnauthorized:
		// Session expired or already revoked — treat as success since the
		// cloud row is already in a state where re-pair would succeed.
		return nil
	case resp.StatusCode >= 500:
		return fmt.Errorf("%w: status %d: %s", ErrCloudUnreachable, resp.StatusCode, string(body))
	default:
		return fmt.Errorf("unpair: status %d: %s", resp.StatusCode, string(body))
	}
}

// RefreshRequest is the rotate-payload.
type RefreshRequest struct {
	RefreshToken  string `json:"refresh_token"`
	HwFingerprint string `json:"hw_fingerprint"`
}

// Refresh performs POST /v1/desktop/sessions/refresh and returns a fresh
// session+refresh pair (rotated).
func (c *PairingClient) Refresh(ctx context.Context, req RefreshRequest) (*ExchangeResponse, error) {
	raw, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal refresh: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/desktop/sessions/refresh", bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("new refresh request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Idempotency-Key", uuid.NewString())
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("refresh http: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusUnauthorized {
		// Cloud rejected the refresh token (rotated/revoked/expired). The
		// caller must trigger a re-pair — refresh cannot recover.
		return nil, fmt.Errorf("refresh: %w (body: %s)", ErrUnauthorized, string(body))
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("refresh: status %d: %s", resp.StatusCode, string(body))
	}
	var env struct {
		Data ExchangeResponse `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("decode refresh: %w", err)
	}
	return &env.Data, nil
}
