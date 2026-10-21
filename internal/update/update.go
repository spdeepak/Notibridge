// Package update implements NotiBridge's release-based self-update.
//
// The flow is deliberately conservative:
//
//  1. Check GitHub Releases for a stable release that is newer than the
//     running one and publishes a build for this Mac's architecture.
//  2. Download that build together with the release's SHA-256 checksum file.
//  3. Verify the checksum and — when a public key is embedded at build time —
//     verify the Ed25519 signature over the checksum file, which proves the
//     release was published by the project owner.
//  4. Stage the unpacked bundle and hand it to a detached helper process that
//     swaps it in once the running instance has exited.
//
// A SHA-256 checksum alone proves that a download was not corrupted in
// transit; it does not prove who published it. That is why the signature
// check is optional but recommended.
package update

import (
	"archive/zip"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// PublicKey is the base64-encoded Ed25519 public key that verifies the
// signature over release checksum files. Set it at build time:
//
//	-X github.com/spdeepak/notibridge/internal/update.PublicKey=<base64>
//
// When it is empty, updates are verified by SHA-256 checksum only, which
// proves the download was intact but not who published it.
var PublicKey = ""

// ReleaseOwner and ReleaseRepository identify the GitHub repository whose
// releases are checked for updates.
var (
	ReleaseOwner      = "spdeepak"
	ReleaseRepository = "Notibridge"
)

// DefaultAPIBaseURL is the GitHub REST API root.
const DefaultAPIBaseURL = "https://api.github.com"

// ChecksumAssetName is the file each release publishes with the SHA-256
// checksums of its downloadable assets.
const ChecksumAssetName = "SHA256SUMS"

// SignatureAssetName is the detached Ed25519 signature over the checksum file.
const SignatureAssetName = ChecksumAssetName + ".sig"

// Options configures an Updater.
type Options struct {
	// Owner and Repo identify the GitHub repository releases are read from.
	Owner string
	Repo  string
	// CurrentVersion is the running version. Releases that are not newer are
	// ignored.
	CurrentVersion string
	// Arch is the CPU architecture to install ("arm64" or "amd64").
	Arch string
	// PublicKey verifies the release checksum file. When nil the checksum
	// file is trusted as published: it proves integrity, not authenticity.
	PublicKey ed25519.PublicKey
	// HTTPClient is used for every request; nil selects a 30 second timeout.
	HTTPClient *http.Client
	// APIBaseURL overrides the GitHub API root (tests).
	APIBaseURL string
}

// Release is an available release that publishes a build for this
// architecture.
type Release struct {
	// Version is the release version without the "v" tag prefix.
	Version string
	// Notes is the release description.
	Notes string
	// URL is the download URL of the application archive.
	URL string
	// AssetName is the file name of the application archive.
	AssetName string
	// Size is the archive size in bytes.
	Size int64
	// ChecksumsURL is the download URL of the release checksum file.
	ChecksumsURL string
	// SignatureURL is the download URL of the detached signature over the
	// checksum file. It is empty when the release publishes no signature.
	SignatureURL string
}

// Updater checks GitHub Releases for newer NotiBridge builds.
type Updater struct {
	opts Options
	http *http.Client
}

// New builds an updater. Empty options fall back to the package defaults.
func New(opts Options) *Updater {
	if opts.Owner == "" {
		opts.Owner = ReleaseOwner
	}
	if opts.Repo == "" {
		opts.Repo = ReleaseRepository
	}
	if opts.Arch == "" {
		opts.Arch = runtime.GOARCH
	}
	if opts.APIBaseURL == "" {
		opts.APIBaseURL = DefaultAPIBaseURL
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &Updater{opts: opts, http: opts.HTTPClient}
}

// ParsePublicKey decodes a base64-encoded Ed25519 public key.
func ParsePublicKey(s string) (ed25519.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return nil, fmt.Errorf("decode public key: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("public key must be %d bytes, got %d", ed25519.PublicKeySize, len(raw))
	}
	return ed25519.PublicKey(raw), nil
}

// AssetName returns the archive file name a release publishes for an
// architecture.
func AssetName(version, arch string) string {
	return fmt.Sprintf("NotiBridge_%s_%s.zip", version, arch)
}

// Latest returns the newest stable release that is newer than the running
// version and publishes a build for this architecture. It returns (nil, nil)
// when the application is up to date.
func (u *Updater) Latest(ctx context.Context) (*Release, error) {
	releases, err := u.fetchReleases(ctx)
	if err != nil {
		return nil, err
	}

	var best *Release
	newestTag := ""
	reason := ""
	for _, r := range releases {
		if r.Draft || r.Prerelease {
			continue
		}
		version := strings.TrimPrefix(r.TagName, "v")
		if Compare(version, u.opts.CurrentVersion) <= 0 {
			continue
		}
		if newestTag == "" || Compare(version, newestTag) > 0 {
			newestTag = version
			reason = ""
		}
		rel, why := u.releaseFor(r, version)
		if rel == nil {
			if reason == "" {
				reason = why
			}
			continue
		}
		if best == nil || Compare(rel.Version, best.Version) > 0 {
			best = rel
		}
	}
	if best == nil && newestTag != "" {
		return nil, fmt.Errorf("release %s cannot be installed on %s: %s", newestTag, u.opts.Arch, reason)
	}
	return best, nil
}

// releaseFor maps a GitHub release to a downloadable release for this
// architecture. It returns nil and a human-readable reason when the release
// does not publish the required files.
func (u *Updater) releaseFor(r ghRelease, version string) (*Release, string) {
	name := AssetName(version, u.opts.Arch)
	archive := assetByName(r.Assets, name)
	if archive == nil {
		return nil, fmt.Sprintf("it publishes no %s build", u.opts.Arch)
	}
	sums := assetByName(r.Assets, ChecksumAssetName)
	if sums == nil {
		return nil, fmt.Sprintf("it publishes no %s file", ChecksumAssetName)
	}
	rel := &Release{
		Version:      version,
		Notes:        r.Body,
		URL:          archive.URL,
		AssetName:    archive.Name,
		Size:         archive.Size,
		ChecksumsURL: sums.URL,
	}
	if sig := assetByName(r.Assets, SignatureAssetName); sig != nil {
		rel.SignatureURL = sig.URL
	}
	return rel, ""
}

// Download fetches the release archive, verifies it against the published
// checksums and unpacks it. It returns the path of the staged application
// bundle. Nothing is installed: call Install for that.
func (u *Updater) Download(ctx context.Context, rel *Release) (string, error) {
	if rel == nil {
		return "", fmt.Errorf("no release given")
	}
	dir, err := os.MkdirTemp("", "notibridge-update-")
	if err != nil {
		return "", fmt.Errorf("create staging directory: %w", err)
	}
	sums, err := u.fetchChecksums(ctx, rel)
	if err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	want, ok := sums[rel.AssetName]
	if !ok {
		os.RemoveAll(dir)
		return "", fmt.Errorf("release %s publishes no checksum for %s", rel.Version, rel.AssetName)
	}

	archive := filepath.Join(dir, rel.AssetName)
	got, err := u.download(ctx, rel.URL, archive)
	if err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	if !strings.EqualFold(got, want) {
		os.RemoveAll(dir)
		return "", fmt.Errorf("checksum mismatch for %s: expected %s, got %s", rel.AssetName, want, got)
	}

	appPath, err := extractApp(archive, dir)
	if err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	return appPath, nil
}

// fetchChecksums downloads and, when a public key is available, verifies the
// release checksum file before parsing it.
func (u *Updater) fetchChecksums(ctx context.Context, rel *Release) (map[string]string, error) {
	raw, err := u.fetchBytes(ctx, rel.ChecksumsURL)
	if err != nil {
		return nil, fmt.Errorf("fetch release checksums: %w", err)
	}
	if u.opts.PublicKey != nil {
		if rel.SignatureURL == "" {
			return nil, fmt.Errorf("release %s publishes no signature and the embedded public key requires one", rel.Version)
		}
		sig, err := u.fetchBytes(ctx, rel.SignatureURL)
		if err != nil {
			return nil, fmt.Errorf("fetch release signature: %w", err)
		}
		if !ed25519.Verify(u.opts.PublicKey, raw, sig) {
			return nil, fmt.Errorf("the release signature is not valid for the embedded public key; refusing to update")
		}
	}
	return parseChecksums(raw)
}

// parseChecksums reads a "sha256sum"-style file: "<hex>  <name>" per line.
func parseChecksums(raw []byte) (map[string]string, error) {
	out := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		name := strings.TrimPrefix(fields[len(fields)-1], "*")
		out[name] = strings.ToLower(fields[0])
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("the release checksum file is empty")
	}
	return out, nil
}

// fetchReleases reads the release list from the GitHub API.
func (u *Updater) fetchReleases(ctx context.Context) ([]ghRelease, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/releases?per_page=30",
		strings.TrimRight(u.opts.APIBaseURL, "/"), u.opts.Owner, u.opts.Repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build releases request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "NotiBridge/"+u.opts.CurrentVersion)

	resp, err := u.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("contact GitHub: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("read releases response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub releases request failed (%s): %s",
			resp.Status, statusHint(resp.StatusCode, body))
	}
	var out []ghRelease
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("parse releases response: %w", err)
	}
	return out, nil
}

// statusHint turns an API error into an actionable hint.
func statusHint(code int, body []byte) string {
	text := strings.ToLower(string(body))
	switch {
	case code == http.StatusForbidden && strings.Contains(text, "rate limit"):
		return "the GitHub API rate limit was reached; try again later"
	case code == http.StatusNotFound:
		return "the repository or its releases were not found"
	case code == http.StatusUnauthorized:
		return "the GitHub API rejected the request"
	}
	if len(body) > 200 {
		body = body[:200]
	}
	return strings.TrimSpace(string(body))
}

// fetchBytes downloads a small file into memory.
func (u *Updater) fetchBytes(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", "NotiBridge/"+u.opts.CurrentVersion)
	resp, err := u.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s responded with %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 4<<20))
}

// download streams a file to dest and returns its SHA-256 sum.
func (u *Updater) download(ctx context.Context, url, dest string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("build download request: %w", err)
	}
	req.Header.Set("User-Agent", "NotiBridge/"+u.opts.CurrentVersion)
	resp, err := u.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s failed (%s)", url, resp.Status)
	}
	f, err := os.Create(dest)
	if err != nil {
		return "", fmt.Errorf("create %s: %w", dest, err)
	}
	h := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(f, h), resp.Body)
	closeErr := f.Close()
	if copyErr != nil {
		return "", fmt.Errorf("download %s: %w", url, copyErr)
	}
	if closeErr != nil {
		return "", fmt.Errorf("write %s: %w", dest, closeErr)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// extractApp unpacks a release archive and returns the path of the
// application bundle inside it.
func extractApp(archive, dir string) (string, error) {
	r, err := zip.OpenReader(archive)
	if err != nil {
		return "", fmt.Errorf("open release archive: %w", err)
	}
	defer r.Close()

	out := filepath.Join(dir, "unpacked")
	if err := os.MkdirAll(out, 0o755); err != nil {
		return "", fmt.Errorf("create unpack directory: %w", err)
	}
	for _, f := range r.File {
		name := filepath.Clean(f.Name)
		if name == "." || strings.HasPrefix(name, "__MACOSX") {
			// Resource-fork sidecar created by ditto; not part of the app.
			continue
		}
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("the release archive contains an unsafe path: %s", f.Name)
		}
		target := filepath.Join(out, name)
		if f.Mode().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return "", fmt.Errorf("create %s: %w", target, err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return "", fmt.Errorf("create %s: %w", filepath.Dir(target), err)
		}
		if err := extractFile(f, target); err != nil {
			return "", err
		}
	}
	return findApp(out)
}

// extractFile writes a single archive entry to target.
func extractFile(f *zip.File, target string) error {
	rc, err := f.Open()
	if err != nil {
		return fmt.Errorf("open archive entry %s: %w", f.Name, err)
	}
	defer rc.Close()

	if f.Mode()&os.ModeSymlink != 0 {
		link, err := io.ReadAll(io.LimitReader(rc, 4096))
		if err != nil {
			return fmt.Errorf("read link %s: %w", f.Name, err)
		}
		if err := os.Symlink(string(link), target); err != nil {
			return fmt.Errorf("create link %s: %w", target, err)
		}
		return nil
	}
	perm := f.Mode().Perm()
	if perm == 0 {
		perm = 0o644
	}
	w, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return fmt.Errorf("create %s: %w", target, err)
	}
	if _, err := io.Copy(w, rc); err != nil {
		w.Close()
		return fmt.Errorf("write %s: %w", target, err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("close %s: %w", target, err)
	}
	// OpenFile applies the umask; set the archived mode explicitly.
	if err := os.Chmod(target, perm); err != nil {
		return fmt.Errorf("set mode on %s: %w", target, err)
	}
	return nil
}

// findApp returns the first .app bundle inside an unpacked release.
func findApp(root string) (string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", fmt.Errorf("read unpacked release: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() && strings.HasSuffix(e.Name(), ".app") {
			return filepath.Join(root, e.Name()), nil
		}
	}
	return "", fmt.Errorf("the release archive does not contain an application bundle")
}

// ---- version comparison ----

// Compare returns -1, 0 or 1 comparing two semantic versions. A leading "v"
// is ignored and a pre-release suffix sorts before the matching release.
func Compare(a, b string) int {
	va, vb := parseVersion(a), parseVersion(b)
	for i := 0; i < 3; i++ {
		na, nb := versionNumber(va, i), versionNumber(vb, i)
		if na != nb {
			if na < nb {
				return -1
			}
			return 1
		}
	}
	switch {
	case va.pre == vb.pre:
		return 0
	case va.pre == "":
		return 1
	case vb.pre == "":
		return -1
	default:
		return strings.Compare(va.pre, vb.pre)
	}
}

type version struct {
	nums []int
	pre  string
}

func parseVersion(s string) version {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(strings.TrimPrefix(s, "v"), "V")
	if i := strings.Index(s, "+"); i >= 0 {
		s = s[:i] // build metadata is ignored
	}
	var v version
	if i := strings.Index(s, "-"); i >= 0 {
		v.pre = s[i+1:]
		s = s[:i]
	}
	for _, part := range strings.Split(s, ".") {
		n, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil {
			// A malformed version is treated as 0.0.0 so that a broken tag
			// is never mistaken for something newer than the running build.
			return version{}
		}
		v.nums = append(v.nums, n)
	}
	return v
}

func versionNumber(v version, i int) int {
	if i < len(v.nums) {
		return v.nums[i]
	}
	return 0
}

// ---- GitHub API shapes ----

type ghAsset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

type ghRelease struct {
	TagName    string    `json:"tag_name"`
	Body       string    `json:"body"`
	Draft      bool      `json:"draft"`
	Prerelease bool      `json:"prerelease"`
	Assets     []ghAsset `json:"assets"`
}

func assetByName(assets []ghAsset, name string) *ghAsset {
	for i := range assets {
		if assets[i].Name == name {
			return &assets[i]
		}
	}
	return nil
}
