package notify

import (
	"context"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/Miista/albumarr/internal/requests"
)

const queueSize = 128

type job struct {
	srv Server
	msg Message
	to  string // for the log and the page: "the shared channel" or "a requester"
}

// Dispatcher turns what happens to requests into messages and sends them, without
// ever making a request wait for ntfy: an event is queued and a worker sends it. A
// slow or dead server costs notifications, never approvals.
type Dispatcher struct {
	settings *Settings
	store    *Store
	sender   Sender
	log      zerolog.Logger
	queue    chan job
	now      func() time.Time
}

// NewDispatcher returns a Dispatcher. Run it to have it send.
func NewDispatcher(set *Settings, st *Store, sender Sender, log zerolog.Logger) *Dispatcher {
	return &Dispatcher{settings: set, store: st, sender: sender, log: log, queue: make(chan job, queueSize), now: time.Now}
}

// Run sends queued messages until ctx ends.
func (d *Dispatcher) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case j := <-d.queue:
			d.deliver(ctx, j)
		}
	}
}

// drain sends everything queued now, in the caller's goroutine. For tests.
func (d *Dispatcher) drain(ctx context.Context) {
	for {
		select {
		case j := <-d.queue:
			d.deliver(ctx, j)
		default:
			return
		}
	}
}

func (d *Dispatcher) deliver(ctx context.Context, j job) {
	cctx, cancel := context.WithTimeout(ctx, sendTimeout+2*time.Second)
	defer cancel()
	err := d.sender.Send(cctx, j.srv, j.msg)
	if err != nil {
		// The error names no address, topic or credential (see Client.Send).
		d.log.Warn().Msg("notification to " + j.to + " failed: " + err.Error())
		d.settings.Record(Delivery{OK: false, Message: err.Error(), At: d.now()})
		return
	}
	d.settings.Record(Delivery{OK: true, At: d.now()})
	d.log.Info().Str("to", j.to).Msg("notification sent")
}

// Observe is how the request flow reports what happened.
func (d *Dispatcher) Observe(e requests.Event) {
	t := map[requests.EventKind]Type{
		requests.EventPending:      Pending,
		requests.EventAutoApproved: AutoApproved,
		requests.EventApproved:     Approved,
		requests.EventDeclined:     Declined,
		requests.EventFailed:       Failed,
	}[e.Kind]
	if t == "" {
		return
	}
	d.Notify(Event{Type: t, Request: e.Record, Actor: e.Actor})
}

// Notify works out who should hear about an event and queues a message for each:
//   - the shared channel, if it has a topic and has chosen this event;
//   - the person whose request it is, if they have a topic and chose it, and they
//     did not do it themselves (nobody is told about their own action).
//
// Pending and automatic approvals are for the shared channel only: approvers
// subscribe to it. If both would land on the same topic, it is sent once.
func (d *Dispatcher) Notify(e Event) {
	cfg := d.settings.Resolved().Config
	if !cfg.Usable() {
		return
	}
	srv := cfg.Server()
	tags := splitTags(cfg.Tags)
	finish := func(m Message, topic string) Message {
		m.Topic, m.Priority, m.Tags = topic, cfg.Priority, tags
		return m
	}
	sent := map[string]bool{}

	if cfg.Topic != "" && Wants(cfg.Types, e.Type) {
		d.enqueue(job{srv, finish(Compose(e, Shared, cfg.AppURL, cfg.EmbedPoster), cfg.Topic), "the shared channel"})
		sent[strings.ToLower(cfg.Topic)] = true
	}

	r := e.Request.Requester
	if contains(ForRequesters, e.Type) && r.Sub != "" && r.Sub != e.Actor.Sub {
		p, found, err := d.store.Prefs(r.Sub)
		switch {
		case err != nil:
			d.log.Warn().Err(err).Msg("could not read a requester's notification settings")
		case found && Wants(p.Types, e.Type) && !sent[strings.ToLower(p.Topic)]:
			d.enqueue(job{srv, finish(Compose(e, Requester, cfg.AppURL, cfg.EmbedPoster), p.Topic), "a requester"})
		}
	}
}

func (d *Dispatcher) enqueue(j job) {
	select {
	case d.queue <- j:
	default:
		d.log.Warn().Msg("a notification was dropped: too many are waiting to be sent")
	}
}
