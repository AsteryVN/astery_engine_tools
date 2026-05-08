package observability

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// TestRenderErrorAsString asserts that wrapped errors logged via slog render
// as their .Error() string, not as `{}`. The empty-object regression made the
// `claim unauthorized` debug session significantly harder during v0.2.5.
func TestRenderErrorAsString(t *testing.T) {
	var buf bytes.Buffer
	h := slog.NewJSONHandler(&buf, &slog.HandlerOptions{
		Level:       slog.LevelDebug,
		ReplaceAttr: renderErrorAsString,
	})
	logger := slog.New(h)

	wrapped := fmt.Errorf("outer: %w", errors.New("inner cause"))
	logger.Warn("test", "error", wrapped)

	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	errVal, ok := got["error"].(string)
	if !ok {
		t.Fatalf("error field is %T, want string; full record: %s", got["error"], buf.String())
	}
	if !strings.Contains(errVal, "inner cause") {
		t.Fatalf("error string lost wrap chain: %q", errVal)
	}
}
