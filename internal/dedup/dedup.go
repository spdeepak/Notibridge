// Package dedup computes durable deduplication keys for notifications.
//
// macOS assigns a UUID to each notification record and stores it in the
// Notification Center database. Testing on supported macOS versions shows the
// UUID survives polling cycles and application restarts, so it is used as the
// primary key. Record IDs (rec_id) are never used: they can be renumbered when
// Notification Center reorganizes its database.
//
// When a record has no UUID (which can happen for some system notifications),
// a content fingerprint is used instead. The fingerprint hashes the strongest
// available combination of metadata:
//
//	bundle id | request id | delivered timestamp | title | subtitle | body
//
// Fingerprints are inherently imperfect: two legitimate notifications that
// arrive in the same second with identical app, request id and text produce
// the same fingerprint and only one of them will be forwarded. In practice
// the timestamp plus request identifier makes collisions rare; apps that
// reuse request identifiers for identical content (for example a repeating
// timer notification) are the most likely to collide.
package dedup

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/spdeepak/notibridge/internal/notifications"
)

// Key returns the durable deduplication key for a notification: a UUID-based
// key when the record carries a UUID, otherwise a content fingerprint.
// The returned key is never empty.
func Key(n notifications.Notification) string {
	if n.UUID != "" {
		return "u:" + strings.ToLower(n.UUID)
	}
	return "f:" + Fingerprint(n)
}

// Fingerprint computes the fallback content fingerprint described in the
// package documentation.
func Fingerprint(n notifications.Notification) string {
	h := sha256.New()
	writeField := func(parts ...string) {
		for _, p := range parts {
			h.Write([]byte(p))
			h.Write([]byte{0}) // unambiguous field separator
		}
	}
	ts := ""
	if !n.DeliveredAt.IsZero() {
		ts = n.DeliveredAt.UTC().Format("2006-01-02T15:04:05.000000000Z07:00")
	}
	writeField(n.AppID, n.RequestID, ts, n.Title, n.Subtitle, n.Body)
	return hex.EncodeToString(h.Sum(nil))
}
