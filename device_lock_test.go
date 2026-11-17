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
		for i := 0; i < 10; i++ {
			g.SetLight(clu.Id, "DOU0001", i%2 == 0)
			g.SetThermo(clu.Id, "THE0002", float64(i), 21, 0, 1)
			gs.Refresh()
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

	if n := g.RequestCount(fakeReadPath); n != 10 {
		t.Errorf("read requests = %d, want 10", n)
	}
	if n := g.RequestCount(fakeSetPath); n != 15 {
		t.Errorf("commands at the fake GATE = %d, want 15", n)
	}
}
