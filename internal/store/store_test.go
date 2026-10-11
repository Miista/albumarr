package store

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestOpenCreatesAnEmptyDocumentWithOwnerOnlyPermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "data")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.View(); got != (State{}) {
		t.Errorf("a fresh store must be empty, got %+v", got)
	}
	info, err := os.Stat(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("config.json is %o, want 600: it holds an API key", perm)
	}
	if dinfo, _ := os.Stat(dir); dinfo.Mode().Perm() != 0o700 {
		t.Errorf("data dir is %o, want 700", dinfo.Mode().Perm())
	}
}

func TestUpdatePersistsAcrossOpen(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)
	want := Lidarr{URL: "http://lidarr:8686", APIKey: "k", RootFolder: "/music", QualityProfileID: 5, MetadataProfileID: 1}
	if err := s.Update(func(st *State) { st.Lidarr = want }); err != nil {
		t.Fatal(err)
	}
	again, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := again.View().Lidarr; got != want {
		t.Errorf("reopened store has %+v, want %+v", got, want)
	}
	info, _ := os.Stat(filepath.Join(dir, "config.json"))
	if info.Mode().Perm() != 0o600 {
		t.Errorf("after an update config.json is %o, want 600", info.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(dir, "config.json.tmp")); !os.IsNotExist(err) {
		t.Error("the temp file must not be left behind")
	}
}

func TestOpenRefusesACorruptFileInsteadOfOverwritingIt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"lidarr": {"url": `), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); err == nil || !strings.Contains(err.Error(), "parsing") {
		t.Errorf("err = %v, want a parse error", err)
	}
	if raw, _ := os.ReadFile(path); string(raw) != `{"lidarr": {"url": ` {
		t.Error("a corrupt file must be left for the operator to inspect, not rewritten")
	}
}

func TestOpenFailsWhenTheDataDirCannotBeCreated(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(filepath.Join(blocker, "data")); err == nil {
		t.Error("expected an error when the data dir sits under a regular file")
	}
}

func TestFailedSaveRollsBackMemory(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)
	if err := s.Update(func(st *State) { st.Lidarr.URL = "http://one" }); err != nil {
		t.Fatal(err)
	}
	// Make the next write impossible: a directory where the temp file must go.
	if err := os.Mkdir(filepath.Join(dir, "config.json.tmp"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(st *State) { st.Lidarr.URL = "http://two" }); err == nil {
		t.Fatal("expected the save to fail")
	}
	if got := s.View().Lidarr.URL; got != "http://one" {
		t.Errorf("memory says %q after a failed save, want the last saved value", got)
	}
}

func TestViewReturnsACopy(t *testing.T) {
	s, _ := Open(t.TempDir())
	v := s.View()
	v.Lidarr.URL = "mutated"
	if s.View().Lidarr.URL != "" {
		t.Error("changing the copy must not change the store")
	}
}

func TestConcurrentUpdatesAreSerialised(t *testing.T) {
	s, _ := Open(t.TempDir())
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
}
