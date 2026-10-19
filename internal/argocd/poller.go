package argocd

import (
	"context"
	"fmt"
	"time"

	"github.com/atlas-platform-tcc/atlas-api/internal/domain"
)

// Recorder appends a derived event, injected so the poller does not depend on
// persistence directly.
type Recorder func(service, evType, outcome, detail string)

// Poller records an event whenever a service's (sync, health) changes, turning the
// reconciler's state into an audit stream (ADR atlas-api/0017).
type Poller struct {
	statuses func(context.Context) (map[string]Status, error)
	record   Recorder
	interval time.Duration
	seen     map[string]Status
	primed   bool // after the first tick we have a baseline; only then do we emit
}

// NewPoller builds a poller over a status source (e.g. Client.AppStatuses).
func NewPoller(statuses func(context.Context) (map[string]Status, error), record Recorder, interval time.Duration) *Poller {
	return &Poller{statuses: statuses, record: record, interval: interval, seen: map[string]Status{}}
}

// Run polls until the context is cancelled. The first tick only establishes the
// baseline; later ticks emit an event per changed or newly appeared service.
func (p *Poller) Run(ctx context.Context) {
	t := time.NewTicker(p.interval)
	defer t.Stop()
	for {
		p.tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (p *Poller) tick(ctx context.Context) {
	statuses, err := p.statuses(ctx)
	if err != nil {
		return // transient (e.g. Argo CD momentarily unreachable); try again next tick
	}
	for name, st := range statuses {
		prev, existed := p.seen[name]
		p.seen[name] = st
		if !p.primed || (existed && prev == st) {
			continue
		}
		evType, outcome := classify(st)
		p.record(name, evType, outcome, fmt.Sprintf("sync=%s health=%s", st.Sync, st.Health))
	}
	p.primed = true
}

func classify(st Status) (string, string) {
	switch st.Health {
	case "Degraded", "Missing":
		return domain.EventArgoDegraded, "error"
	case "Healthy":
		if st.Sync == "Synced" {
			return domain.EventArgoHealthy, "ok"
		}
	}
	return domain.EventArgoProgressing, "ok"
}
