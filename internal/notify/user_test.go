package notify

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/Miista/albumarr/internal/requests"
)

func userRig(t *testing.T) (*UserSettings, *rig) {
	t.Helper()
	r := newRig(t)
	r.st.DeletePrefs("sub-alice")
	return NewUserSettings(r.st, r.set, r.fs, zerolog.Nop()), r
}

func TestAPersonSeesWhetherNotificationsAreSetUpAndOnlyTheServersHost(t *testing.T) {
	u, r := userRig(t)
	v, err := u.View("sub-bob")
	if err != nil || !v.Available || v.Server != "ntfy.example.com" || v.Topic != "" || v.Suggested == "" {
		t.Fatalf("%+v %v", v, err)
	}
	if !reflect.DeepEqual(v.Types, typeNames(ForRequesters)) || len(v.EventTypes) != len(ForRequesters) {
		t.Errorf("%+v", v)
	}
	for _, e := range v.EventTypes {
		if e.ID == Pending || e.ID == AutoApproved {
			t.Errorf("a requester is not offered the approvers' events: %v", e.ID)
		}
	}
	r.set.Patch(context.Background(), Patch{Enabled: bp(false)})
	v, _ = u.View("sub-bob")
	if v.Available || v.Server != "" || v.Suggested != "" {
		t.Errorf("when notifications are off a person learns nothing about the server: %+v", v)
	}
}

func TestSuggestedTopicsCannotBeGuessed(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		s := SuggestTopic()
		if !ValidTopic(s) || !strings.HasPrefix(s, "albumarr-") || len(s) < 20 || seen[s] {
			t.Fatalf("%q", s)
		}
		seen[s] = true
	}
}

func TestSavingATopicAndChoosingEvents(t *testing.T) {
	u, r := userRig(t)
	v, err := u.Save("sub-bob", "bob-phone-7k2x", nil)
	if err != nil || v.Topic != "bob-phone-7k2x" || !reflect.DeepEqual(v.Types, typeNames(ForRequesters)) || v.Suggested != "" {
		t.Fatalf("a first save with no events chosen means all of them: %+v %v", v, err)
	}
	v, err = u.Save("sub-bob", "bob-phone-7k2x", []string{"declined"})
	if err != nil || !reflect.DeepEqual(v.Types, []string{"declined"}) {
		t.Fatalf("%+v %v", v, err)
	}
	v, _ = u.Save("sub-bob", "bob-tablet", nil)
	if !reflect.DeepEqual(v.Types, []string{"declined"}) {
		t.Errorf("leaving the events out keeps what was chosen: %v", v.Types)
	}
	// Choosing none is allowed: they just hear nothing.
	if v, err := u.Save("sub-bob", "bob-tablet", []string{}); err != nil || len(v.Types) != 0 {
		t.Errorf("%+v %v", v, err)
	}
	// It is really what the dispatcher uses.
	u.Save("sub-bob", "bob-tablet", []string{"approved"})
	r.d.Notify(Event{Type: Approved, Request: recordFor("sub-bob", "bob"), Actor: anna})
	if got := r.topics(t); !eq(got, []string{"approvers", "bob-tablet"}) {
		t.Errorf("%v", got)
	}
}

func recordFor(sub, name string) (rec requests.Record) {
	rec = record()
	rec.Requester.Sub, rec.Requester.Name = sub, name
	return rec
}

func TestSavingRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		topic string
		types []string
	}{
		"a topic with a slash":             {"a/b", nil},
		"a topic with a space":             {"my topic", nil},
		"a topic that is too long":         {strings.Repeat("a", 65), nil},
		"a topic with markup":              {"<script>", nil},
		"the shared channel's topic":       {"approvers", nil},
		"the shared topic in another case": {"APPROVERS", nil},
		"an event that does not exist":     {"bob-phone", []string{"aproved"}},
		"an event only approvers hear":     {"bob-phone", []string{"pending"}},
	} {
		u, _ := userRig(t)
		if _, err := u.Save("sub-bob", tc.topic, tc.types); !isValidation(err) {
			t.Errorf("%s: err = %v, want a validation error", name, err)
		}
		if _, found, _ := u.store.Prefs("sub-bob"); found {
			t.Errorf("%s: a refused save left settings behind", name)
		}
	}
}

func TestTwoPeopleCannotShareATopic(t *testing.T) {
	u, _ := userRig(t)
	u.Save("sub-bob", "bob-phone", nil)
	_, err := u.Save("sub-carol", "BOB-PHONE", nil)
	if !isValidation(err) {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "bob") {
		t.Errorf("the refusal must not say whose it is: %v", err)
	}
	if v, err := u.Save("sub-bob", "bob-phone", []string{"declined"}); err != nil || v.Topic != "bob-phone" {
		t.Errorf("keeping your own is not a clash: %v", err)
	}
}

func TestAnEmptyTopicRemovesTheSettingsEvenWhenNotificationsAreOff(t *testing.T) {
	u, r := userRig(t)
	u.Save("sub-bob", "bob-phone", nil)
	r.set.Patch(context.Background(), Patch{Enabled: bp(false)})
	if _, err := u.Save("sub-bob", "bob-phone-2", nil); !isValidation(err) {
		t.Errorf("setting a topic needs notifications to be set up: %v", err)
	}
	if _, err := u.Save("sub-bob", "  ", nil); err != nil {
		t.Errorf("removing is always allowed: %v", err)
	}
	if _, found, _ := u.store.Prefs("sub-bob"); found {
		t.Error("the settings should be gone")
	}
}

func TestAPersonsTestGoesOnlyToTheirOwnSavedTopic(t *testing.T) {
	u, r := userRig(t)
	if _, err := u.Test(context.Background(), "sub-bob"); !isValidation(err) {
		t.Errorf("no topic saved yet: %v", err)
	}
	u.Save("sub-bob", "bob-phone", nil)
	res, err := u.Test(context.Background(), "sub-bob")
	if err != nil || !res.OK {
		t.Fatalf("%+v %v", res, err)
	}
	got := r.fs.all()
	if len(got) != 1 || got[0].msg.Topic != "bob-phone" || got[0].srv.URL != "https://ntfy.example.com" {
		t.Errorf("%+v", got)
	}
	// Somebody else's test cannot reach bob's topic, and bob's cannot reach theirs.
	u.Save("sub-carol", "carol-phone", nil)
	u.Test(context.Background(), "sub-carol")
	if got := r.fs.all(); got[1].msg.Topic != "carol-phone" {
		t.Errorf("%+v", got[1].msg)
	}
}

func TestTestsAreLimitedToOneEveryFewSeconds(t *testing.T) {
	u, r := userRig(t)
	clock := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)
	u.now = func() time.Time { return clock }
	u.Save("sub-bob", "bob-phone", nil)
	u.Test(context.Background(), "sub-bob")
	if _, err := u.Test(context.Background(), "sub-bob"); !isValidation(err) || len(r.fs.all()) != 1 {
		t.Errorf("a second test at once: %v, sent %d", err, len(r.fs.all()))
	}
	u.Save("sub-carol", "carol-phone", nil)
	if _, err := u.Test(context.Background(), "sub-carol"); err != nil {
		t.Errorf("the limit is per person: %v", err)
	}
	clock = clock.Add(testEvery + time.Second)
	if _, err := u.Test(context.Background(), "sub-bob"); err != nil {
		t.Errorf("after the wait: %v", err)
	}
}

func TestAPersonsFailedTestShowsAFriendlyMessageWithNoAddress(t *testing.T) {
	u, r := userRig(t)
	u.Save("sub-bob", "bob-phone", nil)
	r.fs.err = errors.New("could not reach the ntfy server: the connection was refused")
	res, err := u.Test(context.Background(), "sub-bob")
	if err != nil || res.OK || !strings.Contains(res.Message, "connection was refused") {
		t.Fatalf("%+v %v", res, err)
	}
	for _, leaked := range []string{"ntfy.example.com", "tk_secret_value", "approvers"} {
		if strings.Contains(res.Message, leaked) {
			t.Errorf("a person's test leaks %q: %s", leaked, res.Message)
		}
	}
}
