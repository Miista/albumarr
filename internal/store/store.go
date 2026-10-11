// Package store persists albumreq's user-configurable settings to a single
// JSON file under a data directory, the same shape as reaparr's store. All
// writes go through Update, which rewrites the whole file atomically
// (temp file, fsync, rename) with 0600 permissions, since it holds an API key.
//
// Environment variables never land here: the store holds only what was entered
// in the settings page (see internal/settings for the override rules).
package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Lidarr is everything albumreq needs to know about your Lidarr.
type Lidarr struct {
	URL    string `json:"url"`
	APIKey string `json:"api_key"`
	// Requesting is on when RootFolder and both profile IDs are set: where a
	// requested artist goes, and which profiles it gets.
	RootFolder        string `json:"root_folder"`
	QualityProfileID  int    `json:"quality_profile_id"`
	MetadataProfileID int    `json:"metadata_profile_id"`
}

// State is the full persisted document.
type State struct {
	Lidarr Lidarr `json:"lidarr"`
}

// Store guards State with a mutex and persists it to dataDir/config.json.
type Store struct {
	mu    sync.RWMutex
	path  string
	state State
}

// Open loads state from dataDir/config.json, creating the directory and an
// empty document if none exists yet.
func Open(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("creating data dir: %w", err)
	}
	s := &Store{path: filepath.Join(dataDir, "config.json")}
	raw, err := os.ReadFile(s.path)
	switch {
	case err == nil:
		if err := json.Unmarshal(raw, &s.state); err != nil {
			return nil, fmt.Errorf("parsing %s: %w", s.path, err)
		}
	case os.IsNotExist(err):
		if err := s.saveLocked(); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("reading %s: %w", s.path, err)
	}
	return s, nil
}

// View returns a copy of the current state.
func (s *Store) View() State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state
}

// Update runs fn with the write lock held, letting it change state in place,
// then persists the result. If saving fails the in-memory state is rolled
// back, so memory never claims something the disk does not have.
func (s *Store) Update(fn func(*State)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	before := s.state
	fn(&s.state)
	if err := s.saveLocked(); err != nil {
		s.state = before
		return err
	}
	return nil
}

// saveLocked writes state to disk. The caller holds s.mu.
func (s *Store) saveLocked() error {
	raw, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling state: %w", err)
	}
	tmp := s.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("writing state file: %w", err)
	}
	if _, err := f.Write(raw); err != nil {
		f.Close()
		return fmt.Errorf("writing state file: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("syncing state file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("closing state file: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("renaming state file: %w", err)
	}
	return nil
}
