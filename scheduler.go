package main

import (
	"context"
	"sync/atomic"
	"time"
)

// clock is the time source of the refresh scheduler, injected so tests control time.
type clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// nextBoundary returns the first refresh boundary strictly after now, and how many
// boundaries were skipped since the refresh that began at begun: those after begun and not
// after now, i.e. reached while that refresh was still running.
//
// Boundaries are fixed from the start: origin + k*cycle for k >= 1 (origin itself is the
// startup refresh). A refresh that takes exactly one cycle therefore skips the boundary it
// ends on, and the next one starts a full cycle later instead of immediately.
func nextBoundary(origin time.Time, cycle time.Duration, begun, now time.Time) (next time.Time, skipped int) {
	index := func(t time.Time) int64 {
		d := t.Sub(origin)
		if d < 0 {
			return -1
		}
		return int64(d / cycle)
	}
	kNow := index(now)
	k := max(kNow+1, 1)
	if kBegun := index(begun); kNow > kBegun {
		skipped = int(kNow - kBegun)
	}
	return origin.Add(time.Duration(k) * cycle), skipped
}

// refreshScheduler runs refresh at every cycle boundary, one refresh at a time. Boundaries
// that pass while a refresh runs are skipped (counted and logged): after a refresh it waits
// for the first boundary still in the future. A time.Ticker is not used because it keeps one
// tick that fired during a slow refresh and delivers it right after.
type refreshScheduler struct {
	origin  time.Time
	cycle   time.Duration
	clock   clock
	refresh func()
	logf    func(string, ...any)

	skipped atomic.Int64 // boundaries skipped so far
}

func (s *refreshScheduler) run(ctx context.Context) {
	begun := s.origin
	for {
		now := s.clock.Now()
		next, skipped := nextBoundary(s.origin, s.cycle, begun, now)
		if skipped > 0 {
			total := s.skipped.Add(int64(skipped))
			s.logf("refresh took %v, longer than the %v cycle: %d refresh(es) skipped (%d so far), next in %v",
				now.Sub(begun).Round(time.Millisecond), s.cycle, skipped, total, next.Sub(now).Round(time.Millisecond))
		}
		select {
		case <-s.clock.After(next.Sub(now)):
		case <-ctx.Done():
			return
		}
		begun = next
		s.refresh()
	}
}
