// Package api implements albumreq's HTTP handlers: album search, one album's
// detail, requesting, settings, users, the cover-art proxy, and a health probe.
// Signing in is oidcgate's, wrapped around the whole server in main.go; what a
// signed-in person may do is decided here, per route, from their role (see
// internal/auth).
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/rs/zerolog"

	"github.com/Miista/albumreq/internal/auth"
	"github.com/Miista/albumreq/internal/catalog"
	"github.com/Miista/albumreq/internal/coverart"
	"github.com/Miista/albumreq/internal/lidarr"
	"github.com/Miista/albumreq/internal/settings"
	"github.com/Miista/albumreq/internal/users"
)

const (
	minQueryLen = 2
	maxQueryLen = 100
)

// Catalog is the part of the catalog the handlers use.
type Catalog interface {
	Search(ctx context.Context, term string, offset int) (catalog.SearchResult, error)
	Detail(ctx context.Context, id string) (catalog.Detail, error)
	Request(ctx context.Context, id string) (catalog.RequestResult, error)
}

// CoverFetcher fetches an album's front cover at 250 or 500 pixels.
type CoverFetcher interface {
	Front(ctx context.Context, id string, size int) (data []byte, contentType string, err error)
}

// Settings is the part of the settings service the handlers use.
type Settings interface {
	View() settings.View
	Patch(ctx context.Context, p settings.Patch) (settings.PatchResult, error)
	Test(ctx context.Context, url, key string) (settings.TestResult, error)
}

// Authenticator says who a request is from and what they may do.
type Authenticator interface {
	Principal(r *http.Request) (auth.Principal, bool)
}

// Users is the list of people who have signed in, and who is an approver.
type Users interface {
	List() []users.User
	SetApprover(sub string, approver bool) error
}

// Server holds the handlers' dependencies.
type Server struct {
	catalog  Catalog
	covers   CoverFetcher
	settings Settings
	auth     Authenticator
	users    Users
	log      zerolog.Logger
}

// New returns a Server.
func New(c Catalog, covers CoverFetcher, st Settings, a Authenticator, u Users, log zerolog.Logger) *Server {
	return &Server{catalog: c, covers: covers, settings: st, auth: a, users: u, log: log}
}

type principalKey struct{}

// guard runs h only for someone signed in who may do what min may. Nobody signed
// in is 401, so the page knows to sign in; signed in but not allowed is 403. The
// check is here, in the handler's path, and not in the page: a hidden button is
// not access control.
func (s *Server) guard(min auth.Role, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := s.auth.Principal(r)
		switch {
		case !ok:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
			return
		case !p.Role.AtLeast(min) || p.Role == auth.None:
			http.Error(w, "You do not have access to this", http.StatusForbidden)
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, p)))
	}
}

// who is the signed-in person behind a guarded request, for the log.
func who(r *http.Request) auth.Principal {
	p, _ := r.Context().Value(principalKey{}).(auth.Principal)
	return p
}

// Routes registers the API on mux.
func (s *Server) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/me", s.handleMe)
	mux.HandleFunc("GET /api/search", s.guard(auth.User, s.handleSearch))
	mux.HandleFunc("GET /api/album/{id}", s.guard(auth.User, s.handleAlbum))
	// Approver for now: a request goes straight to Lidarr, which spends indexer
	// queries and starts a download. A plain user's request will wait for approval
	// once requests are recorded; until then it is not theirs to make.
	mux.HandleFunc("POST /api/album/{id}/request", s.guard(auth.Approver, s.handleRequest))
	mux.HandleFunc("GET /api/cover/{id}", s.guard(auth.User, s.handleCover))
	mux.HandleFunc("GET /api/settings", s.guard(auth.Admin, s.handleSettings))
	mux.HandleFunc("POST /api/settings", s.guard(auth.Admin, s.handleSaveSettings))
	mux.HandleFunc("POST /api/settings/test", s.guard(auth.Admin, s.handleTestSettings))
	mux.HandleFunc("GET /api/users", s.guard(auth.Admin, s.handleUsers))
	mux.HandleFunc("POST /api/users/approver", s.guard(auth.Admin, s.handleSetApprover))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})
}

// handleMe tells the page who is signed in and what to show. It is open to anyone
// signed in, including someone who has no access, so the page can say so instead
// of failing every call; it says nothing about Lidarr beyond what a user sees anyway.
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	p, ok := s.auth.Principal(r)
	if !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
		return
	}
	me := map[string]any{"name": p.Name, "email": p.Email, "role": p.Role.String(), "local": p.Local}
	if p.Role.AtLeast(auth.User) {
		v := s.settings.View()
		me["lidarr_configured"] = v.Configured
		me["requests_enabled"] = v.RequestsEnabled
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, me)
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if n := utf8.RuneCountInString(q); n < minQueryLen || n > maxQueryLen {
		http.Error(w, fmt.Sprintf("q must be %d-%d characters", minQueryLen, maxQueryLen), http.StatusBadRequest)
		return
	}

	offset := 0
	if v := r.URL.Query().Get("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > catalog.MaxOffset {
			http.Error(w, fmt.Sprintf("offset must be a whole number from 0 to %d", catalog.MaxOffset), http.StatusBadRequest)
			return
		}
		offset = n
	}

	res, err := s.catalog.Search(r.Context(), q, offset)
	if err != nil {
		if r.Context().Err() == nil {
			s.log.Error().Err(err).Msg(fmt.Sprintf("search for %q failed", q))
		}
		http.Error(w, "Search is unavailable", http.StatusBadGateway)
		return
	}

	writeJSON(w, map[string]any{"query": q, "offset": offset, "albums": res.Albums, "libraryOk": res.LibraryOK, "hasMore": res.HasMore})
}

func (s *Server) handleAlbum(w http.ResponseWriter, r *http.Request) {
	d, err := s.catalog.Detail(r.Context(), r.PathValue("id"))
	if errors.Is(err, catalog.ErrInvalidID) {
		http.Error(w, "invalid album id", http.StatusBadRequest)
		return
	}
	if err != nil {
		s.log.Error().Err(err).Msg("album detail failed")
		http.Error(w, "Album details are unavailable", http.StatusBadGateway)
		return
	}
	writeJSON(w, d)
}

// writeGuard refuses a write that a browser sent on behalf of another site.
// albumreq has no login, so anything that can reach the port can write; what
// this prevents is a web page you merely visit making your browser do it. A
// custom header cannot be sent cross-origin without a CORS preflight, which
// this server never grants, and Sec-Fetch-Site says outright where a modern
// browser's request came from.
func writeGuard(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("X-Albumreq") != "1" {
		http.Error(w, "missing X-Albumreq header", http.StatusForbidden)
		return false
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		http.Error(w, "cross-site request refused", http.StatusForbidden)
		return false
	}
	return true
}

func (s *Server) handleRequest(w http.ResponseWriter, r *http.Request) {
	if !writeGuard(w, r) {
		return
	}
	id := r.PathValue("id")
	s.log.Info().Str("by", who(r).Sub).Msg(fmt.Sprintf("%s requests album %s", who(r).Name, id))
	result, err := s.catalog.Request(r.Context(), id)

	var rejected *lidarr.RejectedError
	switch {
	case err == nil:
		s.log.Info().Msg(fmt.Sprintf("request for album %s: %s", id, result))
		status := http.StatusOK
		if result != catalog.RequestExists {
			status = http.StatusCreated
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"result": result})
	case errors.Is(err, catalog.ErrInvalidID):
		http.Error(w, "invalid album id", http.StatusBadRequest)
	case errors.Is(err, catalog.ErrRequestsDisabled):
		http.Error(w, "Requests are not enabled", http.StatusForbidden)
	case errors.Is(err, catalog.ErrNotInMetadata):
		http.Error(w, "Lidarr's metadata does not have this album yet", http.StatusUnprocessableEntity)
	case errors.Is(err, catalog.ErrUnavailable):
		s.log.Warn().Err(err).Msg(fmt.Sprintf("request for album %s failed: a service is unavailable", id))
		http.Error(w, "Lidarr or its metadata service is unavailable, try again later", http.StatusServiceUnavailable)
	case errors.As(err, &rejected):
		// Lidarr's own words (a bad root folder, a missing profile) stay in the
		// log: they describe albumreq's settings, not something a visitor can fix.
		s.log.Error().Msg(fmt.Sprintf("request for album %s: %s", id, rejected))
		http.Error(w, "Lidarr rejected the request; check albumreq's Lidarr settings", http.StatusBadGateway)
	default:
		if r.Context().Err() == nil {
			s.log.Error().Err(err).Msg(fmt.Sprintf("request for album %s failed", id))
		}
		http.Error(w, "Could not add the album", http.StatusBadGateway)
	}
}

// maxSettingsBody bounds a settings request: five short fields.
const maxSettingsBody = 8 << 10

// decodeBody reads a small JSON body, refusing unknown fields and trailing data.
func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSettingsBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil || dec.More() {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return false
	}
	return true
}

// settingsFailure answers a failed settings call. Validation messages are written
// for the person at the page and are safe to show; anything else stays in the log.
func (s *Server) settingsFailure(w http.ResponseWriter, r *http.Request, err error) {
	var invalid *settings.ValidationError
	switch {
	case errors.Is(err, settings.ErrReadOnly):
		http.Error(w, "Settings are read-only", http.StatusForbidden)
	case errors.As(err, &invalid):
		http.Error(w, invalid.Message, http.StatusBadRequest)
	default:
		if r.Context().Err() == nil {
			s.log.Error().Err(err).Msg("settings call failed")
		}
		http.Error(w, "Could not save the settings", http.StatusInternalServerError)
	}
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, s.settings.View())
}

type settingsBody struct {
	Lidarr struct {
		URL               *string `json:"url"`
		APIKey            *string `json:"api_key"`
		RootFolder        *string `json:"root_folder"`
		QualityProfileID  *int    `json:"quality_profile_id"`
		MetadataProfileID *int    `json:"metadata_profile_id"`
	} `json:"lidarr"`
}

func (s *Server) handleSaveSettings(w http.ResponseWriter, r *http.Request) {
	if !writeGuard(w, r) {
		return
	}
	var body settingsBody
	if !decodeBody(w, r, &body) {
		return
	}
	l := body.Lidarr
	s.log.Info().Str("by", who(r).Sub).Msg(who(r).Name + " is changing the settings")
	res, err := s.settings.Patch(r.Context(), settings.Patch{
		URL: l.URL, APIKey: l.APIKey, RootFolder: l.RootFolder,
		QualityProfileID: l.QualityProfileID, MetadataProfileID: l.MetadataProfileID,
	})
	if err != nil {
		s.settingsFailure(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, map[string]any{"settings": res.View, "warning": res.Warning})
}

func (s *Server) handleTestSettings(w http.ResponseWriter, r *http.Request) {
	if !writeGuard(w, r) {
		return
	}
	var body struct {
		URL    string `json:"url"`
		APIKey string `json:"api_key"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	res, err := s.settings.Test(r.Context(), body.URL, body.APIKey)
	if err != nil {
		s.settingsFailure(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, res)
}

func (s *Server) handleUsers(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, map[string]any{"users": s.users.List()})
}

func (s *Server) handleSetApprover(w http.ResponseWriter, r *http.Request) {
	if !writeGuard(w, r) {
		return
	}
	var body struct {
		Sub      string `json:"sub"`
		Approver *bool  `json:"approver"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if body.Sub == "" || body.Approver == nil {
		http.Error(w, "sub and approver are required", http.StatusBadRequest)
		return
	}
	switch err := s.users.SetApprover(body.Sub, *body.Approver); {
	case errors.Is(err, users.ErrNotFound):
		http.Error(w, "No such user", http.StatusNotFound)
		return
	case err != nil:
		s.log.Error().Err(err).Msg("could not save an approver change")
		http.Error(w, "Could not save the change", http.StatusInternalServerError)
		return
	}
	s.log.Info().Str("by", who(r).Sub).Str("user", body.Sub).Bool("approver", *body.Approver).
		Msg(fmt.Sprintf("%s set approver=%v for %s", who(r).Name, *body.Approver, body.Sub))
	s.handleUsers(w, r)
}

func (s *Server) handleCover(w http.ResponseWriter, r *http.Request) {
	size := 250
	switch r.URL.Query().Get("size") {
	case "", "250":
	case "500":
		size = 500
	default:
		http.Error(w, "size must be 250 or 500", http.StatusBadRequest)
		return
	}

	data, contentType, err := s.covers.Front(r.Context(), r.PathValue("id"), size)
	switch {
	case errors.Is(err, coverart.ErrInvalid):
		http.Error(w, "invalid album id", http.StatusBadRequest)
		return
	case errors.Is(err, coverart.ErrNoCover):
		w.Header().Set("Cache-Control", "public, max-age=3600")
		http.Error(w, "no cover", http.StatusNotFound)
		return
	case err != nil:
		if r.Context().Err() == nil {
			s.log.Warn().Err(err).Msg("cover fetch failed")
		}
		http.Error(w, "cover unavailable", http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(data)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
