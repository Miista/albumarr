package notify

import (
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Miista/albumarr/internal/db"
)

func openStore(t *testing.T) (*Store, *db.DB) {
	t.Helper()
	d, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	s, err := NewStore(d)
	if err != nil {
		t.Fatal(err)
	}
	return s, d
}

func TestAFreshStoreIsOffWithSensibleDefaults(t *testing.T) {
	s, _ := openStore(t)
	c := s.Config()
	if c.Enabled || c.URL != "" || c.Topic != "" || c.Auth != AuthNone || c.Priority != 3 || c.EmbedPoster || c.AppURL != "" || c.EnabledSince != 0 {
		t.Errorf("%+v", c)
	}
	if !reflect.DeepEqual(c.Types, All) {
		t.Errorf("a fresh agent should offer every event: %v", c.Types)
	}
}

func TestUpdatePersistsAcrossReopening(t *testing.T) {
	s, d := openStore(t)
	err := s.Update(func(c *Config) {
		c.Enabled, c.URL, c.Topic, c.Tags, c.Auth = true, "https://ntfy.example.com", "albumarr", "eyes,warning", AuthToken
		c.Token, c.Priority, c.EmbedPoster, c.AppURL = "tk_secret", 4, true, "http://albumarr.example"
		c.Types = []Type{Pending, Declined}
	})
	if err != nil {
		t.Fatal(err)
	}
	again, err := NewStore(d)
	if err != nil {
		t.Fatal(err)
	}
	c := again.Config()
	if !c.Enabled || c.URL != "https://ntfy.example.com" || c.Topic != "albumarr" || c.Tags != "eyes,warning" || c.Auth != AuthToken || c.Token != "tk_secret" ||
		c.Priority != 4 || !c.EmbedPoster || c.AppURL != "http://albumarr.example" || !reflect.DeepEqual(c.Types, []Type{Pending, Declined}) {
		t.Errorf("%+v", c)
	}
}

func TestEnablingForTheFirstTimeStampsTheTimeAndLaterChangesKeepIt(t *testing.T) {
	s, _ := openStore(t)
	clock := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return clock }
	s.Update(func(c *Config) { c.URL = "https://n" })
	if s.Config().EnabledSince != 0 {
		t.Error("changing settings while off must not stamp the time")
	}
	s.Update(func(c *Config) { c.Enabled = true })
	first := s.Config().EnabledSince
	if first != clock.Unix() {
		t.Fatalf("enabled since %d, want %d", first, clock.Unix())
	}
	clock = clock.Add(48 * time.Hour)
	s.Update(func(c *Config) { c.Enabled = false })
	s.Update(func(c *Config) { c.Enabled = true })
	if s.Config().EnabledSince != first {
		t.Error("turning it off and on again must not move the date: the backlog from before is not new")
	}
}

func TestAFailedSaveLeavesTheSettingsAsTheyWere(t *testing.T) {
	s, d := openStore(t)
	s.Update(func(c *Config) { c.Topic = "kept" })
	d.Close()
	if err := s.Update(func(c *Config) { c.Topic = "lost"; c.Token = "lost" }); err == nil {
		t.Fatal("the save should fail")
	}
	if c := s.Config(); c.Topic != "kept" || c.Token != "" {
		t.Errorf("%+v", c)
	}
}

func TestConfigReturnsACopy(t *testing.T) {
	s, _ := openStore(t)
	c := s.Config()
	c.Types[0] = "tampered"
	c.Topic = "x"
	if s.Config().Types[0] == "tampered" || s.Config().Topic == "x" {
		t.Error("a caller changed the stored settings through a copy")
	}
}

func TestTheDatabaseRefusesBadAuthAndPriority(t *testing.T) {
	_, d := openStore(t)
	if _, err := d.Exec(`UPDATE ntfy SET auth = 'digest'`); err == nil {
		t.Error("an unknown auth method must be refused by the schema")
	}
	for _, p := range []int{0, 6, -1} {
		if _, err := d.Exec(`UPDATE ntfy SET priority = ?`, p); err == nil {
			t.Errorf("priority %d must be refused", p)
		}
	}
}

func TestPrefsRoundTripAndAreOwnedBySomeone(t *testing.T) {
	s, _ := openStore(t)
	if _, found, err := s.Prefs("nobody"); found || err != nil {
		t.Fatalf("%v %v", found, err)
	}
	if err := s.SetPrefs("alice", "alice-phone", []Type{Approved, Available}); err != nil {
		t.Fatal(err)
	}
	p, found, _ := s.Prefs("alice")
	if !found || p.Topic != "alice-phone" || !reflect.DeepEqual(p.Types, []Type{Approved, Available}) || p.UpdatedAt.IsZero() {
		t.Errorf("%+v", p)
	}
	if err := s.SetPrefs("alice", "alice-tablet", []Type{Declined}); err != nil {
		t.Fatal(err)
	}
	if p, _, _ := s.Prefs("alice"); p.Topic != "alice-tablet" || len(p.Types) != 1 {
		t.Errorf("an update must replace, not add: %+v", p)
	}
	if err := s.DeletePrefs("alice"); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := s.Prefs("alice"); found {
		t.Error("deleted prefs are gone")
	}
}

func TestATopicBelongsToOnePersonWhateverTheCase(t *testing.T) {
	s, _ := openStore(t)
	s.SetPrefs("alice", "Shared-Name", nil)
	for _, topic := range []string{"Shared-Name", "shared-name", "SHARED-NAME"} {
		if err := s.SetPrefs("bob", topic, nil); !errors.Is(err, ErrTopicTaken) {
			t.Errorf("%q: err = %v, want ErrTopicTaken", topic, err)
		}
	}
	if err := s.SetPrefs("alice", "shared-name", nil); err != nil {
		t.Errorf("keeping your own topic (even re-cased) is not a clash: %v", err)
	}
	if _, found, _ := s.Prefs("bob"); found {
		t.Error("a refused topic must leave nothing behind")
	}
}

func TestConcurrentUpdatesAreAllKept(t *testing.T) {
	s, _ := openStore(t)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Update(func(c *Config) { c.Tags += "x" })
		}()
	}
	wg.Wait()
	if n := strings.Count(s.Config().Tags, "x"); n != 20 {
		t.Errorf("kept %d of 20 updates", n)
	}
}

func TestTypeListsAreCanonicalAndStrict(t *testing.T) {
	got, err := ParseTypes([]string{"declined", "pending", " pending ", ""}, All)
	if err != nil || !reflect.DeepEqual(got, []Type{Pending, Declined}) {
		t.Errorf("%v %v: once each, in the canonical order, blanks ignored", got, err)
	}
	if _, err := ParseTypes([]string{"approved", "aproved"}, All); err == nil {
		t.Error("a typo must be an error, not a silent unsubscribe")
	}
	if _, err := ParseTypes([]string{"pending"}, ForRequesters); err == nil {
		t.Error("a requester cannot choose the approvers' event")
	}
	if got := decodeTypes("approved,bogus,approved,pending"); !reflect.DeepEqual(got, []Type{Approved, Pending}) {
		t.Errorf("an unknown stored name is ignored and duplicates collapse: %v", got)
	}
}
