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

// LoadReqObject checks object received from http request end reads it into MotionSensor
func (ms *MotionSensor) LoadReqObject(obj ReqObject) error {
	if obj.Kind != "MotionSensor" {
		return fmt.Errorf("MotionSensor LoadReqObject: wrong object kind (%s)", obj.Kind)
	}

	if obj.MotionSensor == nil {
		return fmt.Errorf("MotionSensor LoadReqObject: missing MotionSensor object")
	}

	ms.clu.set.Debugf("MotionSensor LoadReqObject loading: \n%+v", obj)

	ms.Set(obj.MotionSensor.State)

	return nil
}
