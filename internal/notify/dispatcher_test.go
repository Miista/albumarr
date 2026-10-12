package notify

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/Miista/albumreq/internal/requests"
)

type rig struct {
	d    *Dispatcher
	set  *Settings
	st   *Store
	fs   *fakeSender
	logs *bytes.Buffer
}

// newRig has notifications on, a shared channel for approvers that wants every
// event, and alice with her own topic.
func newRig(t *testing.T) *rig {
	t.Helper()
	clearEnv(t)
	st, _ := openStore(t)
	fs := &fakeSender{}
	logs := &bytes.Buffer{}
	log := zerolog.New(logs)
	set := NewSettings(st, fs, log)
	if _, err := set.Patch(context.Background(), Patch{
		Enabled: bp(true), URL: sp("https://ntfy.example.com"), Topic: sp("approvers"), Auth: sp("token"), Token: sp("tk_secret_value"),
		Priority: ip(4), Tags: sp("eyes"), AppURL: sp("http://albumreq.example:8768"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetPrefs("sub-alice", "alice-phone", ForRequesters); err != nil {
		t.Fatal(err)
	}
	return &rig{d: NewDispatcher(set, st, fs, log), set: set, st: st, fs: fs, logs: logs}
}

var (
	alice = requests.Person{Sub: "sub-alice", Name: "alice"}
	anna  = requests.Person{Sub: "sub-anna", Name: "anna"}
)

func record() requests.Record {
	return requests.Record{ID: 7, AlbumID: "b1392450-e666-3926-a536-22c65f834433", Title: "OK Computer", Artist: "Radiohead", Requester: alice}
}

func (r *rig) topics(t *testing.T) []string {
	t.Helper()
	r.d.drain(context.Background())
	var out []string
	for _, s := range r.fs.all() {
		out = append(out, s.msg.Topic)
	}
	return out
}

func eq(a, b []string) bool { return strings.Join(a, ",") == strings.Join(b, ",") }

func TestWhoHearsAboutWhat(t *testing.T) {
	for name, tc := range map[string]struct {
		e    Event
		want []string
	}{
		"a pending request goes to the shared channel only":        {Event{Type: Pending, Request: record(), Actor: alice}, []string{"approvers"}},
		"an automatic approval goes to the shared channel only":    {Event{Type: AutoApproved, Request: record(), Actor: alice}, []string{"approvers"}},
		"an approval goes to the shared channel and the requester": {Event{Type: Approved, Request: record(), Actor: anna}, []string{"approvers", "alice-phone"}},
		"a decline goes to both, with the reason":                  {Event{Type: Declined, Request: record(), Actor: anna}, []string{"approvers", "alice-phone"}},
		"availability has no actor and goes to both":               {Event{Type: Available, Request: record()}, []string{"approvers", "alice-phone"}},
		"a failed search goes to both":                             {Event{Type: Failed, Request: record(), Actor: anna}, []string{"approvers", "alice-phone"}},
		"nobody is told about their own action":                    {Event{Type: Approved, Request: record(), Actor: alice}, []string{"approvers"}},
	} {
		r := newRig(t)
		r.d.Notify(tc.e)
		if got := r.topics(t); !eq(got, tc.want) {
			t.Errorf("%s: sent to %v, want %v", name, got, tc.want)
		}
	}
}

func TestNothingIsSentWhenItIsOffOrHasNowhereToGo(t *testing.T) {
	for name, patch := range map[string]Patch{
		"switched off": {Enabled: bp(false)},
	} {
		r := newRig(t)
		r.set.Patch(context.Background(), patch)
		r.d.Notify(Event{Type: Approved, Request: record(), Actor: anna})
		if got := r.topics(t); len(got) != 0 {
			t.Errorf("%s: sent to %v", name, got)
		}
	}
	r := newRig(t)
	r.st.Update(func(c *Config) { c.URL = "" })
	r.d.Notify(Event{Type: Approved, Request: record(), Actor: anna})
	if got := r.topics(t); len(got) != 0 {
		t.Errorf("no server address: sent to %v", got)
	}
}

func TestTheSharedChannelOnlyGetsTheEventsItChose(t *testing.T) {
	r := newRig(t)
	r.set.Patch(context.Background(), Patch{Types: &[]string{"pending"}})
	r.d.Notify(Event{Type: Declined, Request: record(), Actor: anna})
	if got := r.topics(t); !eq(got, []string{"alice-phone"}) {
		t.Errorf("a declined request is not for the shared channel here: %v", got)
	}
	r.d.Notify(Event{Type: Pending, Request: record(), Actor: alice})
	if got := r.topics(t); !eq(got, []string{"alice-phone", "approvers"}) {
		t.Errorf("%v", got)
	}
	// And it can choose nothing at all.
	r.set.Patch(context.Background(), Patch{Types: &[]string{}})
	r.d.Notify(Event{Type: Pending, Request: record(), Actor: alice})
	if got := r.topics(t); len(got) != 2 {
		t.Errorf("a channel with no events chosen is sent nothing: %v", got)
	}
}

func TestARequesterOnlyGetsTheEventsTheyChose(t *testing.T) {
	r := newRig(t)
	r.st.SetPrefs("sub-alice", "alice-phone", []Type{Declined})
	r.d.Notify(Event{Type: Approved, Request: record(), Actor: anna})
	r.d.Notify(Event{Type: Available, Request: record()})
	if got := r.topics(t); !eq(got, []string{"approvers", "approvers"}) {
		t.Errorf("alice chose only declines, so she hears nothing else: %v", got)
	}
	r.d.Notify(Event{Type: Declined, Request: record(), Actor: anna})
	if got := r.topics(t); !eq(got, []string{"approvers", "approvers", "approvers", "alice-phone"}) {
		t.Errorf("%v", got)
	}
}

func TestAFailedSearchReachesSomeoneWhoOnlyAskedAboutApprovals(t *testing.T) {
	r := newRig(t)
	r.st.SetPrefs("sub-alice", "alice-phone", []Type{Approved})
	r.set.Patch(context.Background(), Patch{Types: &[]string{}})
	r.d.Notify(Event{Type: Failed, Request: record(), Actor: anna})
	if got := r.topics(t); !eq(got, []string{"alice-phone"}) {
		t.Errorf("the approval is the news, and the failed search is how it turned out: %v", got)
	}
	// ...but not someone who asked only about declines.
	r2 := newRig(t)
	r2.st.SetPrefs("sub-alice", "alice-phone", []Type{Declined})
	r2.set.Patch(context.Background(), Patch{Types: &[]string{}})
	r2.d.Notify(Event{Type: Failed, Request: record(), Actor: anna})
	if got := r2.topics(t); len(got) != 0 {
		t.Errorf("%v", got)
	}
}

func TestAPersonWithNoTopicHearsNothingAndATopicIsNotSentTwice(t *testing.T) {
	r := newRig(t)
	r.st.DeletePrefs("sub-alice")
	r.d.Notify(Event{Type: Approved, Request: record(), Actor: anna})
	if got := r.topics(t); !eq(got, []string{"approvers"}) {
		t.Errorf("%v", got)
	}
	// Defence in depth: a person whose topic somehow equals the shared one (any case) is not messaged twice.
	r.st.SetPrefs("sub-alice", "Approvers", ForRequesters)
	r.d.Notify(Event{Type: Approved, Request: record(), Actor: anna})
	if got := r.topics(t); len(got) != 2 {
		t.Errorf("the same topic must be sent once: %v", got)
	}
}

func TestMessagesCarryTheAgentsPriorityTagsLinksAndCredentials(t *testing.T) {
	r := newRig(t)
	r.set.Patch(context.Background(), Patch{EmbedPoster: bp(true)})
	r.d.Notify(Event{Type: Declined, Request: withReasonRec("Already own it"), Actor: anna})
	r.d.drain(context.Background())
	got := r.fs.all()
	if len(got) != 2 {
		t.Fatalf("%d messages", len(got))
	}
	for _, s := range got {
		if s.srv.URL != "https://ntfy.example.com" || s.srv.Auth != AuthToken || s.srv.Token != "tk_secret_value" {
			t.Errorf("server: %+v", s.srv)
		}
		if s.msg.Priority != 4 || len(s.msg.Tags) != 1 || s.msg.Tags[0] != "eyes" || !strings.HasPrefix(s.msg.Attach, "https://") {
			t.Errorf("message: %+v", s.msg)
		}
	}
	shared, mine := got[0].msg, got[1].msg
	if !strings.Contains(shared.Body, "anna declined alice's request") || !strings.Contains(mine.Body, "anna declined it") || !strings.Contains(mine.Body, "Reason: Already own it") {
		t.Errorf("each reads for its audience:\n%q\n%q", shared.Body, mine.Body)
	}
	if mine.Click != "http://albumreq.example:8768/#/requests" {
		t.Errorf("click %q", mine.Click)
	}
}

func withReasonRec(reason string) requests.Record {
	r := record()
	r.DeclineReason = reason
	return r
}

func TestADeadServerCostsNotificationsAndIsRecordedWithoutLeaking(t *testing.T) {
	r := newRig(t)
	r.fs.err = errors.New("could not reach the ntfy server: the connection was refused")
	r.d.Notify(Event{Type: Approved, Request: record(), Actor: anna})
	r.d.drain(context.Background())
	d := r.set.LastDelivery()
	if d == nil || d.OK || !strings.Contains(d.Message, "connection was refused") {
		t.Fatalf("%+v", d)
	}
	if v := r.set.View(); v.LastError == nil {
		t.Error("the page should show the failure")
	}
	for _, secret := range []string{"tk_secret_value", "approvers", "alice-phone", "ntfy.example.com"} {
		if strings.Contains(r.logs.String(), secret) {
			t.Errorf("the log leaks %q: %s", secret, r.logs.String())
		}
	}
	// A later success clears it.
	r.fs.err = nil
	r.d.Notify(Event{Type: Pending, Request: record(), Actor: alice})
	r.d.drain(context.Background())
	if r.set.View().LastError != nil {
		t.Error("a send that works clears the failure")
	}
}

func TestANotificationNeverMakesTheCallerWait(t *testing.T) {
	r := newRig(t)
	start := time.Now()
	for i := 0; i < queueSize*3; i++ { // far more than fits, and nothing is sending
		r.d.Notify(Event{Type: Pending, Request: record(), Actor: alice})
	}
	if time.Since(start) > time.Second {
		t.Errorf("queuing took %v: a full queue must drop, not block an approval", time.Since(start))
	}
	if !strings.Contains(r.logs.String(), "dropped") {
		t.Error("a dropped notification should be logged")
	}
}

func TestTheWorkerSendsInTheBackgroundAndStopsWithItsContext(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.d.Run(ctx); close(done) }()
	r.d.Notify(Event{Type: Pending, Request: record(), Actor: alice})
	deadline := time.Now().Add(2 * time.Second)
	for len(r.fs.all()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(r.fs.all()) != 1 {
		t.Fatal("nothing was sent by the worker")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Error("the worker did not stop")
	}
}

func TestObserveMapsTheFlowsEvents(t *testing.T) {
	for kind, want := range map[requests.EventKind]Type{
		requests.EventPending: Pending, requests.EventAutoApproved: AutoApproved, requests.EventApproved: Approved,
		requests.EventDeclined: Declined, requests.EventFailed: Failed,
	} {
		r := newRig(t)
		r.d.Observe(requests.Event{Kind: kind, Record: record(), Actor: anna})
		r.d.drain(context.Background())
		if len(r.fs.all()) == 0 || !strings.HasPrefix(r.fs.all()[0].msg.Title, titleStart(want)) {
			t.Errorf("%s: %+v", kind, r.fs.all())
		}
	}
	r := newRig(t)
	r.d.Observe(requests.Event{Kind: "something new", Record: record()})
	if got := r.topics(t); len(got) != 0 {
		t.Errorf("an event this version does not know sends nothing: %v", got)
	}
}

func titleStart(t Type) string {
	return map[Type]string{Pending: "New request", AutoApproved: "Approved automatically", Approved: "Approved:", Declined: "Declined", Failed: "Approved, but"}[t]
}
