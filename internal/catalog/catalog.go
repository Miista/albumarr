// Package catalog turns Lidarr's album lookup into what the search page shows:
// one card per album, with a status chip derived from the album's state in the
// Lidarr library and download queue.
package catalog

import (
	"context"
	"net/url"
	"sync"
	"time"

	"github.com/Miista/albumreq/internal/lidarr"
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
	Cover          string   `json:"cover,omitempty"`
	Status         Status   `json:"status"`
}

// Source is the part of the Lidarr client the catalog uses.
type Source interface {
	Lookup(ctx context.Context, term string) ([]lidarr.LookupAlbum, error)
	Albums(ctx context.Context, ids []int) (map[int]lidarr.AlbumState, error)
	QueuedAlbumIDs(ctx context.Context) (map[int]bool, error)
}

// Catalog searches Lidarr and annotates the results with library status.
type Catalog struct {
	src Source
}

// New returns a Catalog backed by src.
func New(src Source) *Catalog {
	return &Catalog{src: src}
}

// Search looks term up in Lidarr and returns one Album per hit, in Lidarr's
// relevance order. State and queue are only fetched when at least one hit is
// already in the library.
func (c *Catalog) Search(ctx context.Context, term string) ([]Album, error) {
	hits, err := c.src.Lookup(ctx, term)
	if err != nil {
		return nil, err
	}

	var ids []int
	for _, h := range hits {
		if h.ID != nil {
			ids = append(ids, *h.ID)
		}
	}

	var (
		states  map[int]lidarr.AlbumState
		queued  map[int]bool
		errS    error
		errQ    error
		wg      sync.WaitGroup
		inLidar = len(ids) > 0
	)
	if inLidar {
		wg.Add(2)
		go func() { defer wg.Done(); states, errS = c.src.Albums(ctx, ids) }()
		go func() { defer wg.Done(); queued, errQ = c.src.QueuedAlbumIDs(ctx) }()
		wg.Wait()
		if errS != nil {
			return nil, errS
		}
		if errQ != nil {
			return nil, errQ
		}
	}

	albums := make([]Album, 0, len(hits))
	for _, h := range hits {
		a := Album{
			ID:             h.ForeignAlbumID,
			Title:          h.Title,
			Artist:         h.Artist.Name,
			ArtistID:       h.Artist.ForeignID,
			Type:           h.AlbumType,
			SecondaryTypes: h.SecondaryTypes,
			Year:           year(h.ReleaseDate),
		}
		if a.SecondaryTypes == nil {
			a.SecondaryTypes = []string{}
		}
		if p := h.CoverPath(); p != "" {
			a.Cover = "/api/cover?p=" + url.QueryEscape(p)
		}
		if h.ID != nil {
			if st, ok := states[*h.ID]; ok {
				a.Status = DeriveStatus(st, queued[*h.ID])
			}
		}
		albums = append(albums, a)
	}
	return albums, nil
}

// DeriveStatus maps an album already in Lidarr to its chip. "Not found" means
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
		return StatusNone
	}
}

// searched reports whether Lidarr has ever searched for the album. Lidarr
// serializes "never" as null or the zero time, depending on the version.
func searched(lastSearch string) bool {
	t, err := time.Parse(time.RFC3339, lastSearch)
	return err == nil && t.Year() >= 2000
}

// year pulls the year out of an ISO release date ("2001-10-23T00:00:00Z").
func year(date string) int {
	t, err := time.Parse(time.RFC3339, date)
	if err != nil {
		return 0
	}
	return t.Year()
}
