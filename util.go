package main

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// newUUID returns a random RFC-4122 v4 UUID string.
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand should never fail; fall back to a timestamp-free constant
		// rather than panicking inside a request path.
		return "00000000-0000-4000-8000-000000000000"
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func urlEncode(s string) string { return url.QueryEscape(s) }

// ptr returns a pointer to v (Bifrost schemas are pointer-heavy).
func ptr[T any](v T) *T { return &v }

// canonicalJSON produces a stable compact JSON representation used for session
// fingerprints. Keys are emitted in a deterministic order by re-marshalling via
// a sorted-key encoder.
func canonicalJSON(v any) string {
	// encoding/json emits map keys in sorted order, which is what we want for a
	// stable fingerprint.
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

func nonEmpty(s string) bool { return strings.TrimSpace(s) != "" }
