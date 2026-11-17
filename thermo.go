package main

import (
	"fmt"

	"github.com/brutella/hap/accessory"
)

type Thermo struct {
	CluObject

	Source string

	hk *accessory.Thermostat `json:"-"`

	TempCurrent,
	TempSetpoint,
	TempTarget float64

	Mode,
	State int
}

func (gt *Thermo) applyRead(obj ReqObject, sentGen, _ uint64) (ApplyOutcome, error) {
	in, err := validThermo(obj)
	if err != nil {
		return ApplyInvalid, err
	}
	gt.mu.Lock()
	defer gt.mu.Unlock()
	if !gt.acceptReadLocked(sentGen) {
		return ApplyDiscarded, nil
	}
	changed := gt.TempCurrent != *in.TempCurrent || gt.TempSetpoint != *in.TempSetpoint || gt.TempTarget != *in.TempTarget ||
		gt.State != *in.State || gt.Mode != *in.Mode

	gt.TempCurrent = *in.TempCurrent
	gt.TempSetpoint = *in.TempSetpoint
	gt.TempTarget = *in.TempTarget
	gt.State = *in.State
	gt.Mode = *in.Mode
	return gt.appliedLocked(changed), nil
}

func (gt *Thermo) InitAll() {
	gt.Req = ReqObject{
		Kind:   "Thermo",
		Clu:    gt.clu.Id,
		Id:     gt.GetMixedId(),
		Source: gt.Source,
	}
	gt.AppendHk()
}
func (gt *Thermo) AppendHk() *accessory.Thermostat {
	info := accessory.Info{
		Name:         gt.Name,
		SerialNumber: fmt.Sprintf("%d", gt.Id),
		Manufacturer: "Grenton",
		Model:        gt.Kind,
	}

	gt.hk = accessory.NewThermostat(info)
	gt.hk.Id = gt.GetLongId()

	gt.hk.Thermostat.TargetHeatingCoolingState.OnValueRemoteUpdate(gt.SetState)
	gt.hk.Thermostat.TargetTemperature.OnValueRemoteUpdate(gt.SetTemperature)

	gt.clu.set.Logf("HK Thermostat added (id: %x, type: %d", gt.hk.A.Id, gt.hk.A.Type)
	return gt.hk
}

func (gt *Thermo) Sync() {
	gt.syncMu.Lock()
	defer gt.syncMu.Unlock()
	gt.mu.Lock()
	current, target, state := gt.TempCurrent, gt.TempTarget, gt.State
	gt.mu.Unlock()

	gt.hk.Thermostat.CurrentTemperature.SetValue(current)
	gt.hk.Thermostat.TargetTemperature.SetValue(target)
	gt.hk.Thermostat.CurrentHeatingCoolingState.SetValue(state)
}

// commandLocked builds the write payload (all three fields, as the current script needs)
// from a copy of the device state. Call with gt.mu held.
func (gt *Thermo) commandLocked() ReqObject {
	req := gt.Req
	req.Thermo = &ThermoPayload{
		TempSetpoint: ptr(gt.TempSetpoint),
		State:        ptr(gt.State),
		Mode:         ptr(gt.Mode),
	}
	return req
}

// SetTemperature is the HomeKit write of the target temperature (rule 1): optimistic state
// and command snapshot under the device lock, no waiting for the GATE.
func (gt *Thermo) SetTemperature(temp float64) {
	gt.mu.Lock()
	defer gt.mu.Unlock()
	gt.TempSetpoint = temp
	gt.TempTarget = temp // what HomeKit shows as the target until the confirm read
	gt.submitCommandLocked(gt.commandLocked())
}

// SetState is the HomeKit write of the target heating/cooling state (rule 1): off, heat
// (State 1, Mode 0) or auto (State 1, Mode 1); cool is not supported and switches off.
func (gt *Thermo) SetState(state int) {
	gt.mu.Lock()
	defer gt.mu.Unlock()
	switch state {
	case 1:
		gt.State = 1
		gt.Mode = 0
	case 3:
		gt.State = 1
		gt.Mode = 1
	default:
		gt.State = 0
	}
	gt.submitCommandLocked(gt.commandLocked())
}
