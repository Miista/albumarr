// Package catalog is albumarr's model of an album. What exists comes from two
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
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Miista/albumarr/internal/coverart"
	"github.com/Miista/albumarr/internal/lidarr"
	"github.com/Miista/albumarr/internal/lidarrmeta"
	"github.com/Miista/albumarr/internal/musicbrainz"
)

// ErrInvalidID is returned for an album ID that is not a MusicBrainz UUID.
var ErrInvalidID = errors.New("catalog: invalid album id")

// ErrInvalidOffset is returned for a search offset outside 0..MaxOffset.
var ErrInvalidOffset = errors.New("catalog: invalid search offset")

// ErrRequestsDisabled is returned when albumarr has not been told where to put
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
	StatusPending    Status = "pending"    // asked for, waiting for an approver; never set by DeriveStatus
	StatusInLidarr   Status = "inlidarr"   // in Lidarr, not monitored: Lidarr is not looking for it; a request makes it
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
	Group          string   `json:"group,omitempty"` // on an artist's page: albums, eps, singles or more

	releaseDate string // for ordering an artist's page; not sent
}

// Searcher finds albums and artists by free text.
type Searcher interface {
	Search(ctx context.Context, term string, limit, offset int) (musicbrainz.Page, error)
	SearchArtists(ctx context.Context, term string, limit int) ([]musicbrainz.ArtistHit, error)
}

// Metadata supplies one album's details, editions and tracks, and an artist's
// release groups.
type Metadata interface {
	Album(ctx context.Context, id string) (lidarrmeta.Album, error)
	Artist(ctx context.Context, id string) (lidarrmeta.Artist, error)
}

// Photos fetches an artist's picture from the address the metadata service gave.
type Photos interface {
	Photo(ctx context.Context, artistID, rawURL string, width int) ([]byte, string, error)
}

// Library is the part of the Lidarr client the catalog uses.
type Library interface {
	AlbumByForeignID(ctx context.Context, foreignID string) (*lidarr.AlbumState, error)
	QueuedAlbumIDs(ctx context.Context) (map[int]bool, error)
	ArtistAlbumStates(ctx context.Context, artistMBID string) (map[string]lidarr.AlbumState, error)
	Queue(ctx context.Context) (map[int]lidarr.Download, error)
	Tracks(ctx context.Context, albumID int) ([]lidarr.LibraryTrack, error)
	AddAlbum(ctx context.Context, a lidarr.NewAlbum) error
	MetadataProfileByName(ctx context.Context, name string) (int, error)
	MonitorAlbum(ctx context.Context, albumID int) error
	ChooseRelease(ctx context.Context, albumID int) error
	SearchAlbum(ctx context.Context, albumID int) error
}

// Catalog combines the sources.
type Catalog struct {
	search   Searcher
	meta     Metadata
	lib      Library
	photos   Photos                          // nil: artists have no pictures
	requests atomic.Pointer[RequestSettings] // nil: requesting is off; replaced when the settings change
}

// RequestSettings is where requested albums go in Lidarr. Giving them to the
// catalog is what turns requesting on.
type RequestSettings struct {
	RootFolder       string
	QualityProfileID int
	// MetadataProfile is the name of the metadata profile new artists get. It is a
	// name, not an ID, and is looked up in Lidarr when an album is added.
	MetadataProfile string
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
	case complete(st):
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

	d.CanRequest = c.requests.Load() != nil && d.LibraryOK && ((st == nil && d.MetadataOK) || (st != nil && wanted(*st)))

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

// RequestsEnabled reports whether requesting is switched on (the root folder
// and both profiles are set and valid).
func (c *Catalog) RequestsEnabled() bool { return c.requests.Load() != nil }

// RequestResult says what a request did.
type RequestResult string

const (
	RequestAdded RequestResult = "added" // the album was added to Lidarr, monitored, and a search was started
	// RequestAddedUnsearched means the album was added and is monitored, but the
	// search could not be started. Lidarr will not search for it by itself.
	RequestAddedUnsearched RequestResult = "added_unsearched"
	RequestExists          RequestResult = "exists" // Lidarr already had the album; nothing changed
)

// Wanted reports whether requesting the album would change anything in Lidarr:
// Lidarr does not have it, or has it but is not looking for it and it is not
// complete. It is how a request learns "already requested" without recording one.
// Lidarr being unable to answer is ErrUnavailable.
func (c *Catalog) Wanted(ctx context.Context, id string) (bool, error) {
	if !musicbrainz.ValidID(id) {
		return false, ErrInvalidID
	}
	st, err := c.lib.AlbumByForeignID(ctx, id)
	if err != nil {
		return false, fmt.Errorf("%w: asking Lidarr: %v", ErrUnavailable, err)
	}
	return st == nil || wanted(*st), nil
}

// Available reports whether Lidarr has every track of the album on disk. Lidarr
// being unable to answer is ErrUnavailable; an album it does not have is simply
// not available.
func (c *Catalog) Available(ctx context.Context, id string) (bool, error) {
	if !musicbrainz.ValidID(id) {
		return false, ErrInvalidID
	}
	st, err := c.lib.AlbumByForeignID(ctx, id)
	if err != nil {
		return false, fmt.Errorf("%w: asking Lidarr: %v", ErrUnavailable, err)
	}
	return st != nil && complete(*st), nil
}

// Request adds an album to Lidarr, monitored, with its artist if need be
// (unmonitored), and starts a search for it, so Lidarr looks for and downloads
// it. Two calls, not one flag: Lidarr only searches on add when it also adds the
// artist. It is idempotent: an album Lidarr already has is reported as such, not
// added or searched again, and Lidarr is never written to when it cannot first
// be asked what it has.
//
// The artist's ID is the one MusicBrainz gave with the album. Lidarr only insists
// that one is present: it works out the real artist from the album (checked: a
// different artist's ID, or a made-up one, still files the album under its own
// artist). So nothing here asks the metadata service; an album Lidarr's own
// metadata does not know is refused by Lidarr, and reported as ErrNotInMetadata.
func (c *Catalog) Request(ctx context.Context, id, artistID string) (RequestResult, error) {
	if !musicbrainz.ValidID(id) || !musicbrainz.ValidID(artistID) {
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
		// In Lidarr but not wanted: added earlier without monitoring, or imported
		// with its artist. Requesting it is what makes Lidarr look for it.
		if wanted(*st) {
			return c.monitorAndSearch(ctx, st), nil
		}
		return RequestExists, nil
	}

	profile, err := c.lib.MetadataProfileByName(ctx, req.MetadataProfile)
	switch {
	case errors.Is(err, lidarr.ErrBadSettings):
		return "", err // nothing has been written
	case err != nil:
		return "", fmt.Errorf("%w: looking up the metadata profile: %v", ErrUnavailable, err)
	}

	err = c.lib.AddAlbum(ctx, lidarr.NewAlbum{
		AlbumID: id, ArtistID: artistID,
		RootFolder: req.RootFolder, QualityProfileID: req.QualityProfileID, MetadataProfileID: profile,
	})
	switch {
	case err == nil:
		return c.startSearch(ctx, id), nil
	case errors.Is(err, lidarr.ErrAlreadyAdded):
		return RequestExists, nil
	case errors.Is(err, lidarr.ErrAlbumNotFound):
		return "", ErrNotInMetadata
	case errors.Is(err, lidarr.ErrUnavailable):
		return "", fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return "", err
}

// startSearch finishes a request for an album that was just added: make sure it
// is monitored, then search for it. Its Lidarr ID is only known by asking again.
// Lidarr can leave a new artist's album unmonitored however the add asked, and
// an unmonitored album is not wanted, so it is checked, not assumed. The album
// is in Lidarr either way, so a failure here is reported as a result, not an
// error: the caller must not tell the user the request failed, and must not
// retry the add.
func (c *Catalog) startSearch(ctx context.Context, id string) RequestResult {
	st, err := c.lib.AlbumByForeignID(ctx, id)
	if err != nil || st == nil {
		return RequestAddedUnsearched
	}
	return c.monitorAndSearch(ctx, st)
}

// wanted reports whether a request would change anything for an album Lidarr
// already has: it is not monitored and not already complete on disk. An album
// that is monitored is already being looked for, and one that is complete has
// nothing to look for.
func wanted(st lidarr.AlbumState) bool { return !st.Monitored && !complete(st) }

// complete reports whether every track of the album has a file.
func complete(st lidarr.AlbumState) bool {
	return st.Statistics.TrackFileCount > 0 && st.Statistics.PercentOfTracks >= 100
}

// monitorAndSearch makes Lidarr want an album that is in Lidarr, and look for it.
func (c *Catalog) monitorAndSearch(ctx context.Context, st *lidarr.AlbumState) RequestResult {
	if !st.Monitored {
		if err := c.lib.MonitorAlbum(ctx, st.ID); err != nil {
			return RequestAddedUnsearched
		}
	}
	if st.Statistics.TrackFileCount == 0 {
		// Best effort: Lidarr's own pick (the biggest edition) can make every
		// ordinary download fail to import, but searching with it is still
		// better than not searching. Never changed under files already imported.
		_ = c.lib.ChooseRelease(ctx, st.ID)
	}
	if err := c.lib.SearchAlbum(ctx, st.ID); err != nil {
		return RequestAddedUnsearched
	}
	return RequestAdded
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

// Progress is where an album stands in Lidarr, for the requests list: the same
// chip the search shows, plus which stage the download is at when there is one.
type Progress struct {
	Status Status       `json:"status"`
	Stage  lidarr.Stage `json:"stage,omitempty"`
}

const (
	progressMax  = 50 // albums asked about at once; the rest simply have no progress
	progressPara = 8
)

// Progress says where each of the albums stands. Albums Lidarr cannot be asked
// about are left out, never guessed. Lidarr being down is an empty answer.
func (c *Catalog) Progress(ctx context.Context, ids []string) map[string]Progress {
	out := map[string]Progress{}
	if len(ids) > progressMax {
		ids = ids[:progressMax]
	}
	states := make([]*lidarr.AlbumState, len(ids))
	ok := make([]bool, len(ids))
	sem := make(chan struct{}, progressPara)
	var wg sync.WaitGroup
	for i, id := range ids {
		if !musicbrainz.ValidID(id) {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			st, err := c.lib.AlbumByForeignID(ctx, id)
			states[i], ok[i] = st, err == nil
		}()
	}
	wg.Wait()
	var queue map[int]lidarr.Download
	var qerr error
	for _, st := range states {
		if st != nil && !complete(*st) {
			queue, qerr = c.lib.Queue(ctx)
			break
		}
	}
	for i, id := range ids {
		st := states[i]
		if !ok[i] || st == nil {
			continue
		}
		if complete(*st) {
			out[id] = Progress{Status: StatusAvailable}
			continue
		}
		if qerr != nil {
			continue
		}
		d, queued := queue[st.ID]
		p := Progress{Status: DeriveStatus(*st, queued)}
		if queued {
			p.Stage = d.Stage
		}
		out[id] = p
	}
	return out
}

// ArtistHit is one artist in the results of a search.
type ArtistHit struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Disambiguation string `json:"disambiguation,omitempty"`
	Type           string `json:"type,omitempty"`
	Country        string `json:"country,omitempty"`
}

const (
	maxArtistHits  = 6
	minArtistScore = 50 // MusicBrainz scores 0-100; weaker matches are noise next to the album results
)

// SearchArtists finds the artists whose name matches term, best first. It only
// offers a way into their pages: nothing about the request flow changes.
func (c *Catalog) SearchArtists(ctx context.Context, term string) ([]ArtistHit, error) {
	hits, err := c.search.SearchArtists(ctx, term, maxArtistHits)
	if err != nil {
		return nil, err
	}
	out := make([]ArtistHit, 0, len(hits))
	for _, h := range hits {
		if h.Score < minArtistScore {
			continue
		}
		out = append(out, ArtistHit{ID: h.ID, Name: h.Name, Disambiguation: h.Disambiguation, Type: h.Type, Country: h.Country})
	}
	return out, nil
}

// ArtistPage is an artist and their release groups, each with its state in Lidarr.
type ArtistPage struct {
	ID             string  `json:"id"`
	Name           string  `json:"name"`
	Disambiguation string  `json:"disambiguation,omitempty"`
	Type           string  `json:"type,omitempty"`
	Albums         []Album `json:"albums"`
	LibraryOK      bool    `json:"libraryOk"`
	Photo          string  `json:"photo,omitempty"` // where to fetch the artist's picture, when there is one
}

// groupOf sorts a release group into one of the page's filters: studio albums,
// EPs, singles, and everything else (live albums, compilations, broadcasts, …).
func groupOf(primary string, secondary []string) string {
	switch {
	case primary == "Album" && len(secondary) == 0:
		return "albums"
	case primary == "EP":
		return "eps"
	case primary == "Single":
		return "singles"
	}
	return "more"
}

// Artist returns an artist's release groups, newest first, annotated with what
// Lidarr has. A Lidarr that cannot be asked leaves the albums without a status
// (LibraryOK false), as in a search.
func (c *Catalog) Artist(ctx context.Context, id string) (ArtistPage, error) {
	if !musicbrainz.ValidID(id) {
		return ArtistPage{}, ErrInvalidID
	}
	a, err := c.meta.Artist(ctx, id)
	switch {
	case errors.Is(err, lidarrmeta.ErrNotFound):
		return ArtistPage{}, ErrNotInMetadata
	case err != nil:
		return ArtistPage{}, fmt.Errorf("%w: asking the metadata service: %v", ErrUnavailable, err)
	}
	page := ArtistPage{ID: a.ID, Name: a.Name, Disambiguation: a.Disambiguation, Type: a.Type, Albums: make([]Album, 0, len(a.Albums))}
	if c.photos != nil && a.PhotoURL() != "" {
		page.Photo = "/api/artist/" + id + "/photo"
	}
	for _, x := range a.Albums {
		page.Albums = append(page.Albums, Album{
			ID: x.ID, Title: x.Title, Artist: a.Name, ArtistID: a.ID, Type: x.Type, SecondaryTypes: x.SecondaryTypes,
			Year: year(x.ReleaseDate), Cover: "/api/cover/" + x.ID, Group: groupOf(x.Type, x.SecondaryTypes),
			releaseDate: x.ReleaseDate,
		})
	}
	sort.SliceStable(page.Albums, func(i, j int) bool {
		di, dj := page.Albums[i].releaseDate, page.Albums[j].releaseDate
		if (di == "") != (dj == "") {
			return di != ""
		}
		if di != dj {
			return di > dj
		}
		return page.Albums[i].Title < page.Albums[j].Title
	})

	states, err := c.lib.ArtistAlbumStates(ctx, id)
	if err != nil {
		return page, nil
	}
	var queued map[int]bool
	for _, st := range states {
		if !complete(st) {
			if queued, err = c.lib.QueuedAlbumIDs(ctx); err != nil {
				return page, nil
			}
			break
		}
	}
	page.LibraryOK = true
	for i := range page.Albums {
		if st, ok := states[page.Albums[i].ID]; ok {
			page.Albums[i].Status = DeriveStatus(st, queued[st.ID])
		}
	}
	return page, nil
}

// SetPhotos turns on artists' pictures.
func (c *Catalog) SetPhotos(p Photos) { c.photos = p }

// Photo sizes the page can ask for: a thumbnail for a search result, and the
// picture for the artist's page.
const (
	PhotoThumb = 96
	PhotoFull  = 480
)

// ArtistPhoto returns the artist's picture at the given size (PhotoThumb or
// PhotoFull), or coverart.ErrNoCover when there is none. It never takes an address from the caller: the metadata service's own
// answer says where the picture is, and the fetcher accepts only the image CDN.
func (c *Catalog) ArtistPhoto(ctx context.Context, id string, size int) ([]byte, string, error) {
	if !musicbrainz.ValidID(id) {
		return nil, "", ErrInvalidID
	}
	if size != PhotoThumb && size != PhotoFull {
		return nil, "", coverart.ErrInvalid
	}
	if c.photos == nil {
		return nil, "", coverart.ErrNoCover
	}
	a, err := c.meta.Artist(ctx, id)
	switch {
	case errors.Is(err, lidarrmeta.ErrNotFound):
		return nil, "", coverart.ErrNoCover
	case err != nil:
		return nil, "", fmt.Errorf("%w: asking the metadata service: %v", ErrUnavailable, err)
	}
	u := a.PhotoURL()
	if u == "" {
		return nil, "", coverart.ErrNoCover
	}
	return c.photos.Photo(ctx, id, u, size)
}
