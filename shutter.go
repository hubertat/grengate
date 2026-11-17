package main

import (
	"fmt"
	"time"

	"github.com/brutella/hap/accessory"
	"github.com/brutella/hap/characteristic"
	"github.com/brutella/hap/service"
)

// minShutterMaxTime is the shortest travel time (ms) the movement simulation can work with:
// the simulated position advances every MaxTime/100 ms.
const minShutterMaxTime = 100

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
	cancelMovement  chan bool // buffered (1); only sent to while looping, under mu
	looping         bool

	// sim is the simulation goroutine's state (shutter_sim.go).
	sim *shutterSim

	// State is the motor state HomeKit shows: the simulation's belief (0 stopped, 1 going
	// up, 2 going down). Written by the simulation goroutine only.
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
	sh.cancelMovement = make(chan bool, 1)
	sh.sim = newShutterSim(sh)

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

	// A move needs MaxTime (concept 3.5): refused until the shutter was read.
	sh.hk.WindowCovering.TargetPosition.SetValueRequestFunc = sh.writeGuard("move")
	sh.hk.WindowCovering.TargetPosition.OnValueRemoteUpdate(sh.SetPosition)
	sh.guardReads(sh.hk.A)

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
	sh.syncMu.Lock()
	defer sh.syncMu.Unlock()
	sh.mu.Lock()
	position, state := sh.currentPosition, sh.GetHkState()
	sh.mu.Unlock()

	sh.hk.WindowCovering.CurrentPosition.SetValue(position)
	sh.hk.WindowCovering.PositionState.SetValue(state)
}

// SetPosition check which direction should move and call StartMoving func
func (sh *Shutter) SetPosition(target int) {
	sh.mu.Lock()
	if !sh.ready { // refused by writeGuard: no usable MaxTime yet
		sh.mu.Unlock()
		return
	}

	sh.clu.set.Debugf("Shutter SetPosition | target: %d\tcurrent: %d\told target: %d\n", target, sh.currentPosition, sh.targetPosition)
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
	period := time.Duration(sh.MaxTime/100) * time.Millisecond
	sh.clu.set.Debugf("Shutter setting position cmd: %v\n", cmd)
	result := sh.submitCmdLocked(cmd)
	sh.mu.Unlock()

	sh.waitCommand(result)

	sh.mu.Lock()
	if sh.looping {
		// The running loop stops the motor and ends (behaviour kept until stage 4).
		select {
		case sh.cancelMovement <- true:
		default:
		}
		sh.mu.Unlock()
		return
	}
	sh.looping = true
	sh.mu.Unlock()

	sh.clu.set.Debugf("Shutter starting move ticker period: %s\n", period.String())
	go sh.moveLoop(time.NewTicker(period))
}

func (sh *Shutter) moveLoop(ticker *time.Ticker) {
	defer ticker.Stop()
	for {
		select {
		case <-sh.cancelMovement:
			sh.endMove()
			return
		case <-ticker.C:
			sh.mu.Lock()
			reached := sh.targetPosition == sh.currentPosition
			if !reached {
				if sh.currentPosition < sh.targetPosition {
					sh.currentPosition++
				} else {
					sh.currentPosition--
				}
			}
			sh.mu.Unlock()
			if reached {
				sh.endMove()
				return
			}
		}
	}
}

// endMove stops the motor and ends the movement loop.
func (sh *Shutter) endMove() {
	sh.sendCmd(shutterStop)
	sh.mu.Lock()
	sh.looping = false
	select { // a cancel sent while the STOP was in flight has been served by it
	case <-sh.cancelMovement:
	default:
	}
	sh.mu.Unlock()
	sh.Sync()
}

// sendCmd sends a movement command and waits for its result (logged by the write lane).
func (sh *Shutter) sendCmd(cmd shutterCmd) {
	sh.mu.Lock()
	result := sh.submitCmdLocked(cmd)
	sh.mu.Unlock()
	sh.waitCommand(result)
}

// submitCmdLocked submits a movement command; call with sh.mu held (see submitCommandLocked).
func (sh *Shutter) submitCmdLocked(cmd shutterCmd) <-chan laneResult {
	req := sh.Req
	switch cmd {
	case shutterUp:
		req.Cmd = "MOVEUP"
	case shutterDown:
		req.Cmd = "MOVEDOWN"
	case shutterStop:
		req.Cmd = "STOP"
	}
	return sh.submitCommandLocked(req)
}

// applyRead applies a read result. A shutter is ready once a read with State and a usable
// MaxTime (concept 3.5) has been applied. The observed motor state goes to the simulation
// goroutine with the read's completion stamp, without waiting for it; the goroutine decides
// what HomeKit shows (concept 3.4).
func (sh *Shutter) applyRead(obj ReqObject, sentGen, stamp uint64) (ApplyOutcome, error) {
	p, err := validShutter(obj)
	if err != nil {
		return ApplyInvalid, err
	}
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if !sh.acceptReadLocked(sentGen) {
		return ApplyDiscarded, nil
	}
	changed := sh.State != *p.State || sh.MaxTime != *p.MaxTime
	sh.MaxTime = *p.MaxTime
	sh.sim.observed(*p.State, stamp)
	return sh.appliedLocked(changed), nil
}
