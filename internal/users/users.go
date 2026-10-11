// Package users remembers who has signed in, so an admin can pick approvers from
// real people. The identity provider stays the source of truth for who someone
// is and which groups they are in; this keeps only what the identity provider
// cannot know: whether an admin made them an approver.
package users

import (
	"errors"
	"path/filepath"
	"sort"
	"time"

	"github.com/Miista/albumreq/internal/jsonfile"
)

// User is a person who has signed in.
type User struct {
	Sub       string    `json:"sub"` // the identity provider's stable ID
	Name      string    `json:"name"`
	Email     string    `json:"email,omitempty"`
	Local     bool      `json:"local,omitempty"`
	Approver  bool      `json:"approver,omitempty"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
}

type state struct {
	Users []User `json:"users"`
}

// ErrNotFound means nobody with that ID has signed in.
var ErrNotFound = errors.New("users: no such user")

// touchEvery bounds how often a returning user rewrites the file. Last-seen is
// for orientation, not accounting, so an hour is plenty.
const touchEvery = time.Hour

// Directory is the set of known users, in dataDir/users.json.
type Directory struct {
	f   *jsonfile.File[state]
	now func() time.Time
}

// Open loads dataDir/users.json, creating it if need be.
func Open(dataDir string) (*Directory, error) {
	f, err := jsonfile.Open[state](filepath.Join(dataDir, "users.json"))
	if err != nil {
		return nil, err
	}
	return &Directory{f: f, now: time.Now}, nil
}

// Touch records that the person was seen, adding them if new. It writes only when
// they are new, their name or email changed, or the last write was over an hour
// ago, so an ordinary page load costs no disk write. It never changes the
// approver flag. The returned User is what is stored.
func (d *Directory) Touch(sub, name, email string, local bool) (User, error) {
	if sub == "" {
		return User{}, errors.New("users: a user needs a subject")
	}
	now := d.now()
	if cur, ok := d.Get(sub); ok && cur.Name == name && cur.Email == email && cur.Local == local && now.Sub(cur.LastSeen) < touchEvery {
		return cur, nil
	}
	var out User
	err := d.f.Update(func(s *state) {
		for i := range s.Users {
			if s.Users[i].Sub == sub {
				s.Users[i].Name, s.Users[i].Email, s.Users[i].Local, s.Users[i].LastSeen = name, email, local, now
				out = s.Users[i]
				return
			}
		}
		out = User{Sub: sub, Name: name, Email: email, Local: local, FirstSeen: now, LastSeen: now}
		s.Users = append(s.Users, out)
	})
	return out, err
}

// Get returns the user with that ID.
func (d *Directory) Get(sub string) (User, bool) {
	for _, u := range d.f.View().Users {
		if u.Sub == sub {
			return u, true
		}
	}
	return User{}, false
}

// IsApprover reports whether an admin made the person an approver.
func (d *Directory) IsApprover(sub string) bool {
	u, ok := d.Get(sub)
	return ok && u.Approver
}

// List returns everyone, by name.
func (d *Directory) List() []User {
	out := d.f.View().Users
	sort.Slice(out, func(i, j int) bool {
		if a, b := out[i].Name, out[j].Name; a != b {
			return a < b
		}
		return out[i].Sub < out[j].Sub
	})
	return out
}

// SetApprover makes the person an approver or not.
func (d *Directory) SetApprover(sub string, approver bool) error {
	found := false
	err := d.f.Update(func(s *state) {
		for i := range s.Users {
			if s.Users[i].Sub == sub {
				s.Users[i].Approver = approver
				found = true
			}
		}
	})
	if err != nil {
		return err
	}
	if !found {
		return ErrNotFound
	}
	return nil
}
