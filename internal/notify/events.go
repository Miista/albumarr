// Package notify tells people what happened to requests, through an ntfy server:
// approvers hear that something is waiting, requesters hear what became of theirs.
// It is modelled on Seerr's notifications: an agent with an event checklist for the
// shared channel, plus each person's own destination for their own requests.
package notify

import (
	"fmt"
	"strings"
)

// Type is a kind of event.
type Type string

const (
	// Pending: a request is waiting for an approver. For the shared channel.
	Pending Type = "pending"
	// AutoApproved: an approver's or admin's own request went straight through. For the shared channel.
	AutoApproved Type = "auto_approved"
	// Approved: a request was approved and handed to Lidarr.
	Approved Type = "approved"
	// Declined: a request was declined, with the approver's reason.
	Declined Type = "declined"
	// Available: Lidarr has every track of an approved album.
	Available Type = "available"
	// Failed: the album was approved and added to Lidarr but its search could not be
	// started, so Lidarr will not look for it by itself. It stands for the approval,
	// so someone who asked to hear about approvals hears about this too.
	Failed Type = "failed"
)

// All is every event the shared channel can be sent, in the order a page lists them.
var All = []Type{Pending, AutoApproved, Approved, Declined, Available, Failed}

// ForRequesters is what a person can ask to hear about their own requests.
var ForRequesters = []Type{Approved, Declined, Available, Failed}

// Label is how a page names an event.
func (t Type) Label() string {
	switch t {
	case Pending:
		return "A request is waiting for approval"
	case AutoApproved:
		return "A request was approved automatically"
	case Approved:
		return "A request was approved"
	case Declined:
		return "A request was declined"
	case Available:
		return "An approved album is available"
	case Failed:
		return "An approved album could not be searched for"
	}
	return string(t)
}

func contains(list []Type, t Type) bool {
	for _, x := range list {
		if x == t {
			return true
		}
	}
	return false
}

// Has reports whether the list includes t.
func Has(list []Type, t Type) bool { return contains(list, t) }

// ParseTypes reads a list of event names, keeping only those in allowed, once each,
// in the order of allowed (so a stored list is always canonical). An unknown name is
// an error, not skipped: a typo should not quietly unsubscribe someone.
func ParseTypes(names []string, allowed []Type) ([]Type, error) {
	want := map[Type]bool{}
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		t := Type(n)
		if !contains(allowed, t) {
			return nil, fmt.Errorf("%q is not an event you can choose", n)
		}
		want[t] = true
	}
	var out []Type
	for _, t := range allowed {
		if want[t] {
			out = append(out, t)
		}
	}
	return out, nil
}

// encodeTypes is the stored form: a comma-separated list in the canonical order.
func encodeTypes(list []Type) string {
	names := make([]string, len(list))
	for i, t := range list {
		names[i] = string(t)
	}
	return strings.Join(names, ",")
}

// decodeTypes reads a stored list, ignoring anything this version does not know,
// so a database written by a newer one still loads.
func decodeTypes(s string) []Type {
	var out []Type
	for _, n := range strings.Split(s, ",") {
		if t := Type(strings.TrimSpace(n)); contains(All, t) && !contains(out, t) {
			out = append(out, t)
		}
	}
	return out
}

// Wants reports whether a list of chosen events includes t. A failed search stands
// for the approval that led to it, so it also reaches someone who chose approvals.
func Wants(chosen []Type, t Type) bool {
	return contains(chosen, t) || (t == Failed && contains(chosen, Approved))
}
