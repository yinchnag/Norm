package orm

import (
	"context"
	"sync"
)

// writeLifecycle serializes admission with closing; admitted operations drain
// before workers stop. A context lease allows nested Redis calls to finish.
type writeLifecycle struct {
	mu      sync.Mutex
	closing bool
	active  sync.WaitGroup
}
type writeLease struct{ pool *Pool }
type writeLeaseKey struct{}

func (p *Pool) beginWrite(ctx context.Context) (context.Context, func(), error) {
	if lease, ok := ctx.Value(writeLeaseKey{}).(*writeLease); ok && lease.pool == p {
		return ctx, func() {}, nil
	}
	p.writes.mu.Lock()
	if p.writes.closing {
		p.writes.mu.Unlock()
		return ctx, nil, newError("Write", "", nil, ErrStoreStopped, nil)
	}
	p.writes.active.Add(1)
	p.writes.mu.Unlock()
	return context.WithValue(ctx, writeLeaseKey{}, &writeLease{pool: p}), p.writes.active.Done, nil
}

func (p *Pool) stopWrites() {
	p.writes.mu.Lock()
	p.writes.closing = true
	p.writes.mu.Unlock()
	p.writes.active.Wait()
}
