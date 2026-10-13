package catalog

import (
	"context"
	"errors"
	"fmt"
	"github.com/Miista/albumreq/internal/coverart"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/Miista/albumreq/internal/lidarr"
	"github.com/Miista/albumreq/internal/lidarrmeta"
	"github.com/Miista/albumreq/internal/musicbrainz"
)

const (
	id1 = "b1392450-e666-3926-a536-22c65f834433"
	id2 = "59f47b29-50dd-4335-a81b-02297c790208"
	id3 = "00000000-0000-0000-0000-000000000003"
)

func state(id int, monitored bool, fileCount int, percent float64, lastSearch string) lidarr.AlbumState {
	var st lidarr.AlbumState
	st.ID = id
	st.Monitored = monitored
	st.LastSearchTime = lastSearch
	st.Statistics.TrackFileCount = fileCount
	st.Statistics.PercentOfTracks = percent
	return st
}

const (
	searchedAt = "2026-10-10T15:16:09Z"
	never      = "0001-01-01T00:00:00Z"
)

func TestDeriveStatus(t *testing.T) {
	cases := []struct {
		name   string
		st     lidarr.AlbumState
		queued bool
		want   Status
	}{
		{"complete is available", state(1, false, 12, 100, searchedAt), false, StatusAvailable},
		{"complete beats queued", state(1, true, 12, 100, searchedAt), true, StatusAvailable},
		{"complete beats monitored+searched", state(1, true, 12, 100, searchedAt), false, StatusAvailable},
		{"queued is processing", state(1, true, 0, 0, never), true, StatusProcessing},
		{"queued while only partly present is processing", state(1, true, 4, 33, searchedAt), true, StatusProcessing},
		{"monitored, never searched is requested", state(1, true, 0, 0, never), false, StatusRequested},
		{"monitored, empty lastSearchTime is requested", state(1, true, 0, 0, ""), false, StatusRequested},
		{"monitored and searched is not found", state(1, true, 0, 0, searchedAt), false, StatusNotFound},
		{"partial files, monitored, searched is not found", state(1, true, 4, 33, searchedAt), false, StatusNotFound},
		// Known to Lidarr but nothing asked for: added, not monitored. Never "available"
		// without every track on disk, and never mistaken for "not in Lidarr".
		{"unmonitored, no files is in Lidarr", state(1, false, 0, 0, searchedAt), false, StatusInLidarr},
		{"unmonitored, never searched is in Lidarr", state(1, false, 0, 0, never), false, StatusInLidarr},
		{"unmonitored partial is in Lidarr", state(1, false, 4, 33, searchedAt), false, StatusInLidarr},
		{"files but zero percent is not available", state(1, false, 1, 0, searchedAt), false, StatusInLidarr},
		{"100 percent with no files is not available", state(1, false, 0, 100, searchedAt), false, StatusInLidarr},
	}
	for _, c := range cases {
		if got := DeriveStatus(c.st, c.queued); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestYear(t *testing.T) {
	for in, want := range map[string]int{"1997": 1997, "1997-05-21": 1997, "2001-10": 2001, "": 0, "97": 0, "abcd": 0, "19x7": 0, "-199": 0} {
		if got := year(in); got != want {
			t.Errorf("year(%q) = %d, want %d", in, got, want)
		}
	}
}

// --- fakes ---

type fakeSearch struct {
	hits    []musicbrainz.Hit
	artists []musicbrainz.ArtistHit
	total   int // total matches; defaults to len(hits)
	err     error
	limit   int
	offset  int
	calls   int
}

func (f *fakeSearch) Search(_ context.Context, _ string, limit, offset int) (musicbrainz.Page, error) {
	f.calls++
	f.limit, f.offset = limit, offset
	total := f.total
	if total == 0 {
		total = len(f.hits)
	}
	return musicbrainz.Page{Hits: f.hits, Total: total}, f.err
}

func (f *fakeSearch) SearchArtists(context.Context, string, int) ([]musicbrainz.ArtistHit, error) {
	f.calls++
	return f.artists, f.err
}

func (f *fakeMeta) Artist(context.Context, string) (lidarrmeta.Artist, error) {
	f.calls++
	return f.artist, f.err
}

func (f *fakeLib) ArtistAlbumStates(context.Context, string) (map[string]lidarr.AlbumState, error) {
	out := map[string]lidarr.AlbumState{}
	for id, st := range f.states {
		if st != nil {
			out[id] = *st
		}
	}
	return out, f.artistStatesErr
}

type fakeMeta struct {
	album  lidarrmeta.Album
	artist lidarrmeta.Artist
	err    error
	calls  int
}

func (f *fakeMeta) Album(context.Context, string) (lidarrmeta.Album, error) {
	f.calls++
	return f.album, f.err
}

type fakeLib struct {
	states          map[string]*lidarr.AlbumState
	queued          map[int]bool
	tracks          []lidarr.LibraryTrack
	albumErr        error
	queueErr        error
	artistStatesErr error
	tracksErr       error
	addErr          error
	searchErr       error
	monitorErr      error
	profileID       int // what MetadataProfileByName answers; 0 means addedProfileID
	profileErr      error
	profileAsked    []string
	newMonitored    bool // whether the fake's newly added albums come back monitored
	monitored       []int
	added           []lidarr.NewAlbum
	searched        []int
	albumCalls      atomic.Int32 // AlbumByForeignID runs from several goroutines
	queueCalls      atomic.Int32
	downloads       map[int]lidarr.Download
	tracksCalls     atomic.Int32
}

func (f *fakeLib) AlbumByForeignID(_ context.Context, id string) (*lidarr.AlbumState, error) {
	f.albumCalls.Add(1)
	return f.states[id], f.albumErr
}

func (f *fakeLib) QueuedAlbumIDs(context.Context) (map[int]bool, error) {
	f.queueCalls.Add(1)
	return f.queued, f.queueErr
}

func (f *fakeLib) Queue(context.Context) (map[int]lidarr.Download, error) {
	f.queueCalls.Add(1)
	out := map[int]lidarr.Download{}
	for id := range f.queued {
		out[id] = f.downloads[id]
	}
	return out, f.queueErr
}

// addedProfileID is the metadata profile ID the fake finds for "None".
const addedProfileID = 77

func (f *fakeLib) MetadataProfileByName(_ context.Context, name string) (int, error) {
	f.profileAsked = append(f.profileAsked, name)
	if f.profileErr != nil {
		return 0, f.profileErr
	}
	if f.profileID != 0 {
		return f.profileID, nil
	}
	return addedProfileID, nil
}

// addedID is the Lidarr ID the fake gives a newly added album.
const addedID = 42

func (f *fakeLib) AddAlbum(_ context.Context, a lidarr.NewAlbum) error {
	if f.addErr == nil {
		f.added = append(f.added, a)
		if f.states != nil {
			f.states[a.AlbumID] = stp(state(addedID, f.newMonitored, 0, 0, ""))
		}
	}
	return f.addErr
}

func (f *fakeLib) MonitorAlbum(_ context.Context, id int) error {
	if f.monitorErr == nil {
		f.monitored = append(f.monitored, id)
	}
	return f.monitorErr
}

func (f *fakeLib) SearchAlbum(_ context.Context, id int) error {
	if f.searchErr == nil {
		f.searched = append(f.searched, id)
	}
	return f.searchErr
}

func (f *fakeLib) Tracks(context.Context, int) ([]lidarr.LibraryTrack, error) {
	f.tracksCalls.Add(1)
	return f.tracks, f.tracksErr
}

func hit(id, title string) musicbrainz.Hit {
	return musicbrainz.Hit{ID: id, Title: title, Artist: "Radiohead", ArtistID: "artist-1", Type: "Album", SecondaryTypes: []string{}, FirstReleaseDate: "1997-05-21"}
}

func stp(s lidarr.AlbumState) *lidarr.AlbumState { return &s }

// --- search ---

func TestSearchMapsHitsAndAnnotatesStatus(t *testing.T) {
	fs := &fakeSearch{hits: []musicbrainz.Hit{hit(id1, "Owned"), hit(id2, "Downloading"), hit(id3, "Unknown to Lidarr")}}
	lib := &fakeLib{
		states: map[string]*lidarr.AlbumState{
			id1: stp(state(1, false, 10, 100, "")),
			id2: stp(state(2, true, 0, 0, "")),
		},
		queued: map[int]bool{2: true},
	}
	res, err := New(fs, &fakeMeta{}, lib).Search(context.Background(), "x", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !res.LibraryOK || len(res.Albums) != 3 {
		t.Fatalf("unexpected result: %+v", res)
	}
	for i, want := range []Status{StatusAvailable, StatusProcessing, StatusNone} {
		if res.Albums[i].Status != want {
			t.Errorf("album %d (%s): status %q, want %q", i, res.Albums[i].Title, res.Albums[i].Status, want)
		}
	}
	a := res.Albums[0]
	if a.ID != id1 || a.Artist != "Radiohead" || a.Year != 1997 || a.Type != "Album" || a.Cover != "/api/cover/"+id1 {
		t.Errorf("fields not mapped: %+v", a)
	}
	if fs.limit != PageSize || fs.offset != 0 {
		t.Errorf("search asked for limit=%d offset=%d, want %d and 0", fs.limit, fs.offset, PageSize)
	}
}

func TestSearchSkipsTheQueueWhenNothingIsInLidarr(t *testing.T) {
	lib := &fakeLib{states: map[string]*lidarr.AlbumState{}}
	if _, err := New(&fakeSearch{hits: []musicbrainz.Hit{hit(id1, "New")}}, &fakeMeta{}, lib).Search(context.Background(), "x", 0); err != nil {
		t.Fatal(err)
	}
	if lib.queueCalls.Load() != 0 {
		t.Errorf("queue fetched %d times, want 0", lib.queueCalls.Load())
	}
}

func TestSearchDegradesWhenLidarrFails(t *testing.T) {
	hits := []musicbrainz.Hit{hit(id1, "Owned"), hit(id2, "Other")}
	owned := map[string]*lidarr.AlbumState{id1: stp(state(1, false, 10, 100, ""))}
	for name, lib := range map[string]*fakeLib{
		"album lookup fails": {states: owned, albumErr: errors.New("boom")},
		"queue fails":        {states: owned, queueErr: errors.New("boom")},
	} {
		res, err := New(&fakeSearch{hits: hits}, &fakeMeta{}, lib).Search(context.Background(), "x", 0)
		if err != nil {
			t.Fatalf("%s: a Lidarr failure must not fail the search: %v", name, err)
		}
		if res.LibraryOK || len(res.Albums) != 2 {
			t.Errorf("%s: LibraryOK=%v albums=%d, want false and 2", name, res.LibraryOK, len(res.Albums))
		}
		for _, a := range res.Albums {
			if a.Status != StatusNone {
				t.Errorf("%s: %s carries status %q although Lidarr was unavailable", name, a.Title, a.Status)
			}
		}
	}
}

func TestSearchFailsWhenMusicBrainzFails(t *testing.T) {
	boom := errors.New("boom")
	if _, err := New(&fakeSearch{err: boom}, &fakeMeta{}, &fakeLib{}).Search(context.Background(), "x", 0); !errors.Is(err, boom) {
		t.Errorf("err = %v, want boom", err)
	}
}

// --- detail ---

func okcMeta() lidarrmeta.Album {
	tr := func(n int, title string) lidarrmeta.Track {
		return lidarrmeta.Track{Disc: 1, Number: string(rune('0' + n)), Position: n, Title: title, DurationMs: 1000 * n}
	}
	return lidarrmeta.Album{
		ID: id1, Title: "OK Computer", Artist: "Radiohead", ArtistID: "artist-1", Type: "Album", SecondaryTypes: []string{},
		ReleaseDate: "1997-05-21", Genres: []string{"Rock"}, RatingValue: 9.1, RatingVotes: 89,
		Editions: []lidarrmeta.Edition{
			{ID: "e1", Date: "1997-06-16", Country: "GB", Label: "Parlophone", Format: "CD", Tracks: []lidarrmeta.Track{tr(1, "Airbag"), tr(2, "Paranoid Android")}},
			{ID: "e2", Date: "1998", Tracks: []lidarrmeta.Track{tr(1, "Airbag"), tr(2, "Paranoid Android")}},
			{ID: "e3", Date: "2017", Tracks: []lidarrmeta.Track{tr(1, "a"), tr(2, "b"), tr(3, "c")}},
		},
	}
}

func TestDetailForAnAlbumNotInTheLibraryUsesAnEditionsTracks(t *testing.T) {
	lib := &fakeLib{states: map[string]*lidarr.AlbumState{}}
	d, err := New(&fakeSearch{}, &fakeMeta{album: okcMeta()}, lib).Detail(context.Background(), id1)
	if err != nil {
		t.Fatal(err)
	}
	if !d.MetadataOK || !d.LibraryOK || d.Title != "OK Computer" || d.Artist != "Radiohead" || d.Year != 1997 || d.Released != "1997-05-21" {
		t.Errorf("descriptive fields wrong: %+v", d)
	}
	if d.Rating == nil || d.Rating.Value != 9.1 || d.Rating.Votes != 89 || len(d.Genres) != 1 {
		t.Errorf("rating or genres wrong: %+v", d)
	}
	if d.MusicBrainzURL != "https://musicbrainz.org/release-group/"+id1 || d.Cover != "/api/cover/"+id1 {
		t.Errorf("links wrong: %q %q", d.MusicBrainzURL, d.Cover)
	}
	if d.Status != StatusNone || d.Library != nil {
		t.Errorf("an album Lidarr does not have must carry no status or library info: %q %+v", d.Status, d.Library)
	}
	tl := d.Tracks
	if tl.Source != SourceEdition || len(tl.Tracks) != 2 || tl.Edition == nil || tl.Edition.EditionsTotal != 3 || tl.Edition.Date != "1997-06-16" || tl.Edition.Label != "Parlophone" {
		t.Errorf("track list should come from the most common, earliest edition: %+v", tl)
	}
	if tl.Tracks[0].HasFile != nil {
		t.Error("tracks of an album you do not have must not claim a file state")
	}
	if lib.tracksCalls.Load() != 0 {
		t.Error("Lidarr's tracks must not be asked for an album it does not have")
	}
}

func TestDetailForALibraryAlbumUsesLidarrsTracksAndFileState(t *testing.T) {
	lib := &fakeLib{
		states: map[string]*lidarr.AlbumState{id1: stp(func() lidarr.AlbumState {
			s := state(7, false, 1, 50, "")
			s.Statistics.TotalTrackCount = 2
			s.Statistics.SizeOnDisk = 123456
			return s
		}())},
		queued: map[int]bool{},
		tracks: []lidarr.LibraryTrack{
			{Medium: 1, Number: "1", Title: "Airbag", DurationMs: 284400, HasFile: true},
			{Medium: 1, Number: "2", Title: "Missing", HasFile: false},
		},
	}
	d, err := New(&fakeSearch{}, &fakeMeta{album: okcMeta()}, lib).Detail(context.Background(), id1)
	if err != nil {
		t.Fatal(err)
	}
	if d.Library == nil || d.Library.TracksHave != 1 || d.Library.TracksTotal != 2 || d.Library.SizeOnDisk != 123456 {
		t.Errorf("library info wrong: %+v", d.Library)
	}
	if d.Tracks.Source != SourceLibrary || d.Tracks.Edition != nil || len(d.Tracks.Tracks) != 2 {
		t.Fatalf("track list should come from Lidarr: %+v", d.Tracks)
	}
	if h := d.Tracks.Tracks; h[0].HasFile == nil || !*h[0].HasFile || h[1].HasFile == nil || *h[1].HasFile {
		t.Errorf("file state wrong: %+v", h)
	}
}

func TestDetailKeepsWorkingWhenASourceIsDown(t *testing.T) {
	t.Run("metadata service down, album not owned", func(t *testing.T) {
		d, err := New(&fakeSearch{}, &fakeMeta{err: errors.New("boom")}, &fakeLib{states: map[string]*lidarr.AlbumState{}}).Detail(context.Background(), id1)
		if err != nil || d.MetadataOK || !d.LibraryOK || len(d.Tracks.Tracks) != 0 || d.Tracks.Source != SourceNone || d.MusicBrainzURL == "" {
			t.Errorf("got %+v, %v", d, err)
		}
	})
	t.Run("metadata service down, album owned: library tracks still show", func(t *testing.T) {
		lib := &fakeLib{states: map[string]*lidarr.AlbumState{id1: stp(state(7, false, 2, 100, ""))}, queued: map[int]bool{},
			tracks: []lidarr.LibraryTrack{{Number: "1", Title: "Airbag", HasFile: true}}}
		d, err := New(&fakeSearch{}, &fakeMeta{err: errors.New("boom")}, lib).Detail(context.Background(), id1)
		if err != nil || d.MetadataOK || d.Status != StatusAvailable || d.Tracks.Source != SourceLibrary || len(d.Tracks.Tracks) != 1 {
			t.Errorf("got %+v, %v", d, err)
		}
	})
	t.Run("Lidarr down: no status, edition tracks still show", func(t *testing.T) {
		d, err := New(&fakeSearch{}, &fakeMeta{album: okcMeta()}, &fakeLib{albumErr: errors.New("boom")}).Detail(context.Background(), id1)
		if err != nil || !d.MetadataOK || d.LibraryOK || d.Status != StatusNone || d.Tracks.Source != SourceEdition {
			t.Errorf("got %+v, %v", d, err)
		}
	})
	t.Run("queue unavailable: no status, library tracks still show", func(t *testing.T) {
		lib := &fakeLib{states: map[string]*lidarr.AlbumState{id1: stp(state(7, true, 0, 0, ""))}, queueErr: errors.New("boom"),
			tracks: []lidarr.LibraryTrack{{Number: "1", Title: "Airbag"}}}
		d, err := New(&fakeSearch{}, &fakeMeta{album: okcMeta()}, lib).Detail(context.Background(), id1)
		if err != nil || d.LibraryOK || d.Status != StatusNone || d.Library != nil || d.Tracks.Source != SourceLibrary {
			t.Errorf("got %+v, %v", d, err)
		}
	})
	t.Run("Lidarr cannot list tracks: fall back to an edition", func(t *testing.T) {
		lib := &fakeLib{states: map[string]*lidarr.AlbumState{id1: stp(state(7, false, 2, 100, ""))}, queued: map[int]bool{}, tracksErr: errors.New("boom")}
		d, err := New(&fakeSearch{}, &fakeMeta{album: okcMeta()}, lib).Detail(context.Background(), id1)
		if err != nil || d.Tracks.Source != SourceEdition || d.Status != StatusAvailable {
			t.Errorf("got %+v, %v", d, err)
		}
	})
}

func TestDetailWithoutVotesHasNoRating(t *testing.T) {
	m := okcMeta()
	m.RatingValue, m.RatingVotes = 0, 0
	m.Genres = nil
	d, _ := New(&fakeSearch{}, &fakeMeta{album: m}, &fakeLib{states: map[string]*lidarr.AlbumState{}}).Detail(context.Background(), id1)
	if d.Rating != nil || d.Genres == nil {
		t.Errorf("an unrated album must have no rating and genres must be [] not null: %+v", d)
	}
}

func TestDetailRejectsInvalidIDsWithoutCallingAnyone(t *testing.T) {
	meta, lib := &fakeMeta{}, &fakeLib{}
	c := New(&fakeSearch{}, meta, lib)
	for _, id := range []string{"", "nope", "../../x", id1 + "/../x", id1 + "?x=1", "B1392450-E666-3926-A536-22C65F834433"} {
		if _, err := c.Detail(context.Background(), id); !errors.Is(err, ErrInvalidID) {
			t.Errorf("Detail(%q): err = %v, want ErrInvalidID", id, err)
		}
	}
	if meta.calls != 0 || lib.albumCalls.Load() != 0 {
		t.Errorf("invalid IDs reached the sources: meta=%d lib=%d", meta.calls, lib.albumCalls.Load())
	}
}

func TestEditionTracksAreNumberedByPositionNotByVinylNumbering(t *testing.T) {
	m := okcMeta()
	m.Editions = []lidarrmeta.Edition{{ID: "tape", Date: "1997", Tracks: []lidarrmeta.Track{
		{Disc: 1, Number: "A1", Position: 1, Title: "Airbag"},
		{Disc: 1, Number: "B1", Position: 2, Title: "Paranoid Android"},
	}}}
	d, _ := New(&fakeSearch{}, &fakeMeta{album: m}, &fakeLib{states: map[string]*lidarr.AlbumState{}}).Detail(context.Background(), id1)
	if got := d.Tracks.Tracks; len(got) != 2 || got[0].Number != "1" || got[1].Number != "2" {
		t.Errorf("tracks should be numbered 1, 2, not A1, B1: %+v", got)
	}
}

func TestSearchPaging(t *testing.T) {
	page := func(n int) []musicbrainz.Hit {
		out := make([]musicbrainz.Hit, n)
		for i := range out {
			out[i] = hit(id3, "x")
		}
		return out
	}
	cases := []struct {
		name     string
		offset   int
		got      int
		total    int
		wantMore bool
	}{
		{"first page of many", 0, PageSize, 60, true},
		{"middle page", PageSize, PageSize, 60, true},
		{"last page exactly full", 2 * PageSize, PageSize, 3 * PageSize, false},
		{"last page partly full", 48, 12, 60, false},
		{"a single page", 0, 5, 5, false},
		{"no results", 0, 0, 0, false},
		{"an empty page never offers more", 24, 0, 60, false},
		{"the page cap stops the button", MaxOffset, PageSize, 5000, false},
		{"one page before the cap still offers more", MaxOffset - PageSize, PageSize, 5000, true},
	}
	for _, c := range cases {
		fs := &fakeSearch{hits: page(c.got), total: c.total}
		fs.hits = page(c.got)
		res, err := New(fs, &fakeMeta{}, &fakeLib{states: map[string]*lidarr.AlbumState{}}).Search(context.Background(), "x", c.offset)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if res.HasMore != c.wantMore {
			t.Errorf("%s: HasMore = %v, want %v", c.name, res.HasMore, c.wantMore)
		}
		if fs.offset != c.offset {
			t.Errorf("%s: asked MusicBrainz for offset %d, want %d", c.name, fs.offset, c.offset)
		}
	}
}

func TestSearchRejectsOffsetsOutsideTheCap(t *testing.T) {
	fs := &fakeSearch{}
	c := New(fs, &fakeMeta{}, &fakeLib{})
	for _, off := range []int{-1, -24, MaxOffset + 1, MaxOffset + PageSize, 1 << 30} {
		if _, err := c.Search(context.Background(), "x", off); !errors.Is(err, ErrInvalidOffset) {
			t.Errorf("offset %d: err = %v, want ErrInvalidOffset", off, err)
		}
	}
	if fs.calls != 0 {
		t.Errorf("rejected offsets reached MusicBrainz %d times", fs.calls)
	}
	if _, err := c.Search(context.Background(), "x", MaxOffset); err != nil {
		t.Errorf("the last allowed offset was rejected: %v", err)
	}
}

// --- requests ---

const artistMBID = "a74b1b7f-71a5-4011-9441-d0b5e4122711"

func requestCatalog(meta *fakeMeta, lib *fakeLib) *Catalog {
	c := New(&fakeSearch{}, meta, lib)
	c.EnableRequests(RequestSettings{RootFolder: "/music", QualityProfileID: 5, MetadataProfile: "None"})
	return c
}

func metaWithArtist() *fakeMeta {
	m := okcMeta()
	m.ArtistID = artistMBID
	return &fakeMeta{album: m}
}

func TestRequestAddsTheAlbumWithTheConfiguredSettings(t *testing.T) {
	lib := &fakeLib{states: map[string]*lidarr.AlbumState{}}
	res, err := requestCatalog(metaWithArtist(), lib).Request(context.Background(), id1, artistMBID)
	if err != nil || res != RequestAdded {
		t.Fatalf("got %q, %v", res, err)
	}
	want := lidarr.NewAlbum{AlbumID: id1, ArtistID: artistMBID, RootFolder: "/music", QualityProfileID: 5, MetadataProfileID: addedProfileID}
	if len(lib.added) != 1 || lib.added[0] != want {
		t.Errorf("added = %+v, want exactly %+v", lib.added, want)
	}
	if len(lib.searched) != 1 || lib.searched[0] != addedID {
		t.Errorf("searched = %v, want a search for the new album's Lidarr ID %d", lib.searched, addedID)
	}
}

func TestRequestMonitorsAnAlbumLidarrLeftUnmonitored(t *testing.T) {
	// A new artist's album comes back unmonitored, whatever the add asked for.
	lib := &fakeLib{states: map[string]*lidarr.AlbumState{}}
	res, err := requestCatalog(metaWithArtist(), lib).Request(context.Background(), id1, artistMBID)
	if err != nil || res != RequestAdded || len(lib.monitored) != 1 || lib.monitored[0] != addedID || len(lib.searched) != 1 {
		t.Errorf("res=%q err=%v monitored=%v searched=%v", res, err, lib.monitored, lib.searched)
	}
	// An existing artist's album comes back monitored: nothing to fix.
	lib = &fakeLib{states: map[string]*lidarr.AlbumState{}, newMonitored: true}
	requestCatalog(metaWithArtist(), lib).Request(context.Background(), id1, artistMBID)
	if len(lib.monitored) != 0 || len(lib.searched) != 1 {
		t.Errorf("already monitored: monitored=%v searched=%v", lib.monitored, lib.searched)
	}
	// If it cannot be monitored it must not be searched either.
	lib = &fakeLib{states: map[string]*lidarr.AlbumState{}, monitorErr: errors.New("nope")}
	res, err = requestCatalog(metaWithArtist(), lib).Request(context.Background(), id1, artistMBID)
	if err != nil || res != RequestAddedUnsearched || len(lib.searched) != 0 {
		t.Errorf("monitor failed: res=%q err=%v searched=%v", res, err, lib.searched)
	}
}

func TestRequestLooksTheMetadataProfileUpByNameAndUsesItsID(t *testing.T) {
	lib := &fakeLib{states: map[string]*lidarr.AlbumState{}, profileID: 31}
	if _, err := requestCatalog(metaWithArtist(), lib).Request(context.Background(), id1, artistMBID); err != nil {
		t.Fatal(err)
	}
	if len(lib.profileAsked) != 1 || lib.profileAsked[0] != "None" {
		t.Errorf("asked Lidarr for %v, want the profile named None", lib.profileAsked)
	}
	if len(lib.added) != 1 || lib.added[0].MetadataProfileID != 31 {
		t.Errorf("added = %+v, want the looked-up ID 31", lib.added)
	}
}

func TestRequestWritesNothingWhenLidarrHasNoNoneProfile(t *testing.T) {
	lib := &fakeLib{states: map[string]*lidarr.AlbumState{}, profileErr: fmt.Errorf("%w: no profile named None", lidarr.ErrBadSettings)}
	res, err := requestCatalog(metaWithArtist(), lib).Request(context.Background(), id1, artistMBID)
	if !errors.Is(err, lidarr.ErrBadSettings) || res != "" {
		t.Errorf("got %q, %v", res, err)
	}
	if len(lib.added) != 0 || len(lib.monitored) != 0 || len(lib.searched) != 0 {
		t.Errorf("without the profile an add would import the artist's whole catalogue: added=%d monitored=%d searched=%d", len(lib.added), len(lib.monitored), len(lib.searched))
	}
}

func TestRequestTreatsAFailedProfileLookupAsAnOutage(t *testing.T) {
	lib := &fakeLib{states: map[string]*lidarr.AlbumState{}, profileErr: errors.New("connection refused")}
	_, err := requestCatalog(metaWithArtist(), lib).Request(context.Background(), id1, artistMBID)
	if !errors.Is(err, ErrUnavailable) || errors.Is(err, lidarr.ErrBadSettings) || len(lib.added) != 0 {
		t.Errorf("err = %v added = %d", err, len(lib.added))
	}
}

func TestRequestForAnAlbumAlreadyInLidarrNeedsNoProfile(t *testing.T) {
	// Monitoring and searching an album Lidarr has needs no artist, so no profile.
	lib := &fakeLib{states: map[string]*lidarr.AlbumState{id1: stp(state(7, false, 0, 0, ""))}, profileErr: errors.New("must not be asked")}
	if res, err := requestCatalog(metaWithArtist(), lib).Request(context.Background(), id1, artistMBID); err != nil || res != RequestAdded {
		t.Errorf("got %q, %v", res, err)
	}
	if len(lib.profileAsked) != 0 {
		t.Errorf("asked for the profile %v", lib.profileAsked)
	}
}

func TestRequestStillSucceedsWhenOnlyTheSearchFails(t *testing.T) {
	boom := errors.New("command refused")
	for name, lib := range map[string]*fakeLib{
		"the search call fails":               {states: map[string]*lidarr.AlbumState{}, searchErr: boom},
		"the new album cannot be found again": {states: nil},
	} {
		res, err := requestCatalog(metaWithArtist(), lib).Request(context.Background(), id1, artistMBID)
		if err != nil || res != RequestAddedUnsearched {
			t.Errorf("%s: got %q, %v: the album is in Lidarr, so this is not an error, and it must not be retried", name, res, err)
		}
		if len(lib.added) != 1 {
			t.Errorf("%s: added %d times", name, len(lib.added))
		}
	}
}

func TestRequestNeverSearchesWhatLidarrAlreadyWants(t *testing.T) {
	lib := &fakeLib{states: map[string]*lidarr.AlbumState{id1: stp(state(7, true, 0, 0, ""))}}
	requestCatalog(metaWithArtist(), lib).Request(context.Background(), id1, artistMBID)
	complete := &fakeLib{states: map[string]*lidarr.AlbumState{id1: stp(state(8, false, 10, 100, ""))}}
	requestCatalog(metaWithArtist(), complete).Request(context.Background(), id1, artistMBID)
	if len(complete.searched)+len(complete.monitored) != 0 {
		t.Error("a complete album must not be searched for")
	}
	lib2 := &fakeLib{states: map[string]*lidarr.AlbumState{}, addErr: lidarr.ErrAlreadyAdded}
	requestCatalog(metaWithArtist(), lib2).Request(context.Background(), id1, artistMBID)
	if len(lib.searched)+len(lib2.searched) != 0 {
		t.Error("a monitored album must not be searched again: that would re-grab on every click")
	}
}

func TestRequestDoesNotSearchWhenTheAddIsRefused(t *testing.T) {
	lib := &fakeLib{states: map[string]*lidarr.AlbumState{}, addErr: &lidarr.RejectedError{Messages: []string{"bad"}}}
	requestCatalog(metaWithArtist(), lib).Request(context.Background(), id1, artistMBID)
	if len(lib.searched) != 0 {
		t.Error("a refused add must not search")
	}
}

func TestRequestForAnAlbumLidarrAlreadyHasChangesNothing(t *testing.T) {
	meta := metaWithArtist()
	lib := &fakeLib{states: map[string]*lidarr.AlbumState{id1: stp(state(7, true, 0, 0, ""))}}
	res, err := requestCatalog(meta, lib).Request(context.Background(), id1, artistMBID)
	if err != nil || res != RequestExists {
		t.Fatalf("got %q, %v", res, err)
	}
	if len(lib.added) != 0 || meta.calls != 0 {
		t.Errorf("nothing should be written or fetched: added=%d metadata calls=%d", len(lib.added), meta.calls)
	}
}

func TestRequestWantsAnAlbumLidarrHasButDoesNotWant(t *testing.T) {
	// Imported with its artist, or added unmonitored earlier. No metadata call is needed.
	meta := metaWithArtist()
	lib := &fakeLib{states: map[string]*lidarr.AlbumState{id1: stp(state(7, false, 0, 0, ""))}}
	res, err := requestCatalog(meta, lib).Request(context.Background(), id1, artistMBID)
	if err != nil || res != RequestAdded {
		t.Fatalf("got %q, %v", res, err)
	}
	if len(lib.added) != 0 || meta.calls != 0 || len(lib.monitored) != 1 || lib.monitored[0] != 7 || len(lib.searched) != 1 || lib.searched[0] != 7 {
		t.Errorf("want monitor + search of album 7 only: added=%d metadata=%d monitored=%v searched=%v", len(lib.added), meta.calls, lib.monitored, lib.searched)
	}
}

func TestRequestRaceLidarrAddedItFirst(t *testing.T) {
	lib := &fakeLib{states: map[string]*lidarr.AlbumState{}, addErr: lidarr.ErrAlreadyAdded}
	res, err := requestCatalog(metaWithArtist(), lib).Request(context.Background(), id1, artistMBID)
	if err != nil || res != RequestExists {
		t.Errorf("an already-added answer from Lidarr is success: %q, %v", res, err)
	}
}

func TestRequestNeverAsksTheMetadataService(t *testing.T) {
	// The artist's ID comes with the request, and an album Lidarr's metadata does not
	// know is refused by Lidarr itself, so a metadata service that is down (or that
	// would say "not found") makes no difference to a request.
	for name, meta := range map[string]*fakeMeta{"down": {err: errors.New("boom")}, "not found": {err: lidarrmeta.ErrNotFound}, "fine": metaWithArtist()} {
		lib := &fakeLib{states: map[string]*lidarr.AlbumState{}}
		res, err := requestCatalog(meta, lib).Request(context.Background(), id1, artistMBID)
		if err != nil || res != RequestAdded || meta.calls != 0 {
			t.Errorf("%s: res=%q err=%v metadata calls=%d", name, res, err, meta.calls)
		}
		if len(lib.added) != 1 || lib.added[0].ArtistID != artistMBID {
			t.Errorf("%s: the artist ID sent must be the one the request carried: %+v", name, lib.added)
		}
	}
}

func TestWanted(t *testing.T) {
	for name, tc := range map[string]struct {
		states map[string]*lidarr.AlbumState
		err    error
		want   bool
		wantEr error
	}{
		"not in Lidarr":                    {map[string]*lidarr.AlbumState{}, nil, true, nil},
		"in Lidarr, not wanted":            {map[string]*lidarr.AlbumState{id1: stp(state(7, false, 0, 0, ""))}, nil, true, nil},
		"in Lidarr and monitored":          {map[string]*lidarr.AlbumState{id1: stp(state(7, true, 0, 0, ""))}, nil, false, nil},
		"in Lidarr, complete but unwanted": {map[string]*lidarr.AlbumState{id1: stp(state(7, false, 10, 100, ""))}, nil, false, nil},
		"Lidarr cannot be asked":           {map[string]*lidarr.AlbumState{}, errors.New("boom"), false, ErrUnavailable},
	} {
		got, err := New(&fakeSearch{}, &fakeMeta{}, &fakeLib{states: tc.states, albumErr: tc.err}).Wanted(context.Background(), id1)
		if got != tc.want || !errors.Is(err, tc.wantEr) && tc.wantEr != nil || (tc.wantEr == nil && err != nil) {
			t.Errorf("%s: wanted=%v err=%v, want %v / %v", name, got, err, tc.want, tc.wantEr)
		}
	}
	if _, err := New(&fakeSearch{}, &fakeMeta{}, &fakeLib{}).Wanted(context.Background(), "nope"); !errors.Is(err, ErrInvalidID) {
		t.Errorf("an invalid id: %v", err)
	}
}

func TestRequestRefusals(t *testing.T) {
	boom := errors.New("boom")
	cases := []struct {
		name    string
		enabled bool
		id      string
		artist  string
		lib     *fakeLib
		want    error
	}{
		{"requesting is off", false, id1, artistMBID, &fakeLib{}, ErrRequestsDisabled},
		{"an invalid album id", true, "../../x", artistMBID, &fakeLib{}, ErrInvalidID},
		{"an invalid artist id", true, id1, "../../x", &fakeLib{}, ErrInvalidID},
		{"an empty artist id", true, id1, "", &fakeLib{}, ErrInvalidID},
		{"Lidarr cannot be asked what it has", true, id1, artistMBID, &fakeLib{albumErr: boom}, ErrUnavailable},
		{"Lidarr's metadata does not know the album", true, id1, artistMBID, &fakeLib{addErr: lidarr.ErrAlbumNotFound}, ErrNotInMetadata},
		{"Lidarr unavailable on the write", true, id1, artistMBID, &fakeLib{addErr: lidarr.ErrUnavailable}, ErrUnavailable},
	}
	for _, c := range cases {
		if c.lib.states == nil {
			c.lib.states = map[string]*lidarr.AlbumState{}
		}
		cat := New(&fakeSearch{}, &fakeMeta{}, c.lib)
		if c.enabled {
			cat.EnableRequests(RequestSettings{RootFolder: "/music", QualityProfileID: 5, MetadataProfile: "None"})
		}
		_, err := cat.Request(context.Background(), c.id, c.artist)
		if !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", c.name, err, c.want)
		}
		if len(c.lib.added) != 0 {
			t.Errorf("%s: Lidarr was written to", c.name)
		}
	}

	// A refusal that never reaches the metadata or Lidarr must not call them at all.
	meta, lib := metaWithArtist(), &fakeLib{states: map[string]*lidarr.AlbumState{}}
	cat := New(&fakeSearch{}, meta, lib) // requests never enabled
	cat.Request(context.Background(), id1, artistMBID)
	cat.Request(context.Background(), "nope", artistMBID)
	if meta.calls != 0 || lib.albumCalls.Load() != 0 {
		t.Errorf("a disabled or invalid request still called out: meta=%d lib=%d", meta.calls, lib.albumCalls.Load())
	}
}

func TestRequestPassesLidarrsRejectionThrough(t *testing.T) {
	rej := &lidarr.RejectedError{Messages: []string{"Root folder '/nope' does not exist"}}
	lib := &fakeLib{states: map[string]*lidarr.AlbumState{}, addErr: rej}
	_, err := requestCatalog(metaWithArtist(), lib).Request(context.Background(), id1, artistMBID)
	var got *lidarr.RejectedError
	if !errors.As(err, &got) || errors.Is(err, ErrUnavailable) {
		t.Errorf("err = %v, want Lidarr's rejection and not ErrUnavailable", err)
	}
}

func TestDetailCanRequest(t *testing.T) {
	notOwned := func() *fakeLib { return &fakeLib{states: map[string]*lidarr.AlbumState{}} }
	owned := func(monitored bool, files int, percent float64) *fakeLib {
		return &fakeLib{states: map[string]*lidarr.AlbumState{id1: stp(state(7, monitored, files, percent, ""))}, queued: map[int]bool{}}
	}
	cases := []struct {
		name    string
		enabled bool
		meta    *fakeMeta
		lib     *fakeLib
		want    bool
	}{
		{"enabled and not in Lidarr", true, &fakeMeta{album: okcMeta()}, notOwned(), true},
		{"in Lidarr but not wanted", true, &fakeMeta{album: okcMeta()}, owned(false, 0, 0), true},
		{"in Lidarr and monitored", true, &fakeMeta{album: okcMeta()}, owned(true, 0, 0), false},
		{"in Lidarr, unmonitored but complete", true, &fakeMeta{album: okcMeta()}, owned(false, 10, 100), false},
		{"not wanted, and the metadata service is down", true, &fakeMeta{err: errors.New("boom")}, owned(false, 0, 0), true},
		{"requesting is off", false, &fakeMeta{album: okcMeta()}, notOwned(), false},
		{"Lidarr unreachable", true, &fakeMeta{album: okcMeta()}, &fakeLib{albumErr: errors.New("boom")}, false},
		{"metadata unavailable", true, &fakeMeta{err: errors.New("boom")}, notOwned(), false},
	}
	for _, c := range cases {
		cat := New(&fakeSearch{}, c.meta, c.lib)
		if c.enabled {
			cat.EnableRequests(RequestSettings{RootFolder: "/music", QualityProfileID: 1, MetadataProfile: "None"})
		}
		d, err := cat.Detail(context.Background(), id1)
		if err != nil || d.CanRequest != c.want {
			t.Errorf("%s: CanRequest = %v (err %v), want %v", c.name, d.CanRequest, err, c.want)
		}
	}
}

func TestRequestSettingsCanBeSwappedWhileServing(t *testing.T) {
	lib := &fakeLib{states: map[string]*lidarr.AlbumState{}}
	cat := New(&fakeSearch{}, metaWithArtist(), lib)

	if _, err := cat.Request(context.Background(), id1, artistMBID); !errors.Is(err, ErrRequestsDisabled) {
		t.Fatalf("before any settings: err = %v, want ErrRequestsDisabled", err)
	}
	cat.SetRequests(&RequestSettings{RootFolder: "/a", QualityProfileID: 1, MetadataProfile: "None"})
	if _, err := cat.Request(context.Background(), id1, artistMBID); err != nil {
		t.Fatal(err)
	}
	delete(lib.states, id1) // as if the first album had been removed from Lidarr again
	cat.SetRequests(&RequestSettings{RootFolder: "/b", QualityProfileID: 3, MetadataProfile: "None"})
	cat.Request(context.Background(), id1, artistMBID)
	if len(lib.added) != 2 || lib.added[0].RootFolder != "/a" || lib.added[1].RootFolder != "/b" || lib.added[1].QualityProfileID != 3 {
		t.Errorf("each request must use the settings in force when it ran: %+v", lib.added)
	}
	cat.SetRequests(nil)
	if _, err := cat.Request(context.Background(), id1, artistMBID); !errors.Is(err, ErrRequestsDisabled) {
		t.Errorf("after switching off: err = %v", err)
	}
}

func TestSetRequestsCopiesItsArgument(t *testing.T) {
	cat := New(&fakeSearch{}, metaWithArtist(), &fakeLib{states: map[string]*lidarr.AlbumState{}})
	s := RequestSettings{RootFolder: "/a", QualityProfileID: 1, MetadataProfile: "None"}
	cat.SetRequests(&s)
	s.RootFolder = "/mutated"
	lib := cat.lib.(*fakeLib)
	cat.Request(context.Background(), id1, artistMBID)
	if len(lib.added) != 1 || lib.added[0].RootFolder != "/a" {
		t.Errorf("the catalog must keep its own copy of the settings: %+v", lib.added)
	}
}

func TestSwappingRequestSettingsIsRaceFree(t *testing.T) {
	cat := New(&fakeSearch{}, metaWithArtist(), &fakeLib{states: map[string]*lidarr.AlbumState{}})
	done := make(chan struct{})
	go func() {
		for i := 0; i < 300; i++ {
			if i%2 == 0 {
				cat.SetRequests(&RequestSettings{RootFolder: "/a", QualityProfileID: 1, MetadataProfile: "None"})
			} else {
				cat.SetRequests(nil)
			}
		}
		close(done)
	}()
	for {
		select {
		case <-done:
			return
		default:
			cat.Detail(context.Background(), id1)
		}
	}
}

func TestProgressSaysWhereEachAlbumStands(t *testing.T) {
	const idA, idB, idC, idD = id1, "11111111-1111-4111-8111-111111111112", "11111111-1111-4111-8111-111111111113", "11111111-1111-4111-8111-111111111114"
	lib := &fakeLib{
		states: map[string]*lidarr.AlbumState{
			idA: stp(state(1, false, 12, 100, searchedAt)), // complete
			idB: stp(state(2, true, 0, 0, never)),          // downloading
			idC: stp(state(3, true, 0, 0, never)),          // searched for, nothing yet
			// idD: unknown to Lidarr
		},
		queued:    map[int]bool{2: true},
		downloads: map[int]lidarr.Download{2: {Stage: lidarr.StageDownloading}},
	}
	got := New(&fakeSearch{}, &fakeMeta{}, lib).Progress(context.Background(), []string{idA, idB, idC, idD, "not-an-id"})
	if got[idA].Status != StatusAvailable || got[idA].Stage != "" {
		t.Errorf("complete: %+v", got[idA])
	}
	if p := got[idB]; p.Status != StatusProcessing || p.Stage != lidarr.StageDownloading {
		t.Errorf("downloading: %+v", p)
	}
	if p := got[idC]; p.Status != StatusRequested || p.Stage != "" {
		t.Errorf("waiting: %+v", p)
	}
	if _, ok := got[idD]; ok {
		t.Error("an album Lidarr does not have must be left out, not guessed")
	}
	if _, ok := got["not-an-id"]; ok {
		t.Error("an invalid id must be left out")
	}
}

func TestProgressLeavesOutWhatLidarrCannotAnswer(t *testing.T) {
	down := &fakeLib{albumErr: errors.New("down")}
	if got := New(&fakeSearch{}, &fakeMeta{}, down).Progress(context.Background(), []string{id1}); len(got) != 0 {
		t.Errorf("Lidarr down: %v", got)
	}
	noQueue := &fakeLib{states: map[string]*lidarr.AlbumState{id1: stp(state(1, true, 0, 0, never))}, queueErr: errors.New("down")}
	if got := New(&fakeSearch{}, &fakeMeta{}, noQueue).Progress(context.Background(), []string{id1}); len(got) != 0 {
		t.Errorf("queue unreadable must not be reported as 'requested': %v", got)
	}
	done := &fakeLib{states: map[string]*lidarr.AlbumState{id1: stp(state(1, false, 12, 100, searchedAt))}, queueErr: errors.New("down")}
	if got := New(&fakeSearch{}, &fakeMeta{}, done).Progress(context.Background(), []string{id1}); got[id1].Status != StatusAvailable || done.queueCalls.Load() != 0 {
		t.Errorf("a complete album needs no queue: %v, %d calls", got, done.queueCalls.Load())
	}
}

// --- artists ---

func TestSearchArtistsKeepsTheStrongMatchesOnly(t *testing.T) {
	fs := &fakeSearch{artists: []musicbrainz.ArtistHit{
		{ID: "a1", Name: "Ed Sheeran", Type: "Person", Disambiguation: "UK singer", Country: "GB", Score: 100},
		{ID: "a2", Name: "Ed Sheeran", Score: 50},
		{ID: "a3", Name: "Eddie", Score: 49},
	}}
	got, err := New(fs, &fakeMeta{}, &fakeLib{}).SearchArtists(context.Background(), "ed sheeran")
	if err != nil || len(got) != 2 || got[0].ID != "a1" || got[0].Name != "Ed Sheeran" || got[0].Disambiguation != "UK singer" || got[1].ID != "a2" {
		t.Errorf("got %+v, %v", got, err)
	}
	if _, err := New(&fakeSearch{err: errors.New("down")}, &fakeMeta{}, &fakeLib{}).SearchArtists(context.Background(), "x"); err == nil {
		t.Error("a failing search must be an error")
	}
}

const edSheeran = "b8a7c51f-362c-4dcb-a259-bc6e0095f0a6"

func artistMeta() lidarrmeta.Artist {
	return lidarrmeta.Artist{ID: edSheeran, Name: "Ed Sheeran", Type: "Person", Albums: []lidarrmeta.ArtistAlbum{
		{ID: id1, Title: "÷", Type: "Album", SecondaryTypes: []string{}, ReleaseDate: "2017-03-03"},
		{ID: id2, Title: "×", Type: "Album", SecondaryTypes: []string{}, ReleaseDate: "2014-06-20"},
		{ID: id3, Title: "Live at Wembley", Type: "Album", SecondaryTypes: []string{"Live"}, ReleaseDate: "2019-01-01"},
		{ID: "11111111-1111-4111-8111-111111111111", Title: "Sing", Type: "Single", SecondaryTypes: []string{}, ReleaseDate: "2014-04-07"},
		{ID: "11111111-1111-4111-8111-111111111112", Title: "Undated EP", Type: "EP", SecondaryTypes: []string{}},
		{ID: "11111111-1111-4111-8111-111111111113", Title: "A Broadcast", Type: "Broadcast", SecondaryTypes: []string{}, ReleaseDate: "2012-01-01"},
	}}
}

func TestArtistListsTheirAlbumsNewestFirstInGroupsWithLidarrsState(t *testing.T) {
	lib := &fakeLib{
		states: map[string]*lidarr.AlbumState{
			id1: stp(state(1, false, 12, 100, searchedAt)), // complete
			id2: stp(state(2, true, 0, 0, never)),          // downloading
		},
		queued: map[int]bool{2: true},
	}
	page, err := New(&fakeSearch{}, &fakeMeta{artist: artistMeta()}, lib).Artist(context.Background(), edSheeran)
	if err != nil {
		t.Fatal(err)
	}
	if page.Name != "Ed Sheeran" || !page.LibraryOK || len(page.Albums) != 6 {
		t.Fatalf("page: %+v", page)
	}
	var order []string
	for _, a := range page.Albums {
		order = append(order, a.Title)
	}
	if want := []string{"Live at Wembley", "÷", "×", "Sing", "A Broadcast", "Undated EP"}; !reflect.DeepEqual(order, want) {
		t.Errorf("order %v, want newest first with the undated last: %v", order, want)
	}
	byTitle := map[string]Album{}
	for _, a := range page.Albums {
		byTitle[a.Title] = a
	}
	for title, want := range map[string]string{"÷": "albums", "×": "albums", "Live at Wembley": "more", "Sing": "singles", "Undated EP": "eps", "A Broadcast": "more"} {
		if byTitle[title].Group != want {
			t.Errorf("%s is in %q, want %q", title, byTitle[title].Group, want)
		}
	}
	if byTitle["÷"].Status != StatusAvailable || byTitle["×"].Status != StatusProcessing || byTitle["Sing"].Status != "" {
		t.Errorf("statuses: ÷ %q × %q Sing %q", byTitle["÷"].Status, byTitle["×"].Status, byTitle["Sing"].Status)
	}
	if a := byTitle["÷"]; a.Artist != "Ed Sheeran" || a.ArtistID != edSheeran || a.Cover != "/api/cover/"+id1 || a.Year != 2017 {
		t.Errorf("a card carries what the request needs: %+v", a)
	}
}

func TestArtistWithLidarrDownStillListsTheAlbumsWithoutChips(t *testing.T) {
	lib := &fakeLib{artistStatesErr: errors.New("down")}
	page, err := New(&fakeSearch{}, &fakeMeta{artist: artistMeta()}, lib).Artist(context.Background(), edSheeran)
	if err != nil || page.LibraryOK || len(page.Albums) != 6 {
		t.Fatalf("%+v, %v", page, err)
	}
	for _, a := range page.Albums {
		if a.Status != "" {
			t.Errorf("no chip may be shown when Lidarr cannot be asked: %s %q", a.Title, a.Status)
		}
	}
}

func TestArtistDoesNotAskForTheQueueWhenEverythingIsComplete(t *testing.T) {
	lib := &fakeLib{states: map[string]*lidarr.AlbumState{id1: stp(state(1, false, 12, 100, searchedAt))}, queueErr: errors.New("would fail")}
	page, err := New(&fakeSearch{}, &fakeMeta{artist: artistMeta()}, lib).Artist(context.Background(), edSheeran)
	if err != nil || !page.LibraryOK || lib.queueCalls.Load() != 0 {
		t.Errorf("%v, libraryOk %v, %d queue calls", err, page.LibraryOK, lib.queueCalls.Load())
	}
}

func TestArtistErrors(t *testing.T) {
	if _, err := New(&fakeSearch{}, &fakeMeta{}, &fakeLib{}).Artist(context.Background(), "../x"); !errors.Is(err, ErrInvalidID) {
		t.Errorf("invalid id: %v", err)
	}
	if _, err := New(&fakeSearch{}, &fakeMeta{err: lidarrmeta.ErrNotFound}, &fakeLib{}).Artist(context.Background(), edSheeran); !errors.Is(err, ErrNotInMetadata) {
		t.Errorf("unknown artist: %v", err)
	}
	if _, err := New(&fakeSearch{}, &fakeMeta{err: errors.New("503")}, &fakeLib{}).Artist(context.Background(), edSheeran); !errors.Is(err, ErrUnavailable) {
		t.Errorf("metadata down: %v", err)
	}
}

type fakePhotos struct {
	data     []byte
	err      error
	gotID    string
	gotURL   string
	gotWidth int
	calls    int
}

func (f *fakePhotos) Photo(_ context.Context, id, rawURL string, width int) ([]byte, string, error) {
	f.calls++
	f.gotID, f.gotURL, f.gotWidth = id, rawURL, width
	return f.data, "image/jpeg", f.err
}

func metaWithImages(images ...lidarrmeta.ArtistImage) lidarrmeta.Artist {
	a := artistMeta()
	a.Images = images
	return a
}

func TestAnArtistPageOffersAPhotoOnlyWhenThereIsOneToShow(t *testing.T) {
	poster := lidarrmeta.ArtistImage{Type: "Poster", URL: "https://cdn.example/p.jpg"}
	cases := map[string]struct {
		meta   lidarrmeta.Artist
		photos Photos
		want   string
	}{
		"a poster and a fetcher": {metaWithImages(poster), &fakePhotos{}, "/api/artist/" + edSheeran + "/photo"},
		"no images":              {metaWithImages(), &fakePhotos{}, ""},
		"only a banner":          {metaWithImages(lidarrmeta.ArtistImage{Type: "Banner", URL: "x"}), &fakePhotos{}, ""},
		"no fetcher":             {metaWithImages(poster), nil, ""},
	}
	for name, c := range cases {
		cat := New(&fakeSearch{}, &fakeMeta{artist: c.meta}, &fakeLib{})
		if c.photos != nil {
			cat.SetPhotos(c.photos)
		}
		page, err := cat.Artist(context.Background(), edSheeran)
		if err != nil || page.Photo != c.want {
			t.Errorf("%s: photo %q, %v", name, page.Photo, err)
		}
	}
}

func TestArtistPhotoUsesTheAddressTheMetadataServiceGave(t *testing.T) {
	fp := &fakePhotos{data: []byte("jpeg")}
	cat := New(&fakeSearch{}, &fakeMeta{artist: metaWithImages(lidarrmeta.ArtistImage{Type: "Poster", URL: "https://cdn.example/p.jpg"})}, &fakeLib{})
	cat.SetPhotos(fp)
	data, ct, err := cat.ArtistPhoto(context.Background(), edSheeran, PhotoFull)
	if err != nil || string(data) != "jpeg" || ct != "image/jpeg" || fp.gotID != edSheeran || fp.gotURL != "https://cdn.example/p.jpg" || fp.gotWidth != PhotoFull {
		t.Errorf("%q %q %v, asked for %q %q at %d", data, ct, err, fp.gotID, fp.gotURL, fp.gotWidth)
	}
	if _, _, err := cat.ArtistPhoto(context.Background(), edSheeran, PhotoThumb); err != nil || fp.gotWidth != PhotoThumb {
		t.Errorf("the thumbnail size is passed on: %v, %d", err, fp.gotWidth)
	}
	fp.calls = 0
	if _, _, err := cat.ArtistPhoto(context.Background(), edSheeran, 123); !errors.Is(err, coverart.ErrInvalid) || fp.calls != 0 {
		t.Errorf("any other size is refused before anything is fetched: %v, %d calls", err, fp.calls)
	}
}

func TestArtistPhotoErrors(t *testing.T) {
	poster := lidarrmeta.ArtistImage{Type: "Poster", URL: "https://cdn.example/p.jpg"}
	fp := &fakePhotos{}
	catWith := func(m *fakeMeta, photos Photos) *Catalog {
		c := New(&fakeSearch{}, m, &fakeLib{})
		if photos != nil {
			c.SetPhotos(photos)
		}
		return c
	}
	if _, _, err := catWith(&fakeMeta{}, fp).ArtistPhoto(context.Background(), "../x", PhotoFull); !errors.Is(err, ErrInvalidID) || fp.calls != 0 {
		t.Errorf("invalid id: %v", err)
	}
	if _, _, err := catWith(&fakeMeta{artist: metaWithImages(poster)}, nil).ArtistPhoto(context.Background(), edSheeran, PhotoFull); !errors.Is(err, coverart.ErrNoCover) {
		t.Errorf("no fetcher: %v", err)
	}
	if _, _, err := catWith(&fakeMeta{artist: metaWithImages()}, fp).ArtistPhoto(context.Background(), edSheeran, PhotoFull); !errors.Is(err, coverart.ErrNoCover) || fp.calls != 0 {
		t.Errorf("no image: %v, %d calls", err, fp.calls)
	}
	if _, _, err := catWith(&fakeMeta{err: lidarrmeta.ErrNotFound}, fp).ArtistPhoto(context.Background(), edSheeran, PhotoFull); !errors.Is(err, coverart.ErrNoCover) {
		t.Errorf("unknown artist: %v", err)
	}
	if _, _, err := catWith(&fakeMeta{err: errors.New("503")}, fp).ArtistPhoto(context.Background(), edSheeran, PhotoFull); !errors.Is(err, ErrUnavailable) {
		t.Errorf("metadata down: %v", err)
	}
}
