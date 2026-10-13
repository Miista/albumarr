// Package store persists albumarr's user-configurable settings in the database
// (the lidarr table, one row). Reads come from a copy kept in memory, loaded at
// start and replaced only after a write succeeds, so a read can never fail or
// disagree with the disk; all writes go through Update.
//
// Environment variables never land here: the store holds only what was entered
// in the settings page (see internal/settings for the override rules).
package store

import (
	"sync"

	"github.com/Miista/albumarr/internal/db"
)

// Lidarr is everything albumarr needs to know about your Lidarr.
type Lidarr struct {
	URL    string `json:"url"`
	APIKey string `json:"api_key"`
	// Requesting is on when RootFolder and QualityProfileID are set: where a
	// requested artist goes, and which quality profile it gets. The metadata
	// profile is not here: it is always the one named "None".
	RootFolder       string `json:"root_folder"`
	QualityProfileID int    `json:"quality_profile_id"`
	// WebURL is where people open Lidarr in a browser (optional): it only makes
	// the admin's "Open in Lidarr" link.
	WebURL string `json:"web_url"`
}

// State is the full persisted document.
type State struct {
	Lidarr Lidarr `json:"lidarr"`
}

// Store guards State with a mutex and persists it to the database.
type Store struct {
	db    *db.DB
	mu    sync.RWMutex
	state State
}

// New loads the settings from the database.
func New(d *db.DB) (*Store, error) {
	s := &Store{db: d}
	l := &s.state.Lidarr
	err := d.QueryRow(`SELECT url, api_key, root_folder, quality_profile_id, web_url FROM lidarr WHERE id = 1`).
		Scan(&l.URL, &l.APIKey, &l.RootFolder, &l.QualityProfileID, &l.WebURL)
	if err != nil {
		return nil, err
	}
	return s, nil
}

// View returns a copy of the current state.
func (s *Store) View() State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state
}

// Update runs fn on a copy of the state under the write lock, saves the result,
// and only then makes it the current state. If saving fails nothing changed, so
// memory never claims something the database does not have.
func (s *Store) Update(fn func(*State)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.state
	fn(&next)
	l := next.Lidarr
	if _, err := s.db.Exec(
		`UPDATE lidarr SET url = ?, api_key = ?, root_folder = ?, quality_profile_id = ?, web_url = ? WHERE id = 1`,
		l.URL, l.APIKey, l.RootFolder, l.QualityProfileID, l.WebURL); err != nil {
		return err
	}
	s.state = next
	return nil
}
