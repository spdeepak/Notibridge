package queue

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/spdeepak/notibridge/internal/notifications"
	"github.com/spdeepak/notibridge/internal/storage"
)

func newQueue(t *testing.T, opts Options) (*Queue, *storage.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := storage.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return New(db, opts), db, path
}

func notif(title string, ts time.Time) notifications.Notification {
	return notifications.Notification{
		AppID: "com.example.app", Title: title, Subtitle: "sub", Body: "body " + title,
		DeliveredAt: ts, UUID: "",
	}
}

func TestEnqueueDeduplicatesBySourceKey(t *testing.T) {
	q, _, _ := newQueue(t, Options{})
	ctx := context.Background()
	n := notif("hello", time.Now())
	inserted, err := q.Enqueue(ctx, n, "f:abc")
	if err != nil || !inserted {
		t.Fatalf("first enqueue = %v, %v; want true, nil", inserted, err)
	}
	inserted, err = q.Enqueue(ctx, n, "f:abc")
	if err != nil {
		t.Fatal(err)
	}
	if inserted {
		t.Error("duplicate source key must not be inserted")
	}
	counts, _ := q.Counts(ctx)
	if counts.Pending != 1 {
		t.Errorf("pending = %d, want 1", counts.Pending)
	}
}

func TestStatusTransitions(t *testing.T) {
	q, _, _ := newQueue(t, Options{})
	ctx := context.Background()
	if _, err := q.Enqueue(ctx, notif("a", time.Now()), "k1"); err != nil {
		t.Fatal(err)
	}
	due, err := q.ClaimDue(ctx, time.Now(), 10)
	if err != nil || len(due) != 1 {
		t.Fatalf("ClaimDue = %d, %v", len(due), err)
	}
	if err := q.MarkSent(ctx, due[0].ID, 42, time.Now()); err != nil {
		t.Fatal(err)
	}
	counts, _ := q.Counts(ctx)
	if counts.Sent != 1 || counts.Pending != 0 {
		t.Errorf("counts after send = %+v", counts)
	}
	recent, _ := q.Recent(ctx, 10)
	if len(recent) != 1 || recent[0].Status != StatusSent || recent[0].TelegramMessageID != 42 || !recent[0].HasMessageID {
		t.Errorf("recent[0] = %+v", recent[0])
	}
}

func TestRetryThenFailureTransitions(t *testing.T) {
	q, _, _ := newQueue(t, Options{})
	ctx := context.Background()
	q.Enqueue(ctx, notif("a", time.Now()), "k1")
	due, _ := q.ClaimDue(ctx, time.Now(), 1)
	next := time.Now().Add(time.Hour)
	if err := q.MarkRetry(ctx, due[0].ID, next, "boom"); err != nil {
		t.Fatal(err)
	}
	// Not due yet.
	due, _ = q.ClaimDue(ctx, time.Now(), 1)
	if len(due) != 0 {
		t.Fatalf("claimed %d deliveries before due time", len(due))
	}
	// Due after the scheduled time.
	due, _ = q.ClaimDue(ctx, next.Add(time.Second), 1)
	if len(due) != 1 || due[0].Attempts != 1 || due[0].LastError != "boom" {
		t.Fatalf("due[0] = %+v", due[0])
	}
	if err := q.MarkFailed(ctx, due[0].ID, "giving up"); err != nil {
		t.Fatal(err)
	}
	counts, _ := q.Counts(ctx)
	if counts.Failed != 1 || counts.Pending != 0 {
		t.Errorf("counts = %+v", counts)
	}
}

func TestQueuePersistsAcrossReopen(t *testing.T) {
	q, db, path := newQueue(t, Options{})
	ctx := context.Background()
	q.Enqueue(ctx, notif("persist me", time.Now()), "k-persist")
	due, _ := q.ClaimDue(ctx, time.Now(), 1)
	if err := q.MarkRetry(ctx, due[0].ID, time.Now().Add(30*time.Minute), "temp"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db2, err := storage.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	q2 := New(db2, Options{})
	due, err = q2.ClaimDue(ctx, time.Now(), 10)
	if err != nil || len(due) != 0 {
		t.Fatalf("after reopen, claimed %d (want 0): %v", len(due), err)
	}
	due, _ = q2.ClaimDue(ctx, time.Now().Add(time.Hour), 10)
	if len(due) != 1 || due[0].Title != "persist me" || due[0].Attempts != 1 {
		t.Fatalf("pending delivery not restored: %+v", due[0])
	}
	// Dedup key survived too.
	inserted, _ := q2.Enqueue(ctx, notif("persist me", time.Now()), "k-persist")
	if inserted {
		t.Error("deduplication must survive restart")
	}
}

func TestBackoffDelayGrowsAndCaps(t *testing.T) {
	noJitter := func(d time.Duration) time.Duration { return d }
	d1 := BackoffDelay(1, time.Second, time.Minute, noJitter)
	d2 := BackoffDelay(2, time.Second, time.Minute, noJitter)
	d3 := BackoffDelay(3, time.Second, time.Minute, noJitter)
	if d1 != time.Second || d2 != 2*time.Second || d3 != 4*time.Second {
		t.Errorf("backoff = %v %v %v", d1, d2, d3)
	}
	if d := BackoffDelay(20, time.Second, time.Minute, noJitter); d != time.Minute {
		t.Errorf("capped backoff = %v, want 1m", d)
	}
	// Attempt numbers below 1 are normalized.
	if d := BackoffDelay(0, time.Second, time.Minute, noJitter); d != time.Second {
		t.Errorf("attempt 0 = %v", d)
	}
}

func TestBackoffJitterStaysWithinBounds(t *testing.T) {
	q, _, _ := newQueue(t, Options{BaseBackoff: time.Second, MaxBackoff: time.Minute})
	for i := 0; i < 200; i++ {
		d := q.Backoff(1)
		if d < 500*time.Millisecond || d > 1500*time.Millisecond {
			t.Fatalf("jittered backoff out of range: %v", d)
		}
	}
}
