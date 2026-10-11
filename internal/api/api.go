// Package api implements albumreq's HTTP handlers: album search, one album's
// detail, the cover-art proxy, and a health probe. These handlers do no
// authentication themselves; until a login is added in main.go the server is
// meant for loopback or a proxy that authenticates for it.
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

	"github.com/Miista/albumreq/internal/catalog"
	"github.com/Miista/albumreq/internal/coverart"
	"github.com/Miista/albumreq/internal/lidarr"
	"github.com/Miista/albumreq/internal/settings"
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

// Server holds the handlers' dependencies.
type Server struct {
	catalog  Catalog
	covers   CoverFetcher
	settings Settings
	log      zerolog.Logger
}

// New returns a Server.
func New(c Catalog, covers CoverFetcher, st Settings, log zerolog.Logger) *Server {
	return &Server{catalog: c, covers: covers, settings: st, log: log}
}

// Routes registers the API on mux.
func (s *Server) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/search", s.handleSearch)
	mux.HandleFunc("GET /api/album/{id}", s.handleAlbum)
	mux.HandleFunc("POST /api/album/{id}/request", s.handleRequest)
	mux.HandleFunc("GET /api/cover/{id}", s.handleCover)
	mux.HandleFunc("GET /api/settings", s.handleSettings)
	mux.HandleFunc("POST /api/settings", s.handleSaveSettings)
	mux.HandleFunc("POST /api/settings/test", s.handleTestSettings)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})
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
