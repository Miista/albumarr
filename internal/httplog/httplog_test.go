package httplog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// capture returns a logger writing JSON lines at debug level, and a way to read them.
func capture() (zerolog.Logger, func() []map[string]any) {
	var buf bytes.Buffer
	l := zerolog.New(&buf).Level(zerolog.DebugLevel)
	return l, func() []map[string]any {
		var out []map[string]any
		for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
			if line == "" {
				continue
			}
			m := map[string]any{}
			if err := json.Unmarshal([]byte(line), &m); err != nil {
				panic(err)
			}
			out = append(out, m)
		}
		return out
	}
}

func TestQueryKeepsTheShapeAndHidesSecrets(t *testing.T) {
	got := Query("query=ed+sheeran&limit=24&api_key=s3cret&access_token=abc&code=xyz&state=st&Authorization=b&password=p")
	for _, secret := range []string{"s3cret", "abc", "xyz", "=st", "=b&", "=p"} {
		if strings.Contains(got, secret) {
			t.Errorf("%q leaked in %q", secret, got)
		}
	}
	for _, kept := range []string{"query=ed sheeran", "limit=24", "api_key=…", "access_token=…"} {
		if !strings.Contains(got, kept) {
			t.Errorf("%q missing from %q", kept, got)
		}
	}
	if Query("") != "" || Query("%zz") != "(unreadable)" {
		t.Error("empty and unreadable queries")
	}
	if long := Query("q=" + strings.Repeat("a", 500)); len([]rune(long)) > 170 {
		t.Errorf("not cut: %d", len(long))
	}
}

type fakeTransport struct {
	status int
	err    error
	sleep  func()
}

func (f fakeTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if f.sleep != nil {
		f.sleep()
	}
	if f.err != nil {
		return nil, f.err
	}
	return &http.Response{StatusCode: f.status, Body: http.NoBody, Request: r}, nil
}

func TestOutgoingRequestsAreLoggedAtTheRightLevelWithoutSecrets(t *testing.T) {
	for _, tc := range []struct {
		status int
		level  string
	}{{200, "debug"}, {404, "debug"}, {401, "warn"}, {429, "warn"}, {503, "warn"}} {
		log, read := capture()
		req := httptest.NewRequest("GET", "https://musicbrainz.org/ws/2/artist?query=ed&api_key=SECRET", nil)
		req.Header.Set("X-Api-Key", "HEADERSECRET")
		req.Header.Set("Authorization", "Bearer TOKEN")
		if _, err := Transport(fakeTransport{status: tc.status}, log).RoundTrip(req); err != nil {
			t.Fatal(err)
		}
		lines := read()
		if len(lines) != 1 || lines[0]["level"] != tc.level || lines[0]["status"] != float64(tc.status) || lines[0]["host"] != "musicbrainz.org" || lines[0]["path"] != "/ws/2/artist" {
			t.Errorf("%d: %v", tc.status, lines)
		}
		raw, _ := json.Marshal(lines)
		for _, secret := range []string{"SECRET", "HEADERSECRET", "TOKEN"} {
			if strings.Contains(string(raw), secret) {
				t.Errorf("%d: %q leaked: %s", tc.status, secret, raw)
			}
		}
	}
}

func TestAFailedOutgoingRequestNeverQuotesItsAddress(t *testing.T) {
	log, read := capture()
	cause := errors.New("connection refused")
	err := &url.Error{Op: "Get", URL: "https://lidarr:8686/api/v1/album?apikey=SECRET", Err: cause}
	req := httptest.NewRequest("GET", "http://lidarr:8686/api/v1/album", nil)
	_, got := Transport(fakeTransport{err: err}, log).RoundTrip(req)
	if got == nil {
		t.Fatal("the error must still reach the caller")
	}
	lines := read()
	raw, _ := json.Marshal(lines)
	if len(lines) != 1 || lines[0]["level"] != "warn" || !strings.Contains(string(raw), "connection refused") || strings.Contains(string(raw), "SECRET") {
		t.Errorf("%s", raw)
	}
}

func TestASlowRequestIsInfo(t *testing.T) {
	log, read := capture()
	slowT := Transport(fakeTransport{status: 200, sleep: func() { time.Sleep(slow + 50*time.Millisecond) }}, log)
	if _, err := slowT.RoundTrip(httptest.NewRequest("GET", "http://x/y", nil)); err != nil {
		t.Fatal(err)
	}
	if l := read(); len(l) != 1 || l[0]["level"] != "info" {
		t.Errorf("%v", l)
	}
}

func TestAccessLogLevelsFieldsAndUser(t *testing.T) {
	log, read := capture()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/search", func(w http.ResponseWriter, r *http.Request) {
		SetUser(r.Context(), "local:admin")
		w.Write([]byte("hello"))
	})
	mux.HandleFunc("/api/boom", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "x", 502) })
	mux.HandleFunc("/api/nope", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "x", 403) })
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {})
	mux.HandleFunc("/app.js", func(w http.ResponseWriter, r *http.Request) {})
	mux.HandleFunc("/auth/login", func(w http.ResponseWriter, r *http.Request) {})
	h := Access(log)(mux)
	for _, target := range []string{"/api/search?q=ed&token=SECRET", "/api/boom", "/api/nope", "/healthz", "/app.js", "/auth/login"} {
		req := httptest.NewRequest("GET", target, nil)
		req.RemoteAddr = "10.0.0.9:5555"
		req.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.2")
		req.Header.Set("Cookie", "session=COOKIESECRET")
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	lines := read()
	if len(lines) != 6 {
		t.Fatalf("%d lines: %v", len(lines), lines)
	}
	want := []struct{ path, level string }{{"/api/search", "info"}, {"/api/boom", "error"}, {"/api/nope", "warn"}, {"/healthz", "debug"}, {"/app.js", "debug"}, {"/auth/login", "info"}}
	for i, w := range want {
		if lines[i]["path"] != w.path || lines[i]["level"] != w.level {
			t.Errorf("line %d: %v, want %+v", i, lines[i], w)
		}
	}
	if l := lines[0]; l["user"] != "local:admin" || l["ip"] != "203.0.113.7" || l["status"] != float64(200) || l["bytes"] != float64(5) || l["method"] != "GET" || l["took"] == nil {
		t.Errorf("fields: %v", l)
	}
	if _, has := lines[1]["user"]; has {
		t.Errorf("no user was set: %v", lines[1])
	}
	raw, _ := json.Marshal(lines)
	for _, secret := range []string{"SECRET", "COOKIESECRET"} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("%q leaked: %s", secret, raw)
		}
	}
}

func TestAccessLogWithoutAHolderIsHarmlessAndControlCharactersAreStripped(t *testing.T) {
	SetUser(context.Background(), "nobody") // no logger around it: must not panic
	log, read := capture()
	h := Access(log)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		SetUser(r.Context(), "evil\nuser\r")
	}))
	req := httptest.NewRequest("GET", "/api/x", nil)
	req.Header.Set("X-Forwarded-For", "1.2.3.4\nFAKE LOG LINE")
	h.ServeHTTP(httptest.NewRecorder(), req)
	lines := read()
	if len(lines) != 1 || strings.ContainsAny(lines[0]["user"].(string), "\r\n") || strings.Contains(lines[0]["ip"].(string), "\n") {
		t.Errorf("%v", lines)
	}
}

func TestTheRecorderPassesFlushingThrough(t *testing.T) {
	log, _ := capture()
	flushed := false
	h := Access(log)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := http.NewResponseController(w).Flush(); err == nil {
			flushed = true
		}
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/x", nil))
	if !flushed {
		t.Error("a handler must still be able to flush through the logger")
	}
}
