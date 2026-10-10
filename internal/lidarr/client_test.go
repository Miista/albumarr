package lidarr

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

const testKey = "s3cret-key"

func TestLookupSendsKeyAndTerm(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/album/lookup" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("X-Api-Key"); got != testKey {
			t.Errorf("X-Api-Key = %q", got)
		}
		if got := r.URL.Query().Get("term"); got != "ok computer" {
			t.Errorf("term = %q", got)
		}
		fmt.Fprint(w, `[{"id":7,"title":"OK Computer","foreignAlbumId":"abc","albumType":"Album","artist":{"artistName":"Radiohead","foreignArtistId":"xyz"}}]`)
	}))
	defer srv.Close()

	got, err := New(srv.URL, testKey).Lookup(context.Background(), "ok computer")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Title != "OK Computer" || got[0].Artist.Name != "Radiohead" || got[0].ID == nil || *got[0].ID != 7 {
		t.Errorf("unexpected result: %+v", got)
	}
}

func TestAlbumsRepeatsIDs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query()["albumIds"]; len(got) != 2 || got[0] != "3" || got[1] != "9" {
			t.Errorf("albumIds = %v", got)
		}
		fmt.Fprint(w, `[{"id":3,"monitored":true,"statistics":{"trackFileCount":0}},{"id":9,"statistics":{"trackFileCount":12,"totalTrackCount":12,"percentOfTracks":100}}]`)
	}))
	defer srv.Close()

	got, err := New(srv.URL, testKey).Albums(context.Background(), []int{3, 9})
	if err != nil {
		t.Fatal(err)
	}
	if !got[3].Monitored || got[9].Statistics.PercentOfTracks != 100 {
		t.Errorf("unexpected states: %+v", got)
	}
}

func TestAlbumsWithNoIDsMakesNoRequest(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer srv.Close()

	got, err := New(srv.URL, testKey).Albums(context.Background(), nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v", got, err)
	}
	if calls.Load() != 0 {
		t.Errorf("made %d requests, want 0", calls.Load())
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

func TestErrorsNeverContainTheAPIKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom "+r.Header.Get("X-Api-Key"), http.StatusInternalServerError)
	}))
	c := New(srv.URL, testKey)
	_, statusErr := c.Lookup(context.Background(), "x")
	srv.Close()
	_, connErr := c.Lookup(context.Background(), "x") // server is gone now

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

func TestValidCoverPath(t *testing.T) {
	valid := []string{
		"/MediaCoverProxy/2f4978c9de87043e/abc.jpg",
		"/MediaCoverProxy/2f4978c9de87043eabfeb71bfbc5d27b87895eddd31",
	}
	invalid := map[string]string{
		"empty":             "",
		"other lidarr path": "/api/v1/album",
		"traversal":         "/MediaCoverProxy/../api/v1/album",
		"query":             "/MediaCoverProxy/a?x=1",
		"fragment":          "/MediaCoverProxy/a#b",
		"absolute url":      "http://evil.example/MediaCoverProxy/a",
		"scheme-relative":   "//evil.example/MediaCoverProxy/a",
		"encoded dots":      "/MediaCoverProxy/a%2e%2e/b",
		"space":             "/MediaCoverProxy/a b",
		"backslash":         `/MediaCoverProxy/a\b`,
		"too long":          "/MediaCoverProxy/" + strings.Repeat("a", 300),
		"prefix only case":  "/mediacoverproxy/abc",
	}
	for _, p := range valid {
		if !ValidCoverPath(p) {
			t.Errorf("ValidCoverPath(%q) = false, want true", p)
		}
	}
	for name, p := range invalid {
		if ValidCoverPath(p) {
			t.Errorf("%s: ValidCoverPath(%q) = true, want false", name, p)
		}
	}
}

func TestCoverFetchesWithoutAPIKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "" {
			t.Error("cover request must not carry the API key")
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write([]byte("jpegbytes"))
	}))
	defer srv.Close()

	data, ct, err := New(srv.URL, testKey).Cover(context.Background(), "/MediaCoverProxy/abc/cover.jpg")
	if err != nil || string(data) != "jpegbytes" || ct != "image/jpeg" {
		t.Fatalf("got %q, %q, %v", data, ct, err)
	}
}

func TestCoverRejections(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		switch r.URL.Path {
		case "/MediaCoverProxy/html":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, "<html>")
		case "/MediaCoverProxy/huge":
			w.Header().Set("Content-Type", "image/png")
			w.Write(make([]byte, maxCoverBytes+10))
		case "/MediaCoverProxy/missing":
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := New(srv.URL, testKey)

	for name, p := range map[string]string{"non-image": "/MediaCoverProxy/html", "oversized": "/MediaCoverProxy/huge", "upstream 404": "/MediaCoverProxy/missing"} {
		if _, _, err := c.Cover(context.Background(), p); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}

	before := calls.Load()
	_, _, err := c.Cover(context.Background(), "/api/v1/album")
	if !errors.Is(err, ErrInvalidCoverPath) {
		t.Errorf("err = %v, want ErrInvalidCoverPath", err)
	}
	if calls.Load() != before {
		t.Error("an invalid cover path must not reach Lidarr")
	}
}

func TestLookupAlbumCoverPath(t *testing.T) {
	a := LookupAlbum{Images: []Image{
		{CoverType: "disc", URL: "/MediaCoverProxy/disc/1.jpg"},
		{CoverType: "cover", URL: "https://evil.example/x.jpg"},
		{CoverType: "cover", URL: "/MediaCoverProxy/good/2.jpg"},
	}}
	if got := a.CoverPath(); got != "/MediaCoverProxy/good/2.jpg" {
		t.Errorf("CoverPath() = %q", got)
	}
	if got := (LookupAlbum{Images: []Image{{CoverType: "cover", URL: "https://evil.example/x.jpg"}}}).CoverPath(); got != "" {
		t.Errorf("CoverPath() = %q, want empty for a non-proxy URL", got)
	}
}
