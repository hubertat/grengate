package main

import (
	"fmt"
	"time"

	"github.com/brutella/hap/accessory"
	"github.com/brutella/hap/characteristic"
	"github.com/brutella/hap/service"
)

type shutterCmd int

const (
	shutterUp shutterCmd = iota
	shutterDown
	shutterStop
)

type ShutterAccessory struct {
	*accessory.A

	WindowCovering *service.WindowCovering
}
type Shutter struct {
	CluObject

	hk ShutterAccessory

	currentPosition int
	targetPosition  int
	cancelMovement  chan bool
	moveTicker      *time.Ticker
	looping         bool

	// State: 0 - stopped; 1 - going up; 2 - going down
	State   int
	MaxTime int
}

func (sh *Shutter) InitAll() {
	sh.Req = ReqObject{
		Kind: "Shutter",
		Clu:  sh.clu.Id,
		Id:   sh.GetMixedId(),
	}

	sh.currentPosition = 100
	sh.targetPosition = 100

	sh.AppendHk()
}
func (sh *Shutter) AppendHk() {
	info := accessory.Info{
		Name:         sh.Name,
		SerialNumber: fmt.Sprintf("%d", sh.Id),
		Manufacturer: "Grenton",
		Model:        sh.Kind,
	}

	sh.hk = ShutterAccessory(*accessory.NewWindowCovering(info))
	sh.hk.A.Id = sh.GetLongId()

	sh.hk.WindowCovering.CurrentPosition.SetValue(sh.currentPosition)
	sh.hk.WindowCovering.TargetPosition.SetValue(sh.targetPosition)
	sh.hk.WindowCovering.PositionState.SetValue(sh.GetHkState())

	sh.hk.WindowCovering.TargetPosition.OnValueRemoteUpdate(sh.SetPosition)

	sh.clu.set.Logf("HK WindowCovering added (id: %x)", sh.hk.A.Id)
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
	sh.hk.WindowCovering.CurrentPosition.SetValue(sh.currentPosition)
	sh.hk.WindowCovering.PositionState.SetValue(sh.GetHkState())
}

// SetPosition check which direction should move and call StartMoving func
func (sh *Shutter) SetPosition(target int) {
	sh.clu.set.Debugf("Shutter SetPosition | target: %d\tcurrent: %d\told target: %d\n", target, sh.currentPosition, sh.targetPosition)
	sh.hk.WindowCovering.TargetPosition.SetValue(target)
	sh.targetPosition = target

	var cmd shutterCmd

	if target == sh.currentPosition {
		cmd = shutterStop
	} else {
		if target > sh.currentPosition {
			if target == 100 {
				sh.clu.set.Debugf("Shutter || going up and target == 100, setting current to 0")
				sh.currentPosition = 0
			}
			cmd = shutterUp
		} else {
			if target == 0 {
				sh.clu.set.Debugf("Shutter || going down and target == 0, setting current to 100")
				sh.currentPosition = 100
			}
			cmd = shutterDown
		}
	}

	sh.clu.set.Debugf("Shutter setting position cmd: %v\n", cmd)

	err := sh.sendCmd(cmd)
	if err != nil {
		sh.clu.set.Error(fmt.Errorf("Shutter sendCmd: error from sending request:\n%v", err))
	}

	period, err := time.ParseDuration(fmt.Sprintf("%dms", sh.MaxTime/100))
	if err != nil {
		sh.clu.set.Error(err)
		return
	}
	if sh.looping {
		sh.cancelMovement <- true
	}
	sh.clu.set.Debugf("Shutter starting move ticker period: %s\n", period.String())
	sh.moveTicker = time.NewTicker(period)
	go sh.moveLoop()

}

func (sh *Shutter) moveLoop() {
	sh.looping = true

	for {
		select {
		case <-sh.cancelMovement:
			sh.sendCmd(shutterStop)
			sh.moveTicker.Stop()
			sh.Sync()
			sh.looping = false
			return
		case <-sh.moveTicker.C:
			if sh.targetPosition == sh.currentPosition {
				sh.sendCmd(shutterStop)
				sh.moveTicker.Stop()
				sh.Sync()
				sh.looping = false
				return
			}
			if sh.currentPosition < sh.targetPosition {
				sh.currentPosition++
			} else {
				sh.currentPosition--
			}
		}
	}

}

func (sh *Shutter) sendCmd(cmd shutterCmd) error {
	req := ReqObject{
		Kind: "Shutter",
		Clu:  sh.clu.Id,
		Id:   sh.GetMixedId(),
	}

	switch cmd {
	case shutterUp:
		req.Cmd = "MOVEUP"
	case shutterDown:
		req.Cmd = "MOVEDOWN"
	case shutterStop:
		req.Cmd = "STOP"
	}

	// ignoring returned object - cmd endpoint not working correctly
	// status will be updated on next refresh
	// obj, err := sh.SendReq(req)
	_, err := sh.SendReq(req)
	return err
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
