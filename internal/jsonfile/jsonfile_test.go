package jsonfile

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

type doc struct {
	Names []string          `json:"names"`
	Tags  map[string]string `json:"tags"`
}

func TestOpenCreatesTheFileOwnerOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a", "b", "doc.json")
	if _, err := Open[doc](path); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("file is %o, want 600", info.Mode().Perm())
	}
	if info, _ := os.Stat(filepath.Dir(path)); info.Mode().Perm() != 0o700 {
		t.Errorf("dir is %o, want 700", info.Mode().Perm())
	}
}

func TestUpdatePersistsAndReloads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "doc.json")
	f, _ := Open[doc](path)
	if err := f.Update(func(d *doc) { d.Names = []string{"a", "b"}; d.Tags = map[string]string{"k": "v"} }); err != nil {
		t.Fatal(err)
	}
	g, err := Open[doc](path)
	if err != nil {
		t.Fatal(err)
	}
	if v := g.View(); len(v.Names) != 2 || v.Tags["k"] != "v" {
		t.Errorf("reloaded %+v", v)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Error("temp file left behind")
	}
}

func TestViewIsACopyNotTheStoredValue(t *testing.T) {
	f, _ := Open[doc](filepath.Join(t.TempDir(), "doc.json"))
	f.Update(func(d *doc) { d.Names = []string{"a"}; d.Tags = map[string]string{"k": "v"} })
	v := f.View()
	v.Names[0] = "changed"
	v.Tags["k"] = "changed"
	if got := f.View(); got.Names[0] != "a" || got.Tags["k"] != "v" {
		t.Errorf("a caller changed the stored value through View: %+v", got)
	}
}

func TestAFailedSaveRollsBackSlicesAndMapsToo(t *testing.T) {
	dir := t.TempDir()
	f, _ := Open[doc](filepath.Join(dir, "doc.json"))
	f.Update(func(d *doc) { d.Names = []string{"a"}; d.Tags = map[string]string{"k": "v"} })
	if err := os.Chmod(dir, 0o500); err != nil { // no new files: the save fails
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	err := f.Update(func(d *doc) { d.Names[0] = "lost"; d.Names = append(d.Names, "x"); d.Tags["k"] = "lost" })
	if err == nil {
		t.Fatal("the save should have failed")
	}
	if got := f.View(); len(got.Names) != 1 || got.Names[0] != "a" || got.Tags["k"] != "v" {
		t.Errorf("memory claims what the disk does not have: %+v", got)
	}
}

func TestACorruptFileIsAnErrorAndIsKept(t *testing.T) {
	path := filepath.Join(t.TempDir(), "doc.json")
	os.WriteFile(path, []byte(`{"names": [`), 0o600)
	if _, err := Open[doc](path); err == nil {
		t.Fatal("a corrupt file must be an error")
	}
	if raw, _ := os.ReadFile(path); string(raw) != `{"names": [` {
		t.Errorf("the corrupt file was overwritten: %q", raw)
	}
}

func TestConcurrentUpdatesAreAllKept(t *testing.T) {
	f, _ := Open[doc](filepath.Join(t.TempDir(), "doc.json"))
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); f.Update(func(d *doc) { d.Names = append(d.Names, "x") }) }()
		go func() { defer wg.Done(); _ = f.View() }()
	}
	wg.Wait()
	if n := len(f.View().Names); n != 30 {
		t.Errorf("kept %d of 30 updates", n)
	}
}
