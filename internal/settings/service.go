package settings

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/rs/zerolog"

	"github.com/Miista/albumreq/internal/catalog"
	"github.com/Miista/albumreq/internal/issues"
	"github.com/Miista/albumreq/internal/lidarr"
	"github.com/Miista/albumreq/internal/store"
)

const (
	probeTimeout = 8 * time.Second

	// staleMetadataProfileEnv was the variable for a setting that no longer exists.
	staleMetadataProfileEnv = "ALBUMREQ_SETTING_LIDARR_METADATA_PROFILE_ID"
	maxField                = 300
)

// ErrReadOnly is returned for any change while the whole page is locked.
var ErrReadOnly = errors.New("settings are read-only")

// ValidationError is a refusal whose message is safe to show to the user.
type ValidationError struct{ Message string }

func (e *ValidationError) Error() string { return e.Message }

func invalid(format string, a ...any) error { return &ValidationError{fmt.Sprintf(format, a...)} }

// Service applies the Lidarr settings: it validates a change, checks it against
// Lidarr, persists it, and swaps the running Lidarr client and the request
// settings, so a change takes effect without a restart.
type Service struct {
	store *store.Store
	live  *lidarr.Live
	cat   *catalog.Catalog
	log   zerolog.Logger
	mu    sync.Mutex // serialises changes
}

// NewService returns a Service. Call Reload once before serving.
func NewService(st *store.Store, live *lidarr.Live, cat *catalog.Catalog, log zerolog.Logger) *Service {
	return &Service{store: st, live: live, cat: cat, log: log}
}

// Resolved returns the effective settings, secrets included. It is for the
// server's own use; anything sent to a browser goes through View.
func (s *Service) Resolved() Resolved {
	return Resolve(s.store.View().Lidarr)
}

// Reload makes the running process match the effective settings: it points the
// live Lidarr client at the configured Lidarr (or at nothing) and switches
// requesting on or off. A request configuration that Lidarr does not recognise
// switches requesting off and is logged, rather than stopping the process,
// because the settings page is how it gets fixed.
func (s *Service) Reload(ctx context.Context) {
	r := s.Resolved()
	for _, p := range r.Problems {
		s.log.Error().Msg("ignoring a setting: " + p)
	}

	if !r.Configured() {
		s.live.Set(nil)
		s.cat.SetRequests(nil)
		s.log.Info().Msg("Lidarr is not configured: library status is off until it is set on the settings page")
		return
	}
	if _, err := ValidateURL(r.Lidarr.URL); err != nil {
		s.live.Set(nil)
		s.cat.SetRequests(nil)
		s.log.Error().Msg("Lidarr URL is unusable, so Lidarr is off: " + err.Error())
		return
	}
	client := lidarr.New(r.Lidarr.URL, r.Lidarr.APIKey)
	s.live.Set(client)

	if !r.RequestsEnabled() {
		s.cat.SetRequests(nil)
		s.log.Info().Msg("requesting is off (read-only): it needs a root folder and a quality profile on the settings page")
		return
	}
	cctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	switch err := client.CheckSettings(cctx, r.Lidarr.RootFolder, r.Lidarr.QualityProfileID); {
	case errors.Is(err, lidarr.ErrBadSettings):
		s.cat.SetRequests(nil)
		s.log.Error().Msg("requesting is off because its settings do not match Lidarr: " + err.Error())
		return
	case err != nil:
		s.log.Warn().Msg(fmt.Sprintf("could not check the requesting settings against Lidarr (%v); requests will fail until Lidarr answers", err))
	}
	if _, err := client.MetadataProfileByName(cctx, lidarr.RequiredMetadataProfile); errors.Is(err, lidarr.ErrBadSettings) {
		// Not a reason to switch requesting off: a request that needs the profile
		// writes nothing and says why. It is reported on the Issues page.
		s.log.Warn().Msg("Lidarr has no metadata profile named " + fmt.Sprintf("%q", lidarr.RequiredMetadataProfile) + ": requests will be refused until it exists")
	}
	s.cat.SetRequests(&catalog.RequestSettings{RootFolder: r.Lidarr.RootFolder, QualityProfileID: r.Lidarr.QualityProfileID, MetadataProfile: lidarr.RequiredMetadataProfile})
	s.log.Info().Msg(fmt.Sprintf("requesting is ON: albums are added under %s with quality profile %d and the %q metadata profile, then monitored and searched for", r.Lidarr.RootFolder, r.Lidarr.QualityProfileID, lidarr.RequiredMetadataProfile))
}

// View is the settings as a browser may see them: no secret, ever.
type View struct {
	Values          ViewValues      `json:"values"`
	Locks           map[string]Lock `json:"locks"`
	ReadOnly        bool            `json:"read_only"`
	Configured      bool            `json:"configured"`
	RequestsEnabled bool            `json:"requests_enabled"`
	Problems        []string        `json:"problems"`
}

// ViewValues are the field values.
type ViewValues struct {
	URL              string `json:"url"`
	APIKey           string `json:"api_key"` // masked
	APIKeySet        bool   `json:"api_key_set"`
	RootFolder       string `json:"root_folder"`
	QualityProfileID int    `json:"quality_profile_id"`
	WebURL           string `json:"web_url"`
}

// View renders the effective settings for a browser.
func (s *Service) View() View { return viewOf(s.Resolved()) }

func viewOf(r Resolved) View {
	l := r.Lidarr
	locks := make(map[string]Lock, len(r.Locks))
	for k, v := range r.Locks {
		locks[k] = v
	}
	problems := r.Problems
	if problems == nil {
		problems = []string{}
	}
	return View{
		Values: ViewValues{
			URL: l.URL, APIKey: MaskSecret(l.APIKey), APIKeySet: l.APIKey != "",
			RootFolder: l.RootFolder, QualityProfileID: l.QualityProfileID, WebURL: l.WebURL,
		},
		Locks: locks, ReadOnly: ReadOnly(), Configured: r.Configured(), RequestsEnabled: r.RequestsEnabled(), Problems: problems,
	}
}

// Patch is a change to the editable fields. A nil field is left alone; an empty
// API key also means "leave it", because the page never sees the real key and so
// has nothing to send back.
type Patch struct {
	URL              *string
	APIKey           *string
	RootFolder       *string
	QualityProfileID *int
	WebURL           *string
}

// PatchResult is the outcome of a saved change.
type PatchResult struct {
	View    View
	Warning string // set when the change was saved but could not be checked against Lidarr
}

// Patch validates and saves a change. Locked fields are ignored; a change to
// anything else while the page is read-only is refused. The change is checked
// against Lidarr first: a rejected API key, or a root folder or profile Lidarr
// does not have, stops it. An unreachable Lidarr does not: the change is saved
// with a warning.
func (s *Service) Patch(ctx context.Context, p Patch) (PatchResult, error) {
	if ReadOnly() {
		return PatchResult{}, ErrReadOnly
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	cur := s.store.View().Lidarr
	res := Resolve(cur)
	next := cur
	var changed []string

	if p.URL != nil && res.Editable(KeyURL) {
		u := strings.TrimSpace(*p.URL)
		if u != "" {
			var err error
			if u, err = ValidateURL(u); err != nil {
				return PatchResult{}, err
			}
		}
		if u != res.Lidarr.URL {
			key := ""
			if p.APIKey != nil {
				key = strings.TrimSpace(*p.APIKey)
			}
			switch {
			case u == "":
				next.URL, next.APIKey = "", "" // no Lidarr, so no key to keep
			case key == "":
				return PatchResult{}, invalid("Changing the Lidarr URL needs the API key entered again, so the saved key is never sent to a different address.")
			default:
				next.URL = u
			}
			changed = append(changed, KeyURL)
		}
	}
	if p.APIKey != nil && res.Editable(KeyAPIKey) && next.URL != "" {
		if k := strings.TrimSpace(*p.APIKey); k != "" {
			if err := validateKey(k); err != nil {
				return PatchResult{}, err
			}
			if k != next.APIKey {
				next.APIKey = k
				changed = append(changed, KeyAPIKey)
			}
		}
	}
	if p.RootFolder != nil && res.Editable(KeyRootFolder) {
		f := strings.TrimSpace(*p.RootFolder)
		if f != "" && (!strings.HasPrefix(f, "/") || len(f) > maxField || strings.ContainsFunc(f, unicode.IsControl)) {
			return PatchResult{}, invalid("The root folder must be an absolute path, like /music.")
		}
		if f != next.RootFolder {
			next.RootFolder = f
			changed = append(changed, KeyRootFolder)
		}
	}
	if p.QualityProfileID != nil && res.Editable(KeyQualityProfileID) {
		if *p.QualityProfileID < 0 {
			return PatchResult{}, invalid("The quality profile must be one of Lidarr's profiles.")
		}
		if *p.QualityProfileID != next.QualityProfileID {
			next.QualityProfileID = *p.QualityProfileID
			changed = append(changed, KeyQualityProfileID)
		}
	}
	if p.WebURL != nil && res.Editable(KeyWebURL) {
		w := strings.TrimSpace(*p.WebURL)
		if w != "" {
			var err error
			if w, err = ValidateURL(w); err != nil {
				return PatchResult{}, invalid("The web address must start with http:// or https:// and have no credentials or query, like https://lidarr.example.com.")
			}
		}
		if w != res.Lidarr.WebURL {
			next.WebURL = w
			changed = append(changed, KeyWebURL)
		}
	}
	var warning string
	if len(changed) > 0 {
		var err error
		if warning, err = s.verify(ctx, Resolve(next)); err != nil {
			return PatchResult{}, err
		}
		if err := s.store.Update(func(st *store.State) { st.Lidarr = next }); err != nil {
			s.log.Error().Err(err).Msg("saving settings failed")
			return PatchResult{}, fmt.Errorf("saving settings: %w", err)
		}
		s.log.Info().Msg("settings changed: " + strings.Join(changed, ", "))
	}
	s.Reload(ctx)
	return PatchResult{View: s.View(), Warning: warning}, nil
}

// verify checks the settings as they would be effective against Lidarr.
func (s *Service) verify(ctx context.Context, r Resolved) (warning string, err error) {
	if !r.Configured() {
		return "", nil
	}
	cctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	c := lidarr.New(r.Lidarr.URL, r.Lidarr.APIKey)
	if _, err := c.Version(cctx); err != nil {
		if errors.Is(err, lidarr.ErrUnauthorized) {
			return "", invalid("Lidarr rejected the API key.")
		}
		return "Lidarr could not be reached, so these settings were saved without being checked.", nil
	}
	if r.RequestsEnabled() {
		if err := c.CheckSettings(cctx, r.Lidarr.RootFolder, r.Lidarr.QualityProfileID); errors.Is(err, lidarr.ErrBadSettings) {
			return "", invalid("%s", strings.TrimPrefix(err.Error(), lidarr.ErrBadSettings.Error()+": "))
		}
		if _, err := c.MetadataProfileByName(cctx, lidarr.RequiredMetadataProfile); errors.Is(err, lidarr.ErrBadSettings) {
			return fmt.Sprintf("Saved. Lidarr has no metadata profile named %q, so requests will be refused until it exists. See Issues.", lidarr.RequiredMetadataProfile), nil
		}
	}
	return "", nil
}

// Issues checks what an admin should know about: the environment, the connection
// to Lidarr, and what requesting needs from it. It asks Lidarr live, with a
// timeout, and stops at the first thing that makes the rest meaningless (no
// connection means no point asking about profiles).
func (s *Service) Issues(ctx context.Context) []issues.Issue {
	r := s.Resolved()
	var out []issues.Issue
	for _, p := range r.Problems {
		out = append(out, issues.Issue{
			ID: "env-invalid", Severity: issues.Error, Title: "A setting from the environment cannot be used",
			Detail: p + ". It is ignored until it is corrected.", Fix: "Correct the variable and restart albumreq.",
		})
	}
	if _, set := os.LookupEnv(staleMetadataProfileEnv); set {
		out = append(out, issues.Issue{
			ID: "env-metadata-profile-ignored", Severity: issues.Warning, Title: staleMetadataProfileEnv + " is set but no longer used",
			Detail: "Requested artists always get the metadata profile named " + fmt.Sprintf("%q", lidarr.RequiredMetadataProfile) + ", so this variable does nothing.",
			Fix:    "Remove it from the container's environment.",
		})
	}

	if !r.Configured() {
		return append(out, issues.Issue{
			ID: "lidarr-not-configured", Severity: issues.Error, Title: "Lidarr is not set up",
			Detail: "Without Lidarr albumreq cannot show what you already have or add albums.",
			Fix:    "Enter Lidarr's URL and API key on the Settings page.", Link: "#/settings",
		})
	}
	if _, err := ValidateURL(r.Lidarr.URL); err != nil {
		return append(out, issues.Issue{
			ID: "lidarr-url-unusable", Severity: issues.Error, Title: "Lidarr's address cannot be used",
			Detail: err.Error(), Fix: "Correct the URL on the Settings page, or the variable that sets it.", Link: "#/settings",
		})
	}

	cctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	c := lidarr.New(r.Lidarr.URL, r.Lidarr.APIKey)
	if _, err := c.Version(cctx); err != nil {
		if errors.Is(err, lidarr.ErrUnauthorized) {
			return append(out, issues.Issue{
				ID: "lidarr-key-rejected", Severity: issues.Error, Title: "Lidarr rejected the API key",
				Detail: "albumreq reached " + r.Lidarr.URL + ", but it did not accept the key.",
				Fix:    "Copy the key from Lidarr (Settings, General) into albumreq's settings.", Link: "#/settings",
			})
		}
		return append(out, issues.Issue{
			ID: "lidarr-unreachable", Severity: issues.Error, Title: "Lidarr cannot be reached",
			Detail: "albumreq could not connect to " + r.Lidarr.URL + ". Lidarr may be down or restarting, the address may be wrong, or the network may be blocking it.",
			Fix:    "Check that Lidarr is running and that the address is right, then check again.", Link: "#/settings",
		})
	}

	if !r.RequestsEnabled() {
		out = append(out, issues.Issue{
			ID: "requesting-off", Severity: issues.Warning, Title: "Requesting is off",
			Detail: "It needs a root folder and a quality profile.", Fix: "Choose them on the Settings page.", Link: "#/settings",
		})
	} else if err := c.CheckSettings(cctx, r.Lidarr.RootFolder, r.Lidarr.QualityProfileID); errors.Is(err, lidarr.ErrBadSettings) {
		out = append(out, issues.Issue{
			ID: "requesting-settings-mismatch", Severity: issues.Error, Title: "A requesting setting does not match Lidarr",
			Detail: strings.TrimPrefix(err.Error(), lidarr.ErrBadSettings.Error()+": ") + ".",
			Fix:    "Choose the root folder and quality profile again on the Settings page.", Link: "#/settings",
		})
	}

	if _, err := c.MetadataProfileByName(cctx, lidarr.RequiredMetadataProfile); errors.Is(err, lidarr.ErrBadSettings) {
		out = append(out, issues.Issue{
			ID: "no-none-profile", Severity: issues.Error, Title: "Lidarr has no metadata profile named " + fmt.Sprintf("“%s”", lidarr.RequiredMetadataProfile),
			Detail: "Every requested artist is given this profile so that a request adds only the album asked for, not the artist's other albums. Without it, requests are refused and nothing is added.",
			Fix:    "In Lidarr, under Settings, Profiles, Metadata, add a profile named " + fmt.Sprintf("“%s”", lidarr.RequiredMetadataProfile) + " with every album type unticked.",
		})
	}
	return out
}

// TestResult is the outcome of trying a connection.
type TestResult struct {
	OK      bool            `json:"ok"`
	Version string          `json:"version,omitempty"`
	Message string          `json:"message"`
	Options *lidarr.Options `json:"options,omitempty"`
}

// Test tries a connection and, when it works, lists Lidarr's root folders and
// profiles for the page's dropdowns. The saved API key is only ever sent to
// the saved URL: testing a different URL needs a key in the same call. While
// the connection is locked the arguments are ignored and the effective values
// are tested, so a locked page cannot be used to probe other addresses.
func (s *Service) Test(ctx context.Context, rawURL, rawKey string) (TestResult, error) {
	res := s.Resolved()
	u, key := res.Lidarr.URL, res.Lidarr.APIKey

	if res.Editable(KeyURL) && res.Editable(KeyAPIKey) && !ReadOnly() {
		if given := strings.TrimSpace(rawURL); given != "" {
			var err error
			if given, err = ValidateURL(given); err != nil {
				return TestResult{}, err
			}
			if given != u {
				k := strings.TrimSpace(rawKey)
				if k == "" {
					return TestResult{}, invalid("Enter the API key to test a different address.")
				}
				u, key = given, k
			}
		}
		if k := strings.TrimSpace(rawKey); k != "" {
			if err := validateKey(k); err != nil {
				return TestResult{}, err
			}
			key = k
		}
	}
	if u == "" || key == "" {
		return TestResult{Message: "Enter the Lidarr URL and API key first."}, nil
	}
	if _, err := ValidateURL(u); err != nil {
		return TestResult{Message: "The saved Lidarr URL is not usable: " + err.Error()}, nil
	}

	cctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	c := lidarr.New(u, key)
	version, err := c.Version(cctx)
	switch {
	case errors.Is(err, lidarr.ErrUnauthorized):
		return TestResult{Message: "Lidarr rejected the API key."}, nil
	case err != nil:
		return TestResult{Message: fmt.Sprintf("Could not reach Lidarr at %s: %v", u, err)}, nil
	}
	out := TestResult{OK: true, Version: version, Message: "Connected to Lidarr " + version + "."}
	if opts, err := c.Options(cctx); err == nil {
		out.Options = &opts
	} else {
		out.Message += " Its root folders and profiles could not be listed."
	}
	return out, nil
}

// ValidateURL normalises a Lidarr URL and refuses anything that is not a plain
// http(s) address: no credentials in the URL, no query, no fragment.
func ValidateURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > maxField {
		return "", invalid("Enter the Lidarr address, like http://lidarr:8686.")
	}
	u, err := url.Parse(raw)
	switch {
	case err != nil, u.Host == "", u.Hostname() == "":
		return "", invalid("That is not a usable address. Use something like http://lidarr:8686.")
	case u.Scheme != "http" && u.Scheme != "https":
		return "", invalid("The address must start with http:// or https://.")
	case u.User != nil:
		return "", invalid("Leave credentials out of the address; the API key goes in its own field.")
	case u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(raw, " \t\r\n"):
		return "", invalid("The address must be just the base URL, without a query or spaces.")
	}
	return strings.TrimRight(u.String(), "/"), nil
}

func validateKey(k string) error {
	if len(k) > 200 || strings.ContainsFunc(k, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return invalid("That does not look like an API key.")
	}
	return nil
}
