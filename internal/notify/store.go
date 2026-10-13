package notify

import (
	"database/sql"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/Miista/albumarr/internal/db"
)

// How the ntfy server is told who is publishing.
const (
	AuthNone  = "none"
	AuthBasic = "basic"
	AuthToken = "token"
)

// Config is the ntfy agent as stored: the server, the shared channel, how to
// authenticate, and which events the shared channel is sent.
type Config struct {
	Enabled bool
	// EnabledSince is when notifications were first switched on (Unix seconds). The
	// "available" check ignores requests decided before it, so enabling never replays
	// the backlog.
	EnabledSince int64
	URL          string // the ntfy server's root, e.g. https://ntfy.example.com
	Topic        string // the shared channel: approvers subscribe to it
	Tags         string // comma-separated emoji short codes
	Auth         string // AuthNone, AuthBasic or AuthToken
	Username     string
	Password     string // secret
	Token        string // secret
	Priority     int    // 1 (min) to 5 (urgent); 3 is ntfy's default
	EmbedPoster  bool   // attach the album cover
	Types        []Type // the events the shared channel is sent
	AppURL       string // where albumarr is reached, for links in messages
}

func (c Config) clone() Config {
	c.Types = append([]Type(nil), c.Types...)
	return c
}

// Prefs is what one person asked for: a topic on the same ntfy server, and which of
// the events about their own requests to send there.
type Prefs struct {
	Sub       string
	Topic     string
	Types     []Type
	UpdatedAt time.Time
}

// ErrTopicTaken means another person already has that topic.
var ErrTopicTaken = errors.New("notify: that topic is already someone else's")

// Store keeps the ntfy settings and everyone's own topics in the database. Reads of
// the settings come from a copy in memory that is replaced only after a write
// succeeds, so they never fail and never disagree with the disk.
type Store struct {
	db  *db.DB
	now func() time.Time

	mu  sync.RWMutex
	cfg Config
}

// NewStore loads the settings.
func NewStore(d *db.DB) (*Store, error) {
	s := &Store{db: d, now: time.Now}
	var c Config
	var types string
	err := d.QueryRow(`SELECT enabled, enabled_since, url, topic, tags, auth, username, password, token, priority, embed_poster, types, app_url FROM ntfy WHERE id = 1`).
		Scan(&c.Enabled, &c.EnabledSince, &c.URL, &c.Topic, &c.Tags, &c.Auth, &c.Username, &c.Password, &c.Token, &c.Priority, &c.EmbedPoster, &types, &c.AppURL)
	if err != nil {
		return nil, err
	}
	c.Types = decodeTypes(types)
	s.cfg = c
	return s, nil
}

// Config returns a copy of the stored settings.
func (s *Store) Config() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg.clone()
}

// Update runs fn on a copy of the settings, saves the result, and only then makes it
// current. Switching notifications on for the first time stamps EnabledSince.
func (s *Store) Update(fn func(*Config)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.cfg.clone()
	fn(&next)
	if next.Enabled && next.EnabledSince == 0 {
		next.EnabledSince = s.now().Unix()
	}
	if _, err := s.db.Exec(`UPDATE ntfy SET enabled = ?, enabled_since = ?, url = ?, topic = ?, tags = ?, auth = ?, username = ?, password = ?, token = ?, priority = ?, embed_poster = ?, types = ?, app_url = ? WHERE id = 1`,
		next.Enabled, next.EnabledSince, next.URL, next.Topic, next.Tags, next.Auth, next.Username, next.Password, next.Token, next.Priority, next.EmbedPoster, encodeTypes(next.Types), next.AppURL); err != nil {
		return err
	}
	s.cfg = next
	return nil
}

// Prefs returns a person's own settings, if they have set any.
func (s *Store) Prefs(sub string) (Prefs, bool, error) {
	var p Prefs
	var types string
	var at int64
	err := s.db.QueryRow(`SELECT sub, topic, types, updated_at FROM user_notifications WHERE sub = ?`, sub).Scan(&p.Sub, &p.Topic, &types, &at)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Prefs{}, false, nil
	case err != nil:
		return Prefs{}, false, err
	}
	p.Types, p.UpdatedAt = decodeTypes(types), time.Unix(at, 0).UTC()
	return p, true, nil
}

// SetPrefs saves a person's topic and events. A topic belongs to one person, so
// asking for one that is taken is ErrTopicTaken.
func (s *Store) SetPrefs(sub, topic string, types []Type) error {
	_, err := s.db.Exec(`INSERT INTO user_notifications (sub, topic, types, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(sub) DO UPDATE SET topic = excluded.topic, types = excluded.types, updated_at = excluded.updated_at`,
		sub, topic, encodeTypes(types), s.now().Unix())
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return ErrTopicTaken
	}
	return err
}

// DeletePrefs removes a person's settings: they hear nothing about their own requests.
func (s *Store) DeletePrefs(sub string) error {
	_, err := s.db.Exec(`DELETE FROM user_notifications WHERE sub = ?`, sub)
	return err
}
