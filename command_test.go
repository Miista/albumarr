package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestHealthURLIsReachedOnTheLoopbackWhenTheListenAddressHasNoHost(t *testing.T) {
	for addr, want := range map[string]string{
		":8768":          "http://127.0.0.1:8768/healthz",
		"0.0.0.0:8768":   "http://127.0.0.1:8768/healthz",
		"[::]:8768":      "http://127.0.0.1:8768/healthz",
		"127.0.0.1:9000": "http://127.0.0.1:9000/healthz",
		"10.0.0.5:8768":  "http://10.0.0.5:8768/healthz",
		"[::1]:8768":     "http://[::1]:8768/healthz",
		"localhost:80":   "http://localhost:80/healthz",
	} {
		if got := healthURL(addr); got != want {
			t.Errorf("%q -> %q, want %q", addr, got, want)
		}
	}
}

func TestHealthcheckWantsAPlain200(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		healthy bool
	}{
		{"200", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }, true},
		{"500", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", 500) }, false},
		{"503", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", 503) }, false},
		{"401", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", 401) }, false},
		{"a redirect is not followed", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/elsewhere", http.StatusFound) }, false},
	} {
		srv := httptest.NewServer(tc.handler)
		err := healthcheck(srv.URL+"/healthz", time.Second)
		srv.Close()
		if (err == nil) != tc.healthy {
			t.Errorf("%s: err = %v, healthy = %v", tc.name, err, tc.healthy)
		}
	}
}

func TestHealthcheckFailsWhenNothingAnswersOrItAnswersTooSlowly(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	l.Close() // nothing listens here now
	if err := healthcheck("http://"+addr+"/healthz", time.Second); err == nil || strings.Contains(err.Error(), "127.0.0.1:") && strings.Contains(err.Error(), "dial") {
		t.Errorf("nothing listening: %v (and the message must not be a raw dial error)", err)
	}
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(500 * time.Millisecond) }))
	defer slow.Close()
	start := time.Now()
	if err := healthcheck(slow.URL+"/healthz", 100*time.Millisecond); err == nil || time.Since(start) > 400*time.Millisecond {
		t.Errorf("a slow answer must time out: %v after %v", err, time.Since(start))
	}
}

func TestCommandRunsTheHealthcheckAgainstTheConfiguredAddress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			t.Errorf("asked %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")
	var out, errOut bytes.Buffer
	if handled, code := command([]string{"healthcheck"}, func(k string) string { return map[string]string{"ALBUMARR_ADDR": addr}[k] }, &out, &errOut); !handled || code != 0 {
		t.Errorf("healthy: handled %v, code %d, %q", handled, code, errOut.String())
	}
	srv.Close()
	errOut.Reset()
	if handled, code := command([]string{"healthcheck"}, func(k string) string { return map[string]string{"ALBUMARR_ADDR": addr}[k] }, &out, &errOut); !handled || code != 1 || !strings.Contains(errOut.String(), "unhealthy") {
		t.Errorf("down: handled %v, code %d, %q", handled, code, errOut.String())
	}
}

func TestCommandVersionAndUnknownWords(t *testing.T) {
	var out, errOut bytes.Buffer
	none := func(string) string { return "" }
	if handled, _ := command(nil, none, &out, &errOut); handled {
		t.Error("no arguments: the server starts")
	}
	for _, v := range []string{"version", "--version", "-version"} {
		out.Reset()
		if handled, code := command([]string{v}, none, &out, &errOut); !handled || code != 0 || !strings.HasPrefix(out.String(), "albumarr ") || !strings.Contains(out.String(), version) {
			t.Errorf("%s: %v %d %q", v, handled, code, out.String())
		}
	}
	errOut.Reset()
	if handled, code := command([]string{"serv"}, none, &out, &errOut); !handled || code != 2 || !strings.Contains(errOut.String(), "unknown command") {
		t.Errorf("a typo must not start the server: %v %d %q", handled, code, errOut.String())
	}
}

func TestPermissionHintOnlyForPermissionErrors(t *testing.T) {
	denied := fmt.Errorf("creating /app/data/albumarr.db: %w", &fs.PathError{Op: "open", Path: "/app/data/albumarr.db", Err: os.ErrPermission})
	if h := permissionHint(denied); !strings.Contains(h, "65532") || !strings.Contains(h, "--user") || !strings.Contains(h, "owned by that user") {
		t.Errorf("hint = %q", h)
	}
	for _, err := range []error{errors.New("database is locked"), fmt.Errorf("x: %w", fs.ErrNotExist), nil} {
		if h := permissionHint(err); h != "" {
			t.Errorf("%v: %q", err, h)
		}
	}
}
