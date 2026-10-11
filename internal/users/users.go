// Package users remembers who has signed in, so an admin can pick approvers from
// real people. The identity provider stays the source of truth for who someone
// is and which groups they are in; this keeps only what the identity provider
// cannot know: whether an admin made them an approver.
package users

import (
	"database/sql"
	"errors"
	"time"

	"github.com/Miista/albumreq/internal/db"
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

// ErrNotFound means nobody with that ID has signed in.
var ErrNotFound = errors.New("users: no such user")

// touchEvery bounds how often a returning user is written. Last-seen is for
// orientation, not accounting, so an hour is plenty.
const touchEvery = time.Hour

// Directory is the set of known users, in the users table.
type Directory struct {
	db  *db.DB
	now func() time.Time
}

// New returns a Directory on the database.
func New(d *db.DB) *Directory { return &Directory{db: d, now: time.Now} }

const columns = `sub, name, email, local, approver, first_seen, last_seen`

func scan(row interface{ Scan(...any) error }) (User, error) {
	var u User
	var first, last int64
	if err := row.Scan(&u.Sub, &u.Name, &u.Email, &u.Local, &u.Approver, &first, &last); err != nil {
		return User{}, err
	}
	u.FirstSeen, u.LastSeen = time.Unix(first, 0).UTC(), time.Unix(last, 0).UTC()
	return u, nil
}

func (d *Directory) get(sub string) (User, bool, error) {
	u, err := scan(d.db.QueryRow(`SELECT `+columns+` FROM users WHERE sub = ?`, sub))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return User{}, false, nil
	case err != nil:
		return User{}, false, err
	}
	return u, true, nil
}

// Touch records that the person was seen, adding them if new. It writes only when
// they are new, their name or email changed, or the last write was over an hour
// ago, so an ordinary page load costs no write. It never changes the approver
// flag. The returned User is what is stored.
func (d *Directory) Touch(sub, name, email string, local bool) (User, error) {
	if sub == "" {
		return User{}, errors.New("users: a user needs a subject")
	}
	now := d.now()
	cur, found, err := d.get(sub)
	if err != nil {
		return User{}, err
	}
	if found && cur.Name == name && cur.Email == email && cur.Local == local && now.Sub(cur.LastSeen) < touchEvery {
		return cur, nil
	}
	// An upsert, so two sign-ins at once cannot both try to insert: the second
	// updates. approver is not in the update list, so it is never reset.
	if _, err := d.db.Exec(`INSERT INTO users (`+columns+`) VALUES (?, ?, ?, ?, 0, ?, ?)
		ON CONFLICT(sub) DO UPDATE SET name = excluded.name, email = excluded.email, local = excluded.local, last_seen = excluded.last_seen`,
		sub, name, email, local, now.Unix(), now.Unix()); err != nil {
		return User{}, err
	}
	u, _, err := d.get(sub)
	return u, err
}

// Get returns the user with that ID. A database error reads as "not found".
func (d *Directory) Get(sub string) (User, bool) {
	u, ok, _ := d.get(sub)
	return u, ok
}

// IsApprover reports whether an admin made the person an approver. If the
// database cannot be read the answer is no: a failure never grants anything.
func (d *Directory) IsApprover(sub string) bool {
	u, ok, err := d.get(sub)
	return err == nil && ok && u.Approver
}

// List returns everyone, by name.
func (d *Directory) List() ([]User, error) {
	rows, err := d.db.Query(`SELECT ` + columns + ` FROM users ORDER BY name, sub`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []User{}
	for rows.Next() {
		u, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// SetApprover makes the person an approver or not.
func (d *Directory) SetApprover(sub string, approver bool) error {
	res, err := d.db.Exec(`UPDATE users SET approver = ? WHERE sub = ?`, approver, sub)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrNotFound
	}
	return nil
}
