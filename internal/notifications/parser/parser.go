// Package parser decodes the binary property lists that macOS Notification
// Center stores with each record. Every field is optional and every payload
// is treated as untrusted input: malformed plists and unexpected value types
// produce errors or empty fields, never panics.
package parser

import (
	"fmt"
	"strings"
	"time"

	"howett.net/plist"

	"github.com/spdeepak/notibridge/internal/notifications"
)

// macEpochOffset is the number of seconds between the Unix epoch
// (1970-01-01) and the Mac absolute time epoch (2001-01-01).
const macEpochOffset = 978307200

// Parse decodes a notification payload (a binary or XML property list) into
// the fields NotiBridge forwards. The returned Notification keeps the UUID
// and RecID empty; the source fills those in from the database row.
//
// Known payload keys, as stored by usernoted:
//
//	app          bundle identifier (string)
//	date         notification time, Mac absolute time (number)
//	req.titl     title    (string)
//	req.subt     subtitle (string)
//	req.body     body     (string)
//	req.cate     category (string)
//	req.iden     request identifier (string)
func Parse(payload []byte) (notifications.Notification, error) {
	var n notifications.Notification
	if len(payload) == 0 {
		return n, fmt.Errorf("empty payload")
	}

	var root map[string]interface{}
	if _, err := plist.Unmarshal(payload, &root); err != nil {
		return n, fmt.Errorf("decode plist: %w", err)
	}
	if root == nil {
		return n, fmt.Errorf("payload decoded to non-dictionary root")
	}

	n.AppID = stringAt(root, "app")
	if v, ok := root["date"].(float64); ok {
		n.DeliveredAt = time.Unix(int64(v)+macEpochOffset, 0).UTC()
	}

	req, _ := root["req"].(map[string]interface{})
	n.Title = stringAt(req, "titl")
	n.Subtitle = stringAt(req, "subt")
	n.Body = stringAt(req, "body")
	n.Category = stringAt(req, "cate")
	n.RequestID = stringAt(req, "iden")

	return n, nil
}

// stringAt returns m[key] as a string, tolerating the different string-ish
// types plist decoders produce (plain string, []byte for data values). It
// returns "" for missing keys and for values of any other type.
func stringAt(m map[string]interface{}, key string) string {
	if m == nil {
		return ""
	}
	switch v := m[key].(type) {
	case string:
		return strings.TrimSpace(v)
	case []byte:
		return strings.TrimSpace(string(v))
	case fmt.Stringer:
		return strings.TrimSpace(v.String())
	default:
		return ""
	}
}
