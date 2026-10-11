// Package catalog is albumreq's model of an album. What exists comes from two
// places: MusicBrainz answers searches (its ranking is the best), and Lidarr's
// hosted metadata service gives an album's details and track list in one
// cached call. What you have comes from your own Lidarr: library state, the
// download queue, and the tracks of a library album. Any one source being
// unreachable degrades the answer — a missing chip, a missing track list —
// instead of failing it.
package catalog

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Miista/albumreq/internal/lidarr"
	"github.com/Miista/albumreq/internal/lidarrmeta"
	"github.com/Miista/albumreq/internal/musicbrainz"
)

// ErrInvalidID is returned for an album ID that is not a MusicBrainz UUID.
var ErrInvalidID = errors.New("catalog: invalid album id")

// ErrInvalidOffset is returned for a search offset outside 0..MaxOffset.
var ErrInvalidOffset = errors.New("catalog: invalid search offset")

// ErrRequestsDisabled is returned when albumreq has not been told where to put
// requested albums in Lidarr, which is what switches requesting on.
var ErrRequestsDisabled = errors.New("catalog: requests are not enabled")

// ErrNotInMetadata means Lidarr's metadata service does not know the album (a
// very new MusicBrainz entry may not have reached it yet), so Lidarr cannot
// add it.
var ErrNotInMetadata = errors.New("catalog: album is not in Lidarr's metadata")

// ErrUnavailable means Lidarr or its metadata service could not be reached.
var ErrUnavailable = errors.New("catalog: a required service is unavailable")

const (
	// PageSize is how many results one search request returns.
	PageSize = 24
	// MaxOffset is the furthest page a search can reach (ten pages): past that
	// a query is better refined than paged through, and it bounds the load put
	// on MusicBrainz.
	MaxOffset  = 9 * PageSize
	maxLookups = 6 // concurrent Lidarr library lookups per search
)

// Status is the chip shown on an album card. The empty value means "not in
// Lidarr at all", which shows no chip.
type Status string

const (
	StatusNone       Status = ""
	StatusAvailable  Status = "available"  // every track has a file
	StatusProcessing Status = "processing" // an item for it is in the download queue
	StatusRequested  Status = "requested"  // monitored, not searched yet
	StatusNotFound   Status = "notfound"   // monitored and searched, but nothing was grabbed
	StatusInLidarr   Status = "inlidarr"   // in Lidarr, not monitored: added but nothing asked for yet
)

// Album is one search result as the frontend renders it.
type Album struct {
	ID             string   `json:"id"` // MusicBrainz release-group ID
	Title          string   `json:"title"`
	Artist         string   `json:"artist"`
	ArtistID       string   `json:"artistId"`
	Type           string   `json:"type"`
	SecondaryTypes []string `json:"secondaryTypes"`
	Year           int      `json:"year,omitempty"`
	Cover          string   `json:"cover"`
	Status         Status   `json:"status"`
}

// Searcher finds albums by free text.
type Searcher interface {
	Search(ctx context.Context, term string, limit, offset int) (musicbrainz.Page, error)
}

// Metadata supplies one album's details, editions and tracks.
type Metadata interface {
	Album(ctx context.Context, id string) (lidarrmeta.Album, error)
}

// Library is the part of the Lidarr client the catalog uses.
type Library interface {
	AlbumByForeignID(ctx context.Context, foreignID string) (*lidarr.AlbumState, error)
	QueuedAlbumIDs(ctx context.Context) (map[int]bool, error)
	Tracks(ctx context.Context, albumID int) ([]lidarr.LibraryTrack, error)
	AddAlbum(ctx context.Context, a lidarr.NewAlbum) error
}

// Catalog combines the sources.
type Catalog struct {
	search   Searcher
	meta     Metadata
	lib      Library
	requests atomic.Pointer[RequestSettings] // nil: requesting is off; replaced when the settings change
}

// RequestSettings is where requested albums go in Lidarr. Giving them to the
// catalog is what turns requesting on.
type RequestSettings struct {
	RootFolder        string
	QualityProfileID  int
	MetadataProfileID int
}

// SetRequests switches requesting on with the given settings, or off with nil.
// It is safe to call while requests are being served: the settings page uses it
// to apply a change without a restart.
func (c *Catalog) SetRequests(s *RequestSettings) {
	if s == nil {
		c.requests.Store(nil)
		return
	}
	cp := *s
	c.requests.Store(&cp)
}

// EnableRequests switches requesting on.
func (c *Catalog) EnableRequests(s RequestSettings) { c.SetRequests(&s) }

// New returns a Catalog.
func New(search Searcher, meta Metadata, lib Library) *Catalog {
	return &Catalog{search: search, meta: meta, lib: lib}
}

// SearchResult is what a search returns. LibraryOK is false when Lidarr could
// not be asked, in which case no album carries a status. HasMore says another
// page can be fetched with the next offset.
type SearchResult struct {
	Albums    []Album
	LibraryOK bool
	HasMore   bool
}

// Search finds one page of albums on MusicBrainz, starting at offset, and
// annotates them with their state in the Lidarr library.
func (c *Catalog) Search(ctx context.Context, term string, offset int) (SearchResult, error) {
	if offset < 0 || offset > MaxOffset {
		return SearchResult{}, ErrInvalidOffset
	}
	page, err := c.search.Search(ctx, term, PageSize, offset)
	if err != nil {
		return SearchResult{}, err
	}
	albums := make([]Album, 0, len(page.Hits))
	for _, h := range page.Hits {
		albums = append(albums, Album{
			ID: h.ID, Title: h.Title, Artist: h.Artist, ArtistID: h.ArtistID,
			Type: h.Type, SecondaryTypes: h.SecondaryTypes, Year: year(h.FirstReleaseDate),
			Cover: "/api/cover/" + h.ID,
		})
	}
	next := offset + len(page.Hits)
	return SearchResult{
		Albums:    albums,
		LibraryOK: c.annotate(ctx, albums),
		HasMore:   len(page.Hits) > 0 && next < page.Total && next <= MaxOffset,
	}, nil
}

// annotate sets each album's Status from Lidarr and reports whether Lidarr
// could be asked. On any Lidarr failure no status is set at all, so a card
// never shows a half-true chip.
func (c *Catalog) annotate(ctx context.Context, albums []Album) bool {
	states := make([]*lidarr.AlbumState, len(albums))
	errs := make([]error, len(albums))
	sem := make(chan struct{}, maxLookups)
	var wg sync.WaitGroup
	for i := range albums {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			states[i], errs[i] = c.lib.AlbumByForeignID(ctx, albums[i].ID)
		}(i)
	}
	wg.Wait()

	owned := false
	for i := range albums {
		if errs[i] != nil {
			return false
		}
		owned = owned || states[i] != nil
	}
	var queued map[int]bool
	if owned {
		var err error
		if queued, err = c.lib.QueuedAlbumIDs(ctx); err != nil {
			return false
		}
	}
	for i, st := range states {
		if st != nil {
			albums[i].Status = DeriveStatus(*st, queued[st.ID])
		}
	}
	return true
}

// DeriveStatus maps an album already in Lidarr to its chip; StatusNone is only
// for albums Lidarr does not have. "Not found" means
// Lidarr has searched for a monitored album and grabbed nothing — it is
// inferred from lastSearchTime, since Lidarr has no explicit "search failed"
// state.
func DeriveStatus(st lidarr.AlbumState, queued bool) Status {
	switch {
	case st.Statistics.TrackFileCount > 0 && st.Statistics.PercentOfTracks >= 100:
		return StatusAvailable
	case queued:
		return StatusProcessing
	case st.Monitored && searched(st.LastSearchTime):
		return StatusNotFound
	case st.Monitored:
		return StatusRequested
	default:
		return StatusInLidarr
	}
}

// searched reports whether Lidarr has ever searched for the album. Lidarr
// serializes "never" as null or the zero time, depending on the version.
func searched(lastSearch string) bool {
	t, err := time.Parse(time.RFC3339, lastSearch)
	return err == nil && t.Year() >= 2000
}

// Detail is the album detail view. Title, Artist and the other descriptive
// fields are only set when MetadataOK; otherwise the frontend keeps what it
// already has from the search result. Status and Library need LibraryOK.
type Detail struct {
	Album
	Released       string       `json:"released,omitempty"`
	Disambiguation string       `json:"disambiguation,omitempty"`
	Overview       string       `json:"overview,omitempty"`
	Genres         []string     `json:"genres"`
	Rating         *Rating      `json:"rating,omitempty"`
	MusicBrainzURL string       `json:"musicbrainzUrl"`
	Library        *LibraryInfo `json:"library,omitempty"`
	LibraryOK      bool         `json:"libraryOk"`
	MetadataOK     bool         `json:"metadataOk"`
	CanRequest     bool         `json:"canRequest"` // requesting is on, Lidarr does not have the album yet, and it can be added
	Tracks         TrackList    `json:"trackList"`
}

// Rating is the community rating, out of 10.
type Rating struct {
	Value float64 `json:"value"`
	Votes int     `json:"votes"`
}

// LibraryInfo describes an album that is in the Lidarr library.
type LibraryInfo struct {
	TracksHave  int   `json:"tracksHave"`
	TracksTotal int   `json:"tracksTotal"`
	SizeOnDisk  int64 `json:"sizeOnDisk"`
}

// Track is one track in an album's track list. HasFile is only set for
// library albums.
type Track struct {
	Disc       int    `json:"disc"`
	Number     string `json:"number"`
	Title      string `json:"title"`
	DurationMs int    `json:"durationMs,omitempty"`
	HasFile    *bool  `json:"hasFile,omitempty"`
}

// TrackSource says where a track list came from.
type TrackSource string

const (
	SourceNone    TrackSource = ""        // no track list could be found
	SourceLibrary TrackSource = "library" // the release Lidarr holds, with which tracks have files
	SourceEdition TrackSource = "edition" // a representative edition of the album
)

// EditionInfo describes the edition a track list came from.
type EditionInfo struct {
	Date          string `json:"date,omitempty"`
	Country       string `json:"country,omitempty"`
	Label         string `json:"label,omitempty"`
	Format        string `json:"format,omitempty"`
	TrackCount    int    `json:"trackCount"`
	EditionsTotal int    `json:"editionsTotal"`
}

// TrackList is an album's tracks.
type TrackList struct {
	Source  TrackSource  `json:"source"`
	Edition *EditionInfo `json:"edition,omitempty"`
	Tracks  []Track      `json:"tracks"`
}

// Detail returns one album's details, library state and track list. Failing
// sources are reported through MetadataOK and LibraryOK, not as an error; the
// only error is an invalid ID.
func (c *Catalog) Detail(ctx context.Context, id string) (Detail, error) {
	if !musicbrainz.ValidID(id) {
		return Detail{}, ErrInvalidID
	}
	d := Detail{
		Album:          Album{ID: id, SecondaryTypes: []string{}, Cover: "/api/cover/" + id},
		Genres:         []string{},
		MusicBrainzURL: "https://musicbrainz.org/release-group/" + id,
		Tracks:         TrackList{Tracks: []Track{}},
	}

	meta, merr := c.meta.Album(ctx, id)
	if merr == nil {
		d.MetadataOK = true
		d.Title, d.Artist, d.ArtistID, d.Type, d.SecondaryTypes = meta.Title, meta.Artist, meta.ArtistID, meta.Type, meta.SecondaryTypes
		d.Year = year(meta.ReleaseDate)
		d.Released, d.Disambiguation, d.Overview = meta.ReleaseDate, meta.Disambiguation, meta.Overview
		if meta.Genres != nil {
			d.Genres = meta.Genres
		}
		if meta.RatingVotes > 0 {
			d.Rating = &Rating{Value: meta.RatingValue, Votes: meta.RatingVotes}
		}
	}

	var albumID int
	st, lerr := c.lib.AlbumByForeignID(ctx, id)
	if lerr == nil {
		d.LibraryOK = true
		if st != nil {
			albumID = st.ID
			queued, qerr := c.lib.QueuedAlbumIDs(ctx)
			if qerr != nil {
				d.LibraryOK = false
			} else {
				d.Status = DeriveStatus(*st, queued[st.ID])
				d.Library = &LibraryInfo{TracksHave: st.Statistics.TrackFileCount, TracksTotal: st.Statistics.TotalTrackCount, SizeOnDisk: st.Statistics.SizeOnDisk}
			}
		}
	}

	d.CanRequest = c.requests.Load() != nil && d.MetadataOK && d.LibraryOK && st == nil

	if albumID != 0 {
		if lt, err := c.lib.Tracks(ctx, albumID); err == nil && len(lt) > 0 {
			d.Tracks = libraryTrackList(lt)
			return d, nil
		}
	}
	if merr == nil {
		d.Tracks = editionTrackList(meta.Editions)
	}
	return d, nil
}

// RequestResult says what a request did.
type RequestResult string

const (
	RequestAdded  RequestResult = "added"  // the artist and album were added to Lidarr, not monitored
	RequestExists RequestResult = "exists" // Lidarr already had the album; nothing changed
)

// Request adds an album, and its artist if need be, to Lidarr without
// monitoring either, so Lidarr searches for and downloads nothing. It is
// idempotent: an album Lidarr already has is reported as such, not added twice,
// and Lidarr is never written to when it cannot first be asked what it has.
func (c *Catalog) Request(ctx context.Context, id string) (RequestResult, error) {
	if !musicbrainz.ValidID(id) {
		return "", ErrInvalidID
	}
	req := c.requests.Load()
	if req == nil {
		return "", ErrRequestsDisabled
	}

	st, err := c.lib.AlbumByForeignID(ctx, id)
	if err != nil {
		return "", fmt.Errorf("%w: asking Lidarr: %v", ErrUnavailable, err)
	}
	if st != nil {
		return RequestExists, nil
	}

	meta, err := c.meta.Album(ctx, id)
	switch {
	case errors.Is(err, lidarrmeta.ErrNotFound):
		return "", ErrNotInMetadata
	case err != nil:
		return "", fmt.Errorf("%w: asking the metadata service: %v", ErrUnavailable, err)
	case meta.ArtistID == "" || !musicbrainz.ValidID(meta.ArtistID):
		return "", ErrNotInMetadata
	}

	err = c.lib.AddAlbum(ctx, lidarr.NewAlbum{
		AlbumID: id, ArtistID: meta.ArtistID,
		RootFolder: req.RootFolder, QualityProfileID: req.QualityProfileID, MetadataProfileID: req.MetadataProfileID,
	})
	switch {
	case err == nil:
		return RequestAdded, nil
	case errors.Is(err, lidarr.ErrAlreadyAdded):
		return RequestExists, nil
	case errors.Is(err, lidarr.ErrUnavailable):
		return "", fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return "", err
}

func libraryTrackList(lt []lidarr.LibraryTrack) TrackList {
	tracks := make([]Track, 0, len(lt))
	for _, t := range lt {
		has := t.HasFile
		tracks = append(tracks, Track{Disc: t.Medium, Number: t.Number, Title: t.Title, DurationMs: t.DurationMs, HasFile: &has})
	}
	return TrackList{Source: SourceLibrary, Tracks: tracks}
}

func editionTrackList(eds []lidarrmeta.Edition) TrackList {
	pick, ok := lidarrmeta.PickEdition(eds)
	if !ok {
		return TrackList{Tracks: []Track{}}
	}
	tracks := make([]Track, 0, len(pick.Tracks))
	for _, t := range pick.Tracks {
		// Number by position: a vinyl or cassette edition's own numbering ("A1")
		// would look odd next to the CD-style lists everywhere else.
		number := t.Number
		if t.Position > 0 {
			number = strconv.Itoa(t.Position)
		}
		tracks = append(tracks, Track{Disc: t.Disc, Number: number, Title: t.Title, DurationMs: t.DurationMs})
	}
	return TrackList{
		Source: SourceEdition,
		Edition: &EditionInfo{
			Date: pick.Date, Country: pick.Country, Label: pick.Label, Format: pick.Format,
			TrackCount: len(pick.Tracks), EditionsTotal: len(eds),
		},
		Tracks: tracks,
	}
}

// year pulls the year out of an ISO date ("1997", "1997-05-21").
func year(date string) int {
	if len(date) < 4 {
		return 0
	}
	n := 0
	for _, r := range date[:4] {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}
