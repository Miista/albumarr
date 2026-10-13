package coverart

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	imgjpeg "image/jpeg"
	"image/png"
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
	c := NewWithBase(srv.URL, "albumarr/0.1 ( test )", allow)
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
		if r.Header.Get("User-Agent") != "albumarr/0.1 ( test )" {
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

// --- artist photos ---

const artistID = "b8a7c51f-362c-4dcb-a259-bc6e0095f0a6"

// picture makes a w x h image, left half dark and right half light, encoded as
// PNG or JPEG.
func picture(t *testing.T, w, h int, asPNG bool) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := uint8(20)
			if x >= w/2 {
				v = 235
			}
			img.Set(x, y, color.RGBA{v, v, v, 255})
		}
	}
	var b bytes.Buffer
	if asPNG {
		png.Encode(&b, img)
	} else {
		imgjpeg.Encode(&b, img, nil)
	}
	return b.Bytes()
}

func serve(w http.ResponseWriter, ct string, body []byte) {
	w.Header().Set("Content-Type", ct)
	w.Write(body)
}

func decode(t *testing.T, data []byte) image.Image {
	t.Helper()
	img, format, err := image.Decode(bytes.NewReader(data))
	if err != nil || format != "jpeg" {
		t.Fatalf("not a JPEG: %v %s", err, format)
	}
	return img
}

func TestPhotoIsShrunkReencodedAsJPEGAndCached(t *testing.T) {
	for _, asPNG := range []bool{false, true} {
		src := picture(t, 1000, 600, asPNG)
		c, calls := newTest(t, allowAll, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/cache/ed.jpg" || r.Header.Get("User-Agent") != "albumarr/0.1 ( test )" {
				t.Errorf("request %s, UA %q", r.URL, r.Header.Get("User-Agent"))
			}
			serve(w, "image/jpeg", src)
		})
		var data []byte
		for i := 0; i < 3; i++ {
			var ct string
			var err error
			data, ct, err = c.Photo(context.Background(), artistID, c.base+"/cache/ed.jpg", photoWidth)
			if err != nil || ct != "image/jpeg" {
				t.Fatalf("png=%v: %v %q", asPNG, err, ct)
			}
		}
		if calls.Load() != 1 {
			t.Errorf("a repeated photo must come from the cache: %d upstream calls", calls.Load())
		}
		img := decode(t, data)
		if b := img.Bounds(); b.Dx() != photoWidth || b.Dy() != 288 { // 600 * 480 / 1000
			t.Errorf("size %dx%d, want %dx288: shrunk, proportions kept", b.Dx(), b.Dy(), photoWidth)
		}
		l, _, _, _ := img.At(20, 100).RGBA()
		r, _, _, _ := img.At(photoWidth-20, 100).RGBA()
		if l>>8 > 60 || r>>8 < 200 {
			t.Errorf("the picture's content survived: left %d, right %d", l>>8, r>>8)
		}
		if len(data) >= len(src) && !asPNG {
			t.Errorf("not smaller: %d >= %d", len(data), len(src))
		}
	}
}

func TestPhotoIsNotEnlarged(t *testing.T) {
	c, _ := newTest(t, allowAll, func(w http.ResponseWriter, r *http.Request) { serve(w, "image/jpeg", picture(t, 200, 100, false)) })
	data, _, err := c.Photo(context.Background(), artistID, c.base+"/p.jpg", photoWidth)
	if err != nil {
		t.Fatal(err)
	}
	if b := decode(t, data).Bounds(); b.Dx() != 200 || b.Dy() != 100 {
		t.Errorf("size %dx%d", b.Dx(), b.Dy())
	}
}

func TestPhotoOnlyFetchesFromTheImageHost(t *testing.T) {
	c, calls := newTest(t, allowAll, func(w http.ResponseWriter, r *http.Request) { serve(w, "image/jpeg", picture(t, 50, 50, false)) })
	host := strings.TrimPrefix(c.base, "http://")
	for name, raw := range map[string]string{
		"another host":   "http://evil.example/p.jpg",
		"another scheme": "https://" + host + "/p.jpg",
		"userinfo":       "http://user:pw@" + host + "/p.jpg",
		"a lookalike":    "http://" + host + ".evil.example/p.jpg",
		"not a URL":      "::not a url",
		"too long":       c.base + "/" + strings.Repeat("a", 1100),
		"empty":          "",
	} {
		if _, _, err := c.Photo(context.Background(), artistID, raw, photoWidth); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, _, err := c.Photo(context.Background(), "../x", c.base+"/p.jpg", photoWidth); !errors.Is(err, ErrInvalid) {
		t.Errorf("bad artist id: %v", err)
	}
	if calls.Load() != 0 {
		t.Errorf("a refused address must never be requested: %d calls", calls.Load())
	}
}

func TestPhotoRefusesWhatIsNotAUsableImage(t *testing.T) {
	huge := picture(t, 7000, 2, true) // tiny on the wire, enormous once decoded
	for name, body := range map[string][]byte{
		"text calling itself an image": []byte("<html>not a picture</html>"),
		"a truncated JPEG":             picture(t, 300, 300, false)[:300],
		"an image over the size limit": huge,
		"empty":                        {},
	} {
		c, _ := newTest(t, allowAll, func(w http.ResponseWriter, r *http.Request) { serve(w, "image/jpeg", body) })
		data, _, err := c.Photo(context.Background(), artistID, c.base+"/p.jpg", photoWidth)
		if err == nil || data != nil {
			t.Errorf("%s: got %d bytes, %v", name, len(data), err)
		}
	}
}

func TestPhotoMissesAreRememberedAndServerErrorsRetriedOnce(t *testing.T) {
	c, calls := newTest(t, allowAll, func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	for i := 0; i < 3; i++ {
		if _, _, err := c.Photo(context.Background(), artistID, c.base+"/p.jpg", photoWidth); !errors.Is(err, ErrNoCover) {
			t.Fatalf("404: %v", err)
		}
	}
	if calls.Load() != 1 {
		t.Errorf("a missing photo is remembered: %d calls", calls.Load())
	}
	var n atomic.Int32
	c, calls = newTest(t, allowAll, func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			http.Error(w, "busy", http.StatusBadGateway)
			return
		}
		serve(w, "image/jpeg", picture(t, 50, 50, false))
	})
	if _, _, err := c.Photo(context.Background(), artistID, c.base+"/p.jpg", photoWidth); err != nil || calls.Load() != 2 {
		t.Errorf("one 502 then success: %v, %d calls", err, calls.Load())
	}
	c, calls = newTest(t, allowAll, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "down", http.StatusServiceUnavailable) })
	if _, _, err := c.Photo(context.Background(), artistID, c.base+"/p.jpg", photoWidth); err == nil || errors.Is(err, ErrNoCover) || calls.Load() != 2 {
		t.Errorf("persistent 503: %v, %d calls", err, calls.Load())
	}
}

func TestShrinkAveragesBlocks(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 4, 2))
	for y := 0; y < 2; y++ {
		for x := 0; x < 4; x++ {
			v := uint8(0)
			if (x+y)%2 == 1 {
				v = 200
			}
			src.Set(x, y, color.RGBA{v, v, v, 255})
		}
	}
	out := shrink(src, 2) // each new pixel averages a 2x2 block: two dark, two light
	r, _, _, _ := out.At(0, 0).RGBA()
	if b := out.Bounds(); b.Dx() != 2 || b.Dy() != 1 || r>>8 != 100 {
		t.Errorf("%v, value %d, want 2x1 and 100", b, r>>8)
	}
	if got := shrink(src, 4); got != image.Image(src) {
		t.Error("an image that is not wider is returned as is")
	}
}

func TestPhotoThumbnailAndFullSizeCostOneDownload(t *testing.T) {
	c, calls := newTest(t, allowAll, func(w http.ResponseWriter, r *http.Request) { serve(w, "image/jpeg", picture(t, 1000, 1000, false)) })
	thumb, _, err := c.Photo(context.Background(), artistID, c.base+"/p.jpg", thumbWidth)
	if err != nil {
		t.Fatal(err)
	}
	full, _, err := c.Photo(context.Background(), artistID, c.base+"/p.jpg", photoWidth)
	if err != nil || calls.Load() != 1 {
		t.Fatalf("the second size must come from the same download: %v, %d calls", err, calls.Load())
	}
	if b := decode(t, thumb).Bounds(); b.Dx() != thumbWidth || b.Dy() != thumbWidth {
		t.Errorf("thumbnail %dx%d", b.Dx(), b.Dy())
	}
	if b := decode(t, full).Bounds(); b.Dx() != photoWidth {
		t.Errorf("full %dx%d", b.Dx(), b.Dy())
	}
	if len(thumb) >= len(full) {
		t.Errorf("a thumbnail should be much smaller: %d vs %d", len(thumb), len(full))
	}
}

func TestPhotoRefusesAnyOtherWidthWithoutARequest(t *testing.T) {
	c, calls := newTest(t, allowAll, func(w http.ResponseWriter, r *http.Request) { serve(w, "image/jpeg", picture(t, 50, 50, false)) })
	for _, w := range []int{0, -1, 1, 97, 479, 10000} {
		if _, _, err := c.Photo(context.Background(), artistID, c.base+"/p.jpg", w); !errors.Is(err, ErrInvalid) {
			t.Errorf("width %d: %v", w, err)
		}
	}
	if calls.Load() != 0 {
		t.Errorf("%d requests", calls.Load())
	}
}

func TestAMissingPhotoIsRememberedForBothSizes(t *testing.T) {
	c, calls := newTest(t, allowAll, func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	for _, w := range []int{thumbWidth, photoWidth, thumbWidth} {
		if _, _, err := c.Photo(context.Background(), artistID, c.base+"/p.jpg", w); !errors.Is(err, ErrNoCover) {
			t.Fatalf("width %d: %v", w, err)
		}
	}
	if calls.Load() != 1 {
		t.Errorf("one miss covers both sizes: %d calls", calls.Load())
	}
}
