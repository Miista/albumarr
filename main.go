// albumreq: a small request frontend for Lidarr. Search for an album, see at a
// glance whether it is already in the library, being downloaded, requested, or
// could not be found, and open it for details and a track list. It never
// writes to Lidarr: it only issues GET requests.
//
// Three sources, each doing what it is best at: MusicBrainz answers searches,
// Lidarr's hosted metadata service gives an album's details and tracks (and
// its image CDN the covers), and your own Lidarr says what you already have.
//
// Configuration comes from environment variables (see the README). There is
// no login yet, so by default the server binds to loopback only; bind it
// wider (ALBUMREQ_ADDR) only behind something that authenticates.
package main

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rs/zerolog"

	"github.com/Miista/albumreq/internal/api"
	"github.com/Miista/albumreq/internal/catalog"
	"github.com/Miista/albumreq/internal/coverart"
	"github.com/Miista/albumreq/internal/lidarr"
	"github.com/Miista/albumreq/internal/lidarrmeta"
	"github.com/Miista/albumreq/internal/musicbrainz"
)

// defaultAddr is loopback on purpose: with no login yet, nothing but this
// machine should be able to reach the app unless the operator opts in.
const defaultAddr = "127.0.0.1:8768"

// staticFiles is the frontend (HTML, JS, CSS), compiled into the binary so
// albumreq ships as a single file.
//
//go:embed web/static
var staticFiles embed.FS

func main() {
	logger := newLogger(os.Getenv("ALBUMREQ_SETTING_LOG_LEVEL"))

	lidarrURL := os.Getenv("ALBUMREQ_SETTING_LIDARR_URL")
	lidarrKey := os.Getenv("ALBUMREQ_SETTING_LIDARR_API_KEY")
	if lidarrURL == "" || lidarrKey == "" {
		logger.Fatal().Msg("ALBUMREQ_SETTING_LIDARR_URL and ALBUMREQ_SETTING_LIDARR_API_KEY must both be set")
	}
	requireHTTPURL(logger, "ALBUMREQ_SETTING_LIDARR_URL", lidarrURL)

	// MusicBrainz requires a contact in the User-Agent. There is deliberately
	// no default: it is the operator's to choose, and it is only ever sent to
	// MusicBrainz.
	mb, err := musicbrainz.New(os.Getenv("ALBUMREQ_SETTING_MUSICBRAINZ_CONTACT"))
	if err != nil {
		logger.Fatal().Msg("ALBUMREQ_SETTING_MUSICBRAINZ_CONTACT must be set to an email address or a project URL: MusicBrainz requires a way to contact whoever runs an application")
	}

	metaURL := os.Getenv("ALBUMREQ_SETTING_METADATA_URL") // default: Lidarr's hosted metadata service
	if metaURL != "" {
		requireHTTPURL(logger, "ALBUMREQ_SETTING_METADATA_URL", metaURL)
	}
	coverURL := os.Getenv("ALBUMREQ_SETTING_COVER_URL") // default: Lidarr's image CDN
	if coverURL != "" {
		requireHTTPURL(logger, "ALBUMREQ_SETTING_COVER_URL", coverURL)
	}

	addr := os.Getenv("ALBUMREQ_ADDR")
	if addr == "" {
		addr = defaultAddr
	}

	library := lidarr.New(lidarrURL, lidarrKey)
	cat := catalog.New(mb, lidarrmeta.New(metaURL), library)
	mux := http.NewServeMux()
	api.New(cat, coverart.New(coverURL), logger).Routes(mux)

	static, err := fs.Sub(staticFiles, "web/static")
	if err != nil {
		logger.Fatal().Err(err).Msg("embedded frontend assets missing")
	}
	mux.Handle("/", noCache(http.FileServer(http.FS(static))))

	httpServer := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		logger.Info().Msg(fmt.Sprintf("starting albumreq on %s, Lidarr at %s", addr, lidarrURL))
		if !isLoopback(addr) {
			logger.Warn().Msg("listening beyond loopback with no login: anyone who can reach this port can search your Lidarr")
		}
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatal().Err(err).Msg("server stopped")
		}
	}()

	<-ctx.Done()
	logger.Info().Msg("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdownCtx)
}

// requireHTTPURL stops the process unless value is an absolute http(s) URL.
func requireHTTPURL(logger zerolog.Logger, name, value string) {
	if u, err := url.Parse(value); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		logger.Fatal().Msg(name + " must be an http(s) URL")
	}
}

// isLoopback reports whether addr (host:port) binds only to the local machine.
// An empty host (":8768") listens on every interface.
func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// noCache makes the browser revalidate the frontend on every load, so a new
// build is never masked by a stale cached script.
func noCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		next.ServeHTTP(w, r)
	})
}
