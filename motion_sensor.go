package main

import (
	"fmt"

	"github.com/brutella/hap/accessory"
	"github.com/brutella/hap/characteristic"
	"github.com/brutella/hap/service"
)

type MotionSensor struct {
	CluObject

	State bool

	hkAccessory *accessory.A
	hkService   *service.MotionSensor
	hkFault     *characteristic.StatusFault
}

func (ms *MotionSensor) Init(clu *Clu) *accessory.A {
	ms.clu = clu

	ms.Req = ReqObject{
		Kind: "MotionSensor",
		Clu:  ms.clu.Id,
		Id:   ms.GetMixedId(),
	}
	return ms.appendHk()
}

func (ms *MotionSensor) GetA() *accessory.A {
	return ms.hkAccessory
}

func (ms *MotionSensor) appendHk() *accessory.A {
	info := accessory.Info{
		Name:         ms.Name,
		SerialNumber: fmt.Sprintf("%d", ms.Id),
		// Model:        ms.Kind,
	}

	ms.hkAccessory = accessory.New(info, accessory.TypeSensor)
	ms.hkAccessory.Id = ms.GetLongId()

	ms.hkService = service.NewMotionSensor()
	ms.hkFault = characteristic.NewStatusFault()
	ms.hkFault.SetValue(characteristic.StatusFaultNoFault)

	ms.hkService.AddC(ms.hkFault.C)
	ms.hkAccessory.AddS(ms.hkService.S)
	ms.hkService.MotionDetected.SetValue(false)
	ms.guardReads(ms.hkAccessory)

	ms.clu.set.Logf("HK MotionSensor added (id: %x)", ms.hkAccessory.Id)

	return ms.hkAccessory
}

// Set applies a pushed state (input server) under rule 3 of concept 3.3: it bumps the
// generation, so a poll result from a read sent before the push is discarded. A push
// carries the sensor's whole state, so the sensor is ready from then on. HomeKit is
// updated through the syncer.
func (ms *MotionSensor) Set(state bool) {
	ms.mu.Lock()
	ms.State = state
	ms.gen++
	ms.ready = true
	ms.mu.Unlock()
	ms.clu.set.hkSync.mark(ms)
}

// Sync pushes the state to HomeKit.
func (ms *MotionSensor) Sync() {
	ms.syncMu.Lock()
	defer ms.syncMu.Unlock()
	ms.mu.Lock()
	state := ms.State
	ms.mu.Unlock()
	ms.hkService.MotionDetected.SetValue(state)
}

func (ms *MotionSensor) applyRead(obj ReqObject, sentGen, _ uint64) (ApplyOutcome, error) {
	p, err := validMotion(obj)
	if err != nil {
		return ApplyInvalid, err
	}
	ms.mu.Lock()
	defer ms.mu.Unlock()
	if !ms.acceptReadLocked(sentGen) {
		return ApplyDiscarded, nil
	}
	changed := ms.State != *p.State
	ms.State = *p.State
	return ms.appliedLocked(changed), nil
}
