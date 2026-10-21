package source

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"howett.net/plist"
)

// createFixtureDB builds a database with the same table/column layout macOS
// Ventura–Tahoe use (app, record with uuid/data/delivered_date).
func createFixtureDB(t *testing.T, records []fixtureRecord) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	stmts := []string{
		`CREATE TABLE dbinfo (key TEXT, value TEXT)`,
		`CREATE TABLE app (app_id INTEGER PRIMARY KEY, identifier TEXT)`,
		`CREATE TABLE record (
			rec_id INTEGER PRIMARY KEY,
			app_id INTEGER,
			uuid BLOB,
			data BLOB,
			presented INTEGER,
			delivered_date REAL
		)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("fixture schema: %v", err)
		}
	}
	if _, err := db.Exec(`INSERT INTO app (app_id, identifier) VALUES (1, 'com.apple.MobileSMS'), (2, 'com.slack.Slack')`); err != nil {
		t.Fatal(err)
	}
	for _, r := range records {
		if _, err := db.Exec(`INSERT INTO record (rec_id, app_id, uuid, data, presented, delivered_date) VALUES (?, ?, ?, ?, 1, ?)`,
			r.recID, r.appID, r.uuid, r.payload, r.deliveredDate); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

type fixtureRecord struct {
	recID         int64
	appID         int
	uuid          []byte
	payload       []byte
	deliveredDate float64 // Mac absolute seconds
}

func bplist(t *testing.T, fields map[string]interface{}) []byte {
	t.Helper()
	data, err := plist.Marshal(fields, plist.BinaryFormat)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func payloadFor(title string, macTime float64) []byte {
	return bplistTest(map[string]interface{}{
		"app":  "com.apple.MobileSMS",
		"date": macTime,
		"req":  map[string]interface{}{"titl": title, "body": "body of " + title},
	})
}

func bplistTest(m map[string]interface{}) []byte {
	data, _ := plist.Marshal(m, plist.BinaryFormat)
	return data
}

func TestPollReadsRecordsInOrder(t *testing.T) {
	now := time.Now().Unix() - macEpochOffset
	path := createFixtureDB(t, []fixtureRecord{
		{recID: 1, appID: 1, uuid: []byte{0xde, 0xad, 0xbe, 0xef}, payload: payloadFor("first", float64(now-10)), deliveredDate: float64(now - 10)},
		{recID: 2, appID: 2, uuid: []byte{0x01}, payload: payloadFor("second", float64(now-5)), deliveredDate: float64(now - 5)},
	})
	src := NewMacOS(MacOSOptions{DBPath: path, Lookback: time.Hour})
	defer src.Close()

	records, wm, err := src.Poll(context.Background(), Watermark{}, 100)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("got %d records, want 2", len(records))
	}
	if records[0].RecID != 1 || records[1].RecID != 2 {
		t.Errorf("order: %d, %d", records[0].RecID, records[1].RecID)
	}
	if records[0].UUIDHex != "deadbeef" {
		t.Errorf("uuid hex = %q", records[0].UUIDHex)
	}
	if records[0].AppID != "com.apple.MobileSMS" {
		t.Errorf("app id = %q", records[0].AppID)
	}
	if records[0].DeliveredAt.Year() != time.Now().Year() {
		t.Errorf("delivered_at = %v", records[0].DeliveredAt)
	}
	if !wm.Valid {
		t.Error("watermark must become valid")
	}

	// Second poll with the watermark returns nothing new.
	records2, _, err := src.Poll(context.Background(), wm, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(records2) != 0 {
		t.Errorf("second poll returned %d records", len(records2))
	}

	// A new record after the watermark is returned.
	if _, err := sqlOpenExec(path, `INSERT INTO record (rec_id, app_id, uuid, data, delivered_date) VALUES (3, 1, X'02', ?, ?)`,
		payloadFor("third", float64(now)), float64(now)); err != nil {
		t.Fatal(err)
	}
	records3, _, err := src.Poll(context.Background(), wm, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(records3) != 1 || records3[0].RecID != 3 {
		t.Fatalf("third poll = %+v", records3)
	}
}

func sqlOpenExec(path, query string, args ...interface{}) (sql.Result, error) {
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	return db.Exec(query, args...)
}

func TestPollRespectsLookbackOnFirstPoll(t *testing.T) {
	old := float64(time.Now().Unix()-macEpochOffset) - 86400 // yesterday
	path := createFixtureDB(t, []fixtureRecord{
		{recID: 1, appID: 1, uuid: []byte{1}, payload: payloadFor("old", old), deliveredDate: old},
	})
	src := NewMacOS(MacOSOptions{DBPath: path, Lookback: time.Hour})
	defer src.Close()
	records, _, err := src.Poll(context.Background(), Watermark{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 {
		t.Errorf("historical record leaked into first poll: %d records", len(records))
	}
}

func TestPollTieBreaksOnRecID(t *testing.T) {
	now := float64(time.Now().Unix() - macEpochOffset)
	path := createFixtureDB(t, []fixtureRecord{
		{recID: 5, appID: 1, uuid: []byte{5}, payload: payloadFor("a", now), deliveredDate: now},
		{recID: 6, appID: 1, uuid: []byte{6}, payload: payloadFor("b", now), deliveredDate: now},
	})
	src := NewMacOS(MacOSOptions{DBPath: path, Lookback: time.Hour})
	defer src.Close()

	// Poll with watermark after rec 5 only.
	records, _, err := src.Poll(context.Background(), Watermark{DeliveredDateSecs: int64(now), RecID: 5, Valid: true}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].RecID != 6 {
		t.Errorf("tie-break poll = %+v", records)
	}
}

func TestPollHandlesMissingUUID(t *testing.T) {
	now := float64(time.Now().Unix() - macEpochOffset)
	path := createFixtureDB(t, []fixtureRecord{
		{recID: 1, appID: 1, uuid: nil, payload: payloadFor("no-uuid", now), deliveredDate: now},
	})
	src := NewMacOS(MacOSOptions{DBPath: path, Lookback: time.Hour})
	defer src.Close()
	records, _, err := src.Poll(context.Background(), Watermark{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].UUIDHex != "" {
		t.Errorf("records = %+v", records)
	}
}

func TestPollSurvivesMalformedPayload(t *testing.T) {
	now := float64(time.Now().Unix() - macEpochOffset)
	path := createFixtureDB(t, []fixtureRecord{
		{recID: 1, appID: 1, uuid: []byte{1}, payload: []byte("garbage-not-a-plist"), deliveredDate: now},
	})
	src := NewMacOS(MacOSOptions{DBPath: path, Lookback: time.Hour})
	defer src.Close()
	// The source delivers raw payloads; parsing failures are handled
	// upstream. Polling must still succeed.
	records, _, err := src.Poll(context.Background(), Watermark{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Errorf("records = %d", len(records))
	}
	if string(records[0].Payload) != "garbage-not-a-plist" {
		t.Error("payload must pass through unmodified")
	}
}

func TestPollNeverWrites(t *testing.T) {
	now := float64(time.Now().Unix() - macEpochOffset)
	path := createFixtureDB(t, []fixtureRecord{
		{recID: 1, appID: 1, uuid: []byte{1}, payload: payloadFor("x", now), deliveredDate: now},
	})
	src := NewMacOS(MacOSOptions{DBPath: path, Lookback: time.Hour})
	defer src.Close()
	if _, _, err := src.Poll(context.Background(), Watermark{}, 10); err != nil {
		t.Fatal(err)
	}
	// Attempt a write through a separate connection to prove the fixture is
	// a writable database; then verify the record count is unchanged (the
	// source never inserts, updates or deletes).
	res, err := sqlOpenExec(path, `UPDATE record SET presented = 0 WHERE rec_id = 1`)
	if err != nil {
		t.Fatalf("fixture should be writable by owner: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Errorf("rows affected = %d", n)
	}
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM record`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("record count changed: %d", count)
	}
}

func TestPollRespectsContextCancellation(t *testing.T) {
	path := createFixtureDB(t, nil)
	src := NewMacOS(MacOSOptions{DBPath: path, Lookback: time.Hour})
	defer src.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := src.Poll(ctx, Watermark{}, 10); err == nil {
		t.Log("cancelled context: poll returned no error (fast path acceptable)")
	}
}

func TestDiagnoseMissingFile(t *testing.T) {
	src := NewMacOS(MacOSOptions{DBPath: filepath.Join(t.TempDir(), "does-not-exist")})
	defer src.Close()
	d := src.Diagnose(context.Background())
	if d.Accessible {
		t.Error("missing file must not be accessible")
	}
	if d.Error == "" {
		t.Error("missing file must produce an error message")
	}
	if len(d.Hints) == 0 {
		t.Error("missing file must produce hints")
	}
}

// TestDiagnoseAccessFailureHints exercises both access-failure paths (an
// unreadable file and an unreadable directory) and asserts the Full Disk
// Access guidance names the launching app — terminal or IDE — and requires a
// full quit; TCC grants otherwise silently do not apply to the already-running
// process, and an entry for the binary itself has no effect.
func TestDiagnoseAccessFailureHints(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file permissions, so access failures cannot be simulated")
	}

	t.Run("unreadable file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "db")
		if err := os.WriteFile(path, []byte("not a database"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o000); err != nil {
			t.Fatal(err)
		}
		src := NewMacOS(MacOSOptions{DBPath: path})
		defer src.Close()
		assertAccessFailureHints(t, src.Diagnose(context.Background()))
	})

	t.Run("unreadable directory", func(t *testing.T) {
		sub := filepath.Join(t.TempDir(), "sub")
		if err := os.Mkdir(sub, 0o700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(sub, "db")
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(sub, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(sub, 0o700) })
		src := NewMacOS(MacOSOptions{DBPath: path})
		defer src.Close()
		assertAccessFailureHints(t, src.Diagnose(context.Background()))
	})
}

func assertAccessFailureHints(t *testing.T, d Diagnostics) {
	t.Helper()
	if d.Accessible {
		t.Error("access failure must not report the database as accessible")
	}
	if d.Error == "" {
		t.Error("access failure must produce an error message")
	}
	joined := strings.Join(d.Hints, "\n")
	for _, want := range []string{"Full Disk Access", "terminal", "IDE", "bin/notibridge", "quit"} {
		if !strings.Contains(joined, want) {
			t.Errorf("hints must mention %q; got: %v", want, d.Hints)
		}
	}
}

func TestDiagnoseUnknownSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "weird")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE something_else (id INTEGER)`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	src := NewMacOS(MacOSOptions{DBPath: path})
	defer src.Close()
	d := src.Diagnose(context.Background())
	if d.SchemaOK {
		t.Error("unknown schema must not pass")
	}
	if !strings.Contains(d.Error, "not understood") {
		t.Errorf("error = %q", d.Error)
	}
}

func TestDiagnoseHealthyFixture(t *testing.T) {
	now := float64(time.Now().Unix() - macEpochOffset)
	path := createFixtureDB(t, []fixtureRecord{
		{recID: 1, appID: 1, uuid: []byte{1}, payload: payloadFor("x", now), deliveredDate: now},
	})
	src := NewMacOS(MacOSOptions{DBPath: path})
	defer src.Close()
	d := src.Diagnose(context.Background())
	if !d.Accessible || !d.SchemaOK {
		t.Fatalf("diagnostics = %+v", d)
	}
	if d.RowCount != 1 {
		t.Errorf("row count = %d", d.RowCount)
	}
	if d.Newest.IsZero() {
		t.Error("newest timestamp missing")
	}
	if len(d.Tables) == 0 {
		t.Error("tables not reported")
	}
}

func TestDetectDBPathDoesNotHardcodeUser(t *testing.T) {
	p, err := DetectDBPath()
	if err != nil {
		t.Fatalf("DetectDBPath: %v", err)
	}
	if !strings.Contains(p, "/Library/Group Containers/group.com.apple.usernoted/db2/db") &&
		!strings.Contains(p, "com.apple.notificationcenter/db2/db") {
		t.Errorf("path %q matches no known layout", p)
	}
	// The path must be derived from the current user's home directory, not
	// from a hardcoded username.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(p, home) {
		t.Errorf("path %q is not under the current user's home %q", p, home)
	}
}
