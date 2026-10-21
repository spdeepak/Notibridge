// Package app contains the application service that coordinates notification
// discovery, deduplication, delivery and lifecycle. It has no dependency on
// the menu bar or web UI, so it can be reused by other frontends (or a future
// Nexflow integration) directly.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/spdeepak/notibridge/internal/config"
	"github.com/spdeepak/notibridge/internal/dedup"
	"github.com/spdeepak/notibridge/internal/filters"
	"github.com/spdeepak/notibridge/internal/logging"
	"github.com/spdeepak/notibridge/internal/notifications"
	"github.com/spdeepak/notibridge/internal/notifications/parser"
	"github.com/spdeepak/notibridge/internal/notifications/source"
	"github.com/spdeepak/notibridge/internal/queue"
	"github.com/spdeepak/notibridge/internal/storage"
	"github.com/spdeepak/notibridge/internal/telegram"
	"github.com/spdeepak/notibridge/internal/update"
)

// Version is the application version, overridable at link time.
var Version = "0.1.0"

const watermarkMetaKey = "source_watermark"

// Options configures a Service.
type Options struct {
	Logger   *slog.Logger
	Source   source.Source // nil selects the macOS Notification Center source
	DBPath   string        // "" uses the default Application Support path
	Lookback time.Duration
	Queue    queue.Options
	// Secrets overrides the secret store (tests use a file store so the
	// real Keychain is never touched). nil selects the default.
	Secrets config.SecretStore
	// TelegramBaseURL overrides the Bot API endpoint (tests).
	TelegramBaseURL string
	// UpdateOwner and UpdateRepo identify the GitHub repository whose
	// releases are checked for updates.
	UpdateOwner string
	UpdateRepo  string
	// UpdateAPIBaseURL overrides the GitHub API root (tests).
	UpdateAPIBaseURL string
	// UpdateHTTPClient overrides the HTTP client used by the updater (tests).
	UpdateHTTPClient *http.Client
}

// Status is a snapshot of service state for the UI.
type Status struct {
	Version           string
	Configured        bool
	ForwardingEnabled bool
	Paused            bool
	SecretStore       string
	ChatIDMasked      string
	PollIntervalSecs  int
	ExcludedApps      int
	Counts            queue.Counts
	SourcePath        string
	SourceAccessible  bool
	SourceError       string
	LastPollAt        time.Time
	LastPollError     string
	LastSleepGap      time.Time
	RecordsSeen       int64
	RecordsQueued     int64
}

// Service coordinates the notification pipeline.
type Service struct {
	logger  *slog.Logger
	src     source.Source
	_ownSrc bool
	db      *storage.DB
	q       *queue.Queue
	secrets config.SecretStore
	httpc   *http.Client
	tgBase  string

	mu     sync.Mutex
	cfg    config.Config
	filter *filters.Filter
	token  config.SecretString

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	wakeCh chan struct{}

	paused atomic.Bool

	// Release-based self-update.
	upd      *update.Updater
	updMu    sync.Mutex
	updState UpdateState

	// QuitRequested is closed when a frontend asks the application to stop.
	quitCh   chan struct{}
	quitOnce sync.Once

	// Observability for the UI.
	srcAccessible atomic.Bool
	srcError      atomic.Value // string
	lastPollAt    atomic.Int64 // unix nano
	lastPollErr   atomic.Value // string
	lastSleepGap  atomic.Int64 // unix nano
	recordsSeen   atomic.Int64
	recordsQueued atomic.Int64
}

// New builds the service, opening the application database and loading
// configuration and secrets.
func New(opts Options) (*Service, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	dbPath := opts.DBPath
	if dbPath == "" {
		var err error
		dbPath, err = config.StateDBPath()
		if err != nil {
			return nil, err
		}
	}
	db, err := storage.Open(dbPath)
	if err != nil {
		return nil, err
	}

	cfg, err := config.Load()
	if err != nil {
		// A broken config should not prevent startup; log and use defaults.
		opts.Logger.Warn("configuration could not be loaded; using defaults", "error", err)
		cfg = config.Default()
	}

	secrets := opts.Secrets
	if secrets == nil {
		secrets = config.DefaultSecretStore(context.Background(), filepath.Dir(dbPath))
	}
	token, err := secrets.Get(context.Background())
	if err != nil {
		opts.Logger.Warn("bot token could not be read", "store", secrets.Name(), "error", err)
	}

	src := opts.Source
	own := false
	if src == nil {
		src = source.NewMacOS(source.MacOSOptions{Lookback: opts.Lookback})
		own = true
	}

	s := &Service{
		logger:  opts.Logger,
		src:     src,
		_ownSrc: own,
		db:      db,
		q:       queue.New(db, opts.Queue),
		secrets: secrets,
		httpc:   &http.Client{Timeout: 20 * time.Second},
		tgBase:  opts.TelegramBaseURL,
		cfg:     cfg,
		filter:  filters.New(cfg.ExcludedApps),
		token:   token,
		wakeCh:  make(chan struct{}, 1),
		quitCh:  make(chan struct{}),
	}
	s.upd = newUpdater(opts.Logger, opts)
	if s.tgBase == "" {
		s.tgBase = telegram.DefaultBaseURL
	}
	s.srcError.Store("")
	s.lastPollErr.Store("")
	if token != "" {
		logging.RegisterSecret(token.Get())
	}
	return s, nil
}

// Start launches the poller, delivery worker and retention loops.
func (s *Service) Start(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx != nil {
		return
	}
	s.ctx, s.cancel = context.WithCancel(ctx)
	s.wg.Add(3)
	go s.pollLoop()
	go s.workerLoop()
	go s.retentionLoop()
	s.logger.Info("NotiBridge service started", "version", Version,
		"configured", s.configuredLocked(), "forwarding", s.cfg.ForwardingEnabled)
}

// Shutdown stops all workers and closes database handles. It blocks until
// workers exit or ctx expires.
func (s *Service) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	cancel := s.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	var err error
	select {
	case <-done:
	case <-ctx.Done():
		err = fmt.Errorf("shutdown timed out: %w", ctx.Err())
	}
	if e := s.db.Close(); e != nil && err == nil {
		err = fmt.Errorf("close state database: %w", e)
	}
	if s._ownSrc {
		if e := s.src.Close(); e != nil && err == nil {
			err = fmt.Errorf("close notification source: %w", e)
		}
	}
	s.logger.Info("NotiBridge service stopped")
	return err
}

// RequestQuit asks the frontends to shut the application down. It is used
// after an update was installed: the update helper waits for this process to
// exit before it replaces the application bundle.
func (s *Service) RequestQuit() {
	s.quitOnce.Do(func() { close(s.quitCh) })
}

// QuitRequested returns a channel that is closed when a quit was requested.
func (s *Service) QuitRequested() <-chan struct{} { return s.quitCh }

// ---- configuration ----

// ConfigUpdate describes a requested configuration change. A nil BotToken
// means "keep the stored token".
type ConfigUpdate struct {
	ChatID               string
	BotToken             *string
	PollIntervalSeconds  int
	ForwardingEnabled    bool
	ExcludedApps         []string
	HistoryRetentionDays int
	MaxAttempts          int
}

// ApplyConfig validates, persists and applies a configuration update. The
// bot token is stored in the configured secret store (macOS Keychain by
// default), never in the config file.
func (s *Service) ApplyConfig(ctx context.Context, upd ConfigUpdate) error {
	newCfg := config.Config{
		ChatID:               upd.ChatID,
		PollIntervalSeconds:  upd.PollIntervalSeconds,
		ForwardingEnabled:    upd.ForwardingEnabled,
		ExcludedApps:         upd.ExcludedApps,
		HistoryRetentionDays: upd.HistoryRetentionDays,
		MaxAttempts:          upd.MaxAttempts,
	}
	if err := newCfg.Validate(); err != nil {
		return err
	}
	if err := config.Save(newCfg); err != nil {
		return fmt.Errorf("persist configuration: %w", err)
	}

	newToken := s.token
	if upd.BotToken != nil {
		if err := s.secrets.Set(ctx, config.SecretString(*upd.BotToken)); err != nil {
			return fmt.Errorf("store bot token (%s): %w", s.secrets.Name(), err)
		}
		logging.UnregisterSecret(s.token.Get())
		newToken = config.SecretString(*upd.BotToken)
		if newToken != "" {
			logging.RegisterSecret(newToken.Get())
		}
	}

	s.mu.Lock()
	s.cfg = newCfg
	s.token = newToken
	s.filter = filters.New(newCfg.ExcludedApps)
	s.mu.Unlock()

	// A newly configured pipeline may have queued work waiting.
	s.wake()
	s.logger.Info("configuration updated", "forwarding", newCfg.ForwardingEnabled,
		"poll_seconds", newCfg.PollIntervalSeconds, "secret_store", s.secrets.Name())
	return nil
}

// SetForwarding toggles the master forwarding switch (persisted).
func (s *Service) SetForwarding(ctx context.Context, enabled bool) error {
	s.mu.Lock()
	upd := ConfigUpdate{
		ChatID:               s.cfg.ChatID,
		PollIntervalSeconds:  s.cfg.PollIntervalSeconds,
		ForwardingEnabled:    enabled,
		ExcludedApps:         s.cfg.ExcludedApps,
		HistoryRetentionDays: s.cfg.HistoryRetentionDays,
		MaxAttempts:          s.cfg.MaxAttempts,
	}
	s.mu.Unlock()
	return s.ApplyConfig(ctx, upd)
}

// Pause stops delivery without changing the persisted configuration; queued
// notifications are delivered after Resume.
func (s *Service) Pause() {
	if !s.paused.Swap(true) {
		s.logger.Info("forwarding paused")
	}
}

// Resume restarts delivery.
func (s *Service) Resume() {
	if s.paused.Swap(false) {
		s.logger.Info("forwarding resumed")
		s.wake()
	}
}

// SetPaused sets the pause state explicitly.
func (s *Service) SetPaused(p bool) {
	if p {
		s.Pause()
	} else {
		s.Resume()
	}
}

// SecretStoreName reports which secret backend is active ("keychain" or
// "file") so the UI can warn about the weaker fallback.
func (s *Service) SecretStoreName() string { return s.secrets.Name() }

// ---- actions ----

// TestConnection validates the bot token via getMe and returns the bot
// username. It does not send a message.
func (s *Service) TestConnection(ctx context.Context) (string, error) {
	c := s.client()
	if c.Token() == "" {
		return "", fmt.Errorf("bot token is not configured")
	}
	return c.Validate(ctx)
}

// SendTestMessage sends a confirmation message to the configured chat.
func (s *Service) SendTestMessage(ctx context.Context) error {
	if s.paused.Load() {
		return fmt.Errorf("forwarding is paused; resume before sending a test message")
	}
	return s.client().SendTestMessage(ctx)
}

// ClearHistory deletes all locally stored delivery records and content.
func (s *Service) ClearHistory(ctx context.Context) (int64, error) {
	n, err := s.db.ClearHistory(ctx)
	if err == nil {
		s.logger.Info("local notification history cleared", "records", n)
	}
	return n, err
}

// RecentDeliveries returns the latest delivery records for the activity view.
func (s *Service) RecentDeliveries(ctx context.Context, limit int) ([]queue.Delivery, error) {
	return s.q.Recent(ctx, limit)
}

// Diagnose probes the notification source and reports access details.
func (s *Service) Diagnose(ctx context.Context) source.Diagnostics {
	return s.src.Diagnose(ctx)
}

// Status returns a consistent snapshot for the UI.
func (s *Service) Status() Status {
	s.mu.Lock()
	cfg, token := s.cfg, s.token
	s.mu.Unlock()

	counts, _ := s.q.Counts(context.Background())
	st := Status{
		Version:           Version,
		Configured:        token != "" && cfg.ChatID != "",
		ForwardingEnabled: cfg.ForwardingEnabled,
		Paused:            s.paused.Load(),
		SecretStore:       s.secrets.Name(),
		ChatIDMasked:      maskChatID(cfg.ChatID),
		PollIntervalSecs:  cfg.PollIntervalSeconds,
		ExcludedApps:      len(cfg.ExcludedApps),
		Counts:            counts,
		SourceAccessible:  s.srcAccessible.Load(),
		SourceError:       s.srcError.Load().(string),
		RecordsSeen:       s.recordsSeen.Load(),
		RecordsQueued:     s.recordsQueued.Load(),
	}
	if v := s.lastPollAt.Load(); v > 0 {
		st.LastPollAt = time.Unix(0, v)
	}
	if v := s.lastSleepGap.Load(); v > 0 {
		st.LastSleepGap = time.Unix(0, v)
	}
	st.LastPollError = s.lastPollErr.Load().(string)
	if p, err := source.DetectDBPath(); err == nil {
		st.SourcePath = p
	}
	return st
}

// ---- internals ----

func (s *Service) configuredLocked() bool { return s.token != "" && s.cfg.ChatID != "" }

func (s *Service) client() *telegram.Client {
	s.mu.Lock()
	token, chatID := s.token, s.cfg.ChatID
	s.mu.Unlock()
	return telegram.NewWithBaseURL(token.Get(), chatID, s.tgBase, s.httpc)
}

func (s *Service) wake() {
	select {
	case s.wakeCh <- struct{}{}:
	default:
	}
}

func (s *Service) pollLoop() {
	defer s.wg.Done()
	lastTick := time.Now()
	for {
		s.mu.Lock()
		interval := s.cfg.PollInterval()
		configured := s.configuredLocked()
		s.mu.Unlock()

		now := time.Now()
		if gap := now.Sub(lastTick); gap > interval*5+2*time.Second {
			// The process cannot run while the Mac sleeps; a large gap
			// means we likely just resumed. Notifications delivered
			// while asleep are still in the database and are picked up
			// now.
			s.lastSleepGap.Store(now.UnixNano())
			s.logger.Info("resuming after long poll gap; the Mac may have been asleep",
				"gap", gap.Round(time.Second).String())
		}
		lastTick = now

		if configured {
			if err := s.pollOnce(s.ctx); err != nil {
				if s.ctx.Err() == nil {
					s.logger.Warn("poll failed", "error", err)
				}
			}
		}

		timer := time.NewTimer(interval)
		select {
		case <-s.ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// pollOnce reads new records from the source, deduplicates them and enqueues
// pending deliveries.
func (s *Service) pollOnce(ctx context.Context) error {
	pollCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	wm, err := s.loadWatermark(pollCtx)
	if err != nil {
		s.logger.Warn("could not load polling watermark", "error", err)
	}
	records, newWM, err := s.src.Poll(pollCtx, wm, 200)
	if err != nil {
		s.srcAccessible.Store(false)
		s.srcError.Store(err.Error())
		s.lastPollErr.Store(err.Error())
		return err
	}
	s.srcAccessible.Store(true)
	s.srcError.Store("")
	s.lastPollErr.Store("")
	s.lastPollAt.Store(time.Now().UnixNano())

	if len(records) == 0 {
		return nil
	}

	s.mu.Lock()
	filter := s.filter
	s.mu.Unlock()

	var skipped int
	for _, rec := range records {
		n, err := parser.Parse(rec.Payload)
		if err != nil {
			s.logger.Debug("skipping unparsable record", "rec_id", rec.RecID, "error", err)
			continue
		}
		n.RecID = rec.RecID
		n.UUID = rec.UUIDHex
		if n.AppID == "" {
			n.AppID = rec.AppID
		}
		if n.DeliveredAt.IsZero() {
			n.DeliveredAt = rec.DeliveredAt
		}
		s.recordsSeen.Add(1)

		if filter.Excluded(n.AppID) {
			skipped++
			continue
		}
		key := dedup.Key(n)
		inserted, err := s.q.Enqueue(pollCtx, n, key)
		if err != nil {
			s.logger.Warn("enqueue failed", "app", n.AppID, "error", err)
			continue
		}
		if inserted {
			s.recordsQueued.Add(1)
			s.logger.Debug("queued notification", "app", n.AppID, "key_prefix", key[:min(12, len(key))])
		}
	}
	if skipped > 0 {
		s.logger.Debug("notifications excluded by filters", "count", skipped)
	}
	if err := s.saveWatermark(pollCtx, newWM); err != nil {
		s.logger.Warn("could not save polling watermark", "error", err)
	}
	s.wake()
	return nil
}

func (s *Service) loadWatermark(ctx context.Context) (source.Watermark, error) {
	raw, err := s.db.MetaGet(ctx, watermarkMetaKey)
	if err != nil || raw == "" {
		return source.Watermark{}, err
	}
	var wm struct {
		D int64 `json:"d"`
		R int64 `json:"r"`
	}
	if err := json.Unmarshal([]byte(raw), &wm); err != nil {
		return source.Watermark{}, fmt.Errorf("parse watermark: %w", err)
	}
	return source.Watermark{DeliveredDateSecs: wm.D, RecID: wm.R, Valid: true}, nil
}

func (s *Service) saveWatermark(ctx context.Context, wm source.Watermark) error {
	if !wm.Valid {
		return nil
	}
	raw, err := json.Marshal(struct {
		D int64 `json:"d"`
		R int64 `json:"r"`
	}{wm.DeliveredDateSecs, wm.RecID})
	if err != nil {
		return err
	}
	return s.db.MetaSet(ctx, watermarkMetaKey, string(raw))
}

// workerLoop delivers queued notifications with retries and backoff.
func (s *Service) workerLoop() {
	defer s.wg.Done()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
		case <-s.wakeCh:
		}
		s.mu.Lock()
		cfg, token := s.cfg, s.token
		s.mu.Unlock()
		if token == "" || cfg.ChatID == "" || !cfg.ForwardingEnabled || s.paused.Load() {
			continue
		}
		if err := s.deliverBatch(s.ctx); err != nil && s.ctx.Err() == nil {
			s.logger.Warn("delivery batch failed", "error", err)
		}
	}
}

func (s *Service) deliverBatch(ctx context.Context) error {
	due, err := s.q.ClaimDue(ctx, time.Now(), 10)
	if err != nil {
		return err
	}
	if len(due) == 0 {
		return nil
	}
	c := s.client()
	for _, d := range due {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.deliverOne(ctx, c, d)
	}
	return nil
}

func (s *Service) deliverOne(ctx context.Context, c *telegram.Client, d queue.Delivery) {
	sendCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()

	n := notifications.Notification{
		AppID:       d.AppID,
		Title:       d.Title,
		Subtitle:    d.Subtitle,
		Body:        d.Body,
		DeliveredAt: d.DeliveredAt,
	}
	msgID, err := c.SendText(sendCtx, telegram.Format(n))
	now := time.Now()
	if err == nil {
		if err := s.q.MarkSent(ctx, d.ID, msgID, now); err != nil {
			s.logger.Error("failed to record sent delivery", "id", d.ID, "error", err)
		} else {
			s.logger.Info("notification forwarded", "app", d.AppID, "message_id", msgID)
		}
		return
	}

	if s.ctx.Err() != nil {
		// Shutting down: leave the record pending for restart recovery.
		return
	}
	attempt := d.Attempts + 1
	if !telegram.Retryable(err) {
		s.logger.Warn("delivery failed permanently", "app", d.AppID, "attempt", attempt, "error", err)
		if mErr := s.q.MarkFailed(ctx, d.ID, err.Error()); mErr != nil {
			s.logger.Error("failed to record delivery failure", "id", d.ID, "error", mErr)
		}
		return
	}
	if attempt >= s.q.MaxAttempts() {
		s.logger.Warn("delivery failed: retry budget exhausted", "app", d.AppID, "attempt", attempt, "error", err)
		if mErr := s.q.MarkFailed(ctx, d.ID, fmt.Sprintf("giving up after %d attempts: %v", attempt, err)); mErr != nil {
			s.logger.Error("failed to record delivery failure", "id", d.ID, "error", mErr)
		}
		return
	}
	delay := s.q.Backoff(attempt)
	var apiErr *telegram.APIError
	if errors.As(err, &apiErr) && apiErr.RetryAfter > delay {
		delay = apiErr.RetryAfter
	}
	next := now.Add(delay)
	s.logger.Info("delivery will be retried", "app", d.AppID, "attempt", attempt,
		"delay", delay.Round(time.Second).String(), "error", err)
	if mErr := s.q.MarkRetry(ctx, d.ID, next, err.Error()); mErr != nil {
		s.logger.Error("failed to schedule retry", "id", d.ID, "error", mErr)
	}
}

// retentionLoop periodically prunes delivery records past the configured
// retention window so notification content is not kept indefinitely.
func (s *Service) retentionLoop() {
	defer s.wg.Done()
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
		}
		s.mu.Lock()
		days := s.cfg.HistoryRetentionDays
		s.mu.Unlock()
		cutoff := time.Now().AddDate(0, 0, -days)
		if n, err := s.db.PurgeOlderThan(s.ctx, cutoff); err != nil {
			s.logger.Warn("history retention purge failed", "error", err)
		} else if n > 0 {
			s.logger.Info("pruned old delivery records", "records", n, "retention_days", days)
		}
	}
}

// maskChatID shows only the last characters of a chat ID for display.
func maskChatID(id string) string {
	if id == "" {
		return ""
	}
	if len(id) <= 4 {
		return "…" + id[len(id)-1:]
	}
	return "…" + id[len(id)-4:]
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
