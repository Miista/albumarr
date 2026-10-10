// Package musicbrainz is a small read-only client for the MusicBrainz web
// service (WS/2): release-group search, a release group's details, the list
// of its editions, and one edition's track list.
//
// It follows MusicBrainz's usage policy: a descriptive User-Agent with a
// contact, at most one request per second (requests are queued, not dropped),
// and aggressive caching. Every ID that reaches an upstream URL is validated
// as a UUID first.
package musicbrainz

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultBase is the public MusicBrainz web service.
	DefaultBase = "https://musicbrainz.org/ws/2"

	version     = "0.1"
	maxBody     = 8 << 20
	minGap      = 1100 * time.Millisecond // MusicBrainz asks for an average of 1 request/second
	searchTTL   = 10 * time.Minute
	detailTTL   = 24 * time.Hour
	maxCacheLen = 512
)

// ErrNotFound is returned when MusicBrainz has no such entity.
var ErrNotFound = errors.New("musicbrainz: not found")

// ErrInvalidID is returned for anything that is not a lowercase UUID.
var ErrInvalidID = errors.New("musicbrainz: invalid id")

var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// ValidID reports whether id is a lowercase UUID, the only form of ID that is
// ever put into an upstream URL.
func ValidID(id string) bool { return uuidRE.MatchString(id) }

// Client talks to MusicBrainz.
type Client struct {
	base string
	ua   string
	http *http.Client
	gap  time.Duration

	mu       sync.Mutex
	next     time.Time // earliest start of the next upstream request
	cache    map[string]cacheEntry
	inflight map[string]*call
}

type cacheEntry struct {
	body []byte
	exp  time.Time
}

type call struct {
	done chan struct{}
	body []byte
	err  error
}

// New returns a Client for the public MusicBrainz. contact goes into the
// User-Agent as MusicBrainz requires (an email address or a project URL).
func New(contact string) (*Client, error) {
	return NewWithBase(DefaultBase, contact, minGap)
}

// NewWithBase is New with an explicit base URL and request gap, for tests.
func NewWithBase(base, contact string, gap time.Duration) (*Client, error) {
	ua, err := UserAgent(contact)
	if err != nil {
		return nil, err
	}
	return &Client{
		base:     strings.TrimRight(base, "/"),
		ua:       ua,
		http:     &http.Client{Timeout: 25 * time.Second},
		gap:      gap,
		cache:    map[string]cacheEntry{},
		inflight: map[string]*call{},
	}, nil
}

// UserAgent builds the User-Agent MusicBrainz (and the Cover Art Archive)
// ask for: application name and version, then a contact in parentheses. The
// contact is something the operator chooses to disclose — an email address or
// a project URL — so there is deliberately no default.
func UserAgent(contact string) (string, error) {
	contact = strings.TrimSpace(contact)
	if contact == "" || len(contact) > 200 || strings.ContainsAny(contact, "\r\n()") {
		return "", errors.New("musicbrainz: contact must be 1-200 characters, without line breaks or parentheses")
	}
	return fmt.Sprintf("albumreq/%s ( %s )", version, contact), nil
}

// Hit is one release-group search result.
type Hit struct {
	ID               string
	Title            string
	Artist           string
	ArtistID         string
	Type             string
	SecondaryTypes   []string
	FirstReleaseDate string
	Score            int
}

// Search finds release groups (albums, EPs, singles, …) matching term.
func (c *Client) Search(ctx context.Context, term string, limit int) ([]Hit, error) {
	q := url.Values{"query": {escapeLucene(term)}, "limit": {fmt.Sprint(limit)}}
	var resp struct {
		Groups []struct {
			ID             string   `json:"id"`
			Title          string   `json:"title"`
			PrimaryType    string   `json:"primary-type"`
			SecondaryTypes []string `json:"secondary-types"`
			FirstRelease   string   `json:"first-release-date"`
			Score          int      `json:"score"`
			ArtistCredit   []credit `json:"artist-credit"`
		} `json:"release-groups"`
	}
	if err := c.get(ctx, "/release-group", q, searchTTL, &resp); err != nil {
		return nil, err
	}
	hits := make([]Hit, 0, len(resp.Groups))
	for _, g := range resp.Groups {
		artist, artistID := creditName(g.ArtistCredit)
		hits = append(hits, Hit{
			ID: g.ID, Title: g.Title, Artist: artist, ArtistID: artistID,
			Type: g.PrimaryType, SecondaryTypes: nonNil(g.SecondaryTypes),
			FirstReleaseDate: g.FirstRelease, Score: g.Score,
		})
	}
	return hits, nil
}

// ReleaseGroup is one album's details.
type ReleaseGroup struct {
	ID               string
	Title            string
	Artist           string
	ArtistID         string
	Type             string
	SecondaryTypes   []string
	FirstReleaseDate string
	Disambiguation   string
	Genres           []string
	RatingValue      float64 // 0 when unrated
	RatingVotes      int
}

// ReleaseGroup looks one release group up.
func (c *Client) ReleaseGroup(ctx context.Context, id string) (ReleaseGroup, error) {
	if !ValidID(id) {
		return ReleaseGroup{}, ErrInvalidID
	}
	var resp struct {
		ID             string   `json:"id"`
		Title          string   `json:"title"`
		PrimaryType    string   `json:"primary-type"`
		SecondaryTypes []string `json:"secondary-types"`
		FirstRelease   string   `json:"first-release-date"`
		Disambiguation string   `json:"disambiguation"`
		ArtistCredit   []credit `json:"artist-credit"`
		Genres         []struct {
			Name  string `json:"name"`
			Count int    `json:"count"`
		} `json:"genres"`
		Rating struct {
			Value *float64 `json:"value"`
			Votes int      `json:"votes-count"`
		} `json:"rating"`
	}
	q := url.Values{"inc": {"artist-credits genres ratings"}}
	if err := c.get(ctx, "/release-group/"+id, q, detailTTL, &resp); err != nil {
		return ReleaseGroup{}, err
	}
	rg := ReleaseGroup{
		ID: resp.ID, Title: resp.Title, Type: resp.PrimaryType, SecondaryTypes: nonNil(resp.SecondaryTypes),
		FirstReleaseDate: resp.FirstRelease, Disambiguation: resp.Disambiguation, RatingVotes: resp.Rating.Votes,
	}
	rg.Artist, rg.ArtistID = creditName(resp.ArtistCredit)
	if resp.Rating.Value != nil {
		rg.RatingValue = *resp.Rating.Value
	}
	genres := resp.Genres
	// Most-voted genres first, at most six.
	for i := 1; i < len(genres); i++ {
		for j := i; j > 0 && genres[j].Count > genres[j-1].Count; j-- {
			genres[j], genres[j-1] = genres[j-1], genres[j]
		}
	}
	for _, g := range genres {
		if len(rg.Genres) == 6 {
			break
		}
		rg.Genres = append(rg.Genres, g.Name)
	}
	return rg, nil
}

// Edition is one release of a release group, reduced to what picking a
// representative edition needs.
type Edition struct {
	ID         string
	Date       string
	Country    string
	Format     string
	TrackCount int
}

// Editions lists a release group's official releases (or all of them when it
// has no official one). MusicBrainz can take several seconds to answer this
// for groups with many editions.
func (c *Client) Editions(ctx context.Context, id string) ([]Edition, error) {
	if !ValidID(id) {
		return nil, ErrInvalidID
	}
	eds, err := c.editions(ctx, id, true)
	if err == nil && len(eds) == 0 {
		eds, err = c.editions(ctx, id, false)
	}
	return eds, err
}

func (c *Client) editions(ctx context.Context, id string, officialOnly bool) ([]Edition, error) {
	q := url.Values{"release-group": {id}, "inc": {"media"}, "limit": {"100"}}
	if officialOnly {
		q.Set("status", "official")
	}
	var resp struct {
		Releases []struct {
			ID      string `json:"id"`
			Date    string `json:"date"`
			Country string `json:"country"`
			Media   []struct {
				Format     string `json:"format"`
				TrackCount int    `json:"track-count"`
			} `json:"media"`
		} `json:"releases"`
	}
	if err := c.get(ctx, "/release", q, detailTTL, &resp); err != nil {
		return nil, err
	}
	eds := make([]Edition, 0, len(resp.Releases))
	for _, r := range resp.Releases {
		e := Edition{ID: r.ID, Date: r.Date, Country: r.Country}
		formats := map[string]bool{}
		for _, m := range r.Media {
			e.TrackCount += m.TrackCount
			if m.Format != "" && !formats[m.Format] {
				formats[m.Format] = true
				if e.Format != "" {
					e.Format += " + "
				}
				e.Format += m.Format
			}
		}
		if e.TrackCount > 0 {
			eds = append(eds, e)
		}
	}
	return eds, nil
}

// PickEdition chooses the representative edition: the one with the most
// common track count (ties go to the shorter list), and among those the
// earliest. ok is false when there are no editions.
func PickEdition(eds []Edition) (pick Edition, ok bool) {
	if len(eds) == 0 {
		return Edition{}, false
	}
	counts := map[int]int{}
	for _, e := range eds {
		counts[e.TrackCount]++
	}
	best := 0
	for n, c := range counts {
		if best == 0 || c > counts[best] || (c == counts[best] && n < best) {
			best = n
		}
	}
	for _, e := range eds {
		if e.TrackCount != best {
			continue
		}
		// Dates are ISO prefixes ("1997", "1997-05-21"); an empty date sorts last.
		if !ok || (e.Date != "" && (pick.Date == "" || e.Date < pick.Date)) {
			pick, ok = e, true
		}
	}
	return pick, ok
}

// Track is one track of an edition.
type Track struct {
	Disc       int
	Number     string
	Title      string
	DurationMs int // 0 when unknown
}

// Tracks returns an edition's track list in order.
func (c *Client) Tracks(ctx context.Context, releaseID string) ([]Track, error) {
	if !ValidID(releaseID) {
		return nil, ErrInvalidID
	}
	var resp struct {
		Media []struct {
			Position int `json:"position"`
			Tracks   []struct {
				Number string `json:"number"`
				Title  string `json:"title"`
				Length *int   `json:"length"`
			} `json:"tracks"`
		} `json:"media"`
	}
	if err := c.get(ctx, "/release/"+releaseID, url.Values{"inc": {"recordings"}}, detailTTL, &resp); err != nil {
		return nil, err
	}
	var out []Track
	for _, m := range resp.Media {
		for _, t := range m.Tracks {
			tr := Track{Disc: m.Position, Number: t.Number, Title: t.Title}
			if t.Length != nil {
				tr.DurationMs = *t.Length
			}
			out = append(out, tr)
		}
	}
	return out, nil
}

type credit struct {
	Name       string `json:"name"`
	JoinPhrase string `json:"joinphrase"`
	Artist     struct {
		ID string `json:"id"`
	} `json:"artist"`
}

// creditName renders an artist credit ("A feat. B") and returns the first
// credited artist's ID.
func creditName(cs []credit) (name, firstID string) {
	for i, c := range cs {
		if i == 0 {
			firstID = c.Artist.ID
		}
		name += c.Name + c.JoinPhrase
	}
	return strings.TrimSpace(name), firstID
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// luceneSpecial are the characters MusicBrainz's search syntax treats as
// operators; a user's text is searched literally, not interpreted.
const luceneSpecial = `+-&|!(){}[]^"~*?:\/`

func escapeLucene(s string) string {
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune(luceneSpecial, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// get fetches path+query as JSON into out, through the cache, the in-flight
// de-duplication and the one-request-per-second queue.
func (c *Client) get(ctx context.Context, path string, q url.Values, ttl time.Duration, out any) error {
	q = cloneValues(q)
	q.Set("fmt", "json")
	key := path + "?" + q.Encode()

	body, err := c.cached(ctx, key, ttl)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("musicbrainz %s: decoding response: %w", path, err)
	}
	return nil
}

func (c *Client) cached(ctx context.Context, key string, ttl time.Duration) ([]byte, error) {
	c.mu.Lock()
	if e, ok := c.cache[key]; ok && time.Now().Before(e.exp) {
		c.mu.Unlock()
		return e.body, nil
	}
	cl, ok := c.inflight[key]
	if !ok {
		cl = &call{done: make(chan struct{})}
		c.inflight[key] = cl
		// The upstream request runs detached from any one caller: others may
		// be waiting on the same answer, and it ends up in the cache. Every
		// caller, this one included, can still walk away on its own context.
		go c.run(context.WithoutCancel(ctx), key, ttl, cl)
	}
	c.mu.Unlock()

	select {
	case <-cl.done:
		return cl.body, cl.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *Client) run(ctx context.Context, key string, ttl time.Duration, cl *call) {
	cl.body, cl.err = c.fetch(ctx, key)

	c.mu.Lock()
	delete(c.inflight, key)
	if cl.err == nil {
		c.store(key, cl.body, ttl)
	}
	c.mu.Unlock()
	close(cl.done)
}

// store adds an entry, evicting expired entries first and then an arbitrary
// one when the cache is full. Callers hold c.mu.
func (c *Client) store(key string, body []byte, ttl time.Duration) {
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
	c.cache[key] = cacheEntry{body: body, exp: time.Now().Add(ttl)}
}

func (c *Client) fetch(ctx context.Context, key string) ([]byte, error) {
	if err := c.wait(ctx); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+key, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.ua)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, fmt.Errorf("musicbrainz request failed: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, ErrNotFound
	default:
		return nil, fmt.Errorf("musicbrainz returned %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, fmt.Errorf("reading musicbrainz response: %w", err)
	}
	if len(body) > maxBody {
		return nil, fmt.Errorf("musicbrainz response larger than %d bytes", maxBody)
	}
	return body, nil
}

// wait reserves the next upstream slot and sleeps until it starts, so
// concurrent callers are spread out rather than bursting.
func (c *Client) wait(ctx context.Context) error {
	c.mu.Lock()
	at := c.next
	if now := time.Now(); at.Before(now) {
		at = now
	}
	c.next = at.Add(c.gap)
	c.mu.Unlock()

	d := time.Until(at)
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func cloneValues(q url.Values) url.Values {
	out := make(url.Values, len(q)+1)
	for k, v := range q {
		out[k] = append([]string(nil), v...)
	}
	return out
}
