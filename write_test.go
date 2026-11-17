package main

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/brutella/hap/characteristic"
)

// hkWrite performs a HomeKit write on c as a paired controller would and returns when
// hap returns (the callback is still waiting for the GATE in this stage).
func hkWrite(c *characteristic.C, v any) {
	c.SetValueRequest(v, httptest.NewRequest("PUT", "/characteristics", nil))
}

// errorLines returns log lines that report an error.
func errorLines(log string) []string {
	var out []string
	for _, l := range strings.Split(log, "\n") {
		low := strings.ToLower(l)
		if strings.Contains(low, "error") || strings.Contains(low, "fail") || strings.Contains(low, "uncertain") {
			out = append(out, l)
		}
	}
	return out
}

func TestWrite_SuccessfulWriteLogsNoError(t *testing.T) {
	for _, style := range []fakeWriteStyle{fakeOldStyle, fakeNewStyle} {
		logs := captureLog(t)
		clu := allKindsClu()
		_, g := newTestSet(t, clu)
		g.SetWriteStyle(style)
		g.SetShutter(clu.Id, "ROL0003", 20000, 0)

		hkWrite(findC(t, clu.Lights[0].HkAccessory(), characteristic.TypeOn), true)
		hkWrite(clu.Therms[0].hk.Thermostat.TargetTemperature.C, 23.0)
		clu.Shutters[0].sendCmd(shutterStop)

		if n := g.RequestCount(fakeSetPath); n != 3 {
			t.Fatalf("style %d: set requests = %d, want 3", style, n)
		}
		if lines := errorLines(logs.String()); len(lines) > 0 {
			t.Errorf("style %d: successful writes logged errors:\n%s", style, strings.Join(lines, "\n"))
		}
		if n := strings.Count(logs.String(), "acknowledged"); n != 3 {
			t.Errorf("style %d: %d acknowledgements logged, want 3", style, n)
		}
	}
}

func TestWrite_LegacyProtocolOneSingleObjectPerCommand(t *testing.T) {
	clu := allKindsClu()
	clu.Lights = append(clu.Lights, &Light{CluObject: CluObject{Id: 5, Kind: "DOU", Name: "second"}})
	_, g := newTestSet(t, clu)

	hkWrite(findC(t, clu.Lights[0].HkAccessory(), characteristic.TypeOn), true)
	hkWrite(findC(t, clu.Lights[1].HkAccessory(), characteristic.TypeOn), true)
	hkWrite(clu.Therms[0].hk.Thermostat.TargetTemperature.C, 22.5)

	reqs := g.Requests(fakeSetPath)
	if len(reqs) != 3 {
		t.Fatalf("set requests = %d, want 3", len(reqs))
	}
	for i, r := range reqs {
		if r.IsArray || len(r.Items) != 1 {
			t.Errorf("request %d: %s, want a single object", i, r.Body)
		}
		if r.Items[0]["Cmd"] != "SET" {
			t.Errorf("request %d: Cmd = %v, want SET", i, r.Items[0]["Cmd"])
		}
	}
	th, _ := reqs[2].Items[0]["Thermo"].(map[string]any)
	for _, f := range []string{"TempSetpoint", "State", "Mode"} {
		if _, ok := th[f]; !ok {
			t.Errorf("thermostat command lacks %s: %s", f, reqs[2].Body)
		}
	}
	if th["TempSetpoint"] != 22.5 {
		t.Errorf("TempSetpoint = %v, want 22.5", th["TempSetpoint"])
	}
}

func TestWrite_ResponseIsNeverLoadedAsState(t *testing.T) {
	clu := allKindsClu()
	_, g := newTestSet(t, clu)
	light, thermo := clu.Lights[0], clu.Therms[0]
	on := findC(t, light.HkAccessory(), characteristic.TypeOn)

	// Old-script style answer with a read-back that contradicts the command.
	g.RespondNext(fakeSetPath, `{"Clu":"CLU_012abcde","Id":"DOU0001","Kind":"Light","Light":{"State":false}}`)
	hkWrite(on, true)
	if on.Val != true {
		t.Errorf("HomeKit On = %v after write, want true", on.Val)
	}
	light.mu.Lock()
	state := light.State
	light.mu.Unlock()
	if !state {
		t.Error("device state taken from the write response")
	}

	g.RespondNext(fakeSetPath, `{"Clu":"CLU_012abcde","Id":"THE0002","Kind":"Thermo","Thermo":{"TempCurrent":99,"TempTarget":99}}`)
	hkWrite(thermo.hk.Thermostat.TargetTemperature.C, 21.0)
	if v := thermo.hk.Thermostat.CurrentTemperature.Value(); v == 99 {
		t.Error("current temperature taken from the write response")
	}
}

func TestWrite_CallbackWaitsForResult(t *testing.T) {
	clu := allKindsClu()
	_, g := newTestSet(t, clu)
	h := g.HoldNext(fakeSetPath)
	done := make(chan struct{})
	go func() {
		defer close(done)
		hkWrite(findC(t, clu.Lights[0].HkAccessory(), characteristic.TypeOn), true)
	}()
	h.Wait(t)
	select {
	case <-done:
		t.Fatal("callback returned before the GATE answered")
	default:
	}
	h.Release()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("callback did not return after the GATE answered")
	}
}

func TestWrite_FailedWriteIsLogged(t *testing.T) {
	logs := captureLog(t)
	clu := allKindsClu()
	_, g := newTestSet(t, clu)
	g.FailNext(fakeSetPath, 500, 1)
	hkWrite(findC(t, clu.Lights[0].HkAccessory(), characteristic.TypeOn), true)
	if !strings.Contains(logs.String(), "uncertain") {
		t.Errorf("failed write not logged as uncertain:\n%s", logs.String())
	}
}
