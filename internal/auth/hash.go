package auth

import (
	"crypto/sha256"
	"encoding/hex"
)

// HashHex returns the SHA-256 hex digest of s. Re-exported so other packages
// don't need to import crypto/sha256 directly for the same use case.
func HashHex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
