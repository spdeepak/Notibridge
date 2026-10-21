package update

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeGitHub serves the releases API and the release assets.
type fakeGitHub struct {
	t         *testing.T
	releases  []ghRelease
	files     map[string][]byte // path -> body
	server    *httptest.Server
	apiStatus int
	apiBody   string
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{t: t, files: map[string][]byte{}}
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeGitHub) handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/repos/spdeepak/Notibridge/releases" {
		if f.apiStatus != 0 {
			w.WriteHeader(f.apiStatus)
			w.Write([]byte(f.apiBody))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(f.releases)
		return
	}
	body, ok := f.files[r.URL.Path]
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Write(body)
}

// addRelease publishes a release with the given assets. A SHA256SUMS file
// covering those assets is added automatically, like the real release
// workflow does.
func (f *fakeGitHub) addRelease(tag string, draft, prerelease bool, assets map[string][]byte) {
	rel := ghRelease{TagName: tag, Body: "notes for " + tag, Draft: draft, Prerelease: prerelease}
	var sums strings.Builder
	for name, body := range assets {
		path := "/dl/" + tag + "/" + name
		f.files[path] = body
		rel.Assets = append(rel.Assets, ghAsset{
			Name: name,
			URL:  f.server.URL + path,
			Size: int64(len(body)),
		})
		fmt.Fprintf(&sums, "%s  %s\n", sha256Hex(body), name)
	}
	sumsPath := "/dl/" + tag + "/" + ChecksumAssetName
	f.files[sumsPath] = []byte(sums.String())
	rel.Assets = append(rel.Assets, ghAsset{
		Name: ChecksumAssetName,
		URL:  f.server.URL + sumsPath,
		Size: int64(len(sums.String())),
	})
	f.releases = append(f.releases, rel)
}

// appZip builds a release archive containing a NotiBridge.app bundle.
// It mimics the archives the release workflow produces with ditto, which also
// contain a __MACOSX resource-fork sidecar.
func appZip(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	files := map[string]string{
		"NotiBridge.app/Contents/Info.plist":                "<plist></plist>",
		"NotiBridge.app/Contents/MacOS/NotiBridge":          "binary",
		"NotiBridge.app/Contents/Resources/NotiBridge.icns": "icns",
		"__MACOSX/NotiBridge.app/Contents/Info.plist":       "sidecar",
	}
	for name, body := range files {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate})
		if err != nil {
			t.Fatalf("create zip entry %s: %v", name, err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatalf("write zip entry %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return buf.Bytes()
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.0.0", "1.0.0", 0},
		{"v1.0.0", "1.0.0", 0},
		{"1.0.1", "1.0.0", 1},
		{"1.0.0", "1.0.1", -1},
		{"2.0.0", "1.9.9", 1},
		{"1.0.0", "1.0.0-beta.1", 1},
		{"1.0.0-beta.1", "1.0.0-beta.2", -1},
		{"1.0.0", "1.0", 0},
		{"1.2", "1.2.0", 0},
		{"not-a-version", "0.0.0", 0},
	}
	for _, c := range cases {
		if got := Compare(c.a, c.b); got != c.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestLatestSelectsNewestStableRelease(t *testing.T) {
	gh := newFakeGitHub(t)
	archive := appZip(t)
	gh.addRelease("v0.2.0", false, false, map[string][]byte{
		AssetName("0.2.0", "arm64"): archive,
	})
	gh.addRelease("v0.3.0-rc.1", false, true, map[string][]byte{
		AssetName("0.3.0-rc.1", "arm64"): archive,
	})
	gh.addRelease("v0.1.5", false, false, map[string][]byte{
		AssetName("0.1.5", "arm64"): archive,
	})

	u := New(Options{Owner: "spdeepak", Repo: "Notibridge", CurrentVersion: "0.1.0", Arch: "arm64",
		APIBaseURL: gh.server.URL})
	rel, err := u.Latest(context.Background())
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if rel == nil {
		t.Fatal("expected a release")
	}
	if rel.Version != "0.2.0" {
		t.Errorf("version = %q, want 0.2.0", rel.Version)
	}
	if rel.AssetName != AssetName("0.2.0", "arm64") {
		t.Errorf("asset = %q", rel.AssetName)
	}
}

func TestLatestReportsUpToDate(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.addRelease("v0.1.0", false, false, map[string][]byte{
		AssetName("0.1.0", "arm64"): appZip(t),
	})
	u := New(Options{Owner: "spdeepak", Repo: "Notibridge", CurrentVersion: "0.1.0", Arch: "arm64",
		APIBaseURL: gh.server.URL})
	rel, err := u.Latest(context.Background())
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if rel != nil {
		t.Errorf("expected no release, got %+v", rel)
	}
}

func TestLatestReportsMissingArchitecture(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.addRelease("v0.2.0", false, false, map[string][]byte{
		AssetName("0.2.0", "amd64"): appZip(t),
	})
	u := New(Options{Owner: "spdeepak", Repo: "Notibridge", CurrentVersion: "0.1.0", Arch: "arm64",
		APIBaseURL: gh.server.URL})
	if _, err := u.Latest(context.Background()); err == nil {
		t.Fatal("expected an error for a release without an arm64 build")
	} else if !strings.Contains(err.Error(), "arm64") {
		t.Errorf("error should name the architecture: %v", err)
	}
}

func TestLatestReportsRateLimit(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.apiStatus = http.StatusForbidden
	gh.apiBody = `{"message":"API rate limit exceeded for 203.0.113.1."}`
	u := New(Options{Owner: "spdeepak", Repo: "Notibridge", CurrentVersion: "0.1.0", Arch: "arm64",
		APIBaseURL: gh.server.URL})
	_, err := u.Latest(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "rate limit") {
		t.Errorf("error should mention the rate limit: %v", err)
	}
}

func TestLatestReportsInvalidJSON(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.apiStatus = http.StatusOK
	gh.apiBody = "{not json"
	u := New(Options{Owner: "spdeepak", Repo: "Notibridge", CurrentVersion: "0.1.0", Arch: "arm64",
		APIBaseURL: gh.server.URL})
	if _, err := u.Latest(context.Background()); err == nil {
		t.Fatal("expected an error")
	}
}

func TestDownloadVerifiesChecksum(t *testing.T) {
	gh := newFakeGitHub(t)
	archive := appZip(t)
	sums := sha256Hex(archive) + "  " + AssetName("0.2.0", "arm64") + "\n"
	gh.addRelease("v0.2.0", false, false, map[string][]byte{
		AssetName("0.2.0", "arm64"): archive,
		ChecksumAssetName:           []byte(sums),
	})
	u := New(Options{Owner: "spdeepak", Repo: "Notibridge", CurrentVersion: "0.1.0", Arch: "arm64",
		APIBaseURL: gh.server.URL})
	rel, err := u.Latest(context.Background())
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	staged, err := u.Download(context.Background(), rel)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	defer os.RemoveAll(filepath.Dir(staged))
	if _, err := os.Stat(filepath.Join(staged, "Contents", "MacOS", "NotiBridge")); err != nil {
		t.Errorf("staged bundle is incomplete: %v", err)
	}
	// The ditto sidecar must not end up inside the staged bundle.
	if _, err := os.Stat(filepath.Join(filepath.Dir(staged), "__MACOSX")); !os.IsNotExist(err) {
		t.Errorf("the __MACOSX sidecar was extracted: %v", err)
	}
}

func TestDownloadRejectsChecksumMismatch(t *testing.T) {
	gh := newFakeGitHub(t)
	archive := appZip(t)
	gh.addRelease("v0.2.0", false, false, map[string][]byte{
		AssetName("0.2.0", "arm64"): archive,
	})
	// Publish a checksum that does not match the archive.
	gh.files["/dl/v0.2.0/"+ChecksumAssetName] = []byte(sha256Hex([]byte("something else")) +
		"  " + AssetName("0.2.0", "arm64") + "\n")
	u := New(Options{Owner: "spdeepak", Repo: "Notibridge", CurrentVersion: "0.1.0", Arch: "arm64",
		APIBaseURL: gh.server.URL})
	rel, err := u.Latest(context.Background())
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if _, err := u.Download(context.Background(), rel); err == nil {
		t.Fatal("expected a checksum mismatch error")
	} else if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestDownloadVerifiesSignature(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	archive := appZip(t)
	sums := []byte(sha256Hex(archive) + "  " + AssetName("0.2.0", "arm64") + "\n")
	sig := ed25519.Sign(priv, sums)

	gh := newFakeGitHub(t)
	gh.addRelease("v0.2.0", false, false, map[string][]byte{
		AssetName("0.2.0", "arm64"): archive,
	})
	// Publish the checksum file and its signature.
	gh.files["/dl/v0.2.0/"+ChecksumAssetName] = sums
	gh.files["/dl/v0.2.0/"+SignatureAssetName] = sig
	gh.releases[0].Assets = append(gh.releases[0].Assets, ghAsset{
		Name: SignatureAssetName,
		URL:  gh.server.URL + "/dl/v0.2.0/" + SignatureAssetName,
	})
	u := New(Options{Owner: "spdeepak", Repo: "Notibridge", CurrentVersion: "0.1.0", Arch: "arm64",
		APIBaseURL: gh.server.URL, PublicKey: pub})
	rel, err := u.Latest(context.Background())
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	staged, err := u.Download(context.Background(), rel)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	os.RemoveAll(filepath.Dir(staged))

	// A different key must be rejected.
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	u.opts.PublicKey = other
	if _, err := u.Download(context.Background(), rel); err == nil {
		t.Fatal("expected a signature error")
	} else if !strings.Contains(err.Error(), "signature") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestParsePublicKey(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	encoded := base64.StdEncoding.EncodeToString(pub)
	got, err := ParsePublicKey(encoded)
	if err != nil {
		t.Fatalf("ParsePublicKey: %v", err)
	}
	if !got.Equal(pub) {
		t.Error("decoded key differs")
	}
	if _, err := ParsePublicKey("not base64!"); err == nil {
		t.Error("expected an error for invalid base64")
	}
	if _, err := ParsePublicKey(base64.StdEncoding.EncodeToString([]byte("short"))); err == nil {
		t.Error("expected an error for a short key")
	}
}

// writeBundle creates a minimal application bundle on disk.
func writeBundle(t *testing.T, dir, name, marker string) string {
	t.Helper()
	bundle := filepath.Join(dir, name)
	exe := filepath.Join(bundle, "Contents", "MacOS", "NotiBridge")
	if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil {
		t.Fatalf("create bundle: %v", err)
	}
	if err := os.WriteFile(exe, []byte(marker), 0o755); err != nil {
		t.Fatalf("write bundle executable: %v", err)
	}
	return bundle
}

func TestApplyReplacesBundle(t *testing.T) {
	dir := t.TempDir()
	target := writeBundle(t, dir, "NotiBridge.app", "old")
	staged := writeBundle(t, filepath.Join(dir, "staged"), "NotiBridge.app", "new")

	old := relaunchCommand
	relaunchCommand = []string{"/usr/bin/true"}
	defer func() { relaunchCommand = old }()

	if err := Apply(ApplyOptions{StagedApp: staged, Target: target}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(target, "Contents", "MacOS", "NotiBridge"))
	if err != nil {
		t.Fatalf("read installed bundle: %v", err)
	}
	if string(body) != "new" {
		t.Errorf("installed bundle = %q, want the staged version", body)
	}
	if _, err := os.Stat(target + ".old"); !os.IsNotExist(err) {
		t.Error("the previous bundle should have been removed")
	}
}

func TestApplyKeepsPreviousBundleOnFailure(t *testing.T) {
	dir := t.TempDir()
	target := writeBundle(t, dir, "NotiBridge.app", "old")
	// A staged path that does not exist makes the move fail.
	if err := Apply(ApplyOptions{StagedApp: filepath.Join(dir, "missing.app"), Target: target}); err == nil {
		t.Fatal("expected an error")
	}
	body, err := os.ReadFile(filepath.Join(target, "Contents", "MacOS", "NotiBridge"))
	if err != nil {
		t.Fatalf("the previous bundle should still be in place: %v", err)
	}
	if string(body) != "old" {
		t.Errorf("installed bundle = %q, want the previous version", body)
	}
}

func TestBundlePathOutsideBundle(t *testing.T) {
	if _, err := BundlePath(); err == nil {
		t.Log("the test binary happens to run from an .app bundle; skipping")
	}
}

func TestProcessAlive(t *testing.T) {
	if !processAlive(os.Getpid()) {
		t.Error("the current process must be reported as alive")
	}
}
