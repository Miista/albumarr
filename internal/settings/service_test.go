package settings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/rs/zerolog"

	"github.com/Miista/albumreq/internal/catalog"
	"github.com/Miista/albumreq/internal/lidarr"
	"github.com/Miista/albumreq/internal/lidarrmeta"
	"github.com/Miista/albumreq/internal/musicbrainz"
	"github.com/Miista/albumreq/internal/store"
)

const (
	goodKey = "good-key-0123456789"
	someID  = "b1392450-e666-3926-a536-22c65f834433"
)

type nopSearch struct{}

func (nopSearch) Search(context.Context, string, int, int) (musicbrainz.Page, error) {
	return musicbrainz.Page{}, errors.New("unused")
}

type nopMeta struct{}

func (nopMeta) Album(context.Context, string) (lidarrmeta.Album, error) {
	return lidarrmeta.Album{}, errors.New("unused")
}

// fakeLidarr answers the endpoints the settings page uses and remembers every
// request, so a test can say which key reached which server.
type fakeLidarr struct {
	*httptest.Server
	mu   sync.Mutex
	keys []string // X-Api-Key of every request, in order
}

func newFakeLidarr(t *testing.T) *fakeLidarr {
	t.Helper()
	f := &fakeLidarr{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.keys = append(f.keys, r.Header.Get("X-Api-Key"))
		f.mu.Unlock()
		if r.Header.Get("X-Api-Key") != goodKey {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/v1/system/status":
			fmt.Fprint(w, `{"version":"3.1.0.4875"}`)
		case "/api/v1/rootfolder":
			fmt.Fprint(w, `[{"id":1,"path":"/music"},{"id":2,"path":"/data/music/lidarr"}]`)
		case "/api/v1/qualityprofile":
			fmt.Fprint(w, `[{"id":1,"name":"Any"},{"id":5,"name":"FLAC then 192+"}]`)
		case "/api/v1/metadataprofile":
			fmt.Fprint(w, `[{"id":1,"name":"Standard"}]`)
		default:
			fmt.Fprint(w, `[]`)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeLidarr) requests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.keys)
}

func (f *fakeLidarr) sawKey(k string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, got := range f.keys {
		if got == k {
			return true
		}
	}
	return false
}

type env struct {
	svc   *Service
	store *store.Store
	live  *lidarr.Live
	cat   *catalog.Catalog
	dir   string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	for _, k := range []string{KeyURL, KeyAPIKey, KeyRootFolder, KeyQualityProfileID, KeyMetadataProfileID} {
		t.Setenv(EnvVar(k), "")
	}
	t.Setenv(readOnlyEnv, "")
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	live := &lidarr.Live{}
	cat := catalog.New(nopSearch{}, nopMeta{}, live)
	e := &env{svc: NewService(st, live, cat, zerolog.Nop()), store: st, live: live, cat: cat, dir: dir}
	e.svc.Reload(context.Background())
	return e
}

func sp(s string) *string { return &s }
func ip(i int) *int       { return &i }

func (e *env) requestsOn() bool {
	_, err := e.cat.Request(context.Background(), someID)
	return !errors.Is(err, catalog.ErrRequestsDisabled)
}

func isValidation(err error) bool {
	var v *ValidationError
	return errors.As(err, &v)
}

func TestNothingIsConfiguredAtFirst(t *testing.T) {
	e := newEnv(t)
	v := e.svc.View()
	if v.Configured || v.RequestsEnabled || v.Values.URL != "" || v.Values.APIKeySet || e.live.Client() != nil || e.requestsOn() {
		t.Errorf("a fresh install must be unconfigured and read-only: %+v", v)
	}
}

func TestSavingTheConnectionConfiguresLidarrWithoutARestart(t *testing.T) {
	e := newEnv(t)
	f := newFakeLidarr(t)
	res, err := e.svc.Patch(context.Background(), Patch{URL: sp(f.URL), APIKey: sp(goodKey)})
	if err != nil || res.Warning != "" {
		t.Fatalf("err = %v warning = %q", err, res.Warning)
	}
	if !res.View.Configured || res.View.Values.URL != f.URL || !res.View.Values.APIKeySet || e.live.Client() == nil {
		t.Errorf("not configured after saving: %+v", res.View)
	}
	if got := e.store.View().Lidarr; got.URL != f.URL || got.APIKey != goodKey {
		t.Errorf("the store has %+v", got)
	}
	// And it survives a restart.
	again, _ := store.Open(e.dir)
	if again.View().Lidarr.APIKey != goodKey {
		t.Error("the key was not persisted")
	}
}

func TestViewNeverContainsTheSecret(t *testing.T) {
	e := newEnv(t)
	f := newFakeLidarr(t)
	if _, err := e.svc.Patch(context.Background(), Patch{URL: sp(f.URL), APIKey: sp(goodKey)}); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(e.svc.View())
	if strings.Contains(string(raw), goodKey) || strings.Contains(string(raw), goodKey[len(goodKey)-4:]) {
		t.Errorf("the view leaks the key (or its tail): %s", raw)
	}
	// Even when the key comes from the environment.
	t.Setenv(EnvVar(KeyAPIKey), "env-secret-ABCDEFGH")
	t.Setenv(EnvVar(KeyURL), f.URL)
	raw, _ = json.Marshal(e.svc.View())
	if strings.Contains(string(raw), "env-secret") || strings.Contains(string(raw), "EFGH") {
		t.Errorf("the view leaks an env-pinned key: %s", raw)
	}
}

func TestChangingTheURLRequiresTheKeyAgain(t *testing.T) {
	e := newEnv(t)
	f := newFakeLidarr(t)
	if _, err := e.svc.Patch(context.Background(), Patch{URL: sp(f.URL), APIKey: sp(goodKey)}); err != nil {
		t.Fatal(err)
	}
	attacker := newFakeLidarr(t)
	for name, p := range map[string]Patch{
		"no key at all":    {URL: sp(attacker.URL)},
		"an empty key":     {URL: sp(attacker.URL), APIKey: sp("")},
		"a blank key":      {URL: sp(attacker.URL), APIKey: sp("   ")},
		"url with a space": {URL: sp(attacker.URL + " "), APIKey: sp("")},
	} {
		_, err := e.svc.Patch(context.Background(), p)
		if !isValidation(err) {
			t.Errorf("%s: err = %v, want a validation error", name, err)
		}
	}
	if e.store.View().Lidarr.URL != f.URL {
		t.Error("the URL changed although no key was given")
	}
	if attacker.requests() != 0 {
		t.Errorf("the other address received %d requests: the saved key must never travel to it", attacker.requests())
	}
	// With a key typed in the same request the change is allowed.
	if _, err := e.svc.Patch(context.Background(), Patch{URL: sp(attacker.URL), APIKey: sp(goodKey)}); err != nil {
		t.Fatal(err)
	}
	if e.store.View().Lidarr.URL != attacker.URL {
		t.Error("the URL should change when the key is re-entered")
	}
}

func TestClearingTheURLClearsTheKey(t *testing.T) {
	e := newEnv(t)
	f := newFakeLidarr(t)
	e.svc.Patch(context.Background(), Patch{URL: sp(f.URL), APIKey: sp(goodKey)})
	res, err := e.svc.Patch(context.Background(), Patch{URL: sp("")})
	if err != nil {
		t.Fatal(err)
	}
	if got := e.store.View().Lidarr; got.URL != "" || got.APIKey != "" || res.View.Configured || e.live.Client() != nil {
		t.Errorf("clearing the URL must leave no key behind and switch Lidarr off: %+v", got)
	}
}

func TestRotatingTheKeyForTheSameURL(t *testing.T) {
	e := newEnv(t)
	f := newFakeLidarr(t)
	e.svc.Patch(context.Background(), Patch{URL: sp(f.URL), APIKey: sp(goodKey)})
	// An empty key means "leave it": the page never has the real one to send back.
	if _, err := e.svc.Patch(context.Background(), Patch{URL: sp(f.URL), APIKey: sp("")}); err != nil {
		t.Fatal(err)
	}
	if e.store.View().Lidarr.APIKey != goodKey {
		t.Error("an empty key must leave the saved key alone")
	}
	if _, err := e.svc.Patch(context.Background(), Patch{APIKey: sp("wrong-key")}); !isValidation(err) {
		t.Errorf("a key Lidarr rejects: err = %v", err)
	}
	if e.store.View().Lidarr.APIKey != goodKey {
		t.Error("a rejected key must not be saved")
	}
}

func TestBadURLsAreRefused(t *testing.T) {
	e := newEnv(t)
	for _, bad := range []string{
		"ftp://lidarr:8686", "lidarr:8686", "//lidarr:8686", "javascript:alert(1)", "file:///etc/passwd", "http://",
		"http://user:pass@lidarr:8686", "http://lidarr:8686/?x=1", "http://lidarr:8686/#f", "http://lid arr:8686",
		"http://" + strings.Repeat("a", 400), "http://[::1", "gopher://x",
	} {
		_, err := e.svc.Patch(context.Background(), Patch{URL: sp(bad), APIKey: sp(goodKey)})
		if !isValidation(err) {
			t.Errorf("%q: err = %v, want a validation error", bad, err)
		}
	}
	if e.store.View().Lidarr.URL != "" {
		t.Error("a refused URL must not be saved")
	}
}

func TestURLsAreNormalised(t *testing.T) {
	for in, want := range map[string]string{
		"http://lidarr:8686":              "http://lidarr:8686",
		" http://lidarr:8686/ ":           "http://lidarr:8686",
		"https://music.example/":          "https://music.example",
		"http://192.0.2.10:8686/lidarr/": "http://192.0.2.10:8686/lidarr",
	} {
		got, err := ValidateURL(in)
		if err != nil || got != want {
			t.Errorf("ValidateURL(%q) = %q, %v, want %q", in, got, err, want)
		}
	}
}

func TestAnUnreachableLidarrSavesWithAWarning(t *testing.T) {
	e := newEnv(t)
	f := newFakeLidarr(t)
	url := f.URL
	f.Close()
	res, err := e.svc.Patch(context.Background(), Patch{URL: sp(url), APIKey: sp(goodKey)})
	if err != nil || res.Warning == "" {
		t.Fatalf("err = %v warning = %q: an outage must not block saving, only warn", err, res.Warning)
	}
	if e.store.View().Lidarr.URL != url {
		t.Error("the settings should still have been saved")
	}
}

func TestRequestSettingsAreCheckedAgainstLidarr(t *testing.T) {
	e := newEnv(t)
	f := newFakeLidarr(t)
	e.svc.Patch(context.Background(), Patch{URL: sp(f.URL), APIKey: sp(goodKey)})
	if e.requestsOn() {
		t.Fatal("requesting must be off until its settings are given")
	}

	for name, p := range map[string]Patch{
		"a root folder Lidarr does not have": {RootFolder: sp("/nope"), QualityProfileID: ip(5), MetadataProfileID: ip(1)},
		"a quality profile that is missing":  {RootFolder: sp("/music"), QualityProfileID: ip(99), MetadataProfileID: ip(1)},
		"a metadata profile that is missing": {RootFolder: sp("/music"), QualityProfileID: ip(5), MetadataProfileID: ip(99)},
	} {
		_, err := e.svc.Patch(context.Background(), p)
		if !isValidation(err) {
			t.Errorf("%s: err = %v, want a validation error", name, err)
		}
		if e.store.View().Lidarr.RootFolder != "" || e.requestsOn() {
			t.Errorf("%s: nothing may be saved or switched on", name)
		}
	}

	res, err := e.svc.Patch(context.Background(), Patch{RootFolder: sp("/data/music/lidarr"), QualityProfileID: ip(5), MetadataProfileID: ip(1)})
	if err != nil || !res.View.RequestsEnabled || !e.requestsOn() {
		t.Errorf("valid settings must switch requesting on at once: err=%v view=%+v", err, res.View)
	}
	if _, err := e.svc.Patch(context.Background(), Patch{RootFolder: sp("")}); err != nil || e.requestsOn() {
		t.Errorf("clearing the root folder must switch requesting off: err=%v", err)
	}
}

func TestFieldValidation(t *testing.T) {
	e := newEnv(t)
	f := newFakeLidarr(t)
	e.svc.Patch(context.Background(), Patch{URL: sp(f.URL), APIKey: sp(goodKey)})
	for name, p := range map[string]Patch{
		"a relative root folder": {RootFolder: sp("music")},
		"a control character":    {RootFolder: sp("/mu\nsic")},
		"a huge root folder":     {RootFolder: sp("/" + strings.Repeat("a", 400))},
		"a negative quality id":  {QualityProfileID: ip(-1)},
		"a negative metadata id": {MetadataProfileID: ip(-5)},
		"a key with a space":     {APIKey: sp("abc def")},
		"a key with a newline":   {APIKey: sp("abc\ndef")},
		"a huge key":             {APIKey: sp(strings.Repeat("k", 300))},
	} {
		before := e.store.View()
		if _, err := e.svc.Patch(context.Background(), p); !isValidation(err) {
			t.Errorf("%s: err = %v, want a validation error", name, err)
		}
		if e.store.View() != before {
			t.Errorf("%s: a refused change was saved", name)
		}
	}
}

func TestLockedFieldsAreIgnoredAndNeverStored(t *testing.T) {
	e := newEnv(t)
	t.Setenv(EnvVar(KeyRootFolder), "/data/music/lidarr")
	f := newFakeLidarr(t)
	e.svc.Patch(context.Background(), Patch{URL: sp(f.URL), APIKey: sp(goodKey)})
	if _, err := e.svc.Patch(context.Background(), Patch{RootFolder: sp("/music"), QualityProfileID: ip(5), MetadataProfileID: ip(1)}); err != nil {
		t.Fatal(err)
	}
	st := e.store.View().Lidarr
	if st.RootFolder != "" {
		t.Errorf("a field pinned by env must not be written to the store, got %q", st.RootFolder)
	}
	if st.QualityProfileID != 5 || st.MetadataProfileID != 1 {
		t.Errorf("the unpinned fields should still save: %+v", st)
	}
	if got := e.svc.Resolved().Lidarr.RootFolder; got != "/data/music/lidarr" {
		t.Errorf("effective root folder = %q, want the pinned value", got)
	}
}

func TestAPinnedKeyLocksTheURLSoItCannotBeRedirected(t *testing.T) {
	e := newEnv(t)
	f := newFakeLidarr(t)
	t.Setenv(EnvVar(KeyAPIKey), goodKey)
	t.Setenv(EnvVar(KeyURL), "")
	e.store.Update(func(st *store.State) { st.Lidarr.URL = f.URL })
	e.svc.Reload(context.Background())

	attacker := newFakeLidarr(t)
	_, err := e.svc.Patch(context.Background(), Patch{URL: sp(attacker.URL), APIKey: sp("anything")})
	if err != nil {
		t.Fatal(err)
	}
	if got := e.store.View().Lidarr.URL; got != f.URL {
		t.Errorf("the URL moved to %q although the key is pinned by env", got)
	}
	e.svc.Reload(context.Background())
	if attacker.requests() != 0 || attacker.sawKey(goodKey) {
		t.Error("the pinned key reached another address")
	}
}

func TestReadOnlyRefusesEveryChange(t *testing.T) {
	e := newEnv(t)
	t.Setenv(readOnlyEnv, "true")
	f := newFakeLidarr(t)
	if _, err := e.svc.Patch(context.Background(), Patch{URL: sp(f.URL), APIKey: sp(goodKey)}); !errors.Is(err, ErrReadOnly) {
		t.Errorf("err = %v, want ErrReadOnly", err)
	}
	if e.store.View() != (store.State{}) {
		t.Error("a read-only page must not write")
	}
	if v := e.svc.View(); !v.ReadOnly {
		t.Error("the view must say the page is read-only")
	}
}

func TestEnvPinnedProblemsDisableRequestingAndAreReported(t *testing.T) {
	e := newEnv(t)
	f := newFakeLidarr(t)
	e.svc.Patch(context.Background(), Patch{URL: sp(f.URL), APIKey: sp(goodKey), RootFolder: sp("/music"), QualityProfileID: ip(5), MetadataProfileID: ip(1)})
	if !e.requestsOn() {
		t.Fatal("setup: requesting should be on")
	}
	t.Setenv(EnvVar(KeyQualityProfileID), "banana")
	e.svc.Reload(context.Background())
	if e.requestsOn() {
		t.Error("an unusable pinned profile must switch requesting off")
	}
	if v := e.svc.View(); len(v.Problems) != 1 || !strings.Contains(v.Problems[0], "QUALITY_PROFILE_ID") {
		t.Errorf("problems = %v", v.Problems)
	}
}

func TestReloadSwitchesRequestingOffWhenLidarrNoLongerHasTheFolder(t *testing.T) {
	e := newEnv(t)
	f := newFakeLidarr(t)
	e.store.Update(func(st *store.State) {
		st.Lidarr = store.Lidarr{URL: f.URL, APIKey: goodKey, RootFolder: "/gone", QualityProfileID: 5, MetadataProfileID: 1}
	})
	e.svc.Reload(context.Background())
	if e.requestsOn() {
		t.Error("a root folder Lidarr lacks must leave requesting off, and the process running so the page can fix it")
	}
	if e.live.Client() == nil {
		t.Error("Lidarr itself should still be connected for search and status")
	}
}

func TestTestUsesTheSavedKeyOnlyForTheSavedURL(t *testing.T) {
	e := newEnv(t)
	f := newFakeLidarr(t)
	e.svc.Patch(context.Background(), Patch{URL: sp(f.URL), APIKey: sp(goodKey)})
	other := newFakeLidarr(t)
	before := f.requests()

	r, err := e.svc.Test(context.Background(), "", "")
	if err != nil || !r.OK || r.Options == nil || len(r.Options.RootFolders) != 2 || r.Options.QualityProfiles[1].Name != "FLAC then 192+" {
		t.Fatalf("the saved connection should test fine and list options: %+v, %v", r, err)
	}
	if !f.sawKey(goodKey) || f.requests() == before {
		t.Error("the test did not use the saved key against the saved URL")
	}

	// A different address with no key typed in: refused before any request is made.
	if _, err := e.svc.Test(context.Background(), other.URL, ""); !isValidation(err) {
		t.Errorf("err = %v, want a validation error", err)
	}
	if other.requests() != 0 {
		t.Errorf("the saved key was sent to a different address (%d requests)", other.requests())
	}

	// With a key typed in, that key (and only that key) goes to the new address.
	r, err = e.svc.Test(context.Background(), other.URL, goodKey)
	if err != nil || !r.OK || other.requests() == 0 {
		t.Errorf("a typed key should be tried against the typed address: %+v, %v", r, err)
	}
	if e.store.View().Lidarr.URL != f.URL {
		t.Error("testing must never save anything")
	}
}

func TestTestReportsFailuresPlainly(t *testing.T) {
	e := newEnv(t)
	f := newFakeLidarr(t)
	r, err := e.svc.Test(context.Background(), f.URL, "wrong-key")
	if err != nil || r.OK || r.Message != "Lidarr rejected the API key." {
		t.Errorf("wrong key: %+v, %v", r, err)
	}
	dead := f.URL
	f.Close()
	r, err = e.svc.Test(context.Background(), dead, goodKey)
	if err != nil || r.OK || !strings.Contains(r.Message, "Could not reach Lidarr") || strings.Contains(r.Message, goodKey) {
		t.Errorf("unreachable: %+v, %v", r, err)
	}
	r, _ = e.svc.Test(context.Background(), "", "")
	if r.OK || r.Message == "" {
		t.Errorf("nothing to test: %+v", r)
	}
	if _, err := e.svc.Test(context.Background(), "ftp://x", goodKey); !isValidation(err) {
		t.Errorf("a bad URL: err = %v", err)
	}
}

func TestALockedConnectionIsTheOnlyOneThatCanBeTested(t *testing.T) {
	f := newFakeLidarr(t)
	probe := newFakeLidarr(t)

	for name, setup := range map[string]func(t *testing.T){
		"env pinned": func(t *testing.T) {
			t.Setenv(EnvVar(KeyURL), f.URL)
			t.Setenv(EnvVar(KeyAPIKey), goodKey)
		},
		"read-only": func(t *testing.T) { t.Setenv(readOnlyEnv, "true") },
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.store.Update(func(st *store.State) { st.Lidarr = store.Lidarr{URL: f.URL, APIKey: goodKey} })
			setup(t)
			before := probe.requests()
			r, err := e.svc.Test(context.Background(), probe.URL, goodKey)
			if err != nil || !r.OK {
				t.Fatalf("%+v, %v", r, err)
			}
			if probe.requests() != before {
				t.Error("a locked page was used to probe another address")
			}
		})
	}
}

func TestConcurrentChangesAreSerialised(t *testing.T) {
	e := newEnv(t)
	f := newFakeLidarr(t)
	e.svc.Patch(context.Background(), Patch{URL: sp(f.URL), APIKey: sp(goodKey)})
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			e.svc.Patch(context.Background(), Patch{QualityProfileID: ip(i%2*4 + 1)})
		}(i)
		go func() { defer wg.Done(); _ = e.svc.View() }()
	}
	wg.Wait()
	raw, err := os.ReadFile(filepath.Join(e.dir, "config.json"))
	if err != nil || !json.Valid(raw) {
		t.Errorf("config.json must stay valid JSON under concurrent changes: %v", err)
	}
}
