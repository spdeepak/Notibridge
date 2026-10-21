package app

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/spdeepak/notibridge/internal/update"
)

// UpdateState describes the updater state for the UI. The stages are
// deliberately distinct: a downloaded update is not an installed one.
type UpdateState struct {
	// CurrentVersion is the running version.
	CurrentVersion string
	// LatestVersion is the newest release found by the last check, empty
	// when the application is up to date.
	LatestVersion string
	// Notes is the release description of the newest release.
	Notes string
	// CheckedAt is when the last check finished.
	CheckedAt time.Time
	// Checking reports whether a check is running right now.
	Checking bool
	// StagedVersion is the version that has been downloaded and verified but
	// not installed yet.
	StagedVersion string
	// StagedPath is the staged application bundle on disk.
	StagedPath string
	// Error is the last updater error, empty when the last action succeeded.
	Error string
}

// Available reports whether a newer release was found.
func (u UpdateState) Available() bool { return u.LatestVersion != "" }

// Staged reports whether an update has been downloaded and verified but is
// not installed yet.
func (u UpdateState) Staged() bool { return u.StagedPath != "" }

// UpdateStatus returns a snapshot of the updater state.
func (s *Service) UpdateStatus() UpdateState {
	s.updMu.Lock()
	defer s.updMu.Unlock()
	st := s.updState
	st.CurrentVersion = Version
	return st
}

// CheckForUpdates asks GitHub Releases whether a newer version is available.
// It reports the resulting state and any error.
func (s *Service) CheckForUpdates(ctx context.Context) (UpdateState, error) {
	s.mutateUpdateState(func(st *UpdateState) { st.Checking = true })

	rel, err := s.upd.Latest(ctx)
	if err != nil {
		s.mutateUpdateState(func(st *UpdateState) {
			st.Checking = false
			st.CheckedAt = time.Now()
			st.Error = err.Error()
		})
		return s.UpdateStatus(), err
	}

	s.mutateUpdateState(func(st *UpdateState) {
		st.Checking = false
		st.CheckedAt = time.Now()
		st.Error = ""
		if rel == nil {
			st.LatestVersion = ""
			st.Notes = ""
			return
		}
		st.LatestVersion = rel.Version
		st.Notes = rel.Notes
	})
	return s.UpdateStatus(), nil
}

// DownloadUpdate downloads the newest release, verifies it against the
// published checksums and stages it for installation. The staged application
// is not installed yet: call InstallUpdate for that.
func (s *Service) DownloadUpdate(ctx context.Context) (UpdateState, error) {
	rel, err := s.upd.Latest(ctx)
	if err != nil {
		s.mutateUpdateState(func(st *UpdateState) { st.Error = err.Error() })
		return s.UpdateStatus(), err
	}
	if rel == nil {
		err := fmt.Errorf("no newer release is available")
		s.mutateUpdateState(func(st *UpdateState) { st.Error = err.Error() })
		return s.UpdateStatus(), err
	}

	staged, err := s.upd.Download(ctx, rel)
	if err != nil {
		s.mutateUpdateState(func(st *UpdateState) { st.Error = err.Error() })
		return s.UpdateStatus(), err
	}
	s.mutateUpdateState(func(st *UpdateState) {
		st.LatestVersion = rel.Version
		st.Notes = rel.Notes
		st.StagedVersion = rel.Version
		st.StagedPath = staged
		st.Error = ""
	})
	return s.UpdateStatus(), nil
}

// InstallUpdate replaces the running application with the staged update. The
// application quits and is relaunched by the update helper.
func (s *Service) InstallUpdate(ctx context.Context) (UpdateState, error) {
	st := s.UpdateStatus()
	if st.StagedPath == "" {
		err := fmt.Errorf("no update has been downloaded and verified yet")
		s.mutateUpdateState(func(u *UpdateState) { u.Error = err.Error() })
		return s.UpdateStatus(), err
	}
	if err := s.upd.Install(ctx, st.StagedPath); err != nil {
		msg := err.Error()
		if errors.Is(err, update.ErrManualInstall) {
			msg += " " + update.ManualSteps()
		}
		s.mutateUpdateState(func(u *UpdateState) { u.Error = msg })
		return s.UpdateStatus(), errors.New(msg)
	}
	s.logger.Info("update staged for installation", "version", st.StagedVersion)
	return s.UpdateStatus(), nil
}

// mutateUpdateState applies a change to the updater state.
func (s *Service) mutateUpdateState(f func(*UpdateState)) {
	s.updMu.Lock()
	defer s.updMu.Unlock()
	f(&s.updState)
}

// newUpdater builds the release updater. The embedded public key is optional:
// without it, updates are verified by SHA-256 checksum only, which proves the
// download was intact but not who published it.
func newUpdater(logger *slog.Logger, opts Options) *update.Updater {
	var key ed25519.PublicKey
	if update.PublicKey != "" {
		k, err := update.ParsePublicKey(update.PublicKey)
		if err != nil {
			logger.Warn("the embedded update public key is invalid; falling back to checksum-only verification", "error", err)
		} else {
			key = k
		}
	}
	return update.New(update.Options{
		Owner:          opts.UpdateOwner,
		Repo:           opts.UpdateRepo,
		CurrentVersion: Version,
		PublicKey:      key,
		HTTPClient:     opts.UpdateHTTPClient,
		APIBaseURL:     opts.UpdateAPIBaseURL,
	})
}
