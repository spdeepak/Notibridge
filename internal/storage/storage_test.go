package storage

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func openTemp(t *testing.T) (*DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, path
}

func TestOpenCreatesSchema(t *testing.T) {
	db, _ := openTemp(t)
	var n int
	if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM deliveries`).Scan(&n); err != nil {
		t.Fatalf("deliveries table missing: %v", err)
	}
	if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM meta`).Scan(&n); err != nil {
		t.Fatalf("meta table missing: %v", err)
	}
	var version int
	if err := db.SQL().QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil {
		t.Fatalf("schema_version missing: %v", err)
	}
	if version != schemaVersion {
		t.Errorf("schema version = %d, want %d", version, schemaVersion)
	}
}

func TestMigrationsAreIdempotent(t *testing.T) {
	db, path := openTemp(t)
	if err := db.MetaSet(context.Background(), "k", "v"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()
	v, err := db2.MetaGet(context.Background(), "k")
	if err != nil || v != "v" {
		t.Errorf("meta lost after reopen: %q %v", v, err)
	}
	var n int
	if err := db2.SQL().QueryRow(`SELECT COUNT(*) FROM schema_version`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("schema_version rows = %d, want 1 (migrations must not re-apply)", n)
	}
}

func TestMetaRoundTrip(t *testing.T) {
	db, _ := openTemp(t)
	ctx := context.Background()
	if v, err := db.MetaGet(ctx, "missing"); err != nil || v != "" {
		t.Errorf("missing key = %q, %v; want empty, nil", v, err)
	}
	if err := db.MetaSet(ctx, "k", "v1"); err != nil {
		t.Fatal(err)
	}
	if err := db.MetaSet(ctx, "k", "v2"); err != nil {
		t.Fatal(err)
	}
	if v, _ := db.MetaGet(ctx, "k"); v != "v2" {
		t.Errorf("upsert failed: %q", v)
	}
}

func TestPurgeAndClear(t *testing.T) {
	db, _ := openTemp(t)
	ctx := context.Background()
	insert := func(key, status string, created time.Time) {
		_, err := db.SQL().Exec(`INSERT INTO deliveries (source_key, status, created_at) VALUES (?, ?, ?)`,
			key, status, created.UTC().Format(time.RFC3339Nano))
		if err != nil {
			t.Fatal(err)
		}
	}
	insert("old-sent", "sent", time.Now().AddDate(0, 0, -30))
	insert("new-sent", "sent", time.Now())
	insert("old-pending", "pending", time.Now().AddDate(0, 0, -30))

	n, err := db.PurgeOlderThan(ctx, time.Now().AddDate(0, 0, -14))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("purged %d, want 1 (pending rows must survive)", n)
	}
	// Remaining: new-sent + old-pending
	var count int
	db.SQL().QueryRow(`SELECT COUNT(*) FROM deliveries`).Scan(&count)
	if count != 2 {
		t.Errorf("remaining rows = %d, want 2", count)
	}

	n, err = db.ClearHistory(ctx)
	if err != nil || n != 2 {
		t.Errorf("ClearHistory = %d, %v; want 2, nil", n, err)
	}
}
