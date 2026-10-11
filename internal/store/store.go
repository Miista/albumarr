// Package store persists albumreq's user-configurable settings to a single
// JSON file under a data directory, the same shape as reaparr's store. All
// writes go through Update, which rewrites the whole file atomically
// (temp file, fsync, rename) with 0600 permissions, since it holds an API key.
//
// Environment variables never land here: the store holds only what was entered
// in the settings page (see internal/settings for the override rules).
package store

import (
	"path/filepath"

	"github.com/Miista/albumreq/internal/jsonfile"
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

// Store holds State in dataDir/config.json. View and Update are those of
// jsonfile.File: atomic, 0600, rolled back in memory if a save fails.
type Store struct {
	*jsonfile.File[State]
}

// Open loads state from dataDir/config.json, creating the directory and an
// empty document if none exists yet.
func Open(dataDir string) (*Store, error) {
	f, err := jsonfile.Open[State](filepath.Join(dataDir, "config.json"))
	if err != nil {
		return nil, err
	}
	return &Store{f}, nil
}
