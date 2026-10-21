// Package webui serves a small loopback-only web interface used by the menu
// bar for configuration, connection testing, pause/resume, activity history
// and clearing local data. It binds to 127.0.0.1 on a random port and
// requires a per-session random token, so other local processes and drive-by
// browser requests cannot reconfigure the app.
package webui

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/spdeepak/notibridge/internal/app"
	"github.com/spdeepak/notibridge/internal/config"
)

// Server is the loopback settings/activity server.
type Server struct {
	svc    *app.Service
	logger *slog.Logger
	token  string
	ln     net.Listener
	mux    *http.ServeMux
}

// New binds the server to 127.0.0.1 on an ephemeral port.
func New(svc *app.Service, logger *slog.Logger) (*Server, error) {
	if logger == nil {
		logger = slog.Default()
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("bind loopback listener: %w", err)
	}
	tok := make([]byte, 16)
	if _, err := rand.Read(tok); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("generate session token: %w", err)
	}
	s := &Server{svc: svc, logger: logger, token: hex.EncodeToString(tok), ln: ln, mux: http.NewServeMux()}
	s.routes()
	return s, nil
}

// Start serves requests until Close is called.
func (s *Server) Start() {
	go func() {
		err := http.Serve(s.ln, s.mux)
		if err != nil && !strings.Contains(err.Error(), "closed") {
			s.logger.Warn("web UI stopped", "error", err)
		}
	}()
}

// Close stops the server.
func (s *Server) Close() error { return s.ln.Close() }

// URL returns the tokenized URL for the settings page.
func (s *Server) URL() string {
	return fmt.Sprintf("http://%s/?t=%s", s.ln.Addr().String(), s.token)
}

// ActivityURL returns the URL anchored on the activity section.
func (s *Server) ActivityURL() string { return s.URL() + "#activity" }

func (s *Server) authorized(r *http.Request) bool {
	t := r.URL.Query().Get("t")
	if t == "" {
		t = r.FormValue("t")
	}
	return t != "" && t == s.token
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /", s.handlePage)
	s.mux.HandleFunc("GET /api/status", s.handleStatusAPI)
	s.mux.HandleFunc("POST /settings", s.handleSaveSettings)
	s.mux.HandleFunc("POST /test-telegram", s.handleTestTelegram)
	s.mux.HandleFunc("POST /test-message", s.handleTestMessage)
	s.mux.HandleFunc("POST /toggle-pause", s.handleTogglePause)
	s.mux.HandleFunc("POST /toggle-forwarding", s.handleToggleForwarding)
	s.mux.HandleFunc("POST /clear", s.handleClear)
}

func (s *Server) redirect(w http.ResponseWriter, r *http.Request, msg string, isErr bool) {
	u := s.URL()
	if msg != "" {
		sep := "?"
		if strings.Contains(u, "?") {
			sep = "&"
		}
		kind := "ok"
		if isErr {
			kind = "err"
		}
		u += sep + "msg=" + template.URLQueryEscaper(msg) + "&kind=" + kind
	}
	http.Redirect(w, r, u, http.StatusSeeOther)
}

func (s *Server) handleStatusAPI(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	st := s.svc.Status()
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"version":%q,"configured":%t,"forwarding_enabled":%t,"paused":%t,"pending":%d,"sent":%d,"failed":%d,"source_accessible":%t}`,
		st.Version, st.Configured, st.ForwardingEnabled, st.Paused,
		st.Counts.Pending, st.Counts.Sent, st.Counts.Failed, st.SourceAccessible)
}

type pageData struct {
	Token   string
	Message string
	Err     bool
	Status  app.Status
	Recent  []activityRow
	Form    settingsForm
	Diag    *appDiag
}

type appDiag struct {
	Path       string
	Accessible bool
	SchemaOK   bool
	RowCount   int64
	Oldest     string
	Newest     string
	Error      string
	Hints      []string
}

type activityRow struct {
	Time     string
	App      string
	Title    string
	Status   string
	Attempts int
	Error    string
}

type settingsForm struct {
	ChatID        string
	PollSeconds   string
	RetentionDays string
	MaxAttempts   string
	Exclusions    string
	Forwarding    bool
	HasToken      bool
}

func (s *Server) handlePage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if !s.authorized(r) {
		http.Error(w, "NotiBridge: missing or invalid session token. Open the settings page from the menu bar icon.", http.StatusUnauthorized)
		return
	}
	st := s.svc.Status()
	data := pageData{
		Token:   s.token,
		Message: r.URL.Query().Get("msg"),
		Err:     r.URL.Query().Get("kind") == "err",
		Status:  st,
		Form: settingsForm{
			ChatID:        "",
			PollSeconds:   strconv.Itoa(st.PollIntervalSecs),
			RetentionDays: "14",
			MaxAttempts:   "8",
			Forwarding:    st.ForwardingEnabled,
			HasToken:      strings.Contains(r.URL.RawQuery, "t="), // token never round-trips into the form
		},
	}
	if cfg, err := config.Load(); err == nil {
		data.Form.ChatID = cfg.ChatID
		data.Form.RetentionDays = strconv.Itoa(cfg.HistoryRetentionDays)
		data.Form.MaxAttempts = strconv.Itoa(cfg.MaxAttempts)
		data.Form.Exclusions = strings.Join(cfg.ExcludedApps, "\n")
		data.Form.Forwarding = cfg.ForwardingEnabled
	}
	if rows, err := s.svc.RecentDeliveries(r.Context(), 25); err == nil {
		for _, d := range rows {
			row := activityRow{
				Time:     d.CreatedAt.Local().Format("2006-01-02 15:04:05"),
				App:      d.AppID,
				Title:    d.Title,
				Status:   d.Status,
				Attempts: d.Attempts,
				Error:    d.LastError,
			}
			if row.Title == "" {
				row.Title = "(no title)"
			}
			data.Recent = append(data.Recent, row)
		}
	}
	if r.URL.Query().Get("diag") == "1" {
		d := s.svc.Diagnose(r.Context())
		ad := &appDiag{
			Path:       d.Path,
			Accessible: d.Accessible,
			SchemaOK:   d.SchemaOK,
			RowCount:   d.RowCount,
			Error:      d.Error,
			Hints:      d.Hints,
		}
		if !d.Oldest.IsZero() {
			ad.Oldest = d.Oldest.Local().Format("2006-01-02 15:04:05")
		}
		if !d.Newest.IsZero() {
			ad.Newest = d.Newest.Local().Format("2006-01-02 15:04:05")
		}
		data.Diag = ad
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := pageTmpl.Execute(w, data); err != nil {
		s.logger.Warn("render web UI page", "error", err)
	}
}

func (s *Server) handleSaveSettings(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if err := r.ParseForm(); err != nil {
		s.redirect(w, r, "could not parse form: "+err.Error(), true)
		return
	}
	upd := app.ConfigUpdate{
		ChatID:               strings.TrimSpace(r.FormValue("chat_id")),
		PollIntervalSeconds:  atoiDefault(r.FormValue("poll_seconds"), 2),
		ForwardingEnabled:    r.FormValue("forwarding") == "on",
		ExcludedApps:         splitLines(r.FormValue("exclusions")),
		HistoryRetentionDays: atoiDefault(r.FormValue("retention_days"), 14),
		MaxAttempts:          atoiDefault(r.FormValue("max_attempts"), 8),
	}
	if tok := strings.TrimSpace(r.FormValue("bot_token")); tok != "" {
		if strings.Contains(tok, "…") || tok == "(unchanged)" {
			// Defensive: never treat a masked placeholder as a token.
			upd.BotToken = nil
		} else {
			upd.BotToken = &tok
		}
	}
	if err := s.svc.ApplyConfig(r.Context(), upd); err != nil {
		s.redirect(w, r, err.Error(), true)
		return
	}
	s.redirect(w, r, "Settings saved.", false)
}

func (s *Server) handleTestTelegram(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	name, err := s.svc.TestConnection(ctx)
	if err != nil {
		s.redirect(w, r, "Connection test failed: "+err.Error(), true)
		return
	}
	s.redirect(w, r, "Connected to bot @"+name+".", false)
}

func (s *Server) handleTestMessage(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := s.svc.SendTestMessage(ctx); err != nil {
		s.redirect(w, r, "Test message failed: "+err.Error(), true)
		return
	}
	s.redirect(w, r, "Test message sent — check your Telegram chat.", false)
}

func (s *Server) handleTogglePause(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if s.svc.Status().Paused {
		s.svc.Resume()
		s.redirect(w, r, "Forwarding resumed.", false)
	} else {
		s.svc.Pause()
		s.redirect(w, r, "Forwarding paused. Queued notifications will be delivered on resume.", false)
	}
}

func (s *Server) handleToggleForwarding(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	st := s.svc.Status()
	if err := s.svc.SetForwarding(r.Context(), !st.ForwardingEnabled); err != nil {
		s.redirect(w, r, err.Error(), true)
		return
	}
	if st.ForwardingEnabled {
		s.redirect(w, r, "Forwarding disabled.", false)
	} else {
		s.redirect(w, r, "Forwarding enabled.", false)
	}
}

func (s *Server) handleClear(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	n, err := s.svc.ClearHistory(r.Context())
	if err != nil {
		s.redirect(w, r, err.Error(), true)
		return
	}
	s.redirect(w, r, fmt.Sprintf("Cleared %d local delivery records.", n), false)
}

func atoiDefault(s string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && n > 0 {
		return n
	}
	return def
}

func splitLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(strings.Split(line, "#")[0])
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

var pageTmpl = template.Must(template.New("page").Parse(pageHTML))

const pageHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>NotiBridge</title>
<meta name="viewport" content="width=device-width, initial-scale=1">
<style>
  :root { color-scheme: light dark; }
  body { font-family: -apple-system, system-ui, sans-serif; margin: 2rem auto; max-width: 46rem; padding: 0 1rem; line-height: 1.45; }
  h1 { font-size: 1.4rem; } h2 { font-size: 1.05rem; margin-top: 2rem; }
  .badge { display:inline-block; padding: .15rem .5rem; border-radius: .4rem; font-size: .8rem; margin-right: .35rem; }
  .ok { background: color-mix(in srgb, green 20%, transparent); }
  .warn { background: color-mix(in srgb, orange 25%, transparent); }
  .bad { background: color-mix(in srgb, red 20%, transparent); }
  .msg { padding: .6rem .8rem; border-radius: .5rem; margin: 1rem 0; }
  .msg.ok { background: color-mix(in srgb, green 15%, transparent); }
  .msg.err { background: color-mix(in srgb, red 15%, transparent); }
  form { margin: .5rem 0; }
  label { display:block; margin-top: .7rem; font-size: .85rem; font-weight: 600; }
  input[type=text], input[type=password], textarea, input[type=number] { width: 100%; padding: .4rem; border: 1px solid #8884; border-radius: .4rem; font-size: .9rem; box-sizing: border-box; }
  textarea { min-height: 4.5rem; font-family: ui-monospace, monospace; }
  button { margin-top: .9rem; margin-right: .5rem; padding: .45rem .9rem; border-radius: .45rem; border: 1px solid #8886; background: #8881; cursor: pointer; font-size: .9rem; }
  table { width: 100%; border-collapse: collapse; margin-top: .5rem; font-size: .82rem; }
  th, td { text-align: left; padding: .3rem .4rem; border-bottom: 1px solid #8883; vertical-align: top; word-break: break-word; }
  .muted { color: #888; font-size: .8rem; }
  code { background: #8882; padding: .05rem .3rem; border-radius: .3rem; }
</style>
</head>
<body>
<h1>NotiBridge <span class="muted">v{{.Status.Version}}</span></h1>

{{if .Message}}<div class="msg {{if .Err}}err{{else}}ok{{end}}">{{.Message}}</div>{{end}}

<p>
  {{if .Status.Configured}}<span class="badge ok">Configured</span>{{else}}<span class="badge bad">Not configured</span>{{end}}
  {{if .Status.Paused}}<span class="badge warn">Paused</span>{{else}}<span class="badge ok">Not paused</span>{{end}}
  {{if .Status.ForwardingEnabled}}<span class="badge ok">Forwarding on</span>{{else}}<span class="badge warn">Forwarding off</span>{{end}}
  {{if .Status.SourceAccessible}}<span class="badge ok">Notification DB readable</span>{{else}}<span class="badge bad">Notification DB inaccessible</span>{{end}}
</p>
<p class="muted">
  Queue: {{.Status.Counts.Pending}} pending · {{.Status.Counts.Sent}} sent · {{.Status.Counts.Failed}} failed
  &nbsp;|&nbsp; Chat: {{if .Status.ChatIDMasked}}{{.Status.ChatIDMasked}}{{else}}—{{end}}
  &nbsp;|&nbsp; Secrets: {{.Status.SecretStore}}
  {{if not .Status.SourceAccessible}}<br>Notification access error: {{.Status.SourceError}}{{end}}
</p>

<form method="post" action="/toggle-pause"><input type="hidden" name="t" value="{{.Token}}">
  {{if .Status.Paused}}<button type="submit">Resume forwarding</button>{{else}}<button type="submit">Pause forwarding</button>{{end}}
</form>

<h2>Settings</h2>
<form method="post" action="/settings"><input type="hidden" name="t" value="{{.Token}}">
  <label for="bot_token">Bot token (from @BotFather){{if .Status.Configured}} — leave blank to keep current token{{end}}</label>
  <input type="password" id="bot_token" name="bot_token" placeholder="123456:ABC-DEF…" autocomplete="off">
  <label for="chat_id">Destination chat ID (64-bit number or @username)</label>
  <input type="text" id="chat_id" name="chat_id" value="{{.Form.ChatID}}" placeholder="-1001234567890">
  <label for="poll_seconds">Poll interval in seconds (1–300)</label>
  <input type="number" id="poll_seconds" name="poll_seconds" value="{{.Form.PollSeconds}}" min="1" max="300">
  <label for="retention_days">Local history retention in days (1–3650)</label>
  <input type="number" id="retention_days" name="retention_days" value="{{.Form.RetentionDays}}" min="1" max="3650">
  <label for="max_attempts">Delivery attempts before giving up (1–100)</label>
  <input type="number" id="max_attempts" name="max_attempts" value="{{.Form.MaxAttempts}}" min="1" max="100">
  <label for="exclusions">Excluded apps (bundle IDs, one per line; <code>com.example.*</code> wildcards allowed)</label>
  <textarea id="exclusions" name="exclusions" placeholder="com.apple.Safari">{{.Form.Exclusions}}</textarea>
  <label><input type="checkbox" name="forwarding" {{if .Form.Forwarding}}checked{{end}}> Forward notifications to Telegram</label>
  <button type="submit">Save settings</button>
</form>

<form method="post" action="/test-telegram"><input type="hidden" name="t" value="{{.Token}}">
  <button type="submit">Test connection (getMe)</button>
</form>
<form method="post" action="/test-message"><input type="hidden" name="t" value="{{.Token}}">
  <button type="submit">Send test message</button>
</form>
<form method="post" action="/toggle-forwarding"><input type="hidden" name="t" value="{{.Token}}">
  <button type="submit">Toggle forwarding</button>
</form>

<h2 id="activity">Recent activity</h2>
{{if .Recent}}
<table>
  <tr><th>Time</th><th>App</th><th>Title</th><th>Status</th><th>Attempts</th><th>Error</th></tr>
  {{range .Recent}}
  <tr>
    <td>{{.Time}}</td><td>{{.App}}</td><td>{{.Title}}</td>
    <td>{{.Status}}</td><td>{{.Attempts}}</td><td>{{.Error}}</td>
  </tr>
  {{end}}
</table>
{{else}}
<p class="muted">No deliveries recorded yet.</p>
{{end}}

<form method="post" action="/clear"><input type="hidden" name="t" value="{{.Token}}">
  <button type="submit" onclick="return confirm('Delete all locally stored notification content and delivery history?')">Clear local history</button>
</form>

<h2>Notification database access</h2>
<p class="muted">NotiBridge reads Apple's undocumented Notification Center database read-only. Access may require Full Disk Access and is not guaranteed on every macOS version.</p>
<form method="get" action="/"><input type="hidden" name="t" value="{{.Token}}"><input type="hidden" name="diag" value="1">
  <button type="submit">Run diagnostics</button>
</form>
{{if .Diag}}
<p>
  Path: <code>{{.Diag.Path}}</code><br>
  Accessible: {{.Diag.Accessible}} · Schema understood: {{.Diag.SchemaOK}} · Records: {{.Diag.RowCount}}
  {{if .Diag.Oldest}}<br>Oldest record: {{.Diag.Oldest}} · Newest record: {{.Diag.Newest}}{{end}}
</p>
{{if .Diag.Error}}<p class="msg err">{{.Diag.Error}}</p>{{end}}
{{range .Diag.Hints}}<p class="muted">💡 {{.}}</p>{{end}}
{{end}}
</body>
</html>
`
