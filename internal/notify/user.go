package notify

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

const testEvery = 10 * time.Second

// UserView is one person's notification settings, as their page shows them.
type UserView struct {
	// Available is whether notifications are set up at all. When they are not, the
	// page says so and shows no server.
	Available  bool        `json:"available"`
	Server     string      `json:"server"` // the host only, for "subscribe to this topic on ..."
	Topic      string      `json:"topic"`
	Types      []string    `json:"types"`
	EventTypes []EventType `json:"event_types"`
	// Suggested is a topic nobody could guess, offered while none is chosen. On an
	// open ntfy server anyone who knows a topic's name can read it.
	Suggested string `json:"suggested_topic,omitempty"`
}

// UserSettings lets a person choose where to hear about their own requests.
type UserSettings struct {
	store    *Store
	settings *Settings
	sender   Sender
	log      zerolog.Logger
	now      func() time.Time

	mu       sync.Mutex
	lastTest map[string]time.Time
}

// NewUserSettings returns the per-person side of notifications.
func NewUserSettings(store *Store, set *Settings, sender Sender, log zerolog.Logger) *UserSettings {
	return &UserSettings{store: store, settings: set, sender: sender, log: log, now: time.Now, lastTest: map[string]time.Time{}}
}

// SuggestTopic returns a topic name that cannot be guessed.
func SuggestTopic() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic("notify: no entropy: " + err.Error())
	}
	return "albumarr-" + hex.EncodeToString(b)
}

func requesterEventTypes() []EventType {
	out := make([]EventType, len(ForRequesters))
	for i, t := range ForRequesters {
		out[i] = EventType{ID: t, Label: t.Label()}
	}
	return out
}

func typeNames(list []Type) []string {
	out := make([]string, len(list))
	for i, t := range list {
		out[i] = string(t)
	}
	return out
}

// View returns the person's settings.
func (u *UserSettings) View(sub string) (UserView, error) {
	cfg := u.settings.Resolved().Config
	v := UserView{Available: cfg.Usable(), Types: typeNames(ForRequesters), EventTypes: requesterEventTypes()}
	if !v.Available {
		return v, nil
	}
	if pu, err := url.Parse(cfg.URL); err == nil {
		v.Server = pu.Host
	}
	p, found, err := u.store.Prefs(sub)
	if err != nil {
		return UserView{}, err
	}
	if found {
		v.Topic, v.Types = p.Topic, typeNames(p.Types)
	} else {
		v.Suggested = SuggestTopic()
	}
	return v, nil
}

// Save sets the person's topic and which events to send there. An empty topic
// removes their settings. Types left out keep what they had (or everything for a
// first save).
func (u *UserSettings) Save(sub, topic string, types []string) (UserView, error) {
	topic = strings.TrimSpace(topic)
	if topic == "" {
		if err := u.store.DeletePrefs(sub); err != nil {
			return UserView{}, err
		}
		return u.View(sub)
	}
	cfg := u.settings.Resolved().Config
	if !cfg.Usable() {
		return UserView{}, invalid("Notifications are not set up yet.")
	}
	if !ValidTopic(topic) {
		return UserView{}, invalid("A topic is letters, digits, - and _, up to 64 characters.")
	}
	if strings.EqualFold(topic, cfg.Topic) {
		return UserView{}, invalid("Choose your own topic: that one is the shared channel.")
	}
	var chosen []Type
	if types == nil {
		if p, found, err := u.store.Prefs(sub); err != nil {
			return UserView{}, err
		} else if found {
			chosen = p.Types
		} else {
			chosen = ForRequesters
		}
	} else {
		var err error
		if chosen, err = ParseTypes(types, ForRequesters); err != nil {
			return UserView{}, invalid("%s.", err.Error())
		}
	}
	switch err := u.store.SetPrefs(sub, topic, chosen); err {
	case nil:
	case ErrTopicTaken:
		return UserView{}, invalid("That topic is already in use. Choose another.")
	default:
		u.log.Error().Err(err).Msg("could not save a person's notification settings")
		return UserView{}, err
	}
	return u.View(sub)
}

// Test sends one message to the person's own topic, so they know it works. It can
// only go to the topic they saved on the server the admin set up: nothing in it is
// chosen by the caller, and it is limited to one every few seconds.
func (u *UserSettings) Test(ctx context.Context, sub string) (TestResult, error) {
	cfg := u.settings.Resolved().Config
	if !cfg.Usable() {
		return TestResult{}, invalid("Notifications are not set up yet.")
	}
	p, found, err := u.store.Prefs(sub)
	if err != nil {
		return TestResult{}, err
	}
	if !found {
		return TestResult{}, invalid("Choose a topic and save it first.")
	}
	u.mu.Lock()
	if last, ok := u.lastTest[sub]; ok && u.now().Sub(last) < testEvery {
		u.mu.Unlock()
		return TestResult{}, invalid("Wait a few seconds before sending another test.")
	}
	u.lastTest[sub] = u.now()
	u.mu.Unlock()

	msg := Message{Topic: p.Topic, Title: "albumarr test notification", Body: "If you can read this, you will hear about your requests here.", Priority: cfg.Priority, Tags: splitTags(cfg.Tags)}
	cctx, cancel := context.WithTimeout(ctx, sendTimeout+2*time.Second)
	defer cancel()
	if err := u.sender.Send(cctx, cfg.Server(), msg); err != nil {
		u.log.Warn().Msg("a person's test notification failed: " + err.Error())
		return TestResult{OK: false, Message: "The test did not go through: " + err.Error() + "."}, nil
	}
	return TestResult{OK: true, Message: "Sent. Check that it arrived."}, nil
}
