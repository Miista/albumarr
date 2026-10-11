// Package jsonfile keeps one value of type T in a JSON file: loaded once, held
// in memory behind a mutex, and rewritten whole, atomically (temp file, fsync,
// rename) with 0600 permissions, on every change. It is for the small amounts of
// state a household app has: settings, users, request records. A file that does
// not parse is an error and is never overwritten, so a bad edit cannot silently
// destroy what was in it.
package jsonfile

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// File holds a T and persists it to a path.
type File[T any] struct {
	mu    sync.RWMutex
	path  string
	value T
}

// Open loads the file, creating its directory (0700) and the file itself, with
// T's zero value, if none exists yet.
func Open[T any](path string) (*File[T], error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("creating data dir: %w", err)
	}
	f := &File[T]{path: path}
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(raw, &f.value); err != nil {
			return nil, fmt.Errorf("parsing %s: %w", path, err)
		}
	case os.IsNotExist(err):
		if err := f.saveLocked(); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	return f, nil
}

// View returns a deep copy of the current value, so a caller can never change
// what the file holds, or race with a writer, through a slice or map it was given.
func (f *File[T]) View() T {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return clone(f.value)
}

// Update runs fn with the write lock held, letting it change the value in place,
// then persists the result. If saving fails the in-memory value is rolled back,
// so memory never claims something the disk does not have.
func (f *File[T]) Update(fn func(*T)) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	before := clone(f.value)
	fn(&f.value)
	if err := f.saveLocked(); err != nil {
		f.value = before
		return err
	}
	return nil
}

// clone deep-copies through JSON, which is what the file holds anyway.
func clone[T any](v T) T {
	raw, err := json.Marshal(v)
	if err != nil {
		panic("jsonfile: value cannot be marshalled: " + err.Error())
	}
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		panic("jsonfile: value cannot be copied: " + err.Error())
	}
	return out
}

func (f *File[T]) saveLocked() error {
	raw, err := json.MarshalIndent(f.value, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling %s: %w", f.path, err)
	}
	tmp := f.path + ".tmp"
	w, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("writing %s: %w", f.path, err)
	}
	if _, err := w.Write(raw); err != nil {
		w.Close()
		return fmt.Errorf("writing %s: %w", f.path, err)
	}
	if err := w.Sync(); err != nil {
		w.Close()
		return fmt.Errorf("syncing %s: %w", f.path, err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", f.path, err)
	}
	if err := os.Rename(tmp, f.path); err != nil {
		return fmt.Errorf("renaming %s: %w", f.path, err)
	}
	return nil
}
