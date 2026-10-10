package catalog

import (
	"context"
	"errors"
	"testing"

	"github.com/Miista/albumreq/internal/lidarr"
)

func state(monitored bool, fileCount int, percent float64, lastSearch string) lidarr.AlbumState {
	var st lidarr.AlbumState
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
		{"complete is available", state(false, 12, 100, searchedAt), false, StatusAvailable},
		{"complete beats queued", state(true, 12, 100, searchedAt), true, StatusAvailable},
		{"complete beats monitored+searched", state(true, 12, 100, searchedAt), false, StatusAvailable},
		{"queued is processing", state(true, 0, 0, never), true, StatusProcessing},
		{"queued while only partly present is processing", state(true, 4, 33, searchedAt), true, StatusProcessing},
		{"monitored, never searched is requested", state(true, 0, 0, never), false, StatusRequested},
		{"monitored, empty lastSearchTime is requested", state(true, 0, 0, ""), false, StatusRequested},
		{"monitored and searched is not found", state(true, 0, 0, searchedAt), false, StatusNotFound},
		{"partial files, monitored, searched is not found", state(true, 4, 33, searchedAt), false, StatusNotFound},
		// The refusing cases: nothing was requested, so there must be no chip.
		{"unmonitored, no files is no chip", state(false, 0, 0, searchedAt), false, StatusNone},
		{"unmonitored partial is no chip", state(false, 4, 33, searchedAt), false, StatusNone},
		{"files but zero percent is not available", state(false, 1, 0, searchedAt), false, StatusNone},
		{"100 percent with no files is not available", state(false, 0, 100, searchedAt), false, StatusNone},
	}
	for _, c := range cases {
		if got := DeriveStatus(c.st, c.queued); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

type fakeSource struct {
	hits   []lidarr.LookupAlbum
	states map[int]lidarr.AlbumState
	queued map[int]bool

	lookupErr, albumsErr, queueErr error
	albumsCalls, queueCalls        int
}

func (f *fakeSource) Lookup(context.Context, string) ([]lidarr.LookupAlbum, error) {
	return f.hits, f.lookupErr
}

func (f *fakeSource) Albums(_ context.Context, ids []int) (map[int]lidarr.AlbumState, error) {
	f.albumsCalls++
	return f.states, f.albumsErr
}

func (f *fakeSource) QueuedAlbumIDs(context.Context) (map[int]bool, error) {
	f.queueCalls++
	return f.queued, f.queueErr
}

func hit(id *int, foreign, title string) lidarr.LookupAlbum {
	h := lidarr.LookupAlbum{ID: id, ForeignAlbumID: foreign, Title: title, AlbumType: "Album", ReleaseDate: "2001-10-23T00:00:00Z"}
	h.Artist.Name = "Radiohead"
	h.Artist.ForeignID = "artist-1"
	return h
}

func intp(i int) *int { return &i }

func TestSearchAnnotatesStatus(t *testing.T) {
	src := &fakeSource{
		hits: []lidarr.LookupAlbum{
			hit(intp(1), "a", "Owned"),
			hit(intp(2), "b", "Downloading"),
			hit(nil, "c", "Unknown to Lidarr"),
		},
		states: map[int]lidarr.AlbumState{
			1: state(false, 10, 100, ""),
			2: state(true, 0, 0, ""),
		},
		queued: map[int]bool{2: true},
	}

	got, err := New(src).Search(context.Background(), "x")
	if err != nil {
		t.Fatal(err)
	}
	want := []Status{StatusAvailable, StatusProcessing, StatusNone}
	if len(got) != len(want) {
		t.Fatalf("got %d albums, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Status != w {
			t.Errorf("album %d (%s): status %q, want %q", i, got[i].Title, got[i].Status, w)
		}
	}
	if got[0].Year != 2001 || got[0].Artist != "Radiohead" || got[0].ID != "a" || got[0].Type != "Album" {
		t.Errorf("fields not mapped: %+v", got[0])
	}
}

func TestSearchSkipsStateLookupsWhenNothingIsInLidarr(t *testing.T) {
	src := &fakeSource{hits: []lidarr.LookupAlbum{hit(nil, "a", "New")}}
	if _, err := New(src).Search(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	if src.albumsCalls != 0 || src.queueCalls != 0 {
		t.Errorf("albums/queue called %d/%d times, want 0/0", src.albumsCalls, src.queueCalls)
	}
}

func TestSearchPropagatesEveryUpstreamError(t *testing.T) {
	boom := errors.New("boom")
	owned := []lidarr.LookupAlbum{hit(intp(1), "a", "Owned")}
	cases := map[string]*fakeSource{
		"lookup": {lookupErr: boom},
		"albums": {hits: owned, albumsErr: boom},
		"queue":  {hits: owned, queueErr: boom},
	}
	for name, src := range cases {
		if _, err := New(src).Search(context.Background(), "x"); !errors.Is(err, boom) {
			t.Errorf("%s: err = %v, want boom", name, err)
		}
	}
}

func TestSearchCoverAndTypes(t *testing.T) {
	h := hit(nil, "a", "Covered")
	h.Images = []lidarr.Image{{CoverType: "cover", URL: "/MediaCoverProxy/ab/c.jpg"}}
	bad := hit(nil, "b", "Hostile cover")
	bad.Images = []lidarr.Image{{CoverType: "cover", URL: "https://evil.example/x.jpg"}}

	got, err := New(&fakeSource{hits: []lidarr.LookupAlbum{h, bad}}).Search(context.Background(), "x")
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Cover != "/api/cover?p=%2FMediaCoverProxy%2Fab%2Fc.jpg" {
		t.Errorf("cover = %q", got[0].Cover)
	}
	if got[1].Cover != "" {
		t.Errorf("a non-proxy cover URL must be dropped, got %q", got[1].Cover)
	}
	if got[0].SecondaryTypes == nil {
		t.Error("SecondaryTypes must be an empty slice, not nil, so it encodes as []")
	}
}
