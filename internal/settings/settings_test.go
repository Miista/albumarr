package settings

import (
	"strings"
	"testing"

	"github.com/Miista/albumarr/internal/store"
)

func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{KeyURL, KeyAPIKey, KeyRootFolder, KeyQualityProfileID} {
		t.Setenv(EnvVar(k), "")
	}
	t.Setenv(readOnlyEnv, "")
}

func TestEnvVarNames(t *testing.T) {
	for key, want := range map[string]string{
		KeyURL:              "ALBUMARR_SETTING_LIDARR_URL",
		KeyAPIKey:           "ALBUMARR_SETTING_LIDARR_API_KEY",
		KeyRootFolder:       "ALBUMARR_SETTING_LIDARR_ROOT_FOLDER",
		KeyQualityProfileID: "ALBUMARR_SETTING_LIDARR_QUALITY_PROFILE_ID",
	} {
		if got := EnvVar(key); got != want {
			t.Errorf("EnvVar(%q) = %q, want %q (the names existing deployments already use)", key, got, want)
		}
	}
}

func TestPersistedValuesApplyWhenNoEnvIsSet(t *testing.T) {
	clearEnv(t)
	p := store.Lidarr{URL: "http://a", APIKey: "k", RootFolder: "/music", QualityProfileID: 5}
	r := Resolve(p)
	if r.Lidarr != p || len(r.Locks) != 0 || len(r.Problems) != 0 {
		t.Errorf("got %+v locks=%v problems=%v", r.Lidarr, r.Locks, r.Problems)
	}
	for _, k := range []string{KeyURL, KeyAPIKey, KeyRootFolder, KeyQualityProfileID} {
		if !r.Editable(k) {
			t.Errorf("%s should be editable", k)
		}
	}
}

func TestEnvWinsAndIsLocked(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvVar(KeyRootFolder), "/from-env")
	t.Setenv(EnvVar(KeyQualityProfileID), "7")
	persisted := store.Lidarr{RootFolder: "/from-store", QualityProfileID: 5}
	r := Resolve(persisted)

	if r.Lidarr.RootFolder != "/from-env" || r.Lidarr.QualityProfileID != 7 {
		t.Errorf("env should win per field and only for the fields it sets: %+v", r.Lidarr)
	}
	if l := r.Locks[KeyRootFolder]; l.Reason != LockEnv || l.EnvVar != "ALBUMARR_SETTING_LIDARR_ROOT_FOLDER" {
		t.Errorf("root folder lock = %+v", l)
	}
	if r.Editable(KeyRootFolder) || r.Editable(KeyQualityProfileID) {
		t.Error("exactly the env-pinned fields must be locked")
	}
	if persisted.RootFolder != "/from-store" {
		t.Error("Resolve must never change the persisted copy: env values do not belong in the store")
	}
}

func TestAnEmptyEnvVarIsNotAnOverride(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvVar(KeyRootFolder), "")
	r := Resolve(store.Lidarr{RootFolder: "/music"})
	if r.Lidarr.RootFolder != "/music" || !r.Editable(KeyRootFolder) {
		t.Errorf("an empty env var must leave the field to the store: %+v locks=%v", r.Lidarr, r.Locks)
	}
}

func TestUnusableNumbersFailSafeAndAreReported(t *testing.T) {
	clearEnv(t)
	for _, bad := range []string{"abc", "0", "-3", "1.5", "5 5", "99999999999999999999"} {
		t.Setenv(EnvVar(KeyQualityProfileID), bad)
		t.Setenv(EnvVar(KeyRootFolder), "/music")
		r := Resolve(store.Lidarr{URL: "http://a", APIKey: "k", QualityProfileID: 5})
		if r.Lidarr.QualityProfileID != 0 {
			t.Errorf("%q: quality profile = %d, want 0", bad, r.Lidarr.QualityProfileID)
		}
		if r.RequestsEnabled() {
			t.Errorf("%q: an unusable profile ID must switch requesting off, not fall back to the stored value", bad)
		}
		if len(r.Problems) != 1 || !strings.Contains(r.Problems[0], "QUALITY_PROFILE_ID") {
			t.Errorf("%q: problems = %v", bad, r.Problems)
		}
		if r.Editable(KeyQualityProfileID) {
			t.Errorf("%q: the field is still pinned by env", bad)
		}
	}
}

func TestTheURLAndKeyAreLockedTogether(t *testing.T) {
	clearEnv(t)

	t.Setenv(EnvVar(KeyAPIKey), "pinned-key")
	r := Resolve(store.Lidarr{URL: "http://stored"})
	if l := r.Locks[KeyURL]; l.Reason != LockPaired || l.EnvVar != "ALBUMARR_SETTING_LIDARR_API_KEY" {
		t.Errorf("a pinned key must lock the URL: %+v", l)
	}
	if r.Editable(KeyURL) || r.Editable(KeyAPIKey) {
		t.Error("with the key pinned, neither the URL nor the key may be edited")
	}

	t.Setenv(EnvVar(KeyAPIKey), "")
	t.Setenv(EnvVar(KeyURL), "http://pinned")
	r = Resolve(store.Lidarr{APIKey: "stored-key"})
	if l := r.Locks[KeyAPIKey]; l.Reason != LockPaired || l.EnvVar != "ALBUMARR_SETTING_LIDARR_URL" {
		t.Errorf("a pinned URL must lock the key: %+v", l)
	}

	t.Setenv(EnvVar(KeyAPIKey), "pinned-key")
	r = Resolve(store.Lidarr{})
	if r.Locks[KeyURL].Reason != LockEnv || r.Locks[KeyAPIKey].Reason != LockEnv {
		t.Errorf("when both are pinned each is simply env-managed: %+v", r.Locks)
	}

	// Other fields are unaffected by the pairing.
	if !r.Editable(KeyRootFolder) {
		t.Error("the pairing must not lock unrelated fields")
	}
}

func TestReadOnlyLocksEverythingButKeepsEnvReasons(t *testing.T) {
	clearEnv(t)
	t.Setenv(readOnlyEnv, "true")
	t.Setenv(EnvVar(KeyRootFolder), "/pinned")
	r := Resolve(store.Lidarr{})
	for _, k := range []string{KeyURL, KeyAPIKey, KeyQualityProfileID} {
		if l := r.Locks[k]; l.Reason != LockReadOnly || l.EnvVar != readOnlyEnv {
			t.Errorf("%s lock = %+v, want read-only", k, l)
		}
	}
	if r.Locks[KeyRootFolder].Reason != LockEnv {
		t.Errorf("an env-pinned field keeps its own reason: %+v", r.Locks[KeyRootFolder])
	}
}

func TestReadOnlyOnlyForClearlyTrueValues(t *testing.T) {
	for v, want := range map[string]bool{"true": true, "1": true, "TRUE": true, "false": false, "0": false, "": false, "yes please": false, "maybe": false} {
		t.Setenv(readOnlyEnv, v)
		if got := ReadOnly(); got != want {
			t.Errorf("ALBUMARR_SETTINGS_READONLY=%q -> %v, want %v", v, got, want)
		}
	}
}

func TestConfiguredAndRequestsEnabled(t *testing.T) {
	clearEnv(t)
	full := store.Lidarr{URL: "http://a", APIKey: "k", RootFolder: "/music", QualityProfileID: 5}
	if r := Resolve(full); !r.Configured() || !r.RequestsEnabled() {
		t.Errorf("a complete configuration must be configured and enabled")
	}
	for name, mutate := range map[string]func(*store.Lidarr){
		"no URL":         func(l *store.Lidarr) { l.URL = "" },
		"no key":         func(l *store.Lidarr) { l.APIKey = "" },
		"no root folder": func(l *store.Lidarr) { l.RootFolder = "" },
		"no quality":     func(l *store.Lidarr) { l.QualityProfileID = 0 },
	} {
		l := full
		mutate(&l)
		if Resolve(l).RequestsEnabled() {
			t.Errorf("%s: requesting must be off", name)
		}
	}
	if r := Resolve(store.Lidarr{URL: "http://a"}); r.Configured() {
		t.Error("a URL without a key is not configured")
	}
}

func TestMaskSecretRevealsNothing(t *testing.T) {
	if MaskSecret("") != "" {
		t.Error("an empty secret stays empty so the UI can tell 'not set' from 'set'")
	}
	a, b := MaskSecret("short"), MaskSecret("a-very-long-api-key-0123456789abcdef")
	if a != b {
		t.Errorf("the mask must not leak the length: %q vs %q", a, b)
	}
	for _, secret := range []string{"abcdef123456", "k"} {
		m := MaskSecret(secret)
		for _, r := range secret {
			if strings.ContainsRune(m, r) {
				t.Errorf("the mask of %q contains %q", secret, string(r))
			}
		}
	}
}
