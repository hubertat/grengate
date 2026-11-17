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

// idle reports whether the simulation has nothing left to handle: its inbox is handled and
// so is the result of every command it submitted.
func (s *shutterSim) idle() bool {
	s.imu.Lock()
	defer s.imu.Unlock()
	return len(s.inbox) == 0 && !s.handling && s.outstanding == 0
}

// inboxHandled reports whether everything posted so far is handled (results of commands
// may still be outstanding).
func (s *shutterSim) inboxHandled() bool {
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

// readyShutter returns a set with one shutter that has been read (stopped, MaxTime 20000,
// so one clock step is 200 ms) and a light (DOU0001) to hold the write lane with.
func readyShutter(t *testing.T) (*GrentonSet, *fakeGate, *Shutter, *fakeTimers) {
	t.Helper()
	clu := shutterClu()
	clu.Lights = []*Light{{CluObject: CluObject{Id: 1, Kind: "DOU", Name: "lamp"}}}
	gs, g := newTestSet(t, clu)
	if c := gs.Refresh(); c.Applied != 2 {
		t.Fatalf("initial refresh = %+v", c)
	}
	settle(t, gs)
	return gs, g, clu.Shutters[0], timersOf(gs)
}

// hkTarget is a HomeKit write of the shutter's target position; it must be accepted.
func hkTarget(t *testing.T, sh *Shutter, v int) {
	t.Helper()
	syncHomeKit(t, sh.clu.set) // hap characteristics are not synchronised: no HomeKit write during a sync
	if status := hkWrite(sh.hk.WindowCovering.TargetPosition.C, v); status != 0 {
		t.Fatalf("target %d: status %d", v, status)
	}
}

// tick advances the shutter clock by n steps; each step is handled before the next fires.
// It does not wait for commands a step submits (settle does).
func tick(t *testing.T, gs *GrentonSet, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		timersOf(gs).fire(t)
		for _, sh := range gs.shutters() {
			waitFor(t, sh.sim.inboxHandled, "the clock step to be handled")
		}
	}
	syncHomeKit(t, gs)
}

// waitFor polls cond (often: the shutter tests wait for it after every clock step) until it
// is true, or fails the test after 2 s.
func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Microsecond)
	}
}

// shutterCmds returns the shutter commands the fake GATE received, in order.
func shutterCmds(g *fakeGate) []string {
	var out []string
	for _, r := range g.Requests(fakeSetPath) {
		for _, it := range r.Items {
			if it["Kind"] == "Shutter" {
				out = append(out, it["Cmd"].(string))
			}
		}
	}
	return out
}

func assertCmds(t *testing.T, g *fakeGate, want ...string) {
	t.Helper()
	if got := shutterCmds(g); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("shutter commands at the GATE = %v, want %v", got, want)
	}
}

func assertShown(t *testing.T, sh *Shutter, want shown) {
	t.Helper()
	if got := shownBy(sh); got != want {
		t.Errorf("HomeKit shows %+v, want %+v (current, target, position state)", got, want)
	}
}

// A new target returns to HomeKit without waiting for the GATE.
func TestShutter_NewTargetReturnsWithoutWaiting(t *testing.T) {
	checkGoroutines(t)
	gs, g, sh, _ := readyShutter(t)
	g.HangAll(fakeSetPath)
	within(t, 50*time.Millisecond, "HomeKit target while the GATE hangs", func() { hkTarget(t, sh, 40) })
	g.WaitForRequests(t, fakeSetPath, 1)
	g.ReleaseAll()
	settle(t, gs)
	assertCmds(t, g, "MOVEDOWN")
}

// Clock starts on acknowledgement, not on submit: the position does not change and the
// motor is not shown moving before the move is acknowledged.
func TestShutter_ClockStartsOnAcknowledgement(t *testing.T) {
	checkGoroutines(t)
	gs, g, sh, clk := readyShutter(t)
	h := g.HoldNext(fakeSetPath)
	hkTarget(t, sh, 40)
	h.Wait(t)
	waitFor(t, sh.sim.inboxHandled, "the target to be handled")
	syncHomeKit(t, gs)
	if clk.running() {
		t.Error("clock started before the acknowledgement")
	}
	assertShown(t, sh, shown{100, 40, hkStopped})

	h.Release()
	settle(t, gs)
	assertShown(t, sh, shown{100, 40, hkDecreasing})
	if d := clk.fire(t); d != 200*time.Millisecond {
		t.Errorf("clock step = %v, want 200 ms (MaxTime 20000 / 100)", d)
	}
	settle(t, gs)
	tick(t, gs, 9)
	assertShown(t, sh, shown{90, 40, hkDecreasing})
	assertCmds(t, g, "MOVEDOWN")
}

// Retarget before the move was sent: the newer command replaces it in the mailbox, one
// command reaches the fake GATE, and the newer target is the one the clock runs to.
func TestShutter_RetargetBeforeSendIsOneCommand(t *testing.T) {
	checkGoroutines(t)
	gs, g, sh, _ := readyShutter(t)
	h := g.HoldNext(fakeSetPath)
	hkWrite(findC(t, gs.Clus[0].Lights[0].HkAccessory(), characteristic.TypeOn), true)
	h.Wait(t) // the light's command holds the write lane
	hkTarget(t, sh, 40)
	hkTarget(t, sh, 60)
	eventually(t, 2*time.Second, func() bool { return queuedIn(gs.writer, tSID) && sh.sim.inboxHandled() },
		"the shutter command waiting in the mailbox")
	h.Release()
	settle(t, gs)
	assertCmds(t, g, "MOVEDOWN")
	tick(t, gs, 40)
	settle(t, gs)
	assertShown(t, sh, shown{60, 60, hkStopped})
	assertCmds(t, g, "MOVEDOWN", "STOP")
}

// Retarget while moving: the new command (other direction) is submitted under the new
// generation. Until its acknowledgement the motor is believed moving the old way, and the old
// generation's target is passed without a STOP; the acknowledgement switches the belief and
// the new generation stops at its own target.
func TestShutter_RetargetWhileMovingPassesOldTargetWithoutStop(t *testing.T) {
	checkGoroutines(t)
	gs, g, sh, _ := readyShutter(t)
	hkTarget(t, sh, 80)
	settle(t, gs)
	tick(t, gs, 10)
	h := g.HoldNext(fakeSetPath)
	hkTarget(t, sh, 95)
	h.Wait(t)
	tick(t, gs, 15) // still believed moving down, past 80
	assertShown(t, sh, shown{75, 95, hkDecreasing})
	assertCmds(t, g, "MOVEDOWN", "MOVEUP")
	h.Release()
	settle(t, gs)
	assertShown(t, sh, shown{75, 95, hkIncreasing})
	tick(t, gs, 20)
	settle(t, gs)
	assertShown(t, sh, shown{95, 95, hkStopped})
	assertCmds(t, g, "MOVEDOWN", "MOVEUP", "STOP")
}

// A new target in the direction the motor already moves needs no command: the movement
// belongs to the new generation and runs to the new target.
func TestShutter_RetargetSameDirectionTakesOverMovement(t *testing.T) {
	checkGoroutines(t)
	gs, g, sh, _ := readyShutter(t)
	hkTarget(t, sh, 40)
	settle(t, gs)
	tick(t, gs, 20)
	hkTarget(t, sh, 20)
	settle(t, gs)
	tick(t, gs, 60) // past 40
	settle(t, gs)
	assertShown(t, sh, shown{20, 20, hkStopped})
	assertCmds(t, g, "MOVEDOWN", "STOP")
}

// An older generation's acknowledgement updates the belief: MOVEUP is in flight, the user
// asks for STOP, MOVEUP is acknowledged. The motor is believed moving up and the clock runs
// upwards without a target; the STOP's acknowledgement then stops it.
func TestShutter_OlderGenerationAckUpdatesBelief(t *testing.T) {
	checkGoroutines(t)
	gs, g, sh, clk := readyShutter(t)
	moveTo(t, gs, sh, 50)
	hm := g.HoldNext(fakeSetPath)
	hkTarget(t, sh, 80)
	hm.Wait(t)
	hs := g.HoldNext(fakeSetPath)
	hkTarget(t, sh, 50) // the current position: STOP
	eventually(t, 2*time.Second, sh.sim.inboxHandled, "the simulation to handle the target")
	hm.Release()
	hs.Wait(t) // MOVEUP acknowledged, STOP in flight
	eventually(t, 2*time.Second, func() bool { return clk.running() }, "the clock to start")
	tick(t, gs, 35) // past MOVEUP's target 80: that generation no longer issues a STOP
	assertShown(t, sh, shown{85, 50, hkIncreasing})
	assertCmds(t, g, "MOVEDOWN", "STOP", "MOVEUP", "STOP")
	hs.Release()
	settle(t, gs)
	assertShown(t, sh, shown{85, 85, hkStopped})
	if clk.running() {
		t.Error("clock still running after the STOP was acknowledged")
	}
	assertCmds(t, g, "MOVEDOWN", "STOP", "MOVEUP", "STOP")

	// A wall switch then moves it up again: that movement is not MOVEUP's any more.
	g.SetShutter(tCLU, tSID, 20000, 1)
	gs.Refresh()
	settle(t, gs)
	assertShown(t, sh, shown{85, 85, hkIncreasing})
	if clk.running() {
		t.Error("clock runs for a movement grengate did not start")
	}
}

// Acknowledgement processed after a newer observation: the move is acknowledged, the
// confirm read shows the motor stopped and is processed first; the late acknowledgement
// records that the move ran (no retry) but leaves the belief at "stopped".
func TestShutter_AckAfterNewerObservationKeepsBelief(t *testing.T) {
	checkGoroutines(t)
	gs, g, sh, clk := readyShutter(t)
	release := make(chan struct{})
	sh.sim.resultHook = func(c *shutterCommand, _ laneResult) {
		if c.cmd == "MOVEDOWN" {
			<-release
		}
	}
	h := g.HoldNext(fakeSetPath)
	hkTarget(t, sh, 40)
	h.Wait(t)
	g.SetShutter(tCLU, tSID, 20000, 0) // the motor stopped before the confirm read
	h.Release()
	eventually(t, 2*time.Second, func() bool {
		return gs.writer.idle() && gs.reader.idle() && sh.sim.inboxHandled() && g.RequestCount(fakeReadPath) == 2
	}, "the confirm read to be handled")
	close(release)
	settle(t, gs)
	assertShown(t, sh, shown{100, 100, hkStopped})
	if clk.running() {
		t.Error("clock started by the late acknowledgement")
	}
	assertCmds(t, g, "MOVEDOWN")
}

// Externally observed stop: a read shows the shutter stopped; the simulation ends, the
// position is kept and the target set to it. No STOP is sent.
func TestShutter_ExternallyObservedStopEndsSimulation(t *testing.T) {
	checkGoroutines(t)
	gs, g, sh, clk := readyShutter(t)
	hkTarget(t, sh, 40)
	settle(t, gs)
	tick(t, gs, 30)
	g.SetShutter(tCLU, tSID, 20000, 0)
	gs.Refresh()
	settle(t, gs)
	assertShown(t, sh, shown{70, 70, hkStopped})
	if clk.running() {
		t.Error("clock still running")
	}
	assertCmds(t, g, "MOVEDOWN")
}

// Movement started from a wall switch updates the HomeKit movement state but not the
// position, as before.
func TestShutter_WallSwitchMovementKeepsPosition(t *testing.T) {
	checkGoroutines(t)
	gs, g, sh, clk := readyShutter(t)
	g.SetShutter(tCLU, tSID, 20000, 2)
	gs.Refresh()
	settle(t, gs)
	assertShown(t, sh, shown{100, 100, hkDecreasing})
	if clk.running() {
		t.Error("clock runs for a movement grengate did not start")
	}
	g.SetShutter(tCLU, tSID, 20000, 0)
	gs.Refresh()
	settle(t, gs)
	assertShown(t, sh, shown{100, 100, hkStopped})
	assertCmds(t, g)
}

// moveTo moves the shutter to a target that is not an end stop: the move is acknowledged,
// the clock runs to the target and the STOP there is acknowledged.
func moveTo(t *testing.T, gs *GrentonSet, sh *Shutter, target int) {
	t.Helper()
	from := shownBy(sh).current
	hkTarget(t, sh, target)
	settle(t, gs)
	tick(t, gs, max(target-from, from-target))
	settle(t, gs)
	assertShown(t, sh, shown{target, target, hkStopped})
}

// A late acknowledgement whose movement a newer observation already showed: the read is
// processed first (the motor moves down, nobody known to have started it, so the clock does
// not run), then the acknowledgement of MOVEDOWN tells that the movement is ours: the clock
// runs towards the target.
func TestShutter_LateAckClaimsObservedMovement(t *testing.T) {
	checkGoroutines(t)
	gs, g, sh, clk := readyShutter(t)
	release := make(chan struct{})
	sh.sim.resultHook = func(c *shutterCommand, _ laneResult) {
		if c.cmd == "MOVEDOWN" {
			<-release
		}
	}
	hkTarget(t, sh, 40)
	eventually(t, 2*time.Second, func() bool {
		return gs.writer.idle() && gs.reader.idle() && sh.sim.inboxHandled() && g.RequestCount(fakeReadPath) == 2
	}, "the confirm read to be handled")
	syncHomeKit(t, gs)
	assertShown(t, sh, shown{100, 40, hkDecreasing})
	if clk.running() {
		t.Error("clock runs before grengate knows the movement is its own")
	}
	close(release)
	settle(t, gs)
	tick(t, gs, 10)
	assertShown(t, sh, shown{90, 40, hkDecreasing})
}

// Automatic STOP at the target: submitted by the latest generation once the clock reaches
// the target; its acknowledgement stops the motor and the simulation.
func TestShutter_StopAtTarget(t *testing.T) {
	checkGoroutines(t)
	gs, g, sh, clk := readyShutter(t)
	hkTarget(t, sh, 40)
	settle(t, gs)
	tick(t, gs, 59)
	assertCmds(t, g, "MOVEDOWN")
	h := g.HoldNext(fakeSetPath)
	tick(t, gs, 1)
	h.Wait(t)
	if clk.running() {
		t.Error("clock still running at the target")
	}
	assertShown(t, sh, shown{40, 40, hkDecreasing})
	h.Release()
	settle(t, gs)
	assertShown(t, sh, shown{40, 40, hkStopped})
	assertCmds(t, g, "MOVEDOWN", "STOP")
}

// A target at an end stop is held for a full travel time before the STOP (as the loop-based
// simulation did), so the motor surely reaches the end stop whatever the simulation's error.
func TestShutter_EndStopTargetRunsFullTravel(t *testing.T) {
	checkGoroutines(t)
	gs, g, sh, _ := readyShutter(t)
	moveTo(t, gs, sh, 50)
	hkTarget(t, sh, 0)
	settle(t, gs)
	tick(t, gs, 99)
	settle(t, gs)
	assertShown(t, sh, shown{0, 0, hkDecreasing})
	assertCmds(t, g, "MOVEDOWN", "STOP", "MOVEDOWN")
	tick(t, gs, 1)
	settle(t, gs)
	assertShown(t, sh, shown{0, 0, hkStopped})
	assertCmds(t, g, "MOVEDOWN", "STOP", "MOVEDOWN", "STOP")
}

// A late acknowledgement of the automatic STOP still records that the STOP ran: the confirm
// read (the CLU applies the STOP with a delay) shows the motor still moving and is processed
// first, so the belief stays "moving down", but no second STOP is sent. The next poll shows
// the motor stopped and ends the simulation.
func TestShutter_LateStopAckIsNotRepeated(t *testing.T) {
	checkGoroutines(t)
	gs, g, sh, clk := readyShutter(t)
	hkTarget(t, sh, 40)
	settle(t, gs)
	tick(t, gs, 59)
	release := make(chan struct{})
	sh.sim.resultHook = func(c *shutterCommand, _ laneResult) {
		if c.cmd == "STOP" {
			<-release
		}
	}
	h := g.HoldNext(fakeSetPath)
	tick(t, gs, 1)
	h.Wait(t)
	g.SetShutter(tCLU, tSID, 20000, 2)
	h.Release()
	eventually(t, 2*time.Second, func() bool {
		return gs.writer.idle() && gs.reader.idle() && sh.sim.inboxHandled() && g.RequestCount(fakeReadPath) == 3
	}, "the confirm read to be handled")
	close(release)
	settle(t, gs)
	assertShown(t, sh, shown{40, 40, hkDecreasing})
	if clk.running() {
		t.Error("clock running at the target")
	}
	assertCmds(t, g, "MOVEDOWN", "STOP")

	g.SetShutter(tCLU, tSID, 20000, 0)
	gs.Refresh()
	settle(t, gs)
	assertShown(t, sh, shown{40, 40, hkStopped})
	assertCmds(t, g, "MOVEDOWN", "STOP")
}

// Failure table, row "per-item error, latest generation": the belief is unchanged, the
// command is retried once, then given up: the HomeKit target is set to where the motor is
// believed to end up (here: stopped, so the current position).
func TestShutter_ItemErrorOnMoveRetriedOnceThenGivenUp(t *testing.T) {
	checkGoroutines(t)
	logs := captureLog(t)
	gs, g, sh, clk := readyShutter(t)
	g.SetWriteStyle(fakeNewStyle)
	g.FailItem(tCLU, tSID, "boom")
	hkTarget(t, sh, 40)
	settle(t, gs)
	assertCmds(t, g, "MOVEDOWN", "MOVEDOWN")
	assertShown(t, sh, shown{100, 100, hkStopped})
	if clk.running() {
		t.Error("clock running after a move that did not run")
	}
	if !strings.Contains(logs.String(), "giving up on target 40") {
		t.Errorf("give-up not logged:\n%s", logs.String())
	}
}

// The retry of a move that got a per-item error runs: the movement goes on as usual.
func TestShutter_ItemErrorOnMoveRetrySucceeds(t *testing.T) {
	checkGoroutines(t)
	gs, g, sh, _ := readyShutter(t)
	g.SetWriteStyle(fakeNewStyle)
	g.FailItem(tCLU, tSID, "boom")
	h := g.HoldNext(fakeSetPath)
	hkTarget(t, sh, 40)
	h.Wait(t) // answered with the per-item error once released
	g.ClearFailItem(tCLU, tSID)
	h.Release()
	settle(t, gs)
	assertShown(t, sh, shown{100, 40, hkDecreasing})
	tick(t, gs, 60)
	settle(t, gs)
	assertShown(t, sh, shown{40, 40, hkStopped})
	assertCmds(t, g, "MOVEDOWN", "MOVEDOWN", "STOP")
}

// Per-item error on a retarget: the new generation's MOVEUP does not run, nor does its
// retry; the motor is still believed moving down (the old generation's movement), so the
// target given up becomes the end stop of that direction, where the clock runs to.
func TestShutter_ItemErrorOnRetargetGivesUpToEndStop(t *testing.T) {
	checkGoroutines(t)
	gs, g, sh, clk := readyShutter(t)
	g.SetWriteStyle(fakeNewStyle)
	hkTarget(t, sh, 40)
	settle(t, gs)
	tick(t, gs, 10)
	g.FailItem(tCLU, tSID, "boom")
	hkTarget(t, sh, 95)
	settle(t, gs)
	assertCmds(t, g, "MOVEDOWN", "MOVEUP", "MOVEUP")
	assertShown(t, sh, shown{90, 0, hkDecreasing})
	tick(t, gs, 90)
	settle(t, gs)
	assertShown(t, sh, shown{0, 0, hkDecreasing})
	if clk.running() {
		t.Error("clock running at the end stop")
	}
	assertCmds(t, g, "MOVEDOWN", "MOVEUP", "MOVEUP")
}

// Per-item error on the automatic STOP: retried once, then given up; the motor is believed
// still moving down, so the target becomes the end stop and no further STOP is sent.
func TestShutter_ItemErrorOnAutoStopGivesUpToEndStop(t *testing.T) {
	checkGoroutines(t)
	gs, g, sh, _ := readyShutter(t)
	g.SetWriteStyle(fakeNewStyle)
	hkTarget(t, sh, 40)
	settle(t, gs)
	tick(t, gs, 59)
	g.FailItem(tCLU, tSID, "boom")
	tick(t, gs, 1)
	settle(t, gs)
	assertCmds(t, g, "MOVEDOWN", "STOP", "STOP")
	assertShown(t, sh, shown{40, 0, hkDecreasing})
	tick(t, gs, 40)
	settle(t, gs)
	assertShown(t, sh, shown{0, 0, hkDecreasing})
	assertCmds(t, g, "MOVEDOWN", "STOP", "STOP")
}

// Failure table, row "per-item error, older generation": nothing happens; the newer
// generation's command decides.
func TestShutter_ItemErrorOlderGenerationDoesNothing(t *testing.T) {
	checkGoroutines(t)
	gs, g, sh, _ := readyShutter(t)
	g.SetWriteStyle(fakeNewStyle)
	g.FailItem(tCLU, tSID, "boom")
	h := g.HoldNext(fakeSetPath)
	hkTarget(t, sh, 40)
	h.Wait(t)
	g.ClearFailItem(tCLU, tSID)
	hkTarget(t, sh, 100) // the position: STOP
	eventually(t, 2*time.Second, sh.sim.inboxHandled, "the simulation to handle the target")
	h.Release()
	settle(t, gs)
	assertCmds(t, g, "MOVEDOWN", "STOP")
	assertShown(t, sh, shown{100, 100, hkStopped})
}

// MOVEUP in flight, STOP requested, MOVEUP acknowledged, STOP fails (per-item error, also on
// its retry): the motor is believed moving up; the target given up is the end stop above.
func TestShutter_FailedStopAfterOlderMoveAckLeavesMotorMovingUp(t *testing.T) {
	checkGoroutines(t)
	gs, g, sh, clk := readyShutter(t)
	g.SetWriteStyle(fakeNewStyle)
	moveTo(t, gs, sh, 50)
	hm := g.HoldNext(fakeSetPath)
	hkTarget(t, sh, 80)
	hm.Wait(t)
	g.FailItem(tCLU, tSID, "boom")
	hkTarget(t, sh, 50) // STOP
	eventually(t, 2*time.Second, sh.sim.inboxHandled, "the simulation to handle the target")
	hm.Release()
	settle(t, gs)
	assertCmds(t, g, "MOVEDOWN", "STOP", "MOVEUP", "STOP", "STOP")
	assertShown(t, sh, shown{50, 100, hkIncreasing})
	tick(t, gs, 50)
	settle(t, gs)
	assertShown(t, sh, shown{100, 100, hkIncreasing})
	if clk.running() {
		t.Error("clock running at the end stop")
	}
	assertCmds(t, g, "MOVEDOWN", "STOP", "MOVEUP", "STOP", "STOP")
}

// uncertainMove submits a move to 40 whose outcome is uncertain: ran says whether the fake
// GATE executed it (answered 500 after processing) or not (answered 500 without processing).
func uncertainMove(t *testing.T, g *fakeGate, sh *Shutter, ran bool) {
	t.Helper()
	if !ran {
		g.FailNext(fakeSetPath, 500, 1)
		hkTarget(t, sh, 40)
		return
	}
	h := g.HoldNext(fakeSetPath)
	hkTarget(t, sh, 40)
	h.Wait(t)
	h.ReleaseWithStatus(500)
}

// Reconciliation table, row "move, read shows moving in its direction": it ran; the clock
// runs towards the target from now. Not retried.
func TestShutter_UncertainMoveReadSameDirection(t *testing.T) {
	checkGoroutines(t)
	gs, g, sh, _ := readyShutter(t)
	uncertainMove(t, g, sh, true)
	settle(t, gs)
	assertShown(t, sh, shown{100, 40, hkDecreasing})
	tick(t, gs, 60)
	settle(t, gs)
	assertShown(t, sh, shown{40, 40, hkStopped})
	assertCmds(t, g, "MOVEDOWN", "STOP")
}

// Reconciliation table, row "move, read shows moving the other way": it did not run; the
// belief follows the read, the move is retried once, and its acknowledgement switches the
// belief.
func TestShutter_UncertainMoveReadOtherDirectionRetried(t *testing.T) {
	checkGoroutines(t)
	gs, g, sh, _ := readyShutter(t)
	g.SetShutter(tCLU, tSID, 20000, 1) // a wall switch moves it up meanwhile
	uncertainMove(t, g, sh, false)
	settle(t, gs)
	assertCmds(t, g, "MOVEDOWN", "MOVEDOWN")
	assertShown(t, sh, shown{100, 40, hkDecreasing})
}

// The retry of a move that did not run (uncertain, read shows the other direction) does not
// run either: given up, the target becomes the end stop of the observed direction.
func TestShutter_UncertainMoveReadOtherDirectionGivenUp(t *testing.T) {
	checkGoroutines(t)
	gs, g, sh, clk := readyShutter(t)
	g.SetShutter(tCLU, tSID, 20000, 1)
	g.FailNext(fakeSetPath, 500, 2)
	hkTarget(t, sh, 40)
	settle(t, gs)
	assertCmds(t, g, "MOVEDOWN", "MOVEDOWN")
	assertShown(t, sh, shown{100, 100, hkIncreasing})
	if clk.running() {
		t.Error("clock runs for a movement grengate did not start")
	}
}

// Reconciliation table, row "move, read shows stopped": ambiguous. The simulation ends with
// the target set to the position; not retried.
func TestShutter_UncertainMoveReadStopped(t *testing.T) {
	checkGoroutines(t)
	gs, g, sh, clk := readyShutter(t)
	uncertainMove(t, g, sh, false)
	settle(t, gs)
	assertShown(t, sh, shown{100, 100, hkStopped})
	assertCmds(t, g, "MOVEDOWN")
	if clk.running() {
		t.Error("clock running")
	}
}

// Reconciliation table, row "any, the read fails as well": it stays uncertain, nothing is
// retried and the clock does not start; the next poll decides (here: the move ran).
func TestShutter_UncertainMoveReadFails(t *testing.T) {
	for _, tc := range []struct {
		name  string
		spoil func(g *fakeGate)
		after func(g *fakeGate)
	}{
		{"read request fails", func(g *fakeGate) { g.FailNext(fakeReadPath, 500, 1) }, func(*fakeGate) {}},
		{"shutter missing from the read", func(g *fakeGate) { g.Omit(tCLU, tSID) }, func(g *fakeGate) { g.Unomit(tCLU, tSID) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checkGoroutines(t)
			gs, g, sh, clk := readyShutter(t)
			tc.spoil(g)
			uncertainMove(t, g, sh, true)
			settle(t, gs)
			assertShown(t, sh, shown{100, 40, hkStopped})
			if clk.running() {
				t.Error("clock started without evidence")
			}
			assertCmds(t, g, "MOVEDOWN")

			tc.after(g)
			gs.Refresh()
			settle(t, gs)
			assertShown(t, sh, shown{100, 40, hkDecreasing})
			tick(t, gs, 60)
			settle(t, gs)
			assertShown(t, sh, shown{40, 40, hkStopped})
			assertCmds(t, g, "MOVEDOWN", "STOP")
		})
	}
}

// An uncertain result handled after a newer observation is reconciled with that observation
// at once instead of waiting for the next poll.
func TestShutter_UncertainResultAfterNewerObservationReconcilesAtOnce(t *testing.T) {
	checkGoroutines(t)
	gs, g, sh, _ := readyShutter(t)
	release := make(chan struct{})
	sh.sim.resultHook = func(c *shutterCommand, _ laneResult) {
		if c.cmd == "MOVEDOWN" {
			<-release
		}
	}
	uncertainMove(t, g, sh, true)
	eventually(t, 2*time.Second, func() bool {
		return gs.writer.idle() && gs.reader.idle() && sh.sim.inboxHandled() && g.RequestCount(fakeReadPath) == 2
	}, "the confirm read to be handled")
	close(release)
	settle(t, gs)
	tick(t, gs, 60)
	settle(t, gs)
	assertShown(t, sh, shown{40, 40, hkStopped})
	assertCmds(t, g, "MOVEDOWN", "STOP")
}

// Reconciliation table, row "STOP, read shows stopped": the simulation ends with the target
// set to the position.
func TestShutter_UncertainStopReadStopped(t *testing.T) {
	checkGoroutines(t)
	gs, g, sh, _ := readyShutter(t)
	hkTarget(t, sh, 40)
	settle(t, gs)
	tick(t, gs, 59)
	h := g.HoldNext(fakeSetPath)
	tick(t, gs, 1)
	h.Wait(t)
	h.ReleaseWithStatus(500)
	settle(t, gs)
	assertShown(t, sh, shown{40, 40, hkStopped})
	assertCmds(t, g, "MOVEDOWN", "STOP")
}

// Reconciliation table, row "STOP, read shows still moving": it did not run. The observed
// movement is kept and the STOP is retried once under the same generation; here the retry
// runs.
func TestShutter_UncertainStopReadStillMovingRetried(t *testing.T) {
	checkGoroutines(t)
	gs, g, sh, _ := readyShutter(t)
	hkTarget(t, sh, 40)
	settle(t, gs)
	tick(t, gs, 59)
	hr := g.HoldNext(fakeReadPath)
	g.FailNext(fakeSetPath, 500, 1)
	tick(t, gs, 1)
	hr.Wait(t) // the confirm read saw the motor still moving down
	waitFor(t, sh.sim.idle, "the uncertain result to be handled")
	syncHomeKit(t, gs)
	assertShown(t, sh, shown{40, 40, hkDecreasing})
	hr.Release()
	settle(t, gs)
	assertShown(t, sh, shown{40, 40, hkStopped})
	assertCmds(t, g, "MOVEDOWN", "STOP", "STOP")
}

// The retry of a STOP that did not run (uncertain, read shows still moving, either
// direction) does not run either: given up, the target becomes the end stop of the observed
// direction.
func TestShutter_UncertainStopReadStillMovingGivenUp(t *testing.T) {
	for _, tc := range []struct {
		name       string
		state      int // what the reads show
		want       shown
		clockAfter bool // the clock runs on: the observed movement is the generation's own
	}{
		{"same direction", 2, shown{40, 0, hkDecreasing}, true},
		{"other direction", 1, shown{40, 100, hkIncreasing}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checkGoroutines(t)
			gs, g, sh, clk := readyShutter(t)
			hkTarget(t, sh, 40)
			settle(t, gs)
			tick(t, gs, 59)
			g.SetShutter(tCLU, tSID, 20000, tc.state)
			g.FailNext(fakeSetPath, 500, 2)
			tick(t, gs, 1)
			settle(t, gs)
			assertCmds(t, g, "MOVEDOWN", "STOP", "STOP")
			assertShown(t, sh, tc.want)
			if clk.running() != tc.clockAfter {
				t.Errorf("clock running = %v, want %v", clk.running(), tc.clockAfter)
			}
		})
	}
}

// Failure table, row "uncertain outcome": the belief is unchanged and the clock carries on
// until the confirm read decides. Here a retarget's MOVEUP is uncertain while the motor is
// believed moving down: the clock keeps moving down; the read shows it still moving down,
// so MOVEUP did not run and is retried.
func TestShutter_UncertainRetargetClockCarriesOn(t *testing.T) {
	checkGoroutines(t)
	gs, g, sh, _ := readyShutter(t)
	hkTarget(t, sh, 40)
	settle(t, gs)
	tick(t, gs, 10)
	hr := g.HoldNext(fakeReadPath)
	g.FailNext(fakeSetPath, 500, 1)
	hkTarget(t, sh, 95)
	hr.Wait(t)
	eventually(t, 2*time.Second, sh.sim.idle, "the uncertain result to be handled")
	tick(t, gs, 5)
	assertShown(t, sh, shown{85, 95, hkDecreasing})
	hr.Release()
	settle(t, gs)
	assertShown(t, sh, shown{85, 95, hkIncreasing})
	assertCmds(t, g, "MOVEDOWN", "MOVEUP", "MOVEUP")
}

// Failure table, row "uncertain outcome, older generation": nothing happens for it; the
// newer generation's command decides. MOVEDOWN is in flight when the user retargets up; it
// ends uncertain, MOVEUP is acknowledged and the reads show the motor moving up: MOVEDOWN is
// not reconciled against them, so it is not retried as "did not run".
func TestShutter_UncertainOlderGenerationDoesNothing(t *testing.T) {
	checkGoroutines(t)
	gs, g, sh, _ := readyShutter(t)
	moveTo(t, gs, sh, 50)
	h := g.HoldNext(fakeSetPath)
	hkTarget(t, sh, 20)
	h.Wait(t)
	hkTarget(t, sh, 80)
	eventually(t, 2*time.Second, sh.sim.inboxHandled, "the simulation to handle the target")
	h.ReleaseWithStatus(500)
	settle(t, gs)
	assertCmds(t, g, "MOVEDOWN", "STOP", "MOVEDOWN", "MOVEUP")
	assertShown(t, sh, shown{50, 80, hkIncreasing})
	tick(t, gs, 30)
	settle(t, gs)
	assertShown(t, sh, shown{80, 80, hkStopped})
	assertCmds(t, g, "MOVEDOWN", "STOP", "MOVEDOWN", "MOVEUP", "STOP")
}
