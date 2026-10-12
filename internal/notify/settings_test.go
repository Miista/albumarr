package notify

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/Miista/albumreq/internal/issues"
	"github.com/Miista/albumreq/internal/settings"
)

type sent struct {
	srv Server
	msg Message
}

type fakeSender struct {
	mu   sync.Mutex
	sent []sent
	err  error
}

func (f *fakeSender) Send(_ context.Context, srv Server, m Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.sent = append(f.sent, sent{srv, m})
	return nil
}

func (f *fakeSender) all() []sent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sent(nil), f.sent...)
}

func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range allKeys {
		t.Setenv(settings.EnvVar(k), "")
	}
	t.Setenv("ALBUMREQ_SETTINGS_READONLY", "")
}

func newSettings(t *testing.T) (*Settings, *Store, *fakeSender) {
	t.Helper()
	clearEnv(t)
	st, _ := openStore(t)
	fs := &fakeSender{}
	return NewSettings(st, fs, zerolog.Nop()), st, fs
}

func sp(s string) *string { return &s }
func ip(i int) *int       { return &i }
func bp(b bool) *bool     { return &b }

func isValidation(err error) bool {
	var v *settings.ValidationError
	return errors.As(err, &v)
}

// ---- Resolve: the environment wins, and says so ----

func TestEnvPinsEveryFieldAndLocksIt(t *testing.T) {
	clearEnv(t)
	for k, v := range map[string]string{
		KeyEnabled: "true", KeyURL: "https://ntfy.example.com", KeyTopic: "approvers", KeyTags: "eyes", KeyAuth: "token",
		KeyToken: "tk_x", KeyPriority: "4", KeyEmbedPoster: "true", KeyTypes: "pending,declined", KeyAppURL: "http://albumreq.example",
	} {
		t.Setenv(settings.EnvVar(k), v)
	}
	r := Resolve(Config{Topic: "stored", Types: All})
	c := r.Config
	if !c.Enabled || c.URL != "https://ntfy.example.com" || c.Topic != "approvers" || c.Tags != "eyes" || c.Auth != AuthToken || c.Token != "tk_x" ||
		c.Priority != 4 || !c.EmbedPoster || !reflect.DeepEqual(c.Types, []Type{Pending, Declined}) || c.AppURL != "http://albumreq.example" {
		t.Errorf("%+v", c)
	}
	for _, k := range []string{KeyEnabled, KeyURL, KeyTopic, KeyTags, KeyAuth, KeyToken, KeyPriority, KeyEmbedPoster, KeyTypes, KeyAppURL} {
		if l := r.Locks[k]; l.Reason != settings.LockEnv || l.EnvVar != settings.EnvVar(k) {
			t.Errorf("%s: lock %+v", k, l)
		}
	}
	if r.Locks[KeyUsername].Reason != settings.LockPaired {
		t.Errorf("the username travels with the pinned credentials: %+v", r.Locks[KeyUsername])
	}
	if len(r.Problems) != 0 {
		t.Errorf("problems: %v", r.Problems)
	}
}

func TestPinningAnyCredentialLocksTheWholeGroup(t *testing.T) {
	for _, pinned := range []string{KeyURL, KeyAuth, KeyUsername, KeyPassword, KeyToken} {
		clearEnv(t)
		t.Setenv(settings.EnvVar(pinned), "x")
		if pinned == KeyAuth {
			t.Setenv(settings.EnvVar(pinned), "basic")
		}
		r := Resolve(Config{})
		for _, k := range credentialKeys {
			l, ok := r.Locks[k]
			if !ok {
				t.Errorf("pinning %s left %s editable: a pinned credential could be sent to an address someone can edit", pinned, k)
			} else if k != pinned && l.Reason != settings.LockPaired {
				t.Errorf("pinning %s: %s is locked as %q, want paired", pinned, k, l.Reason)
			}
		}
		if _, ok := r.Locks[KeyTopic]; ok {
			t.Errorf("pinning %s must not lock the topic", pinned)
		}
	}
}

func TestReadOnlyLocksEverythingAndEnvKeepsItsOwnReason(t *testing.T) {
	clearEnv(t)
	t.Setenv("ALBUMREQ_SETTINGS_READONLY", "true")
	t.Setenv(settings.EnvVar(KeyTopic), "pinned")
	r := Resolve(Config{})
	for _, k := range allKeys {
		if r.Editable(k) {
			t.Errorf("%s is editable on a read-only page", k)
		}
	}
	if r.Locks[KeyTopic].Reason != settings.LockEnv || r.Locks[KeyPriority].Reason != settings.LockReadOnly {
		t.Errorf("%+v / %+v", r.Locks[KeyTopic], r.Locks[KeyPriority])
	}
}

func TestUnusableEnvValuesAreReportedAndFailSafe(t *testing.T) {
	clearEnv(t)
	t.Setenv(settings.EnvVar(KeyEnabled), "yes please")
	t.Setenv(settings.EnvVar(KeyPriority), "9")
	t.Setenv(settings.EnvVar(KeyAuth), "kerberos")
	t.Setenv(settings.EnvVar(KeyTypes), "pending,aproved")
	t.Setenv(settings.EnvVar(KeyEmbedPoster), "maybe")
	r := Resolve(Config{Enabled: true, Priority: 2, Auth: AuthToken, Token: "t", EmbedPoster: true, Types: All})
	c := r.Config
	if c.Enabled || c.Priority != 3 || c.Auth != AuthNone || len(c.Types) != 0 || c.EmbedPoster {
		t.Errorf("a garbled value must fail safe (off, default, nothing sent): %+v", c)
	}
	if len(r.Problems) != 5 {
		t.Errorf("problems: %v", r.Problems)
	}
	for _, p := range r.Problems {
		if strings.Contains(p, "yes please") || strings.Contains(p, "aproved") || strings.Contains(p, "kerberos") {
			t.Errorf("a problem echoes the bad value: %q", p)
		}
	}
}

func TestResolveNeverChangesWhatWasPassedIn(t *testing.T) {
	clearEnv(t)
	t.Setenv(settings.EnvVar(KeyTopic), "pinned")
	stored := Config{Topic: "stored", Types: []Type{Approved}}
	Resolve(stored)
	if stored.Topic != "stored" || !reflect.DeepEqual(stored.Types, []Type{Approved}) {
		t.Errorf("%+v", stored)
	}
}

// ---- Patch ----

func TestPatchSavesAndTheViewHidesSecrets(t *testing.T) {
	s, st, _ := newSettings(t)
	v, err := s.Patch(context.Background(), Patch{
		Enabled: bp(true), URL: sp("https://ntfy.example.com/"), Topic: sp("albumreq-approvers"), Tags: sp(" eyes , warning "),
		Auth: sp("basic"), Username: sp("alice"), Password: sp("hunter2 hunter2"), Priority: ip(4), EmbedPoster: bp(true),
		Types: &[]string{"declined", "pending"}, AppURL: sp("http://albumreq.example:8768/"),
	})
	if err != nil {
		t.Fatal(err)
	}
	c := st.Config()
	if !c.Enabled || c.URL != "https://ntfy.example.com" || c.Topic != "albumreq-approvers" || c.Tags != "eyes,warning" || c.Auth != AuthBasic || c.Username != "alice" ||
		c.Password != "hunter2 hunter2" || c.Priority != 4 || !c.EmbedPoster || !reflect.DeepEqual(c.Types, []Type{Pending, Declined}) || c.AppURL != "http://albumreq.example:8768" || c.EnabledSince == 0 {
		t.Errorf("%+v", c)
	}
	raw, _ := json.Marshal(v)
	if strings.Contains(string(raw), "hunter2") || !v.Values.PasswordSet || v.Values.Password == "" {
		t.Errorf("the view must say a password is set and show none of it: %s", raw)
	}
}

func TestPatchRefusals(t *testing.T) {
	good := Patch{URL: sp("https://ntfy.example.com")}
	for name, p := range map[string]Patch{
		"a url with credentials":            {URL: sp("https://u:p@ntfy.example.com")},
		"a url with a query":                {URL: sp("https://ntfy.example.com/?x=1")},
		"not http":                          {URL: sp("ftp://ntfy.example.com")},
		"no host":                           {URL: sp("https://")},
		"a url with a space":                {URL: sp("https://ntfy .example.com")},
		"an enormous url":                   {URL: sp("https://" + strings.Repeat("a", 400))},
		"a topic with a slash":              {Topic: sp("a/b")},
		"a topic with a space":              {Topic: sp("my topic")},
		"a topic that is too long":          {Topic: sp(strings.Repeat("a", 65))},
		"a bad tag":                         {Tags: sp("eyes,<b>")},
		"too many tags":                     {Tags: sp("a,b,c,d,e,f")},
		"an unknown auth method":            {Auth: sp("digest")},
		"priority 0":                        {Priority: ip(0)},
		"priority 6":                        {Priority: ip(6)},
		"a typo in an event":                {Types: &[]string{"pending", "aproved"}},
		"a bad application url":             {AppURL: sp("javascript:alert(1)")},
		"a control character in a password": {Password: sp("a\nb")},
		"an enormous token":                 {Token: sp(strings.Repeat("t", 300))},
	} {
		s, st, _ := newSettings(t)
		s.Patch(context.Background(), good)
		before := st.Config()
		if _, err := s.Patch(context.Background(), p); !isValidation(err) {
			t.Errorf("%s: err = %v, want a validation error", name, err)
		}
		if !reflect.DeepEqual(st.Config(), before) {
			t.Errorf("%s: a refused change was saved", name)
		}
	}
}

func TestEnablingNeedsAServerAddress(t *testing.T) {
	s, st, _ := newSettings(t)
	if _, err := s.Patch(context.Background(), Patch{Enabled: bp(true)}); !isValidation(err) || st.Config().Enabled {
		t.Errorf("err = %v enabled = %v", err, st.Config().Enabled)
	}
	if _, err := s.Patch(context.Background(), Patch{Enabled: bp(true), URL: sp("https://n.example")}); err != nil {
		t.Errorf("an address in the same change is fine: %v", err)
	}
}

func TestChangingTheAddressNeedsTheCredentialsAgain(t *testing.T) {
	s, st, _ := newSettings(t)
	s.Patch(context.Background(), Patch{URL: sp("https://ntfy.one"), Auth: sp("token"), Token: sp("tk_saved")})
	if _, err := s.Patch(context.Background(), Patch{URL: sp("https://attacker.example")}); !isValidation(err) {
		t.Errorf("err = %v: the saved token must not be sent to a new address", err)
	}
	if c := st.Config(); c.URL != "https://ntfy.one" || c.Token != "tk_saved" {
		t.Errorf("%+v", c)
	}
	if _, err := s.Patch(context.Background(), Patch{URL: sp("https://ntfy.two"), Token: sp("tk_new")}); err != nil {
		t.Fatal(err)
	}
	if c := st.Config(); c.URL != "https://ntfy.two" || c.Token != "tk_new" {
		t.Errorf("%+v", c)
	}
	// With no authentication there is nothing to protect.
	s.Patch(context.Background(), Patch{Auth: sp("none")})
	if _, err := s.Patch(context.Background(), Patch{URL: sp("https://ntfy.three")}); err != nil {
		t.Errorf("no credentials, so no need to retype them: %v", err)
	}
}

func TestEmptySecretsMeanLeaveItAndClearingTheAddressClearsThem(t *testing.T) {
	s, st, _ := newSettings(t)
	s.Patch(context.Background(), Patch{URL: sp("https://n.example"), Auth: sp("basic"), Username: sp("u"), Password: sp("pw"), Enabled: bp(true)})
	s.Patch(context.Background(), Patch{Password: sp(""), Token: sp("")})
	if st.Config().Password != "pw" {
		t.Error("an empty password means leave it")
	}
	if _, err := s.Patch(context.Background(), Patch{URL: sp("")}); err != nil {
		t.Fatal(err)
	}
	if c := st.Config(); c.URL != "" || c.Password != "" || c.Token != "" || c.Enabled {
		t.Errorf("clearing the address must leave no credential behind and switch it off: %+v", c)
	}
}

func TestLockedFieldsAreIgnoredAndTheEnvNeverReachesTheDatabase(t *testing.T) {
	s, st, _ := newSettings(t)
	t.Setenv(settings.EnvVar(KeyTopic), "pinned-topic")
	t.Setenv(settings.EnvVar(KeyPriority), "5")
	if _, err := s.Patch(context.Background(), Patch{Topic: sp("typed"), Priority: ip(1), Tags: sp("eyes")}); err != nil {
		t.Fatal(err)
	}
	c := st.Config()
	if c.Topic != "" || c.Priority != 3 {
		t.Errorf("pinned fields must not be stored, and the env value must not be either: topic=%q priority=%d", c.Topic, c.Priority)
	}
	if c.Tags != "eyes" {
		t.Errorf("an unpinned field still saves: %q", c.Tags)
	}
	if v := s.View(); v.Values.Topic != "pinned-topic" || v.Values.Priority != 5 {
		t.Errorf("the view shows the effective values: %+v", v.Values)
	}
}

func TestPinnedCredentialsLockTheAddressSoItCannotBeRedirected(t *testing.T) {
	s, st, _ := newSettings(t)
	st.Update(func(c *Config) { c.URL = "https://ntfy.one" })
	t.Setenv(settings.EnvVar(KeyToken), "tk_pinned")
	if _, err := s.Patch(context.Background(), Patch{URL: sp("https://attacker.example")}); err != nil {
		t.Fatal(err)
	}
	if st.Config().URL != "https://ntfy.one" {
		t.Error("the address moved although a credential is pinned by the environment")
	}
}

func TestReadOnlyRefusesEveryChange(t *testing.T) {
	s, st, _ := newSettings(t)
	t.Setenv("ALBUMREQ_SETTINGS_READONLY", "true")
	if _, err := s.Patch(context.Background(), Patch{Topic: sp("x")}); !errors.Is(err, settings.ErrReadOnly) {
		t.Errorf("err = %v", err)
	}
	if st.Config().Topic != "" || !s.View().ReadOnly {
		t.Error("nothing may change, and the view says why")
	}
}

// ---- Test ----

func TestTheTestButtonUsesTheFormAndTheSavedSecretOnlyForTheSavedAddress(t *testing.T) {
	s, st, fs := newSettings(t)
	st.Update(func(c *Config) {
		c.URL, c.Topic, c.Auth, c.Token = "https://saved.example", "shared", AuthToken, "tk_saved"
	})

	r, err := s.Test(context.Background(), TestInput{URL: "https://saved.example", Topic: "other", Auth: "token"})
	if err != nil || !r.OK {
		t.Fatalf("%+v %v", r, err)
	}
	if got := fs.all(); len(got) != 1 || got[0].srv.Token != "tk_saved" || got[0].msg.Topic != "other" {
		t.Errorf("%+v", got)
	}

	// A different address, no token typed: refused before anything is sent.
	if _, err := s.Test(context.Background(), TestInput{URL: "https://attacker.example", Topic: "t", Auth: "token"}); !isValidation(err) {
		t.Errorf("err = %v", err)
	}
	if len(fs.all()) != 1 {
		t.Error("the saved token was sent toward a different address")
	}
	// With the token typed, that token (and only that) goes to the new address.
	if r, err := s.Test(context.Background(), TestInput{URL: "https://new.example", Topic: "t", Auth: "token", Token: "tk_typed"}); err != nil || !r.OK {
		t.Fatalf("%+v %v", r, err)
	}
	if got := fs.all(); got[1].srv.URL != "https://new.example" || got[1].srv.Token != "tk_typed" {
		t.Errorf("%+v", got[1].srv)
	}
	if st.Config().URL != "https://saved.example" {
		t.Error("testing must never save anything")
	}
}

func TestTestWithTheAddressPinnedIgnoresTheFormAndCannotProbeAnotherHost(t *testing.T) {
	s, _, fs := newSettings(t)
	t.Setenv(settings.EnvVar(KeyURL), "https://pinned.example")
	t.Setenv(settings.EnvVar(KeyTopic), "pinned-topic")
	r, err := s.Test(context.Background(), TestInput{URL: "https://probe.internal:9000", Topic: "form-topic", Auth: "token", Token: "x"})
	if err != nil || !r.OK {
		t.Fatalf("%+v %v", r, err)
	}
	if got := fs.all(); len(got) != 1 || got[0].srv.URL != "https://pinned.example" || got[0].msg.Topic != "pinned-topic" {
		t.Errorf("a locked page must test the real settings, not what was typed: %+v", got)
	}
}

func TestTestReportsFailuresWithoutLeaking(t *testing.T) {
	s, st, fs := newSettings(t)
	st.Update(func(c *Config) { c.URL, c.Topic = "https://n.example", "shared" })
	fs.err = errors.New("could not reach the ntfy server: the connection was refused")
	r, err := s.Test(context.Background(), TestInput{URL: "https://n.example", Topic: "shared"})
	if err != nil || r.OK || !strings.Contains(r.Message, "connection was refused") {
		t.Errorf("%+v %v", r, err)
	}
	for name, in := range map[string]TestInput{
		"no address":    {Topic: "t"},
		"no topic":      {URL: "https://n.example"},
		"an odd topic":  {URL: "https://n.example", Topic: "a b"},
		"bad priority":  {URL: "https://n.example", Topic: "t", Priority: 9},
		"bad auth":      {URL: "https://n.example", Topic: "t", Auth: "digest"},
		"a bad address": {URL: "ftp://n.example", Topic: "t"},
	} {
		st.Update(func(c *Config) { c.Topic = "" })
		fs.err = nil
		before := len(fs.all())
		r, err := s.Test(context.Background(), in)
		if (err == nil && r.OK) || len(fs.all()) != before {
			t.Errorf("%s: %+v %v: nothing should have been sent", name, r, err)
		}
	}
}

func TestTheLastFailedSendShowsInTheViewUntilASuccessReplacesIt(t *testing.T) {
	s, _, _ := newSettings(t)
	if s.View().LastError != nil {
		t.Error("nothing has been sent")
	}
	s.Record(Delivery{OK: false, Message: "the connection was refused"})
	if v := s.View(); v.LastError == nil || v.LastError.Message != "the connection was refused" {
		t.Errorf("%+v", v.LastError)
	}
	s.Record(Delivery{OK: true})
	if s.View().LastError != nil {
		t.Error("a later success clears the failure")
	}
}

// ---- Issues ----

func issueIDs(list []issues.Issue) []string {
	var out []string
	for _, i := range list {
		out = append(out, i.ID)
	}
	return out
}

func TestNoIssuesWhileNotificationsAreOff(t *testing.T) {
	s, _, _ := newSettings(t)
	if got := s.Issues(context.Background()); len(got) != 0 {
		t.Errorf("%v: an agent nobody switched on is not a problem", issueIDs(got))
	}
}

func TestIssuesForAnEnabledAgent(t *testing.T) {
	ctx := context.Background()
	s, st, _ := newSettings(t)
	st.Update(func(c *Config) { c.Enabled, c.URL, c.Topic = true, "https://n.example", "t" })
	got := s.Issues(ctx)
	if ids := issueIDs(got); len(ids) != 1 || ids[0] != "notifications-no-app-url" || got[0].Severity != issues.Info {
		t.Errorf("a working agent with no links is only a note: %v", ids)
	}
	st.Update(func(c *Config) { c.AppURL = "http://a"; c.Auth = AuthToken })
	if ids := issueIDs(s.Issues(ctx)); len(ids) != 1 || ids[0] != "notifications-no-credentials" {
		t.Errorf("a token method with no token: %v", ids)
	}
	st.Update(func(c *Config) { c.Auth = AuthBasic; c.Username = "u" })
	if ids := issueIDs(s.Issues(ctx)); len(ids) != 1 || ids[0] != "notifications-no-credentials" {
		t.Errorf("basic with only a username: %v", ids)
	}
	st.Update(func(c *Config) { c.Auth = AuthNone })
	if got := s.Issues(ctx); len(got) != 0 {
		t.Errorf("%v", issueIDs(got))
	}
	st.Update(func(c *Config) { c.URL = "" })
	if ids := issueIDs(s.Issues(ctx)); len(ids) != 1 || ids[0] != "notifications-no-server" {
		t.Errorf("%v", ids)
	}
}

func TestADeliveryFailureIsAWarningUntilItIsStaleOrReplaced(t *testing.T) {
	ctx := context.Background()
	s, st, _ := newSettings(t)
	st.Update(func(c *Config) { c.Enabled, c.URL, c.AppURL = true, "https://n.example", "http://a" })
	s.Record(Delivery{OK: false, Message: "the connection was refused", At: time.Now()})
	got := s.Issues(ctx)
	if len(got) != 1 || got[0].ID != "notifications-failing" || got[0].Severity != issues.Warning || !strings.Contains(got[0].Detail, "connection was refused") || got[0].Link != "#/settings/notifications" {
		t.Fatalf("%+v", got)
	}
	for _, secret := range []string{"n.example"} {
		if strings.Contains(got[0].Detail+got[0].Fix, secret) {
			t.Errorf("the issue leaks %q", secret)
		}
	}
	s.Record(Delivery{OK: true, At: time.Now()})
	if got := s.Issues(ctx); len(got) != 0 {
		t.Errorf("a send that worked clears it: %v", issueIDs(got))
	}
	s.Record(Delivery{OK: false, Message: "old news", At: time.Now().Add(-2 * time.Hour)})
	if got := s.Issues(ctx); len(got) != 0 {
		t.Errorf("a failure from two hours ago with nothing since is not a current problem: %v", issueIDs(got))
	}
}

func TestEnvironmentProblemsAreErrorsAndNeverEchoTheValue(t *testing.T) {
	s, _, _ := newSettings(t)
	t.Setenv(settings.EnvVar(KeyPriority), "loud")
	got := s.Issues(context.Background())
	if len(got) != 1 || got[0].ID != "notifications-env-invalid" || got[0].Severity != issues.Error || strings.Contains(got[0].Detail, "loud") {
		t.Errorf("%+v", got)
	}
}
