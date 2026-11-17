package main

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/brutella/hap/characteristic"
)

// withNewProtocol selects the new write protocol (the production default) with the given
// number of commands per request.
func withNewProtocol(queueSize int) func(*GrentonSet) {
	return func(gs *GrentonSet) {
		gs.LegacyWriteScript = false
		gs.SetterQueueSize = queueSize
	}
}

// payloadKeys returns the sorted keys of the kind's payload in a request item.
func payloadKeys(item map[string]any, kind string) []string {
	p, _ := item[kind].(map[string]any)
	var keys []string
	for k := range p {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// num returns a fake GATE payload number (int when set by the test, float64 when written
// through JSON) as a float64.
func num(v any) float64 {
	switch n := v.(type) {
	case int:
		return float64(n)
	case float64:
		return n
	}
	return -1
}

// hkSet is hkWrite once the HomeKit syncs queued so far are done: hap's characteristics
// are not synchronised, so a test must not write one while the syncer sets it.
func hkSet(t *testing.T, gs *GrentonSet, c *characteristic.C, v any) int {
	t.Helper()
	syncHomeKit(t, gs)
	return hkWrite(c, v)
}

// queued returns the number of entries waiting in a lane's mailbox.
func queued(l *lane) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.queue)
}

// newProtocolSet returns a ready set in the new protocol with a light and a thermostat
// (and a shutter and a motion sensor), answered in the new script's style.
func newProtocolSet(t *testing.T, queueSize int) (*GrentonSet, *fakeGate, *Clu) {
	t.Helper()
	clu := allKindsClu()
	gs, g := newTestSetWith(t, withNewProtocol(queueSize), clu)
	g.SetWriteStyle(fakeNewStyle)
	g.SetThermo(tCLU, "THE0002", 20, 21, 1, 1)
	gs.Refresh()
	settle(t, gs)
	return gs, g, clu
}

// A temperature change sends TempSetpoint only; heat and auto send State and Mode only;
// off sends State only (it keeps the mode). Each command leaves as a one-item array.
func TestProtocol_ThermostatPartialCommands(t *testing.T) {
	gs, g, clu := newProtocolSet(t, 5)
	th := clu.Therms[0].hk.Thermostat

	cases := []struct {
		name     string
		write    func() int
		wantKeys []string
		wantGate map[string]float64
	}{
		{"temperature", func() int { return hkWrite(th.TargetTemperature.C, 23.5) },
			[]string{"TempSetpoint"}, map[string]float64{"TempSetpoint": 23.5, "State": 1, "Mode": 1}},
		{"heat", func() int {
			return hkWrite(th.TargetHeatingCoolingState.C, characteristic.TargetHeatingCoolingStateHeat)
		},
			[]string{"Mode", "State"}, map[string]float64{"TempSetpoint": 23.5, "State": 1, "Mode": 0}},
		{"off", func() int {
			return hkWrite(th.TargetHeatingCoolingState.C, characteristic.TargetHeatingCoolingStateOff)
		},
			[]string{"State"}, map[string]float64{"TempSetpoint": 23.5, "State": 0, "Mode": 0}},
		{"auto", func() int {
			return hkWrite(th.TargetHeatingCoolingState.C, characteristic.TargetHeatingCoolingStateAuto)
		},
			[]string{"Mode", "State"}, map[string]float64{"TempSetpoint": 23.5, "State": 1, "Mode": 1}},
	}
	for _, c := range cases {
		before := g.RequestCount(fakeSetPath)
		if st := c.write(); st != 0 {
			t.Fatalf("%s: status %d", c.name, st)
		}
		settle(t, gs)
		reqs := g.Requests(fakeSetPath)[before:]
		if len(reqs) != 1 || !reqs[0].IsArray || len(reqs[0].Items) != 1 {
			t.Fatalf("%s: requests %v, want one one-item array", c.name, reqs)
		}
		if keys := payloadKeys(reqs[0].Items[0], "Thermo"); !slices.Equal(keys, c.wantKeys) {
			t.Errorf("%s: command fields %v, want %v (%s)", c.name, keys, c.wantKeys, reqs[0].Body)
		}
		p := g.Payload(tCLU, "THE0002")
		for f, want := range c.wantGate {
			if num(p[f]) != want {
				t.Errorf("%s: GATE %s = %v, want %v", c.name, f, p[f], want)
			}
		}
	}
}

// A mode change and a temperature change pending together leave as one item carrying the
// three fields, and nothing else.
func TestProtocol_PartialThermostatCommandsMerge(t *testing.T) {
	gs, g, clu := newProtocolSet(t, 5)
	th := clu.Therms[0].hk.Thermostat

	h := g.HoldNext(fakeSetPath)
	hkWrite(findC(t, clu.Lights[0].HkAccessory(), characteristic.TypeOn), true)
	h.Wait(t)
	hkSet(t, gs, th.TargetTemperature.C, 19.0)
	hkSet(t, gs, th.TargetHeatingCoolingState.C, characteristic.TargetHeatingCoolingStateHeat)
	h.Release()
	settle(t, gs)

	reqs := g.Requests(fakeSetPath)
	if len(reqs) != 2 || len(reqs[1].Items) != 1 {
		t.Fatalf("set requests %d, want 2 (light, one thermostat command)", len(reqs))
	}
	item := reqs[1].Items[0]
	if keys := payloadKeys(item, "Thermo"); !slices.Equal(keys, []string{"Mode", "State", "TempSetpoint"}) {
		t.Errorf("merged command fields %v: %s", keys, reqs[1].Body)
	}
	p := item["Thermo"].(map[string]any)
	if p["TempSetpoint"] != 19.0 || p["State"] != 1.0 || p["Mode"] != 0.0 {
		t.Errorf("merged command %s", reqs[1].Body)
	}
}

// With SetterQueueSize 1 in the new protocol each request is a one-item array, and
// thermostat commands stay partial.
func TestProtocol_QueueSizeOneKeepsPartialCommands(t *testing.T) {
	gs, g, clu := newProtocolSet(t, 1)
	th := clu.Therms[0].hk.Thermostat

	h := g.HoldNext(fakeSetPath)
	hkWrite(findC(t, clu.Lights[0].HkAccessory(), characteristic.TypeOn), true)
	h.Wait(t)
	hkSet(t, gs, th.TargetTemperature.C, 22.5)
	hkSet(t, gs, clu.Shutters[0].hk.WindowCovering.TargetPosition.C, 0)
	h.Release()
	settle(t, gs)

	reqs := g.Requests(fakeSetPath)
	if len(reqs) < 3 {
		t.Fatalf("set requests %d, want at least 3 (light, thermostat, shutter move)", len(reqs))
	}
	for i, r := range reqs {
		if !r.IsArray || len(r.Items) != 1 {
			t.Errorf("request %d: %s, want a one-item array", i, r.Body)
		}
	}
	if reqs[1].Items[0]["Kind"] != "Thermo" {
		t.Fatalf("request 1 = %s, want the thermostat command", reqs[1].Body)
	}
	if keys := payloadKeys(reqs[1].Items[0], "Thermo"); !slices.Equal(keys, []string{"TempSetpoint"}) {
		t.Errorf("thermostat command fields %v, want TempSetpoint only", keys)
	}
}

// A thermostat that was never read accepts a partial command in the new protocol (only the
// changed field is sent, so nothing else on the CLU changes) and still refuses in the
// legacy protocol, sending nothing.
func TestProtocol_UnreadThermostat(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%v", legacy), func(t *testing.T) {
			clu := allKindsClu()
			gs, g := newTestSetWith(t, func(gs *GrentonSet) { gs.LegacyWriteScript = legacy }, clu)
			g.SetWriteStyle(fakeNewStyle)
			g.SetThermo(tCLU, "THE0002", 20, 21, 1, 1) // heating, auto: an unread device believes 0, 0
			th := clu.Therms[0].hk.Thermostat

			// Reads hang until both writes were made, so the thermostat is still unread
			// for the second one (a refused write queues a read).
			g.HangAll(fakeReadPath)
			tempStatus := hkSet(t, gs, th.TargetTemperature.C, 24.0)
			modeStatus := hkSet(t, gs, th.TargetHeatingCoolingState.C, characteristic.TargetHeatingCoolingStateHeat)
			g.ReleaseAll()
			settle(t, gs)
			reqs := g.Requests(fakeSetPath)

			if legacy {
				if tempStatus != hkNoResponse || modeStatus != hkNoResponse {
					t.Errorf("statuses %d, %d; want both refused with %d", tempStatus, modeStatus, hkNoResponse)
				}
				if len(reqs) != 0 {
					t.Errorf("refused writes sent %d requests", len(reqs))
				}
				return
			}
			if tempStatus != 0 || modeStatus != 0 {
				t.Fatalf("statuses %d, %d; want both accepted", tempStatus, modeStatus)
			}
			var fields [][]string
			for _, r := range reqs {
				for _, it := range r.Items {
					fields = append(fields, payloadKeys(it, "Thermo"))
				}
			}
			want := [][]string{{"TempSetpoint"}, {"Mode", "State"}}
			if len(fields) == 1 { // both pending together: merged
				want = [][]string{{"Mode", "State", "TempSetpoint"}}
			}
			if fmt.Sprint(fields) != fmt.Sprint(want) {
				t.Errorf("commands sent %v, want %v", fields, want)
			}
			p := g.Payload(tCLU, "THE0002")
			if p["TempSetpoint"] != 24.0 || p["State"] != 1.0 || p["Mode"] != 0.0 {
				t.Errorf("GATE thermostat %v, want setpoint 24, state 1, mode 0", p)
			}
			if !clu.Therms[0].isReady() {
				t.Error("the confirm read did not make the thermostat ready")
			}
		})
	}
}

// LegacyWriteScript sends single objects, one command per request, with three-field
// thermostat commands, whatever SetterQueueSize says.
func TestProtocol_LegacyIgnoresQueueSize(t *testing.T) {
	for _, size := range []int{0, 1, 5, 10} {
		t.Run(fmt.Sprintf("SetterQueueSize=%d", size), func(t *testing.T) {
			clu := lightsClu(4)
			clu.Therms = []*Thermo{{CluObject: CluObject{Id: 9, Kind: "THE", Name: "thermo"}, Source: "sensor"}}
			gs, g := newTestSetWith(t, func(gs *GrentonSet) {
				gs.LegacyWriteScript = true
				gs.SetterQueueSize = size
			}, clu)
			gs.Refresh()
			settle(t, gs)

			h := g.HoldNext(fakeSetPath)
			hkWrite(findC(t, clu.Lights[0].HkAccessory(), characteristic.TypeOn), true)
			h.Wait(t)
			for _, l := range clu.Lights[1:] {
				hkWrite(findC(t, l.HkAccessory(), characteristic.TypeOn), true)
			}
			hkWrite(clu.Therms[0].hk.Thermostat.TargetTemperature.C, 22.5)
			h.Release()
			settle(t, gs)

			reqs := g.Requests(fakeSetPath)
			if len(reqs) != 5 {
				t.Fatalf("set requests = %d, want 5 (one per command)", len(reqs))
			}
			for i, r := range reqs {
				if r.IsArray || len(r.Items) != 1 {
					t.Errorf("request %d: %s, want a single object", i, r.Body)
				}
			}
			if keys := payloadKeys(reqs[4].Items[0], "Thermo"); !slices.Equal(keys, []string{"Mode", "State", "TempSetpoint"}) {
				t.Errorf("thermostat command fields %v, want all three: %s", keys, reqs[4].Body)
			}
		})
	}
}

// A per-item Error in an array response fails only that command: the others are
// acknowledged, and the failed device returns to the GATE's state after its confirm read.
func TestProtocol_PerItemErrorFailsOnlyThatCommand(t *testing.T) {
	logs := captureLog(t)
	clu := lightsClu(4)
	gs, g := newTestSetWith(t, withNewProtocol(5), clu)
	g.SetWriteStyle(fakeNewStyle)
	gs.Refresh()
	settle(t, gs)
	g.FailItem(tCLU, "DOU0003", "boom")

	h := g.HoldNext(fakeSetPath)
	hkWrite(findC(t, clu.Lights[0].HkAccessory(), characteristic.TypeOn), true)
	h.Wait(t)
	for _, l := range clu.Lights[1:] {
		hkWrite(findC(t, l.HkAccessory(), characteristic.TypeOn), true)
	}
	h.Release()
	settle(t, gs)

	reqs := g.Requests(fakeSetPath)
	if len(reqs) != 2 || !slices.Equal(itemIds(reqs[1]), []string{"DOU0002", "DOU0003", "DOU0004"}) {
		t.Fatalf("set requests %d (%v), want the blocker and one array of DOU0002..4", len(reqs), reqs)
	}
	for i, l := range clu.Lights {
		want := i != 2
		if got := g.Payload(tCLU, lightId(i+1))["State"]; got != want {
			t.Errorf("%s: GATE state %v, want %v", lightId(i+1), got, want)
		}
		if v := findC(t, l.HkAccessory(), characteristic.TypeOn).Val; v != want {
			t.Errorf("%s: HomeKit shows %v, want %v", lightId(i+1), v, want)
		}
	}
	out := logs.String()
	if n := strings.Count(out, "acknowledged"); n != 3 {
		t.Errorf("%d acknowledgements logged, want 3", n)
	}
	if !strings.Contains(out, "DOU0003") || !strings.Contains(out, "boom") {
		t.Errorf("per-item error not logged:\n%s", out)
	}
	for _, l := range errorLines(out) {
		for _, id := range []string{"DOU0001", "DOU0002", "DOU0004"} {
			if strings.Contains(l, id) {
				t.Errorf("error logged for %s: %s", id, l)
			}
		}
	}
}

// The shutter in the new protocol: its commands share array requests with other commands;
// a per-item error on the move fails only the move (retried once, then given up) while the
// light in the same request is acknowledged; a move that runs ends with its automatic STOP.
func TestProtocol_ShutterInArrayRequests(t *testing.T) {
	checkGoroutines(t)
	logs := captureLog(t)
	clu := shutterClu()
	clu.Lights = []*Light{{CluObject: CluObject{Id: 1, Kind: "DOU", Name: "lamp"}}}
	gs, g := newTestSetWith(t, withNewProtocol(5), clu)
	g.SetWriteStyle(fakeNewStyle)
	gs.Refresh()
	settle(t, gs)
	sh, on := clu.Shutters[0], findC(t, clu.Lights[0].HkAccessory(), characteristic.TypeOn)

	g.FailItem(tCLU, tSID, "boom")
	h := g.HoldNext(fakeSetPath)
	hkWrite(on, true)
	h.Wait(t)
	hkTarget(t, sh, 40)
	hkSet(t, gs, on, false)
	eventually(t, 2*time.Second, func() bool { return queued(gs.writer) == 2 }, "the move to be submitted")
	h.Release()
	settle(t, gs)

	reqs := g.Requests(fakeSetPath)
	if len(reqs) < 2 {
		t.Fatalf("%d set requests", len(reqs))
	}
	// The simulation goroutine submits the move, so it may enter the mailbox after the light.
	if ids := itemIds(reqs[1]); !slices.Contains(ids, tSID) || !slices.Contains(ids, "DOU0001") || len(ids) != 2 {
		t.Fatalf("second request %s, want the move and the light together", reqs[1].Body)
	}
	assertCmds(t, g, "MOVEDOWN", "MOVEDOWN")
	assertShown(t, sh, shown{100, 100, hkStopped})
	if g.Payload(tCLU, "DOU0001")["State"] != false || on.Val != false {
		t.Error("the light command in the same request was not applied")
	}
	if !strings.Contains(logs.String(), "giving up on target 40") {
		t.Errorf("give-up not logged:\n%s", logs.String())
	}

	g.ClearFailItem(tCLU, tSID)
	hkTarget(t, sh, 40)
	settle(t, gs)
	assertShown(t, sh, shown{100, 40, hkDecreasing})
	tick(t, gs, 60)
	settle(t, gs)
	assertShown(t, sh, shown{40, 40, hkStopped})
	assertCmds(t, g, "MOVEDOWN", "MOVEDOWN", "MOVEDOWN", "STOP")
	for _, r := range g.Requests(fakeSetPath) {
		if !r.IsArray {
			t.Errorf("request %s is not an array", r.Body)
		}
	}
}

// A move whose item failed after the GATE called the CLU (Error with Uncertain) may have
// run: it is reconciled by the confirm read instead of being retried. Here the read shows
// the motor moving down, so the clock runs to the target and the automatic STOP follows.
func TestProtocol_ShutterItemErrorAfterCluCallIsReconciled(t *testing.T) {
	checkGoroutines(t)
	clu := shutterClu()
	gs, g := newTestSetWith(t, withNewProtocol(5), clu)
	g.SetWriteStyle(fakeNewStyle)
	gs.Refresh()
	settle(t, gs)
	sh := clu.Shutters[0]

	g.FailItemUncertain(tCLU, tSID, "CLU did not answer") // the move runs, then the item fails
	hkTarget(t, sh, 40)
	settle(t, gs)
	g.ClearFailItem(tCLU, tSID)
	assertCmds(t, g, "MOVEDOWN")
	assertShown(t, sh, shown{100, 40, hkDecreasing})
	tick(t, gs, 60)
	settle(t, gs)
	assertShown(t, sh, shown{40, 40, hkStopped})
	assertCmds(t, g, "MOVEDOWN", "STOP")
}

// The new protocol against the old update script: the old script answers an array with
// "{}" and runs nothing. Every command is uncertain, HomeKit returns to the real state after
// the confirm read, and the log says what is wrong, once per minute at most.
func TestProtocol_NewProtocolAgainstOldScript(t *testing.T) {
	logs := captureLog(t)
	clu := lightsClu(2)
	gs, g := newTestSetWith(t, withNewProtocol(5), clu) // fake answers in the old script's style
	gs.Refresh()
	settle(t, gs)

	for _, l := range clu.Lights {
		hkSet(t, gs, findC(t, l.HkAccessory(), characteristic.TypeOn), true)
		settle(t, gs)
	}
	if n := g.RequestCount(fakeSetPath); n != 2 {
		t.Fatalf("%d set requests, want 2", n)
	}
	for i, l := range clu.Lights {
		if g.Payload(tCLU, lightId(i+1))["State"] != false {
			t.Errorf("%s: the old script ran an array request", lightId(i+1))
		}
		if v := findC(t, l.HkAccessory(), characteristic.TypeOn).Val; v != false {
			t.Errorf("%s: HomeKit shows %v after the confirm read, want false", lightId(i+1), v)
		}
	}
	out := logs.String()
	if n := strings.Count(out, "old update-script.lua"); n != 1 {
		t.Errorf("%d protocol hints logged, want 1:\n%s", n, out)
	}
	if !strings.Contains(out, "LegacyWriteScript") || !strings.Contains(out, "grenton/update-script.lua") {
		t.Errorf("the hint does not name both remedies:\n%s", out)
	}
}
