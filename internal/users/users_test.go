package users

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func open(t *testing.T) (*Directory, string) {
	t.Helper()
	dir := t.TempDir()
	d, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return d, dir
}

func TestTouchAddsAndRemembersAcrossRestarts(t *testing.T) {
	d, dir := open(t)
	if _, err := d.Touch("sub-1", "alice", "s@example.com", false); err != nil {
		t.Fatal(err)
	}
	again, _ := Open(dir)
	u, ok := again.Get("sub-1")
	if !ok || u.Name != "alice" || u.Email != "s@example.com" || u.FirstSeen.IsZero() {
		t.Errorf("%+v %v", u, ok)
	}
	if info, _ := os.Stat(filepath.Join(dir, "users.json")); info.Mode().Perm() != 0o600 {
		t.Errorf("users.json is %o", info.Mode().Perm())
	}
}

func TestTouchRefusesAnEmptySubject(t *testing.T) {
	d, _ := open(t)
	if _, err := d.Touch("", "x", "", false); err == nil {
		t.Error("two people with no ID would be one entry")
	}
	if len(d.List()) != 0 {
		t.Error("nothing should have been added")
	}
}

func TestTouchWritesOnlyWhenSomethingChangedOrAnHourPassed(t *testing.T) {
	d, dir := open(t)
	clock := time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)
	d.now = func() time.Time { return clock }
	d.Touch("s", "alice", "", false)
	path := filepath.Join(dir, "users.json")
	before, _ := os.ReadFile(path)

	clock = clock.Add(10 * time.Minute)
	d.Touch("s", "alice", "", false)
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Error("a returning user within the hour must not rewrite the file")
	}
	clock = clock.Add(time.Hour)
	d.Touch("s", "alice", "", false)
	if u, _ := d.Get("s"); !u.LastSeen.Equal(clock) {
		t.Errorf("last seen = %v, want %v", u.LastSeen, clock)
	}
	d.Touch("s", "Sam G", "", false)
	if u, _ := d.Get("s"); u.Name != "Sam G" {
		t.Errorf("a changed name must be stored at once: %q", u.Name)
	}
	if len(d.List()) != 1 {
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
	again, _ := Open(dir)
	if !again.IsApprover("s") {
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
	l := d.List()
	if len(l) != 3 || l[0].Sub != "1" || l[1].Sub != "3" || l[2].Sub != "2" {
		t.Errorf("%+v", l)
	}
}
