// Package upload performs PUT uploads against cloud-minted presigned URLs.
// MVP uses a plain http.PUT — for very large files we'd switch to S3
// multipart via the AWS SDK, but the cloud's presigner returns a single
// PUT URL today (multipart support is per architecture spec future work).
package upload

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// Client uploads byte streams to presigned URLs.
type Client struct {
	http *http.Client
}

// New constructs a Client with a generous timeout for large media uploads.
func New() *Client {
	return &Client{
		http: &http.Client{Timeout: 30 * time.Minute},
	}
}

// Result is what UploadFile returns to the caller.
type Result struct {
	Bytes          int64
	ChecksumSHA256 string
}

// UploadFile streams a local file to the presigned URL using the given HTTP
// method and headers, returning size + checksum so the caller can populate
// the manifest entry.
func (c *Client) UploadFile(ctx context.Context, filePath, presignedURL, method string, headers map[string]string) (Result, error) {
	if method == "" {
		method = http.MethodPut
	}
	st, err := os.Stat(filePath)
	if err != nil {
		return Result{}, fmt.Errorf("stat %s: %w", filePath, err)
	}
	f, err := os.Open(filePath)
	if err != nil {
		return Result{}, fmt.Errorf("open %s: %w", filePath, err)
	}
	defer f.Close()

	// Pre-compute the checksum so the manifest is correct even if the
	// upload retry-loops. Two passes (checksum + upload); for MVP this is
	// fine. Future: streaming sha256 alongside upload via TeeReader.
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return Result{}, fmt.Errorf("hash %s: %w", filePath, err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return Result{}, fmt.Errorf("rewind %s: %w", filePath, err)
	}
	checksum := hex.EncodeToString(h.Sum(nil))

	req, err := http.NewRequestWithContext(ctx, method, presignedURL, f)
	if err != nil {
		return Result{}, fmt.Errorf("new upload req: %w", err)
	}
	req.ContentLength = st.Size()
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("upload do: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return Result{}, fmt.Errorf("upload status %d: %s", resp.StatusCode, string(body))
	}
	return Result{Bytes: st.Size(), ChecksumSHA256: checksum}, nil
}
