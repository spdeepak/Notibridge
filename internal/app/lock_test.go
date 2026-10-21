package app

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spdeepak/notibridge/internal/config"
	"github.com/spdeepak/notibridge/internal/queue"
)

// fakeLock is an injectable screen lock state.
type fakeLock struct{ locked atomic.Bool }

func (f *fakeLock) Locked() bool { return f.locked.Load() }

func newLockService(t *testing.T, src *fakeSource, tc *telegramCapture, srv *httptest.Server, lock *fakeLock) *Service {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("NOTIBRIDGE_HOME", dir)
	svc, err := New(Options{
		Source:          src,
		DBPath:          filepath.Join(dir, "state.db"),
		Secrets:         config.NewFileStore(dir),
		TelegramBaseURL: srv.URL,
		LockState:       lock,
		Queue:           queue.Options{MaxAttempts: 5, BaseBackoff: time.Millisecond, MaxBackoff: 10 * time.Millisecond},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return svc
}

func applyLockConfig(t *testing.T, svc *Service, onlyWhenLocked bool) {
	t.Helper()
	token := "test-token-123"
	if err := svc.ApplyConfig(context.Background(), ConfigUpdate{
		ChatID:               "42",
		BotToken:             &token,
		PollIntervalSeconds:  1,
		ForwardingEnabled:    true,
		ExcludedApps:         []string{},
		HistoryRetentionDays: 7,
		MaxAttempts:          5,
		OnlyWhenScreenLocked: onlyWhenLocked,
	}); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
}

// TestIgnoresNotificationsWhileUnlocked covers the screen lock restriction:
// notifications that arrive while the screen is unlocked are ignored, and the
// watermark advances so they are not replayed once the screen locks again.
func TestIgnoresNotificationsWhileUnlocked(t *testing.T) {
	tc := &telegramCapture{}
	srv := httptest.NewServer(tc.handler())
	defer srv.Close()

	now := time.Now()
	src := &fakeSource{}
	src.add(makeRecord(t, 1, []byte{0xcc}, "com.apple.MobileSMS", "Ping", now))
	lock := &fakeLock{}
	svc := newLockService(t, src, tc, srv, lock)
	applyLockConfig(t, svc, true)

	if err := svc.pollOnce(context.Background()); err != nil {
		t.Fatalf("pollOnce: %v", err)
	}
	if n := svc.Status().Counts.Pending; n != 0 {
		t.Errorf("pending = %d, want 0 (notifications are ignored while unlocked)", n)
	}
	if n := svc.Status().RecordsQueued; n != 0 {
		t.Errorf("records queued = %d, want 0", n)
	}

	// The watermark advanced, so the ignored notification is not replayed
	// after the screen locks.
	lock.locked.Store(true)
	if err := svc.pollOnce(context.Background()); err != nil {
		t.Fatalf("pollOnce: %v", err)
	}
	if n := svc.Status().Counts.Pending; n != 0 {
		t.Errorf("pending = %d, want 0 (an ignored notification must not be replayed)", n)
	}
}

// TestForwardsWhileScreenLocked covers the same restriction with the screen
// locked: the notification is forwarded.
func TestForwardsWhileScreenLocked(t *testing.T) {
	tc := &telegramCapture{}
	srv := httptest.NewServer(tc.handler())
	defer srv.Close()

	now := time.Now()
	src := &fakeSource{}
	src.add(makeRecord(t, 1, []byte{0xdd}, "com.whatsapp.WhatsApp", "Hello", now))
	lock := &fakeLock{}
	lock.locked.Store(true)
	svc := newLockService(t, src, tc, srv, lock)
	applyLockConfig(t, svc, true)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc.Start(ctx)
	t.Cleanup(func() {
		shCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = svc.Shutdown(shCtx)
	})

	waitFor(t, 10*time.Second, func() bool { return tc.count() == 1 }, "delivery while the screen is locked")
	if !strings.Contains(tc.last(), "Hello") {
		t.Errorf("message = %q", tc.last())
	}
}

// TestForwardsWhileUnlockedWhenRestrictionOff covers the toggle being turned
// off: notifications are forwarded even while the screen is unlocked.
func TestForwardsWhileUnlockedWhenRestrictionOff(t *testing.T) {
	tc := &telegramCapture{}
	srv := httptest.NewServer(tc.handler())
	defer srv.Close()

	now := time.Now()
	src := &fakeSource{}
	src.add(makeRecord(t, 1, []byte{0xee}, "com.whatsapp.WhatsApp", "Hello", now))
	lock := &fakeLock{}
	svc := newLockService(t, src, tc, srv, lock)
	applyLockConfig(t, svc, false)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc.Start(ctx)
	t.Cleanup(func() {
		shCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = svc.Shutdown(shCtx)
	})

	waitFor(t, 10*time.Second, func() bool { return tc.count() == 1 }, "delivery while the screen is unlocked")
}

// TestTogglingForwardingKeepsTheScreenLockSetting makes sure the forwarding
// switch does not silently reset the screen lock restriction.
func TestTogglingForwardingKeepsTheScreenLockSetting(t *testing.T) {
	tc := &telegramCapture{}
	srv := httptest.NewServer(tc.handler())
	defer srv.Close()

	src := &fakeSource{}
	lock := &fakeLock{}
	svc := newLockService(t, src, tc, srv, lock)
	applyLockConfig(t, svc, true)

	if err := svc.SetForwarding(context.Background(), false); err != nil {
		t.Fatalf("SetForwarding: %v", err)
	}
	if !svc.Status().OnlyWhenScreenLocked {
		t.Error("toggling forwarding must not turn off the screen lock restriction")
	}
	if err := svc.SetOnlyWhenLocked(context.Background(), false); err != nil {
		t.Fatalf("SetOnlyWhenLocked: %v", err)
	}
	if svc.Status().OnlyWhenScreenLocked {
		t.Error("the screen lock restriction should be off")
	}
}
