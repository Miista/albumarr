// Package lidarr is a minimal, read-only client for the slice of Lidarr's v1
// API albumreq needs: album lookup (MusicBrainz search through Lidarr), the
// state of albums already in the library, the download queue, and the
// cover-art proxy. Only GET requests are ever issued.
package lidarr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// coverPrefix is the only path Cover will fetch. Lidarr serves album art
// through this proxy, so the browser never talks to third-party hosts.
const coverPrefix = "/MediaCoverProxy/"

const (
	maxCoverBytes = 5 << 20
	queuePageSize = 200
	maxQueuePages = 10
)

// ErrInvalidCoverPath is returned by Cover for any path that is not a plain
// Lidarr cover-proxy path.
var ErrInvalidCoverPath = errors.New("invalid cover path")

// Client talks to one Lidarr instance.
type Client struct {
	base string
	key  string
	http *http.Client
}

// New returns a Client for the Lidarr at baseURL (e.g. http://lidarr:8686).
func New(baseURL, apiKey string) *Client {
	return &Client{
		base: strings.TrimRight(baseURL, "/"),
		key:  apiKey,
		http: &http.Client{Timeout: 15 * time.Second},
	}
}

// LookupAlbum is one hit from album/lookup. ID is non-nil when the album is
// already known to this Lidarr (in its library, monitored or not).
type LookupAlbum struct {
	ID             *int     `json:"id"`
	Title          string   `json:"title"`
	ForeignAlbumID string   `json:"foreignAlbumId"`
	AlbumType      string   `json:"albumType"`
	SecondaryTypes []string `json:"secondaryTypes"`
	ReleaseDate    string   `json:"releaseDate"`
	Images         []Image  `json:"images"`
	Artist         struct {
		Name      string `json:"artistName"`
		ForeignID string `json:"foreignArtistId"`
	} `json:"artist"`
}

// Image is one piece of artwork attached to a lookup result.
type Image struct {
	CoverType string `json:"coverType"`
	URL       string `json:"url"`
}

// CoverPath returns the cover-art proxy path for the album, or "" when it has
// none (or the path is not one Cover would fetch).
func (a LookupAlbum) CoverPath() string {
	for _, img := range a.Images {
		if img.CoverType == "cover" && ValidCoverPath(img.URL) {
			return img.URL
		}
	}
	return ""
}

// AlbumState is the library state of one album in Lidarr.
type AlbumState struct {
	ID             int    `json:"id"`
	Monitored      bool   `json:"monitored"`
	LastSearchTime string `json:"lastSearchTime"`
	Statistics     struct {
		TrackFileCount  int     `json:"trackFileCount"`
		TotalTrackCount int     `json:"totalTrackCount"`
		PercentOfTracks float64 `json:"percentOfTracks"`
	} `json:"statistics"`
}

// Lookup searches MusicBrainz, through Lidarr, for albums matching term.
func (c *Client) Lookup(ctx context.Context, term string) ([]LookupAlbum, error) {
	var out []LookupAlbum
	err := c.get(ctx, "/api/v1/album/lookup", url.Values{"term": {term}}, &out)
	return out, err
}

// Albums returns the library state of the given Lidarr album IDs.
func (c *Client) Albums(ctx context.Context, ids []int) (map[int]AlbumState, error) {
	out := make(map[int]AlbumState, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	q := url.Values{}
	for _, id := range ids {
		q.Add("albumIds", strconv.Itoa(id))
	}
	var states []AlbumState
	if err := c.get(ctx, "/api/v1/album", q, &states); err != nil {
		return nil, err
	}
	for _, s := range states {
		out[s.ID] = s
	}
	return out, nil
}

// QueuedAlbumIDs returns the IDs of every album with an item in Lidarr's
// download queue.
func (c *Client) QueuedAlbumIDs(ctx context.Context) (map[int]bool, error) {
	out := map[int]bool{}
	for page := 1; page <= maxQueuePages; page++ {
		var resp struct {
			TotalRecords int `json:"totalRecords"`
			Records      []struct {
				AlbumID int `json:"albumId"`
			} `json:"records"`
		}
		q := url.Values{"page": {strconv.Itoa(page)}, "pageSize": {strconv.Itoa(queuePageSize)}}
		if err := c.get(ctx, "/api/v1/queue", q, &resp); err != nil {
			return nil, err
		}
		for _, r := range resp.Records {
			if r.AlbumID != 0 {
				out[r.AlbumID] = true
			}
		}
		if page*queuePageSize >= resp.TotalRecords || len(resp.Records) == 0 {
			break
		}
	}
	return out, nil
}

// ValidCoverPath reports whether p is a plain cover-proxy path: the fixed
// prefix, then only path-safe characters. This is what keeps Cover from being
// steered at any other URL on (or off) the Lidarr host.
func ValidCoverPath(p string) bool {
	if !strings.HasPrefix(p, coverPrefix) || len(p) > 300 || strings.Contains(p, "..") {
		return false
	}
	for _, r := range p {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '/', r == '.', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

// Cover fetches a cover image from Lidarr's cover proxy. The proxy needs no
// credentials, so the API key is deliberately not sent.
func (c *Client) Cover(ctx context.Context, path string) ([]byte, string, error) {
	if !ValidCoverPath(path) {
		return nil, "", ErrInvalidCoverPath
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("fetching cover: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("fetching cover: lidarr returned %s", resp.Status)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "image/") {
		return nil, "", fmt.Errorf("fetching cover: unexpected content type %q", ct)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxCoverBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("reading cover: %w", err)
	}
	if len(data) > maxCoverBytes {
		return nil, "", fmt.Errorf("cover larger than %d bytes", maxCoverBytes)
	}
	return data, ct, nil
}

// get issues an authenticated GET and decodes the JSON response into out.
// Errors carry the status code only — never the request URL's query or any
// header, so the API key cannot leak into logs.
func (c *Client) get(ctx context.Context, path string, q url.Values, out any) error {
	u := c.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Api-Key", c.key)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("lidarr %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("lidarr %s returned %s", path, resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("lidarr %s: decoding response: %w", path, err)
	}
	return nil
}
