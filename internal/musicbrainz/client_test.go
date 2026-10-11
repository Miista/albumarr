package musicbrainz

import (
	"context"
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
	for _, good := range []string{"https://example.org/albumreq", "someone@example.org"} {
		if _, err := New(good); err != nil {
			t.Errorf("New(%q) rejected a good contact: %v", good, err)
		}
	}
}

func TestUserAgents(t *testing.T) {
	ua, err := UserAgent("https://example.org/albumreq")
	if err != nil || ua != "albumreq/0.1 ( https://example.org/albumreq )" {
		t.Errorf("UserAgent = %q, %v", ua, err)
	}
	if strings.ContainsAny(AnonymousUserAgent, "()@") || !strings.HasPrefix(AnonymousUserAgent, "albumreq/") {
		t.Errorf("AnonymousUserAgent must carry no contact: %q", AnonymousUserAgent)
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
	if _, err := c.Search(context.Background(), "x", 5, 0); err != nil {
		t.Fatal(err)
	}
}

func TestSearchParsesHits(t *testing.T) {
	c, _ := newTest(t, 0, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/release-group" || r.URL.Query().Get("limit") != "10" {
			t.Errorf("unexpected request %s", r.URL)
		}
		fmt.Fprint(w, `{"count":5445,"release-groups":[
			{"id":"`+rgID+`","title":"OK Computer","primary-type":"Album","first-release-date":"1997-05-21","score":100,
			 "artist-credit":[{"name":"Radiohead","joinphrase":"","artist":{"id":"a74b1b7f-71a5-4011-9441-d0b5e4122711"}}]},
			{"id":"x","title":"Golden","primary-type":"Single","secondary-types":["Soundtrack"],"score":89,
			 "artist-credit":[{"name":"HUNTR/X","joinphrase":" feat. "},{"name":"Someone","joinphrase":""}]}]}`)
	})
	page, err := c.Search(context.Background(), "ok computer", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	hits := page.Hits
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
	if page.Total != 5445 {
		t.Errorf("Total = %d, want the total match count 5445", page.Total)
	}
}

func TestSearchPagingAndLimitsAreClamped(t *testing.T) {
	var gotLimit, gotOffset string
	c, _ := newTest(t, 0, func(w http.ResponseWriter, r *http.Request) {
		gotLimit, gotOffset = r.URL.Query().Get("limit"), r.URL.Query().Get("offset")
		fmt.Fprint(w, `{"count":0,"release-groups":[]}`)
	})
	for i, tc := range []struct {
		limit, offset         int
		wantLimit, wantOffset string
	}{
		{24, 48, "24", "48"},
		{0, -5, "1", "0"},
		{5000, 0, "100", "0"},
	} {
		if _, err := c.Search(context.Background(), fmt.Sprintf("q%d", i), tc.limit, tc.offset); err != nil {
			t.Fatal(err)
		}
		if gotLimit != tc.wantLimit || gotOffset != tc.wantOffset {
			t.Errorf("limit=%d offset=%d sent limit=%s offset=%s, want %s and %s", tc.limit, tc.offset, gotLimit, gotOffset, tc.wantLimit, tc.wantOffset)
		}
	}
}

func TestDifferentPagesAreCachedSeparately(t *testing.T) {
	c, calls := newTest(t, 0, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"count":60,"release-groups":[]}`) })
	for _, off := range []int{0, 24, 0, 24, 48} {
		if _, err := c.Search(context.Background(), "same term", 24, off); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 3 {
		t.Errorf("upstream calls = %d, want 3 (one per distinct page)", calls.Load())
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
	if _, err := c.Search(context.Background(), `AC/DC "live" (1979) - *`, 5, 0); err != nil {
		t.Fatal(err)
	}
}

func TestValidID(t *testing.T) {
	if !ValidID(rgID) {
		t.Error("a real release-group ID was rejected")
	}
	for _, bad := range []string{
		"", "nope", "../../etc/passwd", rgID + "/../x", rgID + "?x=1", rgID + "#f", rgID + "/front-250",
		strings.ToUpper(rgID), "b1392450-e666-3926-a536-22c65f83443", rgID + "0", " " + rgID, rgID + "\n",
	} {
		if ValidID(bad) {
			t.Errorf("ValidID(%q) = true, want false", bad)
		}
	}
}

func TestRepeatedSearchesAreCached(t *testing.T) {
	c, calls := newTest(t, 0, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"release-groups":[]}`) })
	for i := 0; i < 3; i++ {
		if _, err := c.Search(context.Background(), "same", 5, 0); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.Search(context.Background(), "different", 5, 0); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Errorf("upstream calls = %d, want 2", calls.Load())
	}
}

func TestConcurrentIdenticalSearchesShareOneFetch(t *testing.T) {
	c, calls := newTest(t, 0, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(60 * time.Millisecond)
		fmt.Fprint(w, `{"release-groups":[]}`)
	})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Search(context.Background(), "burst", 5, 0); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Errorf("upstream calls = %d, want 1", calls.Load())
	}
}

func TestSearchesAreSpacedByTheGap(t *testing.T) {
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
			c.Search(context.Background(), fmt.Sprintf("term %d", i), 5, 0)
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
	if _, err := c.Search(context.Background(), "first", 5, 0); err != nil { // takes the immediate slot
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.Search(ctx, "second", 5, 0)
	if err == nil || time.Since(start) > time.Second {
		t.Errorf("err = %v after %v, want a prompt context error", err, time.Since(start))
	}
}

func TestUpstreamFailuresAreErrorsAndNotCached(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	c, calls := newTest(t, 0, func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, `{"release-groups":[]}`)
	})
	if _, err := c.Search(context.Background(), "x", 5, 0); err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("first call: err = %v, want a 503 error", err)
	}
	fail.Store(false)
	if _, err := c.Search(context.Background(), "x", 5, 0); err != nil {
		t.Errorf("second call: %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("calls = %d, want 2", calls.Load())
	}
}

func TestBadJSONIsAnError(t *testing.T) {
	c, _ := newTest(t, 0, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `<html>`) })
	if _, err := c.Search(context.Background(), "x", 5, 0); err == nil {
		t.Error("an HTML response must not decode")
	}
}

func TestErrorsDoNotLeakTheContact(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	c, _ := NewWithBase(srv.URL, "private-contact@example.org", 0)
	srv.Close() // connection errors from now on
	_, err := c.Search(context.Background(), "x", 5, 0)
	if err == nil || strings.Contains(err.Error(), "private-contact") {
		t.Errorf("err = %v", err)
	}
}
