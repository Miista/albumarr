package db

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func open(t *testing.T) (*DB, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "nested", "data")
	d, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d, dir
}

func TestOpenCreatesAnOwnerOnlyDatabaseAndMigrates(t *testing.T) {
	d, dir := open(t)
	if info, _ := os.Stat(filepath.Join(dir, File)); info.Mode().Perm() != 0o600 {
		t.Errorf("database is %o, want 600: it holds an API key", info.Mode().Perm())
	}
	if info, _ := os.Stat(dir); info.Mode().Perm() != 0o700 {
		t.Errorf("data dir is %o, want 700", info.Mode().Perm())
	}
	var v int
	d.QueryRow(`PRAGMA user_version`).Scan(&v)
	if v != len(migrations) {
		t.Errorf("schema version %d, want %d", v, len(migrations))
	}
	var mode string
	d.QueryRow(`PRAGMA journal_mode`).Scan(&mode)
	var fk int
	d.QueryRow(`PRAGMA foreign_keys`).Scan(&fk)
	if mode != "wal" || fk != 1 {
		t.Errorf("journal_mode=%q foreign_keys=%d, want wal and 1", mode, fk)
	}
}

func TestSidecarFilesAreOwnerOnlyToo(t *testing.T) {
	d, dir := open(t)
	d.Exec(`UPDATE lidarr SET api_key = 'secret' WHERE id = 1`) // leaves a -wal behind
	for _, suffix := range []string{"-wal", "-shm"} {
		if info, err := os.Stat(filepath.Join(dir, File+suffix)); err == nil && info.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s is %o: the write-ahead log holds the same secrets", suffix, info.Mode().Perm())
		}
	}
}

func TestReopeningKeepsDataAndDoesNotRemigrate(t *testing.T) {
	d, dir := open(t)
	d.Exec(`UPDATE lidarr SET url = 'http://lidarr:8686' WHERE id = 1`)
	d.Close()
	again, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	var url string
	again.QueryRow(`SELECT url FROM lidarr`).Scan(&url)
	if url != "http://lidarr:8686" {
		t.Errorf("url = %q after reopening", url)
	}
	var n int
	again.QueryRow(`SELECT COUNT(*) FROM lidarr`).Scan(&n)
	if n != 1 {
		t.Errorf("%d lidarr rows after reopening: the seed row must not be inserted twice", n)
	}
}

func TestADatabaseFromANewerAlbumreqIsRefused(t *testing.T) {
	d, dir := open(t)
	d.Exec(`PRAGMA user_version = 999`)
	d.Close()
	if _, err := Open(dir); err == nil || !strings.Contains(err.Error(), "newer albumreq") {
		t.Errorf("err = %v, want a refusal", err)
	}
}

func TestACorruptFileIsAnErrorAndIsKept(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, File)
	junk := []byte("this is not a database, it is a long line of text that SQLite will not accept as a header\n")
	os.WriteFile(path, junk, 0o600)
	d, err := Open(dir)
	if err == nil {
		d.Close()
		t.Fatal("a file that is not a database must be an error")
	}
	if raw, _ := os.ReadFile(path); string(raw) != string(junk) {
		t.Error("the file was overwritten")
	}
}

func TestOnlyOneLidarrRowCanExist(t *testing.T) {
	d, _ := open(t)
	if _, err := d.Exec(`INSERT INTO lidarr (id) VALUES (2)`); err == nil {
		t.Error("a second connection row would make 'the' settings ambiguous")
	}
}

func TestBackupIsAConsistentOwnerOnlyCopy(t *testing.T) {
	d, dir := open(t)
	d.Exec(`UPDATE lidarr SET url = 'http://lidarr:8686', api_key = 'k' WHERE id = 1`)
	dest := filepath.Join(dir, "backup", "albumreq.db")
	if err := d.Backup(context.Background(), dest); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(dest); info.Mode().Perm() != 0o600 {
		t.Errorf("backup is %o, want 600", info.Mode().Perm())
	}
	if _, err := os.Stat(dest + ".tmp"); !os.IsNotExist(err) {
		t.Error("temp file left behind")
	}
	// The copy is a database in its own right, with the data in it.
	cp, err := sql.Open("sqlite", "file:"+dest+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer cp.Close()
	var url, key string
	if err := cp.QueryRow(`SELECT url, api_key FROM lidarr`).Scan(&url, &key); err != nil || url != "http://lidarr:8686" || key != "k" {
		t.Errorf("backup holds %q %q: %v", url, key, err)
	}
	// A second run replaces the first.
	d.Exec(`UPDATE lidarr SET url = 'http://other:1' WHERE id = 1`)
	if err := d.Backup(context.Background(), dest); err != nil {
		t.Fatal(err)
	}
	cp2, _ := sql.Open("sqlite", "file:"+dest+"?mode=ro")
	defer cp2.Close()
	cp2.QueryRow(`SELECT url FROM lidarr`).Scan(&url)
	if url != "http://other:1" {
		t.Errorf("second backup holds %q", url)
	}
}

func TestBackupPathWithAQuoteIsNotAnInjection(t *testing.T) {
	d, dir := open(t)
	dest := filepath.Join(dir, "it's a backup.db")
	if err := d.Backup(context.Background(), dest); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dest); err != nil {
		t.Errorf("a path with a quote should just work: %v", err)
	}
	var n int
	d.QueryRow(`SELECT COUNT(*) FROM lidarr`).Scan(&n)
	if n != 1 {
		t.Error("the database was changed")
	}
}

func TestConcurrentReadsAndWritesDoNotFailWithLocked(t *testing.T) {
	d, _ := open(t)
	var wg sync.WaitGroup
	errs := make(chan error, 100)
	for i := 0; i < 25; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if _, err := d.Exec(`UPDATE lidarr SET quality_profile_id = quality_profile_id + 1 WHERE id = 1`); err != nil {
				errs <- err
			}
		}()
		go func() {
			defer wg.Done()
			var n int
			if err := d.QueryRow(`SELECT quality_profile_id FROM lidarr`).Scan(&n); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	var n int
	d.QueryRow(`SELECT quality_profile_id FROM lidarr`).Scan(&n)
	if n != 25 {
		t.Errorf("%d of 25 increments kept", n)
	}
}

func TestMigratingToSchema2KeepsHistoryAndClosesWhatCouldNeverBeApproved(t *testing.T) {
	// A database as the first release left it: schema 1, with requests in every state.
	dir := t.TempDir()
	raw, err := sql.Open("sqlite", "file:"+filepath.Join(dir, File))
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		migrations[0],
		`UPDATE lidarr SET url = 'http://lidarr:8686', api_key = 'secret', root_folder = '/music', quality_profile_id = 5 WHERE id = 1`,
		`INSERT INTO users (sub, name, approver, first_seen, last_seen) VALUES ('s1', 'anna', 1, 100, 200)`,
		`INSERT INTO requests (album_id, title, artist, state, requester_sub, requester_name, created_at) VALUES ('a-pending', 'Waiting', 'X', 'pending', 's1', 'anna', 300)`,
		`INSERT INTO requests (album_id, title, artist, state, requester_sub, requester_name, decided_by_sub, decided_by_name, decided_at, lidarr_result, created_at) VALUES ('a-done', 'Done', 'Y', 'approved', 's1', 'anna', 's2', 'bob', 400, 'added', 350)`,
		`INSERT INTO requests (album_id, title, artist, state, requester_sub, requester_name, decided_by_sub, decided_by_name, decided_at, decline_reason, created_at) VALUES ('a-no', 'No', 'Z', 'declined', 's1', 'anna', 's2', 'bob', 450, 'not now', 360)`,
		`PRAGMA user_version = 1`,
	} {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatalf("%v: %.60s", err, stmt)
		}
	}
	raw.Close()

	d, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var v int
	d.QueryRow(`PRAGMA user_version`).Scan(&v)
	if v != len(migrations) || v < 2 {
		t.Errorf("schema %d, want %d", v, len(migrations))
	}

	type row struct{ album, state, artistID, by, reason string }
	got := map[string]row{}
	rows, _ := d.Query(`SELECT album_id, state, artist_id, decided_by_name, decline_reason FROM requests`)
	for rows.Next() {
		var r row
		rows.Scan(&r.album, &r.state, &r.artistID, &r.by, &r.reason)
		got[r.album] = r
	}
	rows.Close()
	if r := got["a-done"]; r.state != "approved" || r.by != "bob" || r.artistID != "" {
		t.Errorf("an approved request must be left alone, with an empty artist: %+v", r)
	}
	if r := got["a-no"]; r.state != "declined" || r.reason != "not now" || r.by != "bob" {
		t.Errorf("a declined request must be left alone: %+v", r)
	}
	if r := got["a-pending"]; r.state != "declined" || r.by != "albumreq" || !strings.Contains(r.reason, "request it again") {
		t.Errorf("a pending request that could never be approved is closed, saying why: %+v", r)
	}
	var decidedAt int64
	d.QueryRow(`SELECT decided_at FROM requests WHERE album_id = 'a-pending'`).Scan(&decidedAt)
	if decidedAt < 1_700_000_000 {
		t.Errorf("the closing time is %d: it must be a real time, not 0 (the schema requires one)", decidedAt)
	}
	var pending int
	d.QueryRow(`SELECT COUNT(*) FROM requests WHERE state = 'pending'`).Scan(&pending)
	if pending != 0 {
		t.Errorf("%d pending requests remain", pending)
	}
	// The rest of what was there is intact.
	var url, key string
	var approver bool
	d.QueryRow(`SELECT url, api_key FROM lidarr`).Scan(&url, &key)
	d.QueryRow(`SELECT approver FROM users WHERE sub = 's1'`).Scan(&approver)
	if url != "http://lidarr:8686" || key != "secret" || !approver {
		t.Errorf("settings or users were lost: %q %q %v", url, key, approver)
	}
	// And a new request can now be made for the album that was closed.
	if _, err := d.Exec(`INSERT INTO requests (album_id, artist_id, title, artist, state, requester_sub, requester_name, created_at) VALUES ('a-pending', 'art', 'Waiting', 'X', 'pending', 's1', 'anna', 500)`); err != nil {
		t.Errorf("a closed request must not block asking again: %v", err)
	}
	// Opening again is a no-op: the migration must not run twice.
	d.Close()
	again, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	again.Close()
}

func TestAFreshDatabaseHasTheArtistColumn(t *testing.T) {
	d, _ := open(t)
	if _, err := d.Exec(`INSERT INTO requests (album_id, artist_id, title, artist, state, requester_sub, requester_name, created_at) VALUES ('x', 'y', 't', 'a', 'pending', 's', 'n', 1)`); err != nil {
		t.Errorf("%v", err)
	}
}
