package main

import (
	"context"
	"fmt"
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
