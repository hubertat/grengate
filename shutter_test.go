package main

import (
	"context"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brutella/hap/characteristic"
)

// fakeTimers is a timerClock whose timers fire only when a test fires them.
type fakeTimers struct {
	mu      sync.Mutex
	pending []*fakeTimer
}

type fakeTimer struct {
	d time.Duration
	f func()
}

func newFakeTimers() *fakeTimers { return &fakeTimers{} }

func (c *fakeTimers) AfterFunc(d time.Duration, f func()) func() bool {
	tm := &fakeTimer{d: d, f: f}
	c.mu.Lock()
	c.pending = append(c.pending, tm)
	c.mu.Unlock()
	return func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		for i, p := range c.pending {
			if p == tm {
				c.pending = append(c.pending[:i:i], c.pending[i+1:]...)
				return true
			}
		}
		return false
	}
}

// running reports whether a timer is pending: the shutter clock runs.
func (c *fakeTimers) running() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.pending) > 0
}

// fire fires the oldest pending timer and returns its duration; it fails the test if no
// timer is pending.
func (c *fakeTimers) fire(t testing.TB) time.Duration {
	t.Helper()
	c.mu.Lock()
	if len(c.pending) == 0 {
		c.mu.Unlock()
		t.Fatal("the shutter clock is not running")
	}
	tm := c.pending[0]
	c.pending = c.pending[1:]
	c.mu.Unlock()
	tm.f()
	return tm.d
}

// values returns the recorded values.
func (r *syncRecorder) values() []any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]any(nil), r.vals...)
}

func timersOf(gs *GrentonSet) *fakeTimers { return gs.shutterClock.(*fakeTimers) }

// idle reports whether the simulation goroutine has nothing left to handle.
func (s *shutterSim) idle() bool {
	s.imu.Lock()
	defer s.imu.Unlock()
	return len(s.inbox) == 0 && !s.handling
}

// checkGoroutines fails the test if goroutines started during it are still running once
// every cleanup (Stop, fake GATE close) has run. Call it first, so its cleanup runs last.
func checkGoroutines(t *testing.T) {
	t.Helper()
	before := runtime.NumGoroutine()
	t.Cleanup(func() {
		deadline := time.Now().Add(3 * time.Second)
		for runtime.NumGoroutine() > before {
			if time.Now().After(deadline) {
				buf := make([]byte, 1<<20)
				buf = buf[:runtime.Stack(buf, true)]
				t.Errorf("%d goroutines left behind:\n%s", runtime.NumGoroutine()-before, buf)
				return
			}
			time.Sleep(time.Millisecond)
		}
	})
}

// shutterClu is a CLU with one shutter (ROL0003, as in allKindsClu).
func shutterClu() *Clu {
	return &Clu{Id: tCLU, Shutters: []*Shutter{{CluObject: CluObject{Id: 3, Kind: "ROL", Name: "blind"}}}}
}

const tSID = "ROL0003"

// shown is what HomeKit shows for a shutter.
type shown struct {
	current, target, state int
}

func shownBy(sh *Shutter) shown {
	wc := sh.hk.WindowCovering
	return shown{wc.CurrentPosition.Value(), wc.TargetPosition.Value(), wc.PositionState.Value()}
}

const (
	hkStopped    = characteristic.PositionStateStopped
	hkIncreasing = characteristic.PositionStateIncreasing
	hkDecreasing = characteristic.PositionStateDecreasing
)

// Move refused while MaxTime is unknown (concept 3.5): a shutter that was never read, or
// whose read had no usable MaxTime, refuses a move with an error status at once; nothing is
// sent, the refusal is logged, a read of the shutter is queued and HomeKit keeps its target.
func TestShutter_MoveRefusedWhileNotReady(t *testing.T) {
	for _, tc := range []struct {
		name string
		prep func(gs *GrentonSet, g *fakeGate)
	}{
		{"never read", func(*GrentonSet, *fakeGate) {}},
		{"MaxTime 0", func(gs *GrentonSet, g *fakeGate) { g.SetShutter(tCLU, tSID, 0, 0); gs.Refresh() }},
		{"MaxTime 99", func(gs *GrentonSet, g *fakeGate) { g.SetShutter(tCLU, tSID, 99, 0); gs.Refresh() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checkGoroutines(t)
			logs := captureLog(t)
			clu := shutterClu()
			gs, g := newTestSet(t, clu)
			sh := clu.Shutters[0]
			tc.prep(gs, g)
			settle(t, gs)
			reads := g.RequestCount(fakeReadPath)
			targetSyncs := recordSyncs(sh.hk.WindowCovering.TargetPosition.C)

			var status int
			within(t, 50*time.Millisecond, "refused move", func() {
				status = hkWrite(sh.hk.WindowCovering.TargetPosition.C, 0)
			})
			if status != hkNoResponse {
				t.Errorf("status = %d, want %d", status, hkNoResponse)
			}
			settle(t, gs)
			if got := shownBy(sh); got != (shown{100, 100, hkStopped}) {
				t.Errorf("HomeKit shows %+v, want the untouched defaults", got)
			}
			if v := targetSyncs.values(); len(v) != 0 {
				t.Errorf("TargetPosition synced %v after a refused move", v)
			}
			if n := g.RequestCount(fakeSetPath); n != 0 {
				t.Errorf("set requests = %d, want 0", n)
			}
			rs := g.Requests(fakeReadPath)[reads:]
			if len(rs) != 1 || rs[0].Items[0]["Id"] != tSID {
				t.Errorf("reads after the refusal = %v, want one read of the shutter", rs)
			}
			if !strings.Contains(logs.String(), "move to 0 refused") {
				t.Errorf("refusal not logged:\n%s", logs.String())
			}
		})
	}
}

// The motor state a read observes reaches HomeKit through the simulation goroutine.
func TestShutter_ReadStateReachesHomeKitThroughSimulation(t *testing.T) {
	checkGoroutines(t)
	clu := shutterClu()
	gs, g := newTestSet(t, clu)
	sh := clu.Shutters[0]

	for _, tc := range []struct {
		state int
		want  int
	}{{1, hkIncreasing}, {2, hkDecreasing}, {0, hkStopped}} {
		g.SetShutter(tCLU, tSID, 20000, tc.state)
		gs.Refresh()
		settle(t, gs)
		if got := shownBy(sh); got.state != tc.want || got.current != 100 {
			t.Errorf("State %d: HomeKit shows %+v, want position state %d at 100", tc.state, got, tc.want)
		}
	}
}

// Startup returns only once the simulations took what the startup refresh observed, so the
// HomeKit server starts with it.
func TestShutter_StartupShowsObservedMotorState(t *testing.T) {
	checkGoroutines(t)
	clu := shutterClu()
	gs, g := newTestSet(t, clu)
	g.SetShutter(tCLU, tSID, 20000, 2)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	gs.startup(ctx, false)
	if got := shownBy(clu.Shutters[0]).state; got != hkDecreasing {
		t.Errorf("position state after startup = %d, want %d (decreasing)", got, hkDecreasing)
	}
}

// Posting to a simulation never waits for it: reads complete while the goroutine does not
// run at all.
func TestShutter_ReadWorkerDoesNotWaitForSimulation(t *testing.T) {
	checkGoroutines(t)
	clu := shutterClu()
	gs, g := newTestSet(t, clu)
	clu.Shutters[0].sim.stop()
	within(t, 2*time.Second, "refreshes with the simulation stopped", func() {
		for i := 0; i < 3; i++ {
			g.SetShutter(tCLU, tSID, 20000, i%3)
			if c := gs.Refresh(); c.Applied != 1 {
				t.Errorf("refresh %d = %+v, want the shutter applied", i, c)
			}
		}
	})
}

// The goroutine ends with GrentonSet.Stop; a barrier asked for afterwards is released at once.
func TestShutter_SimulationStopsWithSet(t *testing.T) {
	checkGoroutines(t)
	clu := shutterClu()
	gs, _ := newTestSet(t, clu)
	sim := clu.Shutters[0].sim
	gs.Stop()
	select {
	case <-sim.done:
	default:
		t.Fatal("simulation goroutine still running after Stop")
	}
	select {
	case <-sim.flushed():
	case <-time.After(time.Second):
		t.Fatal("barrier after Stop not released")
	}
}
