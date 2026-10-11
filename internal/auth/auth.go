// Package auth decides what a signed-in person may do. The login itself (the
// session, the identity provider, the local account) is oidcgate's; this turns
// who it says is signed in into one of four roles:
//
//	none      signed in, but not let in: sees a "no access" page and nothing else
//	user      may search and, once requests need approval, ask for albums
//	approver  a user an admin has made an approver: may approve and decline
//	admin     everything, including settings and who the approvers are
//
// Admin and user come from groups at the identity provider, so who is who stays
// there. Approver is the one role albumreq keeps itself, set by an admin.
package auth

import (
	"net/http"
	"os"
	"strings"

	"github.com/Miista/oidcgate"
	"github.com/rs/zerolog"

	"github.com/Miista/albumreq/internal/users"
)

// Role is what someone may do. A higher role may do everything a lower one may.
type Role int

const (
	None Role = iota
	User
	Approver
	Admin
)

func (r Role) String() string {
	switch r {
	case User:
		return "user"
	case Approver:
		return "approver"
	case Admin:
		return "admin"
	}
	return "none"
}

// AtLeast reports whether r may do what min may.
func (r Role) AtLeast(min Role) bool { return r >= min }

// Principal is who a request is from, and what they may do.
type Principal struct {
	Sub   string // stable ID: the identity provider's, or "local:<name>"
	Name  string
	Email string
	Role  Role
	Local bool
}

// Config says how groups map to roles.
type Config struct {
	// AdminGroup makes its members admins. Empty: no group does.
	AdminGroup string
	// UserGroup, when set, is the only way in besides AdminGroup: a signed-in
	// person in neither gets role None. Empty: anyone the identity provider lets
	// sign in is a user.
	UserGroup string
	// GateOn is false when there is no login at all (ALBUMREQ_AUTH_DISABLED): the
	// app is then behind something else, or on loopback, and everyone is an admin.
	GateOn bool
}

// ConfigFromEnv reads ALBUMREQ_ROLE_ADMIN_GROUP and ALBUMREQ_ROLE_USER_GROUP.
func ConfigFromEnv(gateOn bool) Config {
	return Config{
		AdminGroup: strings.TrimSpace(os.Getenv("ALBUMREQ_ROLE_ADMIN_GROUP")),
		UserGroup:  strings.TrimSpace(os.Getenv("ALBUMREQ_ROLE_USER_GROUP")),
		GateOn:     gateOn,
	}
}

// Directory is what Auth needs of the known-users list.
type Directory interface {
	Touch(sub, name, email string, local bool) (users.User, error)
	IsApprover(sub string) bool
}

// Auth resolves a request to a Principal.
type Auth struct {
	cfg  Config
	dir  Directory
	log  zerolog.Logger
	from func(*http.Request) (oidcgate.User, bool) // oidcgate.UserFrom; replaced in tests
}

// New returns an Auth.
func New(cfg Config, dir Directory, log zerolog.Logger) *Auth {
	return &Auth{cfg: cfg, dir: dir, log: log, from: oidcgate.UserFrom}
}

// Principal reports who the request is from. It is false when nobody is signed
// in, which includes a session made before identities were recorded: with a login
// in place, "no identity" must never read as "anyone".
func (a *Auth) Principal(r *http.Request) (Principal, bool) {
	if !a.cfg.GateOn {
		return Principal{Sub: "anonymous", Name: "anonymous", Role: Admin, Local: true}, true
	}
	u, ok := a.from(r)
	if !ok || u.Subject == "" {
		return Principal{}, false
	}
	p := Principal{Sub: u.Subject, Name: u.Name, Email: u.Email, Local: u.Local, Role: a.roleFor(u)}
	if p.Role == None {
		return p, true // known, not let in: never added to the directory
	}
	if _, err := a.dir.Touch(u.Subject, u.Name, u.Email, u.Local); err != nil {
		// Not being able to note that someone was seen must not lock them out.
		a.log.Warn().Err(err).Msg("could not record a sign-in in the users list")
	}
	if p.Role == User && a.dir.IsApprover(u.Subject) {
		p.Role = Approver
	}
	return p, true
}

// roleFor is the role the identity provider's data gives, before approver status.
func (a *Auth) roleFor(u oidcgate.User) Role {
	switch {
	case u.Local:
		return Admin // the account made on first launch is the owner
	case a.cfg.AdminGroup != "" && u.InGroup(a.cfg.AdminGroup):
		return Admin
	case a.cfg.UserGroup == "" || u.InGroup(a.cfg.UserGroup):
		return User
	}
	return None
}
