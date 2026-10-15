// Package lidarr is a minimal client for the slice of Lidarr's v1 API albumarr
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
	"sync"
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

// ArtistAlbumStates returns the library state of every album Lidarr has for the
// artist with the given MusicBrainz ID, by the album's MusicBrainz release-group
// ID. An artist Lidarr does not have has none, which is not an error.
func (c *Client) ArtistAlbumStates(ctx context.Context, artistMBID string) (map[string]AlbumState, error) {
	var artists []struct {
		ID int `json:"id"`
	}
	if err := c.get(ctx, "/api/v1/artist", url.Values{"mbId": {artistMBID}}, &artists); err != nil {
		return nil, err
	}
	out := map[string]AlbumState{}
	if len(artists) == 0 {
		return out, nil
	}
	var albums []struct {
		AlbumState
		ForeignAlbumID string `json:"foreignAlbumId"`
	}
	if err := c.get(ctx, "/api/v1/album", url.Values{"artistId": {strconv.Itoa(artists[0].ID)}}, &albums); err != nil {
		return nil, err
	}
	for _, a := range albums {
		if a.ForeignAlbumID != "" {
			out[a.ForeignAlbumID] = a.AlbumState
		}
	}
	return out, nil
}

// Stage is how far an album's download has got, rolled up from its queue items.
type Stage string

const (
	StageQueued      Stage = "queued"      // waiting for a client or for the grab delay
	StageDownloading Stage = "downloading" // bytes are coming in
	StagePaused      Stage = "paused"
	StageImporting   Stage = "importing" // downloaded; Lidarr is moving it into the library
	StageStalled     Stage = "stalled"   // failed, or blocked on someone: needs a person
)

// Download is an album's place in Lidarr's download queue. An album can have more
// than one item (a retry, a second release); Stage is the one furthest from done.
type Download struct {
	Stage Stage
	Items int
}

// queueItem is what one item of Lidarr's queue says. Names are Lidarr's (Servarr
// QueueResource); the values were not seen live, only read from its source.
type queueItem struct {
	AlbumID               int    `json:"albumId"`
	Status                string `json:"status"`
	TrackedDownloadStatus string `json:"trackedDownloadStatus"`
	TrackedDownloadState  string `json:"trackedDownloadState"`
}

// stageOf says where one item is. The tracked state wins over the client's own
// status, since it is Lidarr's view and knows about the import.
func stageOf(q queueItem) Stage {
	switch strings.ToLower(q.TrackedDownloadState) {
	case "importblocked", "importfailed", "failed", "failedpending":
		return StageStalled
	case "importpending", "importing", "imported":
		return StageImporting
	}
	switch strings.ToLower(q.Status) {
	case "failed", "warning":
		return StageStalled
	case "paused":
		return StagePaused
	case "queued", "delay":
		return StageQueued
	case "completed":
		return StageImporting
	}
	if strings.EqualFold(q.TrackedDownloadStatus, "error") {
		return StageStalled
	}
	return StageDownloading
}

// rank orders stages by how much a person should care: the worst one describes the album.
var rank = map[Stage]int{StageStalled: 5, StagePaused: 4, StageQueued: 3, StageDownloading: 2, StageImporting: 1}

// Queue returns the download of every album with an item in Lidarr's queue.
func (c *Client) Queue(ctx context.Context) (map[int]Download, error) {
	items := map[int][]queueItem{}
	for page := 1; page <= maxQueuePages; page++ {
		var resp struct {
			TotalRecords int         `json:"totalRecords"`
			Records      []queueItem `json:"records"`
		}
		q := url.Values{"page": {strconv.Itoa(page)}, "pageSize": {strconv.Itoa(queuePageSize)}}
		if err := c.get(ctx, "/api/v1/queue", q, &resp); err != nil {
			return nil, err
		}
		for _, r := range resp.Records {
			if r.AlbumID != 0 {
				items[r.AlbumID] = append(items[r.AlbumID], r)
			}
		}
		if page*queuePageSize >= resp.TotalRecords || len(resp.Records) == 0 {
			break
		}
	}
	out := make(map[int]Download, len(items))
	for id, list := range items {
		d := Download{Items: len(list), Stage: StageImporting}
		for _, q := range list {
			if st := stageOf(q); rank[st] > rank[d.Stage] {
				d.Stage = st
			}
		}
		out[id] = d
	}
	return out, nil
}

// QueuedAlbumIDs returns the IDs of every album with an item in Lidarr's
// download queue.
func (c *Client) QueuedAlbumIDs(ctx context.Context) (map[int]bool, error) {
	q, err := c.Queue(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[int]bool, len(q))
	for id := range q {
		out[id] = true
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

// ErrUnauthorized means Lidarr refused the API key.
var ErrUnauthorized = errors.New("lidarr: API key rejected")

// ErrNotConfigured is what a Live client answers until it is given a Lidarr.
var ErrNotConfigured = errors.New("lidarr: not configured")

// ErrAlreadyAdded means Lidarr already has the album. Adding is idempotent: a
// caller can treat this as success.
var ErrAlreadyAdded = errors.New("lidarr: album already added")

// ErrAlbumNotFound means Lidarr's own metadata does not know the album (a very new
// MusicBrainz entry, say), so it cannot be added yet. It is how albumarr finds
// out: Lidarr answers 400 "An album with this ID was not found".
var ErrAlbumNotFound = errors.New("lidarr: album not found in its metadata")

// ErrUnavailable means Lidarr, or the metadata service behind it, could not
// answer (a 5xx or no connection). Trying again later may work.
var ErrUnavailable = errors.New("lidarr: unavailable")

// RejectedError is Lidarr refusing an add with validation messages, which
// almost always means albumarr's own settings (root folder, profiles) are
// wrong, or the album is unknown to Lidarr's metadata.
type RejectedError struct{ Messages []string }

func (e *RejectedError) Error() string {
	return "lidarr rejected the request: " + strings.Join(e.Messages, "; ")
}

// ErrBadSettings means albumarr's settings do not match what Lidarr has: a
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
// album is monitored, so Lidarr wants it; the artist is not, and new releases by
// the artist are not either, so nothing else is wanted. The artist carries no
// addOptions on purpose: Lidarr would apply its "monitor: none" to every album
// imported with a new artist, the requested one included, after the add returns
// (checked on a throwaway: without it only the requested album is monitored, even
// when the profile imports several). searchForNewAlbum is
// false on purpose: Lidarr only honours it when it also adds the artist, so the
// search is a separate, explicit SearchAlbum.
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
	} `json:"artist"`
}

// AddAlbum adds the album, monitored, and its artist when Lidarr does not have
// it yet, unmonitored. An album the artist's metadata profile would normally
// exclude is still added: Lidarr keeps manually added albums. It does not
// search; see SearchAlbum.
func (c *Client) AddAlbum(ctx context.Context, a NewAlbum) error {
	var b addAlbumBody
	b.ForeignAlbumID = a.AlbumID
	b.Monitored = true
	b.AddOptions.AddType = "manual"
	b.Artist.ForeignArtistID = a.ArtistID
	b.Artist.QualityProfileID = a.QualityProfileID
	b.Artist.MetadataProfileID = a.MetadataProfileID
	b.Artist.RootFolderPath = a.RootFolder
	b.Artist.MonitorNewItems = "none"
	return c.post(ctx, "/api/v1/album", b)
}

// MonitorAlbum marks one album as wanted. AddAlbum already asks for that, but
// for a new artist Lidarr applies the artist's own "monitor: none" afterwards
// and unmonitors the album again, so albumarr checks and sets it explicitly.
func (c *Client) MonitorAlbum(ctx context.Context, albumID int) error {
	return c.send(ctx, http.MethodPut, "/api/v1/album/monitor", map[string]any{"albumIds": []int{albumID}, "monitored": true})
}

// SearchAlbum asks Lidarr to search its indexers for one album, as the search
// button on the album does. Lidarr grabs a release if it finds one that fits the
// quality profile, so this is the call that starts a download.
func (c *Client) SearchAlbum(ctx context.Context, albumID int) error {
	return c.post(ctx, "/api/v1/command", map[string]any{"name": "AlbumSearch", "albumIds": []int{albumID}})
}

// RequiredMetadataProfile is the name of the metadata profile every requested
// artist gets. "None" allows no album types, so Lidarr imports none of the
// artist's other albums and a request adds only the album asked for (the
// requested album is added regardless: Lidarr keeps manually added albums).
// It is a rule, not a setting. Lidarr ships a profile by this name; if it has
// been deleted, create one that allows nothing.
const RequiredMetadataProfile = "None"

// MetadataProfileByName returns the ID of the metadata profile with that name,
// ignoring case and surrounding space. It is asked again on every add, so a
// profile that was deleted and recreated is followed, and a missing one is
// found at once. A name Lidarr does not have is ErrBadSettings.
func (c *Client) MetadataProfileByName(ctx context.Context, name string) (int, error) {
	var profiles []Profile
	if err := c.get(ctx, "/api/v1/metadataprofile", nil, &profiles); err != nil {
		return 0, err
	}
	for _, p := range profiles {
		if strings.EqualFold(strings.TrimSpace(p.Name), strings.TrimSpace(name)) {
			return p.ID, nil
		}
	}
	return 0, fmt.Errorf("%w: Lidarr has no metadata profile named %q; create one that allows no album types, so a request adds only the album asked for", ErrBadSettings, name)
}

// CheckSettings verifies that the root folder and the quality profile exist in
// Lidarr, so a typo in albumarr's settings is found at startup, not at the first
// request. Whether Lidarr has the metadata profile requests need is a separate
// question (MetadataProfileByName): it is not something a typo in a setting causes,
// and it is reported on the Issues page, not by refusing to save.
func (c *Client) CheckSettings(ctx context.Context, rootFolder string, qualityProfileID int) error {
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
	var profiles []Profile
	if err := c.get(ctx, "/api/v1/qualityprofile", nil, &profiles); err != nil {
		return err
	}
	ok := false
	for _, p := range profiles {
		ok = ok || p.ID == qualityProfileID
	}
	if !ok {
		return fmt.Errorf("%w: quality profile %d does not exist in Lidarr", ErrBadSettings, qualityProfileID)
	}
	return nil
}

// Version asks Lidarr who it is and returns its version, which proves the URL
// and the API key both work.
func (c *Client) Version(ctx context.Context) (string, error) {
	var st struct {
		Version string `json:"version"`
	}
	if err := c.get(ctx, "/api/v1/system/status", nil, &st); err != nil {
		return "", err
	}
	if st.Version == "" {
		return "", errors.New("lidarr: the answer does not look like Lidarr's")
	}
	return st.Version, nil
}

// Profile is a quality or metadata profile.
type Profile struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// Options is what the settings page offers to choose from.
type Options struct {
	RootFolders     []string  `json:"rootFolders"`
	QualityProfiles []Profile `json:"qualityProfiles"`
}

// Options lists Lidarr's root folders and profiles.
func (c *Client) Options(ctx context.Context) (Options, error) {
	opts := Options{RootFolders: []string{}, QualityProfiles: []Profile{}}
	var roots []struct {
		Path string `json:"path"`
	}
	if err := c.get(ctx, "/api/v1/rootfolder", nil, &roots); err != nil {
		return Options{}, err
	}
	for _, r := range roots {
		opts.RootFolders = append(opts.RootFolders, r.Path)
	}
	if err := c.get(ctx, "/api/v1/qualityprofile", nil, &opts.QualityProfiles); err != nil {
		return Options{}, err
	}
	return opts, nil
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
	return c.send(ctx, http.MethodPost, path, body)
}

// send is post for any method. Lidarr answers 200, 201 or 202 on success.
func (c *Client) send(ctx context.Context, method, path string, body any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(payload))
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
	case resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusAccepted:
		return nil
	case resp.StatusCode == http.StatusBadRequest:
		var failures []struct {
			Message      string `json:"errorMessage"`
			ErrorCode    string `json:"errorCode"`
			PropertyName string `json:"propertyName"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&failures); err != nil || len(failures) == 0 {
			return &RejectedError{Messages: []string{"no reason given"}}
		}
		rejected := &RejectedError{}
		for _, f := range failures {
			if f.ErrorCode == "AlbumExistsValidator" {
				return ErrAlreadyAdded
			}
			if f.PropertyName == "MusicbrainzId" && strings.Contains(strings.ToLower(f.Message), "not found") {
				return ErrAlbumNotFound
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
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("%w: %s returned %s", ErrUnauthorized, path, resp.Status)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("lidarr %s returned %s", path, resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("lidarr %s: decoding response: %w", path, err)
	}
	return nil
}

// Live is a Client that can be replaced while the process runs, so changing the
// Lidarr settings takes effect without a restart. Until it is given a client
// every call answers ErrNotConfigured.
type Live struct {
	mu sync.RWMutex
	c  *Client
}

// Set replaces the client; nil means "not configured".
func (l *Live) Set(c *Client) {
	l.mu.Lock()
	l.c = c
	l.mu.Unlock()
}

// Client returns the current client, or nil.
func (l *Live) Client() *Client {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.c
}

// AlbumByForeignID, QueuedAlbumIDs, Tracks and AddAlbum make Live a drop-in for
// Client wherever the catalog needs one.
func (l *Live) AlbumByForeignID(ctx context.Context, foreignID string) (*AlbumState, error) {
	c := l.Client()
	if c == nil {
		return nil, ErrNotConfigured
	}
	return c.AlbumByForeignID(ctx, foreignID)
}

func (l *Live) QueuedAlbumIDs(ctx context.Context) (map[int]bool, error) {
	c := l.Client()
	if c == nil {
		return nil, ErrNotConfigured
	}
	return c.QueuedAlbumIDs(ctx)
}

func (l *Live) Queue(ctx context.Context) (map[int]Download, error) {
	c := l.Client()
	if c == nil {
		return nil, ErrNotConfigured
	}
	return c.Queue(ctx)
}

func (l *Live) ArtistAlbumStates(ctx context.Context, artistMBID string) (map[string]AlbumState, error) {
	c := l.Client()
	if c == nil {
		return nil, ErrNotConfigured
	}
	return c.ArtistAlbumStates(ctx, artistMBID)
}

func (l *Live) Tracks(ctx context.Context, albumID int) ([]LibraryTrack, error) {
	c := l.Client()
	if c == nil {
		return nil, ErrNotConfigured
	}
	return c.Tracks(ctx, albumID)
}

func (l *Live) MonitorAlbum(ctx context.Context, albumID int) error {
	c := l.Client()
	if c == nil {
		return ErrNotConfigured
	}
	return c.MonitorAlbum(ctx, albumID)
}

func (l *Live) SearchAlbum(ctx context.Context, albumID int) error {
	c := l.Client()
	if c == nil {
		return ErrNotConfigured
	}
	return c.SearchAlbum(ctx, albumID)
}

func (l *Live) MetadataProfileByName(ctx context.Context, name string) (int, error) {
	c := l.Client()
	if c == nil {
		return 0, ErrNotConfigured
	}
	return c.MetadataProfileByName(ctx, name)
}

func (l *Live) AddAlbum(ctx context.Context, a NewAlbum) error {
	c := l.Client()
	if c == nil {
		return ErrNotConfigured
	}
	return c.AddAlbum(ctx, a)
}
