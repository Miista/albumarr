package notify

import (
	"strings"

	"github.com/Miista/albumreq/internal/coverart"
	"github.com/Miista/albumreq/internal/requests"
)

// Event is something that happened to a request.
type Event struct {
	Type    Type
	Request requests.Record
	// Actor is who did it: the approver who approved or declined. For Pending it is
	// the person who asked. They are never told about their own action.
	Actor requests.Person
}

// Audience is who a message is written for.
type Audience int

const (
	// Shared is the channel approvers subscribe to: it says who asked.
	Shared Audience = iota
	// Requester is the person whose request it is: it speaks to them.
	Requester
)

// Compose writes the notification for an event: a title, a body, and where a tap
// should lead. appURL, when set, is where albumreq is reached.
func Compose(e Event, who Audience, appURL string, poster bool) Message {
	r := e.Request
	m := Message{}
	artist := r.Artist
	switch e.Type {
	case Pending:
		m.Title = "New request: " + r.Title
		m.Body = artist + "\nAsked by " + r.Requester.Name + ". Waiting for approval."
		m.Click = link(appURL, "#/approvals")
	case AutoApproved:
		m.Title = "Approved automatically: " + r.Title
		m.Body = artist + "\nRequested by " + r.Requester.Name + ", who can approve their own requests."
		m.Click = albumLink(appURL, r)
	case Approved:
		m.Title = "Approved: " + r.Title
		m.Body = artist + "\n" + e.Actor.Name + " approved it. Lidarr is searching for it now."
		if who == Shared {
			m.Body = artist + "\n" + e.Actor.Name + " approved " + r.Requester.Name + "'s request. Lidarr is searching for it now."
		}
		m.Click = albumLink(appURL, r)
	case Declined:
		m.Title = "Declined: " + r.Title
		m.Body = artist + "\n" + e.Actor.Name + " declined it."
		if who == Shared {
			m.Body = artist + "\n" + e.Actor.Name + " declined " + r.Requester.Name + "'s request."
		}
		if r.DeclineReason != "" {
			m.Body += "\nReason: " + r.DeclineReason
		}
		m.Click = link(appURL, "#/requests")
	case Available:
		m.Title = "Available: " + r.Title
		m.Body = artist + "\nEvery track is in your library."
		if who == Shared {
			m.Body = artist + "\nEvery track is in the library. Requested by " + r.Requester.Name + "."
		}
		m.Click = albumLink(appURL, r)
	case Failed:
		// It stands for the approval as well, so it says that first.
		m.Title = "Approved, but the search did not start: " + r.Title
		m.Body = artist + "\nIt is in Lidarr, but Lidarr could not start the search, so it will not look for it by itself."
		m.Click = albumLink(appURL, r)
	}
	if poster && r.AlbumID != "" {
		m.Attach = coverart.DefaultBase + "/release-group/" + r.AlbumID + "/front-500"
	}
	return m
}

// albumLink opens the album's sheet. The ID is a UUID (checked when the request
// was made), so nothing in it can break out of the address.
func albumLink(appURL string, r requests.Record) string {
	if r.AlbumID == "" {
		return ""
	}
	return link(appURL, "#album="+r.AlbumID)
}

// link joins the application's address and a page, or is empty when the address
// is not set (a message without a link is better than one with a broken link).
func link(appURL, fragment string) string {
	appURL = strings.TrimRight(strings.TrimSpace(appURL), "/")
	if appURL == "" {
		return ""
	}
	return appURL + "/" + fragment
}
