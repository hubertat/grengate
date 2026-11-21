package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/brutella/hap/characteristic"
)

// lightsClu returns a CLU with n lights DOU0001..DOU000n named light1..lightn.
func lightsClu(n int) *Clu {
	clu := &Clu{Id: tCLU}
	for i := 1; i <= n; i++ {
		clu.Lights = append(clu.Lights, &Light{CluObject: CluObject{Id: uint32(i), Kind: "DOU", Name: fmt.Sprintf("light%d", i)}})
	}
	return clu
}

func withQueryLimit(l int) func(*GrentonSet) {
	return func(gs *GrentonSet) { gs.QueryLimit = l }
}

func TestRefresh_RequestCounts(t *testing.T) {
	const limit = 10
	for _, n := range []int{5, 10, 25} {
		t.Run(fmt.Sprintf("N=%d", n), func(t *testing.T) {
			gs, g := newTestSetWith(t, withQueryLimit(limit), lightsClu(n))
			want := (n + limit - 1) / limit

			for cycle := 1; cycle <= 2; cycle++ {
				before := g.RequestCount(fakeReadPath)
				c := gs.Refresh()
				reqs := g.Requests(fakeReadPath)[before:]
				if len(reqs) != want {
					t.Fatalf("cycle %d: %d read requests, want ceil(%d/%d) = %d", cycle, len(reqs), n, limit, want)
				}
				seen := map[string]int{}
				for _, r := range reqs {
					if !r.IsArray || len(r.Items) > limit {
						t.Errorf("cycle %d: request %s: want an array of at most %d items", cycle, r.Body, limit)
					}
					for _, id := range itemIds(r) {
						seen[id]++
					}
				}
				for i := 1; i <= n; i++ {
					if seen[lightId(i)] != 1 {
						t.Errorf("cycle %d: %s read %d times, want once", cycle, lightId(i), seen[lightId(i)])
					}
				}
				if c.Objects != n || c.Applied != n {
					t.Errorf("cycle %d: counts %+v, want %d objects all applied", cycle, c, n)
				}
			}
			if g.RequestCount(fakeSetPath) != 0 {
				t.Error("refresh sent writes")
			}
		})
	}
}

func TestRefresh_AppliesStateToHomeKit(t *testing.T) {
	clu := allKindsClu()
	gs, g := newTestSet(t, clu)
	g.SetLight(tCLU, "DOU0001", true)
	g.SetThermo(tCLU, "THE0002", 19.5, 22, 0, 1)
	g.SetMotion(tCLU, "PIR0004", true)

	c := gs.Refresh()
	if c.Applied != 4 || c.Changed != 4 {
		t.Errorf("first refresh %+v, want 4 applied, 4 changed", c)
	}
	syncHomeKit(t, gs)
	if v := findC(t, clu.Lights[0].HkAccessory(), characteristic.TypeOn).Val; v != true {
		t.Errorf("light On = %v", v)
	}
	if v := clu.Therms[0].hk.Thermostat.CurrentTemperature.Value(); v != 19.5 {
		t.Errorf("current temperature = %v", v)
	}
	if v := findC(t, clu.MotionSensors[0].GetA(), characteristic.TypeMotionDetected).Val; v != true {
		t.Errorf("motion = %v", v)
	}

	c = gs.Refresh()
	if c.Applied != 4 || c.Changed != 0 {
		t.Errorf("second refresh %+v, want 4 applied, none changed", c)
	}
	g.SetLight(tCLU, "DOU0001", false)
	if c = gs.Refresh(); c.Changed != 1 {
		t.Errorf("third refresh %+v, want 1 changed", c)
	}
}

func TestRefresh_CompletesWithFailedRequestsAndMissingDevices(t *testing.T) {
	logs := captureLog(t)
	gs, g := newTestSetWith(t, withQueryLimit(2), lightsClu(6))
	g.FailNext(fakeReadPath, 500, 1)   // first request: light1, light2
	g.Omit(tCLU, "DOU0003")            // not in the response
	g.DropPayload(tCLU, "DOU0004")     // invalid
	g.SetLight(tCLU, "DOU0005", true)  // applied, changed
	g.SetLight(tCLU, "DOU0006", false) // applied

	c := gs.Refresh()
	want := refreshCounts{Objects: 6, Applied: 2, Changed: 2, Missing: 2, Failed: 2}
	c.Duration = 0
	if c != want {
		t.Errorf("counts = %+v, want %+v", c, want)
	}
	if g.RequestCount(fakeReadPath) != 3 {
		t.Errorf("read requests = %d, want 3", g.RequestCount(fakeReadPath))
	}
	out := logs.String()
	for _, s := range []string{"2 missing or invalid", "2 failed", "light3", "light4"} {
		if !strings.Contains(out, s) {
			t.Errorf("log lacks %q:\n%s", s, out)
		}
	}
	for _, s := range []string{"light3", "light4"} {
		if n := strings.Count(out, s); n != 1 {
			t.Errorf("%q logged %d times, want once:\n%s", s, n, out)
		}
	}
}

func TestRefresh_CompletesWhenEveryRequestTimesOut(t *testing.T) {
	gs, g := newTestSetWith(t, func(gs *GrentonSet) {
		gs.QueryLimit = 2
		gs.requestTimeout = 50 * time.Millisecond
	}, lightsClu(3))
	g.HangAll(fakeReadPath)
	done := make(chan refreshCounts, 1)
	go func() { done <- gs.Refresh() }()
	select {
	case c := <-done:
		if c.Failed != 3 || c.Objects != 3 {
			t.Errorf("counts = %+v, want 3 failed", c)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("refresh did not complete")
	}
}

func TestStartup_HangingGateReachesHomeKitAfterTimeouts(t *testing.T) {
	const timeout = 100 * time.Millisecond
	gs, g := newTestSetWith(t, func(gs *GrentonSet) {
		gs.QueryLimit = 2
		gs.requestTimeout = timeout
	}, lightsClu(5))
	g.HangAll("")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	start := time.Now()
	gs.startup(ctx, false)
	elapsed := time.Since(start)

	// 5 devices, 2 per request: three requests, one after another, each ending at the timeout.
	if elapsed < 3*timeout {
		t.Errorf("startup returned after %v, before the three request timeouts (%v)", elapsed, 3*timeout)
	}
	if elapsed > 3*timeout+time.Second {
		t.Errorf("startup took %v, want about %v", elapsed, 3*timeout)
	}
	if n := g.RequestCount(fakeReadPath); n != 3 {
		t.Errorf("read requests = %d, want 3", n)
	}
}

func TestStartup_RefreshesBeforeReturning(t *testing.T) {
	clu := lightsClu(2)
	gs, g := newTestSet(t, clu)
	g.SetLight(tCLU, "DOU0002", true)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	gs.startup(ctx, false)
	if v := findC(t, clu.Lights[1].HkAccessory(), characteristic.TypeOn).Val; v != true {
		t.Errorf("On = %v after startup, want the state read from the GATE", v)
	}
}

// A cancelled lifecycle context (SIGTERM during startup) stops startup promptly: the
// request in flight is aborted instead of running into the request timeout, and the
// requests still queued are not sent.
func TestStartup_CancelStopsPromptly(t *testing.T) {
	for _, autotest := range []bool{false, true} {
		t.Run(fmt.Sprintf("autotest=%v", autotest), func(t *testing.T) {
			gs, g := newTestSetWith(t, func(gs *GrentonSet) {
				gs.QueryLimit = 2
				gs.requestTimeout = 10 * time.Second
			}, lightsClu(5))
			g.HangAll("")
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)

			done := make(chan error, 1)
			go func() { done <- gs.startup(ctx, autotest) }()
			g.WaitForRequests(t, fakeReadPath, 1)
			cancel()

			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Errorf("startup returned %v, want context.Canceled", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("startup did not return after its context was cancelled")
			}
			if n := g.RequestCount(fakeReadPath); n != 1 {
				t.Errorf("read requests = %d, want only the one in flight at cancellation", n)
			}
			stopped := make(chan struct{})
			go func() { gs.Stop(); close(stopped) }()
			select {
			case <-stopped:
			case <-time.After(2 * time.Second):
				t.Fatal("Stop did not return after a cancelled startup")
			}
		})
	}
}

func TestStartup_ReturnsNilWhenNotCancelled(t *testing.T) {
	gs, _ := newTestSet(t, lightsClu(2))
	if err := gs.startup(context.Background(), false); err != nil {
		t.Errorf("startup = %v, want nil", err)
	}
}
