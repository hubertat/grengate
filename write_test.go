package main

import (
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brutella/hap/characteristic"
)

// hkWrite performs a HomeKit write on c as a paired controller would (hap's
// PUT /characteristics handler calls SetValueRequest per item) and returns hap's status.
func hkWrite(c *characteristic.C, v any) int {
	_, status := c.SetValueRequest(v, httptest.NewRequest("PUT", "/characteristics", nil))
	return status
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
		gs, g := newTestSet(t, clu)
		g.SetWriteStyle(style)
		gs.Refresh()
		settle(t, gs)

		hkWrite(findC(t, clu.Lights[0].HkAccessory(), characteristic.TypeOn), true)
		hkWrite(clu.Therms[0].hk.Thermostat.TargetTemperature.C, 23.0)
		clu.Shutters[0].sendCmd(shutterStop)
		settle(t, gs)

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
	gs, g := newTestSet(t, clu)
	gs.Refresh()
	settle(t, gs)

	hkWrite(findC(t, clu.Lights[0].HkAccessory(), characteristic.TypeOn), true)
	hkWrite(findC(t, clu.Lights[1].HkAccessory(), characteristic.TypeOn), true)
	hkWrite(clu.Therms[0].hk.Thermostat.TargetTemperature.C, 22.5)
	settle(t, gs)

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
	gs, g := newTestSet(t, clu)
	gs.Refresh()
	settle(t, gs)
	light, thermo := clu.Lights[0], clu.Therms[0]
	on := findC(t, light.HkAccessory(), characteristic.TypeOn)
	onSyncs := recordSyncs(on)
	currentSyncs := recordSyncs(thermo.hk.Thermostat.CurrentTemperature.C)

	// Old-script style answers with a read-back that contradicts the command; the confirm
	// read is held so the state between acknowledgement and confirm read can be seen.
	g.RespondNext(fakeSetPath, `{"Clu":"CLU_012abcde","Id":"DOU0001","Kind":"Light","Light":{"State":false}}`)
	g.RespondNext(fakeSetPath, `{"Clu":"CLU_012abcde","Id":"THE0002","Kind":"Thermo","Thermo":{"TempCurrent":99,"TempTarget":99,"TempSetpoint":99,"State":1,"Mode":1}}`)
	h := g.HoldNext(fakeReadPath)
	hkWrite(on, true)
	hkWrite(thermo.hk.Thermostat.TargetTemperature.C, 21.5)
	h.Wait(t)
	eventually(t, 2*time.Second, gs.writer.idle, "both writes to finish")
	syncHomeKit(t, gs)

	light.mu.Lock()
	state := light.State
	light.mu.Unlock()
	thermo.mu.Lock()
	current := thermo.TempCurrent
	thermo.mu.Unlock()
	if !state || on.Val != true || onSyncs.contains(false) {
		t.Error("light state taken from the write response")
	}
	if current == 99 || currentSyncs.contains(99.0) {
		t.Error("current temperature taken from the write response")
	}
	h.Release()
	settle(t, gs)
}

// A HomeKit write returns to hap without waiting for the GATE; the command is still sent
// and acknowledged.
func TestWrite_CallbackReturnsWithoutWaiting(t *testing.T) {
	logs := captureLog(t)
	clu := allKindsClu()
	gs, g := newTestSet(t, clu)
	gs.Refresh()
	settle(t, gs)
	g.HangAll(fakeSetPath)

	within(t, 50*time.Millisecond, "HomeKit writes while the GATE hangs", func() {
		hkWrite(findC(t, clu.Lights[0].HkAccessory(), characteristic.TypeOn), true)
		hkWrite(clu.Therms[0].hk.Thermostat.TargetTemperature.C, 23.0)
	})
	g.WaitForRequests(t, fakeSetPath, 1)
	g.ReleaseAll()
	settle(t, gs)
	if g.Payload(tCLU, "DOU0001")["State"] != true || g.Payload(tCLU, "THE0002")["TempSetpoint"] != 23.0 {
		t.Error("commands did not reach the fake GATE")
	}
	if n := strings.Count(logs.String(), "acknowledged"); n != 2 {
		t.Errorf("%d acknowledgements logged, want 2", n)
	}
}

// A HomeKit request setting ten lights (hap runs the items of one PUT /characteristics in
// a loop, one SetValueRequest per item) returns in well under one GATE round trip.
func TestWrite_TenLightsReturnFastAgainstSlowGate(t *testing.T) {
	clu := lightsClu(10)
	gs, g := newTestSet(t, clu)
	g.Delay(fakeSetPath, 300*time.Millisecond)

	start := time.Now()
	for _, l := range clu.Lights {
		if status := hkWrite(findC(t, l.HkAccessory(), characteristic.TypeOn), true); status != 0 {
			t.Fatalf("status %d", status)
		}
	}
	if d := time.Since(start); d >= 50*time.Millisecond {
		t.Errorf("ten HomeKit writes took %v, want under 50 ms", d)
	}

	g.WaitForRequests(t, fakeSetPath, 1)
	g.ClearRules() // the rest of the commands need not wait 300 ms each
	settle(t, gs)
	if n := g.RequestCount(fakeSetPath); n != 10 {
		t.Errorf("set requests = %d, want 10", n)
	}
	for i := 1; i <= 10; i++ {
		if g.Payload(tCLU, lightId(i))["State"] != true {
			t.Errorf("%s not switched on", lightId(i))
		}
	}
}

func TestWrite_FailedWriteIsLogged(t *testing.T) {
	logs := captureLog(t)
	clu := allKindsClu()
	gs, g := newTestSet(t, clu)
	g.FailNext(fakeSetPath, 500, 1)
	hkWrite(findC(t, clu.Lights[0].HkAccessory(), characteristic.TypeOn), true)
	settle(t, gs)
	if !strings.Contains(logs.String(), "uncertain") {
		t.Errorf("failed write not logged as uncertain:\n%s", logs.String())
	}
}

// Two HomeKit callbacks for one device must reach the mailbox in the order they changed the
// device state: the command snapshot is submitted while the device lock is still held.
func TestWrite_CommandEntersMailboxUnderDeviceLock(t *testing.T) {
	clu := allKindsClu()
	clu.Lights = append(clu.Lights, &Light{CluObject: CluObject{Id: 5, Kind: "DOU", Name: "blocker"}})
	gs, g := newTestSet(t, clu)
	gs.Refresh() // the thermostat refuses writes until it was read
	settle(t, gs)

	var mu sync.Mutex
	merges := map[string]bool{} // device id -> device lock held while merging
	gs.writer.cfg.merge = func(p, in ReqObject) (ReqObject, bool) {
		d, _ := gs.registry.find(in.Clu, in.Id)
		held := !d.base().mu.TryLock()
		if !held {
			d.base().mu.Unlock()
		}
		mu.Lock()
		merges[in.Id] = held
		mu.Unlock()
		return mergeReplace(p, in)
	}

	h := g.HoldNext(fakeSetPath)
	blocker := gs.writer.Submit(lightCmd("DOU0005", true))[0]
	h.Wait(t)

	light, thermo, shutter := clu.Lights[0], clu.Therms[0], clu.Shutters[0]
	calls := []func(){
		func() { light.Set(true) }, func() { light.Set(false) },
		func() { thermo.SetTemperature(22) }, func() { thermo.SetState(1) },
		func() { shutter.sendCmd(shutterUp) }, func() { shutter.sendCmd(shutterStop) },
	}
	var wg sync.WaitGroup
	for _, call := range calls {
		wg.Add(1)
		go func() { defer wg.Done(); call() }()
	}
	eventually(t, 2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(merges) == 3
	}, "one merge per device")
	h.Release()
	waitResult(t, blocker)
	wg.Wait()

	for id, held := range merges {
		if !held {
			t.Errorf("%s: command entered the mailbox after the device lock was released", id)
		}
	}
}

// Concurrent callbacks for one light: whatever their order, the GATE ends with the state
// the device (and HomeKit) ended with.
func TestWrite_ConcurrentCallbacksLatestStateWins(t *testing.T) {
	clu := lightsClu(2)
	gs, g := newTestSet(t, clu)
	light := clu.Lights[0]
	var merges atomic.Int32
	gs.writer.cfg.merge = func(p, in ReqObject) (ReqObject, bool) {
		merges.Add(1)
		return mergeReplace(p, in)
	}
	for i := 0; i < 50; i++ {
		h := g.HoldNext(fakeSetPath)
		blocker := gs.writer.Submit(lightCmd("DOU0002", i%2 == 0))[0]
		h.Wait(t)
		var wg sync.WaitGroup
		for _, v := range []bool{true, false} {
			wg.Add(1)
			go func() { defer wg.Done(); light.Set(v) }()
		}
		eventually(t, 2*time.Second, func() bool { return merges.Load() == int32(i+1) },
			"both commands in the mailbox")
		wg.Wait()
		light.mu.Lock()
		state := light.State // what HomeKit was told last
		light.mu.Unlock()
		h.Release()
		waitResult(t, blocker)
		settle(t, gs)

		if got := g.Payload(tCLU, "DOU0001")["State"]; got != state {
			t.Fatalf("iteration %d: GATE state %v, last state set by HomeKit %v", i, got, state)
		}
	}
}

// A command replaced in the mailbox by a command of another device with the same Clu and Id
// (a duplicated configuration entry) still finishes for its own device: neither device is
// left with a pending command, so their reads are applied again.
func TestWrite_CommandReplacedByDuplicateDeviceFinishesForBoth(t *testing.T) {
	clu := lightsClu(2)
	clu.Lights = append(clu.Lights, &Light{CluObject: CluObject{Id: 1, Kind: "DOU", Name: "dup"}})
	gs, g := newTestSet(t, clu)
	gs.Refresh()
	settle(t, gs)
	first, dup := clu.Lights[0], clu.Lights[2]

	h := g.HoldNext(fakeSetPath)
	blocker := gs.writer.Submit(lightCmd("DOU0002", true))[0]
	h.Wait(t)
	first.Set(true)
	first.Set(false)
	dup.Set(true) // replaces first's command in the mailbox
	h.Release()
	waitResult(t, blocker)
	settle(t, gs)

	if n := g.RequestCount(fakeSetPath); n != 2 {
		t.Errorf("set requests = %d, want 2 (blocker, one command for DOU0001)", n)
	}
	g.SetLight(tCLU, "DOU0001", false)
	if c := gs.Refresh(); c.Discarded != 0 || c.Applied != 2 {
		t.Errorf("refresh = %+v, want both registered lights applied", c)
	}
	for _, l := range []*Light{first, dup} {
		l.mu.Lock()
		busy := l.busy
		l.mu.Unlock()
		if busy {
			t.Errorf("%q still has a pending command", l.Name)
		}
	}
}

// Commands still waiting when the write lane stops are finished for their devices.
func TestWrite_StopFinishesPendingCommands(t *testing.T) {
	clu := lightsClu(2)
	gs, g := newTestSet(t, clu)
	g.HangAll(fakeSetPath)
	clu.Lights[0].Set(true) // in flight
	g.WaitForRequests(t, fakeSetPath, 1)
	clu.Lights[1].Set(true) // waiting in the mailbox
	gs.Stop()
	for _, l := range clu.Lights {
		l.mu.Lock()
		busy := l.busy
		l.mu.Unlock()
		if busy {
			t.Errorf("%q still has a pending command after Stop", l.Name)
		}
	}
}
