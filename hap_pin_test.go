package main

import (
	"net/http"
	"testing"

	"github.com/brutella/hap/characteristic"
)

// Pins hap's same-value behaviour (concept 3.5, hap characteristic/c.go setValue): a
// controller write equal to the cached value is answered with success before any
// grengate code runs. If a hap upgrade changes this, this test fails and the readiness
// handling of concept 3.5 should be revisited.
func TestHap_SameValueWriteCallsNoHook(t *testing.T) {
	on := characteristic.NewOn()
	var requestFuncs, updateFuncs int
	on.SetValueRequestFunc = func(any, *http.Request) (any, int) {
		requestFuncs++
		return nil, 0
	}
	on.OnValueRemoteUpdate(func(bool) { updateFuncs++ })

	if status := hkWrite(on.C, false); status != 0 {
		t.Errorf("same-value write status = %d, want 0", status)
	}
	if requestFuncs != 0 || updateFuncs != 0 {
		t.Errorf("same-value write called SetValueRequestFunc %d times and update funcs %d times, want none",
			requestFuncs, updateFuncs)
	}

	if status := hkWrite(on.C, true); status != 0 {
		t.Errorf("different-value write status = %d, want 0", status)
	}
	if requestFuncs != 1 || updateFuncs != 1 {
		t.Errorf("different-value write called SetValueRequestFunc %d times and update funcs %d times, want 1 each",
			requestFuncs, updateFuncs)
	}

	// A refusal from SetValueRequestFunc keeps the cached value and skips the update funcs.
	on.SetValueRequestFunc = func(any, *http.Request) (any, int) { return nil, hkNoResponse }
	if status := hkWrite(on.C, false); status != hkNoResponse {
		t.Errorf("refused write status = %d, want %d", status, hkNoResponse)
	}
	if on.Value() != true || updateFuncs != 1 {
		t.Errorf("refused write: value %v, update funcs %d; want true, 1", on.Value(), updateFuncs)
	}
}

// The same limit seen through a device: a write equal to what HomeKit shows never reaches
// the GATE, even when the device is not ready and refuses other writes.
func TestHap_SameValueWriteNeverReachesGrengate(t *testing.T) {
	clu := allKindsClu()
	gs, g := newTestSet(t, clu)
	on := findC(t, clu.Lights[0].HkAccessory(), characteristic.TypeOn)
	target := clu.Therms[0].hk.Thermostat.TargetHeatingCoolingState.C

	if status := hkWrite(on, false); status != 0 {
		t.Errorf("light same-value status = %d, want 0", status)
	}
	// Not ready: a different value would be refused, the cached value is reported as success.
	if status := hkWrite(target, characteristic.TargetHeatingCoolingStateOff); status != 0 {
		t.Errorf("unready thermostat same-value status = %d, want 0 (the accepted false success)", status)
	}
	settle(t, gs)
	if n := g.RequestCount(""); n != 0 {
		t.Errorf("fake GATE received %d requests, want 0", n)
	}
}
