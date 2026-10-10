package lidarr

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
