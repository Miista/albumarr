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
	c, err := NewWithBase(srv.URL, "https://example.org/albumarr", gap)
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
	for _, good := range []string{"https://example.org/albumarr", "someone@example.org"} {
		if _, err := New(good); err != nil {
			t.Errorf("New(%q) rejected a good contact: %v", good, err)
		}
	}
}

func TestUserAgents(t *testing.T) {
	ua, err := UserAgent("https://example.org/albumarr")
	if err != nil || ua != "albumarr/0.1 ( https://example.org/albumarr )" {
		t.Errorf("UserAgent = %q, %v", ua, err)
	}
	if strings.ContainsAny(AnonymousUserAgent, "()@") || !strings.HasPrefix(AnonymousUserAgent, "albumarr/") {
		t.Errorf("AnonymousUserAgent must carry no contact: %q", AnonymousUserAgent)
	}
}

func TestRequestsCarryTheUserAgentAndAskForJSON(t *testing.T) {
	c, _ := newTest(t, 0, func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.Header.Get("User-Agent"), "albumarr/0.1 ( https://example.org/albumarr )"; got != want {
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
	fast(c)
	if _, err := c.Search(context.Background(), "x", 5, 0); err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("first call: err = %v, want a 503 error", err)
	}
	fail.Store(false)
	if _, err := c.Search(context.Background(), "x", 5, 0); err != nil {
		t.Errorf("second call: %v", err)
	}
	if calls.Load() != 3 { // the failed search and its one retry, then the search that works
		t.Errorf("calls = %d, want 3", calls.Load())
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

const artistID = "b8a7c51f-362c-4dcb-a259-bc6e0095f0a6"

func TestSearchArtistsParsesHitsAndDropsWhatCannotBeUsed(t *testing.T) {
	c, _ := newTest(t, 0, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/artist" || r.URL.Query().Get("limit") != "6" {
			t.Errorf("unexpected request %s", r.URL)
		}
		fmt.Fprint(w, `{"artists":[
			{"id":"`+artistID+`","name":"Ed Sheeran","type":"Person","disambiguation":"UK singer‐songwriter","country":"GB","score":100},
			{"id":"not-a-uuid","name":"Broken","score":99},
			{"id":"`+rgID+`","name":"   ","score":98},
			{"id":"3bca1168-e7c6-4beb-9af9-db9dc674b1fa","name":"Ed Sheeran","score":87}]}`)
	})
	hits, err := c.SearchArtists(context.Background(), "ed sheeran", 6)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 || hits[0].ID != artistID || hits[0].Name != "Ed Sheeran" || hits[0].Type != "Person" ||
		hits[0].Disambiguation != "UK singer‐songwriter" || hits[0].Country != "GB" || hits[0].Score != 100 || hits[1].Type != "" {
		t.Errorf("hits: %+v", hits)
	}
}

func TestSearchArtistsClampsTheLimitEscapesTheTermAndCaches(t *testing.T) {
	var seen []string
	c, calls := newTest(t, 0, func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Query().Get("limit")+" "+r.URL.Query().Get("query"))
		fmt.Fprint(w, `{"artists":[]}`)
	})
	for _, limit := range []int{0, 1000} {
		if _, err := c.SearchArtists(context.Background(), `AC/DC (live)`, limit); err != nil {
			t.Fatal(err)
		}
	}
	if len(seen) != 2 || seen[0] != `1 AC\/DC \(live\)` || seen[1] != `25 AC\/DC \(live\)` {
		t.Errorf("limit and operators: %q", seen)
	}
	for i := 0; i < 3; i++ {
		c.SearchArtists(context.Background(), "again", 6)
	}
	if calls.Load() != 3 { // two distinct limits above, then one for "again"
		t.Errorf("a repeated search must come from the cache: %d upstream calls", calls.Load())
	}
}

func TestSearchArtistsFailuresAndBadJSON(t *testing.T) {
	c, _ := newTest(t, 0, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `<html>`) })
	if _, err := c.SearchArtists(context.Background(), "x", 6); err == nil {
		t.Error("a bad body must be an error")
	}
	c, _ = newTest(t, 0, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", http.StatusServiceUnavailable) })
	if _, err := c.SearchArtists(context.Background(), "x", 6); err == nil {
		t.Error("a 503 must be an error")
	}
}

// --- the one retry ---

func fast(c *Client) *Client { c.retryDelay = time.Millisecond; return c }

func TestASearchThatFailsOnceIsRetriedOnceAndSucceeds(t *testing.T) {
	for _, status := range []int{500, 502, 503, 504, 429} {
		var n atomic.Int32
		c, calls := newTest(t, 0, func(w http.ResponseWriter, r *http.Request) {
			if n.Add(1) == 1 {
				http.Error(w, "busy", status)
				return
			}
			fmt.Fprint(w, `{"count":0,"release-groups":[]}`)
		})
		if _, err := fast(c).Search(context.Background(), "ed sheeran", 10, 0); err != nil || calls.Load() != 2 {
			t.Errorf("%d then OK: %v, %d calls", status, err, calls.Load())
		}
	}
}

func TestADroppedConnectionIsRetried(t *testing.T) {
	var n atomic.Int32
	c, calls := newTest(t, 0, func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close() // no answer at all
			return
		}
		fmt.Fprint(w, `{"count":0,"release-groups":[]}`)
	})
	if _, err := fast(c).Search(context.Background(), "x", 10, 0); err != nil || calls.Load() != 2 {
		t.Errorf("%v, %d calls", err, calls.Load())
	}
}

func TestOnlyOneRetryAndTheLastErrorIsReported(t *testing.T) {
	c, calls := newTest(t, 0, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "down", http.StatusServiceUnavailable) })
	_, err := fast(c).Search(context.Background(), "x", 10, 0)
	if err == nil || !strings.Contains(err.Error(), "503") || calls.Load() != 2 {
		t.Errorf("%v, %d calls: exactly two tries, then the failure", err, calls.Load())
	}
}

func TestWhatCannotPassIsNotRetried(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 405} {
		c, calls := newTest(t, 0, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", status) })
		if _, err := fast(c).Search(context.Background(), "x", 10, 0); err == nil || calls.Load() != 1 {
			t.Errorf("%d: %v, %d calls", status, err, calls.Load())
		}
	}
	c, calls := newTest(t, 0, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, strings.Repeat("x", maxBody+10)) })
	if _, err := fast(c).Search(context.Background(), "x", 10, 0); err == nil || calls.Load() != 1 {
		t.Errorf("an oversized answer: %v, %d calls", err, calls.Load())
	}
}

func TestTheRetryWaitsItsTurnInTheQueueAndForTheDelay(t *testing.T) {
	var times []time.Time
	var mu sync.Mutex
	c, _ := newTest(t, 80*time.Millisecond, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		times = append(times, time.Now())
		n := len(times)
		mu.Unlock()
		if n == 1 {
			http.Error(w, "busy", 503)
			return
		}
		fmt.Fprint(w, `{"count":0,"release-groups":[]}`)
	})
	c.retryDelay = 120 * time.Millisecond
	if _, err := c.Search(context.Background(), "x", 10, 0); err != nil {
		t.Fatal(err)
	}
	if len(times) != 2 || times[1].Sub(times[0]) < 110*time.Millisecond {
		t.Errorf("the retry came too soon: %v", times)
	}
}

func TestRetryAfterIsHonouredButCapped(t *testing.T) {
	for _, tc := range []struct {
		header string
		want   time.Duration
	}{{"", 2 * time.Second}, {"abc", 2 * time.Second}, {"-3", 2 * time.Second}, {"0", 0}, {"1", time.Second}, {" 3 ", 3 * time.Second}, {"3600", maxRetryAfter}} {
		if got := retryWait(tc.header, 2*time.Second); got != tc.want {
			t.Errorf("%q: %v, want %v", tc.header, got, tc.want)
		}
	}
}

func TestWalkingAwayDuringTheWaitEndsItAtOnceWithTheOriginalError(t *testing.T) {
	c, calls := newTest(t, 0, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "busy", 503) })
	c.retryDelay = 10 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.fetch(ctx, "/release-group?x")
	if err == nil || !strings.Contains(err.Error(), "503") || time.Since(start) > 2*time.Second || calls.Load() != 1 {
		t.Errorf("%v after %v, %d calls", err, time.Since(start), calls.Load())
	}
}

func TestAFailureAfterTheRetryIsNotCached(t *testing.T) {
	var n atomic.Int32
	c, calls := newTest(t, 0, func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) <= 2 {
			http.Error(w, "down", 503)
			return
		}
		fmt.Fprint(w, `{"count":0,"release-groups":[]}`)
	})
	fast(c)
	if _, err := c.Search(context.Background(), "again", 10, 0); err == nil {
		t.Fatal("two 503s must fail")
	}
	if _, err := c.Search(context.Background(), "again", 10, 0); err != nil || calls.Load() != 3 {
		t.Errorf("the next try goes upstream again: %v, %d calls", err, calls.Load())
	}
}
