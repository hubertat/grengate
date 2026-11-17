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

	ms.clu.set.Logf("HK MotionSensor added (id: %x)", ms.hkAccessory.Id)

	return ms.hkAccessory
}

// Set applies a pushed state (input server) and shows it in HomeKit.
func (ms *MotionSensor) Set(state bool) {
	ms.State = state
	ms.Sync()
}

// Sync pushes the state to HomeKit.
func (ms *MotionSensor) Sync() {
	ms.hkService.MotionDetected.SetValue(ms.State)
}

// LoadReqObject applies a read item and pushes the state to HomeKit.
func (ms *MotionSensor) LoadReqObject(obj ReqObject) error { return loadReqObject(ms, obj) }

func (ms *MotionSensor) applyRead(obj ReqObject) (bool, error) {
	if obj.Kind != "MotionSensor" {
		return false, fmt.Errorf("MotionSensor: wrong object kind (%s)", obj.Kind)
	}
	if obj.MotionSensor == nil {
		return false, fmt.Errorf("MotionSensor: missing MotionSensor object")
	}
	changed := !ms.loaded || ms.State != obj.MotionSensor.State
	ms.State = obj.MotionSensor.State
	ms.loaded = true
	return changed, nil
}
