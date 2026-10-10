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
		if !strings.HasPrefix(ua, "albumreq/") || strings.ContainsAny(ua, "()@") {
			t.Errorf("User-Agent = %q: it must identify albumreq and carry no contact", ua)
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
