// Package ids makes event ids: ULIDs, 26 characters of Crockford base32 that
// sort by time. Live events get the arrival time; an imported event gets its
// original time and a hash of its original id, so a rerun makes the same id.
package ids

import (
	"crypto/rand"
	"crypto/sha256"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
)

var (
	mu      sync.Mutex
	entropy = ulid.Monotonic(rand.Reader, 0)
)

// New returns a ULID for an event arriving at t. Ids made in the same
// millisecond still sort in the order they were made.
func New(t time.Time) string {
	mu.Lock()
	defer mu.Unlock()
	return ulid.MustNew(ulid.Timestamp(t), entropy).String()
}

// Derived returns the ULID for an imported record: the time part is t and the
// random part comes from origin and originID, so it is stable across reruns.
func Derived(t time.Time, origin, originID string) string {
	h := sha256.Sum256([]byte(origin + "\x00" + originID))
	var id ulid.ULID
	if err := id.SetTime(ulid.Timestamp(t)); err != nil {
		// Times before 1970 or past year 10889 cannot be encoded.
		_ = id.SetTime(0)
	}
	_ = id.SetEntropy(h[:10])
	return id.String()
}

// Valid reports whether s is a well-formed ULID.
func Valid(s string) bool {
	_, err := ulid.ParseStrict(s)
	return err == nil
}
