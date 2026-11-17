package main

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// cadenceClu has a light and two thermostats; the fake GATE leaves a field out of THE0003,
// so that one never becomes ready.
func cadenceClu() *Clu {
	return &Clu{
		Id:     tCLU,
		Lights: []*Light{{CluObject: CluObject{Id: 1, Kind: "DOU", Name: "lamp"}}},
		Therms: []*Thermo{
			{CluObject: CluObject{Id: 2, Kind: "THE", Name: "ready"}, Source: "sensor"},
			{CluObject: CluObject{Id: 3, Kind: "THE", Name: "unready"}, Source: "sensor"},
		},
	}
}

// readsPerDevice counts the read items per device id in the given read requests.
func readsPerDevice(reqs []fakeGateRequest) map[string]int {
	n := map[string]int{}
	for _, r := range reqs {
		for _, id := range itemIds(r) {
			n[id]++
		}
	}
	return n
}

// With a 10 s cycle and ThermoCycleInSeconds 60, a thermostat is read once per six cycles;
// one that is not ready is read every cycle, as is every light.
func TestRefresh_ThermostatCadence(t *testing.T) {
	clk := newFakeClock(time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC))
	gs, g := newTestSetWith(t, func(gs *GrentonSet) {
		gs.clock = clk
		gs.ThermoCycleInSeconds = 60
	}, cadenceClu())
	g.DropField(tCLU, "THE0003", "TempCurrent")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	gs.startup(ctx, false) // reads everything at +0 s
	gs.startCycling(ctx)
	const cycles = 18 // refreshes at +10 s .. +180 s
	for k := 1; k <= cycles; k++ {
		clk.nextWait(t)
		clk.Advance(10 * time.Second)
		g.WaitForRequests(t, fakeReadPath, 1+k)
	}
	clk.nextWait(t) // the last refresh finished

	reqs := g.Requests(fakeReadPath)
	if len(reqs) != 1+cycles {
		t.Fatalf("%d read requests, want one per refresh (%d)", len(reqs), 1+cycles)
	}
	got := readsPerDevice(reqs)
	want := map[string]int{"DOU0001": 1 + cycles, "THE0002": 4, "THE0003": 1 + cycles} // THE0002 at +0, +60, +120, +180
	for id, n := range want {
		if got[id] != n {
			t.Errorf("%s read %d times in %d refreshes, want %d", id, got[id], 1+cycles, n)
		}
	}
	for k, r := range reqs {
		polled := false
		for _, id := range itemIds(r) {
			polled = polled || id == "THE0002"
		}
		if polled != (k%6 == 0) {
			t.Errorf("refresh at +%d s: ready thermostat read = %v, want %v", 10*k, polled, k%6 == 0)
		}
	}
}

// Refreshes start a little after their boundary: a thermostat still comes up every sixth
// cycle when the refresh that polled it started late and the one six cycles later on time.
func TestRefresh_ThermostatCadenceToleratesJitter(t *testing.T) {
	clk := newFakeClock(time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC))
	clu := cadenceClu()
	clu.Therms = clu.Therms[:1]
	gs, g := newTestSetWith(t, func(gs *GrentonSet) {
		gs.clock = clk
		gs.ThermoCycleInSeconds = 60
	}, clu)

	clk.Advance(300 * time.Millisecond)
	gs.Refresh() // polls the thermostat 300 ms late
	for k := 1; k <= 6; k++ {
		clk.Advance(10 * time.Second)
		if k == 1 {
			clk.Advance(-300 * time.Millisecond) // the following refreshes start on time
		}
		c := gs.Refresh()
		if wantSkip := k < 6; (c.Skipped == 1) != wantSkip {
			t.Errorf("refresh %d: skipped %d, want thermostat skipped = %v", k, c.Skipped, wantSkip)
		}
	}
	if n := readsPerDevice(g.Requests(fakeReadPath))["THE0002"]; n != 2 {
		t.Errorf("thermostat read %d times, want 2", n)
	}
}

// Devices not due are counted as skipped and are not part of the refresh's object count:
// applied, discarded, missing and failed still add up to it (concept 3.8).
func TestRefresh_SkippedObjects(t *testing.T) {
	clk := newFakeClock(time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC))
	gs, g, m := newMetricsSet(t, func(gs *GrentonSet) {
		gs.clock = clk
		gs.ThermoCycleInSeconds = 60
	}, cadenceClu())
	g.DropField(tCLU, "THE0003", "TempCurrent")

	first := gs.Refresh()
	clk.Advance(10 * time.Second)
	second := gs.Refresh()

	check := func(name string, c refreshCounts, objects, skipped int) {
		t.Helper()
		if c.Objects != objects || c.Skipped != skipped {
			t.Errorf("%s: objects %d, skipped %d; want %d, %d", name, c.Objects, c.Skipped, objects, skipped)
		}
		if sum := c.Applied + c.Discarded + c.Missing + c.Failed; sum != c.Objects {
			t.Errorf("%s: applied+discarded+missing+failed = %d, object count %d", name, sum, c.Objects)
		}
	}
	check("first refresh", first, 3, 0)
	check("second refresh", second, 2, 1) // the ready thermostat is not due

	_, _, refreshes := m.snapshot()
	if len(refreshes) != 2 || refreshes[1].Counts.Skipped != 1 || refreshes[1].Counts.Objects != 2 {
		t.Errorf("refresh events %+v, want the second with 2 objects and 1 skipped", refreshes)
	}
}

// With ThermoCycleInSeconds at or below the refresh cycle, thermostats are read in every
// refresh.
func TestRefresh_ThermostatCycleNotAboveRefreshCycle(t *testing.T) {
	for _, secs := range []int{1, 10} {
		t.Run(fmt.Sprintf("ThermoCycleInSeconds=%d", secs), func(t *testing.T) {
			clk := newFakeClock(time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC))
			clu := cadenceClu()
			clu.Therms = clu.Therms[:1]
			gs, g := newTestSetWith(t, func(gs *GrentonSet) {
				gs.clock = clk
				gs.ThermoCycleInSeconds = secs
			}, clu)
			for k := 0; k < 3; k++ {
				if c := gs.Refresh(); c.Skipped != 0 {
					t.Errorf("refresh %d skipped %d", k, c.Skipped)
				}
				clk.Advance(10 * time.Second)
			}
			if n := readsPerDevice(g.Requests(fakeReadPath))["THE0002"]; n != 3 {
				t.Errorf("thermostat read %d times, want 3", n)
			}
		})
	}
}
