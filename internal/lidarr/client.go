// Package lidarr is a minimal, read-only client for the slice of Lidarr's v1
// API albumreq needs: whether an album (by MusicBrainz release-group ID) is in
// the library, the download queue, and a library album's tracks. Only GET
// requests are ever issued.
package lidarr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	queuePageSize = 200
	maxQueuePages = 10
)

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

// AlbumState is the library state of one album in Lidarr.
type AlbumState struct {
	ID             int    `json:"id"`
	Monitored      bool   `json:"monitored"`
	LastSearchTime string `json:"lastSearchTime"`
	Statistics     struct {
		TrackFileCount  int     `json:"trackFileCount"`
		TotalTrackCount int     `json:"totalTrackCount"`
		SizeOnDisk      int64   `json:"sizeOnDisk"`
		PercentOfTracks float64 `json:"percentOfTracks"`
	} `json:"statistics"`
}

// AlbumByForeignID returns the library state of the album with the given
// MusicBrainz release-group ID, or nil when Lidarr does not know it.
func (c *Client) AlbumByForeignID(ctx context.Context, foreignID string) (*AlbumState, error) {
	var states []AlbumState
	if err := c.get(ctx, "/api/v1/album", url.Values{"foreignAlbumId": {foreignID}}, &states); err != nil {
		return nil, err
	}
	if len(states) == 0 {
		return nil, nil
	}
	return &states[0], nil
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

// LibraryTrack is one track of a library album, for the release Lidarr has
// selected for it.
type LibraryTrack struct {
	Medium     int    `json:"mediumNumber"`
	Number     string `json:"trackNumber"`
	Title      string `json:"title"`
	DurationMs int    `json:"duration"`
	HasFile    bool   `json:"hasFile"`
}

// Tracks returns the tracks of the library album with the given Lidarr ID.
func (c *Client) Tracks(ctx context.Context, albumID int) ([]LibraryTrack, error) {
	var out []LibraryTrack
	err := c.get(ctx, "/api/v1/track", url.Values{"albumId": {strconv.Itoa(albumID)}}, &out)
	return out, err
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
