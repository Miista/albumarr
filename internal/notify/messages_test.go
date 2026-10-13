package notify

import (
	"strings"
	"testing"

	"github.com/Miista/albumarr/internal/requests"
)

func rec() requests.Record {
	return requests.Record{
		ID: 7, AlbumID: "b1392450-e666-3926-a536-22c65f834433", Title: "OK Computer", Artist: "Radiohead",
		Requester: requests.Person{Sub: "s-alice", Name: "alice"},
	}
}

func TestEveryEventReadsRightForEachAudience(t *testing.T) {
	anna := requests.Person{Sub: "s-anna", Name: "anna"}
	for _, tc := range []struct {
		name  string
		e     Event
		who   Audience
		title string
		body  []string
	}{
		{"pending", Event{Type: Pending, Request: rec(), Actor: rec().Requester}, Shared, "New request: OK Computer", []string{"Radiohead", "Asked by alice", "Waiting for approval"}},
		{"auto-approved", Event{Type: AutoApproved, Request: rec(), Actor: rec().Requester}, Shared, "Approved automatically: OK Computer", []string{"alice", "approve their own"}},
		{"approved to the requester", Event{Type: Approved, Request: rec(), Actor: anna}, Requester, "Approved: OK Computer", []string{"anna approved it"}},
		{"approved on the shared channel", Event{Type: Approved, Request: rec(), Actor: anna}, Shared, "Approved: OK Computer", []string{"anna approved alice's request"}},
		{"declined to the requester", Event{Type: Declined, Request: withReason("Already own it"), Actor: anna}, Requester, "Declined: OK Computer", []string{"anna declined it", "Reason: Already own it"}},
		{"declined on the shared channel", Event{Type: Declined, Request: withReason(""), Actor: anna}, Shared, "Declined: OK Computer", []string{"anna declined alice's request"}},
		{"available to the requester", Event{Type: Available, Request: rec()}, Requester, "Available: OK Computer", []string{"Every track is in your library"}},
		{"available on the shared channel", Event{Type: Available, Request: rec()}, Shared, "Available: OK Computer", []string{"Requested by alice"}},
		{"failed", Event{Type: Failed, Request: rec()}, Requester, "Approved, but it could not be started: OK Computer", []string{"starting it failed"}},
	} {
		m := Compose(tc.e, tc.who, "http://albumarr.example:8768", false)
		if m.Title != tc.title {
			t.Errorf("%s: title %q, want %q", tc.name, m.Title, tc.title)
		}
		for _, want := range tc.body {
			if !strings.Contains(m.Body, want) {
				t.Errorf("%s: body %q lacks %q", tc.name, m.Body, want)
			}
		}
	}
	if m := Compose(Event{Type: Declined, Request: withReason(""), Actor: anna}, Requester, "", false); strings.Contains(m.Body, "Reason") {
		t.Errorf("no reason was given, so none is shown: %q", m.Body)
	}
}

func withReason(s string) requests.Record { r := rec(); r.DeclineReason = s; return r }

func TestLinksPointWhereTheEventIsAndAreLeftOutWithoutAnAppAddress(t *testing.T) {
	const app = "http://albumarr.example:8768/"
	for typ, want := range map[Type]string{
		Pending:   "http://albumarr.example:8768/#/approvals",
		Approved:  "http://albumarr.example:8768/#album=b1392450-e666-3926-a536-22c65f834433",
		Available: "http://albumarr.example:8768/#album=b1392450-e666-3926-a536-22c65f834433",
		Declined:  "http://albumarr.example:8768/#/requests",
	} {
		if got := Compose(Event{Type: typ, Request: rec()}, Shared, app, false).Click; got != want {
			t.Errorf("%s: click %q, want %q", typ, got, want)
		}
	}
	for _, typ := range All {
		if got := Compose(Event{Type: typ, Request: rec()}, Shared, "  ", false).Click; got != "" {
			t.Errorf("%s: with no application address there is no link, got %q", typ, got)
		}
	}
}

func TestThePosterIsOptionalAndPublic(t *testing.T) {
	m := Compose(Event{Type: Approved, Request: rec()}, Requester, "", true)
	if !strings.HasPrefix(m.Attach, "https://") || !strings.HasSuffix(m.Attach, "/release-group/b1392450-e666-3926-a536-22c65f834433/front-500") {
		t.Errorf("attach = %q", m.Attach)
	}
	if Compose(Event{Type: Approved, Request: rec()}, Requester, "", false).Attach != "" {
		t.Error("no poster unless asked for")
	}
}

func TestPeoplesTextIsPassedAsPlainText(t *testing.T) {
	r := rec()
	r.Title, r.Artist, r.Requester.Name = "<script>alert(1)</script> [x](http://evil)", "**bold** `code`", "al\nice"
	m := Compose(Event{Type: Pending, Request: r, Actor: r.Requester}, Shared, "", false)
	if !strings.Contains(m.Title, "<script>") || !strings.Contains(m.Body, "**bold**") {
		t.Errorf("text must arrive as typed (ntfy shows it as plain text): %q / %q", m.Title, m.Body)
	}
}
