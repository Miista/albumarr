package notify

import (
	"context"
	"time"

	"github.com/rs/zerolog"

	"github.com/Miista/albumreq/internal/requests"
)

// Records is what the availability check needs of the request records.
type Records interface {
	AwaitingAvailability(decidedSince int64, limit int) ([]requests.Record, error)
	MarkAvailableNotified(id int64) error
}

// Availability says whether Lidarr has every track of an album.
type Availability interface {
	Available(ctx context.Context, albumID string) (bool, error)
}

const (
	checkEvery  = 5 * time.Minute
	firstCheck  = 30 * time.Second
	giveUpAfter = 90 * 24 * time.Hour // an album still not here after this is not coming
	perPass     = 50
)

// Poller tells requesters when an approved album has arrived. Nothing tells
// albumreq when a download finishes, so it asks Lidarr now and then about the
// requests that are approved and not yet announced.
type Poller struct {
	records  Records
	lidarr   Availability
	settings *Settings
	d        *Dispatcher
	log      zerolog.Logger
	now      func() time.Time
}

// NewPoller returns a Poller.
func NewPoller(records Records, lidarr Availability, set *Settings, d *Dispatcher, log zerolog.Logger) *Poller {
	return &Poller{records: records, lidarr: lidarr, settings: set, d: d, log: log, now: time.Now}
}

// Run checks every few minutes until ctx ends.
func (p *Poller) Run(ctx context.Context) {
	wait := firstCheck
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if _, err := p.Check(ctx); err != nil && ctx.Err() == nil {
			p.log.Warn().Err(err).Msg("could not check which requested albums have arrived")
		}
		wait = checkEvery
	}
}

// Check makes one pass and returns how many requesters were told. It does nothing
// while notifications are off: those requests stay unannounced, and are announced
// if notifications are switched on (but only those decided since they first were,
// so there is no flood of old news).
func (p *Poller) Check(ctx context.Context) (int, error) {
	cfg := p.settings.Resolved().Config
	if !cfg.Usable() {
		return 0, nil
	}
	since := cfg.EnabledSince
	if floor := p.now().Add(-giveUpAfter).Unix(); floor > since {
		since = floor
	}
	recs, err := p.records.AwaitingAvailability(since, perPass)
	if err != nil {
		return 0, err
	}
	told := 0
	for _, rec := range recs {
		ok, err := p.lidarr.Available(ctx, rec.AlbumID)
		if err != nil {
			// Lidarr is not answering: stop here instead of asking 50 more times.
			return told, err
		}
		if !ok {
			continue
		}
		// Mark first: a crash between the two loses one notification, where the other
		// order could send the same one again every pass.
		if err := p.records.MarkAvailableNotified(rec.ID); err != nil {
			return told, err
		}
		p.d.Notify(Event{Type: Available, Request: rec})
		told++
	}
	return told, nil
}
