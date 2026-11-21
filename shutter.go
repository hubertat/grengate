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

type ShutterAccessory struct {
	*accessory.A

	WindowCovering *service.WindowCovering
}
type Shutter struct {
	CluObject

	hk ShutterAccessory

	// Guarded by mu. currentPosition is written by the simulation goroutine only;
	// targetPosition by the HomeKit callback (a new target) and by the simulation (a target
	// it ends or gives up), the latter only while no newer HomeKit target exists: targetGen
	// is the movement generation of the latest HomeKit target.
	currentPosition int
	targetPosition  int
	targetGen       uint64

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

// Sync pushes the target, the position and the movement state to HomeKit.
func (sh *Shutter) Sync() {
	sh.syncMu.Lock()
	defer sh.syncMu.Unlock()
	sh.mu.Lock()
	current, target, state := sh.currentPosition, sh.targetPosition, sh.GetHkState()
	sh.mu.Unlock()

	wc := sh.hk.WindowCovering
	wc.TargetPosition.SetValue(target)
	wc.CurrentPosition.SetValue(current)
	wc.PositionState.SetValue(state)
}

// stepDuration is the time the simulated position takes for one percent: MaxTime/100.
func (sh *Shutter) stepDuration() time.Duration {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	return time.Duration(sh.MaxTime) * time.Millisecond / 100
}

// SetPosition is the HomeKit write of the target position. It records the target, starts a
// new movement generation and hands it to the simulation goroutine without waiting for the
// GATE (concept 3.4). writeGuard refuses it while the shutter is not ready (no MaxTime).
func (sh *Shutter) SetPosition(target int) {
	sh.mu.Lock()
	if !sh.ready { // refused by writeGuard: no usable MaxTime yet
		sh.mu.Unlock()
		return
	}
	sh.targetGen++
	sh.targetPosition = target
	sh.sim.newTarget(sh.targetGen, target)
	sh.mu.Unlock()
	sh.clu.set.hkSync.mark(sh) // see hkSyncer: a sync in progress may have overwritten the write
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
