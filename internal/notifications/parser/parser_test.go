package parser

import (
	"strings"
	"testing"
	"time"

	"howett.net/plist"
)

// buildBplist encodes a payload the same way Notification Center stores it:
// a binary property list.
func buildBplist(t *testing.T, payload map[string]interface{}) []byte {
	t.Helper()
	data, err := plist.Marshal(payload, plist.BinaryFormat)
	if err != nil {
		t.Fatalf("encode fixture bplist: %v", err)
	}
	return data
}

func representativePayload() map[string]interface{} {
	return map[string]interface{}{
		"app":  "com.whatsapp.WhatsApp",
		"date": 814019700.0, // Mac absolute time: 2026-10-18 12:35:00 UTC
		"req": map[string]interface{}{
			"titl": "New message",
			"subt": "Alice",
			"body": "Hey, are you free tonight?",
			"cate": "Notification",
			"iden": "req-123",
		},
	}
}

func TestParseRepresentativePayload(t *testing.T) {
	n, err := Parse(buildBplist(t, representativePayload()))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if n.AppID != "com.whatsapp.WhatsApp" {
		t.Errorf("AppID = %q", n.AppID)
	}
	if n.Title != "New message" || n.Subtitle != "Alice" || n.Body != "Hey, are you free tonight?" {
		t.Errorf("text fields = %q %q %q", n.Title, n.Subtitle, n.Body)
	}
	if n.Category != "Notification" || n.RequestID != "req-123" {
		t.Errorf("category/request = %q %q", n.Category, n.RequestID)
	}
	// 814019700 Mac absolute = 2026-10-18 12:35:00 UTC.
	want := time.Date(2026, 10, 18, 12, 35, 0, 0, time.UTC)
	if !n.DeliveredAt.Equal(want) {
		t.Errorf("DeliveredAt = %v, want %v", n.DeliveredAt, want)
	}
}

func TestParseXMLPlist(t *testing.T) {
	data, err := plist.Marshal(representativePayload(), plist.XMLFormat)
	if err != nil {
		t.Fatal(err)
	}
	n, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse xml plist: %v", err)
	}
	if n.Title != "New message" {
		t.Errorf("Title = %q", n.Title)
	}
}

func TestParseEmptyPayload(t *testing.T) {
	if _, err := Parse(nil); err == nil {
		t.Error("expected error for nil payload")
	}
	if _, err := Parse([]byte{}); err == nil {
		t.Error("expected error for empty payload")
	}
}

func TestParseMalformedPayload(t *testing.T) {
	junk := []byte("this is definitely not a plist")
	if _, err := Parse(junk); err == nil {
		t.Fatal("expected error for malformed payload")
	} else if !strings.Contains(err.Error(), "decode plist") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestParseMissingFields(t *testing.T) {
	// Payload with only the app, no req dictionary at all.
	n, err := Parse(buildBplist(t, map[string]interface{}{"app": "com.apple.finder"}))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if n.AppID != "com.apple.finder" {
		t.Errorf("AppID = %q", n.AppID)
	}
	if n.Title != "" || n.Subtitle != "" || n.Body != "" || n.Category != "" || n.RequestID != "" {
		t.Errorf("expected empty fields, got %+v", n)
	}
	if !n.DeliveredAt.IsZero() {
		t.Errorf("expected zero DeliveredAt, got %v", n.DeliveredAt)
	}
}

func TestParseWrongTypesAreTolerated(t *testing.T) {
	// Every field has an unexpected type: must not panic and must not
	// produce garbage text.
	payload := map[string]interface{}{
		"app":  42.0,
		"date": "not a number",
		"req": map[string]interface{}{
			"titl": 1234,
			"subt": []interface{}{"a", "b"},
			"body": true,
		},
	}
	n, err := Parse(buildBplist(t, payload))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if n.AppID != "" || n.Title != "" || n.Subtitle != "" || n.Body != "" || !n.DeliveredAt.IsZero() {
		t.Errorf("expected all fields empty, got %+v", n)
	}
}

func TestParseTrimsWhitespace(t *testing.T) {
	payload := representativePayload()
	payload["req"].(map[string]interface{})["titl"] = "  padded title  "
	n, err := Parse(buildBplist(t, payload))
	if err != nil {
		t.Fatal(err)
	}
	if n.Title != "padded title" {
		t.Errorf("Title = %q", n.Title)
	}
}
