// Package lidarrmeta is a read-only client for Lidarr's hosted metadata
// service, the cached MusicBrainz mirror every Lidarr uses. One request returns
// an album with all of its editions and their tracks, from a Cloudflare cache,
// which is why albumreq asks it — not MusicBrainz — for everything except
// search. IDs are MusicBrainz release-group IDs, the same as everywhere else.
//
// The service has no documented API for third parties, so the base URL is
// configurable and callers treat a failure as "details unavailable".
package lidarrmeta

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Miista/albumreq/internal/musicbrainz"
)

const (
	// DefaultBase is Lidarr's public metadata service.
	DefaultBase = "https://api.lidarr.audio/api/v0.4"

	maxBody     = 8 << 20
	albumTTL    = 6 * time.Hour
	maxCacheLen = 64
)

// ErrNotFound means the service has no such album.
var ErrNotFound = errors.New("lidarrmeta: not found")

// ErrInvalidID is returned for anything that is not a lowercase UUID.
var ErrInvalidID = errors.New("lidarrmeta: invalid id")

// Client talks to the metadata service.
type Client struct {
	base string
	http *http.Client

	mu       sync.Mutex
	cache    map[string]cached
	inflight map[string]*call
}

type cached struct {
	album Album
	exp   time.Time
}

type call struct {
	done  chan struct{}
	album Album
	err   error
}

// New returns a Client for the service at base (DefaultBase when empty).
func New(base string) *Client {
	if base == "" {
		base = DefaultBase
	}
	return &Client{
		base:     strings.TrimRight(base, "/"),
		http:     &http.Client{Timeout: 20 * time.Second},
		cache:    map[string]cached{},
		inflight: map[string]*call{},
	}
}

// Album is one album with everything the detail view needs.
type Album struct {
	ID             string
	Title          string
	Artist         string
	ArtistID       string
	Type           string
	SecondaryTypes []string
	ReleaseDate    string
	Disambiguation string
	Genres         []string
	RatingValue    float64 // out of 10; 0 when unrated
	RatingVotes    int
	Overview       string
	Editions       []Edition
}

// Edition is one release of the album with its tracks.
type Edition struct {
	ID         string
	Title      string
	Date       string
	Country    string
	Label      string
	Format     string
	TrackCount int
	Tracks     []Track
}

// Track is one track of an edition.
type Track struct {
	Disc       int
	Number     string
	Position   int
	Title      string
	DurationMs int // 0 when unknown
}

// Album fetches one album by MusicBrainz release-group ID.
func (c *Client) Album(ctx context.Context, id string) (Album, error) {
	if !musicbrainz.ValidID(id) {
		return Album{}, ErrInvalidID
	}

	c.mu.Lock()
	if e, ok := c.cache[id]; ok && time.Now().Before(e.exp) {
		c.mu.Unlock()
		return e.album, nil
	}
	cl, ok := c.inflight[id]
	if !ok {
		cl = &call{done: make(chan struct{})}
		c.inflight[id] = cl
		// Detached from any one caller, so a caller that gives up does not
		// waste the answer for the others and for the cache.
		go c.run(context.WithoutCancel(ctx), id, cl)
	}
	c.mu.Unlock()

	select {
	case <-cl.done:
		return cl.album, cl.err
	case <-ctx.Done():
		return Album{}, ctx.Err()
	}
}

func (c *Client) run(ctx context.Context, id string, cl *call) {
	cl.album, cl.err = c.fetch(ctx, id)

	c.mu.Lock()
	delete(c.inflight, id)
	if cl.err == nil {
		if len(c.cache) >= maxCacheLen {
			now := time.Now()
			for k, e := range c.cache {
				if now.After(e.exp) {
					delete(c.cache, k)
				}
			}
			for k := range c.cache {
				if len(c.cache) < maxCacheLen {
					break
				}
				delete(c.cache, k)
			}
		}
		c.cache[id] = cached{album: cl.album, exp: time.Now().Add(albumTTL)}
	}
	c.mu.Unlock()
	close(cl.done)
}

func (c *Client) fetch(ctx context.Context, id string) (Album, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/album/"+id, nil)
	if err != nil {
		return Album{}, err
	}
	// No contact here: this service does not ask for one, so the operator's
	// contact goes to MusicBrainz only.
	req.Header.Set("User-Agent", musicbrainz.AnonymousUserAgent)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return Album{}, fmt.Errorf("metadata service request failed: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return Album{}, ErrNotFound
	default:
		return Album{}, fmt.Errorf("metadata service returned %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return Album{}, fmt.Errorf("reading metadata response: %w", err)
	}
	if len(body) > maxBody {
		return Album{}, fmt.Errorf("metadata response larger than %d bytes", maxBody)
	}
	return parse(body)
}

// wire is the service's JSON. Its keys are a mix of lower and Pascal case;
// encoding/json matches them case-insensitively.
type wire struct {
	ID             string   `json:"id"`
	Title          string   `json:"title"`
	Type           string   `json:"type"`
	SecondaryTypes []string `json:"secondarytypes"`
	ReleaseDate    string   `json:"releasedate"`
	Disambiguation string   `json:"disambiguation"`
	Genres         []string `json:"genres"`
	Overview       *string  `json:"overview"`
	Rating         struct {
		Count int     `json:"count"`
		Value float64 `json:"value"`
	} `json:"rating"`
	Artists []struct {
		Name string `json:"artistname"`
		ID   string `json:"id"`
	} `json:"artists"`
	Releases []struct {
		ID          string   `json:"id"`
		Title       string   `json:"title"`
		Status      string   `json:"status"`
		Country     []string `json:"country"`
		Label       []string `json:"label"`
		ReleaseDate string   `json:"releasedate"`
		TrackCount  int      `json:"trackcount"`
		Media       []struct {
			Format string `json:"format"`
		} `json:"media"`
		Tracks []struct {
			Name         string `json:"trackname"`
			Number       string `json:"tracknumber"`
			Position     int    `json:"trackposition"`
			DurationMs   *int   `json:"durationms"`
			MediumNumber int    `json:"mediumnumber"`
		} `json:"tracks"`
	} `json:"releases"`
}

func parse(body []byte) (Album, error) {
	var w wire
	if err := json.Unmarshal(body, &w); err != nil {
		return Album{}, fmt.Errorf("metadata service: decoding response: %w", err)
	}
	a := Album{
		ID: w.ID, Title: w.Title, Type: w.Type, SecondaryTypes: nonNil(w.SecondaryTypes),
		ReleaseDate: w.ReleaseDate, Disambiguation: w.Disambiguation, Genres: nonNil(w.Genres),
		RatingValue: w.Rating.Value, RatingVotes: w.Rating.Count,
	}
	if w.Overview != nil {
		a.Overview = *w.Overview
	}
	if len(w.Artists) > 0 {
		a.Artist, a.ArtistID = w.Artists[0].Name, w.Artists[0].ID
	}
	for _, r := range w.Releases {
		e := Edition{ID: r.ID, Title: r.Title, Date: r.ReleaseDate, TrackCount: r.TrackCount}
		if len(r.Country) > 0 {
			e.Country = r.Country[0]
		}
		if len(r.Label) > 0 {
			e.Label = r.Label[0]
		}
		seen := map[string]bool{}
		for _, m := range r.Media {
			if m.Format != "" && !seen[m.Format] {
				seen[m.Format] = true
				if e.Format != "" {
					e.Format += " + "
				}
				e.Format += m.Format
			}
		}
		for _, t := range r.Tracks {
			tr := Track{Disc: t.MediumNumber, Number: t.Number, Position: t.Position, Title: t.Name}
			if t.DurationMs != nil {
				tr.DurationMs = *t.DurationMs
			}
			e.Tracks = append(e.Tracks, tr)
		}
		a.Editions = append(a.Editions, e)
	}
	return a, nil
}

// PickEdition chooses the representative edition among those that have
// tracks: the one with the most common track count (ties go to the shorter
// list), and among those the earliest. ok is false when none has tracks.
func PickEdition(eds []Edition) (pick Edition, ok bool) {
	counts := map[int]int{}
	for _, e := range eds {
		if len(e.Tracks) > 0 {
			counts[len(e.Tracks)]++
		}
	}
	best := 0
	for n, c := range counts {
		if best == 0 || c > counts[best] || (c == counts[best] && n < best) {
			best = n
		}
	}
	for _, e := range eds {
		if len(e.Tracks) != best || best == 0 {
			continue
		}
		// Dates are ISO prefixes ("1997", "1997-05-21"); an empty date sorts last.
		if !ok || (e.Date != "" && (pick.Date == "" || e.Date < pick.Date)) {
			pick, ok = e, true
		}
	}
	return pick, ok
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
