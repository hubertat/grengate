package main

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/brutella/hap/characteristic"
)

// newBatchingLane is a write lane in the new protocol (arrays of up to size commands) whose
// batching window is driven by the returned fake timers.
func newBatchingLane(t *testing.T, g *fakeGate, size int) (*lane, *fakeTimers) {
	t.Helper()
	timers := newFakeTimers()
	l := newTestLane(t, g, writeLane, func(c *laneConfig) {
		c.asArray, c.maxItems, c.merge = true, size, mergeCommand
		c.window, c.timers = 30*time.Millisecond, timers
	})
	return l, timers
}

// fireWindow waits until the write lane's batching window is open and closes it.
func fireWindow(t *testing.T, timers *fakeTimers) {
	t.Helper()
	eventually(t, 2*time.Second, timers.running, "the batching window to open")
	if d := timers.fire(t); d != 30*time.Millisecond {
		t.Errorf("window of %v, want 30ms", d)
	}
}

func lightIds(from, to int) []string {
	var ids []string
	for i := from; i <= to; i++ {
		ids = append(ids, lightId(i))
	}
	return ids
}

// Commands submitted inside the window leave in one request; more than SetterQueueSize
// commands split into several requests, oldest first.
func TestBatching_WindowGroupsOldestFirst(t *testing.T) {
	for _, n := range []int{3, 12} {
		t.Run(fmt.Sprintf("%d commands", n), func(t *testing.T) {
			g := newFakeGate(t)
			g.SetWriteStyle(fakeNewStyle)
			for i := 1; i <= n; i++ {
				g.SetLight(tCLU, lightId(i), false)
			}
			l, timers := newBatchingLane(t, g, 5)

			var chans []<-chan laneResult
			for i := 1; i <= n; i++ { // one submission each, as the HomeKit callbacks do
				chans = append(chans, l.Submit(lightCmd(lightId(i), true))[0])
			}
			eventually(t, 2*time.Second, timers.running, "the batching window to open")
			if c := g.RequestCount(fakeSetPath); c != 0 {
				t.Fatalf("%d requests sent before the window closed", c)
			}
			fireWindow(t, timers)
			for _, ch := range chans {
				if r := waitResult(t, ch); r.Outcome != OutcomeOK {
					t.Errorf("outcome %v", r.Outcome)
				}
			}

			reqs := g.Requests(fakeSetPath)
			var want [][]string
			for from := 1; from <= n; from += 5 {
				want = append(want, lightIds(from, min(from+4, n)))
			}
			if len(reqs) != len(want) {
				t.Fatalf("%d requests, want %d", len(reqs), len(want))
			}
			for i, r := range reqs {
				if !r.IsArray || !slices.Equal(itemIds(r), want[i]) {
					t.Errorf("request %d = %s, want an array of %v", i, r.Body, want[i])
				}
			}
			if timers.running() {
				t.Error("a second window was opened for the leftovers")
			}
		})
	}
}

// Commands left in the mailbox after a request was taken keep latest-wins semantics until
// they are taken; commands that waited behind a request leave as soon as it completes,
// without another window.
func TestBatching_LeftoversKeepReplaceSemantics(t *testing.T) {
	g := newFakeGate(t)
	g.SetWriteStyle(fakeNewStyle)
	for i := 1; i <= 7; i++ {
		g.SetLight(tCLU, lightId(i), false)
	}
	l, timers := newBatchingLane(t, g, 5)

	var chans []<-chan laneResult
	for i := 1; i <= 7; i++ {
		chans = append(chans, l.Submit(lightCmd(lightId(i), true))[0])
	}
	h := g.HoldNext(fakeSetPath)
	fireWindow(t, timers)
	h.Wait(t)
	newer := l.Submit(lightCmd(lightId(6), false))[0]                 // replaces a leftover
	late := l.Submit(lightCmd(lightId(1), false))[0]                  // the first request has DOU0001: not replaced
	if r := waitResult(t, chans[5]); r.Outcome != OutcomeSuperseded { // DOU0006
		t.Errorf("replaced leftover: %v, want superseded", r.Outcome)
	}
	h.Release()
	for _, ch := range []<-chan laneResult{chans[0], chans[6], newer, late} {
		if r := waitResult(t, ch); r.Outcome != OutcomeOK {
			t.Errorf("outcome %v", r.Outcome)
		}
	}

	reqs := g.Requests(fakeSetPath)
	if len(reqs) != 2 {
		t.Fatalf("%d requests, want 2", len(reqs))
	}
	if ids := itemIds(reqs[1]); !slices.Equal(ids, []string{"DOU0006", "DOU0007", "DOU0001"}) {
		t.Errorf("second request %v, want DOU0006 (in its place), DOU0007, DOU0001", ids)
	}
	if v := reqs[1].Items[0]["Light"].(map[string]any)["State"]; v != false {
		t.Errorf("DOU0006 sent %v, want the newer command (false)", v)
	}
	if timers.running() {
		t.Error("commands that waited behind a request got another window")
	}
}

// A lane stopped while its window is open resolves the waiting commands as not run.
func TestBatching_StopDuringWindow(t *testing.T) {
	g := newFakeGate(t)
	g.SetLight(tCLU, lightId(1), false)
	l, timers := newBatchingLane(t, g, 5)
	ch := l.Submit(lightCmd(lightId(1), true))[0]
	eventually(t, 2*time.Second, timers.running, "the batching window to open")
	l.stop()
	if r := waitResult(t, ch); r.Outcome != OutcomeError {
		t.Errorf("outcome %v, want error (not run)", r.Outcome)
	}
	if n := g.RequestCount(fakeSetPath); n != 0 {
		t.Errorf("%d requests sent", n)
	}
}

// A HomeKit request setting ten lights reaches the GATE as two requests of five.
func TestBatching_TenLightsTwoRequestsOfFive(t *testing.T) {
	timers := newFakeTimers()
	clu := lightsClu(10)
	gs, g := newTestSetWith(t, func(gs *GrentonSet) {
		withNewProtocol(5)(gs)
		gs.writeTimers = timers
	}, clu)
	g.SetWriteStyle(fakeNewStyle)
	gs.Refresh()
	settle(t, gs)

	for _, l := range clu.Lights {
		if st := hkWrite(findC(t, l.HkAccessory(), characteristic.TypeOn), true); st != 0 {
			t.Fatalf("status %d", st)
		}
	}
	fireWindow(t, timers)
	settle(t, gs)

	reqs := g.Requests(fakeSetPath)
	if len(reqs) != 2 {
		t.Fatalf("%d set requests, want 2", len(reqs))
	}
	for i, want := range [][]string{lightIds(1, 5), lightIds(6, 10)} {
		if !reqs[i].IsArray || !slices.Equal(itemIds(reqs[i]), want) {
			t.Errorf("request %d = %s, want an array of %v", i, reqs[i].Body, want)
		}
	}
	for i := 1; i <= 10; i++ {
		if g.Payload(tCLU, lightId(i))["State"] != true {
			t.Errorf("%s not switched on", lightId(i))
		}
	}
}

// Commands submitted after the window opened still leave with the first one, in one request.
func TestBatching_SubmissionsAfterWindowOpenedJoinIt(t *testing.T) {
	g := newFakeGate(t)
	g.SetWriteStyle(fakeNewStyle)
	for i := 1; i <= 4; i++ {
		g.SetLight(tCLU, lightId(i), false)
	}
	l, timers := newBatchingLane(t, g, 5)

	chans := []<-chan laneResult{l.Submit(lightCmd(lightId(1), true))[0]}
	eventually(t, 2*time.Second, timers.running, "the batching window to open")
	for i := 2; i <= 4; i++ {
		chans = append(chans, l.Submit(lightCmd(lightId(i), true))[0])
	}
	if n := g.RequestCount(fakeSetPath); n != 0 {
		t.Fatalf("%d requests sent while the window is open", n)
	}
	fireWindow(t, timers)
	for _, ch := range chans {
		waitResult(t, ch)
	}
	reqs := g.Requests(fakeSetPath)
	if len(reqs) != 1 || !slices.Equal(itemIds(reqs[0]), lightIds(1, 4)) {
		t.Errorf("requests %v, want one array of DOU0001..4", reqs)
	}
}

// A command submitted while the worker is between finding the mailbox empty and taking
// from it still gets the window. The race is short, so it is provoked by submitting right
// after the worker starts, many times.
func TestBatching_SubmissionRacingTheWorkerGetsWindow(t *testing.T) {
	g := newFakeGate(t)
	g.SetLight(tCLU, lightId(1), false)
	for i := 0; i < 300; i++ {
		timers := newFakeTimers()
		l := newLane(laneConfig{
			kind: writeLane, url: g.URL() + fakeSetPath, timeout: 2 * time.Second,
			asArray: true, maxItems: 5, merge: mergeCommand, window: 30 * time.Millisecond, timers: timers,
		})
		l.start()
		for spin := time.Now().Add(time.Duration(i%40) * time.Microsecond); time.Now().Before(spin); {
			// vary when the submission lands relative to the worker's first look at the mailbox
		}
		ch := l.Submit(lightCmd(lightId(1), true))[0]
		eventually(t, 2*time.Second, func() bool { return timers.running() || g.RequestCount(fakeSetPath) > 0 },
			"the window to open or a request to be sent")
		if n := g.RequestCount(fakeSetPath); n > 0 {
			l.stop()
			t.Fatalf("iteration %d: the command left without a window", i)
		}
		timers.fire(t)
		waitResult(t, ch)
		l.stop()
		g.mu.Lock()
		g.requests = nil
		g.mu.Unlock()
	}
}
