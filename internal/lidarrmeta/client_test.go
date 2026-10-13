package lidarrmeta

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const rgID = "b1392450-e666-3926-a536-22c65f834433"

// fixture mirrors the real service's shape: lower-case top-level keys,
// Pascal-case release and track keys, ratings out of 10, countries and labels
// as lists.
const fixture = `{
  "id":"` + rgID + `","title":"OK Computer","type":"Album","secondarytypes":[],"releasedate":"1997-05-21",
  "disambiguation":"","genres":["Alternative Rock","Art Rock"],"overview":null,
  "rating":{"Count":89,"Value":9.1},
  "artists":[{"artistname":"Radiohead","id":"a74b1b7f-71a5-4011-9441-d0b5e4122711"}],
  "Releases":[
    {"Id":"r-vinyl","Title":"OK Computer","Status":"Official","Country":["United Kingdom"],"Label":["Parlophone"],
     "Media":[{"Position":1,"Name":"","Format":"12\" Vinyl"},{"Position":2,"Name":"","Format":"12\" Vinyl"}],
     "ReleaseDate":"1997-06-16","TrackCount":3,
     "Tracks":[{"TrackName":"Airbag","TrackNumber":"A1","TrackPosition":1,"DurationMs":284400,"MediumNumber":1},
               {"TrackName":"Untimed","TrackNumber":"A2","TrackPosition":2,"DurationMs":null,"MediumNumber":1},
               {"TrackName":"Side B","TrackNumber":"B1","TrackPosition":1,"DurationMs":1000,"MediumNumber":2}]},
    {"Id":"r-cd","Title":"OK Computer","Status":"Official","Country":[],"Label":[],
     "Media":[{"Position":1,"Name":"","Format":"CD"}],"ReleaseDate":"1997","TrackCount":3,
     "Tracks":[{"TrackName":"Airbag","TrackNumber":"1","TrackPosition":1,"DurationMs":284400,"MediumNumber":1},
               {"TrackName":"Paranoid Android","TrackNumber":"2","TrackPosition":2,"DurationMs":383493,"MediumNumber":1},
               {"TrackName":"Subterranean","TrackNumber":"3","TrackPosition":3,"DurationMs":267706,"MediumNumber":1}]}
  ]}`

func newTest(t *testing.T, h http.HandlerFunc) (*Client, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL), &calls
}

func TestAlbumParsesTheServicesShape(t *testing.T) {
	c, _ := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/album/"+rgID {
			t.Errorf("path = %s", r.URL.Path)
		}
		fmt.Fprint(w, fixture)
	})
	a, err := c.Album(context.Background(), rgID)
	if err != nil {
		t.Fatal(err)
	}
	if a.Title != "OK Computer" || a.Artist != "Radiohead" || a.ArtistID == "" || a.Type != "Album" || a.ReleaseDate != "1997-05-21" {
		t.Errorf("album fields wrong: %+v", a)
	}
	if a.RatingValue != 9.1 || a.RatingVotes != 89 || len(a.Genres) != 2 || a.Overview != "" || a.SecondaryTypes == nil {
		t.Errorf("rating, genres or overview wrong: %+v", a)
	}
	if len(a.Editions) != 2 {
		t.Fatalf("editions = %d, want 2", len(a.Editions))
	}
	v := a.Editions[0]
	if v.ID != "r-vinyl" || v.Country != "United Kingdom" || v.Label != "Parlophone" || v.Format != `12" Vinyl` || len(v.Tracks) != 3 {
		t.Errorf("vinyl edition wrong: %+v", v)
	}
	if v.Tracks[0].Number != "A1" || v.Tracks[0].DurationMs != 284400 || v.Tracks[1].DurationMs != 0 || v.Tracks[2].Disc != 2 {
		t.Errorf("tracks wrong: %+v", v.Tracks)
	}
	if cd := a.Editions[1]; cd.Country != "" || cd.Label != "" || cd.Format != "CD" {
		t.Errorf("an edition without country or label must parse: %+v", cd)
	}
}

func TestInvalidIDsNeverReachTheService(t *testing.T) {
	c, calls := newTest(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, fixture) })
	for _, id := range []string{"", "nope", "../../etc/passwd", rgID + "/../x", rgID + "?x=1", strings.ToUpper(rgID), rgID + "0", " " + rgID} {
		if _, err := c.Album(context.Background(), id); !errors.Is(err, ErrInvalidID) {
			t.Errorf("Album(%q): err = %v, want ErrInvalidID", id, err)
		}
	}
	if calls.Load() != 0 {
		t.Errorf("%d invalid IDs reached the service", calls.Load())
	}
}

func TestRequestsCarryNoContact(t *testing.T) {
	c, _ := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		ua := r.Header.Get("User-Agent")
		if !strings.HasPrefix(ua, "albumarr/") || strings.ContainsAny(ua, "()@") {
			t.Errorf("User-Agent = %q: it must identify albumarr and carry no contact", ua)
		}
		fmt.Fprint(w, fixture)
	})
	if _, err := c.Album(context.Background(), rgID); err != nil {
		t.Fatal(err)
	}
}

func TestNotFoundAndFailures(t *testing.T) {
	var status atomic.Int32
	status.Store(404)
	c, _ := newTest(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(int(status.Load())) })
	if _, err := c.Album(context.Background(), rgID); !errors.Is(err, ErrNotFound) {
		t.Errorf("404: err = %v, want ErrNotFound", err)
	}
	status.Store(502)
	if _, err := c.Album(context.Background(), rgID); err == nil || !strings.Contains(err.Error(), "502") {
		t.Errorf("502: err = %v", err)
	}
}

func TestFailuresAreNotCached(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	c, calls := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		fmt.Fprint(w, fixture)
	})
	if _, err := c.Album(context.Background(), rgID); err == nil {
		t.Fatal("expected the first call to fail")
	}
	fail.Store(false)
	if _, err := c.Album(context.Background(), rgID); err != nil {
		t.Errorf("second call: %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("calls = %d, want 2", calls.Load())
	}
}

func TestBadJSONIsAnError(t *testing.T) {
	c, _ := newTest(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `<html>`) })
	if _, err := c.Album(context.Background(), rgID); err == nil {
		t.Error("an HTML response must not decode")
	}
}

func TestRepeatedAlbumsAreCached(t *testing.T) {
	c, calls := newTest(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, fixture) })
	for i := 0; i < 3; i++ {
		if _, err := c.Album(context.Background(), rgID); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Errorf("upstream calls = %d, want 1", calls.Load())
	}
}

func TestConcurrentRequestsShareOneFetchAndACallerCanWalkAway(t *testing.T) {
	c, calls := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(80 * time.Millisecond)
		fmt.Fprint(w, fixture)
	})
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Album(context.Background(), rgID); err != nil {
				t.Error(err)
			}
		}()
	}
	// One impatient caller gives up early without disturbing the others.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	if _, err := c.Album(ctx, rgID); err == nil {
		t.Error("the impatient caller should have timed out")
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Errorf("upstream calls = %d, want 1", calls.Load())
	}
}

func TestErrorsDoNotLeakTheServiceURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	c := New(srv.URL + "/secret-prefix")
	srv.Close()
	_, err := c.Album(context.Background(), rgID)
	if err == nil || strings.Contains(err.Error(), "secret-prefix") {
		t.Errorf("err = %v", err)
	}
}

func TestPickEdition(t *testing.T) {
	ed := func(id, date string, n int) Edition {
		return Edition{ID: id, Date: date, TrackCount: n, Tracks: make([]Track, n)}
	}
	edf := func(id, date, format string, n int) Edition {
		e := ed(id, date, n)
		e.Format = format
		return e
	}
	cases := []struct {
		name string
		in   []Edition
		want string
	}{
		{"most common count wins", []Edition{ed("a", "1997", 12), ed("b", "1998", 12), ed("c", "1997", 27)}, "a"},
		{"earliest among the most common", []Edition{ed("late", "2009-01-01", 12), ed("early", "1997-06-16", 12), ed("x", "1990", 27)}, "early"},
		{"tie goes to the shorter list", []Edition{ed("deluxe", "1990", 27), ed("std", "2000", 12)}, "std"},
		{"undated sorts last", []Edition{ed("nodate", "", 12), ed("dated", "2005", 12)}, "dated"},
		{"single edition", []Edition{ed("only", "", 5)}, "only"},
		{"a CD beats an earlier cassette", []Edition{edf("tape", "1997-01-01", "Cassette", 12), edf("cd", "1997-06-16", "CD", 12)}, "cd"},
		{"digital media counts like a CD", []Edition{edf("vinyl", "1997-01-01", `12" Vinyl`, 12), edf("web", "2010", "Digital Media", 12)}, "web"},
		{"earliest CD among CDs", []Edition{edf("cd2", "2005", "CD", 12), edf("cd1", "1997", "CD", 12), edf("tape", "1990", "Cassette", 12)}, "cd1"},
		{"no CD at all falls back to the earliest", []Edition{edf("v2", "2005", `12" Vinyl`, 12), edf("tape", "1990", "Cassette", 12)}, "tape"},
		{"a more common track count still wins over format", []Edition{edf("cd-deluxe", "1990", "CD", 27), edf("tape-a", "1991", "Cassette", 12), edf("tape-b", "1992", "Cassette", 12)}, "tape-a"},
		{"editions without tracks are ignored", []Edition{ed("empty", "1990", 0), ed("real", "2000", 9)}, "real"},
	}
	for _, c := range cases {
		got, ok := PickEdition(c.in)
		if !ok || got.ID != c.want {
			t.Errorf("%s: got %q (ok=%v), want %q", c.name, got.ID, ok, c.want)
		}
	}
	if _, ok := PickEdition(nil); ok {
		t.Error("no editions must not yield a pick")
	}
	if _, ok := PickEdition([]Edition{ed("empty", "1990", 0)}); ok {
		t.Error("editions without any tracks must not yield a pick")
	}
}

func TestPlaceholderDatesAndBracketedRegionsAreCleanedUp(t *testing.T) {
	c, _ := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"id":"`+rgID+`","title":"X","releasedate":"1997-05-21","Releases":[
			{"Id":"unknown","Status":"Official","Country":["[Worldwide]"],"ReleaseDate":"0001-01-01","Media":[{"Format":"CD"}],"TrackCount":1,
			 "Tracks":[{"TrackName":"a","TrackNumber":"1","TrackPosition":1,"MediumNumber":1}]},
			{"Id":"dated","Status":"Official","Country":["United Kingdom"],"ReleaseDate":"2001-02-03","Media":[{"Format":"CD"}],"TrackCount":1,
			 "Tracks":[{"TrackName":"a","TrackNumber":"1","TrackPosition":1,"MediumNumber":1}]}]}`)
	})
	a, err := c.Album(context.Background(), rgID)
	if err != nil {
		t.Fatal(err)
	}
	if a.Editions[0].Date != "" || a.Editions[0].Country != "Worldwide" || a.Editions[1].Country != "United Kingdom" {
		t.Errorf("placeholder date or bracketed region not cleaned: %+v", a.Editions)
	}
	if pick, _ := PickEdition(a.Editions); pick.ID != "dated" {
		t.Errorf("an unknown date must not win as the earliest edition, picked %q", pick.ID)
	}
}

const artistFixture = `{"id":"b8a7c51f-362c-4dcb-a259-bc6e0095f0a6","artistname":"Ed Sheeran","disambiguation":"UK singer‐songwriter","type":"Person",
 "images":[{"CoverType":"Banner","Url":"https://cdn.example/banner.jpg"},{"CoverType":"Fanart","Url":"https://cdn.example/fanart.jpg"},{"CoverType":"Poster","Url":"https://cdn.example/poster.jpg"},{"CoverType":"Logo","Url":""}],
 "Albums":[
  {"Id":"c24e5416-0b6b-3b1d-8a3a-000000000001","Title":"×","Type":"Album","SecondaryTypes":[],"ReleaseDate":"2014-06-20"},
  {"Id":"6f5bdb7d-eb3b-4b24-abc3-3312621cb7e8","Title":"Sing","Type":"Single","SecondaryTypes":null,"ReleaseDate":"2014-04-07"},
  {"Id":"85ae85d3-7f71-473f-ac8f-5bb9be1a10f7","Title":"iTunes Festival: London 2012","Type":"EP","SecondaryTypes":["Live"],"ReleaseDate":"0001-01-01T00:00:00Z"},
  {"Id":"not-a-uuid","Title":"Broken","Type":"Album"},
  {"Id":"2a621cc8-309b-42c6-a2ac-5ac6f73e7ab0","Title":"  ","Type":"Single"}]}`

func TestArtistParsesTheServicesShapeAndSkipsWhatCannotBeUsed(t *testing.T) {
	c, _ := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/artist/b8a7c51f-362c-4dcb-a259-bc6e0095f0a6" {
			t.Errorf("path %q", r.URL.Path)
		}
		fmt.Fprint(w, artistFixture)
	})
	a, err := c.Artist(context.Background(), "b8a7c51f-362c-4dcb-a259-bc6e0095f0a6")
	if err != nil {
		t.Fatal(err)
	}
	if a.Name != "Ed Sheeran" || a.Type != "Person" || a.Disambiguation == "" || len(a.Albums) != 3 {
		t.Fatalf("artist: %+v", a)
	}
	if x := a.Albums[0]; x.Title != "×" || x.Type != "Album" || x.ReleaseDate != "2014-06-20" || x.SecondaryTypes == nil {
		t.Errorf("first album: %+v", x)
	}
	if a.Albums[1].SecondaryTypes == nil || a.Albums[2].SecondaryTypes[0] != "Live" || a.Albums[2].ReleaseDate != "" {
		t.Errorf("secondary types must be a slice and an unknown date empty: %+v", a.Albums[1:])
	}
}

func TestArtistInvalidIDNotFoundFailureAndCache(t *testing.T) {
	var status atomic.Int32
	status.Store(200)
	c, calls := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		if s := int(status.Load()); s != 200 {
			w.WriteHeader(s)
			return
		}
		fmt.Fprint(w, artistFixture)
	})
	const id = "b8a7c51f-362c-4dcb-a259-bc6e0095f0a6"
	if _, err := c.Artist(context.Background(), "../etc/passwd"); !errors.Is(err, ErrInvalidID) || calls.Load() != 0 {
		t.Errorf("an invalid id must not reach the service: %v, %d calls", err, calls.Load())
	}
	for i := 0; i < 3; i++ {
		if _, err := c.Artist(context.Background(), id); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Errorf("a repeated artist must come from the cache: %d calls", calls.Load())
	}
	status.Store(404)
	if _, err := c.Artist(context.Background(), "3bca1168-e7c6-4beb-9af9-db9dc674b1fa"); !errors.Is(err, ErrNotFound) {
		t.Errorf("404: %v", err)
	}
	status.Store(500)
	if _, err := c.Artist(context.Background(), "c590f877-9bd4-45ac-b0f5-799ac5496d1a"); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("500 is a failure, not 'not found': %v", err)
	}
}

func TestArtistImagesAndWhichOneIsThePhoto(t *testing.T) {
	c, _ := newTest(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, artistFixture) })
	a, err := c.Artist(context.Background(), "b8a7c51f-362c-4dcb-a259-bc6e0095f0a6")
	if err != nil || len(a.Images) != 4 || a.Images[0].Type != "Banner" || a.Images[2].URL != "https://cdn.example/poster.jpg" {
		t.Fatalf("%+v, %v", a.Images, err)
	}
	if got := a.PhotoURL(); got != "https://cdn.example/poster.jpg" {
		t.Errorf("the poster is preferred: %q", got)
	}
	for name, tc := range map[string]struct {
		images []ArtistImage
		want   string
	}{
		"fanart when there is no poster":     {[]ArtistImage{{"Banner", "b"}, {"fanart", "f"}}, "f"},
		"the banner and logo carry the name": {[]ArtistImage{{"Banner", "b"}, {"Logo", "l"}}, ""},
		"a poster with no address is no use": {[]ArtistImage{{"Poster", ""}, {"Fanart", "f"}}, "f"},
		"none":                               {nil, ""},
	} {
		if got := (Artist{Images: tc.images}).PhotoURL(); got != tc.want {
			t.Errorf("%s: %q, want %q", name, got, tc.want)
		}
	}
}
