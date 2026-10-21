// Package storage owns NotiBridge's private SQLite database. It stores
// delivery state and (for a bounded period) notification content needed for
// retries. It never touches Apple's Notification Center database.
package storage

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// DB is NotiBridge's application database.
type DB struct {
	sql *sql.DB
}

// schemaVersion is bumped whenever migrations changes; migrations are applied
// in order inside transactions and the resulting version is recorded.
const schemaVersion = 1

var migrations = []string{
	// v1: delivery queue and key/value metadata.
	`
	CREATE TABLE IF NOT EXISTS meta (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL DEFAULT ''
	);
	CREATE TABLE IF NOT EXISTS deliveries (
		id                 INTEGER PRIMARY KEY AUTOINCREMENT,
		source_key         TEXT    NOT NULL UNIQUE,
		app_id             TEXT    NOT NULL DEFAULT '',
		title              TEXT    NOT NULL DEFAULT '',
		subtitle           TEXT    NOT NULL DEFAULT '',
		body               TEXT    NOT NULL DEFAULT '',
		delivered_at       TEXT    NOT NULL DEFAULT '',
		status             TEXT    NOT NULL DEFAULT 'pending'
			CHECK (status IN ('pending','sent','failed')),
		attempts           INTEGER NOT NULL DEFAULT 0,
		next_attempt_at    INTEGER NOT NULL DEFAULT 0,
		last_attempt_at    INTEGER,
		telegram_message_id INTEGER,
		last_error         TEXT    NOT NULL DEFAULT '',
		created_at         INTEGER NOT NULL,
		sent_at            INTEGER
	);
	CREATE INDEX IF NOT EXISTS idx_deliveries_due  ON deliveries(status, next_attempt_at);
	CREATE INDEX IF NOT EXISTS idx_deliveries_time ON deliveries(created_at);
	`,
	// v2 placeholder: future migrations appended here.
}

// Open opens (creating if needed) the application database at path and
// applies migrations. The database is opened in WAL mode with a busy timeout
// so concurrent reads and writes are safe.
func Open(path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create application support directory: %w", err)
	}
	u := url.URL{Scheme: "file", Path: path}
	q := u.Query()
	q.Set("_pragma", "busy_timeout(5000)")
	q.Set("_pragma", "journal_mode(WAL)")
	q.Set("_pragma", "foreign_keys(1)")
	u.RawQuery = q.Encode()

	sqlDB, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, fmt.Errorf("open application database: %w", err)
	}
	// A single pooled connection serializes reads and writes from the
	// poller, delivery worker and UI goroutines. Throughput demands are
	// tiny, and this makes SQLITE_BUSY from concurrent in-process
	// connections impossible. The busy timeout still guards against
	// external interference (e.g. Spotlight or backup tools reading the
	// file mid-write).
	sqlDB.SetMaxOpenConns(1)
	if err := sqlDB.Ping(); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("open application database: %w", err)
	}
	db := &DB{sql: sqlDB}
	if err := db.migrate(context.Background()); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	return db, nil
}

func (db *DB) migrate(ctx context.Context) error {
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("create schema_version: %w", err)
	}
	var version int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0) FROM schema_version`).Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	for i := version; i < schemaVersion; i++ {
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			return fmt.Errorf("apply migration v%d: %w", i+1, err)
		}
	}
	if version < schemaVersion {
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_version (version) VALUES (?)`, schemaVersion); err != nil {
			return fmt.Errorf("record schema version: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration: %w", err)
	}
	return nil
}

// Close closes the underlying database handle.
func (db *DB) Close() error { return db.sql.Close() }

// SQL exposes the raw handle for package-internal queries.
func (db *DB) SQL() *sql.DB { return db.sql }

// MetaGet reads a value from the meta table; missing keys yield "".
func (db *DB) MetaGet(ctx context.Context, key string) (string, error) {
	var v string
	err := db.sql.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("meta read %q: %w", key, err)
	}
	return v, nil
}

// MetaSet upserts a value in the meta table.
func (db *DB) MetaSet(ctx context.Context, key, value string) error {
	_, err := db.sql.ExecContext(ctx,
		`INSERT INTO meta (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	if err != nil {
		return fmt.Errorf("meta write %q: %w", key, err)
	}
	return nil
}

// PurgeOlderThan deletes deliveries whose content is older than the given
// retention period. Sent and permanently failed deliveries are removed;
// pending deliveries are kept so retries survive.
func (db *DB) PurgeOlderThan(ctx context.Context, olderThan time.Time) (int64, error) {
	cutoff := olderThan.UTC().Format(time.RFC3339Nano)
	res, err := db.sql.ExecContext(ctx,
		`DELETE FROM deliveries WHERE status IN ('sent','failed') AND created_at < ?`,
		cutoff)
	if err != nil {
		return 0, fmt.Errorf("purge deliveries: %w", err)
	}
	return res.RowsAffected()
}

// ClearHistory removes all delivery records (sent, failed and pending).
func (db *DB) ClearHistory(ctx context.Context) (int64, error) {
	res, err := db.sql.ExecContext(ctx, `DELETE FROM deliveries`)
	if err != nil {
		return 0, fmt.Errorf("clear history: %w", err)
	}
	return res.RowsAffected()
}
