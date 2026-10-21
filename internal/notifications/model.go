// Package notifications defines the core notification model shared by every
// component of NotiBridge. It has no dependencies on macOS, SQLite or Telegram
// so it can be reused by other applications.
package notifications

import "time"

// Notification is a single macOS notification record after parsing.
// All fields are best-effort: macOS does not guarantee that any particular
// field is present for every notification.
type Notification struct {
	// AppID is the bundle identifier of the application that posted the
	// notification (for example "com.whatsapp.WhatsApp").
	AppID string
	// UUID is the hexadecimal representation of the record UUID stored by
	// Notification Center. It is empty when the record has no UUID.
	UUID string
	// Title, Subtitle and Body come from the notification payload
	// (req.titl / req.subt / req.bode). Any of them may be empty.
	Title    string
	Subtitle string
	Body     string
	// Category is the notification category (req.cate), when present.
	Category string
	// RequestID is the app-supplied request identifier (req.iden), when
	// present. It is more stable than the payload text for some apps.
	RequestID string
	// DeliveredAt is when Notification Center recorded the delivery.
	// It is the zero time when the record carries no usable timestamp.
	DeliveredAt time.Time
	// RecID is the database row identifier. It is NOT stable across
	// Notification Center maintenance and must never be used for
	// deduplication.
	RecID int64
}
