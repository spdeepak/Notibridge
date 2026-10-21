// Package source defines the interface through which NotiBridge discovers
// notifications, plus shared record types. Platform-specific implementations
// live in subpackages; nothing outside this package depends on a concrete
// source, so the source can be replaced for testing or for future macOS
// versions.
package source

import (
	"context"
	"time"
)

// Record is one row discovered in a notification database before payload
// parsing.
type Record struct {
	// RecID is the row identifier in the source database. It is unstable
	// across database maintenance and is only used for pagination.
	RecID int64
	// UUIDHex is the record UUID rendered as lowercase hex; empty when the
	// record has no UUID.
	UUIDHex string
	// AppID is the bundle identifier joined from the app table; empty when
	// unknown.
	AppID string
	// DeliveredAt is the delivery timestamp recorded by the system; the
	// zero time when missing.
	DeliveredAt time.Time
	// Payload is the raw (binary) property list with the notification
	// content.
	Payload []byte
}

// Watermark is an opaque pagination cursor. Its fields are only meaningful to
// the source that produced it, and it is persisted by the application so that
// polling resumes where it left off after a restart.
type Watermark struct {
	// DeliveredDateSecs is the raw delivery timestamp of the newest
	// processed record, in the source database's own units (for macOS,
	// seconds since 2001-01-01).
	DeliveredDateSecs int64
	// RecID breaks ties for records sharing the same delivery timestamp.
	RecID int64
	// Valid is false for the initial watermark (poll everything).
	Valid bool
}

// Diagnostics describes the outcome of a source access check. It is surfaced
// in the menu bar UI and the diagnose command so that permission problems are
// never silent.
type Diagnostics struct {
	// Path is the database file that was probed.
	Path string
	// Accessible reports whether the database could be opened read-only.
	Accessible bool
	// SchemaOK reports whether the expected tables/columns were found.
	SchemaOK bool
	// Tables lists the tables discovered in the database.
	Tables []string
	// RowCount is the number of notification records found (0 when the
	// schema is unknown).
	RowCount int64
	// Oldest and Newest bound the recorded delivery times (zero when no
	// rows or unknown schema).
	Oldest, Newest time.Time
	// Error is the human-readable failure, when any.
	Error string
	// Hints are actionable suggestions, e.g. about macOS privacy
	// settings.
	Hints []string
}

// Source yields notification records in delivery order.
//
// Implementations must be safe for use from a single polling goroutine, must
// open databases read-only (or otherwise never mutate the system store), and
// must return diagnostics instead of silently swallowing access failures.
type Source interface {
	// Poll returns up to limit records strictly after the given watermark,
	// oldest first, together with the updated watermark. A poll that finds
	// nothing returns an empty slice and the unchanged watermark.
	Poll(ctx context.Context, since Watermark, limit int) ([]Record, Watermark, error)
	// Diagnose probes the underlying store and reports accessibility,
	// schema and content information without requiring a poll loop.
	Diagnose(ctx context.Context) Diagnostics
	// Close releases any resources held by the source.
	Close() error
}
