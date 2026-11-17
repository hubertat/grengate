package main

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestNextBoundary(t *testing.T) {
	origin := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	cycle := 10 * time.Second
	at := func(s float64) time.Time { return origin.Add(time.Duration(s * float64(time.Second))) }

	cases := []struct {
		name        string
		begun, now  float64
		wantNext    float64
		wantSkipped int
	}{
		{"startup refresh shorter than the cycle", 0, 2, 10, 0},
		{"startup refresh longer than the cycle", 0, 25, 30, 2},
		{"refresh shorter than the cycle", 10, 13, 20, 0},
		{"refresh just under the cycle", 10, 19.999, 20, 0},
		{"refresh equal to the cycle", 10, 20, 30, 1},
		{"refresh slightly longer than the cycle", 10, 21, 30, 1},
		{"refresh several cycles long", 10, 45, 50, 3},
		{"refresh ending exactly on a later boundary", 10, 40, 50, 3},
		{"clock before origin", -5, -3, 10, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			next, skipped := nextBoundary(origin, cycle, at(c.begun), at(c.now))
			if !next.Equal(at(c.wantNext)) || skipped != c.wantSkipped {
				t.Errorf("next = +%v, skipped = %d; want +%vs, %d", next.Sub(origin), skipped, c.wantNext, c.wantSkipped)
			}
		})
	}
}

// fakeClock is a manually advanced clock. Every After call is reported on afters.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []fakeWaiter
	afters  chan time.Duration
}

type fakeWaiter struct {
	at time.Time
	ch chan time.Time
}

func newFakeClock(start time.Time) *fakeClock {
	return &fakeClock{now: start, afters: make(chan time.Duration, 100)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	ch := make(chan time.Time, 1)
	at := c.now.Add(d)
	if d <= 0 {
		ch <- c.now
	} else {
		c.waiters = append(c.waiters, fakeWaiter{at, ch})
	}
	c.mu.Unlock()
	c.afters <- d
	return ch
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	rest := c.waiters[:0]
	for _, w := range c.waiters {
		if !w.at.After(c.now) {
			w.ch <- c.now
		} else {
			rest = append(rest, w)
		}
	}
	c.waiters = rest
}

// nextWait returns the duration of the scheduler's next After call.
func (c *fakeClock) nextWait(t *testing.T) time.Duration {
	t.Helper()
	select {
	case d := <-c.afters:
		return d
	case <-time.After(2 * time.Second):
		t.Fatal("scheduler did not start waiting")
		return 0
	}
}

// startScheduler runs a scheduler with a fake clock; each refresh advances the clock by the
// next value of durations (0 when exhausted) and is counted.
func startScheduler(t *testing.T, cycle time.Duration, durations ...time.Duration) (*fakeClock, *refreshScheduler, *atomic.Int32, *syncBuffer) {
	t.Helper()
	logs := captureLog(t)
	clk := newFakeClock(time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC))
	var refreshes atomic.Int32
	gs := &GrentonSet{}
	s := &refreshScheduler{
		origin: clk.Now(),
		cycle:  cycle,
		clock:  clk,
		logf:   gs.Logf,
	}
	s.refresh = func() {
		n := int(refreshes.Add(1))
		if n <= len(durations) {
			clk.Advance(durations[n-1])
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); s.run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	return clk, s, &refreshes, logs
}

func TestScheduler_ShortRefreshWaitsForNextBoundary(t *testing.T) {
	cycle := 10 * time.Second
	clk, s, refreshes, _ := startScheduler(t, cycle, 3*time.Second)

	if d := clk.nextWait(t); d != cycle {
		t.Fatalf("first wait %v, want %v", d, cycle)
	}
	clk.Advance(cycle) // boundary 1: refresh of 3 s
	if d := clk.nextWait(t); d != 7*time.Second {
		t.Errorf("wait after a 3 s refresh = %v, want 7s (boundaries are fixed from the start)", d)
	}
	if n := refreshes.Load(); n != 1 {
		t.Errorf("refreshes = %d, want 1", n)
	}
	if s.skipped.Load() != 0 {
		t.Errorf("skipped = %d, want 0", s.skipped.Load())
	}
}

func TestScheduler_OverrunSkipsBoundaries(t *testing.T) {
	cycle := 10 * time.Second
	// first refresh overruns one boundary, the second one three
	clk, s, refreshes, logs := startScheduler(t, cycle, 15*time.Second, 32*time.Second)

	clk.nextWait(t)
	clk.Advance(cycle) // boundary at +10 s: refresh runs until +25 s, boundary +20 s passes
	if d := clk.nextWait(t); d != 5*time.Second {
		t.Fatalf("wait after the overrun = %v, want 5s until the +30 s boundary", d)
	}
	if n := refreshes.Load(); n != 1 {
		t.Fatalf("refreshes = %d right after the overrun, want 1 (no immediate second refresh)", n)
	}
	if got := s.skipped.Load(); got != 1 {
		t.Errorf("skipped = %d, want 1", got)
	}

	clk.Advance(5 * time.Second) // boundary +30 s: refresh until +62 s; +40, +50, +60 pass
	if d := clk.nextWait(t); d != 8*time.Second {
		t.Errorf("wait = %v, want 8s until +70 s", d)
	}
	if n := refreshes.Load(); n != 2 {
		t.Errorf("refreshes = %d, want 2", n)
	}
	if got := s.skipped.Load(); got != 4 {
		t.Errorf("skipped total = %d, want 4", got)
	}
	if !containsAll(logs.String(), "3 refresh(es) skipped", "(4 so far)") {
		t.Errorf("skipped boundaries not logged:\n%s", logs.String())
	}
}

func TestScheduler_StopsWithContext(t *testing.T) {
	clk := newFakeClock(time.Now())
	s := &refreshScheduler{origin: clk.Now(), cycle: time.Second, clock: clk, refresh: func() {}, logf: func(string, ...any) {}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); s.run(ctx) }()
	clk.nextWait(t)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("scheduler did not stop")
	}
}

func TestStartup_SchedulesRefreshesFromStartupOrigin(t *testing.T) {
	clk := newFakeClock(time.Now())
	gs, g := newTestSetWith(t, func(gs *GrentonSet) { gs.clock = clk }, lightsClu(2))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	gs.startup(ctx, false)
	if n := g.RequestCount(fakeReadPath); n != 1 {
		t.Fatalf("read requests after startup = %d, want 1", n)
	}
	if d := clk.nextWait(t); d != 10*time.Second {
		t.Errorf("first wait = %v, want the 10 s cycle", d)
	}
	clk.Advance(10 * time.Second)
	g.WaitForRequests(t, fakeReadPath, 2)
	clk.nextWait(t) // the cycle refresh finished and the scheduler waits again
	if n := g.RequestCount(fakeReadPath); n != 2 {
		t.Errorf("read requests = %d, want 2", n)
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
