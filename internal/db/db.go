// Package db opens albumreq's one SQLite database, albumreq.db in the data
// directory, and keeps its schema up to date. Settings, users and requests all
// live in it, so there is one file to back up and one place their consistency
// is decided.
//
// The driver is modernc.org/sqlite, pure Go, because albumreq is cross-compiled
// with CGO disabled into a distroless static image.
package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// File is the database's name inside the data directory.
const File = "albumreq.db"

// DB is the open database.
type DB struct {
	*sql.DB
	path string
}

// Open opens (creating if need be) dataDir/albumreq.db and migrates it to the
// current schema. The directory is 0700 and the file 0600: it holds the Lidarr
// API key. A database written by a newer albumreq is refused rather than
// guessed at.
func Open(dataDir string) (*DB, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("creating data dir: %w", err)
	}
	path := filepath.Join(dataDir, File)
	// SQLite creates a file with the process's default mode, which is group and
	// world readable under a common umask. Create it ourselves, owner only; an
	// empty file is a valid new database. Its -wal and -shm files copy its mode.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("creating %s: %w", path, err)
	}
	f.Close()
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, fmt.Errorf("securing %s: %w", path, err)
	}

	// WAL lets reads proceed during a write; foreign keys are off in SQLite unless
	// asked for; busy_timeout waits instead of failing when the file is locked.
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=synchronous(FULL)"
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	// One connection: every read and write is serialised, so a transaction that
	// reads and then writes can never fail with "database is locked" because
	// another writer got in between. At this scale nothing needs more.
	sqlDB.SetMaxOpenConns(1)

	d := &DB{DB: sqlDB, path: path}
	if err := d.migrate(); err != nil {
		sqlDB.Close()
		return nil, err
	}
	return d, nil
}

// Path is the database file.
func (d *DB) Path() string { return d.path }

// migrations are applied in order, each in its own transaction, and recorded in
// PRAGMA user_version. Never edit one that has shipped: add the next.
var migrations = []string{
	// 1: the Lidarr connection (one row), and everyone who has signed in.
	`CREATE TABLE lidarr (
		id                  INTEGER PRIMARY KEY CHECK (id = 1),
		url                 TEXT    NOT NULL DEFAULT '',
		api_key             TEXT    NOT NULL DEFAULT '',
		root_folder         TEXT    NOT NULL DEFAULT '',
		quality_profile_id  INTEGER NOT NULL DEFAULT 0,
		metadata_profile_id INTEGER NOT NULL DEFAULT 0
	);
	INSERT INTO lidarr (id) VALUES (1);
	CREATE TABLE users (
		sub        TEXT    PRIMARY KEY,
		name       TEXT    NOT NULL,
		email      TEXT    NOT NULL DEFAULT '',
		local      INTEGER NOT NULL DEFAULT 0,
		approver   INTEGER NOT NULL DEFAULT 0,
		first_seen INTEGER NOT NULL,
		last_seen  INTEGER NOT NULL
	);`,

	// 2: request records. Declined and approved ones stay as history; only a
	// pending one is "open", and the partial unique index lets exactly one album
	// be pending at a time, so two clicks or two people at once cannot make two.
	// Approved records do not block a new request: if the album has since left
	// Lidarr it can be asked for again, and Lidarr's own state, not this table,
	// says whether it is wanted.
	`CREATE TABLE requests (
		id             INTEGER PRIMARY KEY AUTOINCREMENT,
		album_id       TEXT    NOT NULL,
		title          TEXT    NOT NULL,
		artist         TEXT    NOT NULL,
		requester_sub  TEXT    NOT NULL,
		requester_name TEXT    NOT NULL,
		state          TEXT    NOT NULL CHECK (state IN ('pending', 'approved', 'declined')),
		decided_by_sub  TEXT   NOT NULL DEFAULT '',
		decided_by_name TEXT   NOT NULL DEFAULT '',
		decided_at     INTEGER NOT NULL DEFAULT 0,
		auto           INTEGER NOT NULL DEFAULT 0,
		decline_reason TEXT    NOT NULL DEFAULT '',
		lidarr_result  TEXT    NOT NULL DEFAULT '',
		created_at     INTEGER NOT NULL,
		CHECK (state = 'pending' OR decided_at > 0)
	);
	CREATE UNIQUE INDEX requests_one_pending ON requests (album_id) WHERE state = 'pending';
	CREATE INDEX requests_by_state     ON requests (state, created_at);
	CREATE INDEX requests_by_requester ON requests (requester_sub, created_at);`,

	// 3: the metadata profile is not a setting any more. Every requested artist gets
	// the profile named "None", looked up in Lidarr when it is needed, so a request
	// adds only the album asked for.
	`ALTER TABLE lidarr DROP COLUMN metadata_profile_id;`,
}

func (d *DB) migrate() error {
	var have int
	if err := d.QueryRow(`PRAGMA user_version`).Scan(&have); err != nil {
		return fmt.Errorf("reading the schema version: %w", err)
	}
	if have > len(migrations) {
		return fmt.Errorf("%s is from a newer albumreq (schema %d, this one knows %d): refusing to open it", d.path, have, len(migrations))
	}
	for i := have; i < len(migrations); i++ {
		tx, err := d.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migrating to schema %d: %w", i+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			tx.Rollback()
			return fmt.Errorf("recording schema %d: %w", i+1, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("migrating to schema %d: %w", i+1, err)
		}
	}
	return nil
}

// Backup writes a consistent copy of the database to dest, atomically: the
// copy is made next to dest and renamed into place, so a reader (the backup
// job) sees the old copy or the new one, never half of one. Copying the live
// file instead can capture a database and its write-ahead log from two moments.
// The copy is 0600, as it holds the same secrets.
func (d *DB) Backup(ctx context.Context, dest string) error {
	if strings.ContainsRune(dest, 0) {
		return errors.New("db: bad backup path")
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return err
	}
	tmp := dest + ".tmp"
	if err := os.Remove(tmp); err != nil && !os.IsNotExist(err) {
		return err
	}
	// VACUUM INTO needs a target that does not exist; the path is quoted as an
	// SQL string, and ' is doubled, because it cannot be a bound parameter.
	if _, err := d.ExecContext(ctx, `VACUUM INTO '`+strings.ReplaceAll(tmp, `'`, `''`)+`'`); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("backing up: %w", err)
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dest); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// Close closes the database, folding the write-ahead log back into the file.
func (d *DB) Close() error { return d.DB.Close() }
