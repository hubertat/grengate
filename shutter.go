package main

import (
	"fmt"
	"time"
	"github.com/brutella/hc/accessory"
	"github.com/brutella/hc/service"
	"github.com/brutella/hc/characteristic"
)

type ShutterAccessory struct {
	*accessory.Accessory

	WindowCovering			*service.WindowCovering
}
type Shutter struct {
	CluObject

	hk					ShutterAccessory
	
	position			int

	// State: 0 - stopped; 1 - going up; 2 - going down
	State				int		
	MaxTime				int
}

func (sh *Shutter) InitAll() {
	sh.Req = ReqObject{
		Kind: "Shutter",
		Clu: sh.clu.Id,
		Id: sh.GetMixedId(),

	}
	sh.AppendHk()
}
func (sh *Shutter) AppendHk() {
	info := accessory.Info{
		Name:         sh.Name,
		SerialNumber: fmt.Sprintf("%d", sh.Id),
		Manufacturer: "Grenton",
		Model:        sh.Kind,
		ID:           sh.GetLongId(),
	}

	sh.hk = ShutterAccessory{}
	sh.hk.Accessory = accessory.New(info, accessory.TypeWindowCovering)
	sh.hk.WindowCovering = service.NewWindowCovering()
	sh.hk.WindowCovering.CurrentPosition.SetValue(0)
	sh.hk.WindowCovering.TargetPosition.SetValue(0)
	sh.hk.WindowCovering.PositionState.SetValue(characteristic.PositionStateStopped)


	sh.hk.WindowCovering.TargetPosition.OnValueRemoteUpdate(sh.SetPosition)

	
	sh.clu.set.Logf("HK WindowCovering added (id: %x)", sh.hk.Accessory.ID)
}

// GetHkState returns windows covering state in HomeKit characteristic format
func (sh *Shutter) GetHkState() int {
	switch sh.State {
	case 1:
		return characteristic.PositionStateIncreasing
	case 2:
		return characteristic.PositionStateDecreasing
	default:
		return characteristic.PositionStateStopped
	}
}

// Sync sets HK accessory values based on Shutter values
func (sh *Shutter) Sync() {
	sh.hk.WindowCovering.CurrentPosition.SetValue(sh.position)
	sh.hk.WindowCovering.PositionState.SetValue(sh.GetHkState())
}

// SetPosition check which direction should move and call StartMoving func
func (sh *Shutter) SetPosition(target int) {
	sh.hk.WindowCovering.TargetPosition.SetValue(target)
	
	go sh.StartMoving(target > sh.position)
}

// StartMoving	sends request to move up (or != up = down) and simulates motion
func (sh *Shutter) StartMoving(up bool) {
	// SEND request
	req := sh.Req
	req.Shutter = sh
	if up {
		req.Cmd = "MOVEUP"
	} else {
		req.Cmd = "MOVEDOWN"
	}
	obj, err := sh.SendReq(req)
	if err != nil {
		sh.clu.set.Error(fmt.Errorf("Shutter StartMoving: error from sending request:\n%v", err))
		return
	}
	sh.LoadReqObject(obj)

	var increment, period int

	period = sh.MaxTime / 100
	
	if up {
		increment = 1
	} else {
		increment = -1
	}

	for ; sh.position % 100 != 0; sh.position = sh.position + increment {
		time.Sleep(time.Duration(period) * time.Millisecond)
		sh.Sync()
	}
}

// LoadReqObject checks object received from http request end reads it into Shutter
func (sh *Shutter) LoadReqObject(obj ReqObject) error {
	if obj.Kind != "Shutter" {
		return fmt.Errorf("Shutter LoadReqObject: wrong object kind (%s)", obj.Kind)
	}

	if obj.Shutter == nil {
		return fmt.Errorf("Shutter LoadReqObject: missing Shutter object")
	}

	sh.clu.set.Debugf("Shutter LoadReqObject loading: \n%+v", obj)

	sh.State = obj.Shutter.State
	sh.MaxTime = obj.Shutter.MaxTime

	sh.Sync()

	return nil
}