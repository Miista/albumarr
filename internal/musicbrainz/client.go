// Package musicbrainz is a small read-only client for MusicBrainz's release-
// group search — the one thing albumarr asks MusicBrainz itself, because its
// ranking is the best available. Album details, track lists and covers come
// from Lidarr's metadata service instead (see package lidarrmeta).
//
// It follows MusicBrainz's usage policy: a User-Agent with a contact the
// operator chooses, at most one request per second (requests are queued, not
// dropped), and caching. Every ID that reaches an upstream URL elsewhere in
// albumarr is validated with ValidID.
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
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultBase is the public MusicBrainz web service.
	DefaultBase = "https://musicbrainz.org/ws/2"

	version       = "0.1"
	maxBody       = 8 << 20
	minGap        = 1100 * time.Millisecond // MusicBrainz asks for an average of 1 request/second
	searchTTL     = 10 * time.Minute
	maxCacheLen   = 512
	retryDelay    = 2 * time.Second
	maxRetryAfter = 5 * time.Second // MusicBrainz may ask for longer; a person is waiting
)

var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// ValidID reports whether id is a lowercase UUID, the only form of ID that is
// ever put into an upstream URL.
func ValidID(id string) bool { return uuidRE.MatchString(id) }

// UserAgent builds the User-Agent MusicBrainz asks for: application name and
// version, then a contact in parentheses. The contact is something the
// operator chooses to disclose — an email address or a project URL — so there
// is deliberately no default.
func UserAgent(contact string) (string, error) {
	contact = strings.TrimSpace(contact)
	if contact == "" || len(contact) > 200 || strings.ContainsAny(contact, "\r\n()") {
		return "", errors.New("musicbrainz: contact must be 1-200 characters, without line breaks or parentheses")
	}
	return fmt.Sprintf("%s ( %s )", AnonymousUserAgent, contact), nil
}

// AnonymousUserAgent identifies albumarr without any contact. It is what goes
// to services that do not ask for one, so the operator's contact is only ever
// sent to MusicBrainz.
const AnonymousUserAgent = "albumarr/" + version

// Client talks to MusicBrainz.
type Client struct {
	base string
	ua   string
	http *http.Client
	gap  time.Duration
	// retryDelay is how long to wait before the one retry of a request that failed
	// for a reason that may pass (see fetch).
	retryDelay time.Duration

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
// User-Agent as MusicBrainz requires.
func New(contact string) (*Client, error) {
	return NewWithBase(DefaultBase, contact, minGap)
}

// NewAt is New for a MusicBrainz at another base URL, such as a self-hosted
// mirror. The one-request-per-second queue stays: it is harmless there.
func NewAt(base, contact string) (*Client, error) {
	return NewWithBase(base, contact, minGap)
}

// NewWithBase is New with an explicit base URL and request gap, for tests.
func NewWithBase(base, contact string, gap time.Duration) (*Client, error) {
	ua, err := UserAgent(contact)
	if err != nil {
		return nil, err
	}
	return &Client{
		base:       strings.TrimRight(base, "/"),
		ua:         ua,
		http:       &http.Client{Timeout: 25 * time.Second},
		gap:        gap,
		retryDelay: retryDelay,
		cache:      map[string]cacheEntry{},
		inflight:   map[string]*call{},
	}, nil
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

// Page is one page of search results and the total number of matches.
type Page struct {
	Hits  []Hit
	Total int
}

// Search finds release groups (albums, EPs, singles, …) matching term: limit
// results (1-100) starting at offset.
func (c *Client) Search(ctx context.Context, term string, limit, offset int) (Page, error) {
	limit = min(max(limit, 1), 100)
	offset = max(offset, 0)
	q := url.Values{
		"query":  {escapeLucene(term)},
		"limit":  {strconv.Itoa(limit)},
		"offset": {strconv.Itoa(offset)},
		"fmt":    {"json"},
	}
	body, err := c.cached(ctx, "/release-group?"+q.Encode(), searchTTL)
	if err != nil {
		return Page{}, err
	}
	var resp struct {
		Count  int `json:"count"`
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
	if err := json.Unmarshal(body, &resp); err != nil {
		return Page{}, fmt.Errorf("musicbrainz search: decoding response: %w", err)
	}
	page := Page{Hits: make([]Hit, 0, len(resp.Groups)), Total: resp.Count}
	for _, g := range resp.Groups {
		artist, artistID := creditName(g.ArtistCredit)
		page.Hits = append(page.Hits, Hit{
			ID: g.ID, Title: g.Title, Artist: artist, ArtistID: artistID,
			Type: g.PrimaryType, SecondaryTypes: nonNil(g.SecondaryTypes),
			FirstReleaseDate: g.FirstRelease, Score: g.Score,
		})
	}
	return page, nil
}

// ArtistHit is one artist search result.
type ArtistHit struct {
	ID             string
	Name           string
	Disambiguation string
	Type           string // Person, Group, Orchestra, … ("" when MusicBrainz has none)
	Country        string
	Score          int
}

// SearchArtists finds artists matching term: at most limit (1-25), best first.
func (c *Client) SearchArtists(ctx context.Context, term string, limit int) ([]ArtistHit, error) {
	limit = min(max(limit, 1), 25)
	q := url.Values{"query": {escapeLucene(term)}, "limit": {strconv.Itoa(limit)}, "fmt": {"json"}}
	body, err := c.cached(ctx, "/artist?"+q.Encode(), searchTTL)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Artists []struct {
			ID             string `json:"id"`
			Name           string `json:"name"`
			Disambiguation string `json:"disambiguation"`
			Type           string `json:"type"`
			Country        string `json:"country"`
			Score          int    `json:"score"`
		} `json:"artists"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("musicbrainz artist search: decoding response: %w", err)
	}
	out := make([]ArtistHit, 0, len(resp.Artists))
	for _, a := range resp.Artists {
		if !ValidID(a.ID) || strings.TrimSpace(a.Name) == "" {
			continue
		}
		out = append(out, ArtistHit{ID: a.ID, Name: a.Name, Disambiguation: a.Disambiguation, Type: a.Type, Country: a.Country, Score: a.Score})
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

// cached returns the response for key (a path and query), from the cache, an
// identical request already in flight, or a new upstream request.
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

// fetch asks MusicBrainz, and once more if the first try failed in a way that
// often passes by itself: a dropped connection, a timeout, a 429 or a 5xx.
// Anything else (a 404, a 400, a body that is too large) is final. Both tries
// take their place in the one-request-per-second queue.
func (c *Client) fetch(ctx context.Context, key string) ([]byte, error) {
	body, retry, after, err := c.fetchOnce(ctx, key)
	if err == nil || !retry {
		return body, err
	}
	select {
	case <-time.After(after):
	case <-ctx.Done():
		return nil, err
	}
	body, _, _, err2 := c.fetchOnce(ctx, key)
	if err2 != nil {
		return nil, err2
	}
	return body, nil
}

// fetchOnce makes one request. retry says whether trying again might help, and
// after how long (what MusicBrainz asked for, within reason, else the default).
func (c *Client) fetchOnce(ctx context.Context, key string) (body []byte, retry bool, after time.Duration, err error) {
	after = c.retryDelay
	if err := c.wait(ctx); err != nil {
		return nil, false, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+key, nil)
	if err != nil {
		return nil, false, 0, err
	}
	req.Header.Set("User-Agent", c.ua)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, !errors.Is(err, context.Canceled), after, fmt.Errorf("musicbrainz request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		retry := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		after = retryWait(resp.Header.Get("Retry-After"), after)
		return nil, retry, after, fmt.Errorf("musicbrainz returned %s", resp.Status)
	}
	body, err = io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, true, after, fmt.Errorf("reading musicbrainz response: %w", err)
	}
	if len(body) > maxBody {
		return nil, false, 0, fmt.Errorf("musicbrainz response larger than %d bytes", maxBody)
	}
	return body, false, 0, nil
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

// retryWait is how long to wait before the retry: what the Retry-After header
// asks for (whole seconds), but never more than maxRetryAfter, else def.
func retryWait(header string, def time.Duration) time.Duration {
	secs, err := strconv.Atoi(strings.TrimSpace(header))
	if err != nil || secs < 0 {
		return def
	}
	return min(time.Duration(secs)*time.Second, maxRetryAfter)
}
