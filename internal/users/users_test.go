package users

import (
	"sync"
	"testing"
	"time"

	"github.com/Miista/albumreq/internal/db"
)

func open(t *testing.T) (*Directory, string) {
	t.Helper()
	dir := t.TempDir()
	d, err := db.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return New(d), dir
}

func list(t *testing.T, d *Directory) []User {
	t.Helper()
	l, err := d.List()
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestTouchAddsAndRemembersAcrossRestarts(t *testing.T) {
	d, dir := open(t)
	if _, err := d.Touch("sub-1", "alice", "s@example.com", false); err != nil {
		t.Fatal(err)
	}
	other, err := db.Open(dir) // a second handle, as after a restart
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	u, ok := New(other).Get("sub-1")
	if !ok || u.Name != "alice" || u.Email != "s@example.com" || u.FirstSeen.IsZero() {
		t.Errorf("%+v %v", u, ok)
	}
}

func TestTouchRefusesAnEmptySubject(t *testing.T) {
	d, _ := open(t)
	if _, err := d.Touch("", "x", "", false); err == nil {
		t.Error("two people with no ID would be one entry")
	}
	if len(list(t, d)) != 0 {
		t.Error("nothing should have been added")
	}
}

func TestTouchWritesOnlyWhenSomethingChangedOrAnHourPassed(t *testing.T) {
	d, _ := open(t)
	clock := time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)
	d.now = func() time.Time { return clock }
	d.Touch("s", "alice", "", false)
	first, _ := d.Get("s")

	clock = clock.Add(10 * time.Minute)
	d.Touch("s", "alice", "", false)
	if u, _ := d.Get("s"); !u.LastSeen.Equal(first.LastSeen) {
		t.Error("a returning user within the hour must not be written again")
	}
	clock = clock.Add(time.Hour)
	d.Touch("s", "alice", "", false)
	if u, _ := d.Get("s"); !u.LastSeen.Equal(clock) {
		t.Errorf("last seen = %v, want %v", u.LastSeen, clock)
	}
	if u, _ := d.Get("s"); !u.FirstSeen.Equal(first.FirstSeen) {
		t.Errorf("first seen changed: %v -> %v", first.FirstSeen, u.FirstSeen)
	}
	d.Touch("s", "Sam G", "", false)
	if u, _ := d.Get("s"); u.Name != "Sam G" {
		t.Errorf("a changed name must be stored at once: %q", u.Name)
	}
	if len(list(t, d)) != 1 {
		t.Error("touching must not add a second entry")
	}
}

func TestApproverFlagSurvivesTouchesAndRestarts(t *testing.T) {
	d, dir := open(t)
	d.Touch("s", "alice", "", false)
	if err := d.SetApprover("s", true); err != nil {
		t.Fatal(err)
	}
	d.now = func() time.Time { return time.Now().Add(3 * time.Hour) }
	d.Touch("s", "alice", "", false) // an update path must not reset it
	other, _ := db.Open(dir)
	defer other.Close()
	if !New(other).IsApprover("s") {
		t.Error("the approver flag was lost")
	}
	if err := d.SetApprover("s", false); err != nil || d.IsApprover("s") {
		t.Errorf("removing it: %v", err)
	}
}

func TestSetApproverForSomeoneUnknownIsAnError(t *testing.T) {
	d, _ := open(t)
	d.Touch("s", "alice", "", false)
	if err := d.SetApprover("nobody", true); err != ErrNotFound {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
	if d.IsApprover("nobody") || d.IsApprover("") {
		t.Error("an unknown person is never an approver")
	}
}

func TestListIsSortedByName(t *testing.T) {
	d, _ := open(t)
	d.Touch("2", "bob", "", false)
	d.Touch("1", "alice", "", false)
	d.Touch("3", "alice", "", false)
	l := list(t, d)
	if len(l) != 3 || l[0].Sub != "1" || l[1].Sub != "3" || l[2].Sub != "2" {
		t.Errorf("%+v", l)
	}
}

func TestTwoSignInsAtOnceMakeOneUser(t *testing.T) {
	d, _ := open(t)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := d.Touch("same", "alice", "", false); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := len(list(t, d)); n != 1 {
		t.Errorf("%d entries for one person", n)
	}
}

func TestADatabaseFailureNeverGrantsApprover(t *testing.T) {
	d, _ := open(t)
	d.Touch("s", "alice", "", false)
	d.SetApprover("s", true)
	d.db.Close()
	if d.IsApprover("s") {
		t.Error("an approver check that cannot be answered must say no")
	}
	if _, err := d.List(); err == nil {
		t.Error("a failed read must be an error, not an empty list")
	}
}

func TestNamesWithSQLCharactersAreStoredVerbatim(t *testing.T) {
	d, _ := open(t)
	evil := "Robert'); DROP TABLE users;--"
	d.Touch("s", evil, "a'b@example.com", false)
	u, ok := d.Get("s")
	if !ok || u.Name != evil || u.Email != "a'b@example.com" {
		t.Errorf("%+v %v", u, ok)
	}
	if len(list(t, d)) != 1 {
		t.Error("the table should be intact")
	}
}
