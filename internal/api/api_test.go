package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/Miista/albumreq/internal/catalog"
	"github.com/Miista/albumreq/internal/coverart"
)

const id1 = "b1392450-e666-3926-a536-22c65f834433"

type fakeCatalog struct {
	res          catalog.SearchResult
	detail       catalog.Detail
	searchErr    error
	detailErr    error
	searchCalls  int
	detailCalls  int
	lastDetailID string
	lastOffset   int
}

func (f *fakeCatalog) Search(_ context.Context, _ string, offset int) (catalog.SearchResult, error) {
	f.searchCalls++
	f.lastOffset = offset
	return f.res, f.searchErr
}

func (f *fakeCatalog) Detail(_ context.Context, id string) (catalog.Detail, error) {
	f.detailCalls++
	f.lastDetailID = id
	return f.detail, f.detailErr
}

type fakeCovers struct {
	data     []byte
	ct       string
	err      error
	lastSize int
	lastID   string
}

func (f *fakeCovers) Front(_ context.Context, id string, size int) ([]byte, string, error) {
	f.lastID, f.lastSize = id, size
	return f.data, f.ct, f.err
}

func newMux(c Catalog, cv CoverFetcher) *http.ServeMux {
	mux := http.NewServeMux()
	New(c, cv, zerolog.Nop()).Routes(mux)
	return mux
}

func get(mux http.Handler, target string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}

func TestSearchReturnsAlbumsAndLibraryState(t *testing.T) {
	fc := &fakeCatalog{res: catalog.SearchResult{
		Albums:    []catalog.Album{{ID: "a", Title: "OK Computer", Status: catalog.StatusAvailable}},
		LibraryOK: true,
	}}
	rec := get(newMux(fc, &fakeCovers{}), "/api/search?q=ok+computer")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var body struct {
		Query     string          `json:"query"`
		Albums    []catalog.Album `json:"albums"`
		LibraryOK bool            `json:"libraryOk"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Query != "ok computer" || len(body.Albums) != 1 || body.Albums[0].Status != catalog.StatusAvailable || !body.LibraryOK {
		t.Errorf("unexpected body: %s", rec.Body)
	}
}

func TestSearchWithNoHitsEncodesAnEmptyList(t *testing.T) {
	rec := get(newMux(&fakeCatalog{res: catalog.SearchResult{Albums: []catalog.Album{}}}, &fakeCovers{}), "/api/search?q=nothing")
	if !strings.Contains(rec.Body.String(), `"albums":[]`) {
		t.Errorf("want an empty array, got %s", rec.Body)
	}
}

func TestSearchRejectsBadQueries(t *testing.T) {
	fc := &fakeCatalog{}
	mux := newMux(fc, &fakeCovers{})
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
	if fc.searchCalls != 0 {
		t.Errorf("rejected queries reached the catalog %d times", fc.searchCalls)
	}
	if rec := get(mux, "/api/search?q="+strings.Repeat("a", maxQueryLen)); rec.Code != http.StatusOK {
		t.Errorf("a %d-char query: status = %d, want 200", maxQueryLen, rec.Code)
	}
}

func TestSearchHidesUpstreamErrorDetail(t *testing.T) {
	fc := &fakeCatalog{searchErr: errors.New("dial tcp 192.0.2.10:8686: connection refused")}
	rec := get(newMux(fc, &fakeCovers{}), "/api/search?q=radiohead")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "192.0.2.10") {
		t.Errorf("internal address leaked to the client: %s", rec.Body)
	}
}

func TestAlbumDetail(t *testing.T) {
	fc := &fakeCatalog{detail: catalog.Detail{
		Album:      catalog.Album{ID: id1, Title: "OK Computer"},
		Genres:     []string{"Rock"},
		MetadataOK: true,
		Tracks:     catalog.TrackList{Source: catalog.SourceEdition, Tracks: []catalog.Track{{Disc: 1, Number: "1", Title: "Airbag"}}},
	}}
	rec := get(newMux(fc, &fakeCovers{}), "/api/album/"+id1)
	if rec.Code != http.StatusOK || fc.lastDetailID != id1 {
		t.Fatalf("status = %d, id = %q", rec.Code, fc.lastDetailID)
	}
	var d struct {
		Title      string `json:"title"`
		MetadataOK bool   `json:"metadataOk"`
		TrackList  struct {
			Source string `json:"source"`
			Tracks []struct {
				Title string `json:"title"`
			} `json:"tracks"`
		} `json:"trackList"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if d.Title != "OK Computer" || !d.MetadataOK || d.TrackList.Source != "edition" || len(d.TrackList.Tracks) != 1 || d.TrackList.Tracks[0].Title != "Airbag" {
		t.Errorf("unexpected body: %s", rec.Body)
	}
}

func TestAlbumDetailErrors(t *testing.T) {
	bad := get(newMux(&fakeCatalog{detailErr: catalog.ErrInvalidID}, &fakeCovers{}), "/api/album/not-a-uuid")
	if bad.Code != http.StatusBadRequest {
		t.Errorf("invalid id: status = %d, want 400", bad.Code)
	}
	down := get(newMux(&fakeCatalog{detailErr: errors.New("dial tcp 192.0.2.10:8686: refused")}, &fakeCovers{}), "/api/album/"+id1)
	if down.Code != http.StatusBadGateway || strings.Contains(down.Body.String(), "192.0.2.10") {
		t.Errorf("upstream failure: status = %d body = %q", down.Code, down.Body)
	}
}

func TestCover(t *testing.T) {
	fcv := &fakeCovers{data: []byte("img"), ct: "image/jpeg"}
	ok := get(newMux(&fakeCatalog{}, fcv), "/api/cover/"+id1)
	if ok.Code != http.StatusOK || ok.Body.String() != "img" || ok.Header().Get("Content-Type") != "image/jpeg" {
		t.Errorf("ok case: %d %q %q", ok.Code, ok.Body, ok.Header().Get("Content-Type"))
	}
	if !strings.Contains(ok.Header().Get("Cache-Control"), "max-age") || fcv.lastSize != 250 || fcv.lastID != id1 {
		t.Errorf("Cache-Control = %q, size = %d, id = %q", ok.Header().Get("Cache-Control"), fcv.lastSize, fcv.lastID)
	}

	get(newMux(&fakeCatalog{}, fcv), "/api/cover/"+id1+"?size=500")
	if fcv.lastSize != 500 {
		t.Errorf("size = %d, want 500", fcv.lastSize)
	}
}

func TestCoverRefusals(t *testing.T) {
	fcv := &fakeCovers{}
	for _, size := range []string{"1200", "0", "-250", "abc", "250px", "250%20"} {
		fcv.lastSize = -1
		if rec := get(newMux(&fakeCatalog{}, fcv), "/api/cover/"+id1+"?size="+size); rec.Code != http.StatusBadRequest {
			t.Errorf("size=%s: status = %d, want 400", size, rec.Code)
		}
		if fcv.lastSize != -1 {
			t.Errorf("size=%s reached the fetcher", size)
		}
	}
	if rec := get(newMux(&fakeCatalog{}, &fakeCovers{err: coverart.ErrInvalid}), "/api/cover/nope"); rec.Code != http.StatusBadRequest {
		t.Errorf("invalid id: status = %d, want 400", rec.Code)
	}
	none := get(newMux(&fakeCatalog{}, &fakeCovers{err: coverart.ErrNoCover}), "/api/cover/"+id1)
	if none.Code != http.StatusNotFound || !strings.Contains(none.Header().Get("Cache-Control"), "max-age") {
		t.Errorf("no cover: status = %d, Cache-Control = %q", none.Code, none.Header().Get("Cache-Control"))
	}
	down := get(newMux(&fakeCatalog{}, &fakeCovers{err: errors.New("connection refused")}), "/api/cover/"+id1)
	if down.Code != http.StatusBadGateway || strings.Contains(down.Body.String(), "refused") {
		t.Errorf("upstream failure: status = %d body = %q", down.Code, down.Body)
	}
}

func TestOnlyGetIsAllowed(t *testing.T) {
	mux := newMux(&fakeCatalog{}, &fakeCovers{})
	for _, path := range []string{"/api/search?q=radiohead", "/api/album/" + id1, "/api/cover/" + id1, "/healthz"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s: status = %d, want 405", path, rec.Code)
		}
	}
}

func TestHealthz(t *testing.T) {
	if rec := get(newMux(&fakeCatalog{}, &fakeCovers{}), "/healthz"); rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
}

func TestSearchPagingParameters(t *testing.T) {
	fc := &fakeCatalog{res: catalog.SearchResult{Albums: []catalog.Album{}, HasMore: true}}
	mux := newMux(fc, &fakeCovers{})

	rec := get(mux, "/api/search?q=radiohead&offset=48")
	if rec.Code != http.StatusOK || fc.lastOffset != 48 {
		t.Fatalf("status = %d, offset passed = %d", rec.Code, fc.lastOffset)
	}
	var body struct {
		Offset  int  `json:"offset"`
		HasMore bool `json:"hasMore"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Offset != 48 || !body.HasMore {
		t.Errorf("offset and hasMore should be echoed: %s", rec.Body)
	}

	get(mux, "/api/search?q=radiohead")
	if fc.lastOffset != 0 {
		t.Errorf("a missing offset must mean 0, got %d", fc.lastOffset)
	}
}

func TestSearchRejectsBadOffsets(t *testing.T) {
	fc := &fakeCatalog{}
	mux := newMux(fc, &fakeCovers{})
	for _, off := range []string{"-1", "abc", "1.5", "24px", "%20", "999999999999999999999", fmt.Sprint(catalog.MaxOffset + 1)} {
		if rec := get(mux, "/api/search?q=radiohead&offset="+off); rec.Code != http.StatusBadRequest {
			t.Errorf("offset=%s: status = %d, want 400", off, rec.Code)
		}
	}
	if fc.searchCalls != 0 {
		t.Errorf("rejected offsets reached the catalog %d times", fc.searchCalls)
	}
	if rec := get(mux, fmt.Sprintf("/api/search?q=radiohead&offset=%d", catalog.MaxOffset)); rec.Code != http.StatusOK {
		t.Errorf("the last allowed offset: status = %d, want 200", rec.Code)
	}
}
