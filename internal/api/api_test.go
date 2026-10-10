package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/Miista/albumreq/internal/catalog"
	"github.com/Miista/albumreq/internal/lidarr"
)

type fakeSearch struct {
	albums []catalog.Album
	err    error
	called int
}

func (f *fakeSearch) Search(context.Context, string) ([]catalog.Album, error) {
	f.called++
	return f.albums, f.err
}

type fakeCovers struct {
	data []byte
	ct   string
	err  error
}

func (f fakeCovers) Cover(context.Context, string) ([]byte, string, error) {
	return f.data, f.ct, f.err
}

func newMux(s Searcher, c CoverFetcher) *http.ServeMux {
	mux := http.NewServeMux()
	New(s, c, zerolog.Nop()).Routes(mux)
	return mux
}

func get(mux http.Handler, target string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}

func TestSearchReturnsAlbums(t *testing.T) {
	fs := &fakeSearch{albums: []catalog.Album{{ID: "a", Title: "OK Computer", Status: catalog.StatusAvailable}}}
	rec := get(newMux(fs, fakeCovers{}), "/api/search?q=ok+computer")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var body struct {
		Query  string          `json:"query"`
		Albums []catalog.Album `json:"albums"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Query != "ok computer" || len(body.Albums) != 1 || body.Albums[0].Status != catalog.StatusAvailable {
		t.Errorf("unexpected body: %s", rec.Body)
	}
}

func TestSearchWithNoHitsEncodesAnEmptyList(t *testing.T) {
	rec := get(newMux(&fakeSearch{albums: []catalog.Album{}}, fakeCovers{}), "/api/search?q=nothing")
	if !strings.Contains(rec.Body.String(), `"albums":[]`) {
		t.Errorf("want an empty array, got %s", rec.Body)
	}
}

func TestSearchRejectsBadQueries(t *testing.T) {
	fs := &fakeSearch{}
	mux := newMux(fs, fakeCovers{})
	for name, target := range map[string]string{
		"missing":       "/api/search",
		"empty":         "/api/search?q=",
		"whitespace":    "/api/search?q=%20%20%20",
		"one char":      "/api/search?q=a",
		"too long":      "/api/search?q=" + url.QueryEscape(strings.Repeat("a", maxQueryLen+1)),
		"just too long": "/api/search?q=" + strings.Repeat("%C3%A5", maxQueryLen+1),
	} {
		if rec := get(mux, target); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", name, rec.Code)
		}
	}
	if fs.called != 0 {
		t.Errorf("rejected queries reached Lidarr %d times", fs.called)
	}
	// The boundary itself is fine.
	if rec := get(mux, "/api/search?q="+strings.Repeat("a", maxQueryLen)); rec.Code != http.StatusOK {
		t.Errorf("a %d-char query: status = %d, want 200", maxQueryLen, rec.Code)
	}
}

func TestSearchHidesUpstreamErrorDetail(t *testing.T) {
	fs := &fakeSearch{err: errors.New("dial tcp 192.0.2.10:8686: connection refused")}
	rec := get(newMux(fs, fakeCovers{}), "/api/search?q=radiohead")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "192.0.2.10") {
		t.Errorf("internal address leaked to the client: %s", rec.Body)
	}
}

func TestCover(t *testing.T) {
	ok := get(newMux(&fakeSearch{}, fakeCovers{data: []byte("img"), ct: "image/jpeg"}), "/api/cover?p=/MediaCoverProxy/a/b.jpg")
	if ok.Code != http.StatusOK || ok.Body.String() != "img" || ok.Header().Get("Content-Type") != "image/jpeg" {
		t.Errorf("ok case: %d %q %q", ok.Code, ok.Body, ok.Header().Get("Content-Type"))
	}
	if !strings.Contains(ok.Header().Get("Cache-Control"), "max-age") {
		t.Errorf("Cache-Control = %q", ok.Header().Get("Cache-Control"))
	}

	bad := get(newMux(&fakeSearch{}, fakeCovers{err: lidarr.ErrInvalidCoverPath}), "/api/cover?p=/api/v1/album")
	if bad.Code != http.StatusBadRequest {
		t.Errorf("invalid path: status = %d, want 400", bad.Code)
	}

	down := get(newMux(&fakeSearch{}, fakeCovers{err: errors.New("connection refused")}), "/api/cover?p=/MediaCoverProxy/a")
	if down.Code != http.StatusBadGateway || strings.Contains(down.Body.String(), "refused") {
		t.Errorf("upstream failure: status = %d body = %q", down.Code, down.Body)
	}
}

func TestOnlyGetIsAllowed(t *testing.T) {
	mux := newMux(&fakeSearch{}, fakeCovers{})
	for _, path := range []string{"/api/search?q=radiohead", "/api/cover?p=/MediaCoverProxy/a", "/healthz"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s: status = %d, want 405", path, rec.Code)
		}
	}
}

func TestHealthz(t *testing.T) {
	if rec := get(newMux(&fakeSearch{}, fakeCovers{}), "/healthz"); rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
}
