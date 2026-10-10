// Package api implements albumreq's HTTP handlers: album search, the cover-art
// proxy, and a health probe. These handlers do no authentication themselves;
// until a login is added in main.go the server is meant for loopback or a
// proxy that authenticates for it.
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
	"github.com/Miista/albumreq/internal/lidarr"
)

const (
	minQueryLen = 2
	maxQueryLen = 100
)

// Searcher finds albums for a search term.
type Searcher interface {
	Search(ctx context.Context, term string) ([]catalog.Album, error)
}

// CoverFetcher fetches cover art by its Lidarr cover-proxy path.
type CoverFetcher interface {
	Cover(ctx context.Context, path string) (data []byte, contentType string, err error)
}

// Server holds the handlers' dependencies.
type Server struct {
	search Searcher
	covers CoverFetcher
	log    zerolog.Logger
}

// New returns a Server.
func New(search Searcher, covers CoverFetcher, log zerolog.Logger) *Server {
	return &Server{search: search, covers: covers, log: log}
}

// Routes registers the API on mux.
func (s *Server) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/search", s.handleSearch)
	mux.HandleFunc("GET /api/cover", s.handleCover)
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

	albums, err := s.search.Search(r.Context(), q)
	if err != nil {
		s.log.Error().Err(err).Msg(fmt.Sprintf("search for %q failed", q))
		http.Error(w, "Lidarr is unavailable", http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"query": q, "albums": albums})
}

func (s *Server) handleCover(w http.ResponseWriter, r *http.Request) {
	data, contentType, err := s.covers.Cover(r.Context(), r.URL.Query().Get("p"))
	if errors.Is(err, lidarr.ErrInvalidCoverPath) {
		http.Error(w, "invalid cover path", http.StatusBadRequest)
		return
	}
	if err != nil {
		s.log.Warn().Err(err).Msg("cover fetch failed")
		http.Error(w, "cover unavailable", http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(data)
}
