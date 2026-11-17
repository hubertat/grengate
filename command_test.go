package main

import (
	"sync"
	"testing"
	"time"

	"github.com/brutella/hap/characteristic"
)

func thermoCmd(setpoint *float64, state, mode *int, seq uint64) ReqObject {
	return ReqObject{Clu: tCLU, Id: "THE0002", Kind: "Thermo", Cmd: "SET",
		Thermo: &ThermoPayload{TempSetpoint: setpoint, State: state, Mode: mode}, seq: seq}
}

func TestMergeCommand_ThermostatFieldWise(t *testing.T) {
	setpoint := thermoCmd(ptr(23.0), nil, nil, 1)
	mode := thermoCmd(nil, ptr(1), ptr(1), 2)

	merged, supersede := mergeCommand(setpoint, mode)
	if supersede {
		t.Error("a merged thermostat command must not supersede the pending one")
	}
	th := merged.Thermo
	if th.TempSetpoint == nil || *th.TempSetpoint != 23 || th.State == nil || *th.State != 1 || th.Mode == nil || *th.Mode != 1 {
		t.Errorf("merged = %+v, want setpoint 23, state 1, mode 1", th)
	}
	if merged.seq != 2 {
		t.Errorf("merged seq = %d, want the newer command's 2", merged.seq)
	}
	if setpoint.Thermo.State != nil {
		t.Error("the pending command was modified in place")
	}

	// The newer command's fields win.
	merged, _ = mergeCommand(merged, thermoCmd(ptr(19.5), nil, nil, 3))
	if *merged.Thermo.TempSetpoint != 19.5 || *merged.Thermo.State != 1 {
		t.Errorf("second merge = %+v", merged.Thermo)
	}

	// Lights and shutters: plain replace.
	if out, supersede := mergeCommand(lightCmd("DOU0001", true), lightCmd("DOU0001", false)); !supersede || *out.Light.State {
		t.Errorf("light merge = %+v, supersede %v; want the newer command replacing", out.Light, supersede)
	}
}

// Both submissions of a merged thermostat command share its outcome.
func TestMergeCommand_ThermostatSubmittersShareOutcome(t *testing.T) {
	g := newFakeGate(t)
	g.SetThermo(tCLU, "THE0002", 20, 21, 0, 0)
	g.SetLight(tCLU, "DOU0001", false)
	l := newTestLane(t, g, writeLane, func(c *laneConfig) { c.merge = mergeCommand })

	h := g.HoldNext(fakeSetPath)
	blocker := l.Submit(lightCmd("DOU0001", true))[0]
	h.Wait(t)
	first := l.Submit(thermoCmd(ptr(23.0), nil, nil, 1))[0]
	second := l.Submit(thermoCmd(nil, ptr(1), ptr(1), 2))[0]
	h.Release()
	waitResult(t, blocker)
	r1, r2 := waitResult(t, first), waitResult(t, second)
	if r1.Outcome != OutcomeOK || r2.Outcome != OutcomeOK || r1.Stamp != r2.Stamp {
		t.Errorf("results %v/%d and %v/%d, want both acknowledged with one stamp", r1.Outcome, r1.Stamp, r2.Outcome, r2.Stamp)
	}
	reqs := g.Requests(fakeSetPath)
	if len(reqs) != 2 {
		t.Fatalf("set requests = %d, want 2 (blocker, one merged thermostat command)", len(reqs))
	}
	th, _ := reqs[1].Items[0]["Thermo"].(map[string]any)
	if th["TempSetpoint"] != 23.0 || th["State"] != 1.0 || th["Mode"] != 1.0 {
		t.Errorf("merged command = %s", reqs[1].Body)
	}
}

// A mode change and a setpoint change from HomeKit, pending together, arrive at the GATE
// as one command with both.
func TestThermostat_ModeAndSetpointPendingTogetherAreOneCommand(t *testing.T) {
	clu := allKindsClu()
	gs, g := newTestSet(t, clu)
	gs.Refresh()
	settle(t, gs)
	thermo := clu.Therms[0].hk.Thermostat

	var wg sync.WaitGroup
	h := g.HoldNext(fakeSetPath)
	hkWriteAsync(&wg, findC(t, clu.Lights[0].HkAccessory(), characteristic.TypeOn), true)
	h.Wait(t)
	hkWriteAsync(&wg, thermo.TargetHeatingCoolingState.C, characteristic.TargetHeatingCoolingStateAuto)
	eventually(t, 2*time.Second, func() bool { return queuedIn(gs.writer, "THE0002") }, "mode change queued")
	wg.Wait()          // the callbacks returned: their HomeKit syncs are marked
	syncHomeKit(t, gs) // hap characteristics are not synchronised: no HomeKit write during a sync
	hkWriteAsync(&wg, thermo.TargetTemperature.C, 23.0)
	eventually(t, 2*time.Second, func() bool {
		gs.writer.mu.Lock()
		defer gs.writer.mu.Unlock()
		e := gs.writer.index[entryKey(tCLU, "THE0002")]
		return e != nil && e.obj.Thermo.TempSetpoint != nil && *e.obj.Thermo.TempSetpoint == 23
	}, "setpoint change merged")
	h.Release()
	wg.Wait()
	settle(t, gs)

	var thermoReqs []fakeGateRequest
	for _, r := range g.Requests(fakeSetPath) {
		if r.Items[0]["Kind"] == "Thermo" {
			thermoReqs = append(thermoReqs, r)
		}
	}
	if len(thermoReqs) != 1 {
		t.Fatalf("thermostat commands = %d, want 1", len(thermoReqs))
	}
	th, _ := thermoReqs[0].Items[0]["Thermo"].(map[string]any)
	if th["TempSetpoint"] != 23.0 || th["State"] != 1.0 || th["Mode"] != 1.0 {
		t.Errorf("command = %s, want setpoint 23, state 1, mode 1", thermoReqs[0].Body)
	}
}

// A poll result arriving between a HomeKit write and the dispatch of its command does not
// change what the command carries: the command is a snapshot taken at the write.
func TestCommand_SnapshotUnaffectedByPollBeforeDispatch(t *testing.T) {
	clu := allKindsClu()
	clu.Lights = append(clu.Lights, &Light{CluObject: CluObject{Id: 5, Kind: "DOU", Name: "blocker"}})
	gs, g := newTestSet(t, clu)
	gs.Refresh()
	settle(t, gs)
	light, thermo := clu.Lights[0], clu.Therms[0]

	var wg sync.WaitGroup
	h := g.HoldNext(fakeSetPath)
	hkWriteAsync(&wg, findC(t, clu.Lights[1].HkAccessory(), characteristic.TypeOn), true)
	h.Wait(t)
	hkWriteAsync(&wg, findC(t, light.HkAccessory(), characteristic.TypeOn), true)
	hkWriteAsync(&wg, thermo.hk.Thermostat.TargetTemperature.C, 23.0)
	eventually(t, 2*time.Second, func() bool {
		return queuedIn(gs.writer, "DOU0001") && queuedIn(gs.writer, "THE0002")
	}, "both commands queued")

	// Grenton changes on its own: a wall panel switches the thermostat to auto, the light
	// is switched off. A poll returns that before the commands leave.
	g.SetThermo(tCLU, "THE0002", 20, 21, 1, 1)
	g.SetLight(tCLU, "DOU0001", false)
	gs.Refresh()

	h.Release()
	wg.Wait()
	settle(t, gs)
	reqs := g.Requests(fakeSetPath)
	if len(reqs) != 3 {
		t.Fatalf("set requests = %d, want 3", len(reqs))
	}
	for _, r := range reqs[1:] {
		item := r.Items[0]
		switch item["Kind"] {
		case "Light":
			if l, _ := item["Light"].(map[string]any); l["State"] != true {
				t.Errorf("light command = %s, want State true", r.Body)
			}
		case "Thermo":
			th, _ := item["Thermo"].(map[string]any)
			if th["TempSetpoint"] != 23.0 || th["State"] != 0.0 || th["Mode"] != 0.0 {
				t.Errorf("thermostat command = %s, want setpoint 23 with state and mode as at the write (0, 0)", r.Body)
			}
		}
	}
}
