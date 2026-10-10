// Package coverart fetches album cover images by release-group ID. By default
// it asks Lidarr's image CDN, a cache in front of the Cover Art Archive (the
// MetaBrainz project whose covers MusicBrainz's own pages show), which is far
// more dependable than archive.org's storage nodes. albumreq serves the
// images itself, so the browser never talks to either, and keeps them in
// memory.
package coverart

import (
	"context"
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
	// DefaultBase is Lidarr's image CDN, caching the Cover Art Archive. The
	// paths below it are the archive's own: /release-group/<id>/front-<size>.
	DefaultBase = "https://images.lidarr.audio/cache/https://coverartarchive.org"

	maxImage    = 3 << 20
	maxCacheB   = 48 << 20 // bytes of images kept in memory
	hitTTL      = 24 * time.Hour
	missTTL     = time.Hour
	maxInFlight = 4
	retryDelay  = 300 * time.Millisecond
)

// ErrNoCover means the archive has no cover for the album.
var ErrNoCover = errors.New("coverart: no cover")

// ErrInvalid is returned for an unusable ID or size.
var ErrInvalid = errors.New("coverart: invalid id or size")

// Client fetches covers.
type Client struct {
	base  string
	ua    string
	http  *http.Client
	slots chan struct{}
	retry time.Duration

	mu    sync.Mutex
	cache map[string]entry
	bytes int
}

type entry struct {
	data []byte // nil for a remembered miss
	ct   string
	exp  time.Time
}

// New returns a Client for DefaultBase, or for base when it is not empty. The
// User-Agent identifies albumreq without any contact, since neither the CDN
// nor the archive asks for one.
func New(base string) *Client {
	if base == "" {
		base = DefaultBase
	}
	return NewWithBase(base, musicbrainz.AnonymousUserAgent, hostAllowed)
}

// NewWithBase is New with an explicit base URL and redirect policy, for tests.
// allow reports whether a redirect to the given host is acceptable.
func NewWithBase(base, userAgent string, allow func(host string) bool) *Client {
	return &Client{
		base:  strings.TrimRight(base, "/"),
		ua:    userAgent,
		slots: make(chan struct{}, maxInFlight),
		retry: retryDelay,
		cache: map[string]entry{},
		http: &http.Client{
			Timeout: 20 * time.Second,
			// The archive redirects to archive.org storage nodes; anything
			// else is refused, so a hostile response cannot steer us.
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 5 {
					return fmt.Errorf("too many redirects: %w", errRedirect)
				}
				if !allow(req.URL.Host) {
					return fmt.Errorf("to %q: %w", req.URL.Host, errRedirect)
				}
				return nil
			},
		},
	}
}

func hostAllowed(host string) bool {
	return host == "images.lidarr.audio" || host == "coverartarchive.org" || host == "archive.org" || strings.HasSuffix(host, ".archive.org")
}

// Front returns the front cover of release group id at 250 or 500 pixels.
func (c *Client) Front(ctx context.Context, id string, size int) ([]byte, string, error) {
	if !musicbrainz.ValidID(id) || (size != 250 && size != 500) {
		return nil, "", ErrInvalid
	}
	key := fmt.Sprintf("%s/%d", id, size)
	if data, ct, hit, miss := c.lookup(key); hit {
		return data, ct, nil
	} else if miss {
		return nil, "", ErrNoCover
	}

	select {
	case c.slots <- struct{}{}:
		defer func() { <-c.slots }()
	case <-ctx.Done():
		return nil, "", ctx.Err()
	}

	data, ct, err := c.fetch(ctx, key)
	if err != nil && !errors.Is(err, ErrNoCover) && isRetryable(err) {
		select {
		case <-time.After(c.retry):
			data, ct, err = c.fetch(ctx, key)
		case <-ctx.Done():
			return nil, "", ctx.Err()
		}
	}
	switch {
	case err == nil:
		c.store(key, entry{data: data, ct: ct, exp: time.Now().Add(hitTTL)})
	case errors.Is(err, ErrNoCover):
		c.store(key, entry{exp: time.Now().Add(missTTL)})
	}
	return data, ct, err
}

type statusError struct{ code int }

func (e statusError) Error() string { return fmt.Sprintf("coverart: archive returned %d", e.code) }

// transportError marks a failure to get any response (connection reset,
// timeout): the only non-status failure worth one retry.
type transportError struct{ err error }

func (e transportError) Error() string { return "coverart request failed: " + e.err.Error() }
func (e transportError) Unwrap() error { return e.err }

// errRedirect is a refused redirect; retrying cannot change the answer.
var errRedirect = errors.New("redirect refused")

// isRetryable reports whether one more attempt could plausibly succeed: a 5xx
// from the archive (archive.org storage nodes do flake) or a dropped
// connection, but not a missing cover, a bad body or a refused redirect.
func isRetryable(err error) bool {
	var se statusError
	if errors.As(err, &se) {
		return se.code >= 500
	}
	var te transportError
	return errors.As(err, &te) && !errors.Is(err, errRedirect) && !errors.Is(err, context.Canceled)
}

func (c *Client) fetch(ctx context.Context, key string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/release-group/"+strings.Replace(key, "/", "/front-", 1), nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", c.ua)
	resp, err := c.http.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, "", transportError{err}
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, "", ErrNoCover
	case resp.StatusCode != http.StatusOK:
		return nil, "", statusError{resp.StatusCode}
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "image/") {
		return nil, "", fmt.Errorf("coverart: unexpected content type %q", ct)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxImage+1))
	if err != nil {
		return nil, "", fmt.Errorf("reading cover: %w", err)
	}
	if len(data) > maxImage {
		return nil, "", fmt.Errorf("coverart: image larger than %d bytes", maxImage)
	}
	return data, ct, nil
}

func (c *Client) lookup(key string) (data []byte, ct string, hit, miss bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.cache[key]
	if !ok || time.Now().After(e.exp) {
		return nil, "", false, false
	}
	if e.data == nil {
		return nil, "", false, true
	}
	return e.data, e.ct, true, false
}

// store keeps an entry, evicting expired and then arbitrary entries to stay
// under the byte budget.
func (c *Client) store(key string, e entry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if old, ok := c.cache[key]; ok {
		c.bytes -= len(old.data)
		delete(c.cache, key)
	}
	for k, old := range c.cache {
		if c.bytes+len(e.data) <= maxCacheB {
			break
		}
		if time.Now().After(old.exp) || len(old.data) > 0 {
			c.bytes -= len(old.data)
			delete(c.cache, k)
		}
	}
	c.cache[key] = e
	c.bytes += len(e.data)
}
