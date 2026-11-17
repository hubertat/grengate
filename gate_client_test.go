package main

import (
	"testing"
	"time"
)

// writeOnce sends one command through a fresh legacy write lane and returns its result.
func writeOnce(t *testing.T, g *fakeGate, cmd ReqObject, mut func(*laneConfig)) laneResult {
	t.Helper()
	l := newTestLane(t, g, writeLane, mut)
	return waitResult(t, l.Submit(cmd)[0])
}

func TestGateClient_WriteObjectResponseBothStyles(t *testing.T) {
	for _, style := range []fakeWriteStyle{fakeOldStyle, fakeNewStyle} {
		g := newFakeGate(t)
		g.SetWriteStyle(style)
		g.SetLight(tCLU, tLID, false)
		r := writeOnce(t, g, lightCmd(tLID, true), nil)
		if r.Outcome != OutcomeOK || r.Err != nil {
			t.Errorf("style %d: outcome %v (%v), want ok", style, r.Outcome, r.Err)
		}
		if r.Item != nil {
			t.Errorf("style %d: write result carries an item; a write response is never device state", style)
		}
		reqs := g.Requests(fakeSetPath)
		if len(reqs) != 1 || reqs[0].IsArray {
			t.Errorf("style %d: want one single-object request, got %+v", style, reqs)
		}
	}
}

func TestGateClient_WriteObjectAckMatchesCaseInsensitively(t *testing.T) {
	g := newFakeGate(t)
	g.SetLight(tCLU, tLID, false)
	g.RespondNext(fakeSetPath, `{"Clu":"clu_012ABCDE","Id":"dou0001","Kind":"Light"}`)
	if r := writeOnce(t, g, lightCmd(tLID, true), nil); r.Outcome != OutcomeOK {
		t.Errorf("outcome %v (%v), want ok", r.Outcome, r.Err)
	}
}

func TestGateClient_WriteArrayResponseWithPerItemErrors(t *testing.T) {
	g := newFakeGate(t)
	g.SetWriteStyle(fakeNewStyle)
	g.SetLight(tCLU, "DOU0001", false)
	g.SetLight(tCLU, "DOU0002", false)
	g.FailItem(tCLU, "DOU0002", "boom")
	arrayLane := func(c *laneConfig) { c.asArray, c.maxItems = true, 5 }
	l := newTestLane(t, g, writeLane, arrayLane)

	h := g.HoldNext(fakeSetPath)
	blocker := l.Submit(lightCmd("DOU0001", false))[0]
	h.Wait(t)
	res := l.Submit(lightCmd("DOU0001", true), lightCmd("DOU0002", true), lightCmd("DOU0777", true))
	h.Release()
	waitResult(t, blocker)

	want := []Outcome{OutcomeOK, OutcomeError, OutcomeError}
	for i, ch := range res {
		r := waitResult(t, ch)
		if r.Outcome != want[i] {
			t.Errorf("item %d: %v (%v), want %v", i, r.Outcome, r.Err, want[i])
		}
	}
	reqs := g.Requests(fakeSetPath)
	if len(reqs) != 2 || !reqs[1].IsArray || len(reqs[1].Items) != 3 {
		t.Fatalf("want the three commands in one array request, got %d requests", len(reqs))
	}
}

func TestGateClient_WriteArrayResponseMissingItemIsUncertain(t *testing.T) {
	g := newFakeGate(t)
	g.SetLight(tCLU, tLID, false)
	g.RespondNext(fakeSetPath, `[{"Clu":"CLU_012abcde","Id":"DOU0099","Kind":"Light"}]`)
	if r := writeOnce(t, g, lightCmd(tLID, true), nil); r.Outcome != OutcomeUncertain {
		t.Errorf("outcome %v, want uncertain (no acknowledgement for the command)", r.Outcome)
	}
}

func TestGateClient_WritePerItemErrorOnObject(t *testing.T) {
	g := newFakeGate(t)
	g.SetWriteStyle(fakeNewStyle)
	g.SetLight(tCLU, tLID, false)
	g.FailItem(tCLU, tLID, "no such object")
	r := writeOnce(t, g, lightCmd(tLID, true), nil)
	if r.Outcome != OutcomeError || r.Err == nil {
		t.Errorf("outcome %v (%v), want per-item error", r.Outcome, r.Err)
	}
}

func TestGateClient_WriteWholeRequestErrorObject(t *testing.T) {
	g := newFakeGate(t)
	g.SetLight(tCLU, tLID, false)
	g.RespondNext(fakeSetPath, `{"Error":"empty body"}`)
	r := writeOnce(t, g, lightCmd(tLID, true), nil)
	if r.Outcome != OutcomeError || r.Err == nil {
		t.Errorf("outcome %v (%v), want error for a request rejected as a whole", r.Outcome, r.Err)
	}
}

func TestGateClient_WriteOldStyleEmptyObjectIsUncertain(t *testing.T) {
	g := newFakeGate(t)
	g.SetLight(tCLU, tLID, false)
	g.FailItem(tCLU, tLID, "x") // old style answers "{}"
	if r := writeOnce(t, g, lightCmd(tLID, true), nil); r.Outcome != OutcomeUncertain {
		t.Errorf("outcome %v, want uncertain", r.Outcome)
	}
}

func TestGateClient_WriteFailuresAreUncertain(t *testing.T) {
	cases := map[string]func(g *fakeGate){
		"timeout":  func(g *fakeGate) { g.HangAll(fakeSetPath) },
		"non-2xx":  func(g *fakeGate) { g.FailNext(fakeSetPath, 500, 1) },
		"redirect": func(g *fakeGate) { g.FailNext(fakeSetPath, 304, 1) },
		"broken":   func(g *fakeGate) { g.BreakBodyNext(fakeSetPath, 1) },
		"not json": func(g *fakeGate) { g.RespondNext(fakeSetPath, `OK`) },
		"empty":    func(g *fakeGate) { g.RespondNext(fakeSetPath, ``) },
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			g := newFakeGate(t)
			g.SetLight(tCLU, tLID, false)
			setup(g)
			r := writeOnce(t, g, lightCmd(tLID, true), func(c *laneConfig) { c.timeout = 100 * time.Millisecond })
			if r.Outcome != OutcomeUncertain || r.Err == nil {
				t.Errorf("outcome %v (%v), want uncertain with an error", r.Outcome, r.Err)
			}
			if r.Stamp == 0 {
				t.Error("a given-up request still takes a completion stamp")
			}
		})
	}
}

func TestGateClient_ReadFailuresAndMissingItems(t *testing.T) {
	cases := map[string]func(g *fakeGate){
		"timeout":     func(g *fakeGate) { g.HangAll(fakeReadPath) },
		"non-2xx":     func(g *fakeGate) { g.FailNext(fakeReadPath, 503, 1) },
		"broken":      func(g *fakeGate) { g.BreakBodyNext(fakeReadPath, 1) },
		"object body": func(g *fakeGate) { g.RespondNext(fakeReadPath, `{"Clu":"CLU_012abcde"}`) },
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			g := newFakeGate(t)
			g.SetLight(tCLU, tLID, false)
			setup(g)
			l := newTestLane(t, g, readLane, func(c *laneConfig) { c.timeout = 100 * time.Millisecond })
			if r := waitResult(t, l.Submit(lightRead(tLID))[0]); r.Outcome != OutcomeFailed {
				t.Errorf("outcome %v, want failed", r.Outcome)
			}
		})
	}
	t.Run("missing", func(t *testing.T) {
		g := newFakeGate(t)
		g.SetLight(tCLU, "DOU0001", true)
		g.SetLight(tCLU, "DOU0002", true)
		g.Omit(tCLU, "DOU0002")
		l := newTestLane(t, g, readLane, nil)
		res := l.Submit(lightRead("DOU0001"), lightRead("DOU0002"))
		if r := waitResult(t, res[0]); r.Outcome != OutcomeOK || r.Item == nil {
			t.Errorf("present device: %+v", r)
		}
		if r := waitResult(t, res[1]); r.Outcome != OutcomeMissing {
			t.Errorf("omitted device: %v, want missing", r.Outcome)
		}
	})
}
