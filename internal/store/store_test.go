package store

import (
	"sync"
	"testing"

	"github.com/Miista/albumarr/internal/db"
)

func open(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	d, err := db.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	s, err := New(d)
	if err != nil {
		t.Fatal(err)
	}
	return s, dir
}

func TestAFreshStoreIsEmpty(t *testing.T) {
	s, _ := open(t)
	if got := s.View(); got != (State{}) {
		t.Errorf("a fresh store must be empty, got %+v", got)
	}
}

func TestUpdatePersistsAcrossReopening(t *testing.T) {
	s, dir := open(t)
	want := Lidarr{URL: "http://lidarr:8686", APIKey: "k", RootFolder: "/music", QualityProfileID: 5}
	if err := s.Update(func(st *State) { st.Lidarr = want }); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(dir) // a second handle on the same file, as after a restart
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	again, err := New(d)
	if err != nil {
		t.Fatal(err)
	}
	if got := again.View().Lidarr; got != want {
		t.Errorf("reopened store has %+v, want %+v", got, want)
	}
}

func TestFailedSaveLeavesMemoryAsItWas(t *testing.T) {
	dir := t.TempDir()
	d, _ := db.Open(dir)
	s, _ := New(d)
	if err := s.Update(func(st *State) { st.Lidarr.URL = "http://one" }); err != nil {
		t.Fatal(err)
	}
	d.Close() // every later write fails
	if err := s.Update(func(st *State) { st.Lidarr.URL = "http://two" }); err == nil {
		t.Fatal("expected the save to fail")
	}
	if got := s.View().Lidarr.URL; got != "http://one" {
		t.Errorf("memory says %q after a failed save, want the last saved value", got)
	}
}

func TestViewReturnsACopy(t *testing.T) {
	s, _ := open(t)
	v := s.View()
	v.Lidarr.URL = "mutated"
	if s.View().Lidarr.URL != "" {
		t.Error("changing the copy must not change the store")
	}
}

func TestUpdateCannotSmuggleInAHalfChange(t *testing.T) {
	// fn runs on a copy: a change it makes but whose save fails is never seen.
	s, _ := open(t)
	s.Update(func(st *State) { st.Lidarr.URL = "http://kept" })
	s.db.Close()
	s.Update(func(st *State) { st.Lidarr.URL = "http://lost"; st.Lidarr.APIKey = "lost" })
	if got := s.View().Lidarr; got.URL != "http://kept" || got.APIKey != "" {
		t.Errorf("%+v", got)
	}
}

func TestConcurrentUpdatesAreSerialised(t *testing.T) {
	s, _ := open(t)
	var wg sync.WaitGroup
	for i := 1; i <= 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := s.Update(func(st *State) { st.Lidarr.QualityProfileID += i }); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if got := s.View().Lidarr.QualityProfileID; got != 210 {
		t.Errorf("sum = %d, want 210: an update was lost", got)
	}
	var inDB int
	s.db.QueryRow(`SELECT quality_profile_id FROM lidarr`).Scan(&inDB)
	if inDB != 210 {
		t.Errorf("the database has %d, want 210", inDB)
	}
}
