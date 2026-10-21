// Package source provides platform-specific notification sources. The macOS
// implementation reads the Notification Center database (db2) in read-only
// mode and discovers the schema instead of assuming one fixed layout.
package source

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// macEpochOffset converts Mac absolute time to Unix seconds.
const macEpochOffset = 978307200

// MacOSOptions configures the macOS notification source.
type MacOSOptions struct {
	// Lookback bounds how far back the first poll reaches. It prevents a
	// freshly installed NotiBridge from forwarding a large historical
	// backlog. Defaults to 5 minutes.
	Lookback time.Duration
	// DBPath overrides automatic database path detection. Intended for
	// tests.
	DBPath string
}

// MacOSSource reads notification records from the macOS Notification Center
// SQLite database in read-only mode.
//
// This relies on an undocumented, Apple-internal database. Access can be
// blocked by macOS privacy protections (TCC) and the schema may change in
// future macOS releases; the source detects both and reports them through
// Diagnostics instead of failing silently.
type MacOSSource struct {
	opts    MacOSOptions
	mu      sync.Mutex
	db      *sql.DB
	dbPath  string // resolved lazily on first open
	schema  schemaInfo
	openErr error
}

type schemaInfo struct {
	tables     []string
	recTable   string
	pkCol      string
	uuidCol    string // may be ""
	dataCol    string
	dateCol    string
	appCol     string // may be ""
	appTable   string // may be ""
	appIDCol   string // may be ""
	appNameCol string // may be ""
}

// NewMacOS returns a source for the current user's Notification Center
// database.
func NewMacOS(opts MacOSOptions) *MacOSSource {
	if opts.Lookback <= 0 {
		opts.Lookback = 5 * time.Minute
	}
	return &MacOSSource{opts: opts}
}

// DetectDBPath returns the best candidate path for the Notification Center
// database on this system, checking macOS generations newest-first. It does
// not verify that the file is readable.
func DetectDBPath() (string, error) {
	var candidates []string

	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates,
			filepath.Join(home, "Library", "Group Containers", "group.com.apple.usernoted", "db2", "db"))
	}
	// macOS 10.13–14 keep the database under the per-user DARWIN_USER_DIR.
	if out, err := exec.Command("getconf", "DARWIN_USER_DIR").Output(); err == nil {
		if dir := strings.TrimSpace(string(out)); dir != "" {
			candidates = append(candidates, filepath.Join(dir, "com.apple.notificationcenter", "db2", "db"))
		}
	}

	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	if len(candidates) > 0 {
		return candidates[0], nil // report the primary path even if missing
	}
	return "", errors.New("no candidate Notification Center database path found")
}

func (s *MacOSSource) path() (string, error) {
	if s.opts.DBPath != "" {
		return s.opts.DBPath, nil
	}
	if s.dbPath != "" {
		return s.dbPath, nil
	}
	p, err := DetectDBPath()
	if err != nil {
		return "", err
	}
	s.dbPath = p
	return p, nil
}

// dsn builds the read-only SQLite connection string. immutable=1 is used as a
// last resort for databases whose WAL sidecar files cannot be opened by the
// calling process; it may miss very recent writes, which polling tolerates.
func dsn(path string, immutable bool) string {
	u := url.URL{Scheme: "file", Path: path}
	q := u.Query()
	q.Set("mode", "ro")
	q.Set("_pragma", "busy_timeout(1000)")
	if immutable {
		q.Set("immutable", "1")
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// ensureOpen lazily (re)opens the database read-only and validates the
// schema. Reopening happens automatically after failures so transient errors
// do not permanently disable monitoring.
func (s *MacOSSource) ensureOpen() error {
	if s.db != nil {
		return nil
	}
	path, err := s.path()
	if err != nil {
		return err
	}
	db, err := sql.Open("sqlite", dsn(path, false))
	if err != nil {
		return fmt.Errorf("open notification database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(0)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		// Retry without WAL sidecar access.
		db2, err2 := sql.Open("sqlite", dsn(path, true))
		if err2 != nil {
			return fmt.Errorf("open notification database: %w", err)
		}
		db2.SetMaxOpenConns(1)
		if err := db2.Ping(); err != nil {
			_ = db2.Close()
			return fmt.Errorf("open notification database: %w", err)
		}
		db = db2
	}
	sc, err := discoverSchema(db)
	if err != nil {
		_ = db.Close()
		return err
	}
	s.db = db
	s.schema = sc
	s.openErr = nil
	return nil
}

// invalidate closes the connection so the next call reopens it.
func (s *MacOSSource) invalidate() {
	if s.db != nil {
		_ = s.db.Close()
		s.db = nil
	}
}

// Close implements Source.
func (s *MacOSSource) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.invalidate()
	return nil
}

// discoverSchema inspects sqlite_master and the record/app tables. It returns
// an error describing what was found when the layout is not understood.
func discoverSchema(db *sql.DB) (schemaInfo, error) {
	var sc schemaInfo
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table'`)
	if err != nil {
		return sc, fmt.Errorf("list tables: %w", err)
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return sc, fmt.Errorf("scan table name: %w", err)
		}
		sc.tables = append(sc.tables, name)
		cols[strings.ToLower(name)] = true
	}
	if err := rows.Err(); err != nil {
		return sc, fmt.Errorf("list tables: %w", err)
	}

	if !cols["record"] {
		return sc, fmt.Errorf("notification schema not understood: no 'record' table (found: %s)", strings.Join(sc.tables, ", "))
	}
	recordCols, err := tableColumns(db, "record")
	if err != nil {
		return sc, err
	}
	pk, err := tablePK(db, "record")
	if err != nil {
		return sc, err
	}
	sc.recTable = "record"
	sc.pkCol = pk
	sc.dataCol = pick(recordCols, "data")
	sc.dateCol = pick(recordCols, "delivered_date")
	sc.uuidCol = pick(recordCols, "uuid")
	sc.appCol = pick(recordCols, "app_id")
	if sc.dataCol == "" || sc.dateCol == "" || sc.pkCol == "" {
		return sc, fmt.Errorf("notification schema not understood: 'record' table is missing required columns (found: %s)", strings.Join(recordCols, ", "))
	}

	if cols["app"] {
		appCols, err := tableColumns(db, "app")
		if err == nil {
			sc.appTable = "app"
			sc.appIDCol = pick(appCols, "app_id")
			sc.appNameCol = pick(appCols, "identifier")
			if sc.appIDCol == "" || sc.appNameCol == "" {
				sc.appTable, sc.appIDCol, sc.appNameCol = "", "", ""
				sc.appCol = ""
			}
		}
	} else {
		sc.appCol = ""
	}
	return sc, nil
}

func tableColumns(db *sql.DB, table string) ([]string, error) {
	rows, err := db.Query(fmt.Sprintf(`PRAGMA table_info(%s)`, table))
	if err != nil {
		return nil, fmt.Errorf("describe %s: %w", table, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var cid int
		var name, colType string
		var notNull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dflt, &pk); err != nil {
			return nil, fmt.Errorf("describe %s: %w", table, err)
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

func tablePK(db *sql.DB, table string) (string, error) {
	rows, err := db.Query(fmt.Sprintf(`PRAGMA table_info(%s)`, table))
	if err != nil {
		return "", fmt.Errorf("describe %s: %w", table, err)
	}
	defer rows.Close()
	type col struct {
		name string
		pk   int
	}
	var cols []col
	for rows.Next() {
		var cid int
		var name, colType string
		var notNull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dflt, &pk); err != nil {
			return "", fmt.Errorf("describe %s: %w", table, err)
		}
		cols = append(cols, col{name, pk})
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	// Prefer an INTEGER primary key.
	for _, c := range cols {
		if c.pk == 1 {
			return c.name, nil
		}
	}
	return "", fmt.Errorf("no primary key found in %s", table)
}

func pick(cols []string, want string) string {
	for _, c := range cols {
		if strings.EqualFold(c, want) {
			return c
		}
	}
	return ""
}

// quoteIdent quotes an identifier discovered from the schema. Only
// double quotes are valid in SQLite identifiers, and they cannot appear in
// discovered names in practice; strip them defensively.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, "") + `"`
}

// Poll implements Source.
func (s *MacOSSource) Poll(ctx context.Context, since Watermark, limit int) ([]Record, Watermark, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 {
		limit = 200
	}
	if err := s.ensureOpen(); err != nil {
		return nil, since, err
	}

	sc := s.schema
	join := ""
	if sc.appCol != "" && sc.appTable != "" {
		join = fmt.Sprintf(" LEFT JOIN %s a ON a.%s = r.%s",
			quoteIdent(sc.appTable), quoteIdent(sc.appIDCol), quoteIdent(sc.appCol))
	}
	appSel := "''"
	if sc.appCol != "" && sc.appTable != "" {
		appSel = fmt.Sprintf("COALESCE(a.%s,'')", quoteIdent(sc.appNameCol))
	}
	dateExpr := fmt.Sprintf("COALESCE(r.%s,0)", quoteIdent(sc.dateCol))

	var query string
	var args []interface{}
	base := fmt.Sprintf("SELECT r.%s, %s, %s, %s, r.%s FROM record r%s",
		quoteIdent(sc.pkCol), uuidSel(sc), appSel, dateExpr, quoteIdent(sc.dataCol), join)

	switch {
	case since.Valid:
		query = base + fmt.Sprintf(" WHERE %s > ? OR (%s = ? AND r.%s > ?) ORDER BY %s ASC, r.%s ASC LIMIT ?",
			dateExpr, dateExpr, quoteIdent(sc.pkCol), dateExpr, quoteIdent(sc.pkCol))
		args = []interface{}{since.DeliveredDateSecs, since.DeliveredDateSecs, since.RecID, limit}
	default:
		// First poll: only reach back as far as the lookback window so a
		// new install does not replay a historical backlog.
		cutoff := time.Now().Add(-s.opts.Lookback).Unix() - macEpochOffset
		query = base + fmt.Sprintf(" WHERE %s >= ? ORDER BY %s ASC, r.%s ASC LIMIT ?",
			dateExpr, dateExpr, quoteIdent(sc.pkCol))
		args = []interface{}{cutoff, limit}
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		s.invalidate()
		return nil, since, fmt.Errorf("query notifications: %w", err)
	}
	defer rows.Close()

	var out []Record
	for rows.Next() {
		var rec Record
		var uuidHex string
		var dateSecs float64
		var payload []byte
		if err := rows.Scan(&rec.RecID, &uuidHex, &rec.AppID, &dateSecs, &payload); err != nil {
			return nil, since, fmt.Errorf("scan notification row: %w", err)
		}
		rec.UUIDHex = strings.ToLower(strings.TrimSpace(uuidHex))
		if dateSecs > 0 {
			rec.DeliveredAt = time.Unix(int64(dateSecs)+macEpochOffset, 0).UTC()
		}
		rec.Payload = payload
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		s.invalidate()
		return nil, since, fmt.Errorf("read notifications: %w", err)
	}

	if n := len(out); n > 0 {
		last := out[n-1]
		since = Watermark{
			DeliveredDateSecs: last.DeliveredAt.Unix() - macEpochOffset,
			RecID:             last.RecID,
			Valid:             true,
		}
	}
	return out, since, nil
}

func uuidSel(sc schemaInfo) string {
	if sc.uuidCol == "" {
		return "''"
	}
	return fmt.Sprintf("COALESCE(lower(hex(r.%s)),'')", quoteIdent(sc.uuidCol))
}

// tccAccessHints returns the actionable Full Disk Access guidance shared by
// both Diagnose access-failure paths. macOS attributes the notibridge process
// to the application that launched it, not to the binary itself: launching
// from a terminal makes that terminal responsible, and launching from an IDE's
// built-in terminal makes the IDE responsible. TCC consults the responsible
// app's grant, so an entry for the binary (or for a different terminal) has no
// effect. It requires a full quit because TCC grants only take effect for
// newly launched processes.
func tccAccessHints() []string {
	return []string{
		"Grant Full Disk Access in System Settings → Privacy & Security → Full Disk Access to the app you launch from: your terminal app (Terminal, iTerm, …), or your IDE (GoLand, VS Code, …) when you run notibridge from its built-in terminal. Grant it to NotiBridge itself when you open the .app.",
		"macOS attributes the process to that launching app, not to the notibridge binary, so granting Full Disk Access to bin/notibridge (or to a different terminal than the one you are using) has no effect.",
		"Fully quit and reopen that app (closing its window is not enough; quit the IDE too) so the grant applies to a fresh process, then re-run diagnostics.",
		"Full Disk Access is required on Sequoia and later but is not guaranteed to be sufficient: Apple's database is undocumented and its schema may change in future releases.",
	}
}

// Diagnose implements Source.
func (s *MacOSSource) Diagnose(ctx context.Context) Diagnostics {
	s.mu.Lock()
	defer s.mu.Unlock()

	var d Diagnostics
	path, err := s.path()
	if err != nil {
		d.Error = err.Error()
		d.Hints = append(d.Hints, "No candidate database path could be determined for this system.")
		return d
	}
	d.Path = path

	if _, err := os.Stat(path); err != nil {
		switch {
		case errors.Is(err, fs.ErrPermission):
			d.Error = "permission denied while accessing the notification database"
			d.Hints = append(d.Hints, tccAccessHints()...)
		case errors.Is(err, fs.ErrNotExist):
			d.Error = "notification database not found at the expected location"
			d.Hints = append(d.Hints,
				"The database is created after the first notification is delivered on macOS 15 (Sequoia) and later. Older macOS versions store it under the per-user TemporaryItems directory (run: getconf DARWIN_USER_DIR).")
		default:
			d.Error = err.Error()
		}
		return d
	}

	if err := s.ensureOpen(); err != nil {
		d.Error = err.Error()
		if strings.Contains(err.Error(), "authorization") || strings.Contains(err.Error(), "not permitted") || strings.Contains(err.Error(), "unable to open") {
			d.Hints = append(d.Hints, tccAccessHints()...)
		}
		return d
	}
	d.Accessible = true
	d.Tables = s.schema.tables
	d.SchemaOK = true

	if err := s.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT COUNT(*) FROM record`)).Scan(&d.RowCount); err != nil {
		d.SchemaOK = false
		d.Error = fmt.Sprintf("schema discovered but record count failed: %v", err)
		return d
	}
	var oldest, newest sql.NullFloat64
	dateExpr := fmt.Sprintf("COALESCE(%s,0)", quoteIdent(s.schema.dateCol))
	_ = s.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT MIN(%s), MAX(%s) FROM record`, dateExpr, dateExpr)).Scan(&oldest, &newest)
	if oldest.Valid && oldest.Float64 > 0 {
		d.Oldest = time.Unix(int64(oldest.Float64)+macEpochOffset, 0).UTC()
	}
	if newest.Valid && newest.Float64 > 0 {
		d.Newest = time.Unix(int64(newest.Float64)+macEpochOffset, 0).UTC()
	}
	if d.RowCount == 0 {
		d.Hints = append(d.Hints, "The database is readable but contains no records yet; deliver a test notification and re-run diagnostics.")
	}
	return d
}
