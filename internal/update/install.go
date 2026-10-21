package update

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ApplyUpdateCommand is the internal subcommand that swaps a staged bundle in
// place. It is started by Install and runs detached from the application.
const ApplyUpdateCommand = "apply-update"

// ErrManualInstall reports that the update cannot be applied automatically.
// The wrapped message explains how to update by hand.
var ErrManualInstall = errors.New("manual installation required")

// ManualSteps returns instructions for installing a newer version by hand.
func ManualSteps() string {
	return "Download the newer version from the releases page, quit NotiBridge, " +
		"replace the installed NotiBridge.app with the new one and start NotiBridge again. " +
		"Your settings and the bot token are kept: they live outside the application bundle."
}

// Install replaces the running application bundle with the staged one and
// relaunches it.
//
// The swap is performed by a detached helper process that waits for this
// instance to exit, so the running binary is never replaced underneath
// itself. When the bundle location is not writable, Install returns an error
// describing the manual steps instead of pretending the update was applied.
func (u *Updater) Install(ctx context.Context, stagedApp string) error {
	if stagedApp == "" {
		return fmt.Errorf("no staged application to install")
	}
	if _, err := os.Stat(stagedApp); err != nil {
		return fmt.Errorf("the staged update is no longer available: %w", err)
	}
	target, err := BundlePath()
	if err != nil {
		return fmt.Errorf("%w: %s", ErrManualInstall, err)
	}
	if err := checkWritable(filepath.Dir(target)); err != nil {
		return fmt.Errorf("%w: %s", ErrManualInstall, err)
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("%w: cannot locate the running executable: %v", ErrManualInstall, err)
	}
	cmd := exec.Command(exe, ApplyUpdateCommand, "--staged", stagedApp, "--pid", strconv.Itoa(os.Getpid()))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%w: could not start the update helper: %v", ErrManualInstall, err)
	}
	// The helper outlives this process; reap it in the background.
	go func() { _ = cmd.Wait() }()
	return nil
}

// ApplyOptions configures the swap helper.
type ApplyOptions struct {
	// StagedApp is the unpacked application bundle to install.
	StagedApp string
	// Target is the bundle to replace. Empty means the bundle that contains
	// the running executable.
	Target string
	// ParentPID is the application instance to wait for. Zero skips waiting.
	ParentPID int
	// Timeout bounds the wait for the parent process. Zero waits for an hour.
	Timeout time.Duration
}

// relaunchCommand starts the installed bundle. It is a variable so that
// tests can replace it.
var relaunchCommand = []string{"/usr/bin/open"}

// Apply installs a staged bundle: it waits for the running application to
// exit, replaces the bundle and relaunches the application.
//
// The previous bundle is moved aside first and restored if the new bundle
// cannot be moved into place, so a failed update never leaves the user
// without an application.
func Apply(opts ApplyOptions) error {
	if opts.StagedApp == "" {
		return fmt.Errorf("no staged application given")
	}
	target := opts.Target
	if target == "" {
		var err error
		target, err = BundlePath()
		if err != nil {
			return err
		}
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = time.Hour
	}
	if err := waitForExit(opts.ParentPID, timeout); err != nil {
		return err
	}

	backup := target + ".old"
	if err := os.RemoveAll(backup); err != nil {
		return fmt.Errorf("remove the previous backup: %w", err)
	}
	if err := os.Rename(target, backup); err != nil {
		return fmt.Errorf("move the current application aside: %w", err)
	}
	if err := moveBundle(opts.StagedApp, target); err != nil {
		if rerr := os.Rename(backup, target); rerr != nil {
			return fmt.Errorf("install the new application: %w (the previous version could not be restored: %v)", err, rerr)
		}
		return fmt.Errorf("install the new application: %w", err)
	}
	if err := relaunch(target); err != nil {
		return fmt.Errorf("relaunch the application: %w", err)
	}
	// The old bundle still holds this helper's own executable; removing it is
	// safe on macOS because the running image keeps its inode.
	if err := os.RemoveAll(backup); err != nil {
		logf("could not remove the previous application: %v", err)
	}
	return nil
}

// BundlePath returns the .app bundle that contains the running executable.
// It fails when the application runs from a plain binary (a development
// build), where there is no bundle to replace.
func BundlePath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	// .../NotiBridge.app/Contents/MacOS/NotiBridge
	macOS := filepath.Dir(exe)
	if filepath.Base(macOS) != "MacOS" {
		return "", fmt.Errorf("NotiBridge is not running from an application bundle")
	}
	contents := filepath.Dir(macOS)
	if filepath.Base(contents) != "Contents" {
		return "", fmt.Errorf("NotiBridge is not running from an application bundle")
	}
	bundle := filepath.Dir(contents)
	if !strings.HasSuffix(bundle, ".app") {
		return "", fmt.Errorf("NotiBridge is not running from an application bundle")
	}
	return bundle, nil
}

// checkWritable reports whether a file can be created in dir.
func checkWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".notibridge-write-test-")
	if err != nil {
		return fmt.Errorf("%s is not writable", dir)
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	return nil
}

// waitForExit blocks until the process with the given PID is gone.
func waitForExit(pid int, timeout time.Duration) error {
	if pid <= 0 {
		return nil
	}
	deadline := time.Now().Add(timeout)
	for {
		if !processAlive(pid) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the running application did not exit within %s", timeout)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// processAlive reports whether a process with the given PID exists.
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// relaunch starts the installed application bundle.
func relaunch(bundle string) error {
	args := append(append([]string{}, relaunchCommand[1:]...), bundle)
	return exec.Command(relaunchCommand[0], args...).Run()
}

// moveBundle moves src to dst, falling back to a recursive copy when the two
// live on different volumes.
func moveBundle(src, dst string) error {
	err := os.Rename(src, dst)
	if err == nil {
		return nil
	}
	if !errors.Is(err, syscall.EXDEV) {
		return err
	}
	if err := copyTree(src, dst); err != nil {
		return err
	}
	return os.RemoveAll(src)
}

// copyTree recursively copies a directory tree, preserving permissions and
// symbolic links.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o755)
		case d.Type()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		default:
			info, err := d.Info()
			if err != nil {
				return err
			}
			return copyFile(path, target, info.Mode().Perm())
		}
	})
}

// copyFile copies one file with the given permissions.
func copyFile(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// logf appends a line to the update log in the temporary directory. The
// helper runs detached, so its output is not visible to the user.
func logf(format string, args ...any) {
	path := filepath.Join(os.TempDir(), "notibridge-update.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %s\n", time.Now().Format(time.RFC3339), fmt.Sprintf(format, args...))
}
