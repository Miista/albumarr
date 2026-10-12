package notify

import (
	"context"
	"fmt"
	"time"

	"github.com/Miista/albumreq/internal/issues"
)

// deliveryStale is how long a failure counts as current. After that, with nothing
// newer, it is probably over, and a warning nobody can clear would only teach people
// to ignore the page.
const deliveryStale = time.Hour

// Issues reports what is wrong with notifications. It asks nobody: it reads the
// settings and the outcome of the last send, so it is cheap and never slow.
func (s *Settings) Issues(context.Context) []issues.Issue {
	r := s.Resolved()
	var out []issues.Issue
	for _, p := range r.Problems {
		out = append(out, issues.Issue{
			ID: "notifications-env-invalid", Severity: issues.Error, Title: "A notification setting from the environment cannot be used",
			Detail: p + ". It is ignored until it is corrected.", Fix: "Correct the variable and restart albumreq.",
		})
	}
	c := r.Config
	if !c.Enabled {
		return out
	}
	const where = "#/settings/notifications"
	if c.URL == "" {
		out = append(out, issues.Issue{
			ID: "notifications-no-server", Severity: issues.Error, Title: "Notifications are on, but there is no ntfy server",
			Detail: "Nothing can be sent without the server's address.", Fix: "Enter it under Settings, Notifications.", Link: where,
		})
		return out
	}
	switch {
	case c.Auth == AuthToken && c.Token == "":
		out = append(out, issues.Issue{
			ID: "notifications-no-credentials", Severity: issues.Warning, Title: "Notifications use a token, but none is saved",
			Detail: "The server will refuse messages if it needs one.", Fix: "Enter the token under Settings, Notifications, or switch authentication off.", Link: where,
		})
	case c.Auth == AuthBasic && (c.Username == "" || c.Password == ""):
		out = append(out, issues.Issue{
			ID: "notifications-no-credentials", Severity: issues.Warning, Title: "Notifications use a username and password, but they are incomplete",
			Detail: "The server will refuse messages if it needs them.", Fix: "Enter both under Settings, Notifications, or switch authentication off.", Link: where,
		})
	}
	if d := s.LastDelivery(); d != nil && !d.OK && time.Since(d.At) < deliveryStale {
		out = append(out, issues.Issue{
			ID: "notifications-failing", Severity: issues.Warning, Title: "Notifications are not being delivered",
			Detail: fmt.Sprintf("The last attempt failed: %s (%s).", d.Message, d.At.Local().Format("15:04")),
			Fix:    "Check that the ntfy server is running and the address, topic and credentials are right, then send a test.", Link: where,
		})
	}
	if c.AppURL == "" {
		out = append(out, issues.Issue{
			ID: "notifications-no-app-url", Severity: issues.Info, Title: "Notifications have no links",
			Detail: "Tapping a notification cannot open albumreq, because it does not know its own address.",
			Fix:    "Enter the application address under Settings, Notifications.", Link: where,
		})
	}
	return out
}
