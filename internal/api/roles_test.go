package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/Miista/albumreq/internal/auth"
	"github.com/Miista/albumreq/internal/issues"
	"github.com/Miista/albumreq/internal/notify"
	"github.com/Miista/albumreq/internal/requests"
	"github.com/Miista/albumreq/internal/users"
)

type fakeAuth struct {
	p  auth.Principal
	ok bool
}

func (f fakeAuth) Principal(*http.Request) (auth.Principal, bool) { return f.p, f.ok }

func adminAuth() fakeAuth { return as(auth.Admin) }

func as(r auth.Role) fakeAuth {
	return fakeAuth{p: auth.Principal{Sub: "sub-" + r.String(), Name: r.String(), Role: r}, ok: true}
}

type fakeUsers struct {
	list      []users.User
	approvers map[string]bool
	err       error
	calls     int
	reads     int
	listErr   error
}

func (f *fakeUsers) List() ([]users.User, error) { f.reads++; return f.list, f.listErr }
func (f *fakeUsers) SetApprover(sub string, a bool) error {
	f.calls++
	if f.err != nil {
		return f.err
	}
	if f.approvers == nil {
		f.approvers = map[string]bool{}
	}
	f.approvers[sub] = a
	return nil
}

// fakeRequests stands in for the approval flow and records what it was asked.
type fakeRequests struct {
	out     requests.Outcome
	err     error
	calls   int // writes: submit, approve, decline
	by      auth.Principal
	listErr error

	submitted string
	ask       requests.Ask
	approved  int64
	declined  int64
	reason    string

	list      []requests.Record
	listScope requests.Scope
	listCalls int

	lastApproved    requests.Record
	lastApprovedErr error
	pending         map[string]requests.Record
	pendingErr      error
	pendingCalls    int
}

func (f *fakeRequests) Submit(_ context.Context, p auth.Principal, a requests.Ask) (requests.Outcome, error) {
	f.calls++
	f.by, f.submitted, f.ask = p, a.AlbumID, a
	return f.out, f.err
}
func (f *fakeRequests) Approve(_ context.Context, p auth.Principal, id int64) (requests.Outcome, error) {
	f.calls++
	f.by, f.approved = p, id
	return f.out, f.err
}
func (f *fakeRequests) Decline(_ context.Context, p auth.Principal, id int64, reason string) (requests.Outcome, error) {
	f.calls++
	f.by, f.declined, f.reason = p, id, reason
	return f.out, f.err
}
func (f *fakeRequests) List(p auth.Principal, scope requests.Scope) ([]requests.Record, error) {
	f.listCalls++
	f.by, f.listScope = p, scope
	return f.list, f.listErr
}
func (f *fakeRequests) LastApproved(string) (requests.Record, bool, error) {
	return f.lastApproved, f.lastApproved.ID != 0, f.lastApprovedErr
}
func (f *fakeRequests) Pending(ids []string) (map[string]requests.Record, error) {
	f.pendingCalls++
	return f.pending, f.pendingErr
}

// fakeIssues stands in for the checks.
type fakeIssues struct {
	list       []issues.Issue
	issueCalls int
	freshCalls int
}

func (f *fakeIssues) Issues(context.Context) []issues.Issue { f.issueCalls++; return f.list }
func (f *fakeIssues) Fresh(context.Context) []issues.Issue  { f.freshCalls++; return f.list }

// requestBody is what a card sends with a request.
const requestBody = `{"artistId":"a74b1b7f-71a5-4011-9441-d0b5e4122711","title":"OK Computer","artist":"Radiohead"}`

// fakeNotifyAdmin and fakeNotifyUser stand in for the notification settings.
type fakeNotifyAdmin struct {
	patches []notify.Patch
	tests   []notify.TestInput
	reads   int
	err     error
	testErr error
}

func (f *fakeNotifyAdmin) View() notify.View { f.reads++; return notify.View{Problems: []string{}} }
func (f *fakeNotifyAdmin) Patch(_ context.Context, p notify.Patch) (notify.View, error) {
	f.patches = append(f.patches, p)
	return notify.View{Problems: []string{}}, f.err
}
func (f *fakeNotifyAdmin) Test(_ context.Context, in notify.TestInput) (notify.TestResult, error) {
	f.tests = append(f.tests, in)
	return notify.TestResult{OK: true, Message: "Sent."}, f.testErr
}

type fakeNotifyUser struct {
	views []string
	saves []userSave
	tests []string
	err   error
}

type userSave struct {
	sub, topic string
	types      []string
	typesNil   bool
}

func (f *fakeNotifyUser) View(sub string) (notify.UserView, error) {
	f.views = append(f.views, sub)
	return notify.UserView{Available: true, Topic: "my-topic"}, f.err
}
func (f *fakeNotifyUser) Save(sub, topic string, types []string) (notify.UserView, error) {
	f.saves = append(f.saves, userSave{sub, topic, types, types == nil})
	return notify.UserView{Available: true, Topic: topic}, f.err
}
func (f *fakeNotifyUser) Test(_ context.Context, sub string) (notify.TestResult, error) {
	f.tests = append(f.tests, sub)
	return notify.TestResult{OK: true, Message: "Sent."}, f.err
}

// routes is every endpoint and the lowest role that may use it.
var routes = []struct {
	method, path, body string
	min                auth.Role
}{
	{"GET", "/api/search?q=radiohead", "", auth.User},
	{"GET", "/api/album/" + id1, "", auth.User},
	{"GET", "/api/cover/" + id1, "", auth.User},
	{"POST", "/api/album/" + id1 + "/request", requestBody, auth.User},
	{"GET", "/api/requests", "", auth.User},
	{"POST", "/api/requests/1/approve", "", auth.Approver},
	{"POST", "/api/requests/1/decline", `{"reason":"no"}`, auth.Approver},
	{"GET", "/api/settings", "", auth.Admin},
	{"POST", "/api/settings", `{"lidarr":{"url":"http://x:1"}}`, auth.Admin},
	{"POST", "/api/settings/test", `{"url":"http://x:1","api_key":"k"}`, auth.Admin},
	{"GET", "/api/notifications", "", auth.Admin},
	{"POST", "/api/notifications", `{"ntfy":{"topic":"approvers"}}`, auth.Admin},
	{"POST", "/api/notifications/test", `{"url":"https://n.example","topic":"t"}`, auth.Admin},
	{"GET", "/api/me/notifications", "", auth.User},
	{"POST", "/api/me/notifications", `{"topic":"my-topic"}`, auth.User},
	{"POST", "/api/me/notifications/test", `{}`, auth.User},
	{"GET", "/api/issues", "", auth.Admin},
	{"GET", "/api/issues?fresh=1", "", auth.Admin},
	{"GET", "/api/users", "", auth.Admin},
	{"POST", "/api/users/approver", `{"sub":"sub-user","approver":true}`, auth.Admin},
}

type rig struct {
	mux      *http.ServeMux
	cat      *fakeCatalog
	covers   *fakeCovers
	settings *fakeSettings
	users    *fakeUsers
	requests *fakeRequests
	issues   *fakeIssues
	notifyA  *fakeNotifyAdmin
	notifyU  *fakeNotifyUser
}

func newRig(a Authenticator) *rig {
	r := &rig{cat: &fakeCatalog{}, covers: &fakeCovers{data: []byte("img"), ct: "image/png"}, settings: &fakeSettings{}, users: &fakeUsers{},
		requests: &fakeRequests{out: requests.Outcome{Result: "added"}}, issues: &fakeIssues{},
		notifyA: &fakeNotifyAdmin{}, notifyU: &fakeNotifyUser{}}
	r.mux = http.NewServeMux()
	New(r.cat, r.covers, r.settings, a, r.users, r.requests, r.issues, zerolog.Nop()).Notifications(r.notifyA, r.notifyU).Routes(r.mux)
	return r
}

// touched reports whether anything behind the handlers was reached.
func (r *rig) touched() bool {
	return r.cat.searchCalls+r.cat.detailCalls != 0 || r.requests.calls+r.requests.listCalls != 0 || r.covers.lastID != "" ||
		len(r.settings.patches)+len(r.settings.testCalls)+r.settings.reads != 0 || r.users.calls+r.users.reads != 0 || r.issues.issueCalls+r.issues.freshCalls != 0 || len(r.notifyA.patches)+len(r.notifyA.tests)+r.notifyA.reads != 0 || len(r.notifyU.views)+len(r.notifyU.saves)+len(r.notifyU.tests) != 0
}

func call(r *rig, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("X-Albumreq", "1") // the CSRF guard is not what is under test here
	rec := httptest.NewRecorder()
	r.mux.ServeHTTP(rec, req)
	return rec
}

func TestEveryRouteAgainstEveryRole(t *testing.T) {
	roles := []struct {
		name string
		a    fakeAuth
		role auth.Role
	}{
		{"user", as(auth.User), auth.User},
		{"approver", as(auth.Approver), auth.Approver},
		{"admin", as(auth.Admin), auth.Admin},
	}
	for _, rt := range routes {
		for _, r := range roles {
			rg := newRig(r.a)
			rec := call(rg, rt.method, rt.path, rt.body)
			if r.role.AtLeast(rt.min) {
				if rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
					t.Errorf("%s %s as %s: %d, want it allowed", rt.method, rt.path, r.name, rec.Code)
				}
				if !rg.touched() {
					t.Errorf("%s %s as %s: allowed, but nothing behind it was reached", rt.method, rt.path, r.name)
				}
			} else {
				if rec.Code != http.StatusForbidden {
					t.Errorf("%s %s as %s: %d, want 403", rt.method, rt.path, r.name, rec.Code)
				}
				if rg.touched() {
					t.Errorf("%s %s as %s: refused, but the action still ran", rt.method, rt.path, r.name)
				}
			}
		}
	}
}

func TestSomeoneWhoWasNotLetInGetsNothing(t *testing.T) {
	for _, rt := range routes {
		rg := newRig(as(auth.None))
		if rec := call(rg, rt.method, rt.path, rt.body); rec.Code != http.StatusForbidden || rg.touched() {
			t.Errorf("%s %s as none: %d touched=%v, want 403 and nothing run", rt.method, rt.path, rec.Code, rg.touched())
		}
	}
}

func TestNobodySignedInGetsA401OnEveryRoute(t *testing.T) {
	for _, rt := range routes {
		rg := newRig(fakeAuth{})
		rec := call(rg, rt.method, rt.path, rt.body)
		if rec.Code != http.StatusUnauthorized || rg.touched() {
			t.Errorf("%s %s anonymous: %d touched=%v, want 401 and nothing run", rt.method, rt.path, rec.Code, rg.touched())
		}
	}
	if rec := call(newRig(fakeAuth{}), "GET", "/api/me", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("/api/me anonymous: %d", rec.Code)
	}
}

func TestHealthzNeedsNoOneToBeSignedIn(t *testing.T) {
	if rec := call(newRig(fakeAuth{}), "GET", "/healthz", ""); rec.Code != http.StatusOK {
		t.Errorf("healthz: %d", rec.Code)
	}
}

func TestMeTellsEveryoneWhoTheyAreAndLeaksNothingToAnOutsider(t *testing.T) {
	rg := newRig(as(auth.Approver))
	rg.settings.view.Configured, rg.settings.view.RequestsEnabled = true, true
	rec := call(rg, "GET", "/api/me", "")
	for _, want := range []string{`"role":"approver"`, `"requests_enabled":true`, `"lidarr_configured":true`} {
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("%d %s lacks %s", rec.Code, rec.Body, want)
		}
	}
	rg = newRig(as(auth.None))
	rg.settings.view.Configured = true
	rec = call(rg, "GET", "/api/me", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"role":"none"`) {
		t.Errorf("someone turned away still needs to be told so: %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "lidarr") || strings.Contains(rec.Body.String(), "requests_enabled") {
		t.Errorf("someone turned away must learn nothing about the setup: %s", rec.Body)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Error("who you are must not be cached")
	}
}

func TestSettingsViewIsNeverSentToANonAdmin(t *testing.T) {
	for _, role := range []auth.Role{auth.User, auth.Approver} {
		rg := newRig(as(role))
		rg.settings.view.Values.URL = "http://lidarr.internal:8686"
		rec := call(rg, "GET", "/api/settings", "")
		if rec.Code != 403 || strings.Contains(rec.Body.String(), "lidarr.internal") {
			t.Errorf("%v: %d %s", role, rec.Code, rec.Body)
		}
	}
}

func TestApproverEndpointValidation(t *testing.T) {
	for name, body := range map[string]string{
		"no sub":           `{"approver":true}`,
		"no approver":      `{"sub":"x"}`,
		"empty sub":        `{"sub":"","approver":true}`,
		"unknown field":    `{"sub":"x","approver":true,"role":"admin"}`,
		"not json":         `nope`,
		"approver as text": `{"sub":"x","approver":"yes"}`,
	} {
		rg := newRig(adminAuth())
		if rec := call(rg, "POST", "/api/users/approver", body); rec.Code != 400 || rg.users.calls != 0 {
			t.Errorf("%s: %d calls=%d", name, rec.Code, rg.users.calls)
		}
	}
}

func TestApproverEndpointNeedsTheCSRFHeaderToo(t *testing.T) {
	rg := newRig(adminAuth())
	req := httptest.NewRequest("POST", "/api/users/approver", strings.NewReader(`{"sub":"x","approver":true}`))
	rec := httptest.NewRecorder()
	rg.mux.ServeHTTP(rec, req)
	if rec.Code != 403 || rg.users.calls != 0 {
		t.Errorf("an admin's browser tricked by another site: %d calls=%d", rec.Code, rg.users.calls)
	}
}

func TestApproverEndpointAnswersWithTheUpdatedList(t *testing.T) {
	rg := newRig(adminAuth())
	rg.users.list = []users.User{{Sub: "x", Name: "Xavier"}}
	rec := call(rg, "POST", "/api/users/approver", `{"sub":"x","approver":true}`)
	if rec.Code != 200 || !rg.users.approvers["x"] || !strings.Contains(rec.Body.String(), "Xavier") {
		t.Errorf("%d %s %v", rec.Code, rec.Body, rg.users.approvers)
	}
	rg.users.err = users.ErrNotFound
	if rec := call(rg, "POST", "/api/users/approver", `{"sub":"ghost","approver":true}`); rec.Code != 404 {
		t.Errorf("unknown user: %d", rec.Code)
	}
}

func TestAFailedUsersReadIsA500NotAnEmptyList(t *testing.T) {
	rg := newRig(adminAuth())
	rg.users.listErr = errors.New("database is locked: /app/data/albumreq.db")
	rec := call(rg, "GET", "/api/users", "")
	if rec.Code != 500 || strings.Contains(rec.Body.String(), "albumreq.db") {
		t.Errorf("%d %s: an outage must not read as 'nobody has signed in', nor leak a path", rec.Code, rec.Body)
	}
}

func TestIssuesAreAnAdminsAndFreshRunsTheChecksAgain(t *testing.T) {
	rg := newRig(adminAuth())
	rg.issues.list = []issues.Issue{
		{ID: "no-none-profile", Severity: issues.Error, Title: "Lidarr has no metadata profile named “None”"},
		{ID: "login-off", Severity: issues.Warning, Title: "The login is off"},
		{ID: "x", Severity: issues.Info, Title: "a note"},
	}
	rec := call(rg, "GET", "/api/issues", "")
	if rec.Code != 200 || rg.issues.issueCalls != 1 || rg.issues.freshCalls != 0 || !strings.Contains(rec.Body.String(), `"count":2`) || !strings.Contains(rec.Body.String(), "no-none-profile") {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Error("what is wrong now must not be cached by the browser")
	}
	call(rg, "GET", "/api/issues?fresh=1", "")
	if rg.issues.freshCalls != 1 {
		t.Errorf("fresh=1 must run the checks again, fresh calls = %d", rg.issues.freshCalls)
	}
	for _, q := range []string{"?fresh=0", "?fresh=true", "?fresh="} {
		before := rg.issues.freshCalls
		call(rg, "GET", "/api/issues"+q, "")
		if rg.issues.freshCalls != before {
			t.Errorf("%q should not force a fresh run", q)
		}
	}
}

func TestNoIssuesIsAnEmptyListNotNull(t *testing.T) {
	rec := call(newRig(adminAuth()), "GET", "/api/issues", "")
	if !strings.Contains(rec.Body.String(), `"issues":[]`) || !strings.Contains(rec.Body.String(), `"count":0`) {
		t.Errorf("%s", rec.Body)
	}
}

func TestIssuesAreNeverShownToANonAdmin(t *testing.T) {
	for _, role := range []auth.Role{auth.User, auth.Approver, auth.None} {
		rg := newRig(as(role))
		rg.issues.list = []issues.Issue{{ID: "x", Severity: issues.Error, Title: "secret detail about the setup"}}
		rec := call(rg, "GET", "/api/issues", "")
		if rec.Code != 403 || strings.Contains(rec.Body.String(), "secret detail") {
			t.Errorf("%v: %d %s", role, rec.Code, rec.Body)
		}
	}
}
