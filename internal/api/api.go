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
	"strings"
	"unicode/utf8"

	"github.com/rs/zerolog"

	"github.com/Miista/albumreq/internal/catalog"
	"github.com/Miista/albumreq/internal/coverart"
)

const (
	minQueryLen = 2
	maxQueryLen = 100
)

// Catalog is the part of the catalog the handlers use.
type Catalog interface {
	Search(ctx context.Context, term string) (catalog.SearchResult, error)
	Detail(ctx context.Context, id string) (catalog.Detail, error)
}

// CoverFetcher fetches an album's front cover at 250 or 500 pixels.
type CoverFetcher interface {
	Front(ctx context.Context, id string, size int) (data []byte, contentType string, err error)
}

// Server holds the handlers' dependencies.
type Server struct {
	catalog Catalog
	covers  CoverFetcher
	log     zerolog.Logger
}

// New returns a Server.
func New(c Catalog, covers CoverFetcher, log zerolog.Logger) *Server {
	return &Server{catalog: c, covers: covers, log: log}
}

// Routes registers the API on mux.
func (s *Server) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/search", s.handleSearch)
	mux.HandleFunc("GET /api/album/{id}", s.handleAlbum)
	mux.HandleFunc("GET /api/cover/{id}", s.handleCover)
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

	res, err := s.catalog.Search(r.Context(), q)
	if err != nil {
		if r.Context().Err() == nil {
			s.log.Error().Err(err).Msg(fmt.Sprintf("search for %q failed", q))
		}
		http.Error(w, "Search is unavailable", http.StatusBadGateway)
		return
	}

	writeJSON(w, map[string]any{"query": q, "albums": res.Albums, "libraryOk": res.LibraryOK})
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
