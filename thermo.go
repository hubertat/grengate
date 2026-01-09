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

func (gt *Thermo) LoadReqObject(obj ReqObject) error {
	if obj.Kind != "Thermo" {
		return fmt.Errorf("Thermo LoadReqObject: wrong object kind (%s)", obj.Kind)
	}

	if obj.Thermo == nil {
		return fmt.Errorf("Thermo LoadReqObject: missing Thermo object")
	}

	gt.clu.set.Debugf("Thermo LoadReqObject loading: \n%+v", obj)

	gt.TempCurrent = obj.Thermo.TempCurrent
	gt.TempSetpoint = obj.Thermo.TempSetpoint
	gt.TempTarget = obj.Thermo.TempTarget
	gt.TempMin = obj.Thermo.TempMin
	gt.TempMax = obj.Thermo.TempMax
	gt.TempHoliday = obj.Thermo.TempHoliday
	gt.State = obj.Thermo.State
	gt.Mode = obj.Thermo.Mode

	gt.Sync()

	return nil
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
	// gt.hk.Thermostat.CurrentTemperature.OnValueRemoteGet(gt.GetTemperature)
	// gt.hk.Thermostat.CurrentHeatingCoolingState.OnValueRemoteGet(gt.GetState)

	gt.clu.set.Logf("HK Thermostat added (id: %x, type: %d", gt.hk.A.Id, gt.hk.A.Type)
	return gt.hk
}

func (gt *Thermo) Sync() {

	gt.hk.Thermostat.CurrentTemperature.SetValue(gt.TempCurrent)
	gt.hk.Thermostat.TargetTemperature.SetValue(gt.TempTarget)
	gt.hk.Thermostat.CurrentHeatingCoolingState.SetValue(gt.GetHkState())

}

func (gt *Thermo) GetTemperature() float64 {

	if gt.clu.set.CheckFreshness() {
		return gt.TempCurrent
	}

	err := gt.Update()
	if err != nil {
		gt.clu.set.Error(err)
	}

	return gt.TempCurrent

}
func (gt *Thermo) GetState() int {

	if gt.clu.set.CheckFreshness() {

		return gt.GetHkState()
	}

	err := gt.Update()
	if err != nil {
		gt.clu.set.Error(err)
	}

	return gt.GetHkState()

}

func (gt *Thermo) SetTemperature(temp float64) {
	gt.hk.Thermostat.TargetTemperature.SetValue(temp)
	gt.TempSetpoint = temp

	req := gt.Req
	req.Thermo = gt

	// Use async send to allow command batching
	gt.SendReqAsync(req)
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

	// Use async send to allow command batching
	gt.SendReqAsync(req)
}
