// Command notifyrelay is NotiBridge: a macOS menu bar utility that forwards
// Notification Center notifications to a Telegram chat.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/spdeepak/notibridge/internal/app"
	"github.com/spdeepak/notibridge/internal/logging"
	"github.com/spdeepak/notibridge/internal/menubar"
	"github.com/spdeepak/notibridge/internal/notifications/source"
	"github.com/spdeepak/notibridge/internal/webui"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "diagnose":
			os.Exit(runDiagnose())
		case "version":
			fmt.Println("NotiBridge", app.Version)
			return
		case "run":
			os.Args = append(os.Args[:1], os.Args[2:]...)
		case "-h", "--help", "help":
			usage()
			return
		}
	}
	runApp()
}

func usage() {
	fmt.Fprintf(os.Stderr, `NotiBridge %s — macOS notifications → Telegram

Usage:
  notibridge              Start the menu bar application (default)
  notibridge run          Same as above
  notibridge diagnose     Check Notification Center database access and exit
  notibridge version      Print the version

Environment:
  NOTIBRIDGE_HOME         Override the configuration/state directory
  NOTIBRIDGE_LOG_LEVEL    debug, info, warn or error (default: info, or debug with -debug)

Flags:
  -debug                  Verbose logging (notification content is still never logged)
`, app.Version)
}

func newLogger(debug bool) *slog.Logger {
	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}
	switch os.Getenv("NOTIBRIDGE_LOG_LEVEL") {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	return logging.New(level)
}

func runApp() {
	debug := flag.Bool("debug", false, "verbose logging")
	flag.Parse()

	logger := newLogger(*debug)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	svc, err := app.New(app.Options{Logger: logger})
	if err != nil {
		logger.Error("failed to initialize NotiBridge", "error", err)
		os.Exit(1)
	}
	svc.Start(ctx)

	web, err := webui.New(svc, logger)
	if err != nil {
		logger.Error("failed to start settings server", "error", err)
		os.Exit(1)
	}
	web.Start()
	logger.Info("settings page ready", "url", web.URL())

	st := svc.Status()
	if !st.Configured {
		logger.Info("NotiBridge is not configured yet — open Settings from the menu bar to add your bot token and chat ID")
	}
	if !st.SourceAccessible && st.SourceError != "" {
		logger.Warn("notification database is not accessible; run 'notibridge diagnose' for details", "error", st.SourceError)
	}

	// Blocks until the user selects Quit in the menu bar.
	menubar.Run(svc, web, logger, func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer shutdownCancel()
		if err := svc.Shutdown(shutdownCtx); err != nil {
			logger.Warn("shutdown did not complete cleanly", "error", err)
		}
		_ = web.Close()
	})
}

func runDiagnose() int {
	src := source.NewMacOS(source.MacOSOptions{Lookback: time.Minute})
	defer src.Close()

	d := src.Diagnose(context.Background())
	fmt.Println("NotiBridge — Notification Center database diagnostics")
	fmt.Printf("  Database path:   %s\n", d.Path)
	fmt.Printf("  Accessible:      %t\n", d.Accessible)
	fmt.Printf("  Schema known:    %t\n", d.SchemaOK)
	if len(d.Tables) > 0 {
		fmt.Printf("  Tables found:    %v\n", d.Tables)
	}
	if d.RowCount > 0 {
		fmt.Printf("  Records:         %d\n", d.RowCount)
	}
	if !d.Oldest.IsZero() {
		fmt.Printf("  Oldest record:   %s\n", d.Oldest.Local())
	}
	if !d.Newest.IsZero() {
		fmt.Printf("  Newest record:   %s\n", d.Newest.Local())
	}
	if d.Error != "" {
		fmt.Printf("  Error:           %s\n", d.Error)
	}
	for _, h := range d.Hints {
		fmt.Printf("  Hint: %s\n", h)
	}
	if d.Accessible && d.SchemaOK {
		fmt.Println("\nOK: notification records can be read from this Mac.")
		return 0
	}
	fmt.Println("\nFAILED: notification records cannot be read. See hints above.")
	return 1
}
