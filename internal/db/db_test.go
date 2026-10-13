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

func TestADatabaseFromANewerAlbumarrIsRefused(t *testing.T) {
	d, dir := open(t)
	d.Exec(`PRAGMA user_version = 999`)
	d.Close()
	if _, err := Open(dir); err == nil || !strings.Contains(err.Error(), "newer albumarr") {
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
	dest := filepath.Join(dir, "backup", "albumarr.db")
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
