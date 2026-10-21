package webui

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/spdeepak/notibridge/internal/app"
	"github.com/spdeepak/notibridge/internal/config"
	"github.com/spdeepak/notibridge/internal/notifications/source"
)

// stubSource satisfies source.Source; the web UI never polls.
type stubSource struct{}

func (stubSource) Poll(context.Context, source.Watermark, int) ([]source.Record, source.Watermark, error) {
	return nil, source.Watermark{}, nil
}
func (stubSource) Diagnose(context.Context) source.Diagnostics { return source.Diagnostics{} }
func (stubSource) Close() error                                { return nil }

// newTestServer starts a web UI against a service whose update checks go to
// the given GitHub API stub.
func newTestServer(t *testing.T, api *httptest.Server) *Server {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("NOTIBRIDGE_HOME", dir)
	svc, err := app.New(app.Options{
		Logger:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		Source:           stubSource{},
		DBPath:           filepath.Join(dir, "state.db"),
		Secrets:          config.NewFileStore(dir),
		UpdateOwner:      "spdeepak",
		UpdateRepo:       "Notibridge",
		UpdateAPIBaseURL: api.URL,
	})
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	s, err := New(svc, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("webui.New: %v", err)
	}
	s.Start()
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// baseURL is the server root without the query string, so paths can be
// appended to it.
func (s *Server) baseURL() string { return "http://" + s.ln.Addr().String() }

// noRedirect returns a client that does not follow redirects, so the handler's
// own status code can be asserted.
func noRedirect() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
}

// tokenOf extracts the session token from the settings URL.
func tokenOf(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	tok := u.Query().Get("t")
	if tok == "" {
		t.Fatalf("no session token in %q", raw)
	}
	return tok
}

// releasesStub serves a GitHub releases payload with one newer release for
// the architecture the test runs on.
func releasesStub(t *testing.T, tag string) *httptest.Server {
	arch := runtime.GOARCH
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/releases") {
			http.NotFound(w, r)
			return
		}
		body := []map[string]any{{
			"tag_name":   tag,
			"body":       "notes",
			"draft":      false,
			"prerelease": false,
			"assets": []map[string]any{
				{"name": "NotiBridge_" + strings.TrimPrefix(tag, "v") + "_" + arch + ".zip", "browser_download_url": "http://example.invalid/app.zip", "size": 10},
				{"name": "SHA256SUMS", "browser_download_url": "http://example.invalid/SHA256SUMS", "size": 10},
			},
		}}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestUpdateCheckFormPost posts to /update/check the way the settings page
// form does: token in a form field, no query string.
func TestUpdateCheckFormPost(t *testing.T) {
	api := releasesStub(t, "v99.0.0")
	s := newTestServer(t, api)
	tok := tokenOf(t, s.URL())

	res, err := noRedirect().PostForm(s.baseURL()+"/update/check", url.Values{"t": {tok}})
	if err != nil {
		t.Fatalf("post form: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", res.StatusCode, http.StatusSeeOther)
	}
	loc := res.Header.Get("Location")
	if !strings.Contains(loc, "99.0.0") {
		t.Fatalf("Location = %q, want it to mention the available version", loc)
	}
	if st := s.svc.UpdateStatus(); st.LatestVersion != "99.0.0" {
		t.Fatalf("LatestVersion = %q, want 99.0.0", st.LatestVersion)
	}
}

// TestUpdateCheckQueryToken posts with the token in the query string.
func TestUpdateCheckQueryToken(t *testing.T) {
	api := releasesStub(t, "v99.0.0")
	s := newTestServer(t, api)
	tok := tokenOf(t, s.URL())

	res, err := noRedirect().Post(s.baseURL()+"/update/check?t="+url.QueryEscape(tok), "application/x-www-form-urlencoded", nil)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", res.StatusCode, http.StatusSeeOther)
	}
}

// TestUpdateEndpointsRequireToken rejects requests without a session token.
func TestUpdateEndpointsRequireToken(t *testing.T) {
	api := releasesStub(t, "v99.0.0")
	s := newTestServer(t, api)

	for _, path := range []string{"/update/check", "/update/download", "/update/install"} {
		res, err := noRedirect().PostForm(s.baseURL()+path, url.Values{})
		if err != nil {
			t.Fatalf("post %s: %v", path, err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s without token: status = %d, want %d", path, res.StatusCode, http.StatusUnauthorized)
		}
	}
}

// TestSettingsScreenLockToggle checks that the settings page offers the
// screen lock checkbox and that saving it persists the choice.
func TestSettingsScreenLockToggle(t *testing.T) {
	api := releasesStub(t, "v99.0.0")
	s := newTestServer(t, api)
	tok := tokenOf(t, s.URL())

	res, err := noRedirect().Get(s.baseURL() + "/?t=" + url.QueryEscape(tok))
	if err != nil {
		t.Fatalf("get page: %v", err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if !strings.Contains(string(body), `name="only_when_locked"`) {
		t.Error("the settings page does not offer the screen lock setting")
	}

	// An unchecked checkbox is not submitted, so the restriction is turned off.
	form := url.Values{"t": {tok}, "chat_id": {"42"}, "poll_seconds": {"2"},
		"retention_days": {"14"}, "max_attempts": {"8"}}
	res, err = noRedirect().PostForm(s.baseURL()+"/settings", form)
	if err != nil {
		t.Fatalf("post form: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", res.StatusCode, http.StatusSeeOther)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if cfg.OnlyWhenScreenLocked {
		t.Error("the checkbox was not submitted, so the restriction must be off")
	}
}

// TestUpdateInstallWithoutDownload reports that nothing is staged instead of
// pretending an update was installed.
func TestUpdateInstallWithoutDownload(t *testing.T) {
	api := releasesStub(t, "v99.0.0")
	s := newTestServer(t, api)
	tok := tokenOf(t, s.URL())

	res, err := noRedirect().PostForm(s.baseURL()+"/update/install", url.Values{"t": {tok}})
	if err != nil {
		t.Fatalf("post form: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", res.StatusCode, http.StatusSeeOther)
	}
	if loc := res.Header.Get("Location"); !strings.Contains(loc, "kind=err") {
		t.Fatalf("Location = %q, want an error message", loc)
	}
	if st := s.svc.UpdateStatus(); st.Staged() {
		t.Fatal("nothing was downloaded, but the state reports a staged update")
	}
}
