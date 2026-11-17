package main

import (
	"context"
	"net/http/httptest"
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

func newTestShutter(maxTime int) *Shutter {
	return &Shutter{
		CluObject: CluObject{Id: 7, Kind: "ROL", Name: "blind"},
		MaxTime:   maxTime,
	}
}

func TestShutter_MoveRefusedWithoutMaxTime(t *testing.T) {
	logs := captureLog(t)
	sh := newTestShutter(0)
	clu := &Clu{Id: "CLU_012abcde", Shutters: []*Shutter{sh}}
	_, gate := newTestSet(t, clu)

	target := sh.hk.WindowCovering.TargetPosition
	done := make(chan struct{})
	go func() {
		defer close(done)
		target.SetValueRequest(0, httptest.NewRequest("PUT", "/characteristics", nil))
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("move with MaxTime 0 did not return at once")
	}

	if got := target.Value(); got != 100 {
		t.Errorf("HomeKit target = %v, want 100 (current position)", got)
	}
	if n := gate.RequestCount(""); n != 0 {
		t.Errorf("fake GATE received %d requests, want 0", n)
	}
	if !strings.Contains(logs.String(), "MaxTime") {
		t.Errorf("refusal not logged: %q", logs.String())
	}
	if sh.looping {
		t.Error("move loop started")
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
