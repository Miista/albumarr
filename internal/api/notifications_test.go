package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/Miista/albumreq/internal/auth"
	"github.com/Miista/albumreq/internal/catalog"
	"github.com/Miista/albumreq/internal/notify"
	"github.com/Miista/albumreq/internal/requests"
	"github.com/Miista/albumreq/internal/settings"
)

func notifyRig(role auth.Role) *rig {
	rg := newRig(as(role))
	return rg
}

// ---- the admin's side ----

func TestAdminSavesTheNtfySettingsFieldByField(t *testing.T) {
	rg := notifyRig(auth.Admin)
	body := `{"ntfy":{"enabled":true,"url":"https://ntfy.example.com","topic":"approvers","tags":"eyes","auth":"token","token":"tk_x","priority":4,"embed_poster":true,"types":["pending","declined"],"app_url":"http://albumreq.example"}}`
	rec := call(rg, "POST", "/api/notifications", body)
	if rec.Code != 200 || len(rg.notifyA.patches) != 1 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	p := rg.notifyA.patches[0]
	if !*p.Enabled || *p.URL != "https://ntfy.example.com" || *p.Topic != "approvers" || *p.Tags != "eyes" || *p.Auth != "token" || *p.Token != "tk_x" ||
		*p.Priority != 4 || !*p.EmbedPoster || !reflect.DeepEqual(*p.Types, []string{"pending", "declined"}) || *p.AppURL != "http://albumreq.example" {
		t.Errorf("%+v", p)
	}
	if p.Username != nil || p.Password != nil {
		t.Errorf("fields that were not sent must stay nil, so a save cannot wipe what it did not mention: %+v", p)
	}
	if !strings.Contains(rec.Body.String(), `"settings"`) || rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("%s %v", rec.Body, rec.Header())
	}
}

func TestAdminBodiesAreValidated(t *testing.T) {
	for name, body := range map[string]string{
		"not json":      `nope`,
		"unknown field": `{"ntfy":{"topic":"x","role":"admin"}}`,
		"wrong type":    `{"ntfy":{"priority":"high"}}`,
		"another agent": `{"discord":{"url":"x"}}`,
		"empty":         ``,
		"too large":     `{"ntfy":{"tags":"` + strings.Repeat("a", 20000) + `"}}`,
	} {
		rg := notifyRig(auth.Admin)
		if rec := call(rg, "POST", "/api/notifications", body); rec.Code != 400 || len(rg.notifyA.patches) != 0 {
			t.Errorf("%s: %d patches=%d", name, rec.Code, len(rg.notifyA.patches))
		}
	}
	for name, body := range map[string]string{
		"unknown field": `{"url":"x","topic":"t","password_hint":"x"}`,
		"wrong type":    `{"priority":"high"}`,
		"not json":      `nope`,
	} {
		rg := notifyRig(auth.Admin)
		if rec := call(rg, "POST", "/api/notifications/test", body); rec.Code != 400 || len(rg.notifyA.tests) != 0 {
			t.Errorf("test %s: %d", name, rec.Code)
		}
	}
}

func TestNotificationErrorsMapToTheRightStatusWithoutLeaking(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want int
		body string
	}{
		"a validation message":  {&settings.ValidationError{Message: "A topic is letters, digits, - and _."}, 400, "A topic is letters"},
		"the page is read-only": {settings.ErrReadOnly, 403, "read-only"},
		"a database error":      {errors.New("open /app/data/albumreq.db: disk full"), 500, "Could not save"},
	} {
		rg := notifyRig(auth.Admin)
		rg.notifyA.err = tc.err
		rec := call(rg, "POST", "/api/notifications", `{"ntfy":{"topic":"x"}}`)
		if rec.Code != tc.want || !strings.Contains(rec.Body.String(), tc.body) || strings.Contains(rec.Body.String(), "albumreq.db") {
			t.Errorf("%s: %d %q", name, rec.Code, rec.Body)
		}
	}
}

func TestTheTestPassesTheFormThrough(t *testing.T) {
	rg := notifyRig(auth.Admin)
	rec := call(rg, "POST", "/api/notifications/test", `{"url":"https://n.example","topic":"t","auth":"basic","username":"u","password":"pw","priority":2,"tags":"eyes"}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	want := notify.TestInput{URL: "https://n.example", Topic: "t", Auth: "basic", Username: "u", Password: "pw", Priority: 2, Tags: "eyes"}
	if rg.notifyA.tests[0] != want {
		t.Errorf("%+v", rg.notifyA.tests[0])
	}
}

// ---- each person's side ----

func TestAPersonsCallsAreAlwaysAboutThemselves(t *testing.T) {
	rg := notifyRig(auth.User)
	call(rg, "GET", "/api/me/notifications", "")
	call(rg, "POST", "/api/me/notifications", `{"topic":"mine","types":["approved"]}`)
	call(rg, "POST", "/api/me/notifications/test", `{}`)
	if !reflect.DeepEqual(rg.notifyU.views, []string{"sub-user"}) || rg.notifyU.saves[0].sub != "sub-user" || !reflect.DeepEqual(rg.notifyU.tests, []string{"sub-user"}) {
		t.Errorf("%+v", rg.notifyU)
	}
	// Nothing in the body can name someone else.
	for _, body := range []string{`{"topic":"x","sub":"sub-admin"}`, `{"topic":"x","user":"sub-admin"}`} {
		before := len(rg.notifyU.saves)
		if rec := call(rg, "POST", "/api/me/notifications", body); rec.Code != 400 || len(rg.notifyU.saves) != before {
			t.Errorf("%s: %d", body, rec.Code)
		}
	}
}

func TestLeavingTheEventsOutIsNotTheSameAsChoosingNone(t *testing.T) {
	rg := notifyRig(auth.User)
	call(rg, "POST", "/api/me/notifications", `{"topic":"mine"}`)
	call(rg, "POST", "/api/me/notifications", `{"topic":"mine","types":[]}`)
	call(rg, "POST", "/api/me/notifications", `{"topic":"mine","types":["declined"]}`)
	s := rg.notifyU.saves
	if !s[0].typesNil || s[1].typesNil || len(s[1].types) != 0 || s[2].typesNil || !reflect.DeepEqual(s[2].types, []string{"declined"}) {
		t.Errorf("%+v: left out means keep, an empty list means none", s)
	}
}

func TestAPersonsRefusalsAre400WithTheMessage(t *testing.T) {
	rg := notifyRig(auth.User)
	rg.notifyU.err = &settings.ValidationError{Message: "That topic is already in use. Choose another."}
	for _, tc := range []struct{ path, body string }{
		{"/api/me/notifications", `{"topic":"taken"}`},
		{"/api/me/notifications/test", `{}`},
	} {
		rec := call(rg, "POST", tc.path, tc.body)
		if rec.Code != 400 || !strings.Contains(rec.Body.String(), "already in use") {
			t.Errorf("%s: %d %s", tc.path, rec.Code, rec.Body)
		}
	}
}

func TestEveryNotificationWriteNeedsTheCSRFHeader(t *testing.T) {
	for _, tc := range []struct {
		path, body string
		role       auth.Role
	}{
		{"/api/notifications", `{"ntfy":{}}`, auth.Admin},
		{"/api/notifications/test", `{}`, auth.Admin},
		{"/api/me/notifications", `{"topic":"x"}`, auth.User},
		{"/api/me/notifications/test", `{}`, auth.User},
	} {
		rg := notifyRig(tc.role)
		for name, hdr := range map[string]map[string]string{"no header": nil, "cross-site": {"X-Albumreq": "1", "Sec-Fetch-Site": "cross-site"}} {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest("POST", tc.path, strings.NewReader(tc.body))
			for k, v := range hdr {
				req.Header.Set(k, v)
			}
			rg.mux.ServeHTTP(rec, req)
			if rec.Code != 403 || len(rg.notifyA.patches)+len(rg.notifyA.tests)+len(rg.notifyU.saves)+len(rg.notifyU.tests) != 0 {
				t.Errorf("%s %s: %d: a page the person merely visits must not change their notifications or send messages", tc.path, name, rec.Code)
			}
		}
	}
}

func TestWithoutNotificationsWiredTheRoutesAreNotThere(t *testing.T) {
	mux := http.NewServeMux()
	New(&fakeCatalog{}, &fakeCovers{}, &fakeSettings{}, adminAuth(), &fakeUsers{}, &fakeRequests{}, &fakeIssues{}, zerolog.Nop()).Routes(mux)
	for _, tc := range []struct{ method, path string }{
		{"GET", "/api/notifications"}, {"POST", "/api/notifications"}, {"POST", "/api/notifications/test"},
		{"GET", "/api/me/notifications"}, {"POST", "/api/me/notifications"}, {"POST", "/api/me/notifications/test"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`))
		req.Header.Set("X-Albumreq", "1")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != 404 {
			t.Errorf("%s %s: %d, want 404", tc.method, tc.path, rec.Code)
		}
	}
}

type fakeProgress struct{ asked []string }

func (f *fakeProgress) Progress(_ context.Context, ids []string) map[string]catalog.Progress {
	f.asked = ids
	out := map[string]catalog.Progress{}
	for _, id := range ids {
		out[id] = catalog.Progress{Status: catalog.StatusProcessing, Stage: "downloading"}
	}
	return out
}

func TestRequestsListCarriesProgressForApprovedRequestsOnly(t *testing.T) {
	rq := &fakeRequests{list: []requests.Record{
		{ID: 1, AlbumID: id1, State: requests.Approved},
		{ID: 2, AlbumID: id2, State: requests.Pending},
		{ID: 3, AlbumID: "x", State: requests.Declined},
	}}
	fp := &fakeProgress{}
	mux := http.NewServeMux()
	New(&fakeCatalog{}, &fakeCovers{}, &fakeSettings{}, adminAuth(), &fakeUsers{}, rq, &fakeIssues{}, zerolog.Nop()).Progress(fp).Routes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/requests?scope=mine", nil))
	var body struct {
		Progress map[string]catalog.Progress `json:"progress"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || rec.Code != 200 {
		t.Fatalf("%d %v %s", rec.Code, err, rec.Body)
	}
	if len(fp.asked) != 1 || fp.asked[0] != id1 {
		t.Errorf("only the approved one is asked about: %v", fp.asked)
	}
	if p := body.Progress[id1]; p.Stage != "downloading" {
		t.Errorf("progress: %+v", body.Progress)
	}
}

func TestRequestsListWithoutProgressStillWorks(t *testing.T) {
	rq := &fakeRequests{list: []requests.Record{{ID: 1, AlbumID: id1, State: requests.Approved}}}
	rec := httptest.NewRecorder()
	muxWith(rq, adminAuth()).ServeHTTP(rec, httptest.NewRequest("GET", "/api/requests?scope=mine", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"progress":{}`) {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
}
