// Package httplog writes down every HTTP request albumarr serves and every one it
// makes, so a failure that happened once can still be explained later.
//
// What it records is the method, the address, the status and how long it took;
// never a body, a header or a cookie, and the value of any query parameter that
// could carry a secret is replaced. Requests for the page's own files and the
// health check are logged at debug level, everything else at info (warn for a
// 4xx, error for a 5xx).
package httplog

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/rs/zerolog"
)

// secretParam matches query parameter names whose value must never be written down.
var secretParam = regexp.MustCompile(`(?i)(key|token|secret|pass|auth|code|state|nonce|session|cookie|sig)`)

// Query returns a query string that is safe to log: the parameters are kept,
// but the value of any that looks like a secret is replaced, and it is cut to
// a sensible length.
func Query(raw string) string {
	if raw == "" {
		return ""
	}
	vals, err := url.ParseQuery(raw)
	if err != nil {
		return "(unreadable)"
	}
	parts := make([]string, 0, len(vals))
	for k, vs := range vals {
		for _, v := range vs {
			if secretParam.MatchString(k) {
				v = "…"
			}
			parts = append(parts, k+"="+v)
		}
	}
	out := strings.Join(parts, "&")
	if r := []rune(out); len(r) > 160 {
		out = string(r[:160]) + "…"
	}
	return out
}

func clean(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

// Transport logs the requests made through next. A successful one is debug (a
// search asks Lidarr about every result), a failure or a slow one is info or warn.
func Transport(next http.RoundTripper, log zerolog.Logger) http.RoundTripper {
	return roundTripper{next: next, log: log}
}

type roundTripper struct {
	next http.RoundTripper
	log  zerolog.Logger
}

const slow = 2 * time.Second

func (t roundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()
	resp, err := t.next.RoundTrip(req)
	took := time.Since(start)
	ev := t.log.With().
		Str("method", req.Method).
		Str("host", req.URL.Host).
		Str("path", clean(req.URL.Path)).
		Str("query", clean(Query(req.URL.RawQuery))).
		Dur("took", took.Round(time.Millisecond)).Logger()
	switch {
	case err != nil:
		// A transport error can quote the whole URL: keep what is underneath.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		ev.Warn().Str("error", clean(err.Error())).Msg("outgoing request failed")
	case resp.StatusCode >= 500:
		ev.Warn().Int("status", resp.StatusCode).Msg("outgoing request answered with a server error")
	case resp.StatusCode >= 400 && resp.StatusCode != http.StatusNotFound:
		ev.Warn().Int("status", resp.StatusCode).Msg("outgoing request refused")
	case took > slow:
		ev.Info().Int("status", resp.StatusCode).Msg("outgoing request was slow")
	default:
		ev.Debug().Int("status", resp.StatusCode).Msg("outgoing request")
	}
	return resp, err
}

type userKey struct{}

// holder carries who made a request from the code that knows to the logger that
// reports it.
type holder struct{ sub string }

// SetUser says who the request in ctx is from, for the access log.
func SetUser(ctx context.Context, sub string) {
	if h, ok := ctx.Value(userKey{}).(*holder); ok {
		h.sub = clean(sub)
	}
}

type recorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *recorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

// Unwrap lets http.ResponseController reach the real writer (flushing and so on).
func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// quiet reports whether a path is only noise at info level: the page's own files
// and the health check.
func quiet(path string) bool {
	return path == "/healthz" || !(strings.HasPrefix(path, "/api/") || strings.HasPrefix(path, "/auth/"))
}

// clientIP is the address the request came from: the one the proxy in front
// reports, else the peer's. It is for the log only, so it is not trusted for anything.
func clientIP(r *http.Request) string {
	if f := r.Header.Get("X-Forwarded-For"); f != "" {
		first, _, _ := strings.Cut(f, ",")
		return clean(strings.TrimSpace(first))
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return clean(r.RemoteAddr)
	}
	return host
}

// Access logs every request that goes through next.
func Access(log zerolog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			h := &holder{}
			rec := &recorder{ResponseWriter: w}
			next.ServeHTTP(rec, r.WithContext(context.WithValue(r.Context(), userKey{}, h)))
			status := rec.status
			if status == 0 {
				status = http.StatusOK
			}
			ev := log.With().
				Str("method", r.Method).
				Str("path", clean(r.URL.Path)).
				Int("status", status).
				Int("bytes", rec.bytes).
				Dur("took", time.Since(start).Round(time.Millisecond)).
				Str("ip", clientIP(r))
			if h.sub != "" {
				ev = ev.Str("user", h.sub)
			}
			if q := Query(r.URL.RawQuery); q != "" {
				ev = ev.Str("query", clean(q))
			}
			l := ev.Logger()
			switch {
			case status >= 500:
				l.Error().Msg("request")
			case status >= 400:
				l.Warn().Msg("request")
			case quiet(r.URL.Path):
				l.Debug().Msg("request")
			default:
				l.Info().Msg("request")
			}
		})
	}
}
