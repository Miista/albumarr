package requests

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Miista/albumreq/internal/auth"
	"github.com/Miista/albumreq/internal/catalog"
	"github.com/Miista/albumreq/internal/db"
)

const (
	album1 = "b1392450-e666-3926-a536-22c65f834433"
	album2 = "48140466-cff6-3222-bd55-63c27e43190d"
)

// artist1 is the artist ID the cards in these tests carry.
const artist1 = "a74b1b7f-71a5-4011-9441-d0b5e4122711"

type fakeCatalog struct {
	mu        sync.Mutex
	enabled   bool
	wanted    bool // would a request change anything in Lidarr
	wantedErr error
	result    catalog.RequestResult
	reqErr    error
	requested []string // album IDs sent to Lidarr
	artists   []string // and the artist ID each was sent with
	asked     atomic.Int32
}

func newFake() *fakeCatalog {
	return &fakeCatalog{enabled: true, wanted: true, result: catalog.RequestAdded}
}

func (f *fakeCatalog) RequestsEnabled() bool { return f.enabled }
func (f *fakeCatalog) Wanted(context.Context, string) (bool, error) {
	f.asked.Add(1)
	return f.wanted, f.wantedErr
}
func (f *fakeCatalog) Request(_ context.Context, album, artist string) (catalog.RequestResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.reqErr != nil {
		return "", f.reqErr
	}
	f.requested = append(f.requested, album)
	f.artists = append(f.artists, artist)
	return f.result, nil
}
func (f *fakeCatalog) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requested)
}

// askFor is what a card for the album carries.
func askFor(id string) Ask {
	return Ask{AlbumID: id, ArtistID: artist1, Title: "OK Computer", Artist: "Radiohead"}
}

func p(role auth.Role, name string) auth.Principal {
	return auth.Principal{Sub: "sub-" + name, Name: name, Role: role}
}

var (
	alice = p(auth.User, "alice")
	bob   = p(auth.User, "bob")
	anna  = p(auth.Approver, "anna")
	root  = p(auth.Admin, "root")
)

func rig(t *testing.T) (*Service, *fakeCatalog, *db.DB) {
	t.Helper()
	d, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	f := newFake()
	return NewService(NewStore(d), f), f, d
}

func submit(t *testing.T, s *Service, who auth.Principal, id string) Outcome {
	t.Helper()
	o, err := s.Submit(context.Background(), who, askFor(id))
	if err != nil {
		t.Fatalf("Submit as %s: %v", who.Name, err)
	}
	return o
}

func count(t *testing.T, d *db.DB, where string) int {
	t.Helper()
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM requests WHERE ` + where).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// --- submitting ---

func TestAPlainUsersRequestWaitsAndLidarrIsNotTouched(t *testing.T) {
	s, f, _ := rig(t)
	o := submit(t, s, alice, album1)
	if o.Result != "pending" || o.Record.State != Pending || o.Record.Requester.Sub != "sub-alice" || o.Record.Title != "OK Computer" || o.Record.Artist != "Radiohead" || o.Record.DecidedBy != nil {
		t.Errorf("%+v", o)
	}
	if f.calls() != 0 {
		t.Error("nothing may reach Lidarr until someone approves")
	}
}

func TestAnApproversOrAdminsRequestIsApprovedAtOnce(t *testing.T) {
	for _, who := range []auth.Principal{anna, root} {
		s, f, _ := rig(t)
		o := submit(t, s, who, album1)
		if o.Result != "added" || o.Record.State != Approved || !o.Record.AutoApproved || o.Record.DecidedBy == nil || o.Record.DecidedBy.Sub != who.Sub || o.Record.LidarrResult != "added" {
			t.Errorf("%s: %+v", who.Name, o)
		}
		if f.calls() != 1 || f.requested[0] != album1 {
			t.Errorf("%s: Lidarr was asked %v", who.Name, f.requested)
		}
	}
}

func TestTheSecondRequestForAPendingAlbumIsNotASecondRecord(t *testing.T) {
	s, f, d := rig(t)
	first := submit(t, s, alice, album1)
	for _, who := range []auth.Principal{alice, bob} {
		o := submit(t, s, who, album1)
		if o.Result != "pending" || o.Record.ID != first.Record.ID || o.Record.Requester.Sub != "sub-alice" {
			t.Errorf("%s: %+v, want the existing pending request", who.Name, o)
		}
	}
	if n := count(t, d, "1=1"); n != 1 || f.calls() != 0 {
		t.Errorf("%d records, %d Lidarr calls", n, f.calls())
	}
}

func TestAnApproversRequestForAPendingAlbumApprovesTheExistingOne(t *testing.T) {
	s, f, d := rig(t)
	first := submit(t, s, alice, album1)
	o := submit(t, s, anna, album1)
	if o.Record.ID != first.Record.ID || o.Record.State != Approved || o.Record.AutoApproved || o.Record.Requester.Sub != "sub-alice" || o.Record.DecidedBy.Sub != "sub-anna" {
		t.Errorf("%+v: alice asked, anna approved; auto means approving your own", o.Record)
	}
	if count(t, d, "1=1") != 1 || f.calls() != 1 {
		t.Error("one record, one Lidarr call")
	}
}

func TestNothingIsRecordedForAnAlbumLidarrAlreadyWants(t *testing.T) {
	s, f, d := rig(t)
	f.wanted = false // Lidarr is already looking for it
	o := submit(t, s, alice, album1)
	if o.Result != "exists" || count(t, d, "1=1") != 0 || f.calls() != 0 {
		t.Errorf("%+v records=%d calls=%d", o, count(t, d, "1=1"), f.calls())
	}
}

func TestSubmitRefusals(t *testing.T) {
	good := askFor(album1)
	with := func(f func(*Ask)) Ask { a := good; f(&a); return a }
	for name, tc := range map[string]struct {
		setup func(*fakeCatalog)
		who   auth.Principal
		ask   Ask
		want  error
	}{
		"requesting is off":          {func(f *fakeCatalog) { f.enabled = false }, alice, good, catalog.ErrRequestsDisabled},
		"an invalid album id":        {func(*fakeCatalog) {}, alice, with(func(a *Ask) { a.AlbumID = "../../etc/passwd" }), catalog.ErrInvalidID},
		"an empty album id":          {func(*fakeCatalog) {}, alice, with(func(a *Ask) { a.AlbumID = "" }), catalog.ErrInvalidID},
		"an invalid artist id":       {func(*fakeCatalog) {}, alice, with(func(a *Ask) { a.ArtistID = "x; DROP TABLE" }), catalog.ErrInvalidID},
		"an empty artist id":         {func(*fakeCatalog) {}, alice, with(func(a *Ask) { a.ArtistID = "" }), catalog.ErrInvalidID},
		"no title":                   {func(*fakeCatalog) {}, alice, with(func(a *Ask) { a.Title = "" }), ErrBadInput},
		"a title of only spaces":     {func(*fakeCatalog) {}, alice, with(func(a *Ask) { a.Title = "  \n\t " }), ErrBadInput},
		"no artist":                  {func(*fakeCatalog) {}, alice, with(func(a *Ask) { a.Artist = "" }), ErrBadInput},
		"someone with no access":     {func(*fakeCatalog) {}, p(auth.None, "eve"), good, ErrForbidden},
		"Lidarr cannot be asked":     {func(f *fakeCatalog) { f.wantedErr = catalog.ErrUnavailable }, alice, good, catalog.ErrUnavailable},
		"Lidarr refuses an approver": {func(f *fakeCatalog) { f.reqErr = catalog.ErrUnavailable }, anna, good, catalog.ErrUnavailable},
	} {
		s, f, d := rig(t)
		tc.setup(f)
		_, err := s.Submit(context.Background(), tc.who, tc.ask)
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
		if name != "Lidarr refuses an approver" {
			if n := count(t, d, "1=1"); n != 0 {
				t.Errorf("%s: %d records written for a refused request", name, n)
			}
		}
	}
}

func TestTheArtistIDTheRequestCarriedIsWhatApprovalSends(t *testing.T) {
	s, f, _ := rig(t)
	other := "5b11f4ce-a62d-471e-81fc-a69a8278c7da"
	ask := askFor(album1)
	ask.ArtistID = other
	o, err := s.Submit(context.Background(), alice, ask)
	if err != nil || o.Record.ArtistID != other {
		t.Fatalf("%+v %v: the record must keep the artist ID", o, err)
	}
	if f.calls() != 0 {
		t.Fatal("nothing reaches Lidarr until approved")
	}
	// Approval has only the record: it must use the stored artist ID.
	if _, err := s.Approve(context.Background(), anna, o.Record.ID); err != nil {
		t.Fatal(err)
	}
	if f.calls() != 1 || f.artists[0] != other || f.requested[0] != album1 {
		t.Errorf("Lidarr was sent album %v with artist %v, want %s with %s", f.requested, f.artists, album1, other)
	}
}

func TestTheNamesAreCleanedAndCutBeforeTheyAreStored(t *testing.T) {
	s, _, _ := rig(t)
	ask := askFor(album1)
	ask.Title, ask.Artist = "  OK\x00 Comp\x07uter \n", strings.Repeat("é", 500)
	o, err := s.Submit(context.Background(), alice, ask)
	if err != nil || o.Record.Title != "OK Computer" || len([]rune(o.Record.Artist)) != maxText {
		t.Errorf("%q / %d runes: %v", o.Record.Title, len([]rune(o.Record.Artist)), err)
	}
}

func TestWhenLidarrRefusesAnApproversRequestItStaysPendingToRetry(t *testing.T) {
	s, f, d := rig(t)
	f.reqErr = catalog.ErrUnavailable
	if _, err := s.Submit(context.Background(), anna, askFor(album1)); !errors.Is(err, catalog.ErrUnavailable) {
		t.Fatalf("err = %v", err)
	}
	if count(t, d, "state = 'pending'") != 1 {
		t.Fatal("the request should wait in the queue")
	}
	f.reqErr = nil
	o := submit(t, s, anna, album1) // clicking again approves the one that waits
	if o.Record.State != Approved || count(t, d, "1=1") != 1 {
		t.Errorf("%+v", o)
	}
}

// --- approving ---

func TestApprovingSendsTheAlbumToLidarrAndRecordsWhoAndWhat(t *testing.T) {
	s, f, _ := rig(t)
	rec := submit(t, s, alice, album1).Record
	f.result = catalog.RequestAddedUnsearched
	o, err := s.Approve(context.Background(), anna, rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if o.Result != "added_unsearched" || o.Record.State != Approved || o.Record.DecidedBy.Name != "anna" || o.Record.AutoApproved || o.Record.DecidedAt == nil || o.Record.LidarrResult != "added_unsearched" {
		t.Errorf("%+v", o)
	}
	if f.calls() != 1 {
		t.Errorf("%d Lidarr calls", f.calls())
	}
}

func TestOnlyApproversMayApproveOrDecline(t *testing.T) {
	s, f, _ := rig(t)
	rec := submit(t, s, alice, album1).Record
	for _, who := range []auth.Principal{alice, bob, p(auth.None, "eve")} {
		if _, err := s.Approve(context.Background(), who, rec.ID); !errors.Is(err, ErrForbidden) {
			t.Errorf("%s approving: %v", who.Name, err)
		}
		if _, err := s.Decline(context.Background(), who, rec.ID, "no"); !errors.Is(err, ErrForbidden) {
			t.Errorf("%s declining: %v", who.Name, err)
		}
	}
	if got, _ := s.Store().Get(rec.ID); got.State != Pending || f.calls() != 0 {
		t.Errorf("a refused decision changed something: %+v calls=%d", got, f.calls())
	}
}

func TestADecidedRequestCannotBeDecidedAgain(t *testing.T) {
	s, f, _ := rig(t)
	a := submit(t, s, alice, album1).Record
	b := submit(t, s, alice, album2).Record
	s.Approve(context.Background(), anna, a.ID)
	s.Decline(context.Background(), anna, b.ID, "not now")
	calls := f.calls()
	for _, id := range []int64{a.ID, b.ID} {
		if _, err := s.Approve(context.Background(), root, id); !errors.Is(err, ErrNotPending) {
			t.Errorf("approve %d: %v", id, err)
		}
		if _, err := s.Decline(context.Background(), root, id, "x"); !errors.Is(err, ErrNotPending) {
			t.Errorf("decline %d: %v", id, err)
		}
	}
	if f.calls() != calls {
		t.Error("deciding twice must not send anything to Lidarr again")
	}
	if got, _ := s.Store().Get(b.ID); got.DeclineReason != "not now" || got.DecidedBy.Name != "anna" {
		t.Errorf("the first decision must stand: %+v", got)
	}
}

func TestUnknownRequestIsNotFound(t *testing.T) {
	s, _, _ := rig(t)
	if _, err := s.Approve(context.Background(), anna, 999); !errors.Is(err, ErrNotFound) {
		t.Errorf("approve: %v", err)
	}
	if _, err := s.Decline(context.Background(), anna, 999, ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("decline: %v", err)
	}
}

func TestIfLidarrCannotAcceptItTheRequestStaysPending(t *testing.T) {
	s, f, _ := rig(t)
	rec := submit(t, s, alice, album1).Record
	f.reqErr = catalog.ErrUnavailable
	if _, err := s.Approve(context.Background(), anna, rec.ID); !errors.Is(err, catalog.ErrUnavailable) {
		t.Fatalf("err = %v", err)
	}
	if got, _ := s.Store().Get(rec.ID); got.State != Pending || got.DecidedBy != nil {
		t.Errorf("%+v", got)
	}
	f.reqErr = nil
	if o, err := s.Approve(context.Background(), anna, rec.ID); err != nil || o.Record.State != Approved {
		t.Errorf("retry: %+v %v", o, err)
	}
}

func TestApprovingWhileRequestingIsOffChangesNothing(t *testing.T) {
	s, f, _ := rig(t)
	rec := submit(t, s, alice, album1).Record
	f.enabled = false
	if _, err := s.Approve(context.Background(), anna, rec.ID); !errors.Is(err, catalog.ErrRequestsDisabled) {
		t.Errorf("err = %v", err)
	}
	if got, _ := s.Store().Get(rec.ID); got.State != Pending {
		t.Error("it must still be pending")
	}
}

// --- declining, and asking again ---

func TestDecliningKeepsLidarrOutAndCanBeRequestedAgain(t *testing.T) {
	s, f, d := rig(t)
	first := submit(t, s, alice, album1).Record
	o, err := s.Decline(context.Background(), anna, first.ID, "  Already have it on vinyl\n")
	if err != nil || o.Record.State != Declined || o.Record.DeclineReason != "Already have it on vinyl" || o.Record.DecidedBy.Name != "anna" || f.calls() != 0 {
		t.Fatalf("%+v %v calls=%d", o, err, f.calls())
	}
	again := submit(t, s, alice, album1)
	if again.Result != "pending" || again.Record.ID == first.ID || again.Record.State != Pending {
		t.Errorf("a declined album must be requestable again, as a new record: %+v", again)
	}
	if count(t, d, "album_id = '"+album1+"'") != 2 || count(t, d, "state = 'declined'") != 1 {
		t.Error("the declined record must stay as history")
	}
}

func TestAnApprovedRecordDoesNotBlockAskingAgainAfterTheAlbumLeavesLidarr(t *testing.T) {
	s, f, d := rig(t)
	submit(t, s, anna, album1) // approved
	// Lidarr no longer has it: the sheet offers Request again, and the detail says so.
	o := submit(t, s, alice, album1)
	if o.Result != "pending" || count(t, d, "album_id = '"+album1+"'") != 2 || f.calls() != 1 {
		t.Errorf("%+v", o)
	}
}

func TestDeclineReasonIsCleanedAndCut(t *testing.T) {
	long := strings.Repeat("é", 700)
	for in, want := range map[string]string{
		"":                    "",
		"  spaced  ":          "spaced",
		"bell\x07 and\x00nul": "bell andnul",
		long:                  strings.Repeat("é", maxReason),
	} {
		s, _, _ := rig(t)
		rec := submit(t, s, alice, album1).Record
		o, err := s.Decline(context.Background(), anna, rec.ID, in)
		if err != nil || o.Record.DeclineReason != want {
			t.Errorf("%q -> %q (%v), want %q", truncate(in), truncate(o.Record.DeclineReason), err, truncate(want))
		}
	}
}

func truncate(s string) string {
	if len(s) > 30 {
		return s[:30] + "…"
	}
	return s
}

func TestTextWithSQLCharactersIsStoredVerbatim(t *testing.T) {
	s, _, d := rig(t)
	ask := askFor(album1)
	ask.Title, ask.Artist = "Robert'); DROP TABLE requests;--", "<script>alert(1)</script>"
	o, err := s.Submit(context.Background(), p(auth.User, "O'Brien"), ask)
	if err != nil {
		t.Fatal(err)
	}
	rec := o.Record
	if rec.Title != "Robert'); DROP TABLE requests;--" || rec.Artist != "<script>alert(1)</script>" || rec.Requester.Name != "O'Brien" || count(t, d, "1=1") != 1 {
		t.Errorf("%+v", rec)
	}
}

// --- listing ---

func TestListScopesShowEachRoleOnlyWhatItMay(t *testing.T) {
	s, _, _ := rig(t)
	a := submit(t, s, alice, album1).Record
	time.Sleep(1100 * time.Millisecond) // created_at is in seconds: make the order observable
	b := submit(t, s, bob, album2).Record
	s.Decline(context.Background(), anna, b.ID, "no")

	mine, err := s.List(alice, ScopeMine)
	if err != nil || len(mine) != 1 || mine[0].ID != a.ID {
		t.Errorf("alice sees %+v %v: only her own", mine, err)
	}
	if q, err := s.List(anna, ScopePending); err != nil || len(q) != 1 || q[0].ID != a.ID {
		t.Errorf("the queue: %+v %v", q, err)
	}
	all, err := s.List(root, ScopeAll)
	if err != nil || len(all) != 2 || all[0].ID != b.ID {
		t.Errorf("all, newest first: %+v %v", all, err)
	}

	for name, tc := range map[string]struct {
		who   auth.Principal
		scope Scope
	}{
		"a user reading the queue":       {alice, ScopePending},
		"a user reading everything":      {alice, ScopeAll},
		"an approver reading everything": {anna, ScopeAll},
		"nobody reading anything":        {p(auth.None, "eve"), ScopeMine},
	} {
		if _, err := s.List(tc.who, tc.scope); !errors.Is(err, ErrForbidden) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := s.List(root, "bogus"); err == nil {
		t.Error("an unknown scope must be an error")
	}
}

func TestThePendingQueueIsOldestFirst(t *testing.T) {
	s, _, _ := rig(t)
	submit(t, s, alice, album1)
	time.Sleep(1100 * time.Millisecond)
	submit(t, s, bob, album2)
	q, _ := s.List(anna, ScopePending)
	if len(q) != 2 || q[0].AlbumID != album1 {
		t.Errorf("the request that has waited longest comes first: %+v", q)
	}
}

func TestPendingLookupForTheSearchOverlay(t *testing.T) {
	s, _, _ := rig(t)
	submit(t, s, alice, album1)
	rec2 := submit(t, s, bob, album2).Record
	s.Decline(context.Background(), anna, rec2.ID, "no")
	got, err := s.Store().Pending([]string{album1, album2, "00000000-0000-4000-8000-000000000404"})
	if err != nil || len(got) != 1 || got[album1].Requester.Sub != "sub-alice" {
		t.Errorf("%+v %v: only pending requests are waiting", got, err)
	}
	if got, err := s.Store().Pending(nil); err != nil || len(got) != 0 {
		t.Errorf("%+v %v", got, err)
	}
}

// --- concurrency ---

func TestManyPeopleRequestingTheSameAlbumAtOnceMakeOneRecord(t *testing.T) {
	s, f, d := rig(t)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := s.Submit(context.Background(), p(auth.User, string(rune('a'+i))), askFor(album1)); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if n := count(t, d, "state = 'pending'"); n != 1 || f.calls() != 0 {
		t.Errorf("%d pending, %d Lidarr calls", n, f.calls())
	}
}

func TestTheStoreItselfRefusesASecondPendingRecord(t *testing.T) {
	// Without the service's lock: the unique index is the real guarantee.
	_, _, d := rig(t)
	st := NewStore(d)
	var wg sync.WaitGroup
	var created atomic.Int32
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, ok, err := st.CreatePending(album1, artist1, "t", "a", Person{Sub: "s", Name: "n"})
			if err != nil {
				t.Error(err)
			}
			if ok {
				created.Add(1)
			}
		}()
	}
	wg.Wait()
	if created.Load() != 1 || count(t, d, "1=1") != 1 {
		t.Errorf("%d created, %d rows", created.Load(), count(t, d, "1=1"))
	}
}

func TestSeveralApproversAtOnceApproveItOnce(t *testing.T) {
	s, f, _ := rig(t)
	rec := submit(t, s, alice, album1).Record
	var wg sync.WaitGroup
	var ok, notPending atomic.Int32
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.Approve(context.Background(), p(auth.Approver, string(rune('a'+i))), rec.ID)
			switch {
			case err == nil:
				ok.Add(1)
			case errors.Is(err, ErrNotPending):
				notPending.Add(1)
			default:
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if ok.Load() != 1 || notPending.Load() != 5 || f.calls() != 1 {
		t.Errorf("approved %d, not pending %d, Lidarr calls %d: want 1, 5, 1", ok.Load(), notPending.Load(), f.calls())
	}
}

func TestApprovingAndDecliningAtOnceHasOneWinner(t *testing.T) {
	s, _, _ := rig(t)
	rec := submit(t, s, alice, album1).Record
	var wg sync.WaitGroup
	var wins atomic.Int32
	for _, decide := range []func() error{
		func() error { _, err := s.Approve(context.Background(), anna, rec.ID); return err },
		func() error { _, err := s.Decline(context.Background(), root, rec.ID, "no"); return err },
	} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := decide(); err == nil {
				wins.Add(1)
			} else if !errors.Is(err, ErrNotPending) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Errorf("%d decisions won", wins.Load())
	}
}

// --- the table's own rules ---

func TestTheDatabaseRefusesAnInvalidState(t *testing.T) {
	_, _, d := rig(t)
	if _, err := d.Exec(`INSERT INTO requests (album_id, artist_id, title, artist, state, requester_sub, requester_name, created_at) VALUES ('x','y','t','a','maybe','s','n',1)`); err == nil {
		t.Error("an unknown state must be refused by the schema, not trusted to the code")
	}
	if _, err := d.Exec(`INSERT INTO requests (album_id, artist_id, title, artist, state, requester_sub, requester_name, created_at) VALUES ('x','y','t','a','approved','s','n',1)`); err == nil {
		t.Error("a decided request must have a decision time")
	}
}
