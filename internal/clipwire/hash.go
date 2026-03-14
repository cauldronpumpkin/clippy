package clipwire

import "crypto/sha256"

// ComputeContentSHA256 returns the raw SHA-256 digest for clipboard content.
func ComputeContentSHA256(data []byte) [sha256.Size]byte {
	return sha256.Sum256(data)
}
