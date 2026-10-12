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
	album3 = "59f47b29-50dd-4335-a81b-02297c790208"
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

// --- what the flow announces ---

type recorder struct {
	mu     sync.Mutex
	events []Event
}

func (r *recorder) Observe(e Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *recorder) kinds() []EventKind {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []EventKind
	for _, e := range r.events {
		out = append(out, e.Kind)
	}
	return out
}

func observed(t *testing.T) (*Service, *fakeCatalog, *recorder) {
	t.Helper()
	s, f, _ := rig(t)
	rec := &recorder{}
	s.SetObserver(rec)
	return s, f, rec
}

func kindsAre(got []EventKind, want ...EventKind) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestAPlainUsersRequestIsAnnouncedAsPendingByTheRequester(t *testing.T) {
	s, _, rec := observed(t)
	submit(t, s, alice, album1)
	if !kindsAre(rec.kinds(), EventPending) || rec.events[0].Actor.Sub != "sub-alice" || rec.events[0].Record.Title != "OK Computer" {
		t.Errorf("%+v", rec.events)
	}
	// A second click by anyone is not a new event: nothing new happened.
	submit(t, s, bob, album1)
	if len(rec.kinds()) != 1 {
		t.Errorf("%v", rec.kinds())
	}
}

func TestAnApproversOwnRequestIsAnnouncedAsAutomaticNotPendingThenApproved(t *testing.T) {
	s, _, rec := observed(t)
	submit(t, s, anna, album1)
	if !kindsAre(rec.kinds(), EventAutoApproved) || rec.events[0].Actor.Sub != "sub-anna" {
		t.Errorf("%v: it never waited, so it was never pending", rec.kinds())
	}
}

func TestApprovingSomeoneElsesRequestIsAnnouncedAsApprovedByTheApprover(t *testing.T) {
	s, _, rec := observed(t)
	r := submit(t, s, alice, album1).Record
	s.Approve(context.Background(), anna, r.ID)
	if !kindsAre(rec.kinds(), EventPending, EventApproved) || rec.events[1].Actor.Sub != "sub-anna" || rec.events[1].Record.Requester.Sub != "sub-alice" || rec.events[1].Result != "added" {
		t.Errorf("%+v", rec.events)
	}
	// An approver clicking Request on a pending album approves it the same way.
	submit(t, s, alice, album2)
	submit(t, s, root, album2)
	if got := rec.kinds(); !kindsAre(got, EventPending, EventApproved, EventPending, EventApproved) || rec.events[3].Actor.Sub != "sub-root" {
		t.Errorf("%v", got)
	}
}

func TestADeclineIsAnnouncedWithTheReasonAndTheDecider(t *testing.T) {
	s, _, rec := observed(t)
	r := submit(t, s, alice, album1).Record
	s.Decline(context.Background(), anna, r.ID, "Already own it")
	last := rec.events[len(rec.events)-1]
	if last.Kind != EventDeclined || last.Actor.Sub != "sub-anna" || last.Record.DeclineReason != "Already own it" {
		t.Errorf("%+v", last)
	}
}

func TestAnApprovalWhoseSearchCouldNotStartIsAnnouncedAsFailedInsteadOfApproved(t *testing.T) {
	s, f, rec := observed(t)
	f.result = catalog.RequestAddedUnsearched
	r := submit(t, s, alice, album1).Record
	s.Approve(context.Background(), anna, r.ID)
	if got := rec.kinds(); !kindsAre(got, EventPending, EventFailed) || rec.events[1].Result != "added_unsearched" {
		t.Errorf("%v: one announcement, not two: it stands for the approval", got)
	}
	// An approver's own request that cannot be searched is also announced as failed.
	submit(t, s, anna, album2)
	if got := rec.kinds(); got[len(got)-1] != EventFailed {
		t.Errorf("%v", got)
	}
}

func TestNothingIsAnnouncedForWhatDidNotHappen(t *testing.T) {
	s, f, rec := observed(t)
	f.wanted = false
	submit(t, s, alice, album1) // Lidarr already wants it: nothing recorded
	if len(rec.kinds()) != 0 {
		t.Errorf("%v", rec.kinds())
	}
	f.wanted = true
	f.reqErr = catalog.ErrUnavailable
	s.Submit(context.Background(), anna, askFor(album1)) // Lidarr refuses an approver's request
	if len(rec.kinds()) != 0 {
		t.Errorf("a failed approval is not an approval: %v", rec.kinds())
	}
	f.reqErr = nil
	r := submit(t, s, alice, album2).Record
	s.Approve(context.Background(), anna, r.ID)
	n := len(rec.kinds())
	s.Approve(context.Background(), root, r.ID)                         // already decided
	s.Decline(context.Background(), root, r.ID, "")                     // already decided
	s.Approve(context.Background(), alice, r.ID)                        // not allowed
	s.Submit(context.Background(), p(auth.None, "eve"), askFor(album2)) // not allowed
	if len(rec.kinds()) != n {
		t.Errorf("refused actions must announce nothing: %v", rec.kinds()[n:])
	}
}

func TestTheFlowWorksWithNoObserver(t *testing.T) {
	s, _, _ := rig(t)
	r := submit(t, s, alice, album1).Record
	if _, err := s.Approve(context.Background(), anna, r.ID); err != nil {
		t.Fatal(err)
	}
	s.SetObserver(nil)
	submit(t, s, anna, album2)
}

// --- the availability check's query ---

func TestAwaitingAvailabilityIsApprovedUnannouncedAndRecentOldestFirst(t *testing.T) {
	s, _, d := rig(t)
	st := s.Store()
	clock := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	st.now = func() time.Time { return clock }
	mk := func(album string, decide func(Record)) Record {
		r, _, _ := st.CreatePending(album, artist1, "t", "a", Person{Sub: "s", Name: "n"})
		decide(r)
		return r
	}
	old := mk("11111111-1111-4111-8111-111111111111", func(r Record) { st.Approve(r.ID, Person{Sub: "a"}, "added", false) })
	clock = clock.Add(2 * time.Hour)
	recent := mk("22222222-2222-4222-8222-222222222222", func(r Record) { st.Approve(r.ID, Person{Sub: "a"}, "added", false) })
	mk("33333333-3333-4333-8333-333333333333", func(r Record) { st.Decline(r.ID, Person{Sub: "a"}, "no") })
	mk("44444444-4444-4444-8444-444444444444", func(r Record) {}) // pending
	announced := mk("55555555-5555-4555-8555-555555555555", func(r Record) { st.Approve(r.ID, Person{Sub: "a"}, "added", false) })
	st.MarkAvailableNotified(announced.ID)

	got, err := st.AwaitingAvailability(0, 10)
	if err != nil || len(got) != 2 || got[0].ID != old.ID || got[1].ID != recent.ID {
		t.Fatalf("%v %v: approved, not yet announced, oldest first", ids(got), err)
	}
	since := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC).Unix()
	if got, _ := st.AwaitingAvailability(since, 10); len(got) != 1 || got[0].ID != recent.ID {
		t.Errorf("only the request decided at 11:00 is after a 10:00 cutoff, not the one decided at 09:00: %v", ids(got))
	}
	if got, _ := st.AwaitingAvailability(clock.Unix()+1, 10); len(got) != 0 {
		t.Errorf("nothing was decided after the last decision: %v", ids(got))
	}
	if got, _ := st.AwaitingAvailability(0, 1); len(got) != 1 {
		t.Errorf("the limit is honoured: %v", ids(got))
	}
	st.MarkAvailableNotified(old.ID)
	if got, _ := st.AwaitingAvailability(0, 10); len(got) != 1 || got[0].ID != recent.ID {
		t.Errorf("an announced request is not offered again: %v", ids(got))
	}
	d.Close()
}

func ids(l []Record) []int64 {
	var out []int64
	for _, r := range l {
		out = append(out, r.ID)
	}
	return out
}

func TestLastApprovedIsTheNewestApprovalOfThatAlbumOnly(t *testing.T) {
	s, _, _ := rig(t)
	if _, ok, err := s.LastApproved(album1); err != nil || ok {
		t.Fatalf("nothing approved yet: %v %v", ok, err)
	}
	submit(t, s, alice, album1) // pending
	if _, ok, _ := s.LastApproved(album1); ok {
		t.Error("a pending request is not an approval")
	}
	first := submit(t, s, anna, album2).Record
	other := submit(t, s, anna, album1).Record // approved at once
	if rec, ok, _ := s.LastApproved(album1); !ok || rec.ID != other.ID {
		t.Errorf("album1: %+v (ok %v), want %d", rec, ok, other.ID)
	}
	if rec, ok, _ := s.LastApproved(album2); !ok || rec.ID != first.ID {
		t.Errorf("album2: %+v (ok %v), want %d", rec, ok, first.ID)
	}
}

func TestAnAdminMayDeleteAnyRequestAndLidarrIsNotTouched(t *testing.T) {
	s, f, d := rig(t)
	pending := submit(t, s, alice, album1).Record
	approved := submit(t, s, anna, album2).Record
	before := f.calls()
	for _, id := range []int64{pending.ID, approved.ID} {
		if _, err := s.Delete(context.Background(), root, id); err != nil {
			t.Fatalf("delete %d: %v", id, err)
		}
	}
	if n := count(t, d, "1=1"); n != 0 {
		t.Errorf("%d records left", n)
	}
	if f.calls() != before {
		t.Error("deleting a record must not reach Lidarr")
	}
	if _, err := s.Delete(context.Background(), root, pending.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleting twice: %v", err)
	}
}

func TestAPersonMayOnlyTakeBackTheirOwnPendingRequest(t *testing.T) {
	s, _, d := rig(t)
	mine := submit(t, s, alice, album1).Record
	theirs := submit(t, s, bob, album2).Record
	if _, err := s.Delete(context.Background(), alice, theirs.ID); !errors.Is(err, ErrForbidden) {
		t.Errorf("someone else's pending request: %v", err)
	}
	if _, err := s.Delete(context.Background(), anna, theirs.ID); !errors.Is(err, ErrForbidden) {
		t.Errorf("an approver is not an admin: %v", err)
	}
	if n := count(t, d, "1=1"); n != 2 {
		t.Fatalf("refused deletes removed things: %d left", n)
	}
	if _, err := s.Delete(context.Background(), alice, mine.ID); err != nil {
		t.Fatalf("their own pending request: %v", err)
	}
	// It can be asked for again at once: the pending slot is free.
	if o := submit(t, s, alice, album1); o.Result != "pending" {
		t.Errorf("asking again after taking it back: %+v", o)
	}
	// Once decided it is history: the person can no longer remove it.
	decided := submit(t, s, anna, album3).Record
	if _, err := s.Delete(context.Background(), anna, decided.ID); !errors.Is(err, ErrForbidden) {
		t.Errorf("an approved request is history for non-admins: %v", err)
	}
}

func TestNobodyWithoutARoleMayDelete(t *testing.T) {
	s, _, _ := rig(t)
	rec := submit(t, s, alice, album1).Record
	if _, err := s.Delete(context.Background(), auth.Principal{Sub: "sub-alice", Name: "alice", Role: auth.None}, rec.ID); !errors.Is(err, ErrForbidden) {
		t.Errorf("no role: %v", err)
	}
}
