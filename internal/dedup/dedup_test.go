package dedup

import (
	"strings"
	"testing"
	"time"

	"github.com/spdeepak/notibridge/internal/notifications"
)

func sample() notifications.Notification {
	return notifications.Notification{
		AppID:       "com.apple.MobileSMS",
		UUID:        "a1b2c3d4e5f6",
		Title:       "New message",
		Subtitle:    "Alice",
		Body:        "Hello!",
		RequestID:   "iMessage;chat123",
		DeliveredAt: time.Date(2026, 10, 18, 22, 35, 0, 0, time.UTC),
	}
}

func TestKeyUsesUUIDWhenPresent(t *testing.T) {
	n := sample()
	key := Key(n)
	if key != "u:a1b2c3d4e5f6" {
		t.Errorf("Key = %q", key)
	}
	// Same UUID, completely different content → same key (the UUID is the
	// durable identity of the record).
	n.Title = "Changed by DB reorganization"
	n.RecID = 999999
	if Key(n) != key {
		t.Error("key changed when only content and rec_id changed")
	}
}

func TestKeyNormalizesUUIDCase(t *testing.T) {
	n := sample()
	n.UUID = "A1B2C3D4E5F6"
	m := sample()
	m.UUID = "a1b2c3d4e5f6"
	if Key(n) != Key(m) {
		t.Error("UUID case should not affect the key")
	}
}

func TestFingerprintFallbackForMissingUUID(t *testing.T) {
	n := sample()
	n.UUID = ""
	key := Key(n)
	if !strings.HasPrefix(key, "f:") {
		t.Errorf("expected fingerprint key, got %q", key)
	}
	if key != Key(n) {
		t.Error("fingerprint is not deterministic")
	}
}

func TestFingerprintDistinguishesDifferentNotifications(t *testing.T) {
	base := sample()
	base.UUID = ""
	other := base
	other.Body = "Different body"
	if Fingerprint(base) == Fingerprint(other) {
		t.Error("different bodies produced the same fingerprint")
	}
	other = base
	other.AppID = "com.slack.Slack"
	if Fingerprint(base) == Fingerprint(other) {
		t.Error("different apps produced the same fingerprint")
	}
	other = base
	other.DeliveredAt = base.DeliveredAt.Add(time.Second)
	if Fingerprint(base) == Fingerprint(other) {
		t.Error("different timestamps produced the same fingerprint")
	}
	other = base
	other.RequestID = "other-request"
	if Fingerprint(base) == Fingerprint(other) {
		t.Error("different request ids produced the same fingerprint")
	}
	// Title/subtitle/body must contribute.
	other = base
	other.Title = base.Title + "!"
	if Fingerprint(base) == Fingerprint(other) {
		t.Error("different titles produced the same fingerprint")
	}
	// Field separator must prevent concatenation ambiguity.
	a := sample()
	a.UUID = ""
	a.Title, a.Subtitle = "ab", "c"
	b := sample()
	b.UUID = ""
	b.Title, b.Subtitle = "a", "bc"
	if Fingerprint(a) == Fingerprint(b) {
		t.Error("field boundaries are ambiguous")
	}
}

// TestRecIDNotPartOfKey documents the requirement that unstable rec_id values
// never affect deduplication.
func TestRecIDNotPartOfKey(t *testing.T) {
	n := sample()
	n.UUID = ""
	before := Key(n)
	n.RecID = 1
	after := Key(n)
	if before != after {
		t.Error("rec_id must not influence the deduplication key")
	}
}

// TestIdenticalContentSameSecondCollides documents the known limitation of
// fingerprint-based deduplication.
func TestIdenticalContentSameSecondCollides(t *testing.T) {
	a := sample()
	a.UUID = ""
	b := a
	if Key(a) != Key(b) {
		t.Error("identical notifications should collide by design (documented limitation)")
	}
}
