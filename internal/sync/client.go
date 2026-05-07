// Package sync is the cloud client. Polling is the source of truth (5s
// interval); WebSocket is acceleration only. Lease heartbeat ticks every
// 25s while a workload is in flight.
//
// All HTTPS calls add the device session bearer + the X-Device-Fingerprint
// header. Idempotency-Key UUIDs are generated per attempt.
package sync

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

// Client is the HTTPS client that talks to the cloud control plane.
type Client struct {
	baseURL     string
	http        *http.Client
	tokenSource TokenSource
	fingerprint string
}

// TokenSource yields the current bearer token. Implementations may rotate
// the underlying refresh token — they MUST be safe to call concurrently.
type TokenSource interface {
	BearerToken(ctx context.Context) (string, error)
}

// Config bundles constructor inputs.
type Config struct {
	BaseURL       string
	TokenSource   TokenSource
	Fingerprint   string
	HTTPTimeout   time.Duration
}

// DefaultHTTPTimeout caps each HTTP request — sized for normal API latency
// + retry headroom. Long uploads run through internal/upload, not here.
const DefaultHTTPTimeout = 30 * time.Second

// New constructs a Client.
func New(cfg Config) *Client {
	timeout := cfg.HTTPTimeout
	if timeout <= 0 {
		timeout = DefaultHTTPTimeout
	}
	return &Client{
		baseURL:     strings.TrimRight(cfg.BaseURL, "/"),
		http:        &http.Client{Timeout: timeout},
		tokenSource: cfg.TokenSource,
		fingerprint: cfg.Fingerprint,
	}
}

// ─── workload protocol ─────────────────────────────────────────────────────

// ClaimResponse is the cloud's `GET /v1/workloads/claim` body.
type ClaimResponse struct {
	Workload WorkloadDTO `json:"workload"`
	Lease    LeaseDTO    `json:"lease"`
}

// WorkloadDTO mirrors the cloud-side wire shape.
type WorkloadDTO struct {
	ID                   string         `json:"id"`
	OrganizationID       string         `json:"organization_id"`
	Type                 string         `json:"workload_type"`
	Version              int            `json:"workload_version"`
	Payload              map[string]any `json:"payload"`
	RequiredCapabilities map[string]any `json:"required_capabilities"`
	RequiredExecutor     string         `json:"required_executor"`
}

// LeaseDTO is the lease envelope returned with a claim/heartbeat.
type LeaseDTO struct {
	Token                string    `json:"token"`
	ExpiresAt            time.Time `json:"expires_at"`
	TTLSec               int       `json:"ttl_sec"`
	HeartbeatIntervalSec int       `json:"heartbeat_interval_sec"`
}

// ErrNoWork is returned when the claim endpoint replied 204.
var ErrNoWork = errors.New("no workload available")

// ErrUnauthorized is returned on 401 — caller should refresh.
var ErrUnauthorized = errors.New("unauthorized — refresh required")

// Claim polls the cloud for the next workload. ErrNoWork is the happy-no-work
// path; ErrUnauthorized triggers a refresh.
func (c *Client) Claim(ctx context.Context) (*ClaimResponse, error) {
	body, status, err := c.do(ctx, http.MethodGet, "/v1/workloads/claim", nil)
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusOK:
		var env struct {
			Data ClaimResponse `json:"data"`
		}
		if err := json.Unmarshal(body, &env); err != nil {
			return nil, fmt.Errorf("decode claim: %w", err)
		}
		return &env.Data, nil
	case http.StatusNoContent:
		return nil, ErrNoWork
	case http.StatusUnauthorized:
		return nil, ErrUnauthorized
	default:
		return nil, fmt.Errorf("claim: unexpected status %d: %s", status, string(body))
	}
}

// Heartbeat renews a lease.
func (c *Client) Heartbeat(ctx context.Context, workloadID, leaseToken string) (*LeaseDTO, error) {
	body, status, err := c.do(ctx, http.MethodPost,
		"/v1/workloads/"+workloadID+"/heartbeat",
		map[string]any{"lease_token": leaseToken})
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("heartbeat: status %d: %s", status, string(body))
	}
	var env struct {
		Data struct {
			Lease LeaseDTO `json:"lease"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("decode heartbeat: %w", err)
	}
	return &env.Data.Lease, nil
}

// Progress posts a progress update — best-effort.
func (c *Client) Progress(ctx context.Context, workloadID, leaseToken string, fraction float64, stage, detail string) error {
	_, _, err := c.do(ctx, http.MethodPost,
		"/v1/workloads/"+workloadID+"/progress",
		map[string]any{
			"lease_token": leaseToken,
			"fraction":    fraction,
			"stage":       stage,
			"detail":      detail,
		})
	return err
}

// PresignUploadResponse mirrors the cloud body.
type PresignUploadResponse struct {
	StorageKey string            `json:"storage_key"`
	UploadURL  string            `json:"upload_url"`
	ExpiresAt  time.Time         `json:"expires_at"`
	Method     string            `json:"method"`
	Headers    map[string]string `json:"headers"`
}

// PresignUpload asks cloud for a PUT URL for one expected output.
func (c *Client) PresignUpload(ctx context.Context, workloadID, leaseToken, outputKind, keySuffix string, sizeBytes int64) (*PresignUploadResponse, error) {
	body, status, err := c.do(ctx, http.MethodPost,
		"/v1/workloads/"+workloadID+"/upload-url",
		map[string]any{
			"lease_token": leaseToken,
			"output_kind": outputKind,
			"key_suffix":  keySuffix,
			"size_bytes":  sizeBytes,
		})
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("presign upload: status %d: %s", status, string(body))
	}
	var env struct {
		Data PresignUploadResponse `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("decode presign: %w", err)
	}
	return &env.Data, nil
}

// OutputManifestEntry is one entry in the post-upload manifest.
type OutputManifestEntry struct {
	Kind           string         `json:"kind"`
	StorageKey     string         `json:"storage_key"`
	Bytes          int64          `json:"bytes"`
	ChecksumSHA256 string         `json:"checksum_sha256,omitempty"`
	Metadata       map[string]any `json:"metadata,omitempty"`
}

// RecordOutputs posts the manifest after upload.
func (c *Client) RecordOutputs(ctx context.Context, workloadID, leaseToken string, outputs []OutputManifestEntry) error {
	body, status, err := c.do(ctx, http.MethodPost,
		"/v1/workloads/"+workloadID+"/outputs",
		map[string]any{
			"lease_token": leaseToken,
			"outputs":     outputs,
		})
	if err != nil {
		return err
	}
	if status >= 400 {
		return fmt.Errorf("record outputs: status %d: %s", status, string(body))
	}
	return nil
}

// Complete posts terminal success.
func (c *Client) Complete(ctx context.Context, workloadID, leaseToken string, summary map[string]any) error {
	body, status, err := c.do(ctx, http.MethodPost,
		"/v1/workloads/"+workloadID+"/complete",
		map[string]any{"lease_token": leaseToken, "summary": summary})
	if err != nil {
		return err
	}
	if status >= 400 {
		return fmt.Errorf("complete: status %d: %s", status, string(body))
	}
	return nil
}

// Fail posts terminal failure.
func (c *Client) Fail(ctx context.Context, workloadID, leaseToken, errMsg string, retryable bool) error {
	body, status, err := c.do(ctx, http.MethodPost,
		"/v1/workloads/"+workloadID+"/fail",
		map[string]any{
			"lease_token": leaseToken,
			"error":       errMsg,
			"retryable":   retryable,
		})
	if err != nil {
		return err
	}
	if status >= 400 {
		return fmt.Errorf("fail: status %d: %s", status, string(body))
	}
	return nil
}

// Surrender voluntarily releases a lease.
func (c *Client) Surrender(ctx context.Context, workloadID, leaseToken, reason string) error {
	body, status, err := c.do(ctx, http.MethodPost,
		"/v1/workloads/"+workloadID+"/surrender",
		map[string]any{"lease_token": leaseToken, "reason": reason})
	if err != nil {
		return err
	}
	if status >= 400 {
		return fmt.Errorf("surrender: status %d: %s", status, string(body))
	}
	return nil
}

// ReportCapability pushes the capability JSON.
func (c *Client) ReportCapability(ctx context.Context, deviceID string, payload []byte) error {
	body, status, err := c.doRaw(ctx, http.MethodPost,
		"/v1/desktop/devices/"+deviceID+"/capabilities", payload)
	if err != nil {
		return err
	}
	if status >= 400 {
		return fmt.Errorf("report capability: status %d: %s", status, string(body))
	}
	return nil
}

// ─── HTTP plumbing ─────────────────────────────────────────────────────────

func (c *Client) do(ctx context.Context, method, path string, body any) ([]byte, int, error) {
	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		if err != nil {
			return nil, 0, fmt.Errorf("marshal: %w", err)
		}
	}
	return c.doRaw(ctx, method, path, raw)
}

func (c *Client) doRaw(ctx context.Context, method, path string, raw []byte) ([]byte, int, error) {
	tok, err := c.tokenSource.BearerToken(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("token source: %w", err)
	}
	var bodyReader io.Reader
	if len(raw) > 0 {
		bodyReader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
	if err != nil {
		return nil, 0, fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if c.fingerprint != "" {
		req.Header.Set("X-Device-Fingerprint", c.fingerprint)
	}
	if method == http.MethodPost || method == http.MethodPut || method == http.MethodPatch || method == http.MethodDelete {
		req.Header.Set("Idempotency-Key", uuid.NewString())
	}
	if len(raw) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("http do: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read body: %w", err)
	}
	return respBody, resp.StatusCode, nil
}
