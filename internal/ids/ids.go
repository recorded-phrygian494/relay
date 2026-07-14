// Package ids generates time-prefixed unique identifiers for requests and
// synthesized wire objects. No external dependencies; not ULID-compatible,
// just sortable-enough and collision-safe.
package ids

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// New returns an id like "req_18f3c2a41b9d6e_4f21ac", prefixed with p.
func New(p string) string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%s_%x_%s", p, time.Now().UnixMicro(), hex.EncodeToString(b[:]))
}
