package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type captured struct {
	method, path, auth, ctype string
	body                      map[string]any
	raw                       string
}

type fakeNtfy struct {
	*httptest.Server
	mu   sync.Mutex
	got  []captured
	code int
	resp string
}

func newFakeNtfy(t *testing.T) *fakeNtfy {
	t.Helper()
	f := &fakeNtfy{code: 200, resp: `{"id":"x"}`}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		json.Unmarshal(raw, &body)
		f.mu.Lock()
		f.got = append(f.got, captured{r.Method, r.URL.RequestURI(), r.Header.Get("Authorization"), r.Header.Get("Content-Type"), body, string(raw)})
		code, resp := f.code, f.resp
		f.mu.Unlock()
		w.WriteHeader(code)
		io.WriteString(w, resp)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeNtfy) last() captured {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.got) == 0 {
		return captured{}
	}
	return f.got[len(f.got)-1]
}

func (f *fakeNtfy) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.got)
}

func TestSendPublishesJSONToTheServersRoot(t *testing.T) {
	f := newFakeNtfy(t)
	m := Message{Topic: "albumreq-approvers", Title: "New request: OK Computer", Body: "Radiohead", Click: "http://a/#/approvals", Attach: "https://img/x.jpg", Priority: 4, Tags: []string{"eyes", "warning"}}
	if err := NewClient().Send(context.Background(), Server{URL: f.URL, Auth: AuthNone}, m); err != nil {
		t.Fatal(err)
	}
	g := f.last()
	if g.method != "POST" || g.path != "/" || g.ctype != "application/json" || g.auth != "" {
		t.Errorf("%s %s %s auth=%q", g.method, g.path, g.ctype, g.auth)
	}
	for k, want := range map[string]any{"topic": "albumreq-approvers", "title": "New request: OK Computer", "message": "Radiohead", "priority": float64(4), "click": "http://a/#/approvals", "attach": "https://img/x.jpg"} {
		if g.body[k] != want {
			t.Errorf("%s = %v, want %v", k, g.body[k], want)
		}
	}
	if tags, _ := g.body["tags"].([]any); len(tags) != 2 || tags[0] != "eyes" {
		t.Errorf("tags = %v", g.body["tags"])
	}
	if strings.Contains(g.path, "albumreq-approvers") {
		t.Error("the topic must travel in the body, not the address")
	}
}

func TestSendKeepsAPathPrefixForAReverseProxy(t *testing.T) {
	f := newFakeNtfy(t)
	NewClient().Send(context.Background(), Server{URL: f.URL + "/ntfy/"}, Message{Topic: "t", Body: "b"})
	if f.last().path != "/ntfy/" {
		t.Errorf("path = %q", f.last().path)
	}
}

func TestSendAuthenticates(t *testing.T) {
	f := newFakeNtfy(t)
	c := NewClient()
	c.Send(context.Background(), Server{URL: f.URL, Auth: AuthBasic, Username: "alice", Password: "p:ass w0rd"}, Message{Topic: "t", Body: "b"})
	if f.last().auth != "Basic c29yZW46cDphc3MgdzByZA==" {
		t.Errorf("basic: %q", f.last().auth)
	}
	c.Send(context.Background(), Server{URL: f.URL, Auth: AuthToken, Token: "tk_abc"}, Message{Topic: "t", Body: "b"})
	if f.last().auth != "Bearer tk_abc" {
		t.Errorf("token: %q", f.last().auth)
	}
	// An agent set to no auth must not send leftover credentials from an earlier setting.
	c.Send(context.Background(), Server{URL: f.URL, Auth: AuthNone, Username: "alice", Password: "x", Token: "tk"}, Message{Topic: "t", Body: "b"})
	if f.last().auth != "" {
		t.Errorf("none: sent %q", f.last().auth)
	}
	for _, leaked := range []string{"p:ass w0rd", "tk_abc"} {
		for _, g := range f.got {
			if strings.Contains(g.raw, leaked) {
				t.Errorf("a credential ended up in the message body: %s", g.raw)
			}
		}
	}
}

func TestSendErrorsAreFriendlyAndLeakNothing(t *testing.T) {
	const secret, topic = "tk_super_secret_value", "private-topic-name"
	for name, tc := range map[string]struct {
		code int
		resp string
		want string
	}{
		"unauthorised":      {401, `{"error":"unauthorized"}`, "refused the credentials"},
		"forbidden":         {403, `{"error":"forbidden: topic secret-room"}`, "refused the credentials"},
		"ntfy explains":     {400, `{"code":40001,"error":"topic invalid"}`, "topic invalid"},
		"no explanation":    {502, `<html>Bad gateway</html>`, "answered 502"},
		"a redirect":        {302, ``, "answered 302"},
		"a very long error": {400, `{"error":"` + strings.Repeat("x", 5000) + `"}`, "answered 400"},
	} {
		f := newFakeNtfy(t)
		f.code, f.resp = tc.code, tc.resp
		err := NewClient().Send(context.Background(), Server{URL: f.URL, Auth: AuthToken, Token: secret}, Message{Topic: topic, Body: "b"})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to mention %q", name, err, tc.want)
			continue
		}
		for _, bad := range []string{secret, topic, f.URL, strings.TrimPrefix(f.URL, "http://")} {
			if strings.Contains(err.Error(), bad) {
				t.Errorf("%s: the error leaks %q: %v", name, bad, err)
			}
		}
		if len(err.Error()) > 300 {
			t.Errorf("%s: an unbounded error: %d bytes", name, len(err.Error()))
		}
	}
}

func TestSendDoesNotFollowARedirectSoCredentialsStayPut(t *testing.T) {
	elsewhere := newFakeNtfy(t)
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/", http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()
	err := NewClient().Send(context.Background(), Server{URL: redirector.URL, Auth: AuthToken, Token: "tk"}, Message{Topic: "t", Body: "b"})
	if err == nil {
		t.Error("a redirect is not a success")
	}
	if elsewhere.count() != 0 {
		t.Errorf("the redirect target received %d requests, including the credentials", elsewhere.count())
	}
}

func TestSendToAServerThatIsDownLeaksNoAddress(t *testing.T) {
	f := newFakeNtfy(t)
	url := f.URL
	f.Close()
	err := NewClient().Send(context.Background(), Server{URL: url, Auth: AuthBasic, Username: "u", Password: "hunter2"}, Message{Topic: "private", Body: "b"})
	if err == nil || !strings.Contains(err.Error(), "could not reach the ntfy server") {
		t.Fatalf("err = %v", err)
	}
	for _, bad := range []string{strings.TrimPrefix(url, "http://"), "hunter2", "private"} {
		if strings.Contains(err.Error(), bad) {
			t.Errorf("the error leaks %q: %v", bad, err)
		}
	}
}

func TestSendHonoursTheContext(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(2 * time.Second) }))
	defer slow.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := NewClient().Send(ctx, Server{URL: slow.URL}, Message{Topic: "t", Body: "b"}); err == nil || time.Since(start) > time.Second {
		t.Errorf("err = %v after %v: a cancelled send must stop at once", err, time.Since(start))
	}
}

func TestSendRefusesWhatCannotBeSent(t *testing.T) {
	f := newFakeNtfy(t)
	if err := NewClient().Send(context.Background(), Server{URL: f.URL}, Message{Body: "no topic"}); err == nil || f.count() != 0 {
		t.Errorf("a message with no topic must not reach the server: %v", err)
	}
	if err := NewClient().Send(context.Background(), Server{URL: "http://a b"}, Message{Topic: "t", Body: "b"}); err == nil {
		t.Error("an unusable address is an error")
	}
}

func TestSendCapsTheSizeWithoutBreakingACharacter(t *testing.T) {
	f := newFakeNtfy(t)
	NewClient().Send(context.Background(), Server{URL: f.URL}, Message{Topic: "t", Title: strings.Repeat("é", 400), Body: strings.Repeat("é", 4000)})
	title, _ := f.last().body["title"].(string)
	body, _ := f.last().body["message"].(string)
	if len(title) > maxTitleLen || len(body) > maxBodyLen {
		t.Errorf("title %d, body %d bytes", len(title), len(body))
	}
	if strings.ContainsRune(title+body, '�') {
		t.Error("a character was cut in half")
	}
}
