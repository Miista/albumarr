package catalog

import (
	"context"
	"errors"
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

func TestDeriveStatus(t *testing.T) {
	const searchedAt = "2026-10-10T15:16:09Z"
	const never = "0001-01-01T00:00:00Z"

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
		// The refusing cases: nothing was requested, so there must be no chip.
		{"unmonitored, no files is no chip", state(1, false, 0, 0, searchedAt), false, StatusNone},
		{"unmonitored partial is no chip", state(1, false, 4, 33, searchedAt), false, StatusNone},
		{"files but zero percent is not available", state(1, false, 1, 0, searchedAt), false, StatusNone},
		{"100 percent with no files is not available", state(1, false, 0, 100, searchedAt), false, StatusNone},
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
	hits  []musicbrainz.Hit
	err   error
	limit int
}

func (f *fakeSearch) Search(_ context.Context, _ string, limit int) ([]musicbrainz.Hit, error) {
	f.limit = limit
	return f.hits, f.err
}

type fakeMeta struct {
	album lidarrmeta.Album
	err   error
	calls int
}

func (f *fakeMeta) Album(context.Context, string) (lidarrmeta.Album, error) {
	f.calls++
	return f.album, f.err
}

type fakeLib struct {
	states      map[string]*lidarr.AlbumState
	queued      map[int]bool
	tracks      []lidarr.LibraryTrack
	albumErr    error
	queueErr    error
	tracksErr   error
	albumCalls  atomic.Int32 // AlbumByForeignID runs from several goroutines
	queueCalls  atomic.Int32
	tracksCalls atomic.Int32
}

func (f *fakeLib) AlbumByForeignID(_ context.Context, id string) (*lidarr.AlbumState, error) {
	f.albumCalls.Add(1)
	return f.states[id], f.albumErr
}

func (f *fakeLib) QueuedAlbumIDs(context.Context) (map[int]bool, error) {
	f.queueCalls.Add(1)
	return f.queued, f.queueErr
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
	res, err := New(fs, &fakeMeta{}, lib).Search(context.Background(), "x")
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
	if fs.limit != resultLimit {
		t.Errorf("search limit = %d, want %d", fs.limit, resultLimit)
	}
}

func TestSearchSkipsTheQueueWhenNothingIsInLidarr(t *testing.T) {
	lib := &fakeLib{states: map[string]*lidarr.AlbumState{}}
	if _, err := New(&fakeSearch{hits: []musicbrainz.Hit{hit(id1, "New")}}, &fakeMeta{}, lib).Search(context.Background(), "x"); err != nil {
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
		res, err := New(&fakeSearch{hits: hits}, &fakeMeta{}, lib).Search(context.Background(), "x")
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
	if _, err := New(&fakeSearch{err: boom}, &fakeMeta{}, &fakeLib{}).Search(context.Background(), "x"); !errors.Is(err, boom) {
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
