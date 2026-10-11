package issues

import (
	"context"
	"testing"
	"time"
)

type fakeSource struct {
	list  []Issue
	calls int
}

func (f *fakeSource) Issues(context.Context) []Issue { f.calls++; return f.list }

func TestIssuesAreMergedWorstFirstKeepingOrderWithinASeverity(t *testing.T) {
	a := &fakeSource{list: []Issue{{ID: "w1", Severity: Warning}, {ID: "i1", Severity: Info}, {ID: "e1", Severity: Error}}}
	b := &fakeSource{list: []Issue{{ID: "e2", Severity: Error}, {ID: "w2", Severity: Warning}}}
	got := Combine(time.Minute, a, b).Fresh(context.Background())
	want := []string{"e1", "e2", "w1", "w2", "i1"}
	for i, id := range want {
		if i >= len(got) || got[i].ID != id {
			t.Fatalf("order = %v, want %v", ids(got), want)
		}
	}
}

func ids(l []Issue) []string {
	out := []string{}
	for _, i := range l {
		out = append(out, i.ID)
	}
	return out
}

func TestTheChecksAreRememberedForTheTTLAndFreshBypassesIt(t *testing.T) {
	src := &fakeSource{list: []Issue{{ID: "x", Severity: Error}}}
	c := Combine(30*time.Second, src)
	clock := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return clock }
	ctx := context.Background()

	c.Issues(ctx)
	c.Issues(ctx)
	if src.calls != 1 {
		t.Errorf("two asks within the TTL ran the checks %d times", src.calls)
	}
	clock = clock.Add(31 * time.Second)
	c.Issues(ctx)
	if src.calls != 2 {
		t.Errorf("after the TTL the checks must run again, calls = %d", src.calls)
	}
	c.Fresh(ctx)
	if src.calls != 3 {
		t.Errorf("Fresh must always run them, calls = %d", src.calls)
	}
	src.list = nil
	if got := c.Issues(ctx); len(got) != 1 {
		t.Error("a remembered answer is served until it expires")
	}
	if got := c.Fresh(ctx); len(got) != 0 {
		t.Error("Fresh must see the change at once")
	}
}

func TestAnEmptyResultIsAnEmptyListAndIsRemembered(t *testing.T) {
	src := &fakeSource{}
	c := Combine(time.Minute, src)
	got := c.Issues(context.Background())
	if got == nil || len(got) != 0 {
		t.Errorf("got %v, want an empty non-nil list", got)
	}
	c.Issues(context.Background())
	if src.calls != 1 {
		t.Errorf("an answer of 'nothing wrong' is an answer: calls = %d", src.calls)
	}
}

func TestACallerCannotChangeWhatIsRemembered(t *testing.T) {
	c := Combine(time.Minute, &fakeSource{list: []Issue{{ID: "keep", Severity: Error}}})
	first := c.Issues(context.Background())
	first[0].ID = "tampered"
	if got := c.Issues(context.Background()); got[0].ID != "keep" {
		t.Errorf("the memory was changed through a returned slice: %v", got[0].ID)
	}
}

func TestCountIsWhatNeedsAttention(t *testing.T) {
	if n := Count([]Issue{{Severity: Error}, {Severity: Warning}, {Severity: Info}, {Severity: Info}}); n != 2 {
		t.Errorf("count = %d, want 2: notes are not things to do", n)
	}
	if Count(nil) != 0 {
		t.Error("nothing is nothing")
	}
}
