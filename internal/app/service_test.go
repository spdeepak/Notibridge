package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"howett.net/plist"

	"github.com/spdeepak/notibridge/internal/config"
	"github.com/spdeepak/notibridge/internal/notifications/source"
	"github.com/spdeepak/notibridge/internal/queue"
)

// fakeSource is an injectable source.Source for tests.
type fakeSource struct {
	mu      sync.Mutex
	records []source.Record
	polls   int
}

func (f *fakeSource) Poll(_ context.Context, since source.Watermark, limit int) ([]source.Record, source.Watermark, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.polls++
	var out []source.Record
	for _, r := range f.records {
		if since.Valid && (r.DeliveredAt.Unix() < since.DeliveredDateSecs ||
			(r.DeliveredAt.Unix() == since.DeliveredDateSecs && r.RecID <= since.RecID)) {
			continue
		}
		out = append(out, r)
	}
	wm := since
	if n := len(out); n > 0 {
		last := out[n-1]
		wm = source.Watermark{DeliveredDateSecs: last.DeliveredAt.Unix(), RecID: last.RecID, Valid: true}
	}
	return out, wm, nil
}

func (f *fakeSource) Diagnose(context.Context) source.Diagnostics {
	return source.Diagnostics{Path: "/fake", Accessible: true, SchemaOK: true}
}

func (f *fakeSource) Close() error { return nil }

func (f *fakeSource) add(r source.Record) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records = append(f.records, r)
}

func makeRecord(t *testing.T, recID int64, uuid []byte, app, title string, ts time.Time) source.Record {
	t.Helper()
	payload, err := plist.Marshal(map[string]interface{}{
		"app":  app,
		"date": float64(ts.Unix()) - 978307200,
		"req":  map[string]interface{}{"titl": title, "body": "body of " + title},
	}, plist.BinaryFormat)
	if err != nil {
		t.Fatal(err)
	}
	return source.Record{RecID: recID, UUIDHex: hexStr(uuid), AppID: app, DeliveredAt: ts, Payload: payload}
}

func hexStr(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, digits[c>>4], digits[c&0xf])
	}
	return string(out)
}

// telegramCapture is a fake Telegram Bot API.
type telegramCapture struct {
	mu       sync.Mutex
	messages []string
	failNext int // number of upcoming sendMessage calls to fail with 500
	rateNext int // number of upcoming calls to fail with 429 retry_after=1
}

func (tc *telegramCapture) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tc.mu.Lock()
		defer tc.mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/getMe") {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"ok": true, "result": map[string]interface{}{"id": 1, "username": "fake_bot"},
			})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/sendMessage") {
			var req struct {
				Text string `json:"text"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			if tc.failNext > 0 {
				tc.failNext--
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "error_code": 500, "description": "boom"})
				return
			}
			if tc.rateNext > 0 {
				tc.rateNext--
				w.WriteHeader(http.StatusTooManyRequests)
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"ok": false, "error_code": 429, "description": "Too Many Requests",
					"parameters": map[string]interface{}{"retry_after": 1},
				})
				return
			}
			tc.messages = append(tc.messages, req.Text)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "result": map[string]interface{}{"message_id": int64(len(tc.messages))}})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}
}

func (tc *telegramCapture) count() int {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	return len(tc.messages)
}

func (tc *telegramCapture) last() string {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	if len(tc.messages) == 0 {
		return ""
	}
	return tc.messages[len(tc.messages)-1]
}

func newTestService(t *testing.T, src *fakeSource, tc *telegramCapture, srv *httptest.Server) *Service {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("NOTIBRIDGE_HOME", dir)
	svc, err := New(Options{
		Source:          src,
		DBPath:          filepath.Join(dir, "state.db"),
		Secrets:         config.NewFileStore(dir),
		TelegramBaseURL: srv.URL,
		Queue:           queue.Options{MaxAttempts: 5, BaseBackoff: time.Millisecond, MaxBackoff: 10 * time.Millisecond},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return svc
}

func configure(t *testing.T, svc *Service) {
	t.Helper()
	token := "test-token-123"
	ctx := context.Background()
	if err := svc.ApplyConfig(ctx, ConfigUpdate{
		ChatID:               "42",
		BotToken:             &token,
		PollIntervalSeconds:  1,
		ForwardingEnabled:    true,
		ExcludedApps:         []string{},
		HistoryRetentionDays: 7,
		MaxAttempts:          5,
	}); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", msg)
}

func TestEndToEndForwardAndDedup(t *testing.T) {
	tc := &telegramCapture{}
	srv := httptest.NewServer(tc.handler())
	defer srv.Close()

	now := time.Now()
	src := &fakeSource{}
	src.add(makeRecord(t, 1, []byte{0xaa}, "com.whatsapp.WhatsApp", "Hello", now))
	svc := newTestService(t, src, tc, srv)
	configure(t, svc)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc.Start(ctx)
	t.Cleanup(func() {
		shCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = svc.Shutdown(shCtx)
	})

	waitFor(t, 10*time.Second, func() bool { return tc.count() == 1 }, "first delivery")
	if !strings.Contains(tc.last(), "Hello") || !strings.Contains(tc.last(), "<b>App:</b> WhatsApp") {
		t.Errorf("message = %q", tc.last())
	}

	// The same records stay in the fake source across polls; no duplicates.
	time.Sleep(1500 * time.Millisecond)
	if n := tc.count(); n != 1 {
		t.Errorf("delivered %d times, want 1 (dedup across polling cycles)", n)
	}

	st := svc.Status()
	if st.Counts.Sent != 1 || st.Counts.Pending != 0 {
		t.Errorf("counts = %+v", st.Counts)
	}
	if !st.Configured || !st.ForwardingEnabled || st.Paused {
		t.Errorf("status = %+v", st)
	}
}

func TestRestartDoesNotRedeliver(t *testing.T) {
	tc := &telegramCapture{}
	srv := httptest.NewServer(tc.handler())
	defer srv.Close()

	dir := t.TempDir()
	t.Setenv("NOTIBRIDGE_HOME", dir)
	now := time.Now()
	src := &fakeSource{}
	src.add(makeRecord(t, 1, []byte{0xbb}, "com.apple.MobileSMS", "Ping", now))

	opts := Options{
		Source:          src,
		DBPath:          filepath.Join(dir, "state.db"),
		Secrets:         config.NewFileStore(dir),
		TelegramBaseURL: srv.URL,
		Queue:           queue.Options{MaxAttempts: 5, BaseBackoff: time.Millisecond, MaxBackoff: 10 * time.Millisecond},
	}
	svc, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	configure(t, svc)
	ctx1, cancel1 := context.WithCancel(context.Background())
	svc.Start(ctx1)
	waitFor(t, 10*time.Second, func() bool { return tc.count() == 1 }, "first-run delivery")
	cancel1()
	shCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
	if err := svc.Shutdown(shCtx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	c()

	// Restart with the same database and the same records still visible.
	svc2, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	configure(t, svc2)
	ctx2, cancel2 := context.WithCancel(context.Background())
	svc2.Start(ctx2)
	defer func() {
		shCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = svc2.Shutdown(shCtx)
	}()
	cancel2_ := cancel2
	_ = cancel2_
	time.Sleep(2 * time.Second)
	if n := tc.count(); n != 1 {
		t.Errorf("after restart: %d deliveries, want 1 (dedup must survive restarts)", n)
	}
}

func TestPauseStopsDeliveryAndResumeDrains(t *testing.T) {
	tc := &telegramCapture{}
	srv := httptest.NewServer(tc.handler())
	defer srv.Close()

	now := time.Now()
	src := &fakeSource{}
	src.add(makeRecord(t, 1, []byte{0xcc}, "com.slack.Slack", "Mention", now))
	svc := newTestService(t, src, tc, srv)
	configure(t, svc)
	svc.Pause()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc.Start(ctx)
	t.Cleanup(func() {
		shCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = svc.Shutdown(shCtx)
	})

	time.Sleep(1500 * time.Millisecond)
	if n := tc.count(); n != 0 {
		t.Fatalf("paused service sent %d messages", n)
	}
	if st := svc.Status(); st.Counts.Pending != 1 {
		t.Errorf("paused queue should hold the notification: %+v", st.Counts)
	}

	svc.Resume()
	waitFor(t, 10*time.Second, func() bool { return tc.count() == 1 }, "delivery after resume")
}

func TestExcludedAppsAreNotQueuedOrSent(t *testing.T) {
	tc := &telegramCapture{}
	srv := httptest.NewServer(tc.handler())
	defer srv.Close()

	now := time.Now()
	src := &fakeSource{}
	src.add(makeRecord(t, 1, []byte{0xdd}, "com.example.noisy", "Noise", now))
	svc := newTestService(t, src, tc, srv)

	token := "tok"
	if err := svc.ApplyConfig(context.Background(), ConfigUpdate{
		ChatID: "42", BotToken: &token, PollIntervalSeconds: 1, ForwardingEnabled: true,
		ExcludedApps: []string{"com.example.*"}, HistoryRetentionDays: 7, MaxAttempts: 5,
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc.Start(ctx)
	t.Cleanup(func() {
		shCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = svc.Shutdown(shCtx)
	})

	time.Sleep(1500 * time.Millisecond)
	if n := tc.count(); n != 0 {
		t.Errorf("excluded app was forwarded %d times", n)
	}
	if st := svc.Status(); st.Counts.Pending != 0 {
		t.Errorf("excluded app was queued: %+v", st.Counts)
	}
}

func TestUnconfiguredServiceStoresNothing(t *testing.T) {
	tc := &telegramCapture{}
	srv := httptest.NewServer(tc.handler())
	defer srv.Close()

	now := time.Now()
	src := &fakeSource{}
	src.add(makeRecord(t, 1, []byte{0xee}, "com.apple.finder", "Window", now))
	svc := newTestService(t, src, tc, srv)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc.Start(ctx)
	t.Cleanup(func() {
		shCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = svc.Shutdown(shCtx)
	})

	time.Sleep(1500 * time.Millisecond)
	if n := tc.count(); n != 0 {
		t.Errorf("unconfigured service sent %d messages", n)
	}
	if st := svc.Status(); st.Counts.Pending != 0 {
		t.Errorf("unconfigured service queued content: %+v", st.Counts)
	}
}

func TestRetryOnServerErrorThenSuccess(t *testing.T) {
	tc := &telegramCapture{}
	tc.failNext = 2
	srv := httptest.NewServer(tc.handler())
	defer srv.Close()

	now := time.Now()
	src := &fakeSource{}
	src.add(makeRecord(t, 1, []byte{0x11}, "com.apple.MobileSMS", "Retry me", now))
	svc := newTestService(t, src, tc, srv)
	configure(t, svc)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc.Start(ctx)
	t.Cleanup(func() {
		shCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = svc.Shutdown(shCtx)
	})

	waitFor(t, 15*time.Second, func() bool {
		st := svc.Status()
		return st.Counts.Sent == 1
	}, "delivery after retries")
	if n := tc.count(); n != 1 {
		t.Errorf("delivered %d times, want exactly 1", n)
	}
}

func TestPermanentAPIErrorMarksFailed(t *testing.T) {
	// A chat-not-found style error should fail permanently.
	var mu sync.Mutex
	reject := true
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/sendMessage") {
			mu.Lock()
			bad := reject
			mu.Unlock()
			if bad {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "error_code": 400, "description": "Bad Request: chat not found"})
				return
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "result": map[string]interface{}{"message_id": 1}})
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	now := time.Now()
	src := &fakeSource{}
	src.add(makeRecord(t, 1, []byte{0x22}, "com.apple.MobileSMS", "Doomed", now))
	svc := newTestService(t, src, nil, srv)
	_ = tcNilGuard(tcNil)
	configure(t, svc)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc.Start(ctx)
	t.Cleanup(func() {
		shCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = svc.Shutdown(shCtx)
	})

	waitFor(t, 15*time.Second, func() bool {
		return svc.Status().Counts.Failed == 1
	}, "permanent failure")
	recent, err := svc.RecentDeliveries(context.Background(), 5)
	if err != nil || len(recent) != 1 {
		t.Fatalf("recent = %+v, %v", recent, err)
	}
	if !strings.Contains(recent[0].LastError, "chat not found") {
		t.Errorf("last error = %q", recent[0].LastError)
	}
}

// tcNilGuard/nil keep the tc param referenced in tests that pass nil captures.
var tcNil *telegramCapture

func tcNilGuard(_ *telegramCapture) error { return nil }

func TestClearHistory(t *testing.T) {
	tc := &telegramCapture{}
	srv := httptest.NewServer(tc.handler())
	defer srv.Close()

	now := time.Now()
	src := &fakeSource{}
	src.add(makeRecord(t, 1, []byte{0x33}, "com.apple.MobileSMS", "Ephemeral", now))
	svc := newTestService(t, src, tc, srv)
	configure(t, svc)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc.Start(ctx)
	t.Cleanup(func() {
		shCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = svc.Shutdown(shCtx)
	})

	waitFor(t, 10*time.Second, func() bool { return tc.count() == 1 }, "delivery")
	n, err := svc.ClearHistory(context.Background())
	if err != nil || n == 0 {
		t.Fatalf("ClearHistory = %d, %v", n, err)
	}
	if st := svc.Status(); st.Counts.Sent != 0 {
		t.Errorf("history not cleared: %+v", st.Counts)
	}
}

func TestGracefulShutdownIsFast(t *testing.T) {
	tc := &telegramCapture{}
	srv := httptest.NewServer(tc.handler())
	defer srv.Close()

	src := &fakeSource{}
	svc := newTestService(t, src, tc, srv)
	configure(t, svc)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc.Start(ctx)

	start := time.Now()
	shCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
	defer c()
	if err := svc.Shutdown(shCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("shutdown took %s", d)
	}
}

func TestTestConnection(t *testing.T) {
	tc := &telegramCapture{}
	srv := httptest.NewServer(tc.handler())
	defer srv.Close()

	src := &fakeSource{}
	svc := newTestService(t, src, tc, srv)
	configure(t, svc)

	name, err := svc.TestConnection(context.Background())
	if err != nil || name != "fake_bot" {
		t.Errorf("TestConnection = %q, %v", name, err)
	}
}

func TestMaskChatID(t *testing.T) {
	if got := maskChatID("-1001234567890"); got != "…7890" {
		t.Errorf("maskChatID = %q", got)
	}
	if got := maskChatID(""); got != "" {
		t.Errorf("maskChatID empty = %q", got)
	}
	if got := maskChatID("1234"); got != "…4" {
		t.Errorf("maskChatID short = %q", got)
	}
	if got := maskChatID("123456789"); got != "…6789" {
		t.Errorf("maskChatID mid = %q", got)
	}
}
