package lidarr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testKey = "s3cret-key"

func TestAlbumByForeignIDSendsKeyAndID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/album" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("X-Api-Key"); got != testKey {
			t.Errorf("X-Api-Key = %q", got)
		}
		if got := r.URL.Query().Get("foreignAlbumId"); got != "abc-123" {
			t.Errorf("foreignAlbumId = %q", got)
		}
		fmt.Fprint(w, `[{"id":7,"monitored":true,"lastSearchTime":"2026-10-10T15:16:09Z",
			"statistics":{"trackFileCount":12,"totalTrackCount":12,"sizeOnDisk":277095930,"percentOfTracks":100}}]`)
	}))
	defer srv.Close()

	st, err := New(srv.URL, testKey).AlbumByForeignID(context.Background(), "abc-123")
	if err != nil || st == nil {
		t.Fatalf("got %v, %v", st, err)
	}
	if st.ID != 7 || !st.Monitored || st.Statistics.SizeOnDisk != 277095930 || st.Statistics.PercentOfTracks != 100 {
		t.Errorf("unexpected state: %+v", st)
	}
}

func TestAlbumByForeignIDUnknownAlbumIsNilNotAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `[]`) }))
	defer srv.Close()
	st, err := New(srv.URL, testKey).AlbumByForeignID(context.Background(), "missing")
	if st != nil || err != nil {
		t.Errorf("got %v, %v, want nil, nil", st, err)
	}
}

func TestAlbumByForeignIDEncodesTheID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("foreignAlbumId"); got != "a&apikey=x#y" {
			t.Errorf("foreignAlbumId = %q: the ID must stay one query value", got)
		}
		if r.URL.Query().Get("apikey") != "" {
			t.Error("an ID must not be able to inject another parameter")
		}
		fmt.Fprint(w, `[]`)
	}))
	defer srv.Close()
	if _, err := New(srv.URL, testKey).AlbumByForeignID(context.Background(), "a&apikey=x#y"); err != nil {
		t.Fatal(err)
	}
}

func TestTracks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/track" || r.URL.Query().Get("albumId") != "1" {
			t.Errorf("unexpected request %s", r.URL)
		}
		fmt.Fprint(w, `[{"mediumNumber":1,"trackNumber":"1","title":"Takedown","duration":181000,"hasFile":true},
			{"mediumNumber":1,"trackNumber":"2","title":"Missing","duration":176000,"hasFile":false}]`)
	}))
	defer srv.Close()

	got, err := New(srv.URL, testKey).Tracks(context.Background(), 1)
	if err != nil || len(got) != 2 {
		t.Fatalf("got %v, %v", got, err)
	}
	if got[0].Title != "Takedown" || got[0].DurationMs != 181000 || !got[0].HasFile || got[1].HasFile {
		t.Errorf("unexpected tracks: %+v", got)
	}
}

func TestQueuedAlbumIDsPaginates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("page") {
		case "1":
			recs := make([]string, queuePageSize)
			for i := range recs {
				recs[i] = fmt.Sprintf(`{"albumId":%d}`, i+1)
			}
			fmt.Fprintf(w, `{"totalRecords":%d,"records":[%s]}`, queuePageSize+1, strings.Join(recs, ","))
		case "2":
			fmt.Fprintf(w, `{"totalRecords":%d,"records":[{"albumId":999}]}`, queuePageSize+1)
		default:
			t.Errorf("unexpected page %q", r.URL.Query().Get("page"))
		}
	}))
	defer srv.Close()

	got, err := New(srv.URL, testKey).QueuedAlbumIDs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != queuePageSize+1 || !got[999] || !got[1] {
		t.Errorf("got %d ids, want %d (including 1 and 999)", len(got), queuePageSize+1)
	}
}

func TestEmptyQueue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"totalRecords":0,"records":[]}`)
	}))
	defer srv.Close()
	got, err := New(srv.URL, testKey).QueuedAlbumIDs(context.Background())
	if err != nil || len(got) != 0 {
		t.Errorf("got %v, %v", got, err)
	}
}

func TestErrorsNeverContainTheAPIKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom "+r.Header.Get("X-Api-Key"), http.StatusInternalServerError)
	}))
	c := New(srv.URL, testKey)
	_, statusErr := c.AlbumByForeignID(context.Background(), "x")
	srv.Close()
	_, connErr := c.AlbumByForeignID(context.Background(), "x") // server is gone now

	for name, err := range map[string]error{"status error": statusErr, "connection error": connErr} {
		if err == nil {
			t.Errorf("%s: expected an error", name)
			continue
		}
		if strings.Contains(err.Error(), testKey) {
			t.Errorf("%s leaks the API key: %v", name, err)
		}
	}
}

func TestBadJSONIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `<html>`) }))
	defer srv.Close()
	if _, err := New(srv.URL, testKey).Tracks(context.Background(), 1); err == nil {
		t.Error("an HTML response must not decode")
	}
}

// --- AddAlbum ---

func newAlbum() NewAlbum {
	return NewAlbum{
		AlbumID: "b1392450-e666-3926-a536-22c65f834433", ArtistID: "a74b1b7f-71a5-4011-9441-d0b5e4122711",
		RootFolder: "/music", QualityProfileID: 5, MetadataProfileID: 1,
	}
}

func TestAddAlbumSendsExactlyTheUnmonitoredRequest(t *testing.T) {
	var gotMethod, gotPath, gotKey, gotType string
	var raw map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotKey, gotType = r.Method, r.URL.Path, r.Header.Get("X-Api-Key"), r.Header.Get("Content-Type")
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			t.Error(err)
		}
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"id":1}`)
	}))
	defer srv.Close()

	if err := New(srv.URL, testKey).AddAlbum(context.Background(), newAlbum()); err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost || gotPath != "/api/v1/album" || gotKey != testKey || gotType != "application/json" {
		t.Errorf("request = %s %s key=%q type=%q", gotMethod, gotPath, gotKey, gotType)
	}

	// Every flag that could trigger a search or a download must be present and false/none:
	// a missing key would fall back to Lidarr's default, which is to monitor.
	artist, _ := raw["artist"].(map[string]any)
	albumOpts, _ := raw["addOptions"].(map[string]any)
	artistOpts, _ := artist["addOptions"].(map[string]any)
	want := map[string]any{
		"foreignAlbumId": "b1392450-e666-3926-a536-22c65f834433",
		"monitored":      false,
	}
	for k, v := range want {
		if raw[k] != v {
			t.Errorf("%s = %v, want %v", k, raw[k], v)
		}
	}
	checks := []struct {
		name string
		got  any
		want any
	}{
		{"addOptions.addType", albumOpts["addType"], "manual"},
		{"addOptions.searchForNewAlbum", albumOpts["searchForNewAlbum"], false},
		{"artist.foreignArtistId", artist["foreignArtistId"], "a74b1b7f-71a5-4011-9441-d0b5e4122711"},
		{"artist.monitored", artist["monitored"], false},
		{"artist.monitorNewItems", artist["monitorNewItems"], "none"},
		{"artist.rootFolderPath", artist["rootFolderPath"], "/music"},
		{"artist.qualityProfileId", artist["qualityProfileId"], float64(5)},
		{"artist.metadataProfileId", artist["metadataProfileId"], float64(1)},
		{"artist.addOptions.monitor", artistOpts["monitor"], "none"},
		{"artist.addOptions.searchForMissingAlbums", artistOpts["searchForMissingAlbums"], false},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v (%T), want %v", c.name, c.got, c.got, c.want)
		}
	}
}

func TestAddAlbumAlreadyAddedIsReportedNotRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `[{"propertyName":"ForeignAlbumId","errorMessage":"This album has already been added.","errorCode":"AlbumExistsValidator"}]`)
	}))
	defer srv.Close()
	err := New(srv.URL, testKey).AddAlbum(context.Background(), newAlbum())
	var rej *RejectedError
	if !errors.Is(err, ErrAlreadyAdded) || errors.As(err, &rej) {
		t.Errorf("err = %v, want ErrAlreadyAdded and not a RejectedError", err)
	}
}

func TestAddAlbumRejections(t *testing.T) {
	cases := map[string]struct {
		body string
		want []string
	}{
		"bad quality profile":   {`[{"errorMessage":"Quality Profile does not exist","errorCode":"QualityProfileExistsValidator"}]`, []string{"Quality Profile does not exist"}},
		"several failures":      {`[{"errorMessage":"Root folder '/nope' does not exist"},{"errorMessage":"Metadata profile does not exist"}]`, []string{"Root folder '/nope' does not exist", "Metadata profile does not exist"}},
		"unknown album":         {`[{"propertyName":"MusicbrainzId","errorMessage":"An album with this ID was not found"}]`, []string{"An album with this ID was not found"}},
		"no body":               {``, []string{"no reason given"}},
		"not json":              {`<html>`, []string{"no reason given"}},
		"an empty failure list": {`[]`, []string{"no reason given"}},
	}
	for name, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, c.body)
		}))
		err := New(srv.URL, testKey).AddAlbum(context.Background(), newAlbum())
		srv.Close()
		var rej *RejectedError
		if !errors.As(err, &rej) || errors.Is(err, ErrAlreadyAdded) {
			t.Errorf("%s: err = %v, want a RejectedError", name, err)
			continue
		}
		if strings.Join(rej.Messages, "|") != strings.Join(c.want, "|") {
			t.Errorf("%s: messages = %v, want %v", name, rej.Messages, c.want)
		}
	}
}

func TestAddAlbumOutagesAreUnavailableAndNeverLeakTheKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom "+r.Header.Get("X-Api-Key"), http.StatusServiceUnavailable)
	}))
	c := New(srv.URL, testKey)
	statusErr := c.AddAlbum(context.Background(), newAlbum())
	srv.Close()
	connErr := c.AddAlbum(context.Background(), newAlbum()) // the server is gone

	for name, err := range map[string]error{"503": statusErr, "connection refused": connErr} {
		if !errors.Is(err, ErrUnavailable) {
			t.Errorf("%s: err = %v, want ErrUnavailable", name, err)
			continue
		}
		if strings.Contains(err.Error(), testKey) {
			t.Errorf("%s leaks the API key: %v", name, err)
		}
	}
}

func TestAddAlbumOtherStatusesAreNeitherSuccessNorRejection(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
			fmt.Fprint(w, "secret detail "+testKey)
		}))
		err := New(srv.URL, testKey).AddAlbum(context.Background(), newAlbum())
		srv.Close()
		var rej *RejectedError
		if err == nil || errors.Is(err, ErrAlreadyAdded) || errors.Is(err, ErrUnavailable) || errors.As(err, &rej) || strings.Contains(err.Error(), "secret") {
			t.Errorf("status %d: err = %v", code, err)
		}
	}
}

func TestAddAlbumHonoursContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	err := New(srv.URL, testKey).AddAlbum(ctx, newAlbum())
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrUnavailable) {
		t.Errorf("err = %v, want the context error and not ErrUnavailable", err)
	}
}

// --- CheckSettings ---

func settingsServer(roots, quality, metadata string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/rootfolder":
			fmt.Fprint(w, roots)
		case "/api/v1/qualityprofile":
			fmt.Fprint(w, quality)
		case "/api/v1/metadataprofile":
			fmt.Fprint(w, metadata)
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestCheckSettings(t *testing.T) {
	const roots = `[{"id":1,"path":"/music"},{"id":2,"path":"/other"}]`
	const quality = `[{"id":1,"name":"Any"},{"id":5,"name":"FLAC"}]`
	const metadata = `[{"id":1,"name":"Standard"}]`
	cases := []struct {
		name            string
		root            string
		qp, mp          int
		wantErrContains string
	}{
		{"all present", "/music", 5, 1, ""},
		{"missing root folder", "/nope", 5, 1, `root folder "/nope"`},
		{"root folder is matched exactly, not by prefix", "/mus", 5, 1, `root folder "/mus"`},
		{"missing quality profile", "/music", 99, 1, "quality profile 99"},
		{"missing metadata profile", "/music", 5, 99, "metadata profile 99"},
	}
	for _, c := range cases {
		srv := settingsServer(roots, quality, metadata)
		err := New(srv.URL, testKey).CheckSettings(context.Background(), c.root, c.qp, c.mp)
		srv.Close()
		switch {
		case c.wantErrContains == "" && err != nil:
			t.Errorf("%s: unexpected error %v", c.name, err)
		case c.wantErrContains != "" && (err == nil || !strings.Contains(err.Error(), c.wantErrContains)):
			t.Errorf("%s: err = %v, want it to mention %q", c.name, err, c.wantErrContains)
		case c.wantErrContains != "" && !errors.Is(err, ErrBadSettings):
			t.Errorf("%s: a mismatch must be ErrBadSettings so startup can tell it from an outage: %v", c.name, err)
		}
	}
}

func TestCheckSettingsWhenLidarrIsDown(t *testing.T) {
	srv := settingsServer(`[]`, `[]`, `[]`)
	c := New(srv.URL, testKey)
	srv.Close()
	if err := c.CheckSettings(context.Background(), "/music", 1, 1); err == nil {
		t.Error("an unreachable Lidarr must be an error, not a pass")
	}
}

// --- settings page support: Version, Options, Live ---

func TestVersion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/system/status" || r.Header.Get("X-Api-Key") != testKey {
			t.Errorf("unexpected request %s key=%q", r.URL, r.Header.Get("X-Api-Key"))
		}
		fmt.Fprint(w, `{"appName":"Lidarr","version":"3.1.0.4875"}`)
	}))
	defer srv.Close()
	v, err := New(srv.URL, testKey).Version(context.Background())
	if err != nil || v != "3.1.0.4875" {
		t.Errorf("got %q, %v", v, err)
	}
}

func TestVersionRecognisesAWrongKeyAndANonLidarr(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(code) }))
		_, err := New(srv.URL, "wrong").Version(context.Background())
		srv.Close()
		if !errors.Is(err, ErrUnauthorized) {
			t.Errorf("status %d: err = %v, want ErrUnauthorized", code, err)
		}
	}
	// Something that answers 200 with JSON but is not Lidarr must not pass as it.
	for name, body := range map[string]string{"empty object": `{}`, "a list": `[]`, "html": `<html>`} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
		_, err := New(srv.URL, testKey).Version(context.Background())
		srv.Close()
		if err == nil {
			t.Errorf("%s: a non-Lidarr answer was accepted", name)
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	c := New(srv.URL, testKey)
	srv.Close()
	if _, err := c.Version(context.Background()); err == nil || errors.Is(err, ErrUnauthorized) || strings.Contains(err.Error(), testKey) {
		t.Errorf("an unreachable Lidarr: err = %v", err)
	}
}

func TestOptions(t *testing.T) {
	srv := settingsServer(`[{"id":1,"path":"/music"},{"id":2,"path":"/data/music/lidarr"}]`,
		`[{"id":1,"name":"Any"},{"id":5,"name":"FLAC then 192+"}]`, `[{"id":1,"name":"Standard"},{"id":2,"name":"None"}]`)
	defer srv.Close()
	o, err := New(srv.URL, testKey).Options(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(o.RootFolders) != 2 || o.RootFolders[1] != "/data/music/lidarr" || len(o.QualityProfiles) != 2 || o.QualityProfiles[1] != (Profile{5, "FLAC then 192+"}) || o.MetadataProfiles[0].Name != "Standard" {
		t.Errorf("unexpected options: %+v", o)
	}
	empty := settingsServer(`[]`, `[]`, `[]`)
	defer empty.Close()
	o, _ = New(empty.URL, testKey).Options(context.Background())
	if o.RootFolders == nil || o.QualityProfiles == nil || o.MetadataProfiles == nil {
		t.Error("empty lists must encode as [], not null")
	}
}

func TestLiveAnswersNotConfiguredUntilGivenAClient(t *testing.T) {
	var l Live
	ctx := context.Background()
	if _, err := l.AlbumByForeignID(ctx, "x"); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("AlbumByForeignID: %v", err)
	}
	if _, err := l.QueuedAlbumIDs(ctx); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("QueuedAlbumIDs: %v", err)
	}
	if _, err := l.Tracks(ctx, 1); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("Tracks: %v", err)
	}
	if err := l.AddAlbum(ctx, newAlbum()); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("AddAlbum must refuse to write when nothing is configured: %v", err)
	}
	if l.Client() != nil {
		t.Error("Client() must be nil before Set")
	}
}

func TestLiveSwitchesLidarrAtRuntime(t *testing.T) {
	var hits [2]int
	mk := func(i int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits[i]++
			fmt.Fprint(w, `[]`)
		}))
	}
	a, b := mk(0), mk(1)
	defer a.Close()
	defer b.Close()

	var l Live
	l.Set(New(a.URL, testKey))
	l.AlbumByForeignID(context.Background(), "x")
	l.Set(New(b.URL, testKey))
	l.AlbumByForeignID(context.Background(), "x")
	if hits[0] != 1 || hits[1] != 1 {
		t.Errorf("calls went to %v, want one each", hits)
	}
	l.Set(nil)
	if _, err := l.AlbumByForeignID(context.Background(), "x"); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("after Set(nil): %v", err)
	}
}

func TestLiveIsSafeUnderConcurrentSwaps(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `[]`) }))
	defer srv.Close()
	var l Live
	done := make(chan struct{})
	go func() {
		for i := 0; i < 200; i++ {
			if i%2 == 0 {
				l.Set(New(srv.URL, testKey))
			} else {
				l.Set(nil)
			}
		}
		close(done)
	}()
	for {
		select {
		case <-done:
			return
		default:
			l.AlbumByForeignID(context.Background(), "x")
		}
	}
}
