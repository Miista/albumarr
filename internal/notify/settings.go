package notify

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/rs/zerolog"

	"github.com/Miista/albumarr/internal/settings"
)

// The setting keys. The env var that pins one is ALBUMARR_SETTING_<KEY>, upper-cased
// with dots as underscores, like every other setting (see settings.EnvVar).
const (
	KeyEnabled     = "notifications.enabled"
	KeyURL         = "notifications.url"
	KeyTopic       = "notifications.topic"
	KeyTags        = "notifications.tags"
	KeyAuth        = "notifications.auth"
	KeyUsername    = "notifications.username"
	KeyPassword    = "notifications.password"
	KeyToken       = "notifications.token"
	KeyPriority    = "notifications.priority"
	KeyEmbedPoster = "notifications.embed_poster"
	KeyTypes       = "notifications.types"
	KeyAppURL      = "notifications.app_url"
)

var allKeys = []string{KeyEnabled, KeyURL, KeyTopic, KeyTags, KeyAuth, KeyUsername, KeyPassword, KeyToken, KeyPriority, KeyEmbedPoster, KeyTypes, KeyAppURL}

// credentialKeys travel together: the server address and everything that
// authenticates to it. If the environment pins any of them, all are locked, so a
// pinned credential can never be sent to an address someone can edit.
var credentialKeys = []string{KeyURL, KeyAuth, KeyUsername, KeyPassword, KeyToken}

// Resolved is the effective settings: the stored ones with any environment pins
// applied, what is locked and why, and what could not be used.
type Resolved struct {
	Config   Config
	Locks    map[string]settings.Lock
	Problems []string
}

// Editable reports whether key can be changed from the page.
func (r Resolved) Editable(key string) bool {
	_, locked := r.Locks[key]
	return !locked
}

// Resolve applies ALBUMARR_SETTING_NOTIFICATIONS_* over the stored settings. An
// unusable value is reported and ignored, and it fails safe: a bad "enabled" means
// off, never on.
func Resolve(stored Config) Resolved {
	r := Resolved{Config: stored.clone(), Locks: map[string]settings.Lock{}}
	env := func(key string) (string, bool) {
		v, ok := os.LookupEnv(settings.EnvVar(key))
		if !ok || v == "" {
			return "", false
		}
		r.Locks[key] = settings.Lock{Reason: settings.LockEnv, EnvVar: settings.EnvVar(key)}
		return v, true
	}
	bad := func(key, why string) { r.Problems = append(r.Problems, settings.EnvVar(key)+" "+why) }

	if v, ok := env(KeyEnabled); ok {
		b, err := strconv.ParseBool(strings.TrimSpace(v))
		if err != nil {
			bad(KeyEnabled, "is not true or false, so notifications stay off")
		}
		r.Config.Enabled = err == nil && b
	}
	if v, ok := env(KeyURL); ok {
		r.Config.URL = strings.TrimSpace(v)
	}
	if v, ok := env(KeyTopic); ok {
		r.Config.Topic = strings.TrimSpace(v)
	}
	if v, ok := env(KeyTags); ok {
		r.Config.Tags = strings.TrimSpace(v)
	}
	if v, ok := env(KeyAuth); ok {
		switch a := strings.ToLower(strings.TrimSpace(v)); a {
		case AuthNone, AuthBasic, AuthToken:
			r.Config.Auth = a
		default:
			bad(KeyAuth, "is not none, basic or token, so no credentials are sent")
			r.Config.Auth = AuthNone
		}
	}
	if v, ok := env(KeyUsername); ok {
		r.Config.Username = v
	}
	if v, ok := env(KeyPassword); ok {
		r.Config.Password = v
	}
	if v, ok := env(KeyToken); ok {
		r.Config.Token = strings.TrimSpace(v)
	}
	if v, ok := env(KeyPriority); ok {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil || n < 1 || n > 5 {
			bad(KeyPriority, "is not a whole number from 1 to 5, so the default (3) is used")
			n = 3
		}
		r.Config.Priority = n
	}
	if v, ok := env(KeyEmbedPoster); ok {
		b, err := strconv.ParseBool(strings.TrimSpace(v))
		if err != nil {
			bad(KeyEmbedPoster, "is not true or false, so no poster is attached")
		}
		r.Config.EmbedPoster = err == nil && b
	}
	if v, ok := env(KeyTypes); ok {
		types, err := ParseTypes(strings.Split(v, ","), All)
		if err != nil {
			bad(KeyTypes, "names an event that does not exist, so the shared channel is sent nothing")
			types = nil
		}
		r.Config.Types = types
	}
	if v, ok := env(KeyAppURL); ok {
		r.Config.AppURL = strings.TrimSpace(v)
	}

	// The server address and its credentials move together.
	var pinned string
	for _, k := range credentialKeys {
		if l, ok := r.Locks[k]; ok && l.Reason == settings.LockEnv {
			pinned = l.EnvVar
			break
		}
	}
	if pinned != "" {
		for _, k := range credentialKeys {
			if _, ok := r.Locks[k]; !ok {
				r.Locks[k] = settings.Lock{Reason: settings.LockPaired, EnvVar: pinned}
			}
		}
	}
	if settings.ReadOnly() {
		for _, k := range allKeys {
			if _, ok := r.Locks[k]; !ok {
				r.Locks[k] = settings.Lock{Reason: settings.LockReadOnly, EnvVar: "ALBUMARR_SETTINGS_READONLY"}
			}
		}
	}
	return r
}

// Usable reports whether the shared channel and per-person topics can be sent to:
// the agent is on and has somewhere to send.
func (c Config) Usable() bool { return c.Enabled && c.URL != "" }

// Server is what a send needs to reach the ntfy server.
func (c Config) Server() Server {
	return Server{URL: c.URL, Auth: c.Auth, Username: c.Username, Password: c.Password, Token: c.Token}
}

// ---- validation ----

var (
	topicRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	tagRe   = regexp.MustCompile(`^[A-Za-z0-9_+-]{1,32}$`)
)

const maxTags = 5

func invalid(format string, a ...any) error {
	return &settings.ValidationError{Message: fmt.Sprintf(format, a...)}
}

// ValidTopic reports whether s can be an ntfy topic: letters, digits, underscore and
// dash, up to 64 characters (what ntfy itself accepts).
func ValidTopic(s string) bool { return topicRe.MatchString(s) }

func validateAddress(raw, what string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	switch {
	case len(raw) > 300:
		return "", invalid("The %s is too long.", what)
	case err != nil, u.Host == "", u.Hostname() == "":
		return "", invalid("The %s is not usable. Use something like https://ntfy.example.com.", what)
	case u.Scheme != "http" && u.Scheme != "https":
		return "", invalid("The %s must start with http:// or https://.", what)
	case u.User != nil:
		return "", invalid("Leave credentials out of the %s; they have their own fields.", what)
	case u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(raw, " \t\r\n"):
		return "", invalid("The %s must be just the base address, without a query or spaces.", what)
	}
	return strings.TrimRight(u.String(), "/"), nil
}

func validateTags(raw string) (string, error) {
	var out []string
	for _, t := range strings.Split(raw, ",") {
		if t = strings.TrimSpace(t); t == "" {
			continue
		}
		if !tagRe.MatchString(t) {
			return "", invalid("Tags are short names like eyes or warning: letters, digits, + - and _.")
		}
		out = append(out, t)
	}
	if len(out) > maxTags {
		return "", invalid("Use at most %d tags.", maxTags)
	}
	return strings.Join(out, ","), nil
}

func validateSecret(s, what string) error {
	if len(s) > 256 || strings.ContainsFunc(s, unicode.IsControl) {
		return invalid("That does not look like a valid %s.", what)
	}
	return nil
}

// ---- the admin's view and changes ----

// View is the settings as a browser may see them: no secret, ever.
type View struct {
	Values     ViewValues               `json:"values"`
	Locks      map[string]settings.Lock `json:"locks"`
	ReadOnly   bool                     `json:"read_only"`
	Problems   []string                 `json:"problems"`
	EventTypes []EventType              `json:"event_types"`
	LastError  *Delivery                `json:"last_error"` // the last send that failed, if it is still the latest
}

// EventType names an event for a page.
type EventType struct {
	ID    Type   `json:"id"`
	Label string `json:"label"`
}

// ViewValues are the field values; secrets are masked and only reported as set.
type ViewValues struct {
	Enabled     bool     `json:"enabled"`
	URL         string   `json:"url"`
	Topic       string   `json:"topic"`
	Tags        string   `json:"tags"`
	Auth        string   `json:"auth"`
	Username    string   `json:"username"`
	Password    string   `json:"password"` // masked
	PasswordSet bool     `json:"password_set"`
	Token       string   `json:"token"` // masked
	TokenSet    bool     `json:"token_set"`
	Priority    int      `json:"priority"`
	EmbedPoster bool     `json:"embed_poster"`
	Types       []string `json:"types"`
	AppURL      string   `json:"app_url"`
}

// Delivery is the outcome of the latest send, for the page and the Issues list.
type Delivery struct {
	OK      bool      `json:"ok"`
	Message string    `json:"message"`
	At      time.Time `json:"at"`
}

// Patch is a change to the editable fields. A nil field is left alone; an empty
// password or token means "leave it", because the page never has the real one.
type Patch struct {
	Enabled     *bool
	URL         *string
	Topic       *string
	Tags        *string
	Auth        *string
	Username    *string
	Password    *string
	Token       *string
	Priority    *int
	EmbedPoster *bool
	Types       *[]string
	AppURL      *string
}

// Settings applies the admin's notification settings and sends the test message.
type Settings struct {
	store  *Store
	sender Sender
	log    zerolog.Logger

	mu       sync.Mutex // serialises changes
	statusMu sync.RWMutex
	last     *Delivery
}

// NewSettings returns the admin side of notifications.
func NewSettings(store *Store, sender Sender, log zerolog.Logger) *Settings {
	return &Settings{store: store, sender: sender, log: log}
}

// Resolved returns the effective settings now.
func (s *Settings) Resolved() Resolved { return Resolve(s.store.Config()) }

// Record notes the outcome of a send, so the page and the Issues list can show it.
func (s *Settings) Record(d Delivery) {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	s.last = &d
}

// LastDelivery is the outcome of the latest send, if there has been one.
func (s *Settings) LastDelivery() *Delivery {
	s.statusMu.RLock()
	defer s.statusMu.RUnlock()
	if s.last == nil {
		return nil
	}
	d := *s.last
	return &d
}

// View renders the effective settings for a browser.
func (s *Settings) View() View {
	r := s.Resolved()
	c := r.Config
	types := make([]string, len(c.Types))
	for i, t := range c.Types {
		types[i] = string(t)
	}
	locks := make(map[string]settings.Lock, len(r.Locks))
	for k, v := range r.Locks {
		locks[k] = v
	}
	problems := r.Problems
	if problems == nil {
		problems = []string{}
	}
	ev := make([]EventType, len(All))
	for i, t := range All {
		ev[i] = EventType{ID: t, Label: t.Label()}
	}
	v := View{
		Values: ViewValues{
			Enabled: c.Enabled, URL: c.URL, Topic: c.Topic, Tags: c.Tags, Auth: c.Auth, Username: c.Username,
			Password: settings.MaskSecret(c.Password), PasswordSet: c.Password != "",
			Token: settings.MaskSecret(c.Token), TokenSet: c.Token != "",
			Priority: c.Priority, EmbedPoster: c.EmbedPoster, Types: types, AppURL: c.AppURL,
		},
		Locks: locks, ReadOnly: settings.ReadOnly(), Problems: problems, EventTypes: ev,
	}
	if d := s.LastDelivery(); d != nil && !d.OK {
		v.LastError = d
	}
	return v
}

// Patch validates and saves a change. Locked fields are ignored (never stored).
// Changing the server address needs the credentials typed again, so a saved one
// can never be sent to an address typed into the page; clearing the address
// clears them and switches notifications off.
func (s *Settings) Patch(ctx context.Context, p Patch) (View, error) {
	if settings.ReadOnly() {
		return View{}, settings.ErrReadOnly
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	cur := s.store.Config()
	res := Resolve(cur)
	next := cur.clone()
	var changed []string
	set := func(key string, apply func()) {
		if res.Editable(key) {
			apply()
			changed = append(changed, key)
		}
	}

	if p.URL != nil && res.Editable(KeyURL) {
		u, err := validateAddress(*p.URL, "server address")
		if err != nil {
			return View{}, err
		}
		if u != next.URL {
			secretTyped := (p.Password != nil && *p.Password != "") || (p.Token != nil && *p.Token != "")
			if u != "" && next.URL != "" && next.Auth != AuthNone && !secretTyped && res.Editable(KeyPassword) {
				return View{}, invalid("Type the password or token again when you change the server address, so the saved one is not sent to a new address.")
			}
			set(KeyURL, func() { next.URL = u })
			if u == "" {
				next.Password, next.Token, next.Enabled = "", "", false
			}
		}
	}
	if p.Auth != nil && res.Editable(KeyAuth) {
		a := strings.ToLower(strings.TrimSpace(*p.Auth))
		if a != AuthNone && a != AuthBasic && a != AuthToken {
			return View{}, invalid("Authentication must be none, a username and password, or a token.")
		}
		if a != next.Auth {
			set(KeyAuth, func() { next.Auth = a })
		}
	}
	if p.Username != nil && res.Editable(KeyUsername) {
		if err := validateSecret(*p.Username, "username"); err != nil {
			return View{}, err
		}
		if *p.Username != next.Username {
			set(KeyUsername, func() { next.Username = *p.Username })
		}
	}
	if p.Password != nil && *p.Password != "" && res.Editable(KeyPassword) {
		if err := validateSecret(*p.Password, "password"); err != nil {
			return View{}, err
		}
		set(KeyPassword, func() { next.Password = *p.Password })
	}
	if p.Token != nil && *p.Token != "" && res.Editable(KeyToken) {
		t := strings.TrimSpace(*p.Token)
		if err := validateSecret(t, "token"); err != nil {
			return View{}, err
		}
		set(KeyToken, func() { next.Token = t })
	}
	if p.Topic != nil && res.Editable(KeyTopic) {
		t := strings.TrimSpace(*p.Topic)
		if t != "" && !ValidTopic(t) {
			return View{}, invalid("A topic is letters, digits, - and _, up to 64 characters.")
		}
		if t != next.Topic {
			set(KeyTopic, func() { next.Topic = t })
		}
	}
	if p.Tags != nil && res.Editable(KeyTags) {
		t, err := validateTags(*p.Tags)
		if err != nil {
			return View{}, err
		}
		if t != next.Tags {
			set(KeyTags, func() { next.Tags = t })
		}
	}
	if p.Priority != nil && res.Editable(KeyPriority) {
		if *p.Priority < 1 || *p.Priority > 5 {
			return View{}, invalid("Priority is a number from 1 (lowest) to 5 (urgent).")
		}
		if *p.Priority != next.Priority {
			set(KeyPriority, func() { next.Priority = *p.Priority })
		}
	}
	if p.EmbedPoster != nil && res.Editable(KeyEmbedPoster) && *p.EmbedPoster != next.EmbedPoster {
		set(KeyEmbedPoster, func() { next.EmbedPoster = *p.EmbedPoster })
	}
	if p.Types != nil && res.Editable(KeyTypes) {
		t, err := ParseTypes(*p.Types, All)
		if err != nil {
			return View{}, invalid("%s.", err.Error())
		}
		set(KeyTypes, func() { next.Types = t })
	}
	if p.AppURL != nil && res.Editable(KeyAppURL) {
		u, err := validateAddress(*p.AppURL, "application address")
		if err != nil {
			return View{}, err
		}
		if u != next.AppURL {
			set(KeyAppURL, func() { next.AppURL = u })
		}
	}
	if p.Enabled != nil && res.Editable(KeyEnabled) && *p.Enabled != next.Enabled {
		set(KeyEnabled, func() { next.Enabled = *p.Enabled })
	}
	if next.Enabled && res.Config.URL == "" && next.URL == "" {
		return View{}, invalid("Enter the ntfy server address before switching notifications on.")
	}

	if len(changed) > 0 {
		if err := s.store.Update(func(c *Config) { *c = next; c.EnabledSince = cur.EnabledSince }); err != nil {
			s.log.Error().Err(err).Msg("could not save the notification settings")
			return View{}, errors.New("could not save the notification settings")
		}
		// Names only: never a value, and never a secret.
		s.log.Info().Msg("notification settings changed: " + strings.Join(changed, ", "))
	}
	return s.View(), nil
}

// TestInput is what the Test button sends: the form as it stands, saved or not.
type TestInput struct {
	URL      string
	Topic    string
	Auth     string
	Username string
	Password string
	Token    string
	Priority int
	Tags     string
}

// TestResult is the outcome of a test message.
type TestResult struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

// Test sends one message to the shared channel using the form's values. A saved
// password or token is only sent to the saved address: a different address needs
// the credentials typed in the form, so the button cannot be used to hand them to a
// server of someone's choosing. If the address or credentials are pinned by the
// environment, or the page is read-only, the form is ignored and the real settings
// are tested.
func (s *Settings) Test(ctx context.Context, in TestInput) (TestResult, error) {
	res := s.Resolved()
	c := res.Config
	srv := c.Server()
	topic, priority, tags := c.Topic, c.Priority, c.Tags

	if res.Editable(KeyURL) {
		u, err := validateAddress(in.URL, "server address")
		if err != nil {
			return TestResult{}, err
		}
		if u == "" {
			return TestResult{OK: false, Message: "Enter the ntfy server address first."}, nil
		}
		auth := strings.ToLower(strings.TrimSpace(in.Auth))
		if auth == "" {
			auth = c.Auth
		}
		if auth != AuthNone && auth != AuthBasic && auth != AuthToken {
			return TestResult{}, invalid("Authentication must be none, a username and password, or a token.")
		}
		srv = Server{URL: u, Auth: auth, Username: in.Username}
		switch auth {
		case AuthBasic:
			srv.Password = in.Password
			if in.Password == "" {
				if u != c.URL {
					return TestResult{}, invalid("Type the password to test a different address.")
				}
				srv.Password = c.Password
			}
		case AuthToken:
			srv.Token = in.Token
			if in.Token == "" {
				if u != c.URL {
					return TestResult{}, invalid("Type the token to test a different address.")
				}
				srv.Token = c.Token
			}
		}
	}
	if res.Editable(KeyTopic) && strings.TrimSpace(in.Topic) != "" {
		topic = strings.TrimSpace(in.Topic)
	}
	if topic == "" {
		return TestResult{OK: false, Message: "Enter a topic for the shared channel first."}, nil
	}
	if !ValidTopic(topic) {
		return TestResult{}, invalid("A topic is letters, digits, - and _, up to 64 characters.")
	}
	if res.Editable(KeyPriority) && in.Priority != 0 {
		if in.Priority < 1 || in.Priority > 5 {
			return TestResult{}, invalid("Priority is a number from 1 (lowest) to 5 (urgent).")
		}
		priority = in.Priority
	}
	if res.Editable(KeyTags) {
		t, err := validateTags(in.Tags)
		if err != nil {
			return TestResult{}, err
		}
		tags = t
	}

	msg := Message{Topic: topic, Title: "albumarr test notification", Body: "If you can read this, notifications are working.", Priority: priority, Tags: splitTags(tags)}
	cctx, cancel := context.WithTimeout(ctx, sendTimeout+2*time.Second)
	defer cancel()
	if err := s.sender.Send(cctx, srv, msg); err != nil {
		s.log.Warn().Msg("test notification failed: " + err.Error())
		return TestResult{OK: false, Message: "The test did not go through: " + err.Error() + "."}, nil
	}
	return TestResult{OK: true, Message: "Sent. Check that it arrived."}, nil
}

func splitTags(s string) []string {
	var out []string
	for _, t := range strings.Split(s, ",") {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}
