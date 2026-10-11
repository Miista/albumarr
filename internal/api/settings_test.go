package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/Miista/albumreq/internal/settings"
)

type fakeSettings struct {
	view      settings.View
	patchErr  error
	testErr   error
	patches   []settings.Patch
	testCalls []string
	reads     int
}

func (f *fakeSettings) View() settings.View { f.reads++; return f.view }

func (f *fakeSettings) Patch(_ context.Context, p settings.Patch) (settings.PatchResult, error) {
	f.patches = append(f.patches, p)
	return settings.PatchResult{View: f.view, Warning: "careful"}, f.patchErr
}

func (f *fakeSettings) Test(_ context.Context, url, key string) (settings.TestResult, error) {
	f.testCalls = append(f.testCalls, url+"|"+key)
	return settings.TestResult{OK: true, Message: "Connected"}, f.testErr
}

func settingsMux(f *fakeSettings) *http.ServeMux {
	mux := http.NewServeMux()
	New(&fakeCatalog{}, &fakeCovers{}, f, adminAuth(), &fakeUsers{}, &fakeRequests{}, zerolog.Nop()).Routes(mux)
	return mux
}

func postBody(mux http.Handler, target, body string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

var guard = map[string]string{"X-Albumreq": "1"}

func TestSettingsWritesNeedTheGuard(t *testing.T) {
	f := &fakeSettings{}
	mux := settingsMux(f)
	body := `{"lidarr":{"url":"http://x:1"}}`
	for _, target := range []string{"/api/settings", "/api/settings/test"} {
		if rec := postBody(mux, target, body, nil); rec.Code != http.StatusForbidden {
			t.Errorf("%s without the header: %d", target, rec.Code)
		}
		if rec := postBody(mux, target, body, map[string]string{"X-Albumreq": "1", "Sec-Fetch-Site": "cross-site"}); rec.Code != http.StatusForbidden {
			t.Errorf("%s from another site: %d", target, rec.Code)
		}
	}
	if len(f.patches) != 0 || len(f.testCalls) != 0 {
		t.Error("a refused request reached the service")
	}
}

func TestSaveSettingsPassesOnlyWhatWasSent(t *testing.T) {
	f := &fakeSettings{}
	rec := postBody(settingsMux(f), "/api/settings", `{"lidarr":{"root_folder":"/music","quality_profile_id":5}}`, guard)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"warning":"careful"`) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	p := f.patches[0]
	if p.URL != nil || p.APIKey != nil || *p.RootFolder != "/music" || *p.QualityProfileID != 5 {
		t.Errorf("fields that were not sent must stay nil: %+v", p)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Error("settings responses must not be cached")
	}
}

func TestSaveSettingsRefusesBadBodies(t *testing.T) {
	f := &fakeSettings{}
	mux := settingsMux(f)
	for name, body := range map[string]string{
		"not json":       `nope`,
		"unknown field":  `{"lidarr":{"url":"http://x"},"admin":true}`,
		"unknown nested": `{"lidarr":{"password":"x"}}`,
		"wrong type":     `{"lidarr":{"quality_profile_id":"five"}}`,
		"trailing data":  `{"lidarr":{}} {"lidarr":{}}`,
		"too large":      `{"lidarr":{"url":"` + strings.Repeat("a", 20000) + `"}}`,
		"empty":          ``,
	} {
		if rec := postBody(mux, "/api/settings", body, guard); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", name, rec.Code)
		}
	}
	if len(f.patches) != 0 {
		t.Error("a malformed body reached the service")
	}
}

func TestSettingsErrorsMapToStatusCodes(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		code int
		body string
	}{
		"read-only":  {settings.ErrReadOnly, http.StatusForbidden, "read-only"},
		"validation": {&settings.ValidationError{Message: "Lidarr rejected the API key."}, http.StatusBadRequest, "rejected the API key"},
		"internal":   {errors.New("open /app/data/config.json: permission denied"), http.StatusInternalServerError, "Could not save"},
	} {
		rec := postBody(settingsMux(&fakeSettings{patchErr: tc.err}), "/api/settings", `{"lidarr":{}}`, guard)
		if rec.Code != tc.code || !strings.Contains(rec.Body.String(), tc.body) {
			t.Errorf("%s: %d %q", name, rec.Code, rec.Body)
		}
		if strings.Contains(rec.Body.String(), "/app/data") {
			t.Errorf("%s: an internal path leaked to the client", name)
		}
	}
}

func TestTestEndpointPassesWhatWasTyped(t *testing.T) {
	f := &fakeSettings{}
	rec := postBody(settingsMux(f), "/api/settings/test", `{"url":"http://lidarr:8686","api_key":"k"}`, guard)
	if rec.Code != http.StatusOK || f.testCalls[0] != "http://lidarr:8686|k" {
		t.Errorf("%d %v", rec.Code, f.testCalls)
	}
	if rec := postBody(settingsMux(f), "/api/settings/test", `{"url":"x","extra":1}`, guard); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown field: %d", rec.Code)
	}
}

func TestGetSettingsIsNotCached(t *testing.T) {
	rec := get(settingsMux(&fakeSettings{}), "/api/settings")
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("%d %v", rec.Code, rec.Header())
	}
}
