package coverart

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	id1 = "b1392450-e666-3926-a536-22c65f834433"
	id2 = "59f47b29-50dd-4335-a81b-02297c790208"
)

func allowAll(string) bool { return true }

func newTest(t *testing.T, allow func(string) bool, h http.HandlerFunc) (*Client, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	c := NewWithBase(srv.URL, "albumreq/0.1 ( test )", allow)
	c.retry = time.Millisecond
	return c, &calls
}

func jpeg(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "image/jpeg")
	w.Write([]byte(body))
}

func TestFrontFetchesAndSendsTheUserAgent(t *testing.T) {
	c, _ := newTest(t, allowAll, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/release-group/"+id1+"/front-250" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.Header.Get("User-Agent") != "albumreq/0.1 ( test )" {
			t.Errorf("User-Agent = %q", r.Header.Get("User-Agent"))
		}
		jpeg(w, "pixels")
	})
	data, ct, err := c.Front(context.Background(), id1, 250)
	if err != nil || string(data) != "pixels" || ct != "image/jpeg" {
		t.Fatalf("got %q, %q, %v", data, ct, err)
	}
}

func TestFrontRejectsBadInputWithoutAnyRequest(t *testing.T) {
	c, calls := newTest(t, allowAll, func(w http.ResponseWriter, r *http.Request) { jpeg(w, "x") })
	for name, tc := range map[string]struct {
		id   string
		size int
	}{
		"traversal":    {"../../x", 250},
		"uppercase":    {strings.ToUpper(id1), 250},
		"query":        {id1 + "?x=1", 250},
		"empty":        {"", 250},
		"huge size":    {id1, 1200},
		"zero size":    {id1, 0},
		"negative":     {id1, -250},
		"odd size":     {id1, 251},
		"size as text": {id1 + "/front-1200", 250},
	} {
		if _, _, err := c.Front(context.Background(), tc.id, tc.size); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
	if calls.Load() != 0 {
		t.Errorf("%d bad requests reached the archive", calls.Load())
	}
	if _, _, err := c.Front(context.Background(), id1, 500); err != nil {
		t.Errorf("a valid 500px request failed: %v", err)
	}
}

func TestHitsAreCachedPerSize(t *testing.T) {
	c, calls := newTest(t, allowAll, func(w http.ResponseWriter, r *http.Request) { jpeg(w, "pixels") })
	for i := 0; i < 3; i++ {
		if _, _, err := c.Front(context.Background(), id1, 250); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := c.Front(context.Background(), id1, 500); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Errorf("upstream calls = %d, want 2", calls.Load())
	}
}

func TestMissingCoverIsRememberedAndNotRetried(t *testing.T) {
	c, calls := newTest(t, allowAll, func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	for i := 0; i < 3; i++ {
		if _, _, err := c.Front(context.Background(), id2, 250); !errors.Is(err, ErrNoCover) {
			t.Fatalf("err = %v, want ErrNoCover", err)
		}
	}
	if calls.Load() != 1 {
		t.Errorf("upstream calls = %d, want 1 (a 404 is final and cached)", calls.Load())
	}
}

func TestFlakyArchiveIsRetriedOnce(t *testing.T) {
	var n atomic.Int32
	c, calls := newTest(t, allowAll, func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			http.Error(w, "storage node down", http.StatusInternalServerError)
			return
		}
		jpeg(w, "recovered")
	})
	data, _, err := c.Front(context.Background(), id1, 250)
	if err != nil || string(data) != "recovered" || calls.Load() != 2 {
		t.Errorf("got %q, %v after %d calls", data, err, calls.Load())
	}
}

func TestPersistentFailureIsNotCachedAndNotRetriedForever(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	c, calls := newTest(t, allowAll, func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		jpeg(w, "back")
	})
	if _, _, err := c.Front(context.Background(), id1, 250); err == nil {
		t.Fatal("expected an error")
	}
	if calls.Load() != 2 {
		t.Errorf("calls = %d, want exactly 2 (one retry)", calls.Load())
	}
	fail.Store(false)
	if data, _, err := c.Front(context.Background(), id1, 250); err != nil || string(data) != "back" {
		t.Errorf("after recovery: %q, %v", data, err)
	}
}

func TestBadBodiesAreRefusedAndNotRetried(t *testing.T) {
	var mode atomic.Value
	mode.Store("html")
	c, calls := newTest(t, allowAll, func(w http.ResponseWriter, r *http.Request) {
		switch mode.Load() {
		case "html":
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte("<html>"))
		case "huge":
			w.Header().Set("Content-Type", "image/png")
			w.Write(make([]byte, maxImage+10))
		}
	})
	if _, _, err := c.Front(context.Background(), id1, 250); err == nil {
		t.Error("a non-image body was accepted")
	}
	mode.Store("huge")
	if _, _, err := c.Front(context.Background(), id2, 250); err == nil {
		t.Error("an oversized image was accepted")
	}
	if calls.Load() != 2 {
		t.Errorf("calls = %d, want 2 (neither failure is retried)", calls.Load())
	}
}

func TestRedirectsStayOnAllowedHosts(t *testing.T) {
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { jpeg(w, "from storage") }))
	defer storage.Close()
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { jpeg(w, "from evil") }))
	defer evil.Close()

	var target atomic.Value
	target.Store(storage.URL)
	archive := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.Load().(string)+"/blob.jpg", http.StatusTemporaryRedirect)
	}))
	defer archive.Close()

	storageHost := strings.TrimPrefix(storage.URL, "http://")
	c := NewWithBase(archive.URL, "ua", func(host string) bool { return host == storageHost })
	c.retry = time.Millisecond

	data, _, err := c.Front(context.Background(), id1, 250)
	if err != nil || string(data) != "from storage" {
		t.Fatalf("allowed redirect: %q, %v", data, err)
	}

	target.Store(evil.URL)
	if data, _, err := c.Front(context.Background(), id2, 250); err == nil || strings.Contains(string(data), "evil") {
		t.Errorf("a redirect to a foreign host was followed: %q, %v", data, err)
	}
}

func TestDefaultHostPolicy(t *testing.T) {
	for _, h := range []string{"images.lidarr.audio", "coverartarchive.org", "archive.org", "dn710005.ca.archive.org", "ia800.us.archive.org"} {
		if !hostAllowed(h) {
			t.Errorf("%s should be allowed", h)
		}
	}
	for _, h := range []string{"evil.example", "archive.org.evil.example", "notarchive.org", "evilarchive.org", "images.lidarr.audio.evil.example", "evilimages.lidarr.audio", "169.254.169.254", "localhost:8686", ""} {
		if hostAllowed(h) {
			t.Errorf("%s must be refused", h)
		}
	}
}

func TestConcurrencyIsBounded(t *testing.T) {
	var cur, peak atomic.Int32
	c, _ := newTest(t, allowAll, func(w http.ResponseWriter, r *http.Request) {
		n := cur.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(40 * time.Millisecond)
		cur.Add(-1)
		jpeg(w, "x")
	})
	ids := []string{
		"00000000-0000-0000-0000-000000000001", "00000000-0000-0000-0000-000000000002", "00000000-0000-0000-0000-000000000003",
		"00000000-0000-0000-0000-000000000004", "00000000-0000-0000-0000-000000000005", "00000000-0000-0000-0000-000000000006",
		"00000000-0000-0000-0000-000000000007", "00000000-0000-0000-0000-000000000008", "00000000-0000-0000-0000-000000000009",
	}
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func(id string) { defer wg.Done(); c.Front(context.Background(), id, 250) }(id)
	}
	wg.Wait()
	if peak.Load() > maxInFlight {
		t.Errorf("peak concurrency %d, want <= %d", peak.Load(), maxInFlight)
	}
}

func TestCacheStaysWithinItsByteBudget(t *testing.T) {
	c, _ := newTest(t, allowAll, func(w http.ResponseWriter, r *http.Request) {})
	chunk := make([]byte, 2<<20)
	for i := 0; i < 40; i++ { // 80 MiB offered, budget is 48 MiB
		c.store(strings.Repeat("a", i+1), entry{data: chunk, ct: "image/jpeg", exp: time.Now().Add(time.Hour)})
	}
	if c.bytes > maxCacheB {
		t.Errorf("cache holds %d bytes, budget %d", c.bytes, maxCacheB)
	}
}
