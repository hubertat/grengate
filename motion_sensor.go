package main

import (
	"fmt"
	"time"

	"github.com/brutella/hap/accessory"
	"github.com/brutella/hap/characteristic"
	"github.com/brutella/hap/service"
)

const clearMotionEventDuration = 15 * time.Second

type MotionSensor struct {
	CluObject

	State bool

	clearTimer      *time.Timer
	resetClearTimer chan bool

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
	ms.hkService = service.NewMotionSensor()
	ms.hkFault = characteristic.NewStatusFault()
	ms.hkFault.SetValue(characteristic.StatusFaultNoFault)

	ms.hkService.AddC(ms.hkFault.C)
	ms.hkAccessory.AddS(ms.hkService.S)
	ms.hkService.MotionDetected.SetValue(false)

	return ms.hkAccessory
}

func (ms *MotionSensor) Set(state bool) {
	ms.State = state
	ms.hkService.MotionDetected.SetValue(state)
}

func (ms *MotionSensor) SetOn() {
	ms.Set(true)
	ms.clearTimer = time.NewTimer(clearMotionEventDuration)
	go func() {
		for {
			select {
			case <-ms.clearTimer.C:
				ms.Set(false)
				return
			case <-ms.resetClearTimer:
				ms.clearTimer.Stop()
				return
			}
		}
	}()
}
