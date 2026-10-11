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

func TestMigratingAnExistingDeploymentKeepsItsData(t *testing.T) {
	// A database as the previous release left it: schema 2, with data, including the
	// metadata profile column that schema 3 drops.
	dir := t.TempDir()
	path := filepath.Join(dir, File)
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range append(append([]string{}, migrations[:2]...),
		`UPDATE lidarr SET url = 'http://lidarr:8686', api_key = 'secret', root_folder = '/music', quality_profile_id = 5, metadata_profile_id = 7 WHERE id = 1`,
		`INSERT INTO users (sub, name, email, approver, first_seen, last_seen) VALUES ('s1', 'anna', 'a@example.com', 1, 100, 200)`,
		`INSERT INTO requests (album_id, title, artist, state, requester_sub, requester_name, created_at) VALUES ('x', 'OK Computer', 'Radiohead', 'pending', 's1', 'anna', 300)`,
		`PRAGMA user_version = 2`) {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatalf("%v: %s", err, stmt)
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
	if v != len(migrations) || v < 3 {
		t.Errorf("schema %d after opening, want %d", v, len(migrations))
	}
	var url, key, root string
	var qp int
	if err := d.QueryRow(`SELECT url, api_key, root_folder, quality_profile_id FROM lidarr`).Scan(&url, &key, &root, &qp); err != nil || url != "http://lidarr:8686" || key != "secret" || root != "/music" || qp != 5 {
		t.Errorf("settings after the upgrade: %q %q %q %d (%v)", url, key, root, qp, err)
	}
	if _, err := d.Exec(`UPDATE lidarr SET metadata_profile_id = 1`); err == nil {
		t.Error("the metadata profile column should be gone")
	}
	var name string
	var approver bool
	if err := d.QueryRow(`SELECT name, approver FROM users WHERE sub = 's1'`).Scan(&name, &approver); err != nil || name != "anna" || !approver {
		t.Errorf("users after the upgrade: %v %v %v", name, approver, err)
	}
	var title string
	if err := d.QueryRow(`SELECT title FROM requests`).Scan(&title); err != nil || title != "OK Computer" {
		t.Errorf("requests after the upgrade: %q %v", title, err)
	}
	// Opening it again is a no-op.
	d.Close()
	again, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	again.Close()
}
