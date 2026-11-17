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

// LoadReqObject applies a read item and pushes the state to HomeKit.
func (gt *Thermo) LoadReqObject(obj ReqObject) error { return loadReqObject(gt, obj) }

func (gt *Thermo) applyRead(obj ReqObject) (bool, error) {
	in, err := validThermo(obj)
	if err != nil {
		return false, err
	}
	gt.mu.Lock()
	defer gt.mu.Unlock()
	changed := !gt.loaded ||
		gt.TempCurrent != *in.TempCurrent || gt.TempSetpoint != *in.TempSetpoint || gt.TempTarget != *in.TempTarget ||
		gt.State != *in.State || gt.Mode != *in.Mode

	gt.TempCurrent = *in.TempCurrent
	gt.TempSetpoint = *in.TempSetpoint
	gt.TempTarget = *in.TempTarget
	gt.State = *in.State
	gt.Mode = *in.Mode
	gt.loaded = true
	return changed, nil
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

func (gt *Thermo) SetTemperature(temp float64) {
	gt.mu.Lock()
	gt.TempSetpoint = temp
	result := gt.submitCommandLocked(gt.commandLocked())
	gt.mu.Unlock()

	gt.waitCommand(result)
}
func (gt *Thermo) SetState(state int) {
	gt.mu.Lock()
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
	result := gt.submitCommandLocked(gt.commandLocked())
	gt.mu.Unlock()

	gt.waitCommand(result)
}
