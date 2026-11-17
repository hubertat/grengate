package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestAutotest_PassesOnMatchingItemsForAllKinds(t *testing.T) {
	logs := captureLog(t)
	gs, g := newTestSet(t, allKindsClu())
	rep := gs.Autotest()
	if rep.Passed != 4 || len(rep.Failures) != 0 {
		t.Errorf("report = %+v, want 4 passed", rep)
	}
	reqs := g.Requests(fakeReadPath)
	if len(reqs) != 1 || !reqs[0].IsArray || len(reqs[0].Items) != 4 {
		t.Fatalf("want one array read request with the 4 devices, got %+v", reqs)
	}
	kinds := map[string]bool{}
	for _, it := range reqs[0].Items {
		kinds[it["Kind"].(string)] = true
	}
	for _, k := range []string{"Light", "Thermo", "Shutter", "MotionSensor"} {
		if !kinds[k] {
			t.Errorf("kind %s not tested", k)
		}
	}
	if g.RequestCount(fakeSetPath) != 0 {
		t.Error("autotest must not write")
	}
	if !strings.Contains(logs.String(), "all 4 devices passed") {
		t.Errorf("result not logged:\n%s", logs.String())
	}
}

func TestAutotest_FailsOnEmptyResponse(t *testing.T) {
	gs, g := newTestSet(t, allKindsClu())
	g.RespondNext(fakeReadPath, `[]`)
	rep := gs.Autotest()
	if rep.Passed != 0 || len(rep.Failures) != 4 {
		t.Errorf("report = %+v, want 4 failures", rep)
	}
}

func TestAutotest_FailsOnWrongIdentity(t *testing.T) {
	clu := allKindsClu()
	gs, g := newTestSet(t, clu)
	g.RespondNext(fakeReadPath, `[
		{"Clu":"CLU_012abcde","Id":"DOU0001","Kind":"Thermo","Thermo":{"TempCurrent":20}},
		{"Clu":"CLU_other","Id":"THE0002","Kind":"Thermo","Thermo":{"TempCurrent":20}},
		{"Clu":"CLU_012abcde","Id":"ROL0009","Kind":"Shutter","Shutter":{"MaxTime":20000}},
		{"Clu":"CLU_012abcde","Id":"PIR0004","Kind":"MotionSensor","MotionSensor":{"State":false}}]`)
	rep := gs.Autotest()
	if rep.Passed != 1 || len(rep.Failures) != 3 {
		t.Fatalf("report = %+v, want only the motion sensor to pass", rep)
	}
	all := strings.Join(rep.Failures, "\n")
	for _, name := range []string{"lamp", "thermo", "blind"} {
		if !strings.Contains(all, name) {
			t.Errorf("failure of %q not reported:\n%s", name, all)
		}
	}
	if !strings.Contains(rep.Failures[0], "wrong object kind") {
		t.Errorf("kind mismatch not explained: %s", rep.Failures[0])
	}
}

func TestAutotest_FailsOnMissingPayload(t *testing.T) {
	clu := allKindsClu()
	gs, g := newTestSet(t, clu)
	g.DropPayload(tCLU, "THE0002")
	rep := gs.Autotest()
	if rep.Passed != 3 || len(rep.Failures) != 1 || !strings.Contains(rep.Failures[0], "missing Thermo") {
		t.Errorf("report = %+v, want the thermostat to fail for its missing payload", rep)
	}
}

func TestAutotest_FailsOnFailedRequestAndKeepsDevices(t *testing.T) {
	gs, g := newTestSet(t, allKindsClu())
	g.FailNext(fakeReadPath, 500, 1)
	rep := gs.Autotest()
	if rep.Passed != 0 || len(rep.Failures) != 4 || !strings.Contains(rep.Failures[0], "request failed") {
		t.Errorf("report = %+v", rep)
	}
	// report-only: every device is still read afterwards
	if c := gs.Refresh(); c.Applied != 4 {
		t.Errorf("refresh after a failed autotest = %+v, want all 4 applied", c)
	}
}

func TestStartup_WithAutotestAndHangingGate(t *testing.T) {
	const timeout = 100 * time.Millisecond
	gs, g := newTestSetWith(t, func(gs *GrentonSet) {
		gs.QueryLimit = 2
		gs.requestTimeout = timeout
	}, lightsClu(3))
	g.HangAll("")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	start := time.Now()
	gs.startup(ctx, true)
	elapsed := time.Since(start)
	// autotest: 2 requests, startup refresh: 2 requests, each ending at the timeout
	if elapsed < 4*timeout || elapsed > 4*timeout+time.Second {
		t.Errorf("startup took %v, want about %v", elapsed, 4*timeout)
	}
	if n := g.RequestCount(fakeReadPath); n != 4 {
		t.Errorf("read requests = %d, want 4", n)
	}
	if n := g.RequestCount(fakeSetPath); n != 0 {
		t.Errorf("set requests = %d, want 0", n)
	}
}
