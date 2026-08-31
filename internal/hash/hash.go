// Package hash computes the canonical content address of an artifact's
// identity. The hash is the contract between the controller, the store key,
// and the generator's provenance stamp — its canonical form must never change
// incompatibly.
package hash

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// Canonical returns "sha256:<hex>" over the canonical JSON encoding of the
// identity map. encoding/json marshals map keys in sorted order, which is the
// stability guarantee we rely on.
func Canonical(identity map[string]string) string {
	b, err := json.Marshal(identity)
	if err != nil {
		// map[string]string cannot fail to marshal; guard anyway.
		panic(fmt.Sprintf("hash: marshal identity: %v", err))
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Short returns the first n hex characters of a canonical hash, for use in
// object names. It tolerates the "sha256:" prefix.
func Short(canonical string, n int) string {
	const prefix = "sha256:"
	s := canonical
	if len(s) > len(prefix) && s[:len(prefix)] == prefix {
		s = s[len(prefix):]
	}
	if n > len(s) {
		n = len(s)
	}
	return s[:n]
}
