// Package menubar implements the macOS menu bar interface using the
// lightweight fyne/systray library. All behavior lives in the app service;
// this package only renders state and forwards user actions.
package menubar

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"fyne.io/systray"

	"github.com/spdeepak/notibridge/internal/app"
	"github.com/spdeepak/notibridge/internal/webui"
)

// Run starts the menu bar UI and blocks until the user quits. The onExit
// callback runs after the tray shuts down; it should stop the service and
// close the web UI.
func Run(svc *app.Service, web *webui.Server, logger *slog.Logger, onExit func()) {
	if logger == nil {
		logger = slog.Default()
	}
	cleanup := makeCleanup(onExit)

	// Quit cleanly on Ctrl-C / SIGTERM (useful for development runs).
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		logger.Info("received termination signal; quitting")
		systray.Quit()
	}()

	systray.Run(func() {
		systray.SetTemplateIcon(generateIcon(), generateIcon())
		systray.SetTitle("")
		systray.SetTooltip("NotiBridge — macOS notifications → Telegram")

		statusItem := systray.AddMenuItem("Status: starting…", "Current monitoring status")
		statusItem.Disable()
		queueItem := systray.AddMenuItem("Queue: —", "Delivery counts")
		queueItem.Disable()
		dbItem := systray.AddMenuItem("Notification database: checking…", "Notification Center database access")
		dbItem.Disable()

		systray.AddSeparator()

		pauseItem := systray.AddMenuItemCheckbox("Pause forwarding", "Stop sending notifications to Telegram (queued items are kept)", false)
		forwardItem := systray.AddMenuItemCheckbox("Forwarding enabled", "Master switch, persisted in settings", false)
		testItem := systray.AddMenuItem("Send test message", "Send a test message to the configured Telegram chat")

		systray.AddSeparator()

		settingsItem := systray.AddMenuItem("Settings…", "Open the settings page in your browser")
		activityItem := systray.AddMenuItem("Recent activity…", "Open the delivery activity view in your browser")
		diagItem := systray.AddMenuItem("Run database diagnostics", "Check Notification Center database access")
		clearItem := systray.AddMenuItem("Clear local history", "Delete locally stored notification content and delivery history")

		systray.AddSeparator()
		quitItem := systray.AddMenuItem("Quit NotiBridge", "Stop monitoring and quit")

		go func() {
			for {
				select {
				case <-pauseItem.ClickedCh:
					st := svc.Status()
					if st.Paused {
						svc.Resume()
					} else {
						svc.Pause()
					}
				case <-forwardItem.ClickedCh:
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					st := svc.Status()
					if err := svc.SetForwarding(ctx, !st.ForwardingEnabled); err != nil {
						logger.Warn("could not change forwarding setting", "error", err)
					}
					cancel()
				case <-testItem.ClickedCh:
					ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					if err := svc.SendTestMessage(ctx); err != nil {
						logger.Warn("test message failed", "error", err)
					} else {
						logger.Info("test message sent")
					}
					cancel()
				case <-settingsItem.ClickedCh:
					openBrowser(web.URL(), logger)
				case <-activityItem.ClickedCh:
					openBrowser(web.ActivityURL(), logger)
				case <-diagItem.ClickedCh:
					openBrowser(web.URL()+"&diag=1", logger)
				case <-clearItem.ClickedCh:
					ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
					if _, err := svc.ClearHistory(ctx); err != nil {
						logger.Warn("clear history failed", "error", err)
					}
					cancel()
				case <-quitItem.ClickedCh:
					systray.Quit()
					return
				}
			}
		}()

		// Periodic status refresh.
		go func() {
			ticker := time.NewTicker(2 * time.Second)
			defer ticker.Stop()
			for range ticker.C {
				st := svc.Status()
				statusItem.SetTitle(statusTitle(st))
				queueItem.SetTitle("Queue: " + queueTitle(st))
				if st.SourceAccessible {
					dbItem.SetTitle("Notification database: readable")
				} else {
					dbItem.SetTitle("Notification database: inaccessible (see diagnostics)")
				}
				setChecked(pauseItem, st.Paused)
				if st.Paused {
					pauseItem.SetTitle("Resume forwarding")
				} else {
					pauseItem.SetTitle("Pause forwarding")
				}
				setChecked(forwardItem, st.ForwardingEnabled)
			}
		}()
	}, cleanup)

	// fyne/systray on macOS ends the Cocoa loop from Quit() without firing
	// applicationWillTerminate, so onExit is not reliable there. Run's
	// return means the loop has ended; run cleanup exactly once from here
	// as well.
	cleanup()
}

// cleanup wraps the user callback so it runs exactly once even though both
// systray's onExit hook and menubar.Run's epilogue may fire it.
func makeCleanup(onExit func()) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			if onExit != nil {
				onExit()
			}
		})
	}
}

func setChecked(item *systray.MenuItem, v bool) {
	if v {
		item.Check()
	} else {
		item.Uncheck()
	}
}

func statusTitle(st app.Status) string {
	switch {
	case !st.Configured:
		return "Status: not configured (open Settings)"
	case st.Paused:
		return "Status: paused"
	case !st.ForwardingEnabled:
		return "Status: forwarding disabled"
	case !st.SourceAccessible:
		return "Status: notification database inaccessible"
	default:
		return "Status: monitoring & forwarding"
	}
}

func queueTitle(st app.Status) string {
	return fmtCounts(st)
}

func fmtCounts(st app.Status) string {
	return trimInt(st.Counts.Pending) + " pending · " + trimInt(st.Counts.Sent) + " sent · " + trimInt(st.Counts.Failed) + " failed"
}

func trimInt(n int64) string {
	return strconv.FormatInt(n, 10)
}

func openBrowser(url string, logger *slog.Logger) {
	if err := exec.Command("open", url).Start(); err != nil {
		logger.Warn("could not open browser; open this URL manually", "url", url, "error", err)
	}
}
