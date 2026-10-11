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
	"github.com/Miista/albumreq/internal/issues"
	"github.com/Miista/albumreq/internal/lidarr"
	"github.com/Miista/albumreq/internal/requests"
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
}

// Requests is the approval flow: asking, deciding, listing, and what is waiting.
type Requests interface {
	Submit(ctx context.Context, p auth.Principal, a requests.Ask) (requests.Outcome, error)
	Approve(ctx context.Context, by auth.Principal, id int64) (requests.Outcome, error)
	Decline(ctx context.Context, by auth.Principal, id int64, reason string) (requests.Outcome, error)
	List(p auth.Principal, scope requests.Scope) ([]requests.Record, error)
	Pending(albumIDs []string) (map[string]requests.Record, error)
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
	List() ([]users.User, error)
	SetApprover(sub string, approver bool) error
}

// Issues is what is wrong, as the Issues page shows it. Fresh runs the checks
// again; Issues may answer from a recent run.
type Issues interface {
	Issues(ctx context.Context) []issues.Issue
	Fresh(ctx context.Context) []issues.Issue
}

// Server holds the handlers' dependencies.
type Server struct {
	catalog  Catalog
	covers   CoverFetcher
	settings Settings
	auth     Authenticator
	users    Users
	requests Requests
	issues   Issues
	log      zerolog.Logger
}

// New returns a Server.
func New(c Catalog, covers CoverFetcher, st Settings, a Authenticator, u Users, rq Requests, is Issues, log zerolog.Logger) *Server {
	return &Server{catalog: c, covers: covers, settings: st, auth: a, users: u, requests: rq, issues: is, log: log}
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
	// Any user may ask. What happens next depends on who: a user's request waits
	// for an approver, an approver's or admin's goes straight to Lidarr (which
	// spends indexer queries and starts a download). The service decides, not this
	// route, so the rule is in one place and tested there.
	mux.HandleFunc("POST /api/album/{id}/request", s.guard(auth.User, s.handleRequest))
	mux.HandleFunc("GET /api/requests", s.guard(auth.User, s.handleListRequests))
	mux.HandleFunc("POST /api/requests/{id}/approve", s.guard(auth.Approver, s.handleApprove))
	mux.HandleFunc("POST /api/requests/{id}/decline", s.guard(auth.Approver, s.handleDecline))
	mux.HandleFunc("GET /api/cover/{id}", s.guard(auth.User, s.handleCover))
	mux.HandleFunc("GET /api/settings", s.guard(auth.Admin, s.handleSettings))
	mux.HandleFunc("POST /api/settings", s.guard(auth.Admin, s.handleSaveSettings))
	mux.HandleFunc("POST /api/settings/test", s.guard(auth.Admin, s.handleTestSettings))
	mux.HandleFunc("GET /api/issues", s.guard(auth.Admin, s.handleIssues))
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

	s.markPending(res.Albums)
	writeJSON(w, map[string]any{"query": q, "offset": offset, "albums": res.Albums, "libraryOk": res.LibraryOK, "hasMore": res.HasMore})
}

// markPending shows a waiting request on the albums that have one. Lidarr's own
// state wins: an album it already knows keeps its chip. A failure to read the
// records only costs the chip; it must not break a search.
func (s *Server) markPending(albums []catalog.Album) {
	ids := make([]string, len(albums))
	for i, a := range albums {
		ids[i] = a.ID
	}
	pending, err := s.requests.Pending(ids)
	if err != nil {
		s.log.Warn().Err(err).Msg("could not read pending requests for the search results")
		return
	}
	for i := range albums {
		if _, ok := pending[albums[i].ID]; ok && albums[i].Status == "" {
			albums[i].Status = catalog.StatusPending
		}
	}
}

// albumDetail is the album sheet: the catalog's detail, and for approvers the
// request that is waiting, so they can decide from the sheet.
type albumDetail struct {
	catalog.Detail
	PendingRequest *requests.Record `json:"pendingRequest,omitempty"`
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
	out := albumDetail{Detail: d}
	if id := r.PathValue("id"); d.Status == "" {
		pending, err := s.requests.Pending([]string{id})
		if err != nil {
			s.log.Warn().Err(err).Msg("could not read the pending request for an album")
		} else if rec, ok := pending[id]; ok {
			out.Status, out.CanRequest = catalog.StatusPending, false
			if p, _ := s.auth.Principal(r); p.Role.AtLeast(auth.Approver) {
				out.PendingRequest = &rec
			}
		}
	}
	writeJSON(w, out)
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
	id, p := r.PathValue("id"), who(r)
	// What the card carried: the artist's ID and the names for the queue. Lidarr
	// works out the real artist from the album, so none of it can file an album under
	// the wrong artist; it is only validated for shape (see requests.Service.Submit).
	var body struct {
		ArtistID string `json:"artistId"`
		Title    string `json:"title"`
		Artist   string `json:"artist"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	out, err := s.requests.Submit(r.Context(), p, requests.Ask{AlbumID: id, ArtistID: body.ArtistID, Title: body.Title, Artist: body.Artist})
	if err != nil {
		s.requestFailed(w, r, err, "request for album "+id)
		return
	}
	s.log.Info().Str("by", p.Sub).Msg(fmt.Sprintf("%s requested album %s: %s", p.Name, id, out.Result))
	// Pending is 202: accepted, not yet done. Exists changed nothing. Anything else
	// is what Lidarr did on an approval, a 201 as before.
	status := http.StatusCreated
	switch out.Result {
	case string(requests.ResultPending):
		status = http.StatusAccepted
	case string(requests.ResultExists):
		status = http.StatusOK
	}
	writeOutcome(w, status, out)
}

// writeOutcome answers with what happened and, when there is one, the record.
func writeOutcome(w http.ResponseWriter, status int, out requests.Outcome) {
	body := map[string]any{"result": out.Result}
	if out.Record.ID != 0 {
		body["request"] = out.Record
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// requestFailed answers a failed request, approval or decline. Messages are
// fixed and say what a person can act on; Lidarr's own words (a bad root folder,
// a missing profile) and addresses stay in the log, where they belong.
func (s *Server) requestFailed(w http.ResponseWriter, r *http.Request, err error, what string) {
	var rejected *lidarr.RejectedError
	switch {
	case errors.Is(err, catalog.ErrInvalidID):
		http.Error(w, "invalid album or artist id", http.StatusBadRequest)
	case errors.Is(err, requests.ErrBadInput):
		http.Error(w, "A request needs the album's title and artist", http.StatusBadRequest)
	case errors.Is(err, requests.ErrForbidden):
		http.Error(w, "You do not have access to this", http.StatusForbidden)
	case errors.Is(err, requests.ErrNotFound):
		http.Error(w, "No such request", http.StatusNotFound)
	case errors.Is(err, requests.ErrNotPending):
		http.Error(w, "That request has already been decided", http.StatusConflict)
	case errors.Is(err, catalog.ErrRequestsDisabled):
		http.Error(w, "Requests are not enabled", http.StatusForbidden)
	case errors.Is(err, catalog.ErrNotInMetadata):
		http.Error(w, "Lidarr's metadata does not have this album yet", http.StatusUnprocessableEntity)
	case errors.Is(err, catalog.ErrUnavailable):
		s.log.Warn().Err(err).Msg(what + " failed: a service is unavailable")
		http.Error(w, "Lidarr or its metadata service is unavailable, try again later", http.StatusServiceUnavailable)
	case errors.Is(err, lidarr.ErrBadSettings):
		// Lidarr lacks something albumreq needs (the "None" metadata profile). The
		// details are on the Issues page, for an admin; a visitor is only told it is
		// not their doing.
		s.log.Error().Msg(fmt.Sprintf("%s: %s", what, err))
		http.Error(w, "Lidarr is missing something albumreq needs, so nothing was added. An admin can see what under Issues.", http.StatusBadGateway)
	case errors.As(err, &rejected):
		s.log.Error().Msg(fmt.Sprintf("%s: %s", what, rejected))
		http.Error(w, "Lidarr rejected the request; check albumreq's Lidarr settings", http.StatusBadGateway)
	default:
		if r.Context().Err() == nil {
			s.log.Error().Err(err).Msg(what + " failed")
		}
		http.Error(w, "Could not complete the request", http.StatusBadGateway)
	}
}

func (s *Server) handleListRequests(w http.ResponseWriter, r *http.Request) {
	scope := requests.Scope(r.URL.Query().Get("scope"))
	if scope == "" {
		scope = requests.ScopeMine
	}
	switch scope {
	case requests.ScopeMine, requests.ScopePending, requests.ScopeAll:
	default:
		http.Error(w, "scope must be mine, pending or all", http.StatusBadRequest)
		return
	}
	list, err := s.requests.List(who(r), scope)
	if err != nil {
		s.requestFailed(w, r, err, "listing requests")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, map[string]any{"scope": scope, "requests": list})
}

// requestID reads the numeric ID in the path; a record's ID is a positive integer.
func requestID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		http.Error(w, "invalid request id", http.StatusBadRequest)
		return 0, false
	}
	return id, true
}

func (s *Server) handleApprove(w http.ResponseWriter, r *http.Request) {
	if !writeGuard(w, r) {
		return
	}
	id, ok := requestID(w, r)
	if !ok {
		return
	}
	p := who(r)
	out, err := s.requests.Approve(r.Context(), p, id)
	if err != nil {
		s.requestFailed(w, r, err, fmt.Sprintf("approving request %d", id))
		return
	}
	s.log.Info().Str("by", p.Sub).Msg(fmt.Sprintf("%s approved request %d (%s): %s", p.Name, id, out.Record.AlbumID, out.Result))
	writeOutcome(w, http.StatusOK, out)
}

func (s *Server) handleDecline(w http.ResponseWriter, r *http.Request) {
	if !writeGuard(w, r) {
		return
	}
	id, ok := requestID(w, r)
	if !ok {
		return
	}
	var body struct {
		Reason string `json:"reason"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	p := who(r)
	out, err := s.requests.Decline(r.Context(), p, id, body.Reason)
	if err != nil {
		s.requestFailed(w, r, err, fmt.Sprintf("declining request %d", id))
		return
	}
	s.log.Info().Str("by", p.Sub).Msg(fmt.Sprintf("%s declined request %d (%s)", p.Name, id, out.Record.AlbumID))
	writeOutcome(w, http.StatusOK, out)
}

// maxSettingsBody bounds a small JSON write: a few short fields.
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
		URL              *string `json:"url"`
		APIKey           *string `json:"api_key"`
		RootFolder       *string `json:"root_folder"`
		QualityProfileID *int    `json:"quality_profile_id"`
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
		QualityProfileID: l.QualityProfileID,
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

// handleIssues lists what is wrong. ?fresh=1 runs the checks again now, which is
// what the page's "Check again" does; otherwise a recent answer will do.
func (s *Server) handleIssues(w http.ResponseWriter, r *http.Request) {
	var list []issues.Issue
	if r.URL.Query().Get("fresh") == "1" {
		list = s.issues.Fresh(r.Context())
	} else {
		list = s.issues.Issues(r.Context())
	}
	if list == nil {
		list = []issues.Issue{}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, map[string]any{"issues": list, "count": issues.Count(list)})
}

func (s *Server) handleUsers(w http.ResponseWriter, r *http.Request) {
	list, err := s.users.List()
	if err != nil {
		s.log.Error().Err(err).Msg("could not read the users list")
		http.Error(w, "Could not read the users", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, map[string]any{"users": list})
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
