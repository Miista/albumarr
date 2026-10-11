// Package requests keeps the record of who asked for which album and what was
// decided, and runs the approval flow around it.
//
// A request is pending until an approver approves or declines it. Approving is the
// only thing that writes to Lidarr (the add, monitor and search in the catalog),
// so a plain user's request costs nothing until someone says yes. A declined
// request is not final: the album can be requested again, and the old decision
// stays as history.
package requests

import (
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/Miista/albumreq/internal/db"
)

// State of a request.
type State string

const (
	Pending  State = "pending"
	Approved State = "approved"
	Declined State = "declined"
)

// Person is someone named in a record: who asked, who decided.
type Person struct {
	Sub  string `json:"sub"`
	Name string `json:"name"`
}

// Record is one request.
type Record struct {
	ID       int64  `json:"id"`
	AlbumID  string `json:"albumId"`  // MusicBrainz release-group ID
	ArtistID string `json:"artistId"` // the artist's MusicBrainz ID, as the album's card carried it
	Title    string `json:"title"`
	Artist   string `json:"artist"`
	State    State  `json:"state"`

	Requester     Person     `json:"requester"`
	DecidedBy     *Person    `json:"decidedBy,omitempty"`
	DecidedAt     *time.Time `json:"decidedAt,omitempty"`
	AutoApproved  bool       `json:"autoApproved,omitempty"` // the requester could approve it, and did
	DeclineReason string     `json:"declineReason,omitempty"`
	LidarrResult  string     `json:"lidarrResult,omitempty"` // what Lidarr did when it was approved
	CreatedAt     time.Time  `json:"createdAt"`
}

var (
	// ErrNotFound means there is no such request.
	ErrNotFound = errors.New("requests: no such request")
	// ErrNotPending means the request was already decided (or is being).
	ErrNotPending = errors.New("requests: the request is not pending")
)

const columns = `id, album_id, artist_id, title, artist, state, requester_sub, requester_name,
	decided_by_sub, decided_by_name, decided_at, auto, decline_reason, lidarr_result, created_at`

// Store is the requests table.
type Store struct {
	db  *db.DB
	now func() time.Time
}

// NewStore returns a Store on the database.
func NewStore(d *db.DB) *Store { return &Store{db: d, now: time.Now} }

func scan(row interface{ Scan(...any) error }) (Record, error) {
	var r Record
	var decidedBySub, decidedByName string
	var decidedAt, createdAt int64
	if err := row.Scan(&r.ID, &r.AlbumID, &r.ArtistID, &r.Title, &r.Artist, &r.State, &r.Requester.Sub, &r.Requester.Name,
		&decidedBySub, &decidedByName, &decidedAt, &r.AutoApproved, &r.DeclineReason, &r.LidarrResult, &createdAt); err != nil {
		return Record{}, err
	}
	r.CreatedAt = time.Unix(createdAt, 0).UTC()
	if decidedAt > 0 {
		t := time.Unix(decidedAt, 0).UTC()
		r.DecidedAt = &t
		r.DecidedBy = &Person{Sub: decidedBySub, Name: decidedByName}
	}
	return r, nil
}

// Get returns one request.
func (s *Store) Get(id int64) (Record, error) {
	r, err := scan(s.db.QueryRow(`SELECT `+columns+` FROM requests WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, ErrNotFound
	}
	return r, err
}

// PendingFor returns the pending request for an album, if there is one.
func (s *Store) PendingFor(albumID string) (Record, bool, error) {
	r, err := scan(s.db.QueryRow(`SELECT `+columns+` FROM requests WHERE album_id = ? AND state = 'pending'`, albumID))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Record{}, false, nil
	case err != nil:
		return Record{}, false, err
	}
	return r, true, nil
}

// Pending returns the pending requests among the albums, by album ID. It is how
// search results and the album sheet learn that something is waiting.
func (s *Store) Pending(albumIDs []string) (map[string]Record, error) {
	out := map[string]Record{}
	if len(albumIDs) == 0 {
		return out, nil
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(albumIDs)), ",")
	args := make([]any, len(albumIDs))
	for i, id := range albumIDs {
		args[i] = id
	}
	rows, err := s.db.Query(`SELECT `+columns+` FROM requests WHERE state = 'pending' AND album_id IN (`+marks+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		r, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out[r.AlbumID] = r
	}
	return out, rows.Err()
}

// CreatePending adds a pending request. If one is already pending for the album
// (the unique index says so, atomically) it returns that one and created=false:
// there is never a second.
func (s *Store) CreatePending(albumID, artistID, title, artist string, by Person) (rec Record, created bool, err error) {
	res, err := s.db.Exec(`INSERT INTO requests (album_id, artist_id, title, artist, state, requester_sub, requester_name, created_at)
		VALUES (?, ?, ?, ?, 'pending', ?, ?, ?)
		ON CONFLICT DO NOTHING`, albumID, artistID, clip(title, maxText), clip(artist, maxText), by.Sub, clip(by.Name, maxText), s.now().Unix())
	if err != nil {
		return Record{}, false, err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		id, err := res.LastInsertId()
		if err != nil {
			return Record{}, false, err
		}
		rec, err = s.Get(id)
		return rec, true, err
	}
	rec, found, err := s.PendingFor(albumID)
	if err == nil && !found {
		err = errors.New("requests: a request conflicted but none is pending")
	}
	return rec, false, err
}

// Approve marks a pending request approved. It fails with ErrNotPending if it was
// decided in the meantime, so two approvers cannot both win.
func (s *Store) Approve(id int64, by Person, lidarrResult string, auto bool) (Record, error) {
	res, err := s.db.Exec(`UPDATE requests SET state = 'approved', decided_by_sub = ?, decided_by_name = ?, decided_at = ?, auto = ?, lidarr_result = ?
		WHERE id = ? AND state = 'pending'`, by.Sub, clip(by.Name, maxText), s.now().Unix(), auto, lidarrResult, id)
	return s.decided(res, err, id)
}

// Decline marks a pending request declined, with the reason an approver gave.
func (s *Store) Decline(id int64, by Person, reason string) (Record, error) {
	res, err := s.db.Exec(`UPDATE requests SET state = 'declined', decided_by_sub = ?, decided_by_name = ?, decided_at = ?, decline_reason = ?
		WHERE id = ? AND state = 'pending'`, by.Sub, clip(by.Name, maxText), s.now().Unix(), CleanReason(reason), id)
	return s.decided(res, err, id)
}

func (s *Store) decided(res sql.Result, err error, id int64) (Record, error) {
	if err != nil {
		return Record{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, err := s.Get(id); err != nil {
			return Record{}, err // ErrNotFound
		}
		return Record{}, ErrNotPending
	}
	return s.Get(id)
}

// Filter selects requests to list.
type Filter struct {
	State        State  // empty: any
	RequesterSub string // empty: anyone
}

// MaxList is the most requests one list returns: a household's history fits, and
// a page that grows without bound would not.
const MaxList = 200

// List returns requests, newest first, or oldest first for the pending queue
// (the one that has waited longest is first in line).
func (s *Store) List(f Filter) ([]Record, error) {
	q := `SELECT ` + columns + ` FROM requests WHERE 1 = 1`
	var args []any
	if f.State != "" {
		q += ` AND state = ?`
		args = append(args, string(f.State))
	}
	if f.RequesterSub != "" {
		q += ` AND requester_sub = ?`
		args = append(args, f.RequesterSub)
	}
	if f.State == Pending {
		q += ` ORDER BY created_at ASC, id ASC`
	} else {
		q += ` ORDER BY created_at DESC, id DESC`
	}
	q += ` LIMIT ` + strconv.Itoa(MaxList)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Record{}
	for rows.Next() {
		r, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

const (
	maxText   = 300 // a title, an artist or a name
	maxReason = 500
)

// CleanReason trims a decline reason, drops control characters and cuts it to a
// sensible length. It is stored and shown as plain text.
func CleanReason(s string) string { return clip(s, maxReason) }

// clip drops control characters and cuts to max runes.
func clip(s string, max int) string {
	s = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' {
			return -1
		}
		return r
	}, s))
	if r := []rune(s); len(r) > max {
		s = string(r[:max])
	}
	return s
}
