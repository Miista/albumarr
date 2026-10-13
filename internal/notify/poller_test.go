package notify

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/Miista/albumarr/internal/requests"
)

type fakeRecords struct {
	recs      []requests.Record
	marked    []int64
	markErr   error
	listErr   error
	sinceSeen []int64
	limitSeen int
}

func (f *fakeRecords) AwaitingAvailability(since int64, limit int) ([]requests.Record, error) {
	f.sinceSeen = append(f.sinceSeen, since)
	f.limitSeen = limit
	var out []requests.Record
	for _, r := range f.recs {
		done := false
		for _, m := range f.marked {
			done = done || m == r.ID
		}
		if !done {
			out = append(out, r)
		}
	}
	return out, f.listErr
}

func (f *fakeRecords) MarkAvailableNotified(id int64) error {
	if f.markErr != nil {
		return f.markErr
	}
	f.marked = append(f.marked, id)
	return nil
}

type fakeLidarr struct {
	have  map[string]bool
	err   error
	calls []string
}

func (f *fakeLidarr) Available(_ context.Context, id string) (bool, error) {
	f.calls = append(f.calls, id)
	return f.have[id], f.err
}

func pollerRig(t *testing.T, recs ...requests.Record) (*Poller, *rig, *fakeRecords, *fakeLidarr) {
	t.Helper()
	r := newRig(t)
	fr := &fakeRecords{recs: recs}
	fl := &fakeLidarr{have: map[string]bool{}}
	return NewPoller(fr, fl, r.set, r.d, zerolog.Nop()), r, fr, fl
}

func approved(id int64, album string) requests.Record {
	rec := record()
	rec.ID, rec.AlbumID, rec.State = id, album, requests.Approved
	return rec
}

const (
	albumA = "b1392450-e666-3926-a536-22c65f834433"
	albumB = "48140466-cff6-3222-bd55-63c27e43190d"
)

func TestAnAlbumThatHasArrivedIsAnnouncedOnceToTheRequester(t *testing.T) {
	p, r, fr, fl := pollerRig(t, approved(1, albumA), approved(2, albumB))
	fl.have[albumA] = true
	told, err := p.Check(context.Background())
	if err != nil || told != 1 {
		t.Fatalf("told %d, %v", told, err)
	}
	if len(fr.marked) != 1 || fr.marked[0] != 1 {
		t.Errorf("marked %v: only the album that arrived", fr.marked)
	}
	if got := r.topics(t); !eq(got, []string{"approvers", "alice-phone"}) {
		t.Errorf("sent to %v", got)
	}
	// Another pass must not announce it again.
	if told, _ := p.Check(context.Background()); told != 0 {
		t.Errorf("announced again: %d", told)
	}
	if got := r.topics(t); len(got) != 2 {
		t.Errorf("%v", got)
	}
	// The second album arrives later and is announced then.
	fl.have[albumB] = true
	if told, _ := p.Check(context.Background()); told != 1 {
		t.Errorf("told %d", told)
	}
}

func TestNothingIsAskedOrMarkedWhileNotificationsAreOff(t *testing.T) {
	p, r, fr, fl := pollerRig(t, approved(1, albumA))
	fl.have[albumA] = true
	r.set.Patch(context.Background(), Patch{Enabled: bp(false)})
	if told, err := p.Check(context.Background()); told != 0 || err != nil || len(fl.calls) != 0 || len(fr.marked) != 0 || len(fr.sinceSeen) != 0 {
		t.Errorf("told=%d err=%v calls=%v marked=%v: it must do nothing, so the news is not lost", told, err, fl.calls, fr.marked)
	}
}

func TestLidarrNotAnsweringStopsThePassAndLosesNothing(t *testing.T) {
	p, _, fr, fl := pollerRig(t, approved(1, albumA), approved(2, albumB))
	fl.err = errors.New("Lidarr is down")
	told, err := p.Check(context.Background())
	if err == nil || told != 0 || len(fl.calls) != 1 || len(fr.marked) != 0 {
		t.Errorf("told=%d err=%v calls=%d marked=%v: stop at the first failure instead of asking again and again", told, err, len(fl.calls), fr.marked)
	}
}

func TestAFailureToMarkMeansNoMessage(t *testing.T) {
	p, r, fr, fl := pollerRig(t, approved(1, albumA))
	fl.have[albumA] = true
	fr.markErr = errors.New("database is locked")
	if told, err := p.Check(context.Background()); told != 0 || err == nil {
		t.Errorf("told=%d err=%v", told, err)
	}
	if got := r.topics(t); len(got) != 0 {
		t.Errorf("a message that could repeat on every pass must not be sent: %v", got)
	}
}

func TestOnlyRecentRequestsDecidedSinceNotificationsWereSwitchedOnAreChecked(t *testing.T) {
	p, r, fr, _ := pollerRig(t)
	fixed := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	p.now = func() time.Time { return fixed }
	enabledAt := r.st.Config().EnabledSince
	p.Check(context.Background())
	if len(fr.sinceSeen) != 1 || fr.sinceSeen[0] < enabledAt {
		t.Errorf("asked for requests decided since %v, but it was switched on at %d: the backlog must not be replayed", fr.sinceSeen, enabledAt)
	}
	if fr.limitSeen != perPass {
		t.Errorf("limit %d", fr.limitSeen)
	}
	// And never further back than the give-up window.
	r.st.Update(func(c *Config) { c.EnabledSince = 1 })
	p.Check(context.Background())
	if want := fixed.Add(-giveUpAfter).Unix(); fr.sinceSeen[1] != want {
		t.Errorf("since %d, want %d: an album still missing after 90 days is not coming", fr.sinceSeen[1], want)
	}
}

func TestAListFailureIsReported(t *testing.T) {
	p, _, fr, _ := pollerRig(t)
	fr.listErr = errors.New("db")
	if _, err := p.Check(context.Background()); err == nil {
		t.Error("a failure to read the requests is an error")
	}
}

func TestRunChecksOnTheIntervalItWasGivenAndStopsWithItsContext(t *testing.T) {
	p, _, fr, _ := pollerRig(t)
	p.first = 5 * time.Millisecond
	p.every = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	time.Sleep(300 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("the poller did not stop")
	}
	if n := len(fr.sinceSeen); n < 3 {
		t.Errorf("only %d checks in 300ms at a 20ms interval", n)
	}
}

func TestTheIntervalHasAFloorSoATypoCannotFloodLidarr(t *testing.T) {
	p, _, _, _ := pollerRig(t)
	if p.SetEvery(time.Millisecond); p.every != MinCheckEvery || p.first > MinCheckEvery {
		t.Errorf("every=%v first=%v", p.every, p.first)
	}
	if p.SetEvery(20 * time.Minute); p.every != 20*time.Minute || p.first != firstCheck {
		t.Errorf("every=%v first=%v", p.every, p.first)
	}
}
