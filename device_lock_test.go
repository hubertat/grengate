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
	gs, _ := newTestSet(t, clu)
	light, thermo, motion := clu.Lights[0], clu.Therms[0], clu.MotionSensors[0]

	var wg sync.WaitGroup
	wg.Add(3)
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
		for i := 0; i < 20; i++ {
			motion.Set(i%2 == 0)
		}
	}()
	wg.Wait()
}
