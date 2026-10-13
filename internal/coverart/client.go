// Package coverart fetches album cover images by release-group ID. By default
// it asks Lidarr's image CDN, a cache in front of the Cover Art Archive (the
// MetaBrainz project whose covers MusicBrainz's own pages show), which is far
// more dependable than archive.org's storage nodes. albumreq serves the
// images itself, so the browser never talks to either, and keeps them in
// memory.
package coverart

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/draw"
	imgjpeg "image/jpeg"
	_ "image/png" // artist photos may be PNG; they are always served as JPEG
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

const (
	maxPhoto     = 6 << 20 // an artist photo as the CDN serves it; it is shrunk before it is kept
	maxPhotoSide = 6000    // pixels: a larger picture is refused before it is decoded
	photoWidth   = 480     // what the page needs; the originals are 1000-1920 pixels
	photoQuality = 82
)

// Photo returns an artist's photo, shrunk to photoWidth and re-encoded as JPEG, for
// the image at rawURL. The URL comes from a third party's JSON, so it is fetched
// only if it points at the same host as this client's base (the image CDN): a
// hostile response cannot make albumreq fetch anything else. The picture is
// decoded here, so only something that really is an image is ever passed on.
func (c *Client) Photo(ctx context.Context, artistID, rawURL string) ([]byte, string, error) {
	if !musicbrainz.ValidID(artistID) {
		return nil, "", ErrInvalid
	}
	u, err := url.Parse(rawURL)
	base, berr := url.Parse(c.base)
	if err != nil || berr != nil || u.Scheme != base.Scheme || u.Host != base.Host || u.User != nil || len(rawURL) > 1000 {
		return nil, "", ErrInvalid
	}
	key := "artist/" + artistID
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
	data, err := c.fetchPhoto(ctx, u.String())
	if err != nil && !errors.Is(err, ErrNoCover) && isRetryable(err) {
		select {
		case <-time.After(c.retry):
			data, err = c.fetchPhoto(ctx, u.String())
		case <-ctx.Done():
			return nil, "", ctx.Err()
		}
	}
	switch {
	case err == nil:
		c.store(key, entry{data: data, ct: "image/jpeg", exp: time.Now().Add(hitTTL)})
		return data, "image/jpeg", nil
	case errors.Is(err, ErrNoCover):
		c.store(key, entry{exp: time.Now().Add(missTTL)})
	}
	return nil, "", err
}

func (c *Client) fetchPhoto(ctx context.Context, rawURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.ua)
	resp, err := c.http.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, transportError{err}
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, ErrNoCover
	case resp.StatusCode != http.StatusOK:
		return nil, statusError{resp.StatusCode}
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxPhoto+1))
	if err != nil {
		return nil, fmt.Errorf("reading photo: %w", err)
	}
	if len(raw) > maxPhoto {
		return nil, fmt.Errorf("coverart: photo larger than %d bytes", maxPhoto)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil || cfg.Width < 1 || cfg.Height < 1 || cfg.Width > maxPhotoSide || cfg.Height > maxPhotoSide {
		return nil, fmt.Errorf("coverart: the photo is not a usable image")
	}
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("coverart: the photo is not a usable image")
	}
	var out bytes.Buffer
	if err := imgjpeg.Encode(&out, shrink(img, photoWidth), &imgjpeg.Options{Quality: photoQuality}); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// shrink scales img down to the given width by averaging the source pixels under
// each new pixel (a box filter), keeping the proportions. A picture that is not
// wider is left alone. Only the standard library is used.
func shrink(img image.Image, width int) image.Image {
	b := img.Bounds()
	if b.Dx() <= width {
		return img
	}
	height := max(1, b.Dy()*width/b.Dx())
	src := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(src, src.Bounds(), img, b.Min, draw.Src)
	dst := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		y0, y1 := y*b.Dy()/height, max(y*b.Dy()/height+1, (y+1)*b.Dy()/height)
		for x := 0; x < width; x++ {
			x0, x1 := x*b.Dx()/width, max(x*b.Dx()/width+1, (x+1)*b.Dx()/width)
			var r, g, bl, a, n uint32
			for sy := y0; sy < y1; sy++ {
				row := src.Pix[sy*src.Stride : sy*src.Stride+b.Dx()*4]
				for sx := x0; sx < x1; sx++ {
					p := row[sx*4 : sx*4+4]
					r, g, bl, a, n = r+uint32(p[0]), g+uint32(p[1]), bl+uint32(p[2]), a+uint32(p[3]), n+1
				}
			}
			o := dst.PixOffset(x, y)
			dst.Pix[o], dst.Pix[o+1], dst.Pix[o+2], dst.Pix[o+3] = uint8(r/n), uint8(g/n), uint8(bl/n), uint8(a/n)
		}
	}
	return dst
}
