// Package issues is what an admin needs to know is wrong: a Lidarr that cannot be
// reached, a profile it lacks, a setting the environment got wrong. Each check
// says what is wrong, why it matters and what to do, so the Issues page can show
// a to-do list instead of a log to read.
package issues

import (
	"context"
	"sort"
	"sync"
	"time"
)

// Severity of an issue.
type Severity string

const (
	// Error: something albumreq needs does not work. A feature is off or will fail.
	Error Severity = "error"
	// Warning: it works, but something is not as it should be.
	Warning Severity = "warning"
	// Info: nothing is broken; this is how it is.
	Info Severity = "info"
)

// Issue is one thing that needs attention.
type Issue struct {
	// ID is stable per kind of issue, so a page can key on it and a test can look for it.
	ID       string   `json:"id"`
	Severity Severity `json:"severity"`
	Title    string   `json:"title"`
	// Detail says what is wrong and what it means, in plain words.
	Detail string `json:"detail"`
	// Fix says what to do about it.
	Fix string `json:"fix,omitempty"`
	// Link is an address in the app where it can be dealt with, e.g. "#/settings".
	Link string `json:"link,omitempty"`
}

// Source produces issues.
type Source interface {
	Issues(ctx context.Context) []Issue
}

// Combine merges several sources, worst first, and remembers the answer for a
// short while: the checks talk to Lidarr, and the nav asks on every page load.
// Fresh bypasses the memory (the "Check again" button).
type Combined struct {
	sources []Source
	ttl     time.Duration
	now     func() time.Time

	mu    sync.Mutex
	at    time.Time
	cache []Issue
}

// Combine returns a Combined over the sources, remembering answers for ttl.
func Combine(ttl time.Duration, sources ...Source) *Combined {
	return &Combined{sources: sources, ttl: ttl, now: time.Now}
}

// Issues returns all issues, worst first, from memory if it is recent.
func (c *Combined) Issues(ctx context.Context) []Issue { return c.get(ctx, false) }

// Fresh runs the checks again now.
func (c *Combined) Fresh(ctx context.Context) []Issue { return c.get(ctx, true) }

func (c *Combined) get(ctx context.Context, fresh bool) []Issue {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !fresh && c.cache != nil && c.now().Sub(c.at) < c.ttl {
		return append([]Issue{}, c.cache...)
	}
	out := []Issue{}
	for _, s := range c.sources {
		out = append(out, s.Issues(ctx)...)
	}
	rank := map[Severity]int{Error: 0, Warning: 1, Info: 2}
	sort.SliceStable(out, func(i, j int) bool { return rank[out[i].Severity] < rank[out[j].Severity] })
	c.cache, c.at = out, c.now()
	return append([]Issue{}, out...)
}

// Count is how many issues need attention: errors and warnings, not notes.
func Count(list []Issue) int {
	n := 0
	for _, i := range list {
		if i.Severity != Info {
			n++
		}
	}
	return n
}
