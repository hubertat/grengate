package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/brutella/hap/characteristic"
)

// hkRead performs a HomeKit read of c as a paired controller would (GET /characteristics).
func hkRead(c *characteristic.C) (any, int) {
	return c.ValueRequest(httptest.NewRequest("GET", "/characteristics", nil))
}

// stateChars returns one state characteristic per device of allKindsClu.
func stateChars(t *testing.T, clu *Clu) map[string]*characteristic.C {
	return map[string]*characteristic.C{
		"light":   findC(t, clu.Lights[0].HkAccessory(), characteristic.TypeOn),
		"thermo":  clu.Therms[0].hk.Thermostat.CurrentTemperature.C,
		"shutter": clu.Shutters[0].hk.WindowCovering.CurrentPosition.C,
		"motion":  findC(t, clu.MotionSensors[0].GetA(), characteristic.TypeMotionDetected),
	}
}

// A thermostat that was never read refuses a temperature change and a mode change with an
// error status; nothing is sent, the refusal is logged and a read of it is queued.
func TestReadiness_UnreadThermostatRefusesWrites(t *testing.T) {
	logs := captureLog(t)
	clu := allKindsClu()
	gs, g := newTestSet(t, clu)
	th := clu.Therms[0].hk.Thermostat

	if status := hkWrite(th.TargetTemperature.C, 25.0); status != hkNoResponse {
		t.Errorf("temperature change status = %d, want %d", status, hkNoResponse)
	}
	if status := hkWrite(th.TargetHeatingCoolingState.C, characteristic.TargetHeatingCoolingStateHeat); status != hkNoResponse {
		t.Errorf("mode change status = %d, want %d", status, hkNoResponse)
	}
	settle(t, gs)
	if n := g.RequestCount(fakeSetPath); n != 0 {
		t.Errorf("set requests = %d, want 0", n)
	}
	if v := th.TargetTemperature.Value(); v == 25 {
		t.Error("HomeKit kept the refused value")
	}
	if n := strings.Count(logs.String(), "refused"); n != 2 {
		t.Errorf("refusals logged %d times, want 2:\n%s", n, logs.String())
	}
	reads := g.Requests(fakeReadPath)
	if len(reads) == 0 || reads[0].Items[0]["Id"] != "THE0002" {
		t.Fatalf("read requests = %v, want a read of the thermostat", reads)
	}

	// The queued read made it ready: the next change goes through.
	if status := hkWrite(th.TargetTemperature.C, 25.0); status != 0 {
		t.Errorf("status after the read = %d, want 0", status)
	}
	settle(t, gs)
	if got := g.Payload(tCLU, "THE0002")["TempSetpoint"]; got != 25.0 {
		t.Errorf("GATE setpoint = %v, want 25", got)
	}
}

// A light needs no baseline: a write to a light that was never read goes through.
func TestReadiness_UnreadLightWriteGoesThrough(t *testing.T) {
	clu := allKindsClu()
	gs, g := newTestSet(t, clu)
	if status := hkWrite(findC(t, clu.Lights[0].HkAccessory(), characteristic.TypeOn), true); status != 0 {
		t.Errorf("status = %d, want 0", status)
	}
	settle(t, gs)
	if g.Payload(tCLU, "DOU0001")["State"] != true {
		t.Error("light command not sent")
	}
}

// Devices that were never read answer HomeKit reads with an error status; once read they
// answer with their values.
func TestReadiness_UnreadDevicesAnswerReadsWithErrorStatus(t *testing.T) {
	clu := allKindsClu()
	gs, g := newTestSet(t, clu)
	g.SetThermo(tCLU, "THE0002", 19.5, 21, 0, 1)
	chars := stateChars(t, clu)
	for name, c := range chars {
		if v, status := hkRead(c); status != hkNoResponse {
			t.Errorf("%s before any read: (%v, %d), want status %d", name, v, status, hkNoResponse)
		}
	}
	// Accessory information is not guarded: HomeKit can still show the device's name.
	info := findC(t, clu.Lights[0].HkAccessory(), characteristic.TypeName)
	if _, status := hkRead(info); status != 0 {
		t.Errorf("name read status = %d, want 0", status)
	}

	gs.Refresh()
	settle(t, gs)
	for name, c := range chars {
		if _, status := hkRead(c); status != 0 {
			t.Errorf("%s after a read: status %d, want 0", name, status)
		}
	}
	if v, _ := hkRead(chars["thermo"]); v != 19.5 {
		t.Errorf("current temperature = %v, want 19.5", v)
	}
}

// Invalid read results are never applied: a device that was not ready stays not ready.
func TestReadiness_InvalidResultsLeaveDeviceNotReady(t *testing.T) {
	cases := []struct {
		name    string
		spoil   func(g *fakeGate)
		invalid string // the device that gets an invalid result
	}{
		{"light without payload", func(g *fakeGate) { g.DropPayload(tCLU, "DOU0001") }, "light"},
		{"thermostat without current temperature", func(g *fakeGate) { g.DropField(tCLU, "THE0002", "TempCurrent") }, "thermo"},
		{"thermostat without mode", func(g *fakeGate) { g.DropField(tCLU, "THE0002", "Mode") }, "thermo"},
		{"shutter with MaxTime 0", func(g *fakeGate) { g.SetShutter(tCLU, "ROL0003", 0, 0) }, "shutter"},
		{"shutter with MaxTime 50", func(g *fakeGate) { g.SetShutter(tCLU, "ROL0003", 50, 0) }, "shutter"},
		{"motion sensor without State", func(g *fakeGate) { g.DropField(tCLU, "PIR0004", "State") }, "motion"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clu := allKindsClu()
			gs, g := newTestSet(t, clu)
			tc.spoil(g)
			c := gs.Refresh()
			if c.Missing != 1 || c.Applied != 3 {
				t.Errorf("refresh = %+v, want 1 invalid and 3 applied", c)
			}
			settle(t, gs)
			for name, ch := range stateChars(t, clu) {
				_, status := hkRead(ch)
				if name == tc.invalid && status != hkNoResponse {
					t.Errorf("%s: status %d after an invalid result, want %d", name, status, hkNoResponse)
				}
				if name != tc.invalid && status != 0 {
					t.Errorf("%s: status %d, want 0", name, status)
				}
			}
		})
	}
}

// An invalid read result for a ready device is not applied either: it keeps its last good
// values and stays ready.
func TestReadiness_InvalidResultKeepsLastGoodValues(t *testing.T) {
	clu := allKindsClu()
	gs, g := newTestSet(t, clu)
	g.SetLight(tCLU, "DOU0001", true)
	g.SetThermo(tCLU, "THE0002", 19.5, 21, 0, 1)
	g.SetShutter(tCLU, "ROL0003", 20000, 0)
	gs.Refresh()
	settle(t, gs)

	// Grenton changes, but every result comes back invalid.
	g.SetLight(tCLU, "DOU0001", false)
	g.DropField(tCLU, "DOU0001", "State")
	g.SetThermo(tCLU, "THE0002", 25, 26, 1, 1)
	g.DropField(tCLU, "THE0002", "TempCurrent")
	g.SetShutter(tCLU, "ROL0003", 0, 1)
	g.DropPayload(tCLU, "PIR0004")
	if c := gs.Refresh(); c.Missing != 4 {
		t.Errorf("refresh = %+v, want 4 invalid", c)
	}
	settle(t, gs)

	chars := stateChars(t, clu)
	for name, want := range map[string]any{"light": true, "thermo": 19.5, "motion": false} {
		if v, status := hkRead(chars[name]); status != 0 || v != want {
			t.Errorf("%s = (%v, %d), want (%v, 0)", name, v, status, want)
		}
	}
	th := clu.Therms[0].hk.Thermostat
	if v := th.TargetTemperature.Value(); v != 21 {
		t.Errorf("target temperature = %v, want 21", v)
	}
	sh := clu.Shutters[0]
	sh.mu.Lock()
	maxTime, state := sh.MaxTime, sh.State
	sh.mu.Unlock()
	if maxTime != 20000 || state != 0 {
		t.Errorf("shutter MaxTime %d, State %d; want the last good 20000, 0", maxTime, state)
	}
	if v, status := hkRead(chars["shutter"]); status != 0 {
		t.Errorf("shutter read = (%v, %d), want status 0", v, status)
	}
}

// The /accessories listing (hap marshals characteristics with ValueRequest(nil)) keeps a
// value for every readable characteristic of a device that is not ready: HAP requires it,
// and a missing one could make iOS reject the whole bridge. Real reads still fail.
func TestReadiness_AccessoryListingKeepsValuesOfUnreadDevice(t *testing.T) {
	clu := allKindsClu()
	newTestSet(t, clu)
	for name, c := range stateChars(t, clu) {
		b, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		if _, ok := m["value"]; !ok {
			t.Errorf("%s: listing has no value: %s", name, b)
		}
		if _, status := hkRead(c); status != hkNoResponse {
			t.Errorf("%s: GET status %d, want %d", name, status, hkNoResponse)
		}
	}
}

// Invalid and missing results of reads outside a refresh (confirm reads, reads queued by a
// refused write) are logged too (concept 3.5).
func TestReadiness_InvalidOrMissingConfirmAndRefusalReadsAreLogged(t *testing.T) {
	logs := captureLog(t)
	clu := allKindsClu()
	gs, g := newTestSet(t, clu)
	gs.Refresh()
	settle(t, gs)

	// The confirm read of a light write comes back without the payload.
	g.RespondNext(fakeReadPath, `[{"Clu":"CLU_012abcde","Id":"DOU0001","Kind":"Light"}]`)
	hkWrite(findC(t, clu.Lights[0].HkAccessory(), characteristic.TypeOn), true)
	settle(t, gs)
	if !strings.Contains(logs.String(), `"lamp" (CLU_012abcde|DOU0001): invalid`) {
		t.Errorf("invalid confirm read not logged:\n%s", logs.String())
	}

	// The read queued by a refused thermostat write finds no item.
	clu2 := allKindsClu()
	gs2, g2 := newTestSet(t, clu2)
	g2.Omit(tCLU, "THE0002")
	hkWrite(clu2.Therms[0].hk.Thermostat.TargetTemperature.C, 25.0)
	settle(t, gs2)
	if !strings.Contains(logs.String(), `"thermo" (CLU_012abcde|THE0002): missing from read response`) {
		t.Errorf("missing refusal read not logged:\n%s", logs.String())
	}
}
