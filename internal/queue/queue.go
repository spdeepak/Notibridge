// Package queue implements the durable delivery queue. Notifications are
// persisted once (deduplicated by source key), then moved through the states
// pending → sent (or failed) by the delivery worker. All state lives in
// NotiBridge's own SQLite database and survives application restarts.
package queue

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/spdeepak/notibridge/internal/notifications"
	"github.com/spdeepak/notibridge/internal/storage"
)

// Delivery status values. They match the CHECK constraint in the schema.
const (
	StatusPending = "pending"
	StatusSent    = "sent"
	StatusFailed  = "failed"
)

// Delivery is one queued notification-to-Telegram delivery.
type Delivery struct {
	ID                int64
	SourceKey         string
	AppID             string
	Title             string
	Subtitle          string
	Body              string
	DeliveredAt       time.Time
	Status            string
	Attempts          int
	NextAttemptAt     time.Time
	LastAttemptAt     time.Time
	TelegramMessageID int64
	HasMessageID      bool
	LastError         string
	CreatedAt         time.Time
	SentAt            time.Time
}

// Counts summarizes queue state for the UI.
type Counts struct {
	Pending int64
	Sent    int64
	Failed  int64
}

// Options tunes retry behavior.
type Options struct {
	// MaxAttempts is the number of delivery attempts after which a
	// notification is marked failed permanently. Defaults to 8.
	MaxAttempts int
	// BaseBackoff is the first retry delay; it doubles per attempt.
	// Defaults to 5s.
	BaseBackoff time.Duration
	// MaxBackoff caps the exponential delay. Defaults to 10m.
	MaxBackoff time.Duration
}

func (o Options) withDefaults() Options {
	if o.MaxAttempts <= 0 {
		o.MaxAttempts = 8
	}
	if o.BaseBackoff <= 0 {
		o.BaseBackoff = 5 * time.Second
	}
	if o.MaxBackoff <= 0 {
		o.MaxBackoff = 10 * time.Minute
	}
	return o
}

// Queue provides atomic enqueue-with-deduplication and status transitions.
// It is safe for concurrent use.
type Queue struct {
	db   *storage.DB
	opts Options

	rngMu sync.Mutex
	rng   *rand.Rand
}

// New creates a queue backed by the given database.
func New(db *storage.DB, opts Options) *Queue {
	opts = opts.withDefaults()
	return &Queue{
		db:   db,
		opts: opts,
		rng:  rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// Enqueue atomically inserts a pending delivery unless a record with the same
// source key already exists (deduplication). It reports whether the record
// was newly inserted. Content is retained only here, for retry purposes.
func (q *Queue) Enqueue(ctx context.Context, n notifications.Notification, sourceKey string) (bool, error) {
	deliveredAt := ""
	if !n.DeliveredAt.IsZero() {
		deliveredAt = n.DeliveredAt.UTC().Format(time.RFC3339Nano)
	}
	res, err := q.db.SQL().ExecContext(ctx, `
		INSERT OR IGNORE INTO deliveries
			(source_key, app_id, title, subtitle, body, delivered_at, status, attempts, next_attempt_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, 'pending', 0, 0, ?)`,
		sourceKey, n.AppID, n.Title, n.Subtitle, n.Body, deliveredAt,
		time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return false, fmt.Errorf("enqueue delivery: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("enqueue delivery: %w", err)
	}
	return affected > 0, nil
}

// ClaimDue returns up to limit pending deliveries whose next attempt time has
// passed, oldest first. Claiming is read-only; the worker confirms outcomes
// through MarkSent/MarkRetry/MarkFailed, so a crash between claim and mark
// simply leaves the record pending for the next cycle.
func (q *Queue) ClaimDue(ctx context.Context, now time.Time, limit int) ([]Delivery, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := q.db.SQL().QueryContext(ctx, `
		SELECT id, source_key, app_id, title, subtitle, body, delivered_at, status,
		       attempts, COALESCE(next_attempt_at, 0), COALESCE(last_attempt_at, 0),
		       COALESCE(telegram_message_id, 0), COALESCE(has_msg_id, 0), last_error, created_at, COALESCE(sent_at, '')
		FROM (
			SELECT id, source_key, app_id, title, subtitle, body, delivered_at, status,
			       attempts, next_attempt_at, last_attempt_at, telegram_message_id,
			       CASE WHEN telegram_message_id IS NULL THEN 0 ELSE 1 END AS has_msg_id,
			       last_error, created_at, sent_at
			FROM deliveries
			WHERE status = 'pending' AND next_attempt_at <= ?
			ORDER BY next_attempt_at ASC, id ASC
			LIMIT ?
		)`, now.UTC().Unix(), limit)
	if err != nil {
		return nil, fmt.Errorf("claim deliveries: %w", err)
	}
	defer rows.Close()
	return scanDeliveries(rows)
}

// MarkSent records a successful delivery. The notification is only marked
// sent after the Telegram API confirmed acceptance.
func (q *Queue) MarkSent(ctx context.Context, id, messageID int64, now time.Time) error {
	_, err := q.db.SQL().ExecContext(ctx, `
		UPDATE deliveries
		SET status = 'sent', attempts = attempts + 1, last_attempt_at = ?,
		    telegram_message_id = ?, last_error = '', sent_at = ?
		WHERE id = ?`,
		now.UTC().Unix(), messageID, now.UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		return fmt.Errorf("mark sent: %w", err)
	}
	return nil
}

// MarkRetry reschedules a pending delivery for a later attempt.
func (q *Queue) MarkRetry(ctx context.Context, id int64, next time.Time, errMsg string) error {
	_, err := q.db.SQL().ExecContext(ctx, `
		UPDATE deliveries
		SET status = 'pending', attempts = attempts + 1, last_attempt_at = ?,
		    next_attempt_at = ?, last_error = ?
		WHERE id = ?`,
		time.Now().UTC().Unix(), next.UTC().Unix(), errMsg, id)
	if err != nil {
		return fmt.Errorf("mark retry: %w", err)
	}
	return nil
}

// MarkFailed permanently fails a delivery after the retry budget is exhausted
// or a non-retryable API error occurred.
func (q *Queue) MarkFailed(ctx context.Context, id int64, errMsg string) error {
	_, err := q.db.SQL().ExecContext(ctx, `
		UPDATE deliveries
		SET status = 'failed', attempts = attempts + 1, last_attempt_at = ?,
		    next_attempt_at = 0, last_error = ?
		WHERE id = ?`,
		time.Now().UTC().Unix(), errMsg, id)
	if err != nil {
		return fmt.Errorf("mark failed: %w", err)
	}
	return nil
}

// Counts returns the number of deliveries in each status.
func (q *Queue) Counts(ctx context.Context) (Counts, error) {
	var c Counts
	rows, err := q.db.SQL().QueryContext(ctx, `SELECT status, COUNT(*) FROM deliveries GROUP BY status`)
	if err != nil {
		return c, fmt.Errorf("count deliveries: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var n int64
		if err := rows.Scan(&status, &n); err != nil {
			return c, fmt.Errorf("count deliveries: %w", err)
		}
		switch status {
		case StatusPending:
			c.Pending = n
		case StatusSent:
			c.Sent = n
		case StatusFailed:
			c.Failed = n
		}
	}
	return c, rows.Err()
}

// Recent returns the most recent deliveries for the activity view.
func (q *Queue) Recent(ctx context.Context, limit int) ([]Delivery, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := q.db.SQL().QueryContext(ctx, `
		SELECT id, source_key, app_id, title, subtitle, body, delivered_at, status,
		       attempts, COALESCE(next_attempt_at, 0), COALESCE(last_attempt_at, 0),
		       COALESCE(telegram_message_id, 0),
		       CASE WHEN telegram_message_id IS NULL THEN 0 ELSE 1 END,
		       last_error, created_at, COALESCE(sent_at, '')
		FROM deliveries
		ORDER BY id DESC
		LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("recent deliveries: %w", err)
	}
	defer rows.Close()
	return scanDeliveries(rows)
}

// ErrExhausted is returned (via errors.Is) by nothing at present; kept as the
// documented sentinel for "retry budget exhausted" decisions in the worker.
var ErrExhausted = errors.New("retry budget exhausted")

// BackoffDelay computes the delay before retry number `attempt` (1-based):
// base * 2^(attempt-1), capped at max, multiplied by a random jitter factor
// in [0.5, 1.5) to avoid thundering herds.
func BackoffDelay(attempt int, base, max time.Duration, jitter func(time.Duration) time.Duration) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := base
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= max {
			d = max
			break
		}
	}
	if d > max {
		d = max
	}
	if jitter != nil {
		d = jitter(d)
	}
	if d < time.Second {
		d = time.Second
	}
	if d > max*2 {
		d = max * 2
	}
	return d
}

// Backoff computes the jittered delay for the given attempt using the
// queue's configuration.
func (q *Queue) Backoff(attempt int) time.Duration {
	return BackoffDelay(attempt, q.opts.BaseBackoff, q.opts.MaxBackoff, q.jitter)
}

func (q *Queue) jitter(d time.Duration) time.Duration {
	q.rngMu.Lock()
	defer q.rngMu.Unlock()
	// factor in [0.5, 1.5)
	factor := 0.5 + q.rng.Float64()
	return time.Duration(float64(d) * factor)
}

// MaxAttempts exposes the configured retry budget.
func (q *Queue) MaxAttempts() int { return q.opts.MaxAttempts }

func scanDeliveries(rows *sql.Rows) ([]Delivery, error) {
	var out []Delivery
	for rows.Next() {
		var d Delivery
		var deliveredAt, createdAt, sentAt string
		var nextAttempt, lastAttempt int64
		var hasMsgID int
		if err := rows.Scan(&d.ID, &d.SourceKey, &d.AppID, &d.Title, &d.Subtitle, &d.Body,
			&deliveredAt, &d.Status, &d.Attempts, &nextAttempt, &lastAttempt,
			&d.TelegramMessageID, &hasMsgID, &d.LastError, &createdAt, &sentAt); err != nil {
			return nil, fmt.Errorf("scan delivery: %w", err)
		}
		d.DeliveredAt, _ = time.Parse(time.RFC3339Nano, deliveredAt)
		d.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
		d.SentAt, _ = time.Parse(time.RFC3339Nano, sentAt)
		if nextAttempt > 0 {
			d.NextAttemptAt = time.Unix(nextAttempt, 0).UTC()
		}
		if lastAttempt > 0 {
			d.LastAttemptAt = time.Unix(lastAttempt, 0).UTC()
		}
		d.HasMessageID = hasMsgID == 1
		out = append(out, d)
	}
	return out, rows.Err()
}
