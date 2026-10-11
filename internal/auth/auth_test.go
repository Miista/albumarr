package auth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Miista/oidcgate"
	"github.com/rs/zerolog"

	"github.com/Miista/albumreq/internal/users"
)

type fakeDir struct {
	approvers map[string]bool
	touched   []string
	touchErr  error
}

func (f *fakeDir) Touch(sub, name, email string, local bool) (users.User, error) {
	f.touched = append(f.touched, sub)
	return users.User{Sub: sub}, f.touchErr
}
func (f *fakeDir) IsApprover(sub string) bool { return f.approvers[sub] }

func resolve(cfg Config, dir *fakeDir, u *oidcgate.User) (Principal, bool) {
	a := New(cfg, dir, zerolog.Nop())
	a.from = func(*http.Request) (oidcgate.User, bool) {
		if u == nil {
			return oidcgate.User{}, false
		}
		return *u, true
	}
	return a.Principal(httptest.NewRequest(http.MethodGet, "/", nil))
}

func idp(sub string, groups ...string) *oidcgate.User {
	return &oidcgate.User{Subject: sub, Name: sub, Groups: groups}
}

func TestRolesFromGroups(t *testing.T) {
	both := Config{GateOn: true, AdminGroup: "admins", UserGroup: "users"}
	adminOnly := Config{GateOn: true, AdminGroup: "admins"}
	open := Config{GateOn: true}
	for _, c := range []struct {
		name string
		cfg  Config
		u    *oidcgate.User
		want Role
	}{
		{"admin group", both, idp("a", "admins"), Admin},
		{"admin who is not in the user group", both, idp("a", "admins"), Admin},
		{"admin and user", both, idp("a", "admins", "users"), Admin},
		{"user group", both, idp("u", "users"), User},
		{"neither group is turned away", both, idp("x", "something-else"), None},
		{"no groups at all is turned away", both, idp("x"), None},
		{"the user group is exact, not a prefix", both, idp("x", "users-extra", "user"), None},
		{"the admin group is case sensitive", both, idp("x", "Admins"), None},
		{"no user group set: any signed-in person is a user", adminOnly, idp("x", "whatever"), User},
		{"no groups configured: everyone is a user, nobody an admin", open, idp("x", "admins"), User},
		{"the local account is the admin", both, &oidcgate.User{Subject: "local:me", Name: "me", Local: true}, Admin},
		{"a local account is admin even with no groups configured", open, &oidcgate.User{Subject: "local:me", Local: true}, Admin},
	} {
		if p, ok := resolve(c.cfg, &fakeDir{}, c.u); !ok || p.Role != c.want {
			t.Errorf("%s: role = %v (ok %v), want %v", c.name, p.Role, ok, c.want)
		}
	}
}

func TestApproverIsSetByAnAdminAndOnlyRaisesAUser(t *testing.T) {
	cfg := Config{GateOn: true, AdminGroup: "admins", UserGroup: "users"}
	dir := &fakeDir{approvers: map[string]bool{"u1": true, "x1": true, "a1": true}}
	if p, _ := resolve(cfg, dir, idp("u1", "users")); p.Role != Approver {
		t.Errorf("a user an admin made approver: %v", p.Role)
	}
	if p, _ := resolve(cfg, dir, idp("u2", "users")); p.Role != User {
		t.Errorf("a user not made approver: %v", p.Role)
	}
	if p, _ := resolve(cfg, dir, idp("a1", "admins")); p.Role != Admin {
		t.Errorf("an admin flagged approver stays admin: %v", p.Role)
	}
	if p, _ := resolve(cfg, dir, idp("x1", "nope")); p.Role != None {
		t.Errorf("the approver flag must not let someone in who the groups turn away: %v", p.Role)
	}
}

func TestNobodyIsNotAnyone(t *testing.T) {
	for _, cfg := range []Config{{GateOn: true}, {GateOn: true, AdminGroup: "a", UserGroup: "u"}} {
		if p, ok := resolve(cfg, &fakeDir{}, nil); ok || p.Role != None {
			t.Errorf("no identity with the login on must be nobody: %+v %v", p, ok)
		}
		if _, ok := resolve(cfg, &fakeDir{}, &oidcgate.User{}); ok {
			t.Error("an identity with no subject must be nobody")
		}
	}
}

func TestWithTheLoginOffEveryoneIsAnAdmin(t *testing.T) {
	dir := &fakeDir{}
	if p, ok := resolve(Config{GateOn: false}, dir, nil); !ok || p.Role != Admin || len(dir.touched) != 0 {
		t.Errorf("%+v %v touched %v", p, ok, dir.touched)
	}
}

func TestOnlyPeopleWhoAreLetInAreRecorded(t *testing.T) {
	cfg := Config{GateOn: true, AdminGroup: "admins", UserGroup: "users"}
	dir := &fakeDir{}
	resolve(cfg, dir, idp("stranger", "other"))
	resolve(cfg, dir, idp("u", "users"))
	resolve(cfg, dir, idp("a", "admins"))
	if len(dir.touched) != 2 || dir.touched[0] != "u" || dir.touched[1] != "a" {
		t.Errorf("touched %v: someone turned away must not appear in the list an admin picks approvers from", dir.touched)
	}
}

func TestAFailingUsersListDoesNotLockAnyoneOut(t *testing.T) {
	cfg := Config{GateOn: true, AdminGroup: "admins"}
	dir := &fakeDir{touchErr: errors.New("disk full")}
	if p, ok := resolve(cfg, dir, idp("a", "admins")); !ok || p.Role != Admin {
		t.Errorf("%+v %v", p, ok)
	}
}

func TestRoleOrderAndNames(t *testing.T) {
	if !(None < User && User < Approver && Approver < Admin) {
		t.Fatal("roles are ordered")
	}
	if !Admin.AtLeast(Approver) || !Approver.AtLeast(User) || User.AtLeast(Approver) || None.AtLeast(User) {
		t.Error("AtLeast")
	}
	for r, s := range map[Role]string{None: "none", User: "user", Approver: "approver", Admin: "admin"} {
		if r.String() != s {
			t.Errorf("%d = %q", r, r.String())
		}
	}
}
