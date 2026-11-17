package main

import (
	"sync"
	"testing"
)

// Reads applied by one goroutine while HomeKit callbacks run in others: with -race this
// fails if a device field is touched without the device lock, or if a command points at
// the live device instead of a snapshot.
func TestDevices_ConcurrentReadsAndCommandsAreRaceFree(t *testing.T) {
	clu := allKindsClu()
	gs, g := newTestSet(t, clu)
	light, thermo, motion := clu.Lights[0], clu.Therms[0], clu.MotionSensors[0]

	var wg sync.WaitGroup
	wg.Add(4)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			gs.update([]ReqObject{
				{Clu: clu.Id, Id: "DOU0001", Kind: "Light", Light: &Light{State: i%2 == 0}},
				{Clu: clu.Id, Id: "THE0002", Kind: "Thermo", Thermo: &Thermo{TempCurrent: float64(i), State: 1}},
				{Clu: clu.Id, Id: "ROL0003", Kind: "Shutter", Shutter: &Shutter{MaxTime: 20000}},
			})
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			light.Sync()
			thermo.Sync()
			clu.Shutters[0].Sync()
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 5; i++ {
			light.Set(i%2 == 1)
			thermo.SetTemperature(20 + float64(i))
			thermo.SetState(1)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			motion.Set(i%2 == 0)
		}
	}()
	wg.Wait()

	if n := g.RequestCount(fakeSetPath); n != 15 {
		t.Errorf("commands at the fake GATE = %d, want 15", n)
	}
}
