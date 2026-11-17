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
	TempTarget,
	TempHoliday,
	TempMax,
	TempMin float64

	Mode,
	State int
}

func (gt *Thermo) GetHkState() (hkState int) {
	hkState = gt.State
	return
}

// LoadReqObject applies a read item and pushes the state to HomeKit.
func (gt *Thermo) LoadReqObject(obj ReqObject) error { return loadReqObject(gt, obj) }

func (gt *Thermo) applyRead(obj ReqObject) (bool, error) {
	if obj.Kind != "Thermo" {
		return false, fmt.Errorf("Thermo: wrong object kind (%s)", obj.Kind)
	}
	if obj.Thermo == nil {
		return false, fmt.Errorf("Thermo: missing Thermo object")
	}
	in := obj.Thermo
	changed := !gt.loaded ||
		gt.TempCurrent != in.TempCurrent || gt.TempSetpoint != in.TempSetpoint || gt.TempTarget != in.TempTarget ||
		gt.TempMin != in.TempMin || gt.TempMax != in.TempMax || gt.TempHoliday != in.TempHoliday ||
		gt.State != in.State || gt.Mode != in.Mode

	gt.TempCurrent = in.TempCurrent
	gt.TempSetpoint = in.TempSetpoint
	gt.TempTarget = in.TempTarget
	gt.TempMin = in.TempMin
	gt.TempMax = in.TempMax
	gt.TempHoliday = in.TempHoliday
	gt.State = in.State
	gt.Mode = in.Mode
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

	gt.hk.Thermostat.CurrentTemperature.SetValue(gt.TempCurrent)
	gt.hk.Thermostat.TargetTemperature.SetValue(gt.TempTarget)
	gt.hk.Thermostat.CurrentHeatingCoolingState.SetValue(gt.GetHkState())

}

func (gt *Thermo) SetTemperature(temp float64) {
	gt.hk.Thermostat.TargetTemperature.SetValue(temp)
	gt.TempSetpoint = temp

	req := gt.Req
	req.Thermo = gt
	obj, err := gt.SendReq(req)

	if err != nil {
		gt.clu.set.Error(fmt.Errorf("Thermo SetTemperature: %w", err))
		return
	}

	gt.LoadReqObject(obj)
}
func (gt *Thermo) SetState(state int) {
	gt.hk.Thermostat.TargetHeatingCoolingState.SetValue(state)
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

	req := gt.Req
	req.Thermo = gt
	obj, err := gt.SendReq(req)

	if err != nil {
		gt.clu.set.Error(fmt.Errorf("Thermo SetState: %w", err))
		return
	}

	gt.LoadReqObject(obj)
}
