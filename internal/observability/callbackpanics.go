// Package observability holds the small cross-cutting pieces that several
// packages need to report on themselves.
package observability

import (
	"sync"
	"sync/atomic"
)

// CallbackPanics counts panics escaping user-supplied observability callbacks,
// keyed by callback name.
//
// Observers are best-effort: a panic in one must not take down the scheduler
// loop or a command in flight. Counting rather than swallowing keeps the
// failure visible — a callback that panics every time shows up as a rising
// count instead of silently never running.
//
// The zero value is ready to use. Scheduler, TCPClient and the app's TCP
// reconnect log dispatcher each embed one; all three previously carried their
// own copy of these three fields and four near-identical methods.
type CallbackPanics struct {
	total atomic.Uint64

	mu     sync.Mutex
	counts map[string]uint64
}

// RecoverFrom records a panic in the named callback, if one is unwinding.
// Call it directly from a defer so recover() sees the callback's panic:
//
//	defer c.panics.RecoverFrom("on_command_done")
func (p *CallbackPanics) RecoverFrom(name string) {
	if recovered := recover(); recovered != nil {
		p.Record(name)
	}
}

// Run invokes fn, recording a panic against name rather than letting it escape.
// A nil fn is a no-op, so callers need not check first.
func (p *CallbackPanics) Run(name string, fn func()) {
	if fn == nil {
		return
	}
	defer p.RecoverFrom(name)
	fn()
}

func (p *CallbackPanics) Record(name string) {
	p.total.Add(1)

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.counts == nil {
		p.counts = make(map[string]uint64)
	}
	p.counts[name]++
}

// Total is the number of callback panics across all names.
func (p *CallbackPanics) Total() uint64 {
	return p.total.Load()
}

// Counts returns a copy of the per-callback tallies, or nil when none have
// panicked. The copy matters: the caller must not be able to mutate the
// counters through the returned map.
func (p *CallbackPanics) Counts() map[string]uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.counts) == 0 {
		return nil
	}
	counts := make(map[string]uint64, len(p.counts))
	for name, count := range p.counts {
		counts[name] = count
	}
	return counts
}
