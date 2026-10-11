// Package lidarr is a minimal client for the slice of Lidarr's v1 API albumreq
// needs: whether an album (by MusicBrainz release-group ID) is in the library,
// the download queue, and a library album's tracks, all reads, plus exactly one
// write: AddAlbum, which adds an artist and an album to the library without
// monitoring either, so Lidarr searches for and downloads nothing.
package lidarr

import (
	"bytes"
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

// ErrAlreadyAdded means Lidarr already has the album. Adding is idempotent: a
// caller can treat this as success.
var ErrAlreadyAdded = errors.New("lidarr: album already added")

// ErrUnavailable means Lidarr, or the metadata service behind it, could not
// answer (a 5xx or no connection). Trying again later may work.
var ErrUnavailable = errors.New("lidarr: unavailable")

// RejectedError is Lidarr refusing an add with validation messages, which
// almost always means albumreq's own settings (root folder, profiles) are
// wrong, or the album is unknown to Lidarr's metadata.
type RejectedError struct{ Messages []string }

func (e *RejectedError) Error() string {
	return "lidarr rejected the request: " + strings.Join(e.Messages, "; ")
}

// ErrBadSettings means albumreq's settings do not match what Lidarr has: a
// missing root folder or profile. Unlike an unreachable Lidarr it will not fix
// itself.
var ErrBadSettings = errors.New("lidarr: settings do not match Lidarr")

// NewAlbum is everything needed to add an album and, if need be, its artist.
type NewAlbum struct {
	AlbumID           string // MusicBrainz release-group ID
	ArtistID          string // MusicBrainz artist ID
	RootFolder        string // an existing Lidarr root folder, e.g. /music
	QualityProfileID  int
	MetadataProfileID int
}

// addAlbumBody is the request Lidarr's UI sends, reduced to what matters. The
// false flags are explicit on purpose: neither the album nor the artist is
// monitored, new releases by the artist are not monitored either, and nothing
// is searched for.
type addAlbumBody struct {
	ForeignAlbumID string `json:"foreignAlbumId"`
	Monitored      bool   `json:"monitored"`
	AddOptions     struct {
		AddType           string `json:"addType"`
		SearchForNewAlbum bool   `json:"searchForNewAlbum"`
	} `json:"addOptions"`
	Artist struct {
		ForeignArtistID   string `json:"foreignArtistId"`
		QualityProfileID  int    `json:"qualityProfileId"`
		MetadataProfileID int    `json:"metadataProfileId"`
		RootFolderPath    string `json:"rootFolderPath"`
		Monitored         bool   `json:"monitored"`
		MonitorNewItems   string `json:"monitorNewItems"`
		AddOptions        struct {
			Monitor                string `json:"monitor"`
			SearchForMissingAlbums bool   `json:"searchForMissingAlbums"`
		} `json:"addOptions"`
	} `json:"artist"`
}

// AddAlbum adds the album, and its artist when Lidarr does not have it yet,
// with nothing monitored. An album the artist's metadata profile would
// normally exclude is still added: Lidarr keeps manually added albums.
func (c *Client) AddAlbum(ctx context.Context, a NewAlbum) error {
	var b addAlbumBody
	b.ForeignAlbumID = a.AlbumID
	b.AddOptions.AddType = "manual"
	b.Artist.ForeignArtistID = a.ArtistID
	b.Artist.QualityProfileID = a.QualityProfileID
	b.Artist.MetadataProfileID = a.MetadataProfileID
	b.Artist.RootFolderPath = a.RootFolder
	b.Artist.MonitorNewItems = "none"
	b.Artist.AddOptions.Monitor = "none"
	return c.post(ctx, "/api/v1/album", b)
}

// CheckSettings verifies that the root folder and both profiles exist in
// Lidarr, so a typo in albumreq's settings is found at startup, not at the
// first request.
func (c *Client) CheckSettings(ctx context.Context, rootFolder string, qualityProfileID, metadataProfileID int) error {
	var roots []struct {
		Path string `json:"path"`
	}
	if err := c.get(ctx, "/api/v1/rootfolder", nil, &roots); err != nil {
		return err
	}
	found := false
	for _, r := range roots {
		found = found || r.Path == rootFolder
	}
	if !found {
		return fmt.Errorf("%w: root folder %q does not exist in Lidarr", ErrBadSettings, rootFolder)
	}
	for _, check := range []struct {
		path string
		id   int
		what string
	}{{"/api/v1/qualityprofile", qualityProfileID, "quality profile"}, {"/api/v1/metadataprofile", metadataProfileID, "metadata profile"}} {
		var profiles []struct {
			ID int `json:"id"`
		}
		if err := c.get(ctx, check.path, nil, &profiles); err != nil {
			return err
		}
		ok := false
		for _, p := range profiles {
			ok = ok || p.ID == check.id
		}
		if !ok {
			return fmt.Errorf("%w: %s %d does not exist in Lidarr", ErrBadSettings, check.what, check.id)
		}
	}
	return nil
}

// Tracks returns the tracks of the library album with the given Lidarr ID.
func (c *Client) Tracks(ctx context.Context, albumID int) ([]LibraryTrack, error) {
	var out []LibraryTrack
	err := c.get(ctx, "/api/v1/track", url.Values{"albumId": {strconv.Itoa(albumID)}}, &out)
	return out, err
}

// post sends body as JSON. Lidarr answers 201 on success. A 400 carries a list
// of validation failures: "already added" is ErrAlreadyAdded, anything else a
// RejectedError. Errors never contain the request URL or any header.
func (c *Client) post(ctx context.Context, path string, body any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("X-Api-Key", c.key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("%w: %s: %v", ErrUnavailable, path, err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusOK:
		return nil
	case resp.StatusCode == http.StatusBadRequest:
		var failures []struct {
			Message   string `json:"errorMessage"`
			ErrorCode string `json:"errorCode"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&failures); err != nil || len(failures) == 0 {
			return &RejectedError{Messages: []string{"no reason given"}}
		}
		rejected := &RejectedError{}
		for _, f := range failures {
			if f.ErrorCode == "AlbumExistsValidator" {
				return ErrAlreadyAdded
			}
			rejected.Messages = append(rejected.Messages, f.Message)
		}
		return rejected
	case resp.StatusCode >= 500:
		return fmt.Errorf("%w: %s returned %s", ErrUnavailable, path, resp.Status)
	default:
		return fmt.Errorf("lidarr %s returned %s", path, resp.Status)
	}
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
