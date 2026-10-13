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

	"github.com/Miista/albumarr/internal/auth"
	"github.com/Miista/albumarr/internal/catalog"
	"github.com/Miista/albumarr/internal/coverart"
	"github.com/Miista/albumarr/internal/lidarr"
	"github.com/Miista/albumarr/internal/requests"
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
	New(c, cv, &fakeSettings{}, adminAuth(), &fakeUsers{}, &fakeRequests{}, &fakeIssues{}, zerolog.Nop()).Routes(mux)
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
	// What a person reads is plain and says what to do, and names no service.
	if body := rec.Body.String(); !strings.Contains(body, "busy") || !strings.Contains(body, "Try again") || strings.Contains(strings.ToLower(body), "lidarr") || strings.Contains(strings.ToLower(body), "musicbrainz") {
		t.Errorf("friendly message expected: %q", body)
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

// --- requests ---

func post(mux http.Handler, target string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, target, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

var sameOrigin = map[string]string{"X-Albumarr": "1"}

// muxWith serves the API over a fake approval flow, as the given person.
func muxWith(rq *fakeRequests, a Authenticator) *http.ServeMux {
	mux := http.NewServeMux()
	New(&fakeCatalog{}, &fakeCovers{}, &fakeSettings{}, a, &fakeUsers{}, rq, &fakeIssues{}, zerolog.Nop()).Routes(mux)
	return mux
}

func TestRequestResultsMapToStatuses(t *testing.T) {
	for result, want := range map[string]int{
		"added":            http.StatusCreated,
		"added_unsearched": http.StatusCreated, // in Lidarr and monitored; only the search failed
		"exists":           http.StatusOK,
		"pending":          http.StatusAccepted,
	} {
		rq := &fakeRequests{out: requests.Outcome{Result: result}}
		rec := postBody(muxWith(rq, adminAuth()), "/api/album/"+id1+"/request", requestBody, sameOrigin)
		if rec.Code != want || !strings.Contains(rec.Body.String(), `"result":"`+result+`"`) || rq.submitted != id1 {
			t.Errorf("%s: %d %s id=%q", result, rec.Code, rec.Body, rq.submitted)
		}
	}
}

func TestRequestAnswerCarriesTheRecordWhenThereIsOne(t *testing.T) {
	rq := &fakeRequests{out: requests.Outcome{Result: "pending", Record: requests.Record{ID: 7, AlbumID: id1, Title: "OK Computer", State: requests.Pending}}}
	rec := postBody(muxWith(rq, as(auth.User)), "/api/album/"+id1+"/request", requestBody, sameOrigin)
	if !strings.Contains(rec.Body.String(), `"request":{"id":7`) {
		t.Errorf("%s", rec.Body)
	}
	rq = &fakeRequests{out: requests.Outcome{Result: "exists"}}
	if rec := postBody(muxWith(rq, as(auth.User)), "/api/album/"+id1+"/request", requestBody, sameOrigin); strings.Contains(rec.Body.String(), `"request"`) {
		t.Errorf("no record, so no request field: %s", rec.Body)
	}
}

func TestRequestIsRefusedWithoutProofItCameFromThisPage(t *testing.T) {
	for name, headers := range map[string]map[string]string{
		"no header":                 nil,
		"a wrong header value":      {"X-Albumarr": "0"},
		"an empty header value":     {"X-Albumarr": ""},
		"a form-style cross-site":   {"Content-Type": "application/x-www-form-urlencoded"},
		"the header but cross-site": {"X-Albumarr": "1", "Sec-Fetch-Site": "cross-site"},
		"the header but same-site":  {"X-Albumarr": "1", "Sec-Fetch-Site": "same-site"},
		"the header, an odd site":   {"X-Albumarr": "1", "Sec-Fetch-Site": "whatever"},
	} {
		rq := &fakeRequests{out: requests.Outcome{Result: "added"}}
		rec := postBody(muxWith(rq, adminAuth()), "/api/album/"+id1+"/request", requestBody, headers)
		if rec.Code != http.StatusForbidden || rq.calls != 0 {
			t.Errorf("%s: status = %d, calls = %d, want 403 and no call", name, rec.Code, rq.calls)
		}
	}
	// ...and what a real same-origin fetch looks like is accepted.
	for _, site := range []string{"same-origin", "none", ""} {
		rq := &fakeRequests{out: requests.Outcome{Result: "added"}}
		rec := postBody(muxWith(rq, adminAuth()), "/api/album/"+id1+"/request", requestBody, map[string]string{"X-Albumarr": "1", "Sec-Fetch-Site": site})
		if rec.Code != http.StatusCreated {
			t.Errorf("Sec-Fetch-Site=%q: status = %d, want 201", site, rec.Code)
		}
	}
}

func TestRequestErrorMapping(t *testing.T) {
	rejected := &lidarr.RejectedError{Messages: []string{"Root folder '/secret/place' does not exist"}}
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"invalid id", catalog.ErrInvalidID, http.StatusBadRequest},
		{"requests off", catalog.ErrRequestsDisabled, http.StatusForbidden},
		{"not allowed", requests.ErrForbidden, http.StatusForbidden},
		{"not in metadata", catalog.ErrNotInMetadata, http.StatusUnprocessableEntity},
		{"unavailable", fmt.Errorf("%w: dial tcp 192.0.2.10:8686: refused", catalog.ErrUnavailable), http.StatusServiceUnavailable},
		{"rejected by Lidarr", rejected, http.StatusBadGateway},
		{"Lidarr lacks the None profile", fmt.Errorf("%w: Lidarr has no metadata profile named %q", lidarr.ErrBadSettings, "None"), http.StatusBadGateway},
		{"something else", errors.New("lidarr /api/v1/album returned 401 Unauthorized"), http.StatusBadGateway},
		{"a database error", errors.New("database is locked: /app/data/albumarr.db"), http.StatusBadGateway},
	}
	for _, c := range cases {
		rec := postBody(muxWith(&fakeRequests{err: c.err}, adminAuth()), "/api/album/"+id1+"/request", requestBody, sameOrigin)
		if rec.Code != c.want {
			t.Errorf("%s: status = %d, want %d", c.name, rec.Code, c.want)
		}
		for _, leaked := range []string{"/secret/place", "192.0.2.10", "401 Unauthorized", "dial tcp", "albumarr.db"} {
			if strings.Contains(rec.Body.String(), leaked) {
				t.Errorf("%s: %q leaked to the client: %s", c.name, leaked, rec.Body)
			}
		}
	}
}

func TestRequestOnlyAcceptsPost(t *testing.T) {
	rq := &fakeRequests{out: requests.Outcome{Result: "added"}}
	mux := muxWith(rq, adminAuth())
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		req := httptest.NewRequest(method, "/api/album/"+id1+"/request", nil)
		req.Header.Set("X-Albumarr", "1")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: status = %d, want 405", method, rec.Code)
		}
	}
	if rq.calls != 0 {
		t.Error("a non-POST reached the flow")
	}
}

func TestAlbumDetailCarriesCanRequest(t *testing.T) {
	fc := &fakeCatalog{detail: catalog.Detail{Album: catalog.Album{ID: id1}, CanRequest: true}}
	rec := get(newMux(fc, &fakeCovers{}), "/api/album/"+id1)
	if !strings.Contains(rec.Body.String(), `"canRequest":true`) {
		t.Errorf("canRequest missing from %s", rec.Body)
	}
}
