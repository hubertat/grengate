package main

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/brutella/hap/characteristic"
)

// HomeKit's target heating/cooling state follows Grenton's state and mode.
func TestThermostat_TargetStateFollowsGrentonStateAndMode(t *testing.T) {
	cases := []struct {
		state, mode         int
		wantTarget, wantCur int
	}{
		{0, 0, characteristic.TargetHeatingCoolingStateOff, characteristic.CurrentHeatingCoolingStateOff},
		{0, 1, characteristic.TargetHeatingCoolingStateOff, characteristic.CurrentHeatingCoolingStateOff},
		{1, 0, characteristic.TargetHeatingCoolingStateHeat, characteristic.CurrentHeatingCoolingStateHeat},
		{1, 1, characteristic.TargetHeatingCoolingStateAuto, characteristic.CurrentHeatingCoolingStateHeat},
	}
	clu := allKindsClu()
	gs, g := newTestSet(t, clu)
	th := clu.Therms[0].hk.Thermostat
	for _, c := range cases {
		t.Run(fmt.Sprintf("state %d mode %d", c.state, c.mode), func(t *testing.T) {
			g.SetThermo(tCLU, "THE0002", 20, 21, c.mode, c.state)
			gs.Refresh()
			settle(t, gs)
			if v := th.TargetHeatingCoolingState.Value(); v != c.wantTarget {
				t.Errorf("target state = %d, want %d", v, c.wantTarget)
			}
			if v := th.CurrentHeatingCoolingState.Value(); v != c.wantCur {
				t.Errorf("current state = %d, want %d", v, c.wantCur)
			}
		})
	}
}

// After a restart HomeKit shows the thermostat's real mode: the startup refresh syncs it
// before the HomeKit server starts.
func TestThermostat_RealModeAfterStartup(t *testing.T) {
	clu := allKindsClu()
	gs, g := newTestSet(t, clu)
	g.SetThermo(tCLU, "THE0002", 20, 22, 1, 1)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	gs.startup(ctx, false)
	th := clu.Therms[0].hk.Thermostat
	if v := th.TargetHeatingCoolingState.Value(); v != characteristic.TargetHeatingCoolingStateAuto {
		t.Errorf("target state = %d after startup, want auto (%d)", v, characteristic.TargetHeatingCoolingStateAuto)
	}
	if v := th.TargetTemperature.Value(); v != 22 {
		t.Errorf("target temperature = %v after startup, want 22", v)
	}
}

// A mode change from HomeKit sends the matching Grenton state and mode.
func TestThermostat_ModeChangeCommand(t *testing.T) {
	cases := []struct {
		target, state, mode int
	}{
		{characteristic.TargetHeatingCoolingStateHeat, 1, 0},
		{characteristic.TargetHeatingCoolingStateAuto, 1, 1},
		{characteristic.TargetHeatingCoolingStateOff, 0, 1}, // off keeps the mode
	}
	clu := allKindsClu()
	gs, g := newTestSet(t, clu)
	g.SetThermo(tCLU, "THE0002", 20, 21, 1, 1)
	gs.Refresh()
	settle(t, gs)
	th := clu.Therms[0].hk.Thermostat
	for _, c := range cases {
		hkWrite(th.TargetHeatingCoolingState.C, c.target)
		settle(t, gs)
		p := g.Payload(tCLU, "THE0002")
		if p["State"] != float64(c.state) || p["Mode"] != float64(c.mode) {
			t.Errorf("target %d: GATE state %v mode %v, want %d %d", c.target, p["State"], p["Mode"], c.state, c.mode)
		}
		if v := th.TargetHeatingCoolingState.Value(); v != c.target {
			t.Errorf("target %d: HomeKit shows %d after the confirm read", c.target, v)
		}
	}
}

// A HomeKit write that lands while a sync of the same device is in progress (after its
// state copy, before its SetValue) must not leave hap's cache at the stale copied value:
// otherwise the user's next write of that value is dropped by hap as unchanged.
func TestThermostat_WriteDuringSyncIsNotOverwritten(t *testing.T) {
	clu := allKindsClu()
	gs, g := newTestSet(t, clu)
	gs.Refresh()
	settle(t, gs)
	th := clu.Therms[0].hk.Thermostat

	var once sync.Once
	th.CurrentTemperature.OnCValueUpdate(func(_ *characteristic.C, _, _ any, req *http.Request) {
		if req != nil {
			return
		}
		// Sync has copied the state (target 21) and is pushing the current temperature;
		// a controller writes the target now.
		once.Do(func() {
			if st := hkWrite(th.TargetTemperature.C, 25.0); st != 0 {
				t.Errorf("write status %d", st)
			}
		})
	})
	h := g.HoldNext(fakeSetPath) // hold the command of the 25 write
	g.SetThermo(tCLU, "THE0002", 20.5, 21, 0, 0)
	gs.Refresh() // the current temperature changed: Sync runs
	syncHomeKit(t, gs)
	syncHomeKit(t, gs) // the write marked the thermostat during that sync, maybe after the first barrier
	h.Wait(t)
	if v := th.TargetTemperature.Value(); v != 25.0 {
		t.Errorf("HomeKit target = %v after the sync, want the written 25", v)
	}

	// The user goes back to 21: it must reach the GATE.
	if st := hkWrite(th.TargetTemperature.C, 21.0); st != 0 {
		t.Fatalf("status %d", st)
	}
	h.Release()
	settle(t, gs)
	if got := g.Payload(tCLU, "THE0002")["TempSetpoint"]; got != 21.0 {
		t.Errorf("GATE setpoint = %v, want 21", got)
	}
}
