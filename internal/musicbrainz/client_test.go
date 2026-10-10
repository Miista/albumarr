package musicbrainz

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

const (
	rgID  = "b1392450-e666-3926-a536-22c65f834433"
	relID = "3c8cbdc8-555f-3987-8f03-6baf9488d1b0"
)

// newTest returns a client against a fake MusicBrainz and a counter of the
// requests that reached it.
func newTest(t *testing.T, gap time.Duration, h http.HandlerFunc) (*Client, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	c, err := NewWithBase(srv.URL, "https://example.org/albumreq", gap)
	if err != nil {
		t.Fatal(err)
	}
	return c, &calls
}

func TestNewRequiresASaneContact(t *testing.T) {
	for _, bad := range []string{"", "   ", "a\nb", "a\rb", "x (y)", strings.Repeat("a", 201)} {
		if _, err := New(bad); err == nil {
			t.Errorf("New(%q) accepted a bad contact", bad)
		}
	}
	if _, err := New("https://example.org/albumreq"); err != nil {
		t.Errorf("a URL contact was rejected: %v", err)
	}
}

func TestRequestsCarryTheUserAgentAndAskForJSON(t *testing.T) {
	c, _ := newTest(t, 0, func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.Header.Get("User-Agent"), "albumreq/0.1 ( https://example.org/albumreq )"; got != want {
			t.Errorf("User-Agent = %q, want %q", got, want)
		}
		if r.URL.Query().Get("fmt") != "json" {
			t.Errorf("fmt = %q", r.URL.Query().Get("fmt"))
		}
		fmt.Fprint(w, `{"release-groups":[]}`)
	})
	if _, err := c.Search(context.Background(), "x", 5); err != nil {
		t.Fatal(err)
	}
}

func TestSearchParsesHits(t *testing.T) {
	c, _ := newTest(t, 0, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/release-group" || r.URL.Query().Get("limit") != "10" {
			t.Errorf("unexpected request %s", r.URL)
		}
		fmt.Fprint(w, `{"release-groups":[
			{"id":"`+rgID+`","title":"OK Computer","primary-type":"Album","first-release-date":"1997-05-21","score":100,
			 "artist-credit":[{"name":"Radiohead","joinphrase":"","artist":{"id":"a74b1b7f-71a5-4011-9441-d0b5e4122711"}}]},
			{"id":"x","title":"Golden","primary-type":"Single","secondary-types":["Soundtrack"],"score":89,
			 "artist-credit":[{"name":"HUNTR/X","joinphrase":" feat. "},{"name":"Someone","joinphrase":""}]}]}`)
	})
	hits, err := c.Search(context.Background(), "ok computer", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 || hits[0].Title != "OK Computer" || hits[0].Artist != "Radiohead" || hits[0].Type != "Album" ||
		hits[0].FirstReleaseDate != "1997-05-21" || hits[0].Score != 100 || hits[0].ArtistID == "" {
		t.Errorf("first hit wrong: %+v", hits[0])
	}
	if hits[1].Artist != "HUNTR/X feat. Someone" || hits[1].SecondaryTypes[0] != "Soundtrack" {
		t.Errorf("second hit wrong: %+v", hits[1])
	}
	if hits[0].SecondaryTypes == nil {
		t.Error("SecondaryTypes must be an empty slice, not nil")
	}
}

func TestSearchEscapesLuceneOperators(t *testing.T) {
	c, _ := newTest(t, 0, func(w http.ResponseWriter, r *http.Request) {
		want := `AC\/DC \"live\" \(1979\) \- \*`
		if got := r.URL.Query().Get("query"); got != want {
			t.Errorf("query = %q, want %q", got, want)
		}
		fmt.Fprint(w, `{"release-groups":[]}`)
	})
	if _, err := c.Search(context.Background(), `AC/DC "live" (1979) - *`, 5); err != nil {
		t.Fatal(err)
	}
}

func TestReleaseGroupDetails(t *testing.T) {
	c, _ := newTest(t, 0, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/release-group/"+rgID {
			t.Errorf("path = %s", r.URL.Path)
		}
		fmt.Fprint(w, `{"id":"`+rgID+`","title":"OK Computer","primary-type":"Album","first-release-date":"1997-05-21",
			"disambiguation":"","artist-credit":[{"name":"Radiohead","joinphrase":"","artist":{"id":"a1"}}],
			"genres":[{"name":"c","count":1},{"name":"rock","count":9},{"name":"b","count":2},{"name":"d","count":1},
			          {"name":"e","count":1},{"name":"f","count":1},{"name":"g","count":1},{"name":"art rock","count":5}],
			"rating":{"value":4.55,"votes-count":89}}`)
	})
	rg, err := c.ReleaseGroup(context.Background(), rgID)
	if err != nil {
		t.Fatal(err)
	}
	if rg.Title != "OK Computer" || rg.Artist != "Radiohead" || rg.RatingValue != 4.55 || rg.RatingVotes != 89 {
		t.Errorf("unexpected details: %+v", rg)
	}
	if len(rg.Genres) != 6 || rg.Genres[0] != "rock" || rg.Genres[1] != "art rock" {
		t.Errorf("genres should be the six most voted, best first: %v", rg.Genres)
	}
}

func TestReleaseGroupWithoutARating(t *testing.T) {
	c, _ := newTest(t, 0, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"id":"`+rgID+`","title":"Unrated","rating":{"value":null,"votes-count":0}}`)
	})
	rg, err := c.ReleaseGroup(context.Background(), rgID)
	if err != nil || rg.RatingValue != 0 || rg.RatingVotes != 0 {
		t.Errorf("got %+v, %v", rg, err)
	}
}

func TestInvalidIDsNeverReachUpstream(t *testing.T) {
	c, calls := newTest(t, 0, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{}`) })
	bad := []string{
		"", "nope", "../../etc/passwd", rgID + "/../x", rgID + "?x=1", rgID + "#f",
		strings.ToUpper(rgID), "b1392450-e666-3926-a536-22c65f83443", rgID + "0", " " + rgID,
	}
	for _, id := range bad {
		if _, err := c.ReleaseGroup(context.Background(), id); !errors.Is(err, ErrInvalidID) {
			t.Errorf("ReleaseGroup(%q): err = %v, want ErrInvalidID", id, err)
		}
		if _, err := c.Editions(context.Background(), id); !errors.Is(err, ErrInvalidID) {
			t.Errorf("Editions(%q): err = %v, want ErrInvalidID", id, err)
		}
		if _, err := c.Tracks(context.Background(), id); !errors.Is(err, ErrInvalidID) {
			t.Errorf("Tracks(%q): err = %v, want ErrInvalidID", id, err)
		}
	}
	if calls.Load() != 0 {
		t.Errorf("%d invalid IDs reached MusicBrainz", calls.Load())
	}
	if !ValidID(rgID) {
		t.Error("a real release-group ID was rejected")
	}
}

func TestEditionsSumDiscsAndFallBackWhenNothingIsOfficial(t *testing.T) {
	c, calls := newTest(t, 0, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("release-group") != rgID {
			t.Errorf("release-group = %q", r.URL.Query().Get("release-group"))
		}
		if r.URL.Query().Get("status") == "official" {
			fmt.Fprint(w, `{"releases":[]}`)
			return
		}
		fmt.Fprint(w, `{"releases":[
			{"id":"`+relID+`","date":"2001","country":"XW","media":[{"format":"CD","track-count":10},{"format":"CD","track-count":8}]},
			{"id":"empty","date":"2002","media":[]}]}`)
	})
	eds, err := c.Editions(context.Background(), rgID)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Errorf("calls = %d, want 2 (official, then all)", calls.Load())
	}
	if len(eds) != 1 || eds[0].TrackCount != 18 || eds[0].Format != "CD" || eds[0].Country != "XW" {
		t.Errorf("unexpected editions: %+v", eds)
	}
}

func TestPickEdition(t *testing.T) {
	ed := func(id, date string, n int) Edition { return Edition{ID: id, Date: date, TrackCount: n} }
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
}

func TestTracksAcrossDiscs(t *testing.T) {
	c, _ := newTest(t, 0, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/release/"+relID {
			t.Errorf("path = %s", r.URL.Path)
		}
		fmt.Fprint(w, `{"media":[
			{"position":1,"tracks":[{"number":"1","title":"Airbag","length":284400},{"number":"2","title":"Untimed","length":null}]},
			{"position":2,"tracks":[{"number":"A1","title":"Side A","length":1000}]}]}`)
	})
	tr, err := c.Tracks(context.Background(), relID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tr) != 3 || tr[0].Title != "Airbag" || tr[0].DurationMs != 284400 || tr[0].Disc != 1 {
		t.Errorf("unexpected tracks: %+v", tr)
	}
	if tr[1].DurationMs != 0 || tr[2].Disc != 2 || tr[2].Number != "A1" {
		t.Errorf("unexpected tracks: %+v", tr)
	}
}

func TestRepeatedRequestsAreCached(t *testing.T) {
	c, calls := newTest(t, 0, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"release-groups":[]}`) })
	for i := 0; i < 3; i++ {
		if _, err := c.Search(context.Background(), "same", 5); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.Search(context.Background(), "different", 5); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Errorf("upstream calls = %d, want 2", calls.Load())
	}
}

func TestConcurrentIdenticalRequestsShareOneFetch(t *testing.T) {
	c, calls := newTest(t, 0, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(60 * time.Millisecond)
		fmt.Fprint(w, `{"release-groups":[]}`)
	})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Search(context.Background(), "burst", 5); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Errorf("upstream calls = %d, want 1", calls.Load())
	}
}

func TestRequestsAreSpacedByTheGap(t *testing.T) {
	const gap = 80 * time.Millisecond
	var mu sync.Mutex
	var times []time.Time
	c, _ := newTest(t, gap, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		times = append(times, time.Now())
		mu.Unlock()
		fmt.Fprint(w, `{"release-groups":[]}`)
	})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c.Search(context.Background(), fmt.Sprintf("term %d", i), 5)
		}(i)
	}
	wg.Wait()
	if len(times) != 4 {
		t.Fatalf("got %d requests", len(times))
	}
	for i := 1; i < len(times); i++ {
		if d := times[i].Sub(times[i-1]); d < gap-15*time.Millisecond {
			t.Errorf("request %d followed the previous one after %v, want >= %v", i, d, gap)
		}
	}
}

func TestWaitingForASlotHonoursContext(t *testing.T) {
	c, _ := newTest(t, 5*time.Second, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"release-groups":[]}`) })
	if _, err := c.Search(context.Background(), "first", 5); err != nil { // takes the immediate slot
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.Search(ctx, "second", 5)
	if err == nil || time.Since(start) > time.Second {
		t.Errorf("err = %v after %v, want a prompt context error", err, time.Since(start))
	}
}

func TestUpstreamFailures(t *testing.T) {
	var status atomic.Int32
	c, _ := newTest(t, 0, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(int(status.Load())) })

	status.Store(404)
	if _, err := c.ReleaseGroup(context.Background(), rgID); !errors.Is(err, ErrNotFound) {
		t.Errorf("404: err = %v, want ErrNotFound", err)
	}
	status.Store(503)
	if _, err := c.Tracks(context.Background(), relID); err == nil || !strings.Contains(err.Error(), "503") {
		t.Errorf("503: err = %v", err)
	}
	// Failures are not cached: once MusicBrainz recovers the same call works.
	status.Store(200)
	c2, _ := newTest(t, 0, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"media":[]}`) })
	if _, err := c2.Tracks(context.Background(), relID); err != nil {
		t.Errorf("a healthy upstream failed: %v", err)
	}
}

func TestFailuresAreNotCached(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	c, calls := newTest(t, 0, func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, `{"release-groups":[]}`)
	})
	if _, err := c.Search(context.Background(), "x", 5); err == nil {
		t.Fatal("expected the first call to fail")
	}
	fail.Store(false)
	if _, err := c.Search(context.Background(), "x", 5); err != nil {
		t.Errorf("second call: %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("calls = %d, want 2", calls.Load())
	}
}

func TestErrorsDoNotLeakTheContact(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	c, _ := NewWithBase(srv.URL, "private-contact@example.org", 0)
	srv.Close() // connection errors from now on
	_, err := c.Search(context.Background(), "x", 5)
	if err == nil || strings.Contains(err.Error(), "private-contact") {
		t.Errorf("err = %v", err)
	}
}
