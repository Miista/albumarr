package requests

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/Miista/albumreq/internal/auth"
	"github.com/Miista/albumreq/internal/catalog"
	"github.com/Miista/albumreq/internal/musicbrainz"
)

// Fulfiller is the part of the catalog the flow uses: whether asking would change
// anything in Lidarr, and getting Lidarr to add, monitor and search for an album.
type Fulfiller interface {
	RequestsEnabled() bool
	Wanted(ctx context.Context, id string) (bool, error)
	Request(ctx context.Context, albumID, artistID string) (catalog.RequestResult, error)
}

// ErrBadInput means the request did not carry what a request needs.
var ErrBadInput = errors.New("requests: a request needs the album's and the artist's IDs, a title and an artist")

// Ask is what a request carries: the album and artist as the card showed them. The
// browser sends them; nothing here asks another service. Lidarr works out the real
// artist from the album, so a wrong artist ID cannot file an album under the wrong
// artist, and the title and artist are for the people reading the queue.
type Ask struct {
	AlbumID  string
	ArtistID string
	Title    string
	Artist   string
}

// ErrForbidden means the person's role may not do that.
var ErrForbidden = errors.New("requests: not allowed")

// Result of asking for an album.
type Result string

const (
	// ResultPending: recorded, waiting for an approver. Nothing was sent to Lidarr.
	ResultPending Result = "pending"
	// ResultExists: Lidarr already wants (or has) the album. Nothing was recorded.
	ResultExists Result = "exists"
)

// Outcome is what Submit, Approve and Decline did.
type Outcome struct {
	// Result is ResultPending, ResultExists, or, once approved, what Lidarr did:
	// "added" or "added_unsearched" (see catalog.RequestResult).
	Result string
	Record Record
}

// Service is the approval flow.
type Service struct {
	store *Store
	f     Fulfiller
	// mu serialises the flow. It is one process with a handful of users, and the
	// alternative, two approvers both calling Lidarr for one request, is worse than
	// a short wait. The database still enforces the rules; this keeps them tidy.
	mu sync.Mutex
}

// NewService returns the flow over a store and the catalog.
func NewService(store *Store, f Fulfiller) *Service { return &Service{store: store, f: f} }

// Store exposes the records.
func (s *Service) Store() *Store { return s.store }

// Pending returns the pending requests among the albums, by album ID.
func (s *Service) Pending(albumIDs []string) (map[string]Record, error) {
	return s.store.Pending(albumIDs)
}

func person(p auth.Principal) Person { return Person{Sub: p.Sub, Name: p.Name} }

// Submit is someone asking for an album.
//
// A plain user's request is recorded as pending and goes no further. An
// approver's or admin's is approved at once, since they could approve it
// themselves a second later; their own approval is marked automatic. If the album
// is already wanted in Lidarr nothing is recorded: the page already shows it as
// requested. A request for an album with a pending request is not a second one:
// a user is told it is pending, an approver's click approves the existing one.
//
// Errors are the catalog's (an invalid ID, requesting switched off, Lidarr
// unavailable, an album Lidarr's metadata does not know), so one mapping serves
// both.
func (s *Service) Submit(ctx context.Context, p auth.Principal, a Ask) (Outcome, error) {
	if !musicbrainz.ValidID(a.AlbumID) || !musicbrainz.ValidID(a.ArtistID) {
		return Outcome{}, catalog.ErrInvalidID
	}
	a.Title, a.Artist = clip(a.Title, maxText), clip(a.Artist, maxText)
	if a.Title == "" || a.Artist == "" {
		return Outcome{}, ErrBadInput
	}
	if !p.Role.AtLeast(auth.User) || p.Role == auth.None {
		return Outcome{}, ErrForbidden
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.f.RequestsEnabled() {
		return Outcome{}, catalog.ErrRequestsDisabled
	}

	if rec, found, err := s.store.PendingFor(a.AlbumID); err != nil {
		return Outcome{}, err
	} else if found {
		if p.Role.AtLeast(auth.Approver) {
			return s.approveLocked(ctx, rec, p)
		}
		return Outcome{Result: string(ResultPending), Record: rec}, nil
	}

	// Would a request change anything? Not if Lidarr already wants the album.
	wanted, err := s.f.Wanted(ctx, a.AlbumID)
	if err != nil {
		return Outcome{}, err
	}
	if !wanted {
		return Outcome{Result: string(ResultExists)}, nil
	}

	rec, _, err := s.store.CreatePending(a.AlbumID, a.ArtistID, a.Title, a.Artist, person(p))
	if err != nil {
		return Outcome{}, err
	}
	if p.Role.AtLeast(auth.Approver) {
		return s.approveLocked(ctx, rec, p)
	}
	return Outcome{Result: string(ResultPending), Record: rec}, nil
}

// Approve approves a pending request: Lidarr adds the album, monitors it and
// searches for it. If Lidarr cannot do it now the request stays pending, so the
// approver can try again, and the error says why.
func (s *Service) Approve(ctx context.Context, by auth.Principal, id int64) (Outcome, error) {
	if !by.Role.AtLeast(auth.Approver) {
		return Outcome{}, ErrForbidden
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.store.Get(id)
	if err != nil {
		return Outcome{}, err
	}
	if rec.State != Pending {
		return Outcome{}, ErrNotPending
	}
	if !s.f.RequestsEnabled() {
		return Outcome{}, catalog.ErrRequestsDisabled
	}
	return s.approveLocked(ctx, rec, by)
}

func (s *Service) approveLocked(ctx context.Context, rec Record, by auth.Principal) (Outcome, error) {
	res, err := s.f.Request(ctx, rec.AlbumID, rec.ArtistID)
	if err != nil {
		return Outcome{}, err // still pending
	}
	done, err := s.store.Approve(rec.ID, person(by), string(res), rec.Requester.Sub == by.Sub)
	if err != nil {
		return Outcome{}, err
	}
	return Outcome{Result: string(res), Record: done}, nil
}

// Decline declines a pending request, with a reason shown to the requester.
// Lidarr is not involved. The album can be requested again.
func (s *Service) Decline(_ context.Context, by auth.Principal, id int64, reason string) (Outcome, error) {
	if !by.Role.AtLeast(auth.Approver) {
		return Outcome{}, ErrForbidden
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.store.Decline(id, person(by), reason)
	if err != nil {
		return Outcome{}, err
	}
	return Outcome{Result: string(Declined), Record: rec}, nil
}

// Scope of a list: whose requests.
type Scope string

const (
	ScopeMine    Scope = "mine"    // the requester's own: any user
	ScopePending Scope = "pending" // the queue: approvers
	ScopeAll     Scope = "all"     // everything, with history: admins
)

// List returns requests in a scope. What a role may see is decided here, not by
// whoever calls: a user sees their own and nothing else.
func (s *Service) List(p auth.Principal, scope Scope) ([]Record, error) {
	switch scope {
	case ScopeMine:
		if !p.Role.AtLeast(auth.User) || p.Role == auth.None {
			return nil, ErrForbidden
		}
		return s.store.List(Filter{RequesterSub: p.Sub})
	case ScopePending:
		if !p.Role.AtLeast(auth.Approver) {
			return nil, ErrForbidden
		}
		return s.store.List(Filter{State: Pending})
	case ScopeAll:
		if !p.Role.AtLeast(auth.Admin) {
			return nil, ErrForbidden
		}
		return s.store.List(Filter{})
	}
	return nil, fmt.Errorf("requests: unknown scope %q", scope)
}
