package api

import (
	"encoding/json"
	"errors"
	"github.com/Miista/albumarr/internal/coverart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/Miista/albumarr/internal/auth"
	"github.com/Miista/albumarr/internal/catalog"
	"github.com/Miista/albumarr/internal/requests"
)

const (
	id2 = "48140466-cff6-3222-bd55-63c27e43190d"
	id3 = "46b38f09-e90b-39eb-b04f-002da20f7f96"
)

func TestApproveAndDeclineGoToTheFlowAsTheSignedInPerson(t *testing.T) {
	rq := &fakeRequests{out: requests.Outcome{Result: "added", Record: requests.Record{ID: 7, State: requests.Approved}}}
	rg := newRig(as(auth.Approver))
	rg.requests = rq
	rg.mux = muxWith(rq, as(auth.Approver))

	rec := post(rg.mux, "/api/requests/7/approve", sameOrigin)
	if rec.Code != 200 || rq.approved != 7 || rq.by.Name != "approver" || !strings.Contains(rec.Body.String(), `"request":{"id":7`) {
		t.Errorf("approve: %d %s approved=%d by=%q", rec.Code, rec.Body, rq.approved, rq.by.Name)
	}

	rec = postBody(rg.mux, "/api/requests/9/decline", `{"reason":"Already have it"}`, sameOrigin)
	if rec.Code != 200 || rq.declined != 9 || rq.reason != "Already have it" {
		t.Errorf("decline: %d %s declined=%d reason=%q", rec.Code, rec.Body, rq.declined, rq.reason)
	}
	// A decline with no reason is fine: it is optional.
	rec = postBody(rg.mux, "/api/requests/10/decline", `{}`, sameOrigin)
	if rec.Code != 200 || rq.declined != 10 || rq.reason != "" {
		t.Errorf("decline without a reason: %d", rec.Code)
	}
}

func TestDecisionsNeedTheCSRFHeaderToo(t *testing.T) {
	for _, tc := range []struct{ path, body string }{
		{"/api/requests/7/approve", ""},
		{"/api/requests/7/decline", `{"reason":"x"}`},
	} {
		rq := &fakeRequests{}
		for name, hdr := range map[string]map[string]string{
			"no header":  nil,
			"cross-site": {"X-Albumarr": "1", "Sec-Fetch-Site": "cross-site"},
		} {
			if rec := postBody(muxWith(rq, adminAuth()), tc.path, tc.body, hdr); rec.Code != 403 || rq.calls != 0 {
				t.Errorf("%s %s: %d calls=%d: an approver's browser tricked by another site must not decide", tc.path, name, rec.Code, rq.calls)
			}
		}
	}
}

func TestRequestIDsMustBePositiveWholeNumbers(t *testing.T) {
	for _, id := range []string{"0", "-1", "abc", "1.5", "1e3", "99999999999999999999", "%20", "1%2F2"} {
		rq := &fakeRequests{}
		for _, action := range []string{"approve", "decline"} {
			rec := postBody(muxWith(rq, adminAuth()), "/api/requests/"+id+"/"+action, `{}`, sameOrigin)
			if rec.Code != http.StatusBadRequest && rec.Code != http.StatusNotFound || rq.calls != 0 {
				t.Errorf("%s %s: %d calls=%d", action, id, rec.Code, rq.calls)
			}
		}
	}
}

func TestDeclineBodyIsValidated(t *testing.T) {
	for name, body := range map[string]string{
		"not json":      `nope`,
		"unknown field": `{"reason":"x","state":"approved"}`,
		"wrong type":    `{"reason":5}`,
		"empty":         ``,
		"trailing":      `{} {}`,
		"too large":     `{"reason":"` + strings.Repeat("a", 20000) + `"}`,
	} {
		rq := &fakeRequests{}
		if rec := postBody(muxWith(rq, adminAuth()), "/api/requests/7/decline", body, sameOrigin); rec.Code != 400 || rq.calls != 0 {
			t.Errorf("%s: %d calls=%d", name, rec.Code, rq.calls)
		}
	}
}

func TestDecisionErrorsMapToTheRightStatus(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want int
	}{
		"already decided":     {requests.ErrNotPending, http.StatusConflict},
		"no such request":     {requests.ErrNotFound, http.StatusNotFound},
		"not allowed":         {requests.ErrForbidden, http.StatusForbidden},
		"Lidarr is down":      {catalog.ErrUnavailable, http.StatusServiceUnavailable},
		"requests turned off": {catalog.ErrRequestsDisabled, http.StatusForbidden},
	} {
		for _, path := range []string{"/api/requests/7/approve", "/api/requests/7/decline"} {
			rec := postBody(muxWith(&fakeRequests{err: tc.err}, adminAuth()), path, `{}`, sameOrigin)
			if rec.Code != tc.want {
				t.Errorf("%s %s: %d, want %d", name, path, rec.Code, tc.want)
			}
		}
	}
}

func TestListRequestsPassesTheScopeAndThePersonAndRefusesBadScopes(t *testing.T) {
	rq := &fakeRequests{list: []requests.Record{{ID: 3, Title: "OK Computer"}}}
	mux := muxWith(rq, as(auth.User))
	for _, c := range []struct {
		query string
		want  requests.Scope
	}{{"", requests.ScopeMine}, {"?scope=mine", requests.ScopeMine}, {"?scope=pending", requests.ScopePending}, {"?scope=all", requests.ScopeAll}} {
		rec := get(mux, "/api/requests"+c.query)
		if rec.Code != 200 || rq.listScope != c.want || rq.by.Role != auth.User || !strings.Contains(rec.Body.String(), `"title":"OK Computer"`) {
			t.Errorf("%q: %d scope=%q by=%v", c.query, rec.Code, rq.listScope, rq.by.Role)
		}
		if rec.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%q: a list of who asked for what must not be cached", c.query)
		}
	}
	before := rq.listCalls
	for _, q := range []string{"?scope=everyone", "?scope=ALL", "?scope=mine,all", "?scope=%00"} {
		if rec := get(mux, "/api/requests"+q); rec.Code != 400 {
			t.Errorf("%q: %d", q, rec.Code)
		}
	}
	if rq.listCalls != before {
		t.Error("a bad scope reached the flow")
	}
}

func TestAScopeTheRoleMayNotReadIs403(t *testing.T) {
	rq := &fakeRequests{listErr: requests.ErrForbidden, list: []requests.Record{{ID: 1, Title: "secret"}}}
	rec := get(muxWith(rq, as(auth.User)), "/api/requests?scope=all")
	if rec.Code != 403 || strings.Contains(rec.Body.String(), "secret") {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
}

// --- what search and the album sheet show for a waiting request ---

func searchRig(albums []catalog.Album, pending map[string]requests.Record, role auth.Role) (*http.ServeMux, *fakeRequests) {
	rq := &fakeRequests{pending: pending}
	mux := http.NewServeMux()
	New(&fakeCatalog{res: catalog.SearchResult{Albums: albums, LibraryOK: true}}, &fakeCovers{}, &fakeSettings{}, as(role), &fakeUsers{}, rq, &fakeIssues{}, zerolog.Nop()).Routes(mux)
	return mux, rq
}

func TestSearchMarksAlbumsWithAWaitingRequestAndLeavesLidarrsChipsAlone(t *testing.T) {
	albums := []catalog.Album{
		{ID: id1, Title: "waiting"},
		{ID: id2, Title: "no request"},
		{ID: id3, Title: "in Lidarr", Status: catalog.StatusAvailable},
	}
	pending := map[string]requests.Record{id1: {ID: 1}, id3: {ID: 2}}
	mux, _ := searchRig(albums, pending, auth.User)
	rec := get(mux, "/api/search?q=anything")
	var got struct {
		Albums []struct{ Title, Status string }
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != 200 {
		t.Fatalf("%d %v %s", rec.Code, err, rec.Body)
	}
	want := map[string]string{
		"waiting":    "pending",   // a waiting request and nothing in Lidarr
		"no request": "",          // nothing at all
		"in Lidarr":  "available", // Lidarr's own state wins over a waiting request
	}
	for _, a := range got.Albums {
		if a.Status != want[a.Title] {
			t.Errorf("%q: status %q, want %q", a.Title, a.Status, want[a.Title])
		}
	}
	if len(got.Albums) != 3 {
		t.Errorf("%d albums", len(got.Albums))
	}
}

func TestAFailedPendingLookupOnlyCostsTheChip(t *testing.T) {
	mux, rq := searchRig([]catalog.Album{{ID: id1, Title: "x"}}, nil, auth.User)
	rq.pendingErr = errors.New("database is locked")
	rec := get(mux, "/api/search?q=anything")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"title":"x"`) || strings.Contains(rec.Body.String(), "locked") {
		t.Errorf("search must still work: %d %s", rec.Code, rec.Body)
	}
}

func detailRig(d catalog.Detail, pending map[string]requests.Record, role auth.Role) *http.ServeMux {
	rq := &fakeRequests{pending: pending}
	mux := http.NewServeMux()
	New(&fakeCatalog{detail: d}, &fakeCovers{}, &fakeSettings{}, as(role), &fakeUsers{}, rq, &fakeIssues{}, zerolog.Nop()).Routes(mux)
	return mux
}

func TestTheSheetOfAnAlbumWithAWaitingRequestShowsItPendingAndNotRequestable(t *testing.T) {
	d := catalog.Detail{Album: catalog.Album{ID: id1}, CanRequest: true}
	rec := get(detailRig(d, map[string]requests.Record{id1: {ID: 5, Requester: requests.Person{Name: "alice"}}}, auth.User), "/api/album/"+id1)
	body := rec.Body.String()
	if !strings.Contains(body, `"status":"pending"`) || !strings.Contains(body, `"canRequest":false`) {
		t.Errorf("%s", body)
	}
	if strings.Contains(body, "pendingRequest") || strings.Contains(body, "alice") {
		t.Errorf("a plain user must not learn who asked: %s", body)
	}
}

func TestApproversSeeTheWaitingRequestOnTheSheet(t *testing.T) {
	d := catalog.Detail{Album: catalog.Album{ID: id1}, CanRequest: true}
	pending := map[string]requests.Record{id1: {ID: 5, Requester: requests.Person{Name: "alice"}}}
	for _, role := range []auth.Role{auth.Approver, auth.Admin} {
		body := get(detailRig(d, pending, role), "/api/album/"+id1).Body.String()
		if !strings.Contains(body, `"pendingRequest":{"id":5`) || !strings.Contains(body, "alice") {
			t.Errorf("%v: %s", role, body)
		}
	}
}

func TestTheSheetLeavesAnAlbumLidarrKnowsAlone(t *testing.T) {
	d := catalog.Detail{Album: catalog.Album{ID: id1, Status: catalog.StatusRequested}}
	rec := get(detailRig(d, map[string]requests.Record{id1: {ID: 5}}, auth.Admin), "/api/album/"+id1)
	if !strings.Contains(rec.Body.String(), `"status":"requested"`) || strings.Contains(rec.Body.String(), "pendingRequest") {
		t.Errorf("%s", rec.Body)
	}
}

func TestTheSheetWorksWhenTheRecordsCannotBeRead(t *testing.T) {
	rq := &fakeRequests{pendingErr: errors.New("boom")}
	mux := http.NewServeMux()
	New(&fakeCatalog{detail: catalog.Detail{Album: catalog.Album{ID: id1}, CanRequest: true}}, &fakeCovers{}, &fakeSettings{}, adminAuth(), &fakeUsers{}, rq, &fakeIssues{}, zerolog.Nop()).Routes(mux)
	rec := get(mux, "/api/album/"+id1)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"canRequest":true`) {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
}

// --- what a request carries ---

func TestTheRequestPassesTheCardsValuesToTheFlow(t *testing.T) {
	rq := &fakeRequests{out: requests.Outcome{Result: "pending"}}
	rec := postBody(muxWith(rq, as(auth.User)), "/api/album/"+id1+"/request",
		`{"artistId":"5b11f4ce-a62d-471e-81fc-a69a8278c7da","title":"Nevermind","artist":"Nirvana"}`, sameOrigin)
	want := requests.Ask{AlbumID: id1, ArtistID: "5b11f4ce-a62d-471e-81fc-a69a8278c7da", Title: "Nevermind", Artist: "Nirvana"}
	if rec.Code != http.StatusAccepted || rq.ask != want {
		t.Errorf("%d: the flow got %+v, want %+v", rec.Code, rq.ask, want)
	}
	if rq.by.Role != auth.User {
		t.Errorf("the flow must be told who asked: %+v", rq.by)
	}
}

func TestTheRequestBodyIsValidatedForShape(t *testing.T) {
	for name, body := range map[string]string{
		"no body":       ``,
		"not json":      `nope`,
		"unknown field": `{"artistId":"a","title":"t","artist":"a","approve":true}`,
		"wrong type":    `{"artistId":5,"title":"t","artist":"a"}`,
		"trailing data": `{} {}`,
		"far too large": `{"title":"` + strings.Repeat("a", 20000) + `"}`,
	} {
		rq := &fakeRequests{out: requests.Outcome{Result: "pending"}}
		rec := postBody(muxWith(rq, adminAuth()), "/api/album/"+id1+"/request", body, sameOrigin)
		if rec.Code != http.StatusBadRequest || rq.calls != 0 {
			t.Errorf("%s: %d calls=%d, want 400 and nothing run", name, rec.Code, rq.calls)
		}
	}
}

func TestMissingOrBadValuesAreRefusedByTheFlowAndShownAs400(t *testing.T) {
	for name, err := range map[string]error{
		"no title or artist": requests.ErrBadInput,
		"a bad id":           catalog.ErrInvalidID,
	} {
		rec := postBody(muxWith(&fakeRequests{err: err}, adminAuth()), "/api/album/"+id1+"/request", `{"artistId":"","title":"","artist":""}`, sameOrigin)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", name, rec.Code)
		}
	}
}

func TestAnAlbumLidarrDoesNotKnowIsA422(t *testing.T) {
	rec := postBody(muxWith(&fakeRequests{err: catalog.ErrNotInMetadata}, adminAuth()), "/api/album/"+id1+"/request", requestBody, sameOrigin)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "cannot be requested yet") {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
}

func earlierRig(d catalog.Detail, rq *fakeRequests, who auth.Role) http.Handler {
	mux := http.NewServeMux()
	New(&fakeCatalog{detail: d}, &fakeCovers{}, &fakeSettings{}, as(who), &fakeUsers{}, rq, &fakeIssues{}, zerolog.Nop()).Routes(mux)
	return mux
}

func TestTheSheetSaysAnAlbumWasApprovedBeforeWhenLidarrNoLongerWantsIt(t *testing.T) {
	at := time.Date(2026, 10, 12, 20, 59, 0, 0, time.UTC)
	d := catalog.Detail{Album: catalog.Album{ID: id1, Status: catalog.StatusInLidarr}, CanRequest: true}
	earlier := requests.Record{ID: 9, State: requests.Approved, Requester: requests.Person{Sub: "sub-user", Name: "Cindy"}, CreatedAt: at.Add(-time.Hour), DecidedAt: &at}
	body := get(earlierRig(d, &fakeRequests{lastApproved: earlier}, auth.User), "/api/album/"+id1).Body.String()
	if !strings.Contains(body, `"earlierApproval":{"at":"2026-10-12T20:59:00Z","byYou":true}`) {
		t.Errorf("the requester sees their own approval: %s", body)
	}
	body = get(earlierRig(d, &fakeRequests{lastApproved: earlier}, auth.Approver), "/api/album/"+id1).Body.String()
	if !strings.Contains(body, `"byYou":false`) || strings.Contains(body, "Cindy") {
		t.Errorf("someone else's approval says so and names nobody: %s", body)
	}
}

func TestTheSheetSaysNothingAboutEarlierApprovalsWhenThereIsNoneOrNoRequestButton(t *testing.T) {
	at := time.Date(2026, 10, 12, 20, 59, 0, 0, time.UTC)
	earlier := requests.Record{ID: 9, State: requests.Approved, DecidedAt: &at}
	d := catalog.Detail{Album: catalog.Album{ID: id1}, CanRequest: true}
	if body := get(earlierRig(d, &fakeRequests{}, auth.User), "/api/album/"+id1).Body.String(); strings.Contains(body, "earlierApproval") {
		t.Errorf("no earlier approval: %s", body)
	}
	d.CanRequest = false // Lidarr wants it: the button is not there, so no note either
	if body := get(earlierRig(d, &fakeRequests{lastApproved: earlier}, auth.User), "/api/album/"+id1).Body.String(); strings.Contains(body, "earlierApproval") {
		t.Errorf("no request button, no note: %s", body)
	}
	d.CanRequest = true
	if rec := get(earlierRig(d, &fakeRequests{lastApprovedErr: errors.New("boom")}, auth.User), "/api/album/"+id1); rec.Code != 200 || strings.Contains(rec.Body.String(), "earlierApproval") {
		t.Errorf("unreadable records must not break the sheet: %d %s", rec.Code, rec.Body)
	}
}

func TestDeletingARequestNeedsTheHeaderAndMapsTheServicesAnswers(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		want int
	}{
		{"deleted", nil, http.StatusNoContent},
		{"not allowed", requests.ErrForbidden, http.StatusForbidden},
		{"no such request", requests.ErrNotFound, http.StatusNotFound},
	} {
		rq := &fakeRequests{err: c.err}
		mux := muxWith(rq, as(auth.User))
		req := httptest.NewRequest("DELETE", "/api/requests/7", nil)
		req.Header.Set("X-Albumarr", "1")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != c.want || rq.deleted != 7 {
			t.Errorf("%s: %d (deleted %d), want %d", c.name, rec.Code, rq.deleted, c.want)
		}
	}
	rq := &fakeRequests{}
	rec := httptest.NewRecorder()
	muxWith(rq, as(auth.Admin)).ServeHTTP(rec, httptest.NewRequest("DELETE", "/api/requests/7", nil)) // no X-Albumarr
	if rec.Code != http.StatusForbidden || rq.deleted != 0 {
		t.Errorf("without the header: %d, deleted %d", rec.Code, rq.deleted)
	}
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("DELETE", "/api/requests/abc", nil)
	req.Header.Set("X-Albumarr", "1")
	muxWith(rq, as(auth.Admin)).ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("a bad id: %d", rec.Code)
	}
}

const artistPath = "/api/artist/b8a7c51f-362c-4dcb-a259-bc6e0095f0a6"

func artistRig(rq *fakeRequests, fa *fakeArtists, who auth.Role) *http.ServeMux {
	mux := http.NewServeMux()
	New(&fakeCatalog{}, &fakeCovers{}, &fakeSettings{}, as(who), &fakeUsers{}, rq, &fakeIssues{}, zerolog.Nop()).Artists(fa).Routes(mux)
	return mux
}

func TestArtistSearchReturnsTheHitsAndValidatesTheQuery(t *testing.T) {
	fa := &fakeArtists{hits: []catalog.ArtistHit{{ID: "b8a7c51f-362c-4dcb-a259-bc6e0095f0a6", Name: "Ed Sheeran", Disambiguation: "UK singer"}}}
	mux := artistRig(&fakeRequests{}, fa, auth.User)
	rec := get(mux, "/api/artists?q=+ed+sheeran+")
	if rec.Code != 200 || fa.lastQ != "ed sheeran" || !strings.Contains(rec.Body.String(), `"name":"Ed Sheeran"`) {
		t.Errorf("%d %q %s", rec.Code, fa.lastQ, rec.Body)
	}
	for _, q := range []string{"", "a", strings.Repeat("x", 101)} {
		fa.calls = 0
		if rec := get(mux, "/api/artists?q="+q); rec.Code != http.StatusBadRequest || fa.calls != 0 {
			t.Errorf("q=%q: %d, %d calls", q, rec.Code, fa.calls)
		}
	}
	fa.err = errors.New("dial tcp 10.0.0.1: secret")
	if rec := get(mux, "/api/artists?q=ed"); rec.Code != http.StatusBadGateway || strings.Contains(rec.Body.String(), "secret") {
		t.Errorf("a failure: %d %s", rec.Code, rec.Body)
	}
}

func TestAnArtistsPageShowsWaitingRequestsAsPending(t *testing.T) {
	fa := &fakeArtists{page: catalog.ArtistPage{ID: "x", Name: "Ed Sheeran", LibraryOK: true, Albums: []catalog.Album{
		{ID: id1, Title: "÷"},
		{ID: "22222222-2222-4222-8222-222222222222", Title: "×", Status: catalog.StatusAvailable},
		{ID: "33333333-3333-4333-8333-333333333333", Title: "+"},
	}}}
	rq := &fakeRequests{pending: map[string]requests.Record{id1: {ID: 5}, "22222222-2222-4222-8222-222222222222": {ID: 6}}}
	rec := get(artistRig(rq, fa, auth.User), artistPath)
	var body catalog.ArtistPage
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || rec.Code != 200 {
		t.Fatalf("%d %v %s", rec.Code, err, rec.Body)
	}
	if body.Albums[0].Status != catalog.StatusPending || body.Albums[1].Status != catalog.StatusAvailable || body.Albums[2].Status != "" {
		t.Errorf("pending shows only where Lidarr has nothing to say: %+v", body.Albums)
	}
	if fa.lastID != "b8a7c51f-362c-4dcb-a259-bc6e0095f0a6" {
		t.Errorf("asked for %q", fa.lastID)
	}
}

func TestAnArtistsPageErrors(t *testing.T) {
	for _, c := range []struct {
		err  error
		want int
	}{{catalog.ErrInvalidID, 400}, {catalog.ErrNotInMetadata, 404}, {errors.New("metadata: secret"), 502}} {
		rec := get(artistRig(&fakeRequests{}, &fakeArtists{err: c.err}, auth.User), artistPath)
		if rec.Code != c.want || strings.Contains(rec.Body.String(), "secret") {
			t.Errorf("%v: %d %s", c.err, rec.Code, rec.Body)
		}
	}
}

func TestArtistRoutesAre404WhenNotSetUp(t *testing.T) {
	mux := http.NewServeMux()
	New(&fakeCatalog{}, &fakeCovers{}, &fakeSettings{}, as(auth.User), &fakeUsers{}, &fakeRequests{}, &fakeIssues{}, zerolog.Nop()).Routes(mux)
	for _, p := range []string{"/api/artists?q=ed", artistPath, artistPath + "/photo"} {
		if rec := get(mux, p); rec.Code != http.StatusNotFound {
			t.Errorf("%s: %d", p, rec.Code)
		}
	}
}

func TestArtistPhotoRouteServesTheImageAndMapsErrors(t *testing.T) {
	fa := &fakeArtists{photo: []byte("jpegbytes")}
	rec := get(artistRig(&fakeRequests{}, fa, auth.User), artistPath+"/photo")
	if rec.Code != 200 || rec.Body.String() != "jpegbytes" || rec.Header().Get("Content-Type") != "image/jpeg" || !strings.Contains(rec.Header().Get("Cache-Control"), "max-age") {
		t.Errorf("%d %q %v", rec.Code, rec.Body, rec.Header())
	}
	for _, c := range []struct {
		err  error
		want int
	}{{catalog.ErrInvalidID, 400}, {coverart.ErrInvalid, 400}, {coverart.ErrNoCover, 404}, {errors.New("dial tcp 10.0.0.1: secret"), 502}} {
		rec := get(artistRig(&fakeRequests{}, &fakeArtists{err: c.err}, auth.User), artistPath+"/photo")
		if rec.Code != c.want || strings.Contains(rec.Body.String(), "secret") {
			t.Errorf("%v: %d %s", c.err, rec.Code, rec.Body)
		}
	}
}

func TestArtistPhotoRouteTakesOnlyTheTwoSizes(t *testing.T) {
	for size, want := range map[string]int{"": 480, "480": 480, "96": 96} {
		fa := &fakeArtists{photo: []byte("x")}
		path := artistPath + "/photo"
		if size != "" {
			path += "?size=" + size
		}
		if rec := get(artistRig(&fakeRequests{}, fa, auth.User), path); rec.Code != 200 || fa.lastSize != want {
			t.Errorf("size %q: %d, asked for %d", size, rec.Code, fa.lastSize)
		}
	}
	for _, size := range []string{"0", "100", "1000", "abc", "-96"} {
		fa := &fakeArtists{photo: []byte("x")}
		if rec := get(artistRig(&fakeRequests{}, fa, auth.User), artistPath+"/photo?size="+size); rec.Code != http.StatusBadRequest || fa.calls != 0 {
			t.Errorf("size %q: %d, %d calls", size, rec.Code, fa.calls)
		}
	}
}
