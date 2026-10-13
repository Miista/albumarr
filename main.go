// albumreq: a small request frontend for Lidarr. Search for an album, see at a
// glance whether it is already in the library, being downloaded, requested, or
// could not be found, and open it for details and a track list. Its one write
// is requesting an album: Lidarr adds it (and its artist, unmonitored),
// monitors it and searches for it, so a download starts if a release is found.
// Requesting is off unless Lidarr's root folder and profiles are configured.
//
// Three sources, each doing what it is best at: MusicBrainz answers searches,
// Lidarr's hosted metadata service gives an album's details and tracks (and
// its image CDN the covers), and your own Lidarr says what you already have.
//
// Lidarr is configured on the settings page; any setting can instead be pinned
// by an environment variable, which locks it on the page (see the README). There is
// login (authy): a local account, or OIDC, with roles from groups.
// With the login switched off the server binds to loopback only by default.
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
	"path/filepath"
	"syscall"
	"time"

	"github.com/miista/authy"
	"github.com/rs/zerolog"

	"github.com/Miista/albumreq/internal/api"
	"github.com/Miista/albumreq/internal/auth"
	"github.com/Miista/albumreq/internal/catalog"
	"github.com/Miista/albumreq/internal/coverart"
	"github.com/Miista/albumreq/internal/db"
	"github.com/Miista/albumreq/internal/issues"
	"github.com/Miista/albumreq/internal/lidarr"
	"github.com/Miista/albumreq/internal/lidarrmeta"
	"github.com/Miista/albumreq/internal/musicbrainz"
	"github.com/Miista/albumreq/internal/notify"
	"github.com/Miista/albumreq/internal/requests"
	"github.com/Miista/albumreq/internal/settings"
	"github.com/Miista/albumreq/internal/store"
	"github.com/Miista/albumreq/internal/users"
)

// defaultAddr is loopback on purpose: with no login yet, nothing but this
// machine should be able to reach the app unless the operator opts in.
const defaultAddr = "127.0.0.1:8768"

// defaultDataDir holds config.json, the settings saved from the settings page.
const defaultDataDir = "/app/data"

// sessionTTL is how long a sign-in lasts without use, and so how long a change of
// someone's groups at the identity provider can go unnoticed.
const sessionTTL = 12 * time.Hour

// staticFiles is the frontend (HTML, JS, CSS), compiled into the binary so
// albumreq ships as a single file.
//
//go:embed web/static
var staticFiles embed.FS

func main() {
	logger := newLogger(os.Getenv("ALBUMREQ_SETTING_LOG_LEVEL"))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// MusicBrainz requires a contact in the User-Agent. There is deliberately
	// no default: it is the operator's to choose, and it is only ever sent to
	// MusicBrainz.
	contact := os.Getenv("ALBUMREQ_SETTING_MUSICBRAINZ_CONTACT")
	mbURL := os.Getenv("ALBUMREQ_SETTING_MUSICBRAINZ_URL") // default: the public MusicBrainz
	var (
		mb  *musicbrainz.Client
		err error
	)
	if mbURL != "" {
		requireHTTPURL(logger, "ALBUMREQ_SETTING_MUSICBRAINZ_URL", mbURL)
		mb, err = musicbrainz.NewAt(mbURL, contact)
	} else {
		mb, err = musicbrainz.New(contact)
	}
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

	dataDir := os.Getenv("ALBUMREQ_DATA_DIR")
	if dataDir == "" {
		dataDir = defaultDataDir
	}
	if _, set := os.LookupEnv("ALBUMREQ_SETTING_LIDARR_METADATA_PROFILE_ID"); set {
		logger.Warn().Msg("ALBUMREQ_SETTING_LIDARR_METADATA_PROFILE_ID is no longer used: requested artists always get the metadata profile named \"None\", looked up in Lidarr")
	}
	database, err := db.Open(dataDir)
	if err != nil {
		logger.Fatal().Err(err).Msg("cannot open the database in " + dataDir)
	}
	defer database.Close()
	st, err := store.New(database)
	if err != nil {
		logger.Fatal().Err(err).Msg("cannot read the settings from the database")
	}
	go backupLoop(ctx, logger, database, dataDir)

	// Login. authy is on unless ALBUMREQ_AUTH_DISABLED=true: with no ALBUMREQ_OIDC_*
	// the first visitor creates a local account (the admin); with them the identity
	// provider signs people in, and ALBUMREQ_LOCAL_ENABLED=true keeps the local
	// account next to it as a way in when the provider is down.
	// albumreq asks the provider for groups, which is how admin and user are told
	// apart. Sessions are shorter than authy's week because a person's groups
	// are only read when they sign in: a change at the provider reaches albumreq
	// at the next sign-in, so this is also how long a demotion can take.
	gate, err := authy.FromEnv(ctx, "albumreq",
		authy.WithScopes("openid", "profile", "email", "groups"),
		authy.WithSessionTTL(sessionTTL),
		authy.WithDataDir(filepath.Join(dataDir, "auth")))
	if err != nil {
		logger.Fatal().Err(err).Msg("login is misconfigured")
	}
	known := users.New(database)
	roles := auth.ConfigFromEnv(gate != nil)
	switch {
	case gate == nil:
		logger.Warn().Msg("login is OFF (ALBUMREQ_AUTH_DISABLED): everyone who can reach this port is an admin")
	case roles.AdminGroup == "":
		logger.Warn().Msg("ALBUMREQ_ROLE_ADMIN_GROUP is not set: no one signing in through OIDC is an admin, only the local account (if enabled)")
	}

	library := &lidarr.Live{}
	cat := catalog.New(mb, lidarrmeta.New(metaURL), library)
	service := settings.NewService(st, library, cat, logger)
	func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		service.Reload(ctx)
	}()
	mux := http.NewServeMux()
	flow := requests.NewService(requests.NewStore(database), cat)
	authn := auth.New(roles, known, logger)

	// Notifications through ntfy. Sending happens in a worker, never in a request;
	// the poller tells requesters when an approved album has arrived.
	ntfyStore, err := notify.NewStore(database)
	if err != nil {
		logger.Fatal().Err(err).Msg("cannot read the notification settings from the database")
	}
	ntfy := notify.NewClient()
	notifySettings := notify.NewSettings(ntfyStore, ntfy, logger)
	dispatcher := notify.NewDispatcher(notifySettings, ntfyStore, ntfy, logger)
	flow.SetObserver(dispatcher)
	go dispatcher.Run(ctx)
	poller := notify.NewPoller(flow.Store(), cat, notifySettings, dispatcher, logger)
	if raw := os.Getenv("ALBUMREQ_NOTIFICATIONS_CHECK_EVERY"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			logger.Fatal().Msg("ALBUMREQ_NOTIFICATIONS_CHECK_EVERY must be a duration like 5m or 90s")
		}
		poller.SetEvery(d)
	}
	go poller.Run(ctx)

	// Live checks against Lidarr, remembered briefly: the nav asks on every page load.
	problems := issues.Combine(30*time.Second, service, authn, notifySettings)
	covers := coverart.New(coverURL)
	cat.SetPhotos(covers)
	api.New(cat, covers, service, authn, known, flow, problems, logger).
		Progress(cat).
		Artists(cat).
		Notifications(notifySettings, notify.NewUserSettings(ntfyStore, notifySettings, ntfy, logger)).
		Routes(mux)

	static, err := fs.Sub(staticFiles, "web/static")
	if err != nil {
		logger.Fatal().Err(err).Msg("embedded frontend assets missing")
	}
	mux.Handle("/", noCache(http.FileServer(http.FS(static))))
	if gate == nil {
		// The page loads authy's /auth/client.js to learn whether a login is in
		// place (it sets an attribute that shows the Sign out link). With no gate
		// there is no such route, and a missing script is a console error. An empty
		// script says the same thing, quietly: the attribute is never set.
		mux.HandleFunc("GET /auth/client.js", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
		})
	}

	httpServer := &http.Server{
		Addr:              addr,
		Handler:           gate.Wrap(mux), // a no-op with the login off
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		logger.Info().Msg(fmt.Sprintf("starting albumreq on %s, settings in %s", addr, dataDir))
		if gate == nil && !isLoopback(addr) {
			logger.Warn().Msg("listening beyond loopback with no login: anyone who can reach this port can search your Lidarr, request albums and change its settings" + readOnlyHint())
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

// readOnlyHint tells the operator how to stop the settings page being used by
// whoever can reach the port.
func readOnlyHint() string {
	if settings.ReadOnly() {
		return ""
	}
	return " (set ALBUMREQ_SETTINGS_READONLY=true to lock the settings page)"
}

// backupEvery is how often a consistent copy of the database is written for the
// backup job to pick up. A file-level copy of the live database can catch it
// mid-write, so restic should cover backup/, not albumreq.db.
const backupEvery = 6 * time.Hour

// backupLoop writes dataDir/backup/albumreq.db now and then every backupEvery,
// until ctx ends. A failed copy is logged and tried again next time.
func backupLoop(ctx context.Context, logger zerolog.Logger, database *db.DB, dataDir string) {
	dest := filepath.Join(dataDir, "backup", db.File)
	for {
		if err := database.Backup(ctx, dest); err != nil && ctx.Err() == nil {
			logger.Error().Err(err).Msg("could not write the database backup copy")
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backupEvery):
		}
	}
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
