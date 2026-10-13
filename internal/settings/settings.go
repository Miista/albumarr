// Package settings implements the env-var override convention reaparr uses:
// every field is pinned by exactly one env var, ALBUMREQ_SETTING_<KEY> (see
// EnvVar), derived from the field's key, for example "lidarr.api_key" ->
// ALBUMREQ_SETTING_LIDARR_API_KEY. If that env var is set and not empty it wins
// over the persisted store and the field is reported as managed, so the UI can
// render it read-only with the env var's name. Otherwise the persisted value
// applies and can be edited from the settings page.
//
// albumreq has no login, so on top of reaparr's convention there are two extra
// locks, both about not handing a secret to somewhere it should not go:
//
//   - The Lidarr URL and API key move together. If either is pinned by the
//     environment, both are locked, because a pinned key must never be sendable
//     to an editable URL.
//   - ALBUMREQ_SETTINGS_READONLY=true locks every field, for a deployment whose
//     port anyone can reach.
package settings

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/Miista/albumreq/internal/store"
)

const (
	envPrefix   = "ALBUMREQ_SETTING_"
	readOnlyEnv = "ALBUMREQ_SETTINGS_READONLY"
)

// The setting keys.
const (
	KeyURL              = "lidarr.url"
	KeyAPIKey           = "lidarr.api_key"
	KeyRootFolder       = "lidarr.root_folder"
	KeyQualityProfileID = "lidarr.quality_profile_id"
	KeyWebURL           = "lidarr.web_url"
)

// EnvVar returns the env var that pins the field with the given key.
func EnvVar(key string) string {
	return envPrefix + strings.ToUpper(strings.ReplaceAll(key, ".", "_"))
}

// ReadOnly reports whether the whole settings page is locked by the operator.
func ReadOnly() bool {
	v, err := strconv.ParseBool(os.Getenv(readOnlyEnv))
	return err == nil && v
}

// Why a field cannot be edited.
const (
	LockEnv      = "env"      // pinned by its own env var
	LockPaired   = "paired"   // the URL and the API key are locked together
	LockReadOnly = "readonly" // the whole page is locked
)

// Lock says why a field is locked and which env var is responsible.
type Lock struct {
	Reason string `json:"reason"`
	EnvVar string `json:"env_var"`
}

// Resolved is the effective Lidarr settings: persisted values with any env
// overrides applied, plus what is locked and why.
type Resolved struct {
	Lidarr store.Lidarr
	// Locks has an entry for every field that cannot be edited.
	Locks map[string]Lock
	// Problems are env vars whose value could not be used, for the log and the UI.
	Problems []string
}

// Resolve merges persisted settings with any active ALBUMREQ_SETTING_* env
// overrides. The persisted value passed in is never changed.
func Resolve(persisted store.Lidarr) Resolved {
	r := Resolved{Lidarr: persisted, Locks: map[string]Lock{}}

	env := func(key string) (string, bool) {
		if v, ok := os.LookupEnv(EnvVar(key)); ok && v != "" {
			r.Locks[key] = Lock{Reason: LockEnv, EnvVar: EnvVar(key)}
			return v, true
		}
		return "", false
	}
	envInt := func(key string, dst *int) {
		v, ok := env(key)
		if !ok {
			return
		}
		// An unusable number fails safe: it becomes 0, which switches requesting off.
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil || n < 1 {
			r.Problems = append(r.Problems, fmt.Sprintf("%s is not a positive whole number", EnvVar(key)))
			n = 0
		}
		*dst = n
	}

	if v, ok := env(KeyURL); ok {
		r.Lidarr.URL = strings.TrimSpace(v)
	}
	if v, ok := env(KeyAPIKey); ok {
		r.Lidarr.APIKey = strings.TrimSpace(v)
	}
	if v, ok := env(KeyRootFolder); ok {
		r.Lidarr.RootFolder = strings.TrimSpace(v)
	}
	envInt(KeyQualityProfileID, &r.Lidarr.QualityProfileID)
	if v, ok := env(KeyWebURL); ok {
		r.Lidarr.WebURL = strings.TrimRight(strings.TrimSpace(v), "/")
	}

	// The URL and key travel as a pair.
	urlLock, keyLock := r.Locks[KeyURL], r.Locks[KeyAPIKey]
	switch {
	case urlLock.Reason == LockEnv && keyLock.Reason == "":
		r.Locks[KeyAPIKey] = Lock{Reason: LockPaired, EnvVar: urlLock.EnvVar}
	case keyLock.Reason == LockEnv && urlLock.Reason == "":
		r.Locks[KeyURL] = Lock{Reason: LockPaired, EnvVar: keyLock.EnvVar}
	}

	if ReadOnly() {
		for _, key := range []string{KeyURL, KeyAPIKey, KeyRootFolder, KeyQualityProfileID, KeyWebURL} {
			if _, locked := r.Locks[key]; !locked {
				r.Locks[key] = Lock{Reason: LockReadOnly, EnvVar: readOnlyEnv}
			}
		}
	}
	return r
}

// Editable reports whether key can be changed from the settings page.
func (r Resolved) Editable(key string) bool {
	_, locked := r.Locks[key]
	return !locked
}

// Configured reports whether there is a Lidarr to talk to.
func (r Resolved) Configured() bool {
	return r.Lidarr.URL != "" && r.Lidarr.APIKey != ""
}

// RequestsEnabled reports whether requesting is on: all of where to put an
// artist and which profiles to give it are set, and Lidarr is configured.
func (r Resolved) RequestsEnabled() bool {
	l := r.Lidarr
	return r.Configured() && l.RootFolder != "" && l.QualityProfileID > 0
}

// MaskSecret stands in for a secret in anything sent to a browser. It reveals
// nothing, not even the last characters or the length: albumreq has no login,
// so whoever can reach the page must learn nothing about the key.
func MaskSecret(value string) string {
	if value == "" {
		return ""
	}
	return "••••••••"
}
