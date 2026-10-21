package app

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/spdeepak/notibridge/internal/config"
)

// updateStub serves a GitHub releases payload plus the release assets.
type updateStub struct {
	version string
	zip     []byte
	sums    []byte
	server  *httptest.Server
}

func newUpdateStub(t *testing.T, version string) *updateStub {
	t.Helper()

	// A minimal but real application bundle.
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range map[string]string{
		"NotiBridge.app/Contents/MacOS/NotiBridge": "binary",
		"NotiBridge.app/Contents/Info.plist":       "<plist/>",
	} {
		f, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip create %s: %v", name, err)
		}
		if _, err := f.Write([]byte(body)); err != nil {
			t.Fatalf("zip write %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	archive := buf.Bytes()

	sum := sha256.Sum256(archive)
	sums := []byte(fmt.Sprintf("%s  NotiBridge_%s_%s.zip\n", hex.EncodeToString(sum[:]), version, runtime.GOARCH))

	st := &updateStub{version: version, zip: archive, sums: sums}
	mux := http.NewServeMux()
	st.server = httptest.NewServer(mux)
	t.Cleanup(st.server.Close)

	mux.HandleFunc("/repos/o/r/releases", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{{
			"tag_name": "v" + version,
			"body":     "release notes",
			"assets": []map[string]any{
				{"name": "NotiBridge_" + version + "_" + runtime.GOARCH + ".zip", "browser_download_url": st.server.URL + "/app.zip"},
				{"name": "SHA256SUMS", "browser_download_url": st.server.URL + "/SHA256SUMS"},
			},
		}})
	})
	mux.HandleFunc("/app.zip", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(st.zip)
	})
	mux.HandleFunc("/SHA256SUMS", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(st.sums)
	})
	return st
}

func newUpdateService(t *testing.T, stub *updateStub) *Service {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("NOTIBRIDGE_HOME", dir)
	svc, err := New(Options{
		Source:           &fakeSource{},
		DBPath:           filepath.Join(dir, "state.db"),
		Secrets:          config.NewFileStore(dir),
		UpdateOwner:      "o",
		UpdateRepo:       "r",
		UpdateAPIBaseURL: stub.server.URL,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return svc
}

func TestCheckForUpdates(t *testing.T) {
	stub := newUpdateStub(t, "9.9.9")
	svc := newUpdateService(t, stub)

	st, err := svc.CheckForUpdates(context.Background())
	if err != nil {
		t.Fatalf("CheckForUpdates: %v", err)
	}
	if !st.Available() {
		t.Fatal("the update should be reported as available")
	}
	if st.LatestVersion != "9.9.9" {
		t.Errorf("LatestVersion = %q, want 9.9.9", st.LatestVersion)
	}
	if st.Notes != "release notes" {
		t.Errorf("Notes = %q, want the release body", st.Notes)
	}
	if st.CheckedAt.IsZero() {
		t.Error("CheckedAt should be set after a check")
	}
	if st.Staged() {
		t.Error("a check must not stage an update")
	}
}

func TestDownloadUpdateStagesButDoesNotInstall(t *testing.T) {
	stub := newUpdateStub(t, "9.9.9")
	svc := newUpdateService(t, stub)

	st, err := svc.DownloadUpdate(context.Background())
	if err != nil {
		t.Fatalf("DownloadUpdate: %v", err)
	}
	if st.StagedVersion != "9.9.9" {
		t.Errorf("StagedVersion = %q, want 9.9.9", st.StagedVersion)
	}
	if !st.Staged() {
		t.Fatal("the verified update should be staged")
	}
	if _, err := os.Stat(filepath.Join(st.StagedPath, "Contents", "Info.plist")); err != nil {
		t.Errorf("the staged bundle is incomplete: %v", err)
	}
}

func TestInstallUpdateWithoutDownload(t *testing.T) {
	stub := newUpdateStub(t, "9.9.9")
	svc := newUpdateService(t, stub)

	_, err := svc.InstallUpdate(context.Background())
	if err == nil {
		t.Fatal("installing without a staged update must fail")
	}
	if !strings.Contains(err.Error(), "downloaded") {
		t.Errorf("error = %q, want it to say that nothing was downloaded", err)
	}
	if st := svc.UpdateStatus(); st.Staged() {
		t.Error("a failed install must not leave an update staged")
	}
}

// TestInstallUpdateFallsBackToManualSteps covers the case where the bundle
// cannot be replaced automatically: the user gets instructions instead of a
// silent failure.
func TestInstallUpdateFallsBackToManualSteps(t *testing.T) {
	stub := newUpdateStub(t, "9.9.9")
	svc := newUpdateService(t, stub)

	if _, err := svc.DownloadUpdate(context.Background()); err != nil {
		t.Fatalf("DownloadUpdate: %v", err)
	}
	// The test binary does not run from an application bundle, so the
	// automatic install is impossible.
	_, err := svc.InstallUpdate(context.Background())
	if err == nil {
		t.Fatal("installing outside an application bundle must fail")
	}
	if !strings.Contains(err.Error(), "replace the installed NotiBridge.app") {
		t.Errorf("error = %q, want the manual installation steps", err)
	}
}

func TestCheckForUpdatesReportsFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	dir := t.TempDir()
	t.Setenv("NOTIBRIDGE_HOME", dir)
	svc, err := New(Options{
		Source:           &fakeSource{},
		DBPath:           filepath.Join(dir, "state.db"),
		Secrets:          config.NewFileStore(dir),
		UpdateOwner:      "o",
		UpdateRepo:       "r",
		UpdateAPIBaseURL: srv.URL,
		UpdateHTTPClient: &http.Client{Timeout: 5 * time.Second},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := svc.CheckForUpdates(context.Background()); err == nil {
		t.Fatal("a failing release request must be reported")
	}
	st := svc.UpdateStatus()
	if st.Error == "" {
		t.Error("the failure should be visible in the update state")
	}
	if st.Checking {
		t.Error("the check must not stay marked as running")
	}
}
